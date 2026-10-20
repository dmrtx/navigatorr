package mediainspect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeProbe writes an executable shell script standing in for ffprobe.
func fakeProbe(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "fake_ffprobe.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// stuckProbe writes a fake ffprobe that never answers: it sleeps in a child
// that keeps the output pipe open, the way a probe stuck behind a stalled
// network read does. With ignoreTerm it also ignores SIGTERM. The sleeping
// child is not killed with the script (a killed shell leaves it behind), so
// the test's cleanup kills every child the script recorded.
func stuckProbe(t *testing.T, dir string, ignoreTerm bool) string {
	t.Helper()
	pids := filepath.Join(dir, "stuck_children")
	body := "sleep 30 &\necho $! >> '" + pids + "'\nwait\n"
	if ignoreTerm {
		body = "trap '' TERM\n" + body
	}
	t.Cleanup(func() {
		data, _ := os.ReadFile(pids)
		for _, field := range strings.Fields(string(data)) {
			if pid, err := strconv.Atoi(field); err == nil && pid > 1 {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	})
	return fakeProbe(t, dir, body)
}

func probeMedia(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "sample.mkv")
	if err := os.WriteFile(p, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func shrinkProbeLimits(t *testing.T, timeout, waitDelay time.Duration) {
	t.Helper()
	oldTimeout, oldFrame, oldWait, oldGrace := detailedProbeTimeout, frameProbeTimeout, probeWaitDelay, probeAbandonGrace
	detailedProbeTimeout, frameProbeTimeout, probeWaitDelay, probeAbandonGrace = timeout, timeout, waitDelay, 200*time.Millisecond
	t.Cleanup(func() {
		detailedProbeTimeout, frameProbeTimeout, probeWaitDelay, probeAbandonGrace = oldTimeout, oldFrame, oldWait, oldGrace
	})
}

func wantProbeError(t *testing.T, err error, kind ProbeKind) *ProbeError {
	t.Helper()
	pe, ok := AsProbeError(err)
	if !ok {
		t.Fatalf("error %v is not a *ProbeError", err)
	}
	if pe.Kind != kind {
		t.Fatalf("kind=%s want %s (err: %v)", pe.Kind, kind, err)
	}
	return pe
}

func TestInspectDetailedTimeoutIsBoundedByDeadlinePlusWaitDelay(t *testing.T) {
	deadline, waitDelay := 100*time.Millisecond, 400*time.Millisecond
	shrinkProbeLimits(t, deadline, waitDelay)
	dir := t.TempDir()
	// Ignores SIGTERM and leaves a child holding the output pipe open, the way
	// a probe stuck behind a stalled network read keeps Output() blocked.
	script := stuckProbe(t, dir, true)
	media := probeMedia(t, dir)

	start := time.Now()
	rep, err := InspectDetailed(context.Background(), script, media)
	took := time.Since(start)

	pe := wantProbeError(t, err, ProbeTimeout)
	if took > deadline+waitDelay+500*time.Millisecond {
		t.Fatalf("probe took %s, want within deadline %s + wait delay %s", took, deadline, waitDelay)
	}
	if rep.Probed {
		t.Fatal("a timed out probe must not be trusted")
	}
	if rep.ProbeFailure != pe {
		t.Fatalf("report did not carry the failure: %v", rep.ProbeFailure)
	}
	if !pe.Transient() {
		t.Fatal("a timeout is worth one more attempt")
	}
	if pe.Elapsed < deadline {
		t.Fatalf("elapsed %s shorter than the deadline", pe.Elapsed)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("message does not name the kind: %v", err)
	}
}

func TestInspectDetailedBadOutput(t *testing.T) {
	dir := t.TempDir()
	script := fakeProbe(t, dir, `printf '{"streams":['`)
	rep, err := InspectDetailed(context.Background(), script, probeMedia(t, dir))
	pe := wantProbeError(t, err, ProbeBadOutput)
	if rep.Probed || pe.Transient() {
		t.Fatalf("bad output is neither probed nor transient: probed=%v transient=%v", rep.Probed, pe.Transient())
	}
	if pe.Err == nil {
		t.Fatal("the decoding error is lost")
	}
}

func TestInspectDetailedToolExitKeepsStderr(t *testing.T) {
	dir := t.TempDir()
	script := fakeProbe(t, dir, "echo 'moov atom not found' >&2\nexit 1")
	_, err := InspectDetailed(context.Background(), script, probeMedia(t, dir))
	pe := wantProbeError(t, err, ProbeToolExit)
	if !strings.Contains(pe.Stderr, "moov atom not found") {
		t.Fatalf("stderr lost: %q", pe.Stderr)
	}
	if pe.Transient() {
		t.Fatal("a demuxer rejection is deterministic")
	}
	if !strings.Contains(err.Error(), "moov atom not found") {
		t.Fatalf("message hides the cause: %v", err)
	}
}

func TestInspectDetailedStorageErrorIsTransient(t *testing.T) {
	dir := t.TempDir()
	script := fakeProbe(t, dir, "echo 'sample.mkv: Input/output error' >&2\nexit 1")
	_, err := InspectDetailed(context.Background(), script, probeMedia(t, dir))
	if pe := wantProbeError(t, err, ProbeToolExit); !pe.Transient() {
		t.Fatal("an I/O error from storage is worth one more attempt")
	}
}

func TestInspectDetailedStderrIsCapped(t *testing.T) {
	dir := t.TempDir()
	script := fakeProbe(t, dir, "i=0\nwhile [ $i -lt 400 ]; do echo 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' >&2; i=$((i+1)); done\nexit 1")
	_, err := InspectDetailed(context.Background(), script, probeMedia(t, dir))
	pe := wantProbeError(t, err, ProbeToolExit)
	if len(pe.Stderr) == 0 || len(pe.Stderr) > probeStderrCap {
		t.Fatalf("stderr length %d, want 1..%d", len(pe.Stderr), probeStderrCap)
	}
}

func TestInspectDetailedUnavailable(t *testing.T) {
	dir := t.TempDir()
	_, err := InspectDetailed(context.Background(), filepath.Join(dir, "no-such-ffprobe"), probeMedia(t, dir))
	wantProbeError(t, err, ProbeUnavailable)
}

func TestInspectDetailedCallerCancelIsNotAProbeFailure(t *testing.T) {
	dir := t.TempDir()
	script := stuckProbe(t, dir, false)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	shrinkProbeLimits(t, time.Minute, 200*time.Millisecond)
	_, err := InspectDetailed(ctx, script, probeMedia(t, dir))
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("want context cancellation, got %v", err)
	}
	if _, ok := AsProbeError(err); ok {
		t.Fatal("a caller cancellation is not a verdict on the file")
	}
}

func TestInspectDetailedSuccessStillProbed(t *testing.T) {
	dir := t.TempDir()
	script := fakeProbe(t, dir, `echo '{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":1280,"height":720}],"format":{"format_name":"matroska,webm","duration":"10.0"}}'`)
	rep, err := InspectDetailed(context.Background(), script, probeMedia(t, dir))
	if err != nil || !rep.Probed || rep.ProbeFailure != nil || len(rep.Video) != 1 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
}

func TestInspectFileFallsBackOnTimeoutWithoutHanging(t *testing.T) {
	deadline, waitDelay := 100*time.Millisecond, 300*time.Millisecond
	shrinkProbeLimits(t, deadline, waitDelay)
	dir := t.TempDir()
	script := stuckProbe(t, dir, true)
	start := time.Now()
	rep, err := InspectFile(context.Background(), script, probeMedia(t, dir))
	if err != nil || rep.Probed {
		t.Fatalf("heuristic report expected: probed=%v err=%v", rep.Probed, err)
	}
	if took := time.Since(start); took > deadline+waitDelay+500*time.Millisecond {
		t.Fatalf("InspectFile took %s", took)
	}
}

func TestProbeFrameSignalsTimeoutAndBadOutput(t *testing.T) {
	deadline, waitDelay := 100*time.Millisecond, 300*time.Millisecond
	shrinkProbeLimits(t, deadline, waitDelay)
	dir := t.TempDir()
	media := probeMedia(t, dir)

	slow := stuckProbe(t, dir, true)
	start := time.Now()
	_, err := ProbeFrameSignals(context.Background(), slow, media, 0, 5)
	wantProbeError(t, err, ProbeTimeout)
	if took := time.Since(start); took > deadline+waitDelay+500*time.Millisecond {
		t.Fatalf("ProbeFrameSignals took %s", took)
	}

	shrinkProbeLimits(t, 10*time.Second, waitDelay)
	bad := fakeProbe(t, t.TempDir(), `printf '{"frames":['`)
	_, err = ProbeFrameSignals(context.Background(), bad, media, 0, 5)
	wantProbeError(t, err, ProbeBadOutput)
}
