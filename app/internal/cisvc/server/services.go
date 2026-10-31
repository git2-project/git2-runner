package server

import (
	"context"
	"errors"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/pipeline"
	"github.com/git2-project/git2-runner/app/internal/cisvc/runners"
	"github.com/git2-project/git2-runner/app/internal/ciyaml"
)

// pipelineServer implements civ1.PipelineServiceServer.
type pipelineServer struct {
	civ1.UnimplementedPipelineServiceServer
	d Deps
}

func triggerFromProto(t civ1.PipelineTrigger) pipeline.Trigger {
	switch t {
	case civ1.PipelineTrigger_PIPELINE_TRIGGER_PUSH:
		return pipeline.TriggerPush
	case civ1.PipelineTrigger_PIPELINE_TRIGGER_MANUAL:
		return pipeline.TriggerManual
	}
	return ""
}

func (s *pipelineServer) TriggerPipeline(ctx context.Context, req *civ1.TriggerPipelineRequest) (*civ1.TriggerPipelineResponse, error) {
	p, created, err := s.d.Pipelines.Trigger(ctx, pipeline.TriggerInput{
		RepositoryID:   req.GetRepositoryId(),
		Ref:            req.GetRef(),
		CommitSHA:      req.GetCommitSha(),
		Trigger:        triggerFromProto(req.GetTrigger()),
		TriggeredByID:  req.GetTriggeredById(),
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.TriggerPipelineResponse{Pipeline: pipelineToProto(p), Created: created}, nil
}

func (s *pipelineServer) GetPipeline(ctx context.Context, req *civ1.GetPipelineRequest) (*civ1.GetPipelineResponse, error) {
	id, err := apierr.ParseID("id", req.GetId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	p, err := s.d.Pipelines.Get(ctx, id)
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.GetPipelineResponse{Pipeline: pipelineToProto(p)}, nil
}

func (s *pipelineServer) ListPipelines(ctx context.Context, req *civ1.ListPipelinesRequest) (*civ1.ListPipelinesResponse, error) {
	repoID, err := apierr.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	list, next, err := s.d.Pipelines.List(ctx, repoID, req.GetRef(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	resp := &civ1.ListPipelinesResponse{NextPageToken: next}
	for _, p := range list {
		resp.Pipelines = append(resp.Pipelines, pipelineToProto(p))
	}
	return resp, nil
}

func (s *pipelineServer) CancelPipeline(ctx context.Context, req *civ1.CancelPipelineRequest) (*civ1.CancelPipelineResponse, error) {
	id, err := apierr.ParseID("id", req.GetId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	p, err := s.d.Pipelines.Cancel(ctx, id)
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.CancelPipelineResponse{Pipeline: pipelineToProto(p)}, nil
}

func (s *pipelineServer) GetJob(ctx context.Context, req *civ1.GetJobRequest) (*civ1.GetJobResponse, error) {
	id, err := apierr.ParseID("id", req.GetId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	j, err := s.d.Pipelines.GetJob(ctx, id)
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.GetJobResponse{Job: jobToProto(j)}, nil
}

func (s *pipelineServer) StreamJobLog(req *civ1.StreamJobLogRequest, stream grpc.ServerStreamingServer[civ1.StreamJobLogResponse]) error {
	ctx := stream.Context()
	id, err := apierr.ParseID("job_id", req.GetJobId())
	if err != nil {
		return toStatus(ctx, s.d.Log, err)
	}
	err = s.d.Pipelines.StreamLog(ctx, id, req.GetOffset(), req.GetFollow(), func(offset int64, data []byte) error {
		return stream.Send(&civ1.StreamJobLogResponse{Offset: offset, Data: data})
	})
	if err != nil {
		return toStatus(ctx, s.d.Log, err)
	}
	return nil
}

func (s *pipelineServer) ValidateConfig(ctx context.Context, req *civ1.ValidateConfigRequest) (*civ1.ValidateConfigResponse, error) {
	if len(req.GetContent()) > ciyaml.MaxFileSize {
		return nil, toStatus(ctx, s.d.Log, apierr.InvalidField("content", "must be at most 256 KiB"))
	}
	_, err := ciyaml.Parse([]byte(req.GetContent()))
	var errs ciyaml.Errors
	if errors.As(err, &errs) {
		return &civ1.ValidateConfigResponse{Errors: configErrorsToProto(errs)}, nil
	}
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.ValidateConfigResponse{Valid: true}, nil
}

// runnerServer implements civ1.RunnerServiceServer.
type runnerServer struct {
	civ1.UnimplementedRunnerServiceServer
	d Deps
}

func (s *runnerServer) RegisterRunner(ctx context.Context, req *civ1.RegisterRunnerRequest) (*civ1.RegisterRunnerResponse, error) {
	if !s.d.RegisterLimit.Allow(ctx, clientIP(ctx)) {
		return nil, toStatus(ctx, s.d.Log, apierr.New(apierr.ResourceExhausted, "too many registration attempts; wait a minute"))
	}
	r, tok, err := s.d.Runners.Register(ctx, runners.RegisterInput{
		RegistrationToken: req.GetRegistrationToken(),
		Name:              req.GetName(),
		Labels:            req.GetLabels(),
		Executor:          req.GetExecutor(),
		Version:           req.GetVersion(),
		Platform:          req.GetPlatform(),
	})
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	s.d.Log.InfoContext(ctx, "runner registered", "runner_id", r.ID.String(), "name", r.Name)
	return &civ1.RegisterRunnerResponse{RunnerId: r.ID.String(), RunnerToken: tok}, nil
}

// clientIP is the caller's IP address, the key of the registration rate
// limit.
func clientIP(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return "unknown"
	}
	if host, _, err := net.SplitHostPort(p.Addr.String()); err == nil {
		return host
	}
	return p.Addr.String()
}

func (s *runnerServer) RequestJob(ctx context.Context, _ *civ1.RequestJobRequest) (*civ1.RequestJobResponse, error) {
	r := runnerFrom(ctx)
	if err := s.d.Runners.Touch(ctx, r.ID); err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	a, err := s.d.Pipelines.RequestJob(ctx, pipeline.Runner{ID: r.ID, Labels: r.Labels, Paused: r.Paused})
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	if a == nil {
		return &civ1.RequestJobResponse{}, nil
	}
	s.d.Log.InfoContext(ctx, "job started", "job_id", a.JobID.String(), "runner_id", r.ID.String())
	return &civ1.RequestJobResponse{Job: assignmentToProto(a)}, nil
}

func jobStatusFromProto(st civ1.JobStatus) pipeline.JobStatus {
	switch st {
	case civ1.JobStatus_JOB_STATUS_RUNNING:
		return pipeline.JobRunning
	case civ1.JobStatus_JOB_STATUS_SUCCESS:
		return pipeline.JobSuccess
	case civ1.JobStatus_JOB_STATUS_FAILED:
		return pipeline.JobFailed
	}
	return ""
}

func (s *runnerServer) UpdateJob(ctx context.Context, req *civ1.UpdateJobRequest) (*civ1.UpdateJobResponse, error) {
	id, err := apierr.ParseID("job_id", req.GetJobId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	reason, ok := pipeline.ReasonFromProto(req.GetFailureReason())
	if !ok {
		return nil, toStatus(ctx, s.d.Log, apierr.InvalidField("failure_reason", "is unknown"))
	}
	canceled, err := s.d.Pipelines.UpdateJob(ctx, runnerFrom(ctx).ID, pipeline.UpdateInput{
		JobID:    id,
		Status:   jobStatusFromProto(req.GetStatus()),
		ExitCode: req.ExitCode,
		Reason:   reason,
	})
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.UpdateJobResponse{CancelRequested: canceled}, nil
}

func (s *runnerServer) AppendLog(ctx context.Context, req *civ1.AppendLogRequest) (*civ1.AppendLogResponse, error) {
	id, err := apierr.ParseID("job_id", req.GetJobId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	size, truncated, err := s.d.Pipelines.AppendLog(ctx, runnerFrom(ctx).ID, id, req.GetOffset(), req.GetData())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.AppendLogResponse{Size: size, Truncated: truncated}, nil
}

// adminServer implements civ1.RunnerAdminServiceServer.
type adminServer struct {
	civ1.UnimplementedRunnerAdminServiceServer
	d Deps
}

func (s *adminServer) CreateRegistrationToken(ctx context.Context, req *civ1.CreateRegistrationTokenRequest) (*civ1.CreateRegistrationTokenResponse, error) {
	var ttl time.Duration
	if req.GetExpiresIn() != nil {
		if err := req.GetExpiresIn().CheckValid(); err != nil {
			return nil, toStatus(ctx, s.d.Log, apierr.InvalidField("expires_in", "is not a valid duration"))
		}
		ttl = req.GetExpiresIn().AsDuration()
	}
	tok, expiresAt, err := s.d.Runners.CreateRegistrationToken(ctx, req.GetDescription(), ttl)
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.CreateRegistrationTokenResponse{Token: tok, ExpiresAt: ts(&expiresAt)}, nil
}

func (s *adminServer) ListRunners(ctx context.Context, req *civ1.ListRunnersRequest) (*civ1.ListRunnersResponse, error) {
	list, next, err := s.d.Runners.List(ctx, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	now := time.Now()
	resp := &civ1.ListRunnersResponse{NextPageToken: next}
	for _, r := range list {
		resp.Runners = append(resp.Runners, runnerToProto(r, now))
	}
	return resp, nil
}

func (s *adminServer) GetRunner(ctx context.Context, req *civ1.GetRunnerRequest) (*civ1.GetRunnerResponse, error) {
	id, err := apierr.ParseID("id", req.GetId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	r, err := s.d.Runners.Get(ctx, id)
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.GetRunnerResponse{Runner: runnerToProto(r, time.Now())}, nil
}

func (s *adminServer) UpdateRunner(ctx context.Context, req *civ1.UpdateRunnerRequest) (*civ1.UpdateRunnerResponse, error) {
	id, err := apierr.ParseID("id", req.GetId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	r, err := s.d.Runners.Update(ctx, id, req.Paused, req.Description)
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.UpdateRunnerResponse{Runner: runnerToProto(r, time.Now())}, nil
}

func (s *adminServer) DeleteRunner(ctx context.Context, req *civ1.DeleteRunnerRequest) (*civ1.DeleteRunnerResponse, error) {
	id, err := apierr.ParseID("id", req.GetId())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	if err := s.d.Runners.Delete(ctx, id); err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.DeleteRunnerResponse{}, nil
}
