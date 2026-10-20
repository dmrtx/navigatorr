package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// failingProbeWorker is a worker whose ffprobe always exits with an error whose
// stderr names a path holding the words "source" and "sha".
func failingProbeWorker(t *testing.T) (*Worker, string) {
	t.Helper()
	dir := t.TempDir()
	clip := filepath.Join(dir, "source", "shared_clip.mkv")
	if err := os.MkdirAll(filepath.Dir(clip), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clip, []byte("not media"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho '" + clip + ": End of file' >&2\nexit 1\n"
	tool := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ffprobe := tool("ffprobe", script)
	ffmpeg := tool("ffmpeg", "#!/bin/sh\nexit 1\n")
	return NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe}), clip
}

// A source that cannot be probed is a failed probe, whatever its path says.
// Before, the stderr (and with it the path) went through the text classifier,
// and "source" with "sha" read as source_changed, which blocks a manual retry.
func TestProbeFailuresKeepTheirClassWhenThePathHoldsClassifierWords(t *testing.T) {
	w, clip := failingProbeWorker(t)
	ctx := context.Background()

	plan := &transcode.Plan{Mode: "size"}
	err := w.validateModeSourceQuality(ctx, clip, plan)
	if err == nil {
		t.Fatal("a source that cannot be probed passed the mode check")
	}
	if got := resilience.ClassifyError(err); got != resilience.MediaProbeFailed {
		t.Fatalf("mode check class %s, want media_probe_failed: %v", got, err)
	}

	_, err = w.validateVideoCopy(ctx, clip, clip, sizeAudioOnlyTestPlan(t, 8), "sha256:x", 1)
	if err == nil {
		t.Fatal("video copy validation passed without being able to probe")
	}
	if got := resilience.ClassifyError(err); got != resilience.MediaProbeFailed {
		t.Fatalf("video copy class %s, want media_probe_failed: %v", got, err)
	}
}

// Audio-only video-copy validation cannot compare what it could not read. That
// is a failed job, not a "preservation_not_met" rejection: the candidate was
// not shown to differ from the source, so it is neither accepted nor blamed.
func TestVideoCopyProbeFailureIsATerminalFailureNotAPolicyRejection(t *testing.T) {
	w, clip := failingProbeWorker(t)
	evidence, err := w.validateVideoCopy(context.Background(), clip, clip, sizeAudioOnlyTestPlan(t, 8), "sha256:x", 1)
	if err == nil {
		t.Fatal("expected an error")
	}
	var rejected *transcode.PolicyRejectionError
	if errors.As(err, &rejected) {
		t.Fatalf("a probe failure was reported as a policy rejection: %v", err)
	}
	if evidence != nil {
		t.Fatalf("a probe failure produced quality evidence: %+v", evidence)
	}
	if got := resilience.ClassifyError(err); got != resilience.MediaProbeFailed {
		t.Fatalf("probe failure class %s, want media_probe_failed: %v", got, err)
	}
}

// The benchmark's own probe of the source wraps the typed error, so the class
// the record carries to the coordinator is the probe's, not a reading of the
// message that names the path.
func TestBenchmarkProbeFailureCarriesTheProbeClass(t *testing.T) {
	clip := "/media/source/shared_clip.mkv"
	probe := &mediainspect.ProbeError{Kind: mediainspect.ProbeToolExit, Err: errors.New("exit status 1"), Stderr: clip + ": End of file"}
	err := fmt.Errorf("inspecting source media %s: %w", clip, probe)
	if got := benchmarkFailureClass(err); got != string(resilience.MediaProbeFailed) {
		t.Fatalf("benchmark failure class %q", got)
	}
}
