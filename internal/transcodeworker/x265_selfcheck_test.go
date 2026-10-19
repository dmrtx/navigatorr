package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeX265Check stands in for ffmpeg: every encode "writes" a marker file,
// every framemd5 prints digest(<encode mode>) for 48 frames. diverge(name)
// decides which encodes (by the dir name "<case>-<variant>") decode differently.
type fakeX265Check struct {
	mu       sync.Mutex
	calls    [][]string
	diverge  func(dir string) bool // dir of a framemd5 call
	failWhen func(dir string, args []string) error
}

func (f *fakeX265Check) run(ctx context.Context, dir string, args []string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{filepath.Base(dir)}, args...))
	f.mu.Unlock()
	if f.failWhen != nil {
		if err := f.failWhen(filepath.Base(dir), args); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, a := range args {
		if a == "-f" && i+1 < len(args) && args[i+1] == "framemd5" {
			var b strings.Builder
			b.WriteString("#format: frame checksums\n#software: fake\n")
			tag := "same"
			if f.diverge != nil && f.diverge(filepath.Base(dir)) {
				tag = "other"
			}
			for n := 0; n < x265CheckFrames; n++ {
				fmt.Fprintf(&b, "0, %d, %d, 1, 3110400, %s%d\n", n, n, tag, n)
			}
			return []byte(b.String()), nil
		}
	}
	return nil, nil
}

func newSelfCheckWorker(t *testing.T, cfg WorkerConfig, f *fakeX265Check) *Worker {
	t.Helper()
	withHostCPUs(t, 10)
	cfg.StateDir = t.TempDir()
	w := &Worker{cfg: &cfg, ffmpegPath: "ffmpeg", x265CheckRun: f.run}
	return w
}

func TestX265SelfCheckPassKeepsPartition(t *testing.T) {
	f := &fakeX265Check{}
	w := newSelfCheckWorker(t, WorkerConfig{MaxParallelJobs: 2}, f)
	rec := w.RunX265PartitionCheck(context.Background())
	if rec.Status != x265CheckPassed || rec.Params != "pools=5:frame-threads=3" {
		t.Fatalf("verdict %+v", rec)
	}
	if got := w.x265ThreadParams(); got != "pools=5:frame-threads=3" {
		t.Fatalf("partition dropped after a pass: %q", got)
	}
	st := w.X265PartitionStatus()
	if st.Status != x265CheckPassed || !st.Active || st.CheckedAt == nil {
		t.Fatalf("status %+v", st)
	}
	// Both encodes of both cases ran, partitioned and not, through the real
	// param merge, and the scratch clip is gone.
	var plain, pooled, twoPass int
	for _, c := range f.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "libx265") {
			if strings.Contains(joined, "pools=5:frame-threads=3") {
				pooled++
			} else {
				plain++
			}
			if strings.Contains(joined, "pass=1") {
				twoPass++
			}
		}
	}
	if plain != 3 || pooled != 3 || twoPass != 2 {
		t.Fatalf("encodes plain=%d pooled=%d firstPass=%d: %v", plain, pooled, twoPass, f.calls)
	}
	if _, err := os.Stat(filepath.Join(w.cfg.StateDir, x265CheckScratch)); !os.IsNotExist(err) {
		t.Fatalf("scratch left behind: %v", err)
	}
}

func TestX265SelfCheckMismatchDisablesPartition(t *testing.T) {
	f := &fakeX265Check{diverge: func(dir string) bool { return dir == "two_pass-partitioned" }}
	w := newSelfCheckWorker(t, WorkerConfig{MaxParallelJobs: 2}, f)
	rec := w.RunX265PartitionCheck(context.Background())
	if rec.Status != x265CheckFailed || !strings.Contains(rec.Detail, "two_pass") || strings.Contains(rec.Detail, "crf") {
		t.Fatalf("verdict %+v", rec)
	}
	if got := w.x265ThreadParams(); got != "" {
		t.Fatalf("partition still in use after a mismatch: %q", got)
	}
	if st := w.X265PartitionStatus(); st.Status != x265CheckFailed || st.Active || st.Params == "" {
		t.Fatalf("status %+v", st)
	}

	// A spawned job process knows nothing of this one, only the stored verdict.
	job := &Worker{cfg: &WorkerConfig{MaxParallelJobs: 2, StateDir: w.cfg.StateDir}}
	if got := job.x265ThreadParams(); got != "" {
		t.Fatalf("job process ignores the stored failure: %q", got)
	}
	// A verdict for another pool shape or host size does not apply.
	other := &Worker{cfg: &WorkerConfig{MaxParallelJobs: 2, X265Pools: 4, StateDir: w.cfg.StateDir}}
	if got := other.x265ThreadParams(); got != "pools=4:frame-threads=3" {
		t.Fatalf("verdict for other params applied: %q", got)
	}
	hostCPUCount = func() int { return 12 }
	if got := job.x265ThreadParams(); got != "pools=6:frame-threads=3" {
		t.Fatalf("verdict for another CPU count applied: %q", got)
	}
}

func TestX265SelfCheckPartitionedEncodeFailureDisables(t *testing.T) {
	f := &fakeX265Check{failWhen: func(dir string, args []string) error {
		if dir == "crf-partitioned" && strings.Contains(strings.Join(args, " "), "libx265") {
			return errors.New("x265 refused pools")
		}
		return nil
	}}
	w := newSelfCheckWorker(t, WorkerConfig{MaxParallelJobs: 2}, f)
	rec := w.RunX265PartitionCheck(context.Background())
	if rec.Status != x265CheckFailed || !strings.Contains(rec.Detail, "partitioned encode failed") {
		t.Fatalf("verdict %+v", rec)
	}
	if w.x265ThreadParams() != "" {
		t.Fatal("partition kept although its encode fails")
	}
}

func TestX265SelfCheckInconclusiveChangesNothing(t *testing.T) {
	cases := map[string]*fakeX265Check{
		"no source": {failWhen: func(dir string, args []string) error {
			if strings.Contains(strings.Join(args, " "), "lavfi") {
				return errors.New("no lavfi")
			}
			return nil
		}},
		"baseline encode fails": {failWhen: func(dir string, args []string) error {
			if dir == "crf-baseline" && strings.Contains(strings.Join(args, " "), "libx265") {
				return errors.New("no libx265")
			}
			return nil
		}},
	}
	for name, f := range cases {
		w := newSelfCheckWorker(t, WorkerConfig{MaxParallelJobs: 2}, f)
		rec := w.RunX265PartitionCheck(context.Background())
		if rec.Status != x265CheckInconclusive {
			t.Fatalf("%s: verdict %+v", name, rec)
		}
		if got := w.x265ThreadParams(); got != "pools=5:frame-threads=3" {
			t.Errorf("%s: inconclusive check changed the partition: %q", name, got)
		}
		if _, err := os.Stat(filepath.Join(w.cfg.StateDir, x265CheckFile)); !os.IsNotExist(err) {
			t.Errorf("%s: inconclusive verdict stored: %v", name, err)
		}
	}

	// A decode that yields no frames must not compare "equal" to another.
	empty := &fakeX265Check{}
	w := newSelfCheckWorker(t, WorkerConfig{MaxParallelJobs: 2}, empty)
	w.x265CheckRun = func(ctx context.Context, dir string, args []string) ([]byte, error) {
		return []byte("#format: frame checksums\n"), nil
	}
	if rec := w.RunX265PartitionCheck(context.Background()); rec.Status != x265CheckInconclusive {
		t.Fatalf("empty decodes: %+v", rec)
	}
}

func TestX265SelfCheckCancelledIsInconclusive(t *testing.T) {
	w := newSelfCheckWorker(t, WorkerConfig{MaxParallelJobs: 2}, &fakeX265Check{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rec := w.RunX265PartitionCheck(ctx); rec.Status != x265CheckInconclusive {
		t.Fatalf("cancelled check: %+v", rec)
	}
	if w.x265ThreadParams() == "" {
		t.Fatal("a cancelled check disabled the partition")
	}
}

func TestX265SelfCheckTimeIsBounded(t *testing.T) {
	old := x265CheckTimeout
	x265CheckTimeout = 50 * time.Millisecond
	defer func() { x265CheckTimeout = old }()
	w := newSelfCheckWorker(t, WorkerConfig{MaxParallelJobs: 2}, &fakeX265Check{})
	w.x265CheckRun = func(ctx context.Context, dir string, args []string) ([]byte, error) {
		<-ctx.Done() // a hung ffmpeg only ends when the deadline kills it
		return nil, ctx.Err()
	}
	start := time.Now()
	rec := w.RunX265PartitionCheck(context.Background())
	if rec.Status != x265CheckInconclusive || time.Since(start) > 5*time.Second {
		t.Fatalf("hung check not bounded: %+v after %v", rec, time.Since(start))
	}
}

func TestX265SelfCheckSkips(t *testing.T) {
	cases := []struct {
		name   string
		cfg    WorkerConfig
		status string
	}{
		{"pools -1", WorkerConfig{MaxParallelJobs: 2, X265Pools: -1}, x265CheckConfigOff},
		{"single job", WorkerConfig{MaxParallelJobs: 1}, x265CheckNotNeeded},
		{"pool below the floor", WorkerConfig{MaxParallelJobs: 2, X265Pools: 3}, x265CheckNotNeeded},
	}
	for _, tc := range cases {
		f := &fakeX265Check{}
		w := newSelfCheckWorker(t, tc.cfg, f)
		select {
		case <-w.StartX265PartitionCheck(context.Background()):
		case <-time.After(time.Second):
			t.Fatalf("%s: skip did not finish", tc.name)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s: skipped check ran ffmpeg: %v", tc.name, f.calls)
		}
		if st := w.X265PartitionStatus(); st.Status != tc.status || st.Active {
			t.Errorf("%s: status %+v, want %s", tc.name, st, tc.status)
		}
	}
}

// Start returns at once and reports "running" until the background check ends,
// and a stored failure keeps the partition off meanwhile.
func TestX265SelfCheckRunsInBackground(t *testing.T) {
	release := make(chan struct{})
	f := &fakeX265Check{}
	w := newSelfCheckWorker(t, WorkerConfig{MaxParallelJobs: 2}, f)
	if err := w.saveX265PartitionCheck(x265PartitionCheck{Status: x265CheckFailed, Params: "pools=5:frame-threads=3", CPUs: 10}); err != nil {
		t.Fatal(err)
	}
	inner := f.run
	w.x265CheckRun = func(ctx context.Context, dir string, args []string) ([]byte, error) {
		<-release
		return inner(ctx, dir, args)
	}
	done := w.StartX265PartitionCheck(context.Background())
	st := w.X265PartitionStatus()
	if st.Status != x265CheckRunning || st.Active || !strings.Contains(st.Detail, "failed") {
		t.Fatalf("status while running %+v", st)
	}
	if w.x265ThreadParams() != "" {
		t.Fatal("a stored failure was forgotten while re-checking")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("check never finished")
	}
	if st := w.X265PartitionStatus(); st.Status != x265CheckPassed || !st.Active {
		t.Fatalf("status after a passing re-check %+v", st)
	}
	if w.x265ThreadParams() != "pools=5:frame-threads=3" {
		t.Fatal("partition not restored by a passing re-check")
	}
	if rec, ok := w.loadX265PartitionCheck("pools=5:frame-threads=3"); !ok || rec.Status != x265CheckPassed {
		t.Fatalf("stored verdict %+v", rec)
	}
}

func TestHealthReportsX265Partition(t *testing.T) {
	f := &fakeX265Check{diverge: func(dir string) bool { return dir == "crf-partitioned" }}
	w := newSelfCheckWorker(t, WorkerConfig{MaxParallelJobs: 2}, f)
	w.RunX265PartitionCheck(context.Background())
	st := w.X265PartitionStatus()
	if st.Status != x265CheckFailed || st.Active || st.Detail == "" {
		t.Fatalf("%+v", st)
	}
}

// The real thing: ffmpeg's libx265 encodes the same frames with and without
// the partition. Skipped without libx265.
func TestRealX265SelfCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("encodes a synthetic clip six times")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output(); err != nil || !strings.Contains(string(out), "libx265") {
		t.Skip("libx265 unavailable")
	}
	withHostCPUs(t, 10)
	w := NewWorker(&WorkerConfig{FFmpeg: ffmpeg, StateDir: t.TempDir(), MaxParallelJobs: 2})
	rec := w.RunX265PartitionCheck(context.Background())
	if rec.Status != x265CheckPassed {
		t.Fatalf("real check: %+v", rec)
	}
}
