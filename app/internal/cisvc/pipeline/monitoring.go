package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/git2-project/git2-runner/app/internal/cisvc/page"
)

// RecordContainerStats stores the latest resource use of a running job's
// container (ci.v1.ContainerStats as JSON), sent by the runner that runs it
// with a heartbeat. A sample for a job that is not running, or not this
// runner's, is dropped: the heartbeat itself already answered for that.
func (s *Service) RecordContainerStats(ctx context.Context, runnerID, jobID uuid.UUID, stats string) error {
	// Sent as text and cast, so it also works in pgx's exec mode.
	_, err := s.pool.Exec(ctx, `
		UPDATE ci_svc.jobs SET container_stats = $3::jsonb, container_stats_at = now()
		WHERE id = $1 AND runner_id = $2 AND status = 'running'`, jobID, runnerID, stats)
	if err != nil {
		return fmt.Errorf("record container stats: %w", err)
	}
	return nil
}

// RunningJob is one job a runner is running now.
type RunningJob struct {
	JobID        uuid.UUID
	Name         string
	Stage        string
	Attempt      int32
	PipelineID   uuid.UUID
	RepositoryID uuid.UUID
	Ref          string
	CommitSHA    string
	RunnerID     uuid.UUID
	RunnerName   string
	StartedAt    time.Time
	// Stats is the container's latest sample as JSON (ci.v1.ContainerStats),
	// and StatsAt when ci-svc received it. Nil before the first sample.
	Stats   *string
	StatsAt *time.Time
}

// runningCursor is the position after the last job of a page.
type runningCursor struct {
	StartedAt time.Time `json:"s"`
	ID        uuid.UUID `json:"i"`
}

// ListRunning returns one page of the running jobs, oldest start first,
// optionally only those of one runner.
func (s *Service) ListRunning(ctx context.Context, runnerID uuid.NullUUID, pageSize int32, pageToken string) ([]RunningJob, string, error) {
	size, err := page.Size(pageSize, 50)
	if err != nil {
		return nil, "", err
	}
	var c runningCursor
	after, err := page.Decode(pageToken, &c)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT j.id, j.name, j.stage, j.attempt, p.id, p.repository_id, p.ref, p.commit_sha,
		       r.id, r.name, j.started_at, j.container_stats::text, j.container_stats_at
		FROM ci_svc.jobs j
		JOIN ci_svc.pipelines p ON p.id = j.pipeline_id
		JOIN ci_svc.runners r ON r.id = j.runner_id
		WHERE j.status = 'running' AND j.started_at IS NOT NULL
		  AND ($1::uuid IS NULL OR j.runner_id = $1)
		  AND (NOT $2 OR (j.started_at, j.id) > ($3, $4))
		ORDER BY j.started_at, j.id
		LIMIT $5`, runnerID, after, c.StartedAt, c.ID, size+1)
	if err != nil {
		return nil, "", fmt.Errorf("list running jobs: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RunningJob, error) {
		var j RunningJob
		err := r.Scan(&j.JobID, &j.Name, &j.Stage, &j.Attempt, &j.PipelineID, &j.RepositoryID, &j.Ref, &j.CommitSHA,
			&j.RunnerID, &j.RunnerName, &j.StartedAt, &j.Stats, &j.StatsAt)
		return j, err
	})
	if err != nil {
		return nil, "", fmt.Errorf("list running jobs: %w", err)
	}
	next := ""
	if len(list) > size {
		list = list[:size]
		last := list[size-1]
		next = page.Encode(runningCursor{StartedAt: last.StartedAt, ID: last.JobID})
	}
	return list, next, nil
}

// Overview is the instance's CI at a glance.
type Overview struct {
	RunnersOnline, RunnersOffline, RunnersPaused int32
	// RunnersDockerUnreachable counts online runners whose latest health
	// report says their Docker daemon did not answer.
	RunnersDockerUnreachable int32
	JobsPending, JobsRunning int32
	// OldestPendingSince is when the job that has waited longest was
	// queued; nil if no job waits.
	OldestPendingSince *time.Time
	// Pipelines counts the pipelines created in the last 24 hours by their
	// status now.
	Pipelines map[Status]int32
}

// Overview counts runners, queued and running jobs, and the last day's
// pipelines. A runner is online if it asked for work within onlineWindow
// (runners.OnlineWindow) and is not paused.
func (s *Service) Overview(ctx context.Context, onlineWindow time.Duration) (Overview, error) {
	o := Overview{Pipelines: map[Status]int32{}}
	// protojson leaves out a false bool, so a report without
	// "dockerReachable" says the daemon did not answer.
	err := s.pool.QueryRow(ctx, `
		WITH r AS (
			SELECT paused,
			       last_seen_at > now() - make_interval(secs => $1::float8) AS online,
			       health IS NOT NULL AND COALESCE((health->>'dockerReachable')::boolean, false) = false AS docker_down
			FROM ci_svc.runners
		)
		SELECT
			(SELECT count(*)::int FROM r WHERE NOT paused AND online IS TRUE),
			(SELECT count(*)::int FROM r WHERE NOT paused AND online IS NOT TRUE),
			(SELECT count(*)::int FROM r WHERE paused),
			(SELECT count(*)::int FROM r WHERE NOT paused AND online IS TRUE AND docker_down),
			(SELECT count(*)::int FROM ci_svc.jobs WHERE status = 'pending'),
			(SELECT count(*)::int FROM ci_svc.jobs WHERE status = 'running'),
			(SELECT min(queued_at) FROM ci_svc.jobs WHERE status = 'pending')`,
		onlineWindow.Seconds()).Scan(&o.RunnersOnline, &o.RunnersOffline, &o.RunnersPaused, &o.RunnersDockerUnreachable,
		&o.JobsPending, &o.JobsRunning, &o.OldestPendingSince)
	if err != nil {
		return Overview{}, fmt.Errorf("overview: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT status, count(*)::int FROM ci_svc.pipelines
		WHERE created_at > now() - interval '24 hours'
		GROUP BY status`)
	if err != nil {
		return Overview{}, fmt.Errorf("overview: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int32
		if err := rows.Scan(&st, &n); err != nil {
			return Overview{}, fmt.Errorf("overview: %w", err)
		}
		o.Pipelines[Status(st)] = n
	}
	if err := rows.Err(); err != nil {
		return Overview{}, fmt.Errorf("overview: %w", err)
	}
	return o, nil
}
