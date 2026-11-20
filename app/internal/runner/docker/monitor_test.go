package docker

import (
	"math"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func TestStatsOf(t *testing.T) {
	var s container.StatsResponse
	// 2 CPUs; over the interval the container used 0.5 s of CPU while the
	// host's two CPUs ran 2 s in all: one quarter of the host, half a CPU.
	s.PreCPUStats.CPUUsage.TotalUsage = 1_000_000_000
	s.PreCPUStats.SystemUsage = 10_000_000_000
	s.CPUStats.CPUUsage.TotalUsage = 1_500_000_000
	s.CPUStats.SystemUsage = 12_000_000_000
	s.CPUStats.OnlineCPUs = 2
	s.MemoryStats.Usage = 300 << 20
	s.MemoryStats.Limit = 2048 << 20
	s.MemoryStats.Stats = map[string]uint64{"inactive_file": 100 << 20}
	s.PidsStats.Current = 7
	s.PidsStats.Limit = 1024

	got := statsOf(s)
	if math.Abs(got.GetCpuPercent()-50) > 1e-9 {
		t.Errorf("cpu = %v, want 50", got.GetCpuPercent())
	}
	if got.GetMemoryBytes() != 200<<20 || got.GetMemoryLimitBytes() != 2048<<20 {
		t.Errorf("memory = %d of %d", got.GetMemoryBytes(), got.GetMemoryLimitBytes())
	}
	if got.GetPids() != 7 || got.GetPidsLimit() != 1024 {
		t.Errorf("pids = %d of %d", got.GetPids(), got.GetPidsLimit())
	}

	// cgroup v1 names the page cache differently; with no previous sample
	// the CPU is unknown and reads 0; percpu_usage counts the CPUs when
	// online_cpus is missing.
	var v1 container.StatsResponse
	v1.MemoryStats.Usage = 50 << 20
	v1.MemoryStats.Stats = map[string]uint64{"total_inactive_file": 10 << 20}
	v1.CPUStats.CPUUsage.TotalUsage = 5
	v1.CPUStats.CPUUsage.PercpuUsage = []uint64{1, 2, 3, 4}
	got = statsOf(v1)
	if got.GetMemoryBytes() != 40<<20 || got.GetCpuPercent() != 0 {
		t.Errorf("v1 sample = %v", got)
	}

	// Page cache larger than the usage (seen right after a start) is
	// ignored rather than going negative.
	var odd container.StatsResponse
	odd.MemoryStats.Usage = 10
	odd.MemoryStats.Stats = map[string]uint64{"inactive_file": 20}
	if got := statsOf(odd); got.GetMemoryBytes() != 10 {
		t.Errorf("odd sample memory = %d", got.GetMemoryBytes())
	}
}

func TestPlatform(t *testing.T) {
	for _, c := range [][3]string{
		{"linux", "x86_64", "linux/amd64"},
		{"linux", "aarch64", "linux/arm64"},
		{"linux", "riscv64", "linux/riscv64"},
		{"", "x86_64", ""},
	} {
		if got := platform(c[0], c[1]); got != c[2] {
			t.Errorf("platform(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}
