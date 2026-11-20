package server

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
)

func TestMonitoring(t *testing.T) {
	forEachMode(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		runnerCtx, runnerID := e.newRunner(t)
		otherCtx, otherID := e.newRunner(t)

		// Before any report, a runner has no health.
		got, err := e.admin.GetRunner(ctx, &civ1.GetRunnerRequest{Id: runnerID})
		if err != nil || got.GetRunner().GetHealth() != nil {
			t.Fatalf("health before any report: %v, %v", got.GetRunner().GetHealth(), err)
		}

		// A runner's check comes with RequestJob. Text is cut to 64 bytes,
		// negative numbers become 0, and reported_at is ci-svc's.
		_, err = e.rs.RequestJob(runnerCtx, &civ1.RequestJobRequest{Health: &civ1.RunnerHealth{
			DockerReachable: true, DockerVersion: strings.Repeat("9", 100), DockerPlatform: "linux/amd64",
			Cpus: 4, MemoryBytes: -1, JobContainers: 0,
		}})
		if err != nil {
			t.Fatalf("RequestJob with health: %v", err)
		}
		got, err = e.admin.GetRunner(ctx, &civ1.GetRunnerRequest{Id: runnerID})
		h := got.GetRunner().GetHealth()
		if err != nil || !h.GetDockerReachable() || len(h.GetDockerVersion()) != 64 || h.GetCpus() != 4 ||
			h.GetMemoryBytes() != 0 || h.GetReportedAt() == nil {
			t.Fatalf("health after a report: %v, %v", h, err)
		}
		// A RequestJob without a check keeps the previous one.
		if _, err := e.rs.RequestJob(runnerCtx, &civ1.RequestJobRequest{}); err != nil {
			t.Fatal(err)
		}
		got, _ = e.admin.GetRunner(ctx, &civ1.GetRunnerRequest{Id: runnerID})
		if got.GetRunner().GetHealth().GetCpus() != 4 {
			t.Fatalf("a RequestJob without a check dropped the report: %v", got.GetRunner().GetHealth())
		}
		// The other runner's daemon does not answer.
		if _, err := e.rs.RequestJob(otherCtx, &civ1.RequestJobRequest{Health: &civ1.RunnerHealth{}}); err != nil {
			t.Fatal(err)
		}

		// A running job and its container's sample.
		repo := e.newRepo(t, threeStages)
		p := e.trigger(t, repo)
		job := e.requestJob(runnerCtx, t)
		list, err := e.admin.ListRunningJobs(ctx, &civ1.ListRunningJobsRequest{RunnerId: runnerID})
		if err != nil || len(list.GetJobs()) != 1 || list.GetJobs()[0].GetContainer() != nil {
			t.Fatalf("running jobs before a sample: %v, %v", list, err)
		}
		_, err = e.rs.UpdateJob(runnerCtx, &civ1.UpdateJobRequest{
			JobId: job.GetJobId(), Status: civ1.JobStatus_JOB_STATUS_RUNNING,
			Container: &civ1.ContainerStats{CpuPercent: 37.25, CpuLimit: 2, MemoryBytes: 64 << 20, MemoryLimitBytes: 2048 << 20, Pids: 3, PidsLimit: 1024},
		})
		if err != nil {
			t.Fatalf("heartbeat with a sample: %v", err)
		}

		// A runner busy with a job asks for no work, but its heartbeats keep
		// it online and its health fresh.
		if _, err := e.pool.Exec(ctx, `UPDATE ci_svc.runners SET last_seen_at = now() - interval '5 minutes' WHERE id = $1`, runnerID); err != nil {
			t.Fatal(err)
		}
		_, err = e.rs.UpdateJob(runnerCtx, &civ1.UpdateJobRequest{
			JobId: job.GetJobId(), Status: civ1.JobStatus_JOB_STATUS_RUNNING,
			Health: &civ1.RunnerHealth{DockerReachable: true, DockerVersion: "29.0.2", Cpus: 8},
		})
		if err != nil {
			t.Fatalf("heartbeat with health: %v", err)
		}
		got, _ = e.admin.GetRunner(ctx, &civ1.GetRunnerRequest{Id: runnerID})
		if got.GetRunner().GetStatus() != civ1.RunnerStatus_RUNNER_STATUS_ONLINE || got.GetRunner().GetHealth().GetCpus() != 8 {
			t.Fatalf("runner during a job: %v, health %v", got.GetRunner().GetStatus(), got.GetRunner().GetHealth())
		}
		// Another runner's sample for this job is dropped.
		_, err = e.rs.UpdateJob(otherCtx, &civ1.UpdateJobRequest{
			JobId: job.GetJobId(), Status: civ1.JobStatus_JOB_STATUS_RUNNING,
			Container: &civ1.ContainerStats{CpuPercent: 99},
		})
		wantCode(t, "another runner's heartbeat", err, codes.NotFound)

		list, err = e.admin.ListRunningJobs(ctx, &civ1.ListRunningJobsRequest{RunnerId: runnerID})
		if err != nil || len(list.GetJobs()) != 1 {
			t.Fatalf("running jobs: %v, %v", list, err)
		}
		rj := list.GetJobs()[0]
		c := rj.GetContainer()
		if rj.GetJobId() != job.GetJobId() || rj.GetPipelineId() != p.GetId() || rj.GetRunnerName() != "runner-"+e.tag ||
			rj.GetName() != "build" || rj.GetStage() != "build" || rj.GetAttempt() != 1 || rj.GetStartedAt() == nil ||
			c.GetCpuPercent() != 37.3 || c.GetCpuLimit() != 2 || c.GetMemoryBytes() != 64<<20 || c.GetPids() != 3 || c.GetReportedAt() == nil {
			t.Fatalf("running job = %v", rj)
		}
		other, err := e.admin.ListRunningJobs(ctx, &civ1.ListRunningJobsRequest{RunnerId: otherID})
		if err != nil || len(other.GetJobs()) != 0 {
			t.Fatalf("the other runner's running jobs: %v, %v", other, err)
		}
		_, err = e.admin.ListRunningJobs(ctx, &civ1.ListRunningJobsRequest{RunnerId: "nope"})
		wantCode(t, "runner_id not a UUID", err, codes.InvalidArgument)

		// The overview counts everything in the database, other tests'
		// rows included, so it is checked with lower bounds.
		o, err := e.admin.GetOverview(ctx, &civ1.GetOverviewRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if o.GetRunnersOnline() < 2 || o.GetRunnersDockerUnreachable() < 1 || o.GetJobsRunning() < 1 ||
			o.GetPipelinesLast_24H().GetRunning() < 1 {
			t.Fatalf("overview = %v", o)
		}
		if _, err := e.admin.UpdateRunner(ctx, &civ1.UpdateRunnerRequest{Id: otherID, Paused: ptrBool(true)}); err != nil {
			t.Fatal(err)
		}
		o2, err := e.admin.GetOverview(ctx, &civ1.GetOverviewRequest{})
		if err != nil || o2.GetRunnersPaused() != o.GetRunnersPaused()+1 || o2.GetRunnersDockerUnreachable() != o.GetRunnersDockerUnreachable()-1 {
			t.Fatalf("overview after pausing the unreachable runner: %v, %v (before %v)", o2, err, o)
		}
	})
}

func ptrBool(b bool) *bool { return &b }
