package transcodeworker

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode/quality"
)

func TestMetricArgsThreadsOption(t *testing.T) {
	model, _ := quality.Model("v1_1080p_3h")
	filter := func(args []string) string {
		for i, a := range args {
			if a == "-filter_complex" && i+1 < len(args) {
				return args[i+1]
			}
		}
		t.Fatalf("no filter in %q", args)
		return ""
	}
	vmaf0, err := BuildVMAFModelArgs("c.mkv", "r.mkv", "v.json", model, 10, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	vmaf5, err := BuildVMAFModelArgs("c.mkv", "r.mkv", "v.json", model, 10, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(filter(vmaf0), "n_threads") {
		t.Fatalf("threads=0 must keep the libvmaf default: %s", filter(vmaf0))
	}
	if !strings.HasSuffix(filter(vmaf5), ":repeatlast=0:n_threads=5") {
		t.Fatalf("VMAF filter lacks n_threads: %s", filter(vmaf5))
	}
	if strings.Contains(filter(BuildCAMBIArgs("c.mkv", "r.mkv", "c.json", 10, 0)), "n_threads") {
		t.Fatal("CAMBI threads=0 must keep the libvmaf default")
	}
	if got := filter(BuildCAMBIArgs("c.mkv", "r.mkv", "c.json", 8, 3)); !strings.HasSuffix(got, ":repeatlast=0:n_threads=3") {
		t.Fatalf("CAMBI filter lacks n_threads: %s", got)
	}
}

func TestWorkerMetricThreads(t *testing.T) {
	cpus := runtime.NumCPU()
	atLeastOne := func(n int) int {
		if n < 1 {
			return 1
		}
		return n
	}
	cases := []struct {
		name       string
		cfg        WorkerConfig
		concurrent int
		want       int
	}{
		{"explicit wins", WorkerConfig{MetricThreads: 3, MaxParallelJobs: 4}, 2, 3},
		{"split across metrics", WorkerConfig{MaxParallelJobs: 1}, 2, atLeastOne(cpus / 2)},
		{"split across jobs", WorkerConfig{MaxParallelJobs: 2}, 2, atLeastOne(cpus / 4)},
		{"zero concurrency treated as one", WorkerConfig{MaxParallelJobs: 1}, 0, cpus},
		{"never below one", WorkerConfig{MaxParallelJobs: 1}, cpus * 4, 1},
	}
	for _, tc := range cases {
		w := &Worker{cfg: &tc.cfg}
		if got := w.metricThreads(tc.concurrent); got != tc.want {
			t.Errorf("%s: metricThreads(%d) = %d, want %d", tc.name, tc.concurrent, got, tc.want)
		}
	}
}

func TestLoadWorkerConfigRejectsNegativeMetricThreads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.yml")
	if err := os.WriteFile(path, []byte("metric_threads: -1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkerConfig(path); err == nil || !strings.Contains(err.Error(), "metric_threads") {
		t.Fatalf("negative metric_threads accepted: %v", err)
	}
}
