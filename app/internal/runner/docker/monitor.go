package docker

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
)

// monitorTimeout bounds one health check or one container sample. A stats
// sample takes about a second, because the daemon measures CPU over one.
const monitorTimeout = 5 * time.Second

// maxStatsBytes bounds the stats document read from the daemon.
const maxStatsBytes = 1 << 20

// Health implements runner.Monitor: it asks the daemon about itself and
// counts this runner's job containers. If the daemon does not answer in
// time, the report says so and nothing else.
func (e *Executor) Health(ctx context.Context) *civ1.RunnerHealth {
	ctx, cancel := context.WithTimeout(ctx, monitorTimeout)
	defer cancel()
	res, err := e.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return &civ1.RunnerHealth{}
	}
	info := res.Info
	h := &civ1.RunnerHealth{
		DockerReachable: true,
		DockerVersion:   info.ServerVersion,
		DockerPlatform:  platform(info.OSType, info.Architecture),
		Cpus:            int32(min(info.NCPU, 1<<20)), //nolint:gosec // bounded above
		MemoryBytes:     info.MemTotal,
	}
	if e.runnerID != "" {
		mine := client.Filters{}.Add("label", LabelRunnerID+"="+e.runnerID)
		if list, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: mine}); err == nil {
			h.JobContainers = int32(min(len(list.Items), 1<<20)) //nolint:gosec // bounded above
		}
	}
	return h
}

// platform turns the daemon's OS and architecture into Go's names, as the
// runner reports its own platform: "linux/amd64", not "linux/x86_64".
func platform(osType, arch string) string {
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	if osType == "" || arch == "" {
		return ""
	}
	return osType + "/" + arch
}

// ContainerStats implements runner.Monitor: one sample of the job
// container's CPU, memory and processes. Nil if the container is gone or
// the daemon does not answer in time.
func (e *Executor) ContainerStats(ctx context.Context, jobID string) *civ1.ContainerStats {
	ctx, cancel := context.WithTimeout(ctx, monitorTimeout)
	defer cancel()
	res, err := e.cli.ContainerStats(ctx, "git2-job-"+jobID, client.ContainerStatsOptions{IncludePreviousSample: true})
	if err != nil {
		return nil
	}
	defer func() { _ = res.Body.Close() }()
	var s container.StatsResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, maxStatsBytes)).Decode(&s); err != nil {
		return nil
	}
	return statsOf(s)
}

// statsOf computes a sample the way `docker stats` does.
//
// CPU: the container's CPU time over the host's during the daemon's
// one-second interval, times the CPUs, in percent; 100 is one full CPU.
// Memory: the usage without the inactive page cache, which the kernel can
// drop at any time ("inactive_file" on cgroup v2, "total_inactive_file" on
// v1).
func statsOf(s container.StatsResponse) *civ1.ContainerStats {
	out := &civ1.ContainerStats{
		MemoryLimitBytes: clampInt64(s.MemoryStats.Limit),
		Pids:             clampInt64(s.PidsStats.Current),
		PidsLimit:        clampInt64(s.PidsStats.Limit),
	}
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpuDelta > 0 && systemDelta > 0 {
		out.CpuPercent = cpuDelta / systemDelta * cpus * 100
	}
	used := s.MemoryStats.Usage
	inactive, ok := s.MemoryStats.Stats["inactive_file"]
	if !ok {
		inactive = s.MemoryStats.Stats["total_inactive_file"]
	}
	if inactive < used {
		used -= inactive
	}
	out.MemoryBytes = clampInt64(used)
	return out
}

// clampInt64 converts a counter from the daemon, which never comes near
// the int64 limit.
func clampInt64(v uint64) int64 {
	return int64(min(v, 1<<62)) //nolint:gosec // bounded above
}
