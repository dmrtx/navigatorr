package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func withHostCPUs(t *testing.T, n int) {
	t.Helper()
	old := hostCPUCount
	hostCPUCount = func() int { return n }
	t.Cleanup(func() { hostCPUCount = old })
}

func TestX265ThreadParamsFor(t *testing.T) {
	cases := []struct {
		cpus, pools int
		want        string
	}{
		{10, 5, "pools=5:frame-threads=3"},
		{10, 4, "pools=4:frame-threads=3"},
		{16, 8, "pools=8:frame-threads=4"},
		{6, 4, "pools=4:frame-threads=2"},
		{10, 3, ""},  // below the frame-identical floor
		{10, 10, ""}, // the whole host: nothing to partition
		{10, 12, ""},
		{4, 2, ""},
		{32, 16, ""}, // x265's own choice there depends on the source height
	}
	for _, tc := range cases {
		if got := x265ThreadParamsFor(tc.cpus, tc.pools); got != tc.want {
			t.Errorf("x265ThreadParamsFor(%d, %d) = %q, want %q", tc.cpus, tc.pools, got, tc.want)
		}
	}
}

func TestWorkerX265ThreadParams(t *testing.T) {
	withHostCPUs(t, 10)
	cases := []struct {
		name string
		cfg  WorkerConfig
		want string
	}{
		{"auto splits across jobs", WorkerConfig{MaxParallelJobs: 2}, "pools=5:frame-threads=3"},
		{"auto single job keeps x265 default", WorkerConfig{MaxParallelJobs: 1}, ""},
		{"auto too many jobs for an identical pool", WorkerConfig{MaxParallelJobs: 4}, ""},
		{"explicit", WorkerConfig{MaxParallelJobs: 1, X265Pools: 6}, "pools=6:frame-threads=3"},
		{"disabled", WorkerConfig{MaxParallelJobs: 2, X265Pools: -1}, ""},
	}
	for _, tc := range cases {
		w := &Worker{cfg: &tc.cfg}
		if got := w.x265ThreadParams(); got != tc.want {
			t.Errorf("%s: x265ThreadParams() = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := (*Worker)(nil).x265ThreadParams(); got != "" {
		t.Errorf("nil worker partitioned: %q", got)
	}
}

func TestWithX265ThreadParams(t *testing.T) {
	const extra = "pools=5:frame-threads=3"
	crf := []string{"-i", "in.mkv", "-c:v", "libx265", "-crf", "22", "-preset", "slow", "out.mkv"}
	got := withX265ThreadParams(crf, extra)
	want := []string{"-i", "in.mkv", "-c:v", "libx265", "-x265-params", extra, "-crf", "22", "-preset", "slow", "out.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CRF args = %q", got)
	}
	if crf[4] != "-crf" {
		t.Fatal("input args mutated")
	}

	abr := []string{"-c:v", "libx265", "-preset", "slow", "-b:v", "4000k", "-x265-params", "rc-lookahead=40:bframes=8", "out.mkv"}
	got = withX265ThreadParams(abr, extra)
	if got[7] != "rc-lookahead=40:bframes=8:pools=5:frame-threads=3" || strings.Count(strings.Join(got, " "), "-x265-params") != 1 {
		t.Fatalf("ABR args = %q", got)
	}
	if abr[7] != "rc-lookahead=40:bframes=8" {
		t.Fatal("input args mutated")
	}
	// Keys already present (or their aliases) are never duplicated.
	again := withX265ThreadParams(got, extra)
	if again[7] != got[7] {
		t.Fatalf("re-merge duplicated keys: %q", again[7])
	}
	own := withX265ThreadParams([]string{"-c:v", "libx265", "-x265-params", "numa-pools=8"}, extra)
	if own[3] != "numa-pools=8:frame-threads=3" {
		t.Fatalf("alias duplicated: %q", own[3])
	}

	for _, args := range [][]string{
		{"-c:v", "hevc_videotoolbox", "-q:v", "60", "out.mkv"},
		{"-c:v", "copy", "out.mkv"},
		{"-c:v", "ffv1", "out.mkv"},
	} {
		if got := withX265ThreadParams(args, extra); !reflect.DeepEqual(got, args) {
			t.Errorf("non-libx265 args changed: %q", got)
		}
	}
	if got := withX265ThreadParams(crf, ""); !reflect.DeepEqual(got, crf) {
		t.Errorf("empty params changed args: %q", got)
	}
}

func TestX265ThreadParamsOf(t *testing.T) {
	args := []string{"-c:v", "libx265", "-x265-params", "rc-lookahead=40:pools=5:bframes=8:frame-threads=3:pass=2"}
	if got := x265ThreadParamsOf(args); got != "pools=5:frame-threads=3" {
		t.Fatalf("x265ThreadParamsOf = %q", got)
	}
	if got := x265ThreadParamsOf([]string{"-c:v", "libx265", "-crf", "20"}); got != "" {
		t.Fatalf("x265ThreadParamsOf without params = %q", got)
	}
}

// The two-pass analysis pass is rebuilt from the plan; it must keep the
// partition of the encode it belongs to.
func TestTwoPassAnalysisKeepsThreadPartition(t *testing.T) {
	d := t.TempDir()
	calls := filepath.Join(d, "calls")
	stub := filepath.Join(d, "ffmpeg")
	script := "#!/bin/sh\necho \"$@\" >> '" + calls + "'\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	p := joyPlan(t)
	out := filepath.Join(d, "output.mkv")
	args, err := joyEncodeArgs(filepath.Join(d, "reference.mkv"), out, p)
	if err != nil {
		t.Fatal(err)
	}
	args = withX265ThreadParams(args, "pools=5:frame-threads=3")
	cmd := exec.CommandContext(context.Background(), stub, args...)
	if err := runEncoderCommand(context.Background(), cmd, args, p, out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want two passes, got %q", lines)
	}
	for i, line := range lines {
		if strings.Count(line, "pools=5") != 1 || strings.Count(line, "frame-threads=3") != 1 || !strings.Contains(line, fmt.Sprintf("pass=%d", i+1)) {
			t.Errorf("pass %d lacks the partition: %s", i+1, line)
		}
	}
}

func TestLoadWorkerConfigX265Pools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.yml")
	if err := os.WriteFile(path, []byte("x265_pools: -2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkerConfig(path); err == nil || !strings.Contains(err.Error(), "x265_pools") {
		t.Fatalf("x265_pools -2 accepted: %v", err)
	}
	if err := os.WriteFile(path, []byte("x265_pools: -1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWorkerConfig(path)
	if err != nil || cfg.X265Pools != -1 {
		t.Fatalf("x265_pools -1: %+v %v", cfg, err)
	}
}

func TestFinalValidationConcurrency(t *testing.T) {
	withHostCPUs(t, 10)
	cases := []struct {
		jobs, windows, want int
	}{
		{2, 6, 2}, // five CPUs per job: two windows of two threads
		{2, 2, 2},
		{2, 1, 1},
		{4, 6, 1},  // two CPUs per job stay sequential
		{10, 6, 1}, // one CPU per job stays sequential
		{1, 12, 3},
		{1, 2, 2},
	}
	for _, tc := range cases {
		w := &Worker{cfg: &WorkerConfig{MaxParallelJobs: tc.jobs}}
		if got := w.finalValidationConcurrency(tc.windows); got != tc.want {
			t.Errorf("jobs=%d windows=%d: concurrency %d, want %d", tc.jobs, tc.windows, got, tc.want)
		}
	}
}

func TestFinalValidationConcurrencyHonoursMetricThreads(t *testing.T) {
	withHostCPUs(t, 10)
	cases := []struct {
		name                         string
		jobs, metricThreads, windows int
		want                         int
	}{
		{"fits three windows", 1, 3, 6, 3},       // 3 x 3 threads <= 10
		{"two jobs halve the share", 2, 2, 6, 2}, // 5 CPUs per job: 2 x 2 threads
		{"wide passes run alone", 1, 8, 6, 1},    // 8 x 2 > 10
		{"all CPUs, one window", 1, 10, 6, 1},
		{"more threads than CPUs", 1, 16, 6, 1},
		{"two jobs, three-thread passes", 2, 3, 6, 1}, // 3 x 2 > 5
		{"below the pass minimum stays sequential", 1, 1, 6, 1},
		{"fewer windows than slots", 1, 2, 2, 2},
	}
	for _, tc := range cases {
		w := &Worker{cfg: &WorkerConfig{MaxParallelJobs: tc.jobs, MetricThreads: tc.metricThreads}}
		got := w.finalValidationConcurrency(tc.windows)
		if got != tc.want {
			t.Errorf("%s: concurrency %d, want %d", tc.name, got, tc.want)
		}
		// The windows together never ask for more threads than the job's share.
		if got > 1 && w.metricThreads(got)*got*tc.jobs > hostCPUCount() {
			t.Errorf("%s: %d windows x %d threads x %d jobs oversubscribe %d CPUs", tc.name, got, w.metricThreads(got), tc.jobs, hostCPUCount())
		}
	}
}

func TestX265PoolsWarning(t *testing.T) {
	cases := []struct {
		pools, cpus int
		warn        bool
	}{
		{0, 10, false}, {-1, 10, false}, {5, 10, false}, {4, 10, false},
		{3, 10, true}, {1, 10, true}, {10, 10, true}, {12, 10, true},
		{16, 64, true}, // 32+ CPUs are never partitioned
	}
	for _, tc := range cases {
		msg := x265PoolsWarning(tc.pools, tc.cpus)
		if (msg != "") != tc.warn {
			t.Errorf("pools=%d cpus=%d: warning %q, want warning=%v", tc.pools, tc.cpus, msg, tc.warn)
		}
		// A warning exactly when the partition would not apply.
		if tc.pools > 0 && tc.warn != (x265ThreadParamsFor(tc.cpus, tc.pools) == "") {
			t.Errorf("pools=%d cpus=%d: warning and partition disagree", tc.pools, tc.cpus)
		}
	}
}

func TestLoadWorkerConfigWarnsAboutIgnoredX265Pools(t *testing.T) {
	withHostCPUs(t, 10)
	var buf strings.Builder
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	path := filepath.Join(t.TempDir(), "worker.yml")
	for pools, warns := range map[string]bool{"2": true, "12": true, "5": false, "0": false} {
		buf.Reset()
		if err := os.WriteFile(path, []byte("x265_pools: "+pools+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadWorkerConfig(path); err != nil {
			t.Fatalf("x265_pools %s rejected: %v", pools, err)
		}
		if got := strings.Contains(buf.String(), "x265_pools "+pools); got != warns {
			t.Errorf("x265_pools %s: warned=%v (%q), want %v", pools, got, buf.String(), warns)
		}
	}
}

func TestRunFinalWindowsOrderAndBound(t *testing.T) {
	var inFlight, peak atomic.Int32
	results, failed, err := runFinalWindows(context.Background(), 7, 3, func(ctx context.Context, pos int) finalWindowResult {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		// Later windows finish first, so order cannot come from timing.
		time.Sleep(time.Duration(7-pos) * 3 * time.Millisecond)
		inFlight.Add(-1)
		v := float64(pos)
		return finalWindowResult{ssim: &v}
	})
	if err != nil || failed != -1 {
		t.Fatalf("failed=%d err=%v", failed, err)
	}
	if peak.Load() > 3 {
		t.Fatalf("%d windows in flight, bound 3", peak.Load())
	}
	for i, r := range results {
		if r.ssim == nil || *r.ssim != float64(i) {
			t.Fatalf("result %d out of order: %+v", i, r)
		}
	}
}

// The verdict matches the sequential loop: the lowest failing window wins,
// every earlier window completed, and later ones are cancelled or skipped.
func TestRunFinalWindowsLowestFailureWins(t *testing.T) {
	var mu sync.Mutex
	started := map[int]bool{}
	results, failed, err := runFinalWindows(context.Background(), 8, 3, func(ctx context.Context, pos int) finalWindowResult {
		mu.Lock()
		started[pos] = true
		mu.Unlock()
		switch pos {
		case 0:
			time.Sleep(50 * time.Millisecond) // finishes after window 1 failed
			if ctx.Err() != nil {
				return finalWindowResult{code: "quality_final_sample_extract_failed", err: ctx.Err()}
			}
			v := 1.0
			return finalWindowResult{ssim: &v}
		case 1:
			return finalWindowResult{code: "quality_final_sample_misaligned", err: errors.New("window 1")}
		}
		<-ctx.Done() // a later window only stops by cancellation
		return finalWindowResult{code: "quality_final_sample_extract_failed", err: ctx.Err()}
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("failed=%d, want 1", failed)
	}
	if results[failed].code != "quality_final_sample_misaligned" {
		t.Fatalf("failed=%d %+v", failed, results[failed])
	}
	if results[0].ssim == nil {
		t.Fatal("window before the failure did not complete")
	}
	for pos := 4; pos < 8; pos++ {
		if started[pos] {
			t.Errorf("window %d started after an earlier failure", pos)
		}
	}
}

func TestRunFinalWindowsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, _, err = runFinalWindows(ctx, 6, 2, func(ctx context.Context, pos int) finalWindowResult {
			if started.Add(1) == 2 {
				cancel()
			}
			<-ctx.Done()
			return finalWindowResult{code: "quality_final_sample_extract_failed", err: ctx.Err()}
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop the windows")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if n := started.Load(); n != 2 {
		t.Fatalf("%d windows started after cancellation", n)
	}
}
