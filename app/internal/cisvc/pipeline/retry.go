package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/db"
	"github.com/git2-project/git2-runner/app/internal/cisvc/notify"
)

// RetryInput is a request to run a finished job again.
type RetryInput struct {
	JobID          string
	IdempotencyKey string
	TriggeredByID  string
}

// Errors of Retry that a client can act on.
var (
	errRetryRunning = apierr.New(apierr.FailedPrecondition,
		"the pipeline has not finished; wait for it or cancel it first")
	errRetryNotNewest = apierr.New(apierr.FailedPrecondition,
		"a newer attempt of this job exists; retry that one")
	errRetryNotFinished = apierr.New(apierr.FailedPrecondition,
		"only a job that succeeded, failed or was canceled can be retried")
	errRetryEarlierStage = apierr.New(apierr.FailedPrecondition,
		"a job of an earlier stage did not succeed; retry that one first")
	errRetryKeyUsed = apierr.New(apierr.AlreadyExists,
		"idempotency_key was used before to retry another job")
)

// Retry runs a finished job of a finished pipeline again, as a new attempt
// in the same pipeline (issue #16). The job must be the newest attempt of
// its name, must have succeeded, failed or been canceled, and every job of
// the stages before it must have succeeded.
//
// The new attempt copies the job's spec. Jobs of later stages that never
// ran (skipped, or canceled before they started) get new attempts too, so
// the pipeline can go on past the retried job. Earlier attempts keep their
// status and log. The pipeline is no longer final and publishes
// PipelineFinished again when it ends.
//
// The same idempotency key returns the attempt the first call created,
// with created false. It reports the new attempt and its pipeline.
func (s *Service) Retry(ctx context.Context, in RetryInput) (Job, Pipeline, bool, error) {
	jobID, err := apierr.ParseID("job_id", in.JobID)
	if err != nil {
		return Job{}, Pipeline{}, false, err
	}
	var triggeredBy uuid.NullUUID
	if in.TriggeredByID != "" {
		id, err := apierr.ParseID("triggered_by_id", in.TriggeredByID)
		if err != nil {
			return Job{}, Pipeline{}, false, err
		}
		triggeredBy = uuid.NullUUID{UUID: id, Valid: true}
	}
	if !validKey(in.IdempotencyKey) {
		return Job{}, Pipeline{}, false, apierr.InvalidField("idempotency_key",
			"must be 1 to 512 bytes of UTF-8 text without control characters")
	}

	var (
		newID      uuid.UUID
		pipelineID uuid.UUID
		created    bool
		queued     bool
	)
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		created, queued = false, false
		err := tx.QueryRow(ctx, `SELECT pipeline_id FROM ci_svc.jobs WHERE id = $1`, jobID).Scan(&pipelineID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errJobNotFound
		}
		if err != nil {
			return fmt.Errorf("find job: %w", err)
		}
		// Lock order (see the package comment): the pipeline's jobs in stage
		// order, then the pipeline, which advance locks.
		if _, err := tx.Exec(ctx, `SELECT id FROM ci_svc.jobs WHERE pipeline_id = $1 ORDER BY stage_index, id FOR UPDATE`, pipelineID); err != nil {
			return fmt.Errorf("lock jobs: %w", err)
		}

		// Under the locks, so a concurrent call with the same key is seen.
		var retryOf uuid.NullUUID
		err = tx.QueryRow(ctx, `SELECT id, retry_of FROM ci_svc.jobs WHERE idempotency_key = $1`, in.IdempotencyKey).
			Scan(&newID, &retryOf)
		switch {
		case err == nil && retryOf.Valid && retryOf.UUID == jobID:
			return nil
		case err == nil:
			return errRetryKeyUsed
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("find retry: %w", err)
		}

		var status Status
		if err := tx.QueryRow(ctx, `SELECT status FROM ci_svc.pipelines WHERE id = $1`, pipelineID).Scan(&status); err != nil {
			return fmt.Errorf("read pipeline: %w", err)
		}
		if !status.Final() {
			return errRetryRunning
		}

		jobs, err := retryCandidates(ctx, tx, pipelineID)
		if err != nil {
			return err
		}
		target := -1
		for i, j := range jobs {
			if j.id == jobID {
				target = i
			}
		}
		if target < 0 {
			return errRetryNotNewest
		}
		t := jobs[target]
		if t.status != JobSuccess && t.status != JobFailed && t.status != JobCanceled {
			return errRetryNotFinished
		}
		var again []retryJob
		for _, j := range jobs {
			switch {
			case j.stageIndex < t.stageIndex && j.status != JobSuccess:
				return errRetryEarlierStage
			case j.id == t.id:
				again = append(again, j)
			case j.stageIndex > t.stageIndex && !j.started:
				again = append(again, j)
			}
		}

		for _, j := range again {
			id := uuid.Must(uuid.NewV7())
			key := textOrNull(in.IdempotencyKey, j.id == t.id)
			// The new attempt waits as created; advance queues it with its
			// stage. spec and tags are copied as they are.
			if _, err := tx.Exec(ctx, `
				INSERT INTO ci_svc.jobs (id, pipeline_id, stage, stage_index, name, spec, tags, status,
					attempt, retry_of, idempotency_key, triggered_by)
				SELECT $1, pipeline_id, stage, stage_index, name, spec, tags, 'created',
					attempt + 1, id, $3, $4
				FROM ci_svc.jobs WHERE id = $2`, id, j.id, key, triggeredBy); err != nil {
				return fmt.Errorf("insert attempt: %w", err)
			}
			if j.id == t.id {
				newID = id
			}
		}
		// The pipeline runs again. advance sets its real status (PENDING or
		// RUNNING) and clears finished_at.
		if _, err := tx.Exec(ctx, `
			UPDATE ci_svc.pipelines SET status = 'running', finished_at = NULL, updated_at = now()
			WHERE id = $1`, pipelineID); err != nil {
			return fmt.Errorf("reopen pipeline: %w", err)
		}
		if queued, err = advance(ctx, tx, pipelineID); err != nil {
			return err
		}
		created = true
		return nil
	})
	if db.IsUniqueViolation(err, "jobs_idempotency_key_key") {
		// Another call with the same key won the race between our two
		// transactions; it holds the answer.
		return s.Retry(ctx, in)
	}
	if err != nil {
		return Job{}, Pipeline{}, false, err
	}
	if queued {
		s.hub.Publish(ctx, notify.JobsTopic)
	}
	j, err := s.GetJob(ctx, newID)
	if err != nil {
		return Job{}, Pipeline{}, false, err
	}
	p, err := s.Get(ctx, pipelineID)
	if err != nil {
		return Job{}, Pipeline{}, false, err
	}
	return j, p, created, nil
}

// retryJob is what Retry needs to know about the newest attempt of a job.
type retryJob struct {
	id         uuid.UUID
	stageIndex int
	status     JobStatus
	started    bool
}

// retryCandidates returns the newest attempt of every job of a pipeline.
func retryCandidates(ctx context.Context, tx pgx.Tx, pipelineID uuid.UUID) ([]retryJob, error) {
	rows, err := tx.Query(ctx, `
		SELECT j.id, j.stage_index, j.status, j.started_at IS NOT NULL FROM ci_svc.jobs j
		WHERE j.pipeline_id = $1 AND `+newestAttempt+` ORDER BY j.stage_index, j.name`, pipelineID)
	if err != nil {
		return nil, fmt.Errorf("read jobs: %w", err)
	}
	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (retryJob, error) {
		var j retryJob
		err := row.Scan(&j.id, &j.stageIndex, &j.status, &j.started)
		return j, err
	})
	if err != nil {
		return nil, fmt.Errorf("read jobs: %w", err)
	}
	return jobs, nil
}

// textOrNull returns s when set is true and NULL otherwise.
func textOrNull(s string, set bool) *string {
	if !set {
		return nil
	}
	return &s
}
