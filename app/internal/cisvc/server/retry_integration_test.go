package server

import (
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
)

// jobNamed returns the job of a pipeline with this name.
func jobNamed(t *testing.T, p *civ1.Pipeline, name string) *civ1.Job {
	t.Helper()
	for _, s := range p.GetStages() {
		for _, j := range s.GetJobs() {
			if j.GetName() == name {
				return j
			}
		}
	}
	t.Fatalf("pipeline has no job %q", name)
	return nil
}

func TestRetryJob(t *testing.T) {
	const (
		ok     = civ1.JobStatus_JOB_STATUS_SUCCESS
		failed = civ1.JobStatus_JOB_STATUS_FAILED
		none   = civ1.JobFailureReason_JOB_FAILURE_REASON_UNSPECIFIED
		script = civ1.JobFailureReason_JOB_FAILURE_REASON_SCRIPT_FAILURE
	)
	forEachMode(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		runnerCtx, _ := e.newRunner(t)
		repo := e.newRepo(t, threeStages)
		retry := func(jobID, key string) (*civ1.RetryJobResponse, error) {
			return e.ps.RetryJob(ctx, &civ1.RetryJobRequest{JobId: jobID, IdempotencyKey: key})
		}

		p := e.trigger(t, repo)
		build := e.requestJob(runnerCtx, t)
		_, err := retry(build.GetJobId(), uuid.NewString())
		wantCode(t, "retry while the pipeline runs", err, codes.FailedPrecondition)

		// build succeeds, lint fails, unit succeeds, so ship is skipped.
		e.finish(runnerCtx, t, build.GetJobId(), ok, 0, none)
		for range 2 {
			a := e.requestJob(runnerCtx, t)
			if a.GetName() == "lint" {
				e.finish(runnerCtx, t, a.GetJobId(), failed, 1, script)
			} else {
				e.finish(runnerCtx, t, a.GetJobId(), ok, 0, none)
			}
		}
		p = e.get(t, p.GetId())
		if got, want := jobStatuses(p), "build/build=SUCCESS test/lint=FAILED test/unit=SUCCESS deploy/ship=SKIPPED"; got != want ||
			p.GetStatus() != civ1.PipelineStatus_PIPELINE_STATUS_FAILED {
			t.Fatalf("before the retry: %v %s", p.GetStatus(), got)
		}
		lint1, ship1 := jobNamed(t, p, "lint"), jobNamed(t, p, "ship")
		if lint1.GetAttempt() != 1 || lint1.GetRetryOfId() != "" {
			t.Fatalf("first attempt: attempt=%d retry_of=%q", lint1.GetAttempt(), lint1.GetRetryOfId())
		}

		for _, c := range []struct {
			what string
			req  *civ1.RetryJobRequest
			code codes.Code
		}{
			{"malformed job_id", &civ1.RetryJobRequest{JobId: "x", IdempotencyKey: "k"}, codes.InvalidArgument},
			{"no idempotency_key", &civ1.RetryJobRequest{JobId: lint1.GetId()}, codes.InvalidArgument},
			{"malformed triggered_by_id", &civ1.RetryJobRequest{JobId: lint1.GetId(), IdempotencyKey: "k", TriggeredById: "x"}, codes.InvalidArgument},
			{"unknown job", &civ1.RetryJobRequest{JobId: uuid.NewString(), IdempotencyKey: uuid.NewString()}, codes.NotFound},
			{"skipped job", &civ1.RetryJobRequest{JobId: ship1.GetId(), IdempotencyKey: uuid.NewString()}, codes.FailedPrecondition},
		} {
			_, err := e.ps.RetryJob(ctx, c.req)
			wantCode(t, c.what, err, c.code)
		}

		// Retry lint: attempt 2 is queued, the skipped ship gets a new
		// attempt, and the pipeline runs again.
		req := &civ1.RetryJobRequest{JobId: lint1.GetId(), IdempotencyKey: uuid.NewString(), TriggeredById: uuid.NewString()}
		resp, err := e.ps.RetryJob(ctx, req)
		if err != nil {
			t.Fatalf("RetryJob: %v", err)
		}
		lint2 := resp.GetJob()
		if !resp.GetCreated() || lint2.GetName() != "lint" || lint2.GetAttempt() != 2 || lint2.GetRetryOfId() != lint1.GetId() ||
			lint2.GetStatus() != civ1.JobStatus_JOB_STATUS_PENDING || lint2.GetId() == lint1.GetId() {
			t.Fatalf("new attempt: created=%v %v", resp.GetCreated(), lint2)
		}
		rp := resp.GetPipeline()
		if got, want := jobStatuses(rp), "build/build=SUCCESS test/lint=PENDING test/unit=SUCCESS deploy/ship=CREATED"; got != want ||
			rp.GetStatus() != civ1.PipelineStatus_PIPELINE_STATUS_RUNNING || rp.GetFinishedAt() != nil {
			t.Fatalf("after the retry: %v %s finished_at=%v", rp.GetStatus(), got, rp.GetFinishedAt())
		}
		if s := jobNamed(t, rp, "ship"); s.GetAttempt() != 2 || s.GetRetryOfId() != ship1.GetId() {
			t.Fatalf("ship: attempt=%d retry_of=%q", s.GetAttempt(), s.GetRetryOfId())
		}

		// The same key returns the same attempt; on another job it is refused.
		again, err := e.ps.RetryJob(ctx, req)
		if err != nil || again.GetCreated() || again.GetJob().GetId() != lint2.GetId() {
			t.Fatalf("same key again: created=%v job=%s err=%v", again.GetCreated(), again.GetJob().GetId(), err)
		}
		_, err = retry(jobNamed(t, p, "unit").GetId(), req.GetIdempotencyKey())
		wantCode(t, "key used for another job", err, codes.AlreadyExists)

		// The earlier attempt keeps its status.
		old, err := e.ps.GetJob(ctx, &civ1.GetJobRequest{Id: lint1.GetId()})
		if err != nil || old.GetJob().GetStatus() != failed || old.GetJob().GetAttempt() != 1 {
			t.Fatalf("old attempt: %v %v", old.GetJob(), err)
		}

		// lint and then ship pass: the pipeline succeeds and announces it again.
		if a := e.requestJob(runnerCtx, t); a.GetJobId() != lint2.GetId() {
			t.Fatalf("leased %s, want lint attempt 2", a.GetName())
		}
		e.finish(runnerCtx, t, lint2.GetId(), ok, 0, none)
		ship := e.requestJob(runnerCtx, t)
		if ship.GetName() != "ship" {
			t.Fatalf("leased %s, want ship", ship.GetName())
		}
		e.finish(runnerCtx, t, ship.GetJobId(), ok, 0, none)
		p = e.get(t, p.GetId())
		if got, want := jobStatuses(p), "build/build=SUCCESS test/lint=SUCCESS test/unit=SUCCESS deploy/ship=SUCCESS"; got != want ||
			p.GetStatus() != civ1.PipelineStatus_PIPELINE_STATUS_SUCCESS || p.GetFinishedAt() == nil {
			t.Fatalf("after attempt 2: %v %s", p.GetStatus(), got)
		}
		var finished int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM ci_svc.outbox
			WHERE event_type = 'ci.v1.PipelineFinished' AND aggregate_id = $1`, p.GetId()).Scan(&finished); err != nil || finished != 2 {
			t.Fatalf("PipelineFinished events = %d (%v), want 2", finished, err)
		}

		_, err = retry(lint1.GetId(), uuid.NewString())
		wantCode(t, "retry an old attempt", err, codes.FailedPrecondition)

		// Retry build and let it fail. The later jobs ran, so they keep
		// their attempts, and none of them can be retried past the failure.
		resp, err = retry(jobNamed(t, p, "build").GetId(), uuid.NewString())
		if err != nil {
			t.Fatalf("RetryJob(build): %v", err)
		}
		if got, want := jobStatuses(resp.GetPipeline()), "build/build=PENDING test/lint=SUCCESS test/unit=SUCCESS deploy/ship=SUCCESS"; got != want {
			t.Fatalf("after retrying build: %s", got)
		}
		e.finish(runnerCtx, t, e.requestJob(runnerCtx, t).GetJobId(), failed, 2, script)
		if p = e.get(t, p.GetId()); p.GetStatus() != civ1.PipelineStatus_PIPELINE_STATUS_FAILED {
			t.Fatalf("after build attempt 2 failed: %v %s", p.GetStatus(), jobStatuses(p))
		}
		_, err = retry(jobNamed(t, p, "lint").GetId(), uuid.NewString())
		wantCode(t, "retry after an earlier stage failed", err, codes.FailedPrecondition)
	})
}
