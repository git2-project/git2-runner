// Package pipeline turns a push into a pipeline of jobs, queues the jobs
// for runners, and tracks their status and logs. It knows nothing about
// gRPC.
//
// PostgreSQL is the source of truth and the queue (ADR 0003, ADR 0006):
//
//   - A job is created, then pending (queued), then running (leased by one
//     runner), then final. Jobs of a stage are queued when every job of the
//     stage before succeeded; after a failure, later stages are skipped.
//   - A runner leases a pending job with FOR UPDATE SKIP LOCKED, so two
//     runners never get the same job. A lease lasts Options.Lease and is
//     extended by every heartbeat. The reaper fails jobs whose lease ran
//     out as runner_lost, which also recovers jobs that were running when
//     the instance was backed up.
//   - Signals (package notify) wake waiting runners and log followers. They
//     are an optimization: every waiter also polls on a timer.
//
// Lock order, to keep deadlocks rare: job rows first, then the pipeline
// row. Transactions run through db.InTx, which retries after a deadlock.
package pipeline

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/db"
	"github.com/git2-project/git2-runner/app/internal/cisvc/secrets"
	"github.com/git2-project/git2-runner/app/internal/cisvc/source"
)

// Status is the state of a pipeline, as stored in ci_svc.pipelines.status.
type Status string

// Pipeline statuses.
const (
	StatusPending  Status = "pending"
	StatusRunning  Status = "running"
	StatusSuccess  Status = "success"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
)

// Final reports whether the status never changes again.
func (s Status) Final() bool {
	return s == StatusSuccess || s == StatusFailed || s == StatusCanceled
}

// Proto returns the ci.v1 enum value.
func (s Status) Proto() civ1.PipelineStatus {
	switch s {
	case StatusPending:
		return civ1.PipelineStatus_PIPELINE_STATUS_PENDING
	case StatusRunning:
		return civ1.PipelineStatus_PIPELINE_STATUS_RUNNING
	case StatusSuccess:
		return civ1.PipelineStatus_PIPELINE_STATUS_SUCCESS
	case StatusFailed:
		return civ1.PipelineStatus_PIPELINE_STATUS_FAILED
	case StatusCanceled:
		return civ1.PipelineStatus_PIPELINE_STATUS_CANCELED
	}
	return civ1.PipelineStatus_PIPELINE_STATUS_UNSPECIFIED
}

// JobStatus is the state of a job, as stored in ci_svc.jobs.status.
type JobStatus string

// Job statuses.
const (
	JobCreated  JobStatus = "created"
	JobPending  JobStatus = "pending"
	JobRunning  JobStatus = "running"
	JobSuccess  JobStatus = "success"
	JobFailed   JobStatus = "failed"
	JobCanceled JobStatus = "canceled"
	JobSkipped  JobStatus = "skipped"
)

// Final reports whether the status never changes again.
func (s JobStatus) Final() bool {
	return s == JobSuccess || s == JobFailed || s == JobCanceled || s == JobSkipped
}

// Proto returns the ci.v1 enum value.
func (s JobStatus) Proto() civ1.JobStatus {
	switch s {
	case JobCreated:
		return civ1.JobStatus_JOB_STATUS_CREATED
	case JobPending:
		return civ1.JobStatus_JOB_STATUS_PENDING
	case JobRunning:
		return civ1.JobStatus_JOB_STATUS_RUNNING
	case JobSuccess:
		return civ1.JobStatus_JOB_STATUS_SUCCESS
	case JobFailed:
		return civ1.JobStatus_JOB_STATUS_FAILED
	case JobCanceled:
		return civ1.JobStatus_JOB_STATUS_CANCELED
	case JobSkipped:
		return civ1.JobStatus_JOB_STATUS_SKIPPED
	}
	return civ1.JobStatus_JOB_STATUS_UNSPECIFIED
}

// FailureReason says why a job failed, as stored in
// ci_svc.jobs.failure_reason. Empty unless the job failed.
type FailureReason string

// Failure reasons.
const (
	ReasonNone          FailureReason = ""
	ReasonScriptFailure FailureReason = "script_failure"
	ReasonTimeout       FailureReason = "timeout"
	ReasonRunnerLost    FailureReason = "runner_lost"
	ReasonImagePull     FailureReason = "image_pull_failure"
	ReasonRunnerSystem  FailureReason = "runner_system_failure"
)

var reasonToProto = map[FailureReason]civ1.JobFailureReason{
	ReasonNone:          civ1.JobFailureReason_JOB_FAILURE_REASON_UNSPECIFIED,
	ReasonScriptFailure: civ1.JobFailureReason_JOB_FAILURE_REASON_SCRIPT_FAILURE,
	ReasonTimeout:       civ1.JobFailureReason_JOB_FAILURE_REASON_TIMEOUT,
	ReasonRunnerLost:    civ1.JobFailureReason_JOB_FAILURE_REASON_RUNNER_LOST,
	ReasonImagePull:     civ1.JobFailureReason_JOB_FAILURE_REASON_IMAGE_PULL_FAILURE,
	ReasonRunnerSystem:  civ1.JobFailureReason_JOB_FAILURE_REASON_RUNNER_SYSTEM_FAILURE,
}

// Proto returns the ci.v1 enum value.
func (r FailureReason) Proto() civ1.JobFailureReason { return reasonToProto[r] }

// ReasonFromProto returns the FailureReason for a ci.v1 enum value, and
// false for an unknown one.
func ReasonFromProto(p civ1.JobFailureReason) (FailureReason, bool) {
	for r, v := range reasonToProto {
		if v == p {
			return r, true
		}
	}
	return "", false
}

// Trigger says what started a pipeline.
type Trigger string

// Triggers.
const (
	TriggerPush   Trigger = "push"
	TriggerManual Trigger = "manual"
)

// Proto returns the ci.v1 enum value.
func (t Trigger) Proto() civ1.PipelineTrigger {
	switch t {
	case TriggerPush:
		return civ1.PipelineTrigger_PIPELINE_TRIGGER_PUSH
	case TriggerManual:
		return civ1.PipelineTrigger_PIPELINE_TRIGGER_MANUAL
	}
	return civ1.PipelineTrigger_PIPELINE_TRIGGER_UNSPECIFIED
}

// Pipeline is one run of .git2-ci.yml for one commit.
type Pipeline struct {
	ID             uuid.UUID
	RepositoryID   uuid.UUID
	Ref            string
	CommitSHA      string
	Trigger        Trigger
	TriggeredBy    uuid.NullUUID
	IdempotencyKey string
	Status         Status
	ErrorMessage   string
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	UpdatedAt      time.Time
	// Jobs sorted by stage, then name.
	Jobs []Job
}

// Job is one job of a pipeline.
type Job struct {
	ID            uuid.UUID
	PipelineID    uuid.UUID
	Stage         string
	StageIndex    int
	Name          string
	Image         string
	Status        JobStatus
	FailureReason FailureReason
	ExitCode      *int32
	RunnerID      uuid.NullUUID
	LogSize       int64
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

// Variable is one environment variable of a job.
type Variable struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Masked bool   `json:"masked,omitempty"`
}

// Spec is what ci_svc.jobs.spec holds: the job as .git2-ci.yml defines it,
// without secrets, which are added when the job is leased.
type Spec struct {
	Image          string     `json:"image"`
	Script         []string   `json:"script"`
	Variables      []Variable `json:"variables,omitempty"`
	TimeoutSeconds int64      `json:"timeout_seconds"`
	Artifacts      *PathsSpec `json:"artifacts,omitempty"`
	Cache          *PathsSpec `json:"cache,omitempty"`
}

// PathsSpec is the artifacts or cache part of a Spec.
type PathsSpec struct {
	Key             string   `json:"key,omitempty"`
	Paths           []string `json:"paths"`
	ExpireInSeconds int64    `json:"expire_in_seconds,omitempty"`
}

// Assignment is a leased job: everything a runner needs to run it.
type Assignment struct {
	JobID        uuid.UUID
	PipelineID   uuid.UUID
	RepositoryID uuid.UUID
	Ref          string
	CommitSHA    string
	Name         string
	Stage        string
	Image        string
	Script       []string
	// Variables in order; a later one with the same name wins. Secrets
	// come last and are Masked.
	Variables []Variable
	Timeout   time.Duration
}

// SecretReader returns the decrypted secrets for a repository's jobs.
type SecretReader interface {
	ForRepository(ctx context.Context, q db.Querier, repositoryID uuid.UUID) ([]secrets.Variable, error)
}

// Notifier carries wake-up signals (package notify).
type Notifier interface {
	Publish(ctx context.Context, topic string)
	Subscribe(topic string) (<-chan struct{}, func())
}

// Options tune the queue and the log. DefaultOptions has the production
// values; tests shorten them.
type Options struct {
	// Lease is how long a runner holds a job without a heartbeat.
	Lease time.Duration
	// LongPoll is how long RequestJob waits for a job.
	LongPoll time.Duration
	// JobPoll is how often a waiting RequestJob looks at the queue when no
	// signal arrives.
	JobPoll time.Duration
	// LogPoll is how often a log follower reads the database when no signal
	// arrives.
	LogPoll time.Duration
	// MaxLogSize is the size at which a job's log is truncated.
	MaxLogSize int64
}

// DefaultOptions returns the production values.
func DefaultOptions() Options {
	return Options{
		Lease:      60 * time.Second,
		LongPoll:   25 * time.Second,
		JobPoll:    3 * time.Second,
		LogPoll:    time.Second,
		MaxLogSize: 4 << 20,
	}
}

// Service is the pipeline engine.
type Service struct {
	pool    *pgxpool.Pool
	source  source.Source
	secrets SecretReader
	hub     Notifier
	log     *slog.Logger
	opts    Options
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool, src source.Source, sec SecretReader, hub Notifier, log *slog.Logger, opts Options) *Service {
	return &Service{pool: pool, source: src, secrets: sec, hub: hub, log: log, opts: opts}
}
