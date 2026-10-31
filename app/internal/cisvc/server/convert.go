package server

import (
	"math"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/pipeline"
	"github.com/git2-project/git2-runner/app/internal/cisvc/runners"
	"github.com/git2-project/git2-runner/app/internal/cisvc/secrets"
	"github.com/git2-project/git2-runner/app/internal/ciyaml"
)

// ts converts an optional time; nil stays unset.
func ts(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func pipelineToProto(p pipeline.Pipeline) *civ1.Pipeline {
	out := &civ1.Pipeline{
		Id:           p.ID.String(),
		RepositoryId: p.RepositoryID.String(),
		Ref:          p.Ref,
		CommitSha:    p.CommitSHA,
		Status:       p.Status.Proto(),
		Trigger:      p.Trigger.Proto(),
		ErrorMessage: p.ErrorMessage,
		CreatedAt:    timestamppb.New(p.CreatedAt),
		StartedAt:    ts(p.StartedAt),
		FinishedAt:   ts(p.FinishedAt),
		UpdatedAt:    timestamppb.New(p.UpdatedAt),
	}
	if p.TriggeredBy.Valid {
		out.TriggeredById = p.TriggeredBy.UUID.String()
	}
	// Jobs come sorted by stage, then name: group neighbours.
	for i, j := range p.Jobs {
		if i == 0 || j.StageIndex != p.Jobs[i-1].StageIndex {
			out.Stages = append(out.Stages, &civ1.Stage{Name: j.Stage})
		}
		stage := out.Stages[len(out.Stages)-1]
		stage.Jobs = append(stage.Jobs, jobToProto(j))
	}
	return out
}

func jobToProto(j pipeline.Job) *civ1.Job {
	out := &civ1.Job{
		Id:            j.ID.String(),
		PipelineId:    j.PipelineID.String(),
		Name:          j.Name,
		Stage:         j.Stage,
		Status:        j.Status.Proto(),
		FailureReason: j.FailureReason.Proto(),
		ExitCode:      j.ExitCode,
		Image:         j.Image,
		LogSize:       j.LogSize,
		CreatedAt:     timestamppb.New(j.CreatedAt),
		StartedAt:     ts(j.StartedAt),
		FinishedAt:    ts(j.FinishedAt),
	}
	if j.RunnerID.Valid {
		out.RunnerId = j.RunnerID.UUID.String()
	}
	return out
}

func runnerToProto(r runners.Runner, now time.Time) *civ1.Runner {
	st := civ1.RunnerStatus_RUNNER_STATUS_OFFLINE
	switch {
	case r.Paused:
		st = civ1.RunnerStatus_RUNNER_STATUS_PAUSED
	case r.Online(now):
		st = civ1.RunnerStatus_RUNNER_STATUS_ONLINE
	}
	return &civ1.Runner{
		Id:          r.ID.String(),
		Name:        r.Name,
		Description: r.Description,
		Labels:      r.Labels,
		Executor:    r.Executor,
		Version:     r.Version,
		Platform:    r.Platform,
		Status:      st,
		Paused:      r.Paused,
		LastSeenAt:  ts(r.LastSeenAt),
		CreatedAt:   timestamppb.New(r.CreatedAt),
		UpdatedAt:   timestamppb.New(r.UpdatedAt),
	}
}

func scopeToProto(s secrets.Scope) civ1.SecretScope {
	switch s {
	case secrets.ScopeInstance:
		return civ1.SecretScope_SECRET_SCOPE_INSTANCE
	case secrets.ScopeRepository:
		return civ1.SecretScope_SECRET_SCOPE_REPOSITORY
	}
	return civ1.SecretScope_SECRET_SCOPE_UNSPECIFIED
}

func scopeFromProto(s civ1.SecretScope) secrets.Scope {
	switch s {
	case civ1.SecretScope_SECRET_SCOPE_INSTANCE:
		return secrets.ScopeInstance
	case civ1.SecretScope_SECRET_SCOPE_REPOSITORY:
		return secrets.ScopeRepository
	}
	return ""
}

func secretToProto(s secrets.Secret) *civ1.Secret {
	out := &civ1.Secret{
		Scope:     scopeToProto(s.Scope),
		Name:      s.Name,
		CreatedAt: timestamppb.New(s.CreatedAt),
		UpdatedAt: timestamppb.New(s.UpdatedAt),
	}
	if s.Scope == secrets.ScopeRepository {
		out.ScopeId = s.ScopeID.String()
	}
	return out
}

func assignmentToProto(a *pipeline.Assignment) *civ1.JobAssignment {
	out := &civ1.JobAssignment{
		JobId:        a.JobID.String(),
		PipelineId:   a.PipelineID.String(),
		RepositoryId: a.RepositoryID.String(),
		Ref:          a.Ref,
		CommitSha:    a.CommitSHA,
		Name:         a.Name,
		Stage:        a.Stage,
		Image:        a.Image,
		Script:       a.Script,
		Timeout:      durationpb.New(a.Timeout),
	}
	for _, v := range a.Variables {
		out.Variables = append(out.Variables, &civ1.JobVariable{Name: v.Name, Value: v.Value, Masked: v.Masked})
	}
	return out
}

func configErrorsToProto(errs ciyaml.Errors) []*civ1.ConfigError {
	out := make([]*civ1.ConfigError, len(errs))
	for i, e := range errs {
		out[i] = &civ1.ConfigError{Path: e.Path, Line: clamp32(e.Line), Column: clamp32(e.Column), Message: e.Message}
	}
	return out
}

// clamp32 converts a line or column number; files are at most 256 KiB, so
// the clamp never applies in practice.
func clamp32(n int) int32 {
	return int32(max(0, min(n, math.MaxInt32))) //nolint:gosec // clamped above
}
