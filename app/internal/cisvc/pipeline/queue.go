package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/db"
	"github.com/git2-project/git2-runner/app/internal/cisvc/notify"
)

// Runner is what the queue needs to know about the runner asking for work.
type Runner struct {
	ID     uuid.UUID
	Labels []string
	Paused bool
}

// leaseSQL hands the oldest pending job this runner can run to the runner.
// SKIP LOCKED lets concurrent runners each take a different job without
// waiting for each other. A job matches when all its tags are among the
// runner's labels; a job without tags matches every runner.
const leaseSQL = `
WITH next AS (
	SELECT id FROM ci_svc.jobs
	WHERE status = 'pending' AND tags <@ $2::text[]
	ORDER BY queued_at, id
	LIMIT 1
	FOR UPDATE SKIP LOCKED
)
UPDATE ci_svc.jobs j SET
	status = 'running', runner_id = $1, started_at = now(),
	lease_expires_at = now() + make_interval(secs => $3::float8), updated_at = now()
FROM next WHERE j.id = next.id
RETURNING j.id, j.pipeline_id, j.name, j.stage, j.spec::text`

// Lease gives the runner one pending job it can run, or nil if there is
// none.
func (s *Service) Lease(ctx context.Context, r Runner) (*Assignment, error) {
	if r.Paused {
		return nil, nil
	}
	labels := r.Labels
	if labels == nil {
		labels = []string{}
	}
	var a *Assignment
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		a = nil
		var (
			x    Assignment
			spec string
		)
		err := tx.QueryRow(ctx, leaseSQL, r.ID, labels, s.opts.Lease.Seconds()).
			Scan(&x.JobID, &x.PipelineID, &x.Name, &x.Stage, &spec)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lease job: %w", err)
		}
		err = tx.QueryRow(ctx, `
			UPDATE ci_svc.pipelines SET
				status = CASE WHEN status = 'pending' THEN 'running' ELSE status END,
				started_at = COALESCE(started_at, now()), updated_at = now()
			WHERE id = $1
			RETURNING repository_id, ref, commit_sha`, x.PipelineID).Scan(&x.RepositoryID, &x.Ref, &x.CommitSHA)
		if err != nil {
			return fmt.Errorf("start pipeline: %w", err)
		}
		var sp Spec
		if err := json.Unmarshal([]byte(spec), &sp); err != nil {
			return fmt.Errorf("decode job spec: %w", err)
		}
		secretVars, err := s.secrets.ForRepository(ctx, tx, x.RepositoryID)
		if err != nil {
			return fmt.Errorf("read secrets for job: %w", err)
		}
		x.Image, x.Script = sp.Image, sp.Script
		x.Timeout = time.Duration(sp.TimeoutSeconds) * time.Second
		x.Variables = append(predefined(&x), sp.Variables...)
		for _, v := range secretVars {
			x.Variables = append(x.Variables, Variable{Name: v.Name, Value: v.Value, Masked: true})
		}
		a = &x
		return nil
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// predefined returns the CI_* variables every job gets.
func predefined(a *Assignment) []Variable {
	short := strings.TrimPrefix(strings.TrimPrefix(a.Ref, "refs/heads/"), "refs/tags/")
	vars := []Variable{
		{Name: "CI", Value: "true"},
		{Name: "GIT2_CI", Value: "true"},
		{Name: "CI_PIPELINE_ID", Value: a.PipelineID.String()},
		{Name: "CI_JOB_ID", Value: a.JobID.String()},
		{Name: "CI_JOB_NAME", Value: a.Name},
		{Name: "CI_JOB_STAGE", Value: a.Stage},
		{Name: "CI_REPOSITORY_ID", Value: a.RepositoryID.String()},
		{Name: "CI_COMMIT_SHA", Value: a.CommitSHA},
		{Name: "CI_COMMIT_REF", Value: a.Ref},
		{Name: "CI_COMMIT_REF_NAME", Value: short},
	}
	if strings.HasPrefix(a.Ref, "refs/heads/") {
		vars = append(vars, Variable{Name: "CI_COMMIT_BRANCH", Value: short})
	} else {
		vars = append(vars, Variable{Name: "CI_COMMIT_TAG", Value: short})
	}
	return vars
}

// RequestJob waits up to Options.LongPoll for a job the runner can run.
// It returns nil when none came. A wake-up signal makes it try at once;
// without signals it tries every Options.JobPoll.
func (s *Service) RequestJob(ctx context.Context, r Runner) (*Assignment, error) {
	wake, unsubscribe := s.hub.Subscribe(notify.JobsTopic)
	defer unsubscribe()
	deadline := time.NewTimer(s.opts.LongPoll)
	defer deadline.Stop()
	poll := time.NewTicker(s.opts.JobPoll)
	defer poll.Stop()
	for {
		a, err := s.Lease(ctx, r)
		if err != nil || a != nil {
			return a, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, nil
		case <-wake:
		case <-poll.C:
		}
	}
}

// UpdateInput is a runner's report about a job.
type UpdateInput struct {
	JobID    uuid.UUID
	Status   JobStatus // JobRunning (heartbeat), JobSuccess or JobFailed
	ExitCode *int32
	Reason   FailureReason
}

func validateUpdate(in UpdateInput) error {
	switch in.Status {
	case JobRunning:
		if in.Reason != ReasonNone {
			return apierr.InvalidField("failure_reason", "must be unset unless status is FAILED")
		}
	case JobSuccess:
		if in.Reason != ReasonNone {
			return apierr.InvalidField("failure_reason", "must be unset unless status is FAILED")
		}
		if in.ExitCode != nil && *in.ExitCode != 0 {
			return apierr.InvalidField("exit_code", "must be 0 with status SUCCESS")
		}
	case JobFailed:
		if in.Reason == ReasonNone || in.Reason == ReasonRunnerLost {
			return apierr.InvalidField("failure_reason", "is required with status FAILED, and RUNNER_LOST is set only by ci-svc")
		}
		if in.Reason == ReasonScriptFailure && in.ExitCode == nil {
			return apierr.InvalidField("exit_code", "is required with reason SCRIPT_FAILURE")
		}
	default:
		return apierr.InvalidField("status", "must be RUNNING, SUCCESS or FAILED")
	}
	return nil
}

var (
	errNotRunnersJob = apierr.New(apierr.NotFound, "this runner is not running a job with this ID")
	errJobFinished   = apierr.New(apierr.FailedPrecondition, "the job has already finished; stop it and do not report it again")
)

// lockRunnersJob locks a job that runnerID runs, and returns its status,
// cancel flag, pipeline, repository and name.
func lockRunnersJob(ctx context.Context, tx pgx.Tx, runnerID, jobID uuid.UUID) (j struct {
	status          JobStatus
	cancelRequested bool
	pipelineID      uuid.UUID
	repositoryID    uuid.UUID
	name            string
	logSize         int64
}, err error) {
	var owner uuid.NullUUID
	err = tx.QueryRow(ctx, `
		SELECT j.status, j.runner_id, j.cancel_requested, j.pipeline_id, p.repository_id, j.name, j.log_size
		FROM ci_svc.jobs j JOIN ci_svc.pipelines p ON p.id = j.pipeline_id
		WHERE j.id = $1 FOR UPDATE OF j`, jobID).
		Scan(&j.status, &owner, &j.cancelRequested, &j.pipelineID, &j.repositoryID, &j.name, &j.logSize)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!owner.Valid || owner.UUID != runnerID)) {
		return j, errNotRunnersJob
	}
	if err != nil {
		return j, fmt.Errorf("lock job: %w", err)
	}
	if j.status != JobRunning {
		return j, errJobFinished
	}
	return j, nil
}

// UpdateJob records a runner's report about a job it runs. A heartbeat
// extends the lease; a final status finishes the job and moves the
// pipeline on. It reports whether the job was canceled.
func (s *Service) UpdateJob(ctx context.Context, runnerID uuid.UUID, in UpdateInput) (bool, error) {
	if err := validateUpdate(in); err != nil {
		return false, err
	}
	var cancelRequested, finished, queued bool
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		cancelRequested, finished, queued = false, false, false
		j, err := lockRunnersJob(ctx, tx, runnerID, in.JobID)
		if err != nil {
			return err
		}
		cancelRequested = j.cancelRequested
		if in.Status == JobRunning {
			_, err := tx.Exec(ctx, `
				UPDATE ci_svc.jobs SET lease_expires_at = now() + make_interval(secs => $2::float8), updated_at = now()
				WHERE id = $1`, in.JobID, s.opts.Lease.Seconds())
			if err != nil {
				return fmt.Errorf("extend lease: %w", err)
			}
			return nil
		}

		status, reason := in.Status, in.Reason
		if j.cancelRequested {
			status, reason = JobCanceled, ReasonNone
		}
		var at time.Time
		err = tx.QueryRow(ctx, `
			UPDATE ci_svc.jobs SET status = $2, exit_code = $3, failure_reason = $4,
				finished_at = now(), lease_expires_at = NULL, updated_at = now()
			WHERE id = $1 RETURNING finished_at`,
			in.JobID, string(status), in.ExitCode, string(reason)).Scan(&at)
		if err != nil {
			return fmt.Errorf("finish job: %w", err)
		}
		if err := jobFinished(ctx, tx, in.JobID, j.pipelineID, j.repositoryID, j.name, status, reason, at); err != nil {
			return err
		}
		finished = true
		queued, err = advance(ctx, tx, j.pipelineID)
		return err
	})
	if err != nil {
		return false, err
	}
	if finished {
		s.hub.Publish(ctx, notify.LogTopic(in.JobID)) // followers can end their streams
	}
	if queued {
		s.hub.Publish(ctx, notify.JobsTopic)
	}
	return cancelRequested, nil
}

// reapBatch is how many expired leases one ReapExpired call handles.
const reapBatch = 50

// ReapExpired finishes running jobs whose lease ran out: CANCELED if they
// were being canceled, otherwise FAILED with reason runner_lost. It
// returns how many jobs it finished.
func (s *Service) ReapExpired(ctx context.Context) (int, error) {
	type expired struct {
		id, pipelineID, repositoryID uuid.UUID
		name                         string
		cancelRequested              bool
	}
	var jobs []expired
	var queued bool
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		jobs, queued = nil, false
		rows, err := tx.Query(ctx, `
			SELECT j.id, j.pipeline_id, p.repository_id, j.name, j.cancel_requested
			FROM ci_svc.jobs j JOIN ci_svc.pipelines p ON p.id = j.pipeline_id
			WHERE j.status = 'running' AND j.lease_expires_at < now()
			ORDER BY j.lease_expires_at LIMIT $1
			FOR UPDATE OF j SKIP LOCKED`, reapBatch)
		if err != nil {
			return fmt.Errorf("find expired leases: %w", err)
		}
		jobs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (expired, error) {
			var e expired
			err := row.Scan(&e.id, &e.pipelineID, &e.repositoryID, &e.name, &e.cancelRequested)
			return e, err
		})
		if err != nil {
			return fmt.Errorf("find expired leases: %w", err)
		}
		pipelines := map[uuid.UUID]bool{}
		for _, e := range jobs {
			status, reason := JobFailed, ReasonRunnerLost
			if e.cancelRequested {
				status, reason = JobCanceled, ReasonNone
			}
			var at time.Time
			err := tx.QueryRow(ctx, `
				UPDATE ci_svc.jobs SET status = $2, failure_reason = $3,
					finished_at = now(), lease_expires_at = NULL, updated_at = now()
				WHERE id = $1 RETURNING finished_at`, e.id, string(status), string(reason)).Scan(&at)
			if err != nil {
				return fmt.Errorf("fail expired job: %w", err)
			}
			if err := jobFinished(ctx, tx, e.id, e.pipelineID, e.repositoryID, e.name, status, reason, at); err != nil {
				return err
			}
			pipelines[e.pipelineID] = true
		}
		for id := range pipelines {
			q, err := advance(ctx, tx, id)
			if err != nil {
				return err
			}
			queued = queued || q
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, e := range jobs {
		s.hub.Publish(ctx, notify.LogTopic(e.id))
	}
	if queued {
		s.hub.Publish(ctx, notify.JobsTopic)
	}
	return len(jobs), nil
}

// RunReaper calls ReapExpired every interval until ctx ends. Every
// replica runs it; SKIP LOCKED keeps them from doing the same work twice.
func (s *Service) RunReaper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for {
			n, err := s.ReapExpired(ctx)
			if err != nil {
				if ctx.Err() == nil {
					s.log.ErrorContext(ctx, "reap expired leases", slog.String("err", err.Error()))
				}
				break
			}
			if n > 0 {
				s.log.InfoContext(ctx, "jobs lost their runner", slog.Int("jobs", n))
			}
			if n < reapBatch {
				break
			}
		}
	}
}
