package server

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/pipeline"
	"github.com/git2-project/git2-runner/app/internal/cisvc/runners"
)

// Runners report on their build hosts and job containers, and admins read
// the latest report (monitoring). Reports are stored as protobuf JSON;
// runners are untrusted, so every value is bounded first.

// maxReportText bounds the free-text fields of a report.
const maxReportText = 64

// healthJSON returns the runner's report as JSON to store, or nil if it
// sent none.
func healthJSON(h *civ1.RunnerHealth) *string {
	if h == nil {
		return nil
	}
	clean := &civ1.RunnerHealth{
		DockerReachable: h.GetDockerReachable(),
		DockerVersion:   cut(h.GetDockerVersion()),
		DockerPlatform:  cut(h.GetDockerPlatform()),
		Cpus:            max(h.GetCpus(), 0),
		MemoryBytes:     max(h.GetMemoryBytes(), 0),
		JobContainers:   max(h.GetJobContainers(), 0),
	}
	b, err := protojson.Marshal(clean)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

// statsJSON returns a container sample as JSON to store, or "" if there is
// none.
func statsJSON(c *civ1.ContainerStats) string {
	if c == nil {
		return ""
	}
	cpu := c.GetCpuPercent()
	if math.IsNaN(cpu) || math.IsInf(cpu, 0) || cpu < 0 {
		cpu = 0
	}
	clean := &civ1.ContainerStats{
		CpuPercent:       math.Round(cpu*10) / 10,
		MemoryBytes:      max(c.GetMemoryBytes(), 0),
		MemoryLimitBytes: max(c.GetMemoryLimitBytes(), 0),
		Pids:             max(c.GetPids(), 0),
		PidsLimit:        max(c.GetPidsLimit(), 0),
	}
	b, err := protojson.Marshal(clean)
	if err != nil {
		return ""
	}
	return string(b)
}

// cut bounds a report's text field and keeps it valid UTF-8.
func cut(s string) string {
	if len(s) <= maxReportText {
		return s
	}
	// A rune cut in half becomes invalid bytes, which this drops.
	return strings.ToValidUTF8(s[:maxReportText], "")
}

// healthFromJSON turns a stored report back into the message, with the
// time ci-svc received it. Nil if there is none or it cannot be read.
func healthFromJSON(s *string, at *time.Time) *civ1.RunnerHealth {
	if s == nil {
		return nil
	}
	var h civ1.RunnerHealth
	if err := protojson.Unmarshal([]byte(*s), &h); err != nil {
		return nil
	}
	h.ReportedAt = ts(at)
	return &h
}

// statsFromJSON is healthFromJSON for a container sample.
func statsFromJSON(s *string, at *time.Time) *civ1.ContainerStats {
	if s == nil {
		return nil
	}
	var c civ1.ContainerStats
	if err := protojson.Unmarshal([]byte(*s), &c); err != nil {
		return nil
	}
	c.ReportedAt = ts(at)
	return &c
}

func (s *adminServer) ListRunningJobs(ctx context.Context, req *civ1.ListRunningJobsRequest) (*civ1.ListRunningJobsResponse, error) {
	var runnerID uuid.NullUUID
	if req.GetRunnerId() != "" {
		id, err := apierr.ParseID("runner_id", req.GetRunnerId())
		if err != nil {
			return nil, toStatus(ctx, s.d.Log, err)
		}
		runnerID = uuid.NullUUID{UUID: id, Valid: true}
	}
	list, next, err := s.d.Pipelines.ListRunning(ctx, runnerID, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	resp := &civ1.ListRunningJobsResponse{NextPageToken: next}
	for _, j := range list {
		resp.Jobs = append(resp.Jobs, &civ1.RunningJob{
			JobId:        j.JobID.String(),
			Name:         j.Name,
			Stage:        j.Stage,
			Attempt:      j.Attempt,
			PipelineId:   j.PipelineID.String(),
			RepositoryId: j.RepositoryID.String(),
			Ref:          j.Ref,
			CommitSha:    j.CommitSHA,
			RunnerId:     j.RunnerID.String(),
			RunnerName:   j.RunnerName,
			StartedAt:    timestamppb.New(j.StartedAt),
			Container:    statsFromJSON(j.Stats, j.StatsAt),
		})
	}
	return resp, nil
}

func (s *adminServer) GetOverview(ctx context.Context, _ *civ1.GetOverviewRequest) (*civ1.GetOverviewResponse, error) {
	o, err := s.d.Pipelines.Overview(ctx, runners.OnlineWindow)
	if err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &civ1.GetOverviewResponse{
		RunnersOnline:            o.RunnersOnline,
		RunnersOffline:           o.RunnersOffline,
		RunnersPaused:            o.RunnersPaused,
		RunnersDockerUnreachable: o.RunnersDockerUnreachable,
		JobsPending:              o.JobsPending,
		JobsRunning:              o.JobsRunning,
		OldestPendingSince:       ts(o.OldestPendingSince),
		PipelinesLast_24H: &civ1.PipelineCounts{
			Pending:  o.Pipelines[pipeline.StatusPending],
			Running:  o.Pipelines[pipeline.StatusRunning],
			Success:  o.Pipelines[pipeline.StatusSuccess],
			Failed:   o.Pipelines[pipeline.StatusFailed],
			Canceled: o.Pipelines[pipeline.StatusCanceled],
		},
	}, nil
}
