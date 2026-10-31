package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/db"
	"github.com/git2-project/git2-runner/app/internal/cisvc/notify"
	"github.com/git2-project/git2-runner/app/internal/cisvc/outbox"
)

// jobState is what advance needs to know about a job.
type jobState struct {
	id         uuid.UUID
	stageIndex int
	name       string
	status     JobStatus
}

// advance moves a pipeline forward after its jobs changed: it queues the
// next stage when the current one succeeded, skips later stages after a
// failure or cancel, and sets the pipeline's status, writing
// PipelineFinished when it becomes final. It reports whether it queued
// jobs, so the caller can publish a wake-up after commit.
//
// It locks the pipeline row. Callers lock job rows before calling it.
func advance(ctx context.Context, tx pgx.Tx, pipelineID uuid.UUID) (bool, error) {
	var (
		status       Status
		repositoryID uuid.UUID
		ref, sha     string
	)
	err := tx.QueryRow(ctx, `
		SELECT status, repository_id, ref, commit_sha FROM ci_svc.pipelines WHERE id = $1 FOR UPDATE`,
		pipelineID).Scan(&status, &repositoryID, &ref, &sha)
	if err != nil {
		return false, fmt.Errorf("lock pipeline: %w", err)
	}
	if status.Final() {
		return false, nil
	}

	rows, err := tx.Query(ctx, `
		SELECT id, stage_index, name, status FROM ci_svc.jobs
		WHERE pipeline_id = $1 ORDER BY stage_index, name`, pipelineID)
	if err != nil {
		return false, fmt.Errorf("read jobs: %w", err)
	}
	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (jobState, error) {
		var j jobState
		err := row.Scan(&j.id, &j.stageIndex, &j.name, &j.status)
		return j, err
	})
	if err != nil {
		return false, fmt.Errorf("read jobs: %w", err)
	}

	queued := false
	blocked := false // an earlier stage failed or was canceled
stages:
	for start := 0; start < len(jobs); {
		end := start
		for end < len(jobs) && jobs[end].stageIndex == jobs[start].stageIndex {
			end++
		}
		stage := jobs[start:end]
		start = end

		switch {
		case blocked:
			for i := range stage {
				if stage[i].status == JobCreated {
					if err := skipJob(ctx, tx, stage[i], pipelineID, repositoryID); err != nil {
						return false, err
					}
					stage[i].status = JobSkipped
				}
			}
		case allFinal(stage):
			for _, j := range stage {
				if j.status != JobSuccess {
					blocked = true
				}
			}
		default:
			// The current stage. Queue whatever of it still waits; later
			// stages wait for it.
			for i := range stage {
				if stage[i].status != JobCreated {
					continue
				}
				if _, err := tx.Exec(ctx, `
					UPDATE ci_svc.jobs SET status = 'pending', queued_at = now(), updated_at = now()
					WHERE id = $1`, stage[i].id); err != nil {
					return false, fmt.Errorf("queue job: %w", err)
				}
				stage[i].status = JobPending
				queued = true
			}
			break stages
		}
	}

	// started_at is set by the lease of the first job, not here: a
	// pipeline canceled before any job ran never started.
	next := pipelineStatus(jobs)
	var finishedAt *time.Time
	err = tx.QueryRow(ctx, `
		UPDATE ci_svc.pipelines SET
			status = $2,
			finished_at = CASE WHEN $3 THEN now() END,
			updated_at = now()
		WHERE id = $1
		RETURNING finished_at`, pipelineID, string(next), next.Final()).Scan(&finishedAt)
	if err != nil {
		return false, fmt.Errorf("update pipeline: %w", err)
	}
	if next.Final() {
		err := outbox.Write(ctx, tx, outbox.AggregatePipeline, pipelineID, &civ1.PipelineFinished{
			PipelineId: pipelineID.String(), RepositoryId: repositoryID.String(), Ref: ref,
			CommitSha: sha, Status: next.Proto(), OccurredAt: timestamppb.New(*finishedAt),
		})
		if err != nil {
			return false, err
		}
	}
	return queued, nil
}

func allFinal(jobs []jobState) bool {
	for _, j := range jobs {
		if !j.status.Final() {
			return false
		}
	}
	return true
}

// pipelineStatus derives a pipeline's status from its jobs.
func pipelineStatus(jobs []jobState) Status {
	var failed, canceled, started bool
	for _, j := range jobs {
		switch j.status {
		case JobFailed:
			failed = true
		case JobCanceled:
			canceled = true
		}
		if j.status != JobCreated && j.status != JobPending {
			started = true
		}
	}
	switch {
	case allFinal(jobs) && failed:
		return StatusFailed
	case allFinal(jobs) && canceled:
		return StatusCanceled
	case allFinal(jobs):
		return StatusSuccess
	case started:
		return StatusRunning
	}
	return StatusPending
}

func skipJob(ctx context.Context, tx pgx.Tx, j jobState, pipelineID, repositoryID uuid.UUID) error {
	var at time.Time
	err := tx.QueryRow(ctx, `
		UPDATE ci_svc.jobs SET status = 'skipped', finished_at = now(), updated_at = now()
		WHERE id = $1 RETURNING finished_at`, j.id).Scan(&at)
	if err != nil {
		return fmt.Errorf("skip job: %w", err)
	}
	return jobFinished(ctx, tx, j.id, pipelineID, repositoryID, j.name, JobSkipped, ReasonNone, at)
}

// jobFinished writes the JobFinished event.
func jobFinished(ctx context.Context, tx pgx.Tx, jobID, pipelineID, repositoryID uuid.UUID, name string,
	status JobStatus, reason FailureReason, at time.Time) error {
	return outbox.Write(ctx, tx, outbox.AggregateJob, jobID, &civ1.JobFinished{
		JobId: jobID.String(), PipelineId: pipelineID.String(), RepositoryId: repositoryID.String(),
		Name: name, Status: status.Proto(), FailureReason: reason.Proto(), OccurredAt: timestamppb.New(at),
	})
}

// Cancel cancels every unfinished job of a pipeline. Waiting jobs are
// canceled at once; running ones are flagged, and their runner stops them
// at its next heartbeat. Canceling a finished pipeline changes nothing.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID) (Pipeline, error) {
	p, err := s.Get(ctx, id)
	if err != nil || p.Status.Final() {
		return p, err
	}
	var running []uuid.UUID
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		running = nil
		// Lock the jobs in stage order first, then (in advance) the
		// pipeline. Stage order matters: a job's result holds its own row
		// and then queues later-stage jobs, so locking those first could
		// close a cycle (see the package comment).
		if _, err := tx.Exec(ctx, `SELECT id FROM ci_svc.jobs WHERE pipeline_id = $1 ORDER BY stage_index, id FOR UPDATE`, id); err != nil {
			return fmt.Errorf("lock jobs: %w", err)
		}
		rows, err := tx.Query(ctx, `
			UPDATE ci_svc.jobs SET status = 'canceled', finished_at = now(), updated_at = now()
			WHERE pipeline_id = $1 AND status IN ('created', 'pending')
			RETURNING id, name, finished_at`, id)
		if err != nil {
			return fmt.Errorf("cancel jobs: %w", err)
		}
		type canceled struct {
			id   uuid.UUID
			name string
			at   time.Time
		}
		list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (canceled, error) {
			var c canceled
			err := row.Scan(&c.id, &c.name, &c.at)
			return c, err
		})
		if err != nil {
			return fmt.Errorf("cancel jobs: %w", err)
		}
		for _, c := range list {
			if err := jobFinished(ctx, tx, c.id, id, p.RepositoryID, c.name, JobCanceled, ReasonNone, c.at); err != nil {
				return err
			}
		}
		rows, err = tx.Query(ctx, `
			UPDATE ci_svc.jobs SET cancel_requested = TRUE, updated_at = now()
			WHERE pipeline_id = $1 AND status = 'running' RETURNING id`, id)
		if err != nil {
			return fmt.Errorf("flag running jobs: %w", err)
		}
		if running, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
			return fmt.Errorf("flag running jobs: %w", err)
		}
		_, err = advance(ctx, tx, id)
		return err
	})
	if err != nil {
		return Pipeline{}, err
	}
	for _, jobID := range running {
		s.hub.Publish(ctx, notify.LogTopic(jobID))
	}
	return s.Get(ctx, id)
}

// errPipelineNotFound is returned for an unknown pipeline ID.
var errPipelineNotFound = apierr.New(apierr.NotFound, "pipeline not found")
