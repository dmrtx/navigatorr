package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// The source's own extract decides whether a fresh window has source pictures:
// a container that runs on past its video (audio here) leaves a window there
// unmeasured, gated or not, while a source extract that fails for any other
// reason is retried once and then fails a gated plan instead of letting the
// gate pass on a window it never measured. An ungated plan only reports it,
// as before.
func TestRealFreshWindowSourceExtract(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	dir := t.TempDir()
	caps, err := ProbeWorkerCapabilitiesWithScratch(ctx, ffmpeg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Filters["ssim"] {
		t.Skip("ssim filter unavailable")
	}
	// Four seconds of pictures in a container six seconds long; the plan drops
	// the audio, so the candidate lasts four. The containers' lengths do not
	// say where either video ends.
	source := filepath.Join(dir, "source.mkv")
	candidate := filepath.Join(dir, "candidate.mkv")
	for _, args := range [][]string{
		{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=4", "-f", "lavfi", "-i", "sine=duration=6", "-c:v", "ffv1", "-pix_fmt", "yuv420p", "-c:a", "flac", source},
		{"-nostdin", "-v", "error", "-i", source, "-map", "0:v", "-c:v", "mpeg4", "-q:v", "2", candidate},
	} {
		if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
	}
	// A wrapper that fails every extract of the source's window 9 and counts
	// the attempts.
	attempts := filepath.Join(dir, "attempts")
	wrapper := filepath.Join(dir, "ffmpeg-wrapper")
	script := fmt.Sprintf("#!/bin/sh\nfor a; do last=$a; done\ncase \"$last\" in */reference_9.mkv) echo x >> %q; echo 'read error' >&2; exit 1;; esac\nexec %q \"$@\"\n", attempts, ffmpeg)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	grid := []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 1}, {Index: 1, StartSeconds: 2, DurationSeconds: 1}}
	plan := func(gate bool, fresh ...transcode.BenchmarkSampleWindow) *transcode.Plan {
		return &transcode.Plan{QualityValidation: &transcode.QualityValidationPlan{
			Metric: "ssim", Samples: grid, FreshSamples: fresh, GateFreshSamples: gate,
			BenchmarkRequestDigest: "sha256:" + strings.Repeat("3", 64),
			Quality:                transcode.BenchmarkQualityConfig{FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}},
		}}
	}
	run := func(name, tool string, p *transcode.Plan) (*transcode.FinalQualityEvidence, error) {
		jobDir := filepath.Join(dir, name)
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state-"+name), AllowedRoots: []string{dir}, FFmpeg: tool, FFprobe: ffprobe, MaxParallelJobs: 1})
		return w.validateFinalPerceptual(ctx, jobDir, source, candidate, p, "sha256:candidate", 1)
	}
	pastVideo := transcode.BenchmarkSampleWindow{Index: 8, StartSeconds: 4.5, DurationSeconds: 1}
	broken := transcode.BenchmarkSampleWindow{Index: 9, StartSeconds: 1, DurationSeconds: 1}
	for _, gate := range []bool{false, true} {
		e, err := run(fmt.Sprintf("past-%v", gate), ffmpeg, plan(gate, pastVideo))
		if err != nil || e.Verdict != "pass" || len(e.FreshUnmeasured) != 1 || e.FreshUnmeasured[0].StartSeconds != 4.5 {
			t.Fatalf("gate %v: a window past the video inside the container: %+v %v", gate, e, err)
		}
	}
	e, err := run("broken-ungated", wrapper, plan(false, broken))
	if err != nil || e.Verdict != "pass" || len(e.FreshUnmeasured) != 1 || e.FreshUnmeasured[0].ReasonCode != "quality_final_sample_extract_failed" {
		t.Fatalf("ungated: a failed source extract must stay a report: %+v %v", e, err)
	}
	if err := os.Remove(attempts); err != nil {
		t.Fatal(err)
	}
	e, err = run("broken-gated", wrapper, plan(true, broken))
	if err == nil || e.Verdict == "pass" || !strings.Contains(err.Error(), "quality_final_sample_extract_failed") || !strings.Contains(err.Error(), "2 attempts") {
		t.Fatalf("gated: a failed source extract published: %+v %v", e, err)
	}
	if data, _ := os.ReadFile(attempts); strings.Count(string(data), "x") != 2 {
		t.Fatalf("source extract attempts = %q, want 2", data)
	}
}
