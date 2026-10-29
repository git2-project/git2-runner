package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/db"
	"github.com/git2-project/git2-runner/app/internal/cisvc/page"
)

const pipelineColumns = `id, repository_id, ref, commit_sha, trigger, triggered_by, idempotency_key,
	status, error_message, created_at, started_at, finished_at, updated_at`

func scanPipeline(row pgx.Row) (Pipeline, error) {
	var p Pipeline
	err := row.Scan(&p.ID, &p.RepositoryID, &p.Ref, &p.CommitSHA, &p.Trigger, &p.TriggeredBy, &p.IdempotencyKey,
		&p.Status, &p.ErrorMessage, &p.CreatedAt, &p.StartedAt, &p.FinishedAt, &p.UpdatedAt)
	return p, err
}

const jobColumns = `id, pipeline_id, stage, stage_index, name, spec->>'image', status, failure_reason,
	exit_code, runner_id, log_size, created_at, started_at, finished_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.PipelineID, &j.Stage, &j.StageIndex, &j.Name, &j.Image, &j.Status, &j.FailureReason,
		&j.ExitCode, &j.RunnerID, &j.LogSize, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	return j, err
}

func collectJobs(rows pgx.Rows) ([]Job, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Job, error) { return scanJob(row) })
}

// Get returns a pipeline with its jobs.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Pipeline, error) {
	p, err := scanPipeline(s.pool.QueryRow(ctx, `SELECT `+pipelineColumns+` FROM ci_svc.pipelines WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Pipeline{}, errPipelineNotFound
	}
	if err != nil {
		return Pipeline{}, fmt.Errorf("get pipeline: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+jobColumns+` FROM ci_svc.jobs WHERE pipeline_id = $1 ORDER BY stage_index, name`, id)
	if err != nil {
		return Pipeline{}, fmt.Errorf("get jobs: %w", err)
	}
	if p.Jobs, err = collectJobs(rows); err != nil {
		return Pipeline{}, fmt.Errorf("get jobs: %w", err)
	}
	return p, nil
}

// listCursor is the position after the last pipeline of a page, plus the
// filter the token was issued for. Pipeline IDs are UUIDv7, so ID order is
// creation order.
type listCursor struct {
	RepositoryID uuid.UUID `json:"r"`
	Ref          string    `json:"f,omitempty"`
	Before       uuid.UUID `json:"b"`
}

// List returns one page of a repository's pipelines, newest first, each
// with its jobs. ref filters by ref when not empty.
func (s *Service) List(ctx context.Context, repositoryID uuid.UUID, ref string, pageSize int32, pageToken string) ([]Pipeline, string, error) {
	size, err := page.Size(pageSize, 20)
	if err != nil {
		return nil, "", err
	}
	c := listCursor{RepositoryID: repositoryID, Ref: ref, Before: uuid.Max}
	ok, err := page.Decode(pageToken, &c)
	if err != nil {
		return nil, "", err
	}
	if ok && (c.RepositoryID != repositoryID || c.Ref != ref) {
		return nil, "", apierr.InvalidField("page_token", "was issued for another repository_id or ref")
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+pipelineColumns+` FROM ci_svc.pipelines
		WHERE repository_id = $1 AND ($2 = '' OR ref = $2) AND id < $3
		ORDER BY id DESC LIMIT $4`, repositoryID, ref, c.Before, size+1)
	if err != nil {
		return nil, "", fmt.Errorf("list pipelines: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Pipeline, error) { return scanPipeline(row) })
	if err != nil {
		return nil, "", fmt.Errorf("list pipelines: %w", err)
	}
	next := ""
	if len(list) > size {
		list = list[:size]
		next = page.Encode(listCursor{RepositoryID: repositoryID, Ref: ref, Before: list[size-1].ID})
	}
	if err := s.attachJobs(ctx, s.pool, list); err != nil {
		return nil, "", err
	}
	return list, next, nil
}

// attachJobs loads the jobs of several pipelines with one query.
func (s *Service) attachJobs(ctx context.Context, q db.Querier, list []Pipeline) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, len(list))
	index := make(map[uuid.UUID]int, len(list))
	for i, p := range list {
		ids[i] = p.ID.String()
		index[p.ID] = i
	}
	rows, err := q.Query(ctx, `
		SELECT `+jobColumns+` FROM ci_svc.jobs
		WHERE pipeline_id = ANY($1::uuid[]) ORDER BY pipeline_id, stage_index, name`, ids)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	jobs, err := collectJobs(rows)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	for _, j := range jobs {
		i := index[j.PipelineID]
		list[i].Jobs = append(list[i].Jobs, j)
	}
	return nil
}

// errJobNotFound is returned for an unknown job ID.
var errJobNotFound = apierr.New(apierr.NotFound, "job not found")

// GetJob returns a job.
func (s *Service) GetJob(ctx context.Context, id uuid.UUID) (Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM ci_svc.jobs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, errJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	return j, nil
}
