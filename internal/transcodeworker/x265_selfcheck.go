package transcodeworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The pool partition (x265ThreadParams) is only safe because x265 encodes the
// same frames with and without it. That holds on the x265 builds it was
// checked against; a different libx265 could break it silently, so the worker
// re-proves it at startup on a tiny synthetic clip and turns the partition off
// when the decoded frames differ. The pool size follows the number of video
// jobs running together, so every partition the current slot limits can
// produce (x265PartitionCandidates) is proved, and a slot change that adds one
// proves it again.

const (
	x265CheckFile    = "x265-partition-check.json"
	x265CheckScratch = "x265-selfcheck"
	x265CheckFrames  = 48 // 2 s at 24 fps
	// x265CheckABRParams has the shape of the params the worker's two-pass
	// encodes pass (buildLibX265Args), so lookahead and rate control are
	// exercised the way production uses them.
	x265CheckABRParams = "rc-lookahead=40:bframes=4:ref=4:aq-mode=3:aq-strength=1:psy-rd=2:psy-rdoq=1:ssim-rd=1:rdoq-level=2"
)

// x265CheckTimeout bounds the whole check. The clip is two seconds of 1080p;
// the six short encodes take well under a minute on the target hosts. A var so
// tests can shrink it.
var x265CheckTimeout = 3 * time.Minute

// Self-check statuses reported by /v1/health.
const (
	x265CheckNotRun       = "not_run"
	x265CheckRunning      = "running"
	x265CheckPassed       = "passed"
	x265CheckFailed       = "failed"
	x265CheckInconclusive = "inconclusive"
	x265CheckConfigOff    = "disabled_by_config"
	x265CheckNotNeeded    = "not_applicable"
)

// x265PartitionCheck is the persisted verdict. Spawned job processes read it,
// because the check runs in the serve process and each job is its own process.
// Only conclusive verdicts are stored.
type x265PartitionCheck struct {
	Status     string    `json:"status"` // passed | failed
	Params     string    `json:"params"` // the candidate partitions checked, comma-separated
	CPUs       int       `json:"cpus"`
	Detail     string    `json:"detail,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
	DurationMs int64     `json:"duration_ms"`
}

// x265PartitionState is the in-process view of the check, for /v1/health.
type x265PartitionState struct {
	mu         sync.Mutex
	status     string
	detail     string
	checkedAt  time.Time
	durationMs int64
	disabled   bool
}

// x265PartitionStatus is the /v1/health report.
type x265PartitionStatus struct {
	Status     string     `json:"status"`
	Params     string     `json:"params,omitempty"`
	Active     bool       `json:"active"`
	Detail     string     `json:"detail,omitempty"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
	DurationMs int64      `json:"duration_ms,omitempty"`
}

// X265PartitionStatus reports the configured partition, whether encodes use
// it, and the self-check outcome. It reads memory only, so health stays cheap.
func (w *Worker) X265PartitionStatus() x265PartitionStatus {
	params := w.x265PartitionKey()
	st := &w.x265Partition
	st.mu.Lock()
	defer st.mu.Unlock()
	out := x265PartitionStatus{Status: st.status, Params: params, Active: params != "" && !st.disabled, Detail: st.detail, DurationMs: st.durationMs}
	if out.Status == "" {
		out.Status = x265CheckNotRun
	}
	if !st.checkedAt.IsZero() {
		at := st.checkedAt
		out.CheckedAt = &at
	}
	return out
}

func (w *Worker) x265CheckPath() string {
	if w == nil || w.cfg == nil || strings.TrimSpace(w.cfg.StateDir) == "" {
		return ""
	}
	return filepath.Join(w.cfg.StateDir, x265CheckFile)
}

// loadX265PartitionCheck returns the stored verdict for the partition params
// and CPU count in force, if there is one.
func (w *Worker) loadX265PartitionCheck(params string) (x265PartitionCheck, bool) {
	path := w.x265CheckPath()
	if path == "" {
		return x265PartitionCheck{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return x265PartitionCheck{}, false
	}
	var rec x265PartitionCheck
	if json.Unmarshal(data, &rec) != nil || rec.Params != params || rec.CPUs != hostCPUCount() {
		return x265PartitionCheck{}, false
	}
	return rec, true
}

// x265PartitionDisabled reports whether a self-check found params unsafe, in
// this process or in the stored verdict of a previous run.
func (w *Worker) x265PartitionDisabled(params string) bool {
	if w == nil {
		return false
	}
	w.x265Partition.mu.Lock()
	disabled := w.x265Partition.disabled
	w.x265Partition.mu.Unlock()
	if disabled {
		return true
	}
	rec, ok := w.loadX265PartitionCheck(params)
	return ok && rec.Status == x265CheckFailed
}

func (w *Worker) saveX265PartitionCheck(rec x265PartitionCheck) error {
	path := w.x265CheckPath()
	if path == "" {
		return errors.New("no state_dir to store the x265 partition check")
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "x265-check-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func (w *Worker) setX265PartitionState(status, detail string, durationMs int64, disabled *bool) {
	st := &w.x265Partition
	st.mu.Lock()
	defer st.mu.Unlock()
	st.status, st.detail = status, detail
	if status != x265CheckRunning {
		st.checkedAt, st.durationMs = time.Now().UTC(), durationMs
	}
	if disabled != nil {
		st.disabled = *disabled
	}
}

// StartX265PartitionCheck runs the self-check in the background, so serving
// and readiness never wait for it, and returns a channel closed when it ends.
// While it runs a previous failed verdict keeps the partition off.
func (w *Worker) StartX265PartitionCheck(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	params := w.x265PartitionKey()
	switch {
	case w == nil || w.cfg == nil:
		close(done)
		return done
	case w.cfg.X265Pools < 0:
		w.setX265PartitionState(x265CheckConfigOff, "x265_pools is -1", 0, nil)
		close(done)
		return done
	case params == "":
		w.setX265PartitionState(x265CheckNotNeeded, "no pool partition for this host and these slot limits", 0, nil)
		close(done)
		return done
	}
	// A stored failure keeps the partition off while the check runs again.
	detail := "checking"
	var disabled *bool
	if rec, ok := w.loadX265PartitionCheck(params); ok {
		detail += " (previous result: " + rec.Status + ")"
		if rec.Status == x265CheckFailed {
			off := true
			disabled = &off
		}
	}
	w.setX265PartitionState(x265CheckRunning, detail, 0, disabled)
	go func() {
		defer close(done)
		w.RunX265PartitionCheck(ctx)
		// A slot split saved while this check ran changed the pools to prove;
		// that change could not start a check of its own.
		if ctx.Err() == nil && w.x265PartitionKey() != params {
			w.recheckX265PartitionAfterSlotChange()
		}
	}()
	return done
}

// RunX265PartitionCheck encodes a synthetic clip with and without the pool
// partition, Main10 CRF and two-pass ABR, and compares the decoded frames
// (framemd5). A difference disables the partition: in memory and in the
// stored verdict job processes read. A check that cannot run (no ffmpeg or
// libx265, cancelled, out of time) is inconclusive and changes nothing.
func (w *Worker) RunX265PartitionCheck(ctx context.Context) x265PartitionCheck {
	candidates := w.x265PartitionCandidates()
	params := strings.Join(candidates, ",")
	rec := x265PartitionCheck{Params: params, CPUs: hostCPUCount()}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, x265CheckTimeout)
	defer cancel()

	mismatches, err := w.compareX265Partition(ctx, candidates)
	rec.CheckedAt, rec.DurationMs = time.Now().UTC(), time.Since(start).Milliseconds()
	switch {
	case err != nil:
		rec.Status, rec.Detail = x265CheckInconclusive, err.Error()
		w.setX265PartitionState(x265CheckInconclusive, rec.Detail, rec.DurationMs, nil)
		log.Printf("x265 pool partition self-check inconclusive, partition unchanged: %v", err)
		return rec
	case len(mismatches) > 0:
		rec.Status, rec.Detail = x265CheckFailed, strings.Join(mismatches, "; ")
		disabled := true
		w.setX265PartitionState(x265CheckFailed, rec.Detail, rec.DurationMs, &disabled)
		log.Printf("x265 pool partition DISABLED: frames differ with %s: %s", params, rec.Detail)
	default:
		rec.Status = x265CheckPassed
		disabled := false
		w.setX265PartitionState(x265CheckPassed, "", rec.DurationMs, &disabled)
		log.Printf("x265 pool partition self-check passed with %s in %dms", params, rec.DurationMs)
	}
	if err := w.saveX265PartitionCheck(rec); err != nil {
		log.Printf("x265 pool partition self-check result not stored: %v", err)
	}
	return rec
}

// recheckX265PartitionAfterSlotChange proves the partitions a new slot split
// can produce, when that is a set no stored verdict covers. It runs in the
// background and does nothing without ffmpeg or while a check is running.
func (w *Worker) recheckX265PartitionAfterSlotChange() {
	if w == nil || w.cfg == nil || w.ffmpegPath == "" {
		return
	}
	key := w.x265PartitionKey()
	if key == "" {
		return
	}
	if _, ok := w.loadX265PartitionCheck(key); ok {
		return
	}
	w.x265Partition.mu.Lock()
	running := w.x265Partition.status == x265CheckRunning
	w.x265Partition.mu.Unlock()
	if running {
		return
	}
	w.StartX265PartitionCheck(context.Background())
}

// compareX265Partition returns one line per encode whose frames differ with a
// candidate partition, or an error when the comparison could not be made. A
// failing partitioned encode counts as a difference; a failing baseline does
// not.
func (w *Worker) compareX265Partition(ctx context.Context, candidates []string) ([]string, error) {
	if w.ffmpegPath == "" {
		return nil, errors.New("ffmpeg not configured")
	}
	scratch := filepath.Join(w.cfg.StateDir, x265CheckScratch)
	if err := os.RemoveAll(scratch); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)

	// One lossless source shared by every encode, so the comparison never
	// depends on the generator being deterministic. Noise does not compress:
	// the clip takes about 200 MB of state_dir scratch until the check ends.
	src := filepath.Join(scratch, "source.mkv")
	if _, err := w.runX265Check(ctx, scratch, "-f", "lavfi", "-i",
		"testsrc2=size=1920x1080:rate=24:duration=2,format=yuv420p10le,noise=alls=6:allf=t+u:all_seed=7",
		"-c:v", "ffv1", "-level", "3", src); err != nil {
		return nil, fmt.Errorf("synthetic source: %w", err)
	}

	cases := []struct {
		name   string
		encode func(ctx context.Context, dir, threads string) (string, error)
	}{
		{"crf", func(ctx context.Context, dir, threads string) (string, error) {
			out := filepath.Join(dir, "out.mkv")
			args := withX265ThreadParams([]string{"-i", src, "-map", "0:V:0", "-c:v", "libx265", "-preset", "medium", "-crf", "23",
				"-pix_fmt", "yuv420p10le", "-profile:v", "main10", "-an", "-sn", "-dn", out}, threads)
			_, err := w.runX265Check(ctx, dir, args...)
			return out, err
		}},
		{"two_pass", func(ctx context.Context, dir, threads string) (string, error) {
			out := filepath.Join(dir, "out.mkv")
			video := withX265ThreadParams([]string{"-c:v", "libx265", "-preset", "medium", "-b:v", "6000k", "-x265-params", x265CheckABRParams,
				"-pix_fmt", "yuv420p10le", "-profile:v", "main10"}, threads)
			first := append([]string{"-i", src, "-map", "0:V:0"}, video...)
			first = append(first, "-an", "-sn", "-dn", "-f", "null", os.DevNull)
			if _, err := w.runX265Check(ctx, dir, withX265Pass(first, 1)...); err != nil {
				return "", fmt.Errorf("first pass: %w", err)
			}
			second := append([]string{"-i", src, "-map", "0:V:0"}, video...)
			second = append(second, "-an", "-sn", "-dn", out)
			_, err := w.runX265Check(ctx, dir, withX265Pass(second, 2)...)
			return out, err
		}},
	}

	var mismatches []string
	for _, c := range cases {
		variants := []struct{ name, threads string }{{"baseline", ""}}
		for i, params := range candidates {
			name := "partitioned"
			if i > 0 {
				name += strconv.Itoa(i + 1)
			}
			variants = append(variants, struct{ name, threads string }{name, params})
		}
		digests := map[string]string{}
		for _, variant := range variants {
			dir := filepath.Join(scratch, c.name+"-"+variant.name)
			if err := os.Mkdir(dir, 0o700); err != nil {
				return nil, err
			}
			encoded, err := c.encode(ctx, dir, variant.threads)
			if err == nil {
				digests[variant.name], err = w.x265FrameDigest(ctx, dir, encoded)
			}
			if err != nil {
				if variant.threads != "" && ctx.Err() == nil && digests["baseline"] != "" {
					mismatches = append(mismatches, fmt.Sprintf("%s: partitioned encode failed (%s): %v", c.name, variant.threads, err))
					continue
				}
				return nil, fmt.Errorf("%s %s: %w", c.name, variant.name, err)
			}
			_ = os.RemoveAll(dir)
			if variant.threads != "" && digests[variant.name] != digests["baseline"] {
				mismatches = append(mismatches, fmt.Sprintf("%s: framemd5 differs with %s", c.name, variant.threads))
			}
		}
	}
	return mismatches, nil
}

// x265FrameDigest decodes an encode and hashes its per-frame MD5 lines (the
// framemd5 comment header is ignored). A wrong frame count is an error: two
// empty decodes must never compare equal.
func (w *Worker) x265FrameDigest(ctx context.Context, dir, encoded string) (string, error) {
	out, err := w.runX265Check(ctx, dir, "-i", encoded, "-map", "0:v:0", "-f", "framemd5", "-")
	if err != nil {
		return "", fmt.Errorf("framemd5: %w", err)
	}
	h := sha256.New()
	frames := 0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		frames++
		h.Write([]byte(line + "\n"))
	}
	if frames != x265CheckFrames {
		return "", fmt.Errorf("decoded %d frames, want %d", frames, x265CheckFrames)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runX265Check runs one bounded ffmpeg invocation in dir and returns stdout.
func (w *Worker) runX265Check(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if w.x265CheckRun != nil {
		return w.x265CheckRun(ctx, dir, args)
	}
	cmd := exec.CommandContext(ctx, w.ffmpegPath, append([]string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "error", "-y"}, args...)...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	var stdout bytes.Buffer
	tail := newTailBuffer(4096)
	cmd.Stdout, cmd.Stderr = &stdout, tail
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, boundedErrorMessage(nil, []byte(tail.String()), 256))
	}
	return stdout.Bytes(), nil
}
