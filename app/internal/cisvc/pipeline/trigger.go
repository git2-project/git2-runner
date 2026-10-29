package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/db"
	"github.com/git2-project/git2-runner/app/internal/cisvc/notify"
	"github.com/git2-project/git2-runner/app/internal/cisvc/outbox"
	"github.com/git2-project/git2-runner/app/internal/cisvc/source"
	"github.com/git2-project/git2-runner/app/internal/ciyaml"
)

var (
	// refRE accepts branch and tag refs. Git's own rules are stricter; this
	// only keeps garbage and control characters out.
	refRE = regexp.MustCompile(`^refs/(heads|tags)/[^\x00-\x20\x7f~^:?*\[\\]+$`)
	// shaRE accepts SHA-1 and SHA-256 object IDs.
	shaRE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	// keyRE accepts printable ASCII.
	keyRE = regexp.MustCompile(`^[\x20-\x7e]{1,200}$`)
)

// maxConfigErrors is how many .git2-ci.yml errors a pipeline's
// error_message lists.
const maxConfigErrors = 10

// TriggerInput is a request to start a pipeline.
type TriggerInput struct {
	RepositoryID   string
	Ref            string
	CommitSHA      string
	Trigger        Trigger
	TriggeredByID  string
	IdempotencyKey string
}

type triggerArgs struct {
	repositoryID uuid.UUID
	triggeredBy  uuid.NullUUID
}

func validateTrigger(in TriggerInput) (triggerArgs, error) {
	var a triggerArgs
	var err error
	if a.repositoryID, err = apierr.ParseID("repository_id", in.RepositoryID); err != nil {
		return a, err
	}
	if len(in.Ref) > 255 || !refRE.MatchString(in.Ref) || strings.Contains(in.Ref, "..") {
		return a, apierr.InvalidField("ref", `must be a full branch or tag name such as "refs/heads/main", at most 255 bytes`)
	}
	if !shaRE.MatchString(in.CommitSHA) {
		return a, apierr.InvalidField("commit_sha", "must be 40 or 64 lowercase hex characters")
	}
	if in.Trigger != TriggerPush && in.Trigger != TriggerManual {
		return a, apierr.InvalidField("trigger", "is required")
	}
	if in.TriggeredByID != "" {
		id, err := apierr.ParseID("triggered_by_id", in.TriggeredByID)
		if err != nil {
			return a, err
		}
		a.triggeredBy = uuid.NullUUID{UUID: id, Valid: true}
	}
	if !keyRE.MatchString(in.IdempotencyKey) {
		return a, apierr.InvalidField("idempotency_key", "must be 1 to 200 printable ASCII characters")
	}
	return a, nil
}

// Trigger starts a pipeline, or returns the one already started with the
// same idempotency key. It reports whether it created the pipeline.
func (s *Service) Trigger(ctx context.Context, in TriggerInput) (Pipeline, bool, error) {
	args, err := validateTrigger(in)
	if err != nil {
		return Pipeline{}, false, err
	}
	if p, found, err := s.existing(ctx, in, args); found || err != nil {
		return p, false, err
	}

	content, err := s.source.ReadCIFile(ctx, args.repositoryID, in.CommitSHA)
	switch {
	case errors.Is(err, source.ErrNoCIFile):
		return Pipeline{}, false, apierr.New(apierr.NotFound, "the commit has no "+ciyaml.FileName)
	case err != nil:
		s.log.WarnContext(ctx, "read CI file", "err", err.Error())
		return Pipeline{}, false, apierr.New(apierr.Unavailable, "could not read "+ciyaml.FileName+" from git-svc; retry later")
	}
	cfg, parseErr := ciyaml.Parse(content)

	id := uuid.Must(uuid.NewV7())
	var created, queued bool
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		created, queued = false, false
		status, errMsg := StatusPending, ""
		if parseErr != nil {
			status, errMsg = StatusFailed, configErrorMessage(parseErr)
		}
		var createdAt time.Time
		var finishedAt *time.Time
		err := tx.QueryRow(ctx, `
			INSERT INTO ci_svc.pipelines
				(id, repository_id, ref, commit_sha, trigger, triggered_by, idempotency_key, status, error_message, finished_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, CASE WHEN $8 = 'failed' THEN now() END)
			ON CONFLICT (idempotency_key) DO NOTHING
			RETURNING created_at, finished_at`,
			id, args.repositoryID, in.Ref, in.CommitSHA, string(in.Trigger), args.triggeredBy,
			in.IdempotencyKey, string(status), errMsg).Scan(&createdAt, &finishedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // a concurrent call with the same key won
		}
		if err != nil {
			return fmt.Errorf("insert pipeline: %w", err)
		}
		created = true

		if err := outbox.Write(ctx, tx, outbox.AggregatePipeline, id, &civ1.PipelineCreated{
			PipelineId: id.String(), RepositoryId: args.repositoryID.String(), Ref: in.Ref,
			CommitSha: in.CommitSHA, Trigger: in.Trigger.Proto(), OccurredAt: timestamppb.New(createdAt),
		}); err != nil {
			return err
		}
		if parseErr != nil {
			return outbox.Write(ctx, tx, outbox.AggregatePipeline, id, &civ1.PipelineFinished{
				PipelineId: id.String(), RepositoryId: args.repositoryID.String(), Ref: in.Ref,
				CommitSha: in.CommitSHA, Status: StatusFailed.Proto(), OccurredAt: timestamppb.New(*finishedAt),
			})
		}
		if err := insertJobs(ctx, tx, id, cfg); err != nil {
			return err
		}
		queued, err = advance(ctx, tx, id)
		return err
	})
	if err != nil {
		return Pipeline{}, false, err
	}
	if !created {
		p, _, err := s.existing(ctx, in, args)
		return p, false, err
	}
	if queued {
		s.hub.Publish(ctx, notify.JobsTopic)
	}
	p, err := s.Get(ctx, id)
	return p, true, err
}

// existing returns the pipeline already created with the input's
// idempotency key, if there is one.
func (s *Service) existing(ctx context.Context, in TriggerInput, args triggerArgs) (Pipeline, bool, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM ci_svc.pipelines WHERE idempotency_key = $1`, in.IdempotencyKey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pipeline{}, false, nil
	}
	if err != nil {
		return Pipeline{}, false, fmt.Errorf("find pipeline by key: %w", err)
	}
	p, err := s.Get(ctx, id)
	if err != nil {
		return Pipeline{}, false, err
	}
	if p.RepositoryID != args.repositoryID || p.Ref != in.Ref || p.CommitSHA != in.CommitSHA {
		return Pipeline{}, true, apierr.New(apierr.AlreadyExists,
			"idempotency_key was already used for another repository, ref or commit")
	}
	return p, true, nil
}

// configErrorMessage turns parse errors into a pipeline's error_message.
func configErrorMessage(err error) string {
	var errs ciyaml.Errors
	if !errors.As(err, &errs) {
		return ciyaml.FileName + " is invalid"
	}
	var lines []string
	for i, e := range errs {
		if i == maxConfigErrors {
			lines = append(lines, fmt.Sprintf("and %d more", len(errs)-i))
			break
		}
		lines = append(lines, e.Error())
	}
	return ciyaml.FileName + " is invalid: " + strings.Join(lines, "; ")
}

// insertJobs creates the jobs of a valid file, all in status created.
// advance then queues the first stage.
func insertJobs(ctx context.Context, tx pgx.Tx, pipelineID uuid.UUID, cfg *ciyaml.Config) error {
	for _, j := range cfg.Jobs {
		spec, err := json.Marshal(specOf(cfg, j))
		if err != nil {
			return fmt.Errorf("encode job spec: %w", err)
		}
		tags := j.Tags
		if tags == nil {
			tags = []string{}
		}
		// The spec is sent as text and cast, so it also works in pgx's exec
		// mode (see package db).
		_, err = tx.Exec(ctx, `
			INSERT INTO ci_svc.jobs (id, pipeline_id, stage, stage_index, name, spec, tags, status)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::text[], 'created')`,
			uuid.Must(uuid.NewV7()), pipelineID, j.Stage, j.StageIndex, j.Name, string(spec), tags)
		if err != nil {
			return fmt.Errorf("insert job: %w", err)
		}
	}
	return nil
}

// specOf builds a job's spec. The file's variables come first and the
// job's own override them; each name appears once.
func specOf(cfg *ciyaml.Config, j ciyaml.Job) Spec {
	spec := Spec{
		Image:          j.Image,
		Script:         j.Script,
		TimeoutSeconds: int64(j.Timeout / time.Second),
	}
	index := map[string]int{}
	for _, v := range append(append([]ciyaml.Variable{}, cfg.Variables...), j.Variables...) {
		if i, ok := index[v.Name]; ok {
			spec.Variables[i].Value = v.Value
			continue
		}
		index[v.Name] = len(spec.Variables)
		spec.Variables = append(spec.Variables, Variable{Name: v.Name, Value: v.Value})
	}
	if a := j.Artifacts; a != nil {
		spec.Artifacts = &PathsSpec{Paths: a.Paths, ExpireInSeconds: int64(a.ExpireIn / time.Second)}
	}
	if c := j.Cache; c != nil {
		spec.Cache = &PathsSpec{Key: c.Key, Paths: c.Paths}
	}
	return spec
}
