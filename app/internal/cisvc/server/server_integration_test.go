package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/notify"
	"github.com/git2-project/git2-runner/app/internal/cisvc/pipeline"
	"github.com/git2-project/git2-runner/app/internal/cisvc/runners"
	"github.com/git2-project/git2-runner/app/internal/cisvc/secretbox"
	"github.com/git2-project/git2-runner/app/internal/cisvc/secrets"
	"github.com/git2-project/git2-runner/app/internal/cisvc/source"
	"github.com/git2-project/git2-runner/app/internal/cisvc/testdb"
)

// fakeSource serves one .git2-ci.yml per repository.
type fakeSource struct {
	mu    sync.Mutex
	files map[uuid.UUID][]byte
}

func (f *fakeSource) set(repo uuid.UUID, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[repo] = []byte(content)
}

func (f *fakeSource) ReadCIFile(_ context.Context, repo uuid.UUID, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.files[repo]; ok {
		return b, nil
	}
	return nil, source.ErrNoCIFile
}

// env is ci-svc wired as main does, on in-memory listeners.
type env struct {
	pool      *pgxpool.Pool
	src       *fakeSource
	pipelines *pipeline.Service
	internal  *grpc.ClientConn
	runnerCC  *grpc.ClientConn
	ps        civ1.PipelineServiceClient
	admin     civ1.RunnerAdminServiceClient
	sec       civ1.SecretServiceClient
	rs        civ1.RunnerServiceClient
	// tag is unique per test: every job and runner of the test carries
	// it, so tests sharing the database never take each other's jobs.
	tag string
}

var testOptions = pipeline.Options{
	Lease:      time.Second,
	LongPoll:   500 * time.Millisecond,
	JobPoll:    50 * time.Millisecond,
	LogPoll:    50 * time.Millisecond,
	MaxLogSize: 64,
}

func newEnv(t *testing.T, mode testdb.Mode) *env {
	t.Helper()
	pool := testdb.Open(t, mode)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := make([]byte, secretbox.KeySize)
	_, _ = rand.Read(key)
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	// Secrets written by other runs of the tests used other keys.
	if _, err := pool.Exec(t.Context(), `DELETE FROM ci_svc.secrets WHERE name LIKE 'T\_%'`); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{files: map[uuid.UUID][]byte{}}
	secretSvc := secrets.NewService(pool, box)
	hub := notify.New(nil, log)
	pipelines := pipeline.NewService(pool, src, secretSvc, hub, log, testOptions)
	deps := Deps{Pipelines: pipelines, Runners: runners.NewService(pool), Secrets: secretSvc, Log: log}

	e := &env{pool: pool, src: src, pipelines: pipelines, tag: "t-" + randHex(6)}
	e.internal = serve(t, NewInternal(deps, false))
	e.runnerCC = serve(t, NewRunner(deps, nil))
	e.ps = civ1.NewPipelineServiceClient(e.internal)
	e.admin = civ1.NewRunnerAdminServiceClient(e.internal)
	e.sec = civ1.NewSecretServiceClient(e.internal)
	e.rs = civ1.NewRunnerServiceClient(e.runnerCC)
	return e
}

func serve(t *testing.T, s *Server) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sha() string { return randHex(20) }

// newRepo returns a repository ID whose pipelines are deleted when the
// test ends.
func (e *env) newRepo(t *testing.T, ciYAML string) uuid.UUID {
	t.Helper()
	repo := uuid.New()
	if ciYAML != "" {
		e.src.set(repo, strings.ReplaceAll(ciYAML, "TAG", e.tag))
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.pool.Exec(ctx, `DELETE FROM ci_svc.outbox WHERE aggregate_id IN
			(SELECT id FROM ci_svc.pipelines WHERE repository_id = $1
			 UNION SELECT j.id FROM ci_svc.jobs j JOIN ci_svc.pipelines p ON p.id = j.pipeline_id WHERE p.repository_id = $1)`, repo)
		_, _ = e.pool.Exec(ctx, `DELETE FROM ci_svc.pipelines WHERE repository_id = $1`, repo)
		_, _ = e.pool.Exec(ctx, `DELETE FROM ci_svc.secrets WHERE scope_id = $1`, repo)
	})
	return repo
}

// newRunner registers a runner with the test's tag and returns a context
// that authenticates as it.
func (e *env) newRunner(t *testing.T) (context.Context, string) {
	t.Helper()
	ctx := t.Context()
	tok, err := e.admin.CreateRegistrationToken(ctx, &civ1.CreateRegistrationTokenRequest{
		Description: "test", ExpiresIn: durationpb.New(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRegistrationToken: %v", err)
	}
	reg, err := e.rs.RegisterRunner(ctx, &civ1.RegisterRunnerRequest{
		RegistrationToken: tok.GetToken(), Name: "runner-" + e.tag, Labels: []string{e.tag, "linux"},
		Executor: "docker", Version: "0.1.0", Platform: "linux/amd64",
	})
	if err != nil {
		t.Fatalf("RegisterRunner: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM ci_svc.runners WHERE id = $1`, reg.GetRunnerId())
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM ci_svc.registration_tokens WHERE runner_id = $1`, reg.GetRunnerId())
	})
	_, err = e.rs.RegisterRunner(ctx, &civ1.RegisterRunnerRequest{
		RegistrationToken: tok.GetToken(), Name: "again", Executor: "docker",
	})
	wantCode(t, "RegisterRunner with a used token", err, codes.Unauthenticated)
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+reg.GetRunnerToken()), reg.GetRunnerId()
}

func (e *env) trigger(t *testing.T, repo uuid.UUID) *civ1.Pipeline {
	t.Helper()
	resp, err := e.ps.TriggerPipeline(t.Context(), &civ1.TriggerPipelineRequest{
		RepositoryId: repo.String(), Ref: "refs/heads/main", CommitSha: sha(),
		Trigger: civ1.PipelineTrigger_PIPELINE_TRIGGER_PUSH, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("TriggerPipeline: %v", err)
	}
	return resp.GetPipeline()
}

func (e *env) requestJob(ctx context.Context, t *testing.T) *civ1.JobAssignment {
	t.Helper()
	resp, err := e.rs.RequestJob(ctx, &civ1.RequestJobRequest{})
	if err != nil {
		t.Fatalf("RequestJob: %v", err)
	}
	if resp.GetJob() == nil {
		t.Fatal("RequestJob returned no job")
	}
	return resp.GetJob()
}

func (e *env) finish(ctx context.Context, t *testing.T, jobID string, st civ1.JobStatus, exit int32, reason civ1.JobFailureReason) {
	t.Helper()
	if _, err := e.rs.UpdateJob(ctx, &civ1.UpdateJobRequest{
		JobId: jobID, Status: st, ExitCode: &exit, FailureReason: reason,
	}); err != nil {
		t.Fatalf("UpdateJob(%v): %v", st, err)
	}
}

func (e *env) get(t *testing.T, id string) *civ1.Pipeline {
	t.Helper()
	resp, err := e.ps.GetPipeline(t.Context(), &civ1.GetPipelineRequest{Id: id})
	if err != nil {
		t.Fatalf("GetPipeline: %v", err)
	}
	return resp.GetPipeline()
}

func wantCode(t *testing.T, what string, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("%s: got %v, want %v", what, err, code)
	}
}

// jobStatuses returns "stage/name=STATUS" for every job, in order.
func jobStatuses(p *civ1.Pipeline) string {
	var out []string
	for _, s := range p.GetStages() {
		for _, j := range s.GetJobs() {
			out = append(out, s.GetName()+"/"+j.GetName()+"="+strings.TrimPrefix(j.GetStatus().String(), "JOB_STATUS_"))
		}
	}
	return strings.Join(out, " ")
}

func varsOf(a *civ1.JobAssignment) map[string]*civ1.JobVariable {
	m := map[string]*civ1.JobVariable{}
	for _, v := range a.GetVariables() {
		m[v.GetName()] = v // later entries win, as on the runner
	}
	return m
}

const threeStages = `
stages: [build, test, deploy]
variables:
  GLOBAL: from-file
build:
  stage: build
  image: alpine:3.20
  tags: [TAG]
  script: [make]
lint:
  stage: test
  image: alpine:3.20
  tags: [TAG]
  script: [make lint]
unit:
  stage: test
  image: alpine:3.20
  tags: [TAG]
  variables:
    GLOBAL: from-job
  script: [make test]
ship:
  stage: deploy
  image: alpine:3.20
  tags: [TAG]
  script: [make ship]
`

func forEachMode(t *testing.T, fn func(t *testing.T, e *env)) {
	for _, mode := range testdb.Modes {
		t.Run(string(mode), func(t *testing.T) { fn(t, newEnv(t, mode)) })
	}
}

func TestPipelineLifecycle(t *testing.T) {
	forEachMode(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		runnerCtx, runnerID := e.newRunner(t)
		repo := e.newRepo(t, threeStages)

		secretName := "T_" + strings.ToUpper(randHex(4))
		for _, s := range []struct {
			scope civ1.SecretScope
			id    string
			value string
		}{
			{civ1.SecretScope_SECRET_SCOPE_INSTANCE, "", "instance-value"},
			{civ1.SecretScope_SECRET_SCOPE_REPOSITORY, repo.String(), "repo-value"},
		} {
			if _, err := e.sec.SetSecret(ctx, &civ1.SetSecretRequest{Scope: s.scope, ScopeId: s.id, Name: secretName, Value: s.value}); err != nil {
				t.Fatalf("SetSecret: %v", err)
			}
		}
		t.Cleanup(func() {
			_, _ = e.pool.Exec(context.Background(), `DELETE FROM ci_svc.secrets WHERE name = $1`, secretName)
		})

		// Trigger, and trigger again with the same key.
		req := &civ1.TriggerPipelineRequest{
			RepositoryId: repo.String(), Ref: "refs/heads/main", CommitSha: sha(),
			Trigger: civ1.PipelineTrigger_PIPELINE_TRIGGER_PUSH, IdempotencyKey: uuid.NewString() + ":refs/heads/main",
		}
		first, err := e.ps.TriggerPipeline(ctx, req)
		if err != nil {
			t.Fatalf("TriggerPipeline: %v", err)
		}
		p := first.GetPipeline()
		if !first.GetCreated() || p.GetStatus() != civ1.PipelineStatus_PIPELINE_STATUS_PENDING {
			t.Fatalf("created=%v status=%v", first.GetCreated(), p.GetStatus())
		}
		if got, want := jobStatuses(p), "build/build=PENDING test/lint=CREATED test/unit=CREATED deploy/ship=CREATED"; got != want {
			t.Fatalf("jobs = %s\nwant   %s", got, want)
		}
		again, err := e.ps.TriggerPipeline(ctx, req)
		if err != nil || again.GetCreated() || again.GetPipeline().GetId() != p.GetId() {
			t.Fatalf("second trigger: created=%v id=%s err=%v", again.GetCreated(), again.GetPipeline().GetId(), err)
		}
		req.CommitSha = sha()
		_, err = e.ps.TriggerPipeline(ctx, req)
		wantCode(t, "same key, other commit", err, codes.AlreadyExists)

		// Stage 1: build.
		build := e.requestJob(runnerCtx, t)
		if build.GetName() != "build" || build.GetPipelineId() != p.GetId() || build.GetTimeout().AsDuration() != time.Hour {
			t.Fatalf("got job %s of %s, timeout %v", build.GetName(), build.GetPipelineId(), build.GetTimeout().AsDuration())
		}
		vars := varsOf(build)
		if vars["CI_JOB_NAME"].GetValue() != "build" || vars["CI_COMMIT_BRANCH"].GetValue() != "main" || vars["GLOBAL"].GetValue() != "from-file" {
			t.Errorf("predefined or file variables wrong: %v", build.GetVariables())
		}
		if v := vars[secretName]; v.GetValue() != "repo-value" || !v.GetMasked() {
			t.Errorf("secret = %q masked=%v, want the repository's value, masked", v.GetValue(), v.GetMasked())
		}
		if got := e.get(t, p.GetId()); got.GetStatus() != civ1.PipelineStatus_PIPELINE_STATUS_RUNNING || got.GetStartedAt() == nil {
			t.Errorf("pipeline after lease: %v started_at=%v", got.GetStatus(), got.GetStartedAt())
		}

		// Follow the log while the runner writes it.
		var streamed strings.Builder
		streamDone := make(chan error, 1)
		go func() {
			stream, err := e.ps.StreamJobLog(ctx, &civ1.StreamJobLogRequest{JobId: build.GetJobId(), Follow: true})
			if err != nil {
				streamDone <- err
				return
			}
			for {
				msg, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					streamDone <- nil
					return
				}
				if err != nil {
					streamDone <- err
					return
				}
				if int(msg.GetOffset()) != streamed.Len() {
					streamDone <- fmt.Errorf("offset %d after %d bytes", msg.GetOffset(), streamed.Len())
					return
				}
				streamed.Write(msg.GetData())
			}
		}()
		for _, c := range []struct {
			offset int64
			data   string
			size   int64
		}{{0, "hello ", 6}, {6, "world\n", 12}, {0, "hello ", 12}, {20, "gap", 12}} {
			resp, err := e.rs.AppendLog(runnerCtx, &civ1.AppendLogRequest{JobId: build.GetJobId(), Offset: c.offset, Data: []byte(c.data)})
			if err != nil || resp.GetSize() != c.size {
				t.Fatalf("AppendLog(%d, %q) = %d, %v; want size %d", c.offset, c.data, resp.GetSize(), err, c.size)
			}
		}
		hb, err := e.rs.UpdateJob(runnerCtx, &civ1.UpdateJobRequest{JobId: build.GetJobId(), Status: civ1.JobStatus_JOB_STATUS_RUNNING})
		if err != nil || hb.GetCancelRequested() {
			t.Fatalf("heartbeat: %v cancel=%v", err, hb.GetCancelRequested())
		}
		e.finish(runnerCtx, t, build.GetJobId(), civ1.JobStatus_JOB_STATUS_SUCCESS, 0, 0)
		select {
		case err := <-streamDone:
			if err != nil || streamed.String() != "hello world\n" {
				t.Fatalf("stream = %q, %v", streamed.String(), err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("log stream did not end after the job finished")
		}
		_, err = e.rs.UpdateJob(runnerCtx, &civ1.UpdateJobRequest{JobId: build.GetJobId(), Status: civ1.JobStatus_JOB_STATUS_RUNNING})
		wantCode(t, "heartbeat after finish", err, codes.FailedPrecondition)

		// Stage 2: lint passes, unit fails; deploy is skipped.
		a, b := e.requestJob(runnerCtx, t), e.requestJob(runnerCtx, t)
		if a.GetName()+","+b.GetName() != "lint,unit" && a.GetName()+","+b.GetName() != "unit,lint" {
			t.Fatalf("stage 2 jobs: %s, %s", a.GetName(), b.GetName())
		}
		unit := a
		if b.GetName() == "unit" {
			unit = b
		}
		if varsOf(unit)["GLOBAL"].GetValue() != "from-job" {
			t.Errorf("job variables must override file variables")
		}
		for _, j := range []*civ1.JobAssignment{a, b} {
			if j == unit {
				e.finish(runnerCtx, t, j.GetJobId(), civ1.JobStatus_JOB_STATUS_FAILED, 2, civ1.JobFailureReason_JOB_FAILURE_REASON_SCRIPT_FAILURE)
			} else {
				e.finish(runnerCtx, t, j.GetJobId(), civ1.JobStatus_JOB_STATUS_SUCCESS, 0, 0)
			}
		}
		final := e.get(t, p.GetId())
		if final.GetStatus() != civ1.PipelineStatus_PIPELINE_STATUS_FAILED || final.GetFinishedAt() == nil {
			t.Fatalf("pipeline = %v finished_at=%v", final.GetStatus(), final.GetFinishedAt())
		}
		if got, want := jobStatuses(final), "build/build=SUCCESS test/lint=SUCCESS test/unit=FAILED deploy/ship=SKIPPED"; got != want {
			t.Fatalf("jobs = %s\nwant   %s", got, want)
		}
		for _, j := range final.GetStages()[1].GetJobs() {
			if j.GetName() == "unit" && (j.GetExitCode() != 2 || j.GetRunnerId() != runnerID ||
				j.GetFailureReason() != civ1.JobFailureReason_JOB_FAILURE_REASON_SCRIPT_FAILURE) {
				t.Errorf("unit = exit %d runner %s reason %v", j.GetExitCode(), j.GetRunnerId(), j.GetFailureReason())
			}
		}

		// Events: one PipelineCreated, one PipelineFinished, four JobFinished.
		var created, finished, jobsFinished int
		var finishedPayload string
		rows, err := e.pool.Query(ctx, `
			SELECT event_type, payload::text FROM ci_svc.outbox
			WHERE aggregate_id = $1 OR aggregate_id IN (SELECT id FROM ci_svc.jobs WHERE pipeline_id = $1)`, p.GetId())
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var typ, payload string
			if err := rows.Scan(&typ, &payload); err != nil {
				t.Fatal(err)
			}
			switch typ {
			case "ci.v1.PipelineCreated":
				created++
			case "ci.v1.PipelineFinished":
				finished++
				finishedPayload = payload
			case "ci.v1.JobFinished":
				jobsFinished++
			}
		}
		if created != 1 || finished != 1 || jobsFinished != 4 || !strings.Contains(finishedPayload, "PIPELINE_STATUS_FAILED") {
			t.Errorf("events: created %d finished %d jobFinished %d payload %s", created, finished, jobsFinished, finishedPayload)
		}

		// The list shows it, newest first.
		second := e.trigger(t, repo)
		list, err := e.ps.ListPipelines(ctx, &civ1.ListPipelinesRequest{RepositoryId: repo.String(), PageSize: 1})
		if err != nil || len(list.GetPipelines()) != 1 || list.GetPipelines()[0].GetId() != second.GetId() || list.GetNextPageToken() == "" {
			t.Fatalf("ListPipelines page 1: %v %v", list, err)
		}
		list, err = e.ps.ListPipelines(ctx, &civ1.ListPipelinesRequest{RepositoryId: repo.String(), PageSize: 1, PageToken: list.GetNextPageToken()})
		if err != nil || len(list.GetPipelines()) != 1 || list.GetPipelines()[0].GetId() != p.GetId() || len(list.GetPipelines()[0].GetStages()) != 3 {
			t.Fatalf("ListPipelines page 2: %v %v", list, err)
		}
		_, err = e.ps.ListPipelines(ctx, &civ1.ListPipelinesRequest{RepositoryId: uuid.NewString(), PageToken: list.GetNextPageToken() + "x"})
		wantCode(t, "bad page token", err, codes.InvalidArgument)
	})
}

func TestCancelAndReaper(t *testing.T) {
	forEachMode(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		runnerCtx, _ := e.newRunner(t)
		repo := e.newRepo(t, threeStages)

		// Cancel while build runs: waiting jobs at once, build at its next
		// report.
		p := e.trigger(t, repo)
		build := e.requestJob(runnerCtx, t)
		resp, err := e.ps.CancelPipeline(ctx, &civ1.CancelPipelineRequest{Id: p.GetId()})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := jobStatuses(resp.GetPipeline()), "build/build=RUNNING test/lint=CANCELED test/unit=CANCELED deploy/ship=CANCELED"; got != want {
			t.Fatalf("after cancel: %s", got)
		}
		hb, err := e.rs.UpdateJob(runnerCtx, &civ1.UpdateJobRequest{JobId: build.GetJobId(), Status: civ1.JobStatus_JOB_STATUS_RUNNING})
		if err != nil || !hb.GetCancelRequested() {
			t.Fatalf("heartbeat must report the cancel: %v %v", hb, err)
		}
		e.finish(runnerCtx, t, build.GetJobId(), civ1.JobStatus_JOB_STATUS_FAILED, 137, civ1.JobFailureReason_JOB_FAILURE_REASON_SCRIPT_FAILURE)
		if got := e.get(t, p.GetId()); got.GetStatus() != civ1.PipelineStatus_PIPELINE_STATUS_CANCELED ||
			jobStatuses(got) != "build/build=CANCELED test/lint=CANCELED test/unit=CANCELED deploy/ship=CANCELED" {
			t.Fatalf("after the runner stopped: %v %s", got.GetStatus(), jobStatuses(got))
		}
		if _, err := e.ps.CancelPipeline(ctx, &civ1.CancelPipelineRequest{Id: p.GetId()}); err != nil {
			t.Fatalf("canceling a finished pipeline: %v", err)
		}

		// A runner that disappears: the lease runs out and the reaper fails
		// the job, as after a restore from a backup taken mid-run.
		p = e.trigger(t, repo)
		e.requestJob(runnerCtx, t)
		time.Sleep(testOptions.Lease + 200*time.Millisecond)
		if _, err := e.pipelines.ReapExpired(ctx); err != nil {
			t.Fatal(err)
		}
		got := e.get(t, p.GetId())
		if got.GetStatus() != civ1.PipelineStatus_PIPELINE_STATUS_FAILED ||
			jobStatuses(got) != "build/build=FAILED test/lint=SKIPPED test/unit=SKIPPED deploy/ship=SKIPPED" ||
			got.GetStages()[0].GetJobs()[0].GetFailureReason() != civ1.JobFailureReason_JOB_FAILURE_REASON_RUNNER_LOST {
			t.Fatalf("after the reaper: %v %s", got.GetStatus(), jobStatuses(got))
		}
	})
}

func TestConfigErrors(t *testing.T) {
	forEachMode(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		repo := e.newRepo(t, "build:\n  image: alpine\n  scrpt: make\n")
		p := e.trigger(t, repo)
		if p.GetStatus() != civ1.PipelineStatus_PIPELINE_STATUS_FAILED || len(p.GetStages()) != 0 ||
			!strings.Contains(p.GetErrorMessage(), "line 3: build.scrpt: unknown key") || p.GetFinishedAt() == nil {
			t.Fatalf("invalid file: status %v stages %d message %q", p.GetStatus(), len(p.GetStages()), p.GetErrorMessage())
		}

		_, err := e.ps.TriggerPipeline(ctx, &civ1.TriggerPipelineRequest{
			RepositoryId: e.newRepo(t, "").String(), Ref: "refs/heads/main", CommitSha: sha(),
			Trigger: civ1.PipelineTrigger_PIPELINE_TRIGGER_PUSH, IdempotencyKey: uuid.NewString(),
		})
		wantCode(t, "no .git2-ci.yml", err, codes.NotFound)

		for name, req := range map[string]*civ1.TriggerPipelineRequest{
			"bad repository": {RepositoryId: "x", Ref: "refs/heads/main", CommitSha: sha(), Trigger: 1, IdempotencyKey: "k"},
			"short ref":      {RepositoryId: repo.String(), Ref: "main", CommitSha: sha(), Trigger: 1, IdempotencyKey: "k"},
			"bad sha":        {RepositoryId: repo.String(), Ref: "refs/heads/main", CommitSha: "ABC", Trigger: 1, IdempotencyKey: "k"},
			"no trigger":     {RepositoryId: repo.String(), Ref: "refs/heads/main", CommitSha: sha(), IdempotencyKey: "k"},
			"no key":         {RepositoryId: repo.String(), Ref: "refs/heads/main", CommitSha: sha(), Trigger: 1},
		} {
			_, err := e.ps.TriggerPipeline(ctx, req)
			wantCode(t, name, err, codes.InvalidArgument)
		}

		v, err := e.ps.ValidateConfig(ctx, &civ1.ValidateConfigRequest{Content: "j:\n  image: x\n"})
		if err != nil || v.GetValid() || len(v.GetErrors()) != 1 || v.GetErrors()[0].GetLine() != 1 {
			t.Fatalf("ValidateConfig: %v %v", v, err)
		}
		v, err = e.ps.ValidateConfig(ctx, &civ1.ValidateConfigRequest{Content: "j:\n  image: x\n  script: y\n"})
		if err != nil || !v.GetValid() {
			t.Fatalf("ValidateConfig of a valid file: %v %v", v, err)
		}
	})
}

func TestRunnerAuthAndAdmin(t *testing.T) {
	forEachMode(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		_, err := e.rs.RequestJob(ctx, &civ1.RequestJobRequest{})
		wantCode(t, "no token", err, codes.Unauthenticated)
		bad := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer g2r_"+strings.Repeat("A", 43))
		_, err = e.rs.RequestJob(bad, &civ1.RequestJobRequest{})
		wantCode(t, "unknown token", err, codes.Unauthenticated)

		runnerCtx, runnerID := e.newRunner(t)
		repo := e.newRepo(t, threeStages)
		e.trigger(t, repo)

		// Paused: no job, even though one waits.
		paused := true
		upd, err := e.admin.UpdateRunner(ctx, &civ1.UpdateRunnerRequest{Id: runnerID, Paused: &paused})
		if err != nil || upd.GetRunner().GetStatus() != civ1.RunnerStatus_RUNNER_STATUS_PAUSED {
			t.Fatalf("pause: %v %v", upd, err)
		}
		resp, err := e.rs.RequestJob(runnerCtx, &civ1.RequestJobRequest{})
		if err != nil || resp.GetJob() != nil {
			t.Fatalf("a paused runner got %v, %v", resp.GetJob(), err)
		}
		paused = false
		desc := "build box"
		upd, err = e.admin.UpdateRunner(ctx, &civ1.UpdateRunnerRequest{Id: runnerID, Paused: &paused, Description: &desc})
		if err != nil || upd.GetRunner().GetStatus() != civ1.RunnerStatus_RUNNER_STATUS_ONLINE || upd.GetRunner().GetDescription() != desc {
			t.Fatalf("resume: %v %v", upd, err)
		}
		job := e.requestJob(runnerCtx, t)

		got, err := e.admin.GetRunner(ctx, &civ1.GetRunnerRequest{Id: runnerID})
		if err != nil || got.GetRunner().GetName() != "runner-"+e.tag || got.GetRunner().GetLastSeenAt() == nil {
			t.Fatalf("GetRunner: %v %v", got, err)
		}
		list, err := e.admin.ListRunners(ctx, &civ1.ListRunnersRequest{PageSize: 100})
		if err != nil || len(list.GetRunners()) == 0 {
			t.Fatalf("ListRunners: %v %v", list, err)
		}

		// Another runner cannot report on this runner's job.
		otherCtx, _ := e.newRunner(t)
		_, err = e.rs.AppendLog(otherCtx, &civ1.AppendLogRequest{JobId: job.GetJobId(), Data: []byte("x")})
		wantCode(t, "AppendLog by another runner", err, codes.NotFound)

		// Log limit: 64 bytes in tests, then a notice.
		big := []byte(strings.Repeat("x", 100))
		lr, err := e.rs.AppendLog(runnerCtx, &civ1.AppendLogRequest{JobId: job.GetJobId(), Data: big})
		if err != nil || !lr.GetTruncated() || lr.GetSize() <= 64 {
			t.Fatalf("AppendLog over the limit: %v %v", lr, err)
		}

		// Deleted: the token stops working.
		if _, err := e.admin.DeleteRunner(ctx, &civ1.DeleteRunnerRequest{Id: runnerID}); err != nil {
			t.Fatal(err)
		}
		_, err = e.rs.RequestJob(runnerCtx, &civ1.RequestJobRequest{})
		wantCode(t, "deleted runner", err, codes.Unauthenticated)
		_, err = e.admin.DeleteRunner(ctx, &civ1.DeleteRunnerRequest{Id: runnerID})
		wantCode(t, "delete twice", err, codes.NotFound)
	})
}

func TestSecretsAPI(t *testing.T) {
	forEachMode(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		repo := e.newRepo(t, "")
		scope := civ1.SecretScope_SECRET_SCOPE_REPOSITORY
		for i, name := range []string{"T_B", "T_A", "T_C", "T_A"} {
			resp, err := e.sec.SetSecret(ctx, &civ1.SetSecretRequest{Scope: scope, ScopeId: repo.String(), Name: name, Value: fmt.Sprint("v", i)})
			if err != nil {
				t.Fatalf("SetSecret %s: %v", name, err)
			}
			if resp.GetCreated() != (i < 3) {
				t.Errorf("SetSecret %s #%d: created = %v", name, i, resp.GetCreated())
			}
		}
		page1, err := e.sec.ListSecrets(ctx, &civ1.ListSecretsRequest{Scope: scope, ScopeId: repo.String(), PageSize: 2})
		if err != nil || len(page1.GetSecrets()) != 2 || page1.GetSecrets()[0].GetName() != "T_A" || page1.GetNextPageToken() == "" {
			t.Fatalf("ListSecrets page 1: %v %v", page1, err)
		}
		page2, err := e.sec.ListSecrets(ctx, &civ1.ListSecretsRequest{Scope: scope, ScopeId: repo.String(), PageSize: 2, PageToken: page1.GetNextPageToken()})
		if err != nil || len(page2.GetSecrets()) != 1 || page2.GetSecrets()[0].GetName() != "T_C" || page2.GetNextPageToken() != "" {
			t.Fatalf("ListSecrets page 2: %v %v", page2, err)
		}
		_, err = e.sec.ListSecrets(ctx, &civ1.ListSecretsRequest{Scope: civ1.SecretScope_SECRET_SCOPE_INSTANCE, PageToken: page1.GetNextPageToken()})
		wantCode(t, "page token of another scope", err, codes.InvalidArgument)

		var stored []byte
		if err := e.pool.QueryRow(ctx, `SELECT ciphertext FROM ci_svc.secrets WHERE scope_id = $1 AND name = 'T_A'`, repo).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(stored), "v3") {
			t.Error("the database holds the value in plain text")
		}

		if _, err := e.sec.DeleteSecret(ctx, &civ1.DeleteSecretRequest{Scope: scope, ScopeId: repo.String(), Name: "T_A"}); err != nil {
			t.Fatal(err)
		}
		_, err = e.sec.DeleteSecret(ctx, &civ1.DeleteSecretRequest{Scope: scope, ScopeId: repo.String(), Name: "T_A"})
		wantCode(t, "delete twice", err, codes.NotFound)

		for name, req := range map[string]*civ1.SetSecretRequest{
			"no scope":           {Name: "T_X", Value: "v"},
			"instance with id":   {Scope: civ1.SecretScope_SECRET_SCOPE_INSTANCE, ScopeId: repo.String(), Name: "T_X", Value: "v"},
			"repository bad id":  {Scope: scope, ScopeId: "nope", Name: "T_X", Value: "v"},
			"lower case":         {Scope: scope, ScopeId: repo.String(), Name: "t_x", Value: "v"},
			"reserved":           {Scope: scope, ScopeId: repo.String(), Name: "CI_TOKEN", Value: "v"},
			"empty value":        {Scope: scope, ScopeId: repo.String(), Name: "T_X"},
			"value with NUL":     {Scope: scope, ScopeId: repo.String(), Name: "T_X", Value: "a\x00b"},
			"value over 32 KiB":  {Scope: scope, ScopeId: repo.String(), Name: "T_X", Value: strings.Repeat("v", 32<<10+1)},
			"starts with digit":  {Scope: scope, ScopeId: repo.String(), Name: "1X", Value: "v"},
			"repository no id":   {Scope: scope, Name: "T_X", Value: "v"},
			"unknown scope enum": {Scope: 9, Name: "T_X", Value: "v"},
		} {
			_, err := e.sec.SetSecret(ctx, req)
			wantCode(t, name, err, codes.InvalidArgument)
		}
	})
}

// TestConcurrentLease checks the queue's main promise: with many runners
// asking at once, every job goes to exactly one runner.
func TestConcurrentLease(t *testing.T) {
	forEachMode(t, func(t *testing.T, e *env) {
		const jobs, runnerCount = 12, 6
		var b strings.Builder
		for i := range jobs {
			fmt.Fprintf(&b, "j%02d:\n  image: alpine\n  tags: [TAG]\n  script: [\"true\"]\n", i)
		}
		repo := e.newRepo(t, b.String())
		e.trigger(t, repo)

		var mu sync.Mutex
		seen := map[string]int{}
		var wg sync.WaitGroup
		for range runnerCount {
			ctx, _ := e.newRunner(t)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					resp, err := e.rs.RequestJob(ctx, &civ1.RequestJobRequest{})
					if err != nil {
						t.Errorf("RequestJob: %v", err)
						return
					}
					if resp.GetJob() == nil {
						return // queue drained; the long poll timed out
					}
					mu.Lock()
					seen[resp.GetJob().GetJobId()]++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(seen) != jobs {
			t.Fatalf("%d distinct jobs leased, want %d", len(seen), jobs)
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("job %s leased %d times", id, n)
			}
		}
	})
}
