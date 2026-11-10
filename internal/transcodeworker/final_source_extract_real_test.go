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

// The packet scan read before the encode says on its own that a window past
// the source's video has no pictures: an extract of it that fails without
// ffmpeg's "Output file is empty" message (here a wrapper that exits 1 and
// says nothing else) leaves the window unmeasured instead of failing a gated
// plan, and the extract is never run. Windows inside the video, grid and
// fresh, are measured as before.
func TestRealFreshWindowPastVideoByScan(t *testing.T) {
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
	attempts := filepath.Join(dir, "attempts")
	wrapper := filepath.Join(dir, "ffmpeg-wrapper")
	script := fmt.Sprintf("#!/bin/sh\nfor a; do last=$a; done\ncase \"$last\" in */reference_8.mkv) echo x >> %q; exit 1;; esac\nexec %q \"$@\"\n", attempts, ffmpeg)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	packets, err := scanFilePackets(ctx, ffprobe, source, packetScanRequest{video: 0})
	if err != nil {
		t.Fatal(err)
	}
	scan := &packets.video
	plan := &transcode.Plan{QualityValidation: &transcode.QualityValidationPlan{
		Metric:                 "ssim",
		Samples:                []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 1}, {Index: 1, StartSeconds: 2, DurationSeconds: 1}},
		FreshSamples:           []transcode.BenchmarkSampleWindow{{Index: 7, StartSeconds: 3, DurationSeconds: 0.5}, {Index: 8, StartSeconds: 4.5, DurationSeconds: 1}},
		GateFreshSamples:       true,
		BenchmarkRequestDigest: "sha256:" + strings.Repeat("3", 64),
		Quality:                transcode.BenchmarkQualityConfig{FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}},
	}}
	run := func(name string, scan *sourceVideoScan) (*transcode.FinalQualityEvidence, error) {
		jobDir := filepath.Join(dir, name)
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state-"+name), AllowedRoots: []string{dir}, FFmpeg: wrapper, FFprobe: ffprobe, MaxParallelJobs: 1})
		return w.validateFinalPerceptualScanned(ctx, jobDir, source, candidate, plan, "sha256:candidate", 1, scan)
	}
	// Without the scan only the extract's words say so, and the wrapper
	// says nothing: the gated plan fails on a window it could not measure.
	if e, err := run("unscanned", nil); err == nil || !strings.Contains(err.Error(), "quality_final_sample_extract_failed") {
		t.Fatalf("without the scan a silent failed extract passed: %+v %v", e, err)
	}
	if err := os.Remove(attempts); err != nil {
		t.Fatal(err)
	}
	e, err := run("scanned", scan)
	if err != nil || e.Verdict != "pass" || len(e.FreshUnmeasured) != 1 || e.FreshUnmeasured[0].StartSeconds != 4.5 {
		t.Fatalf("with the scan a window past the video must be unmeasured: %+v %v", e, err)
	}
	if _, err := os.Stat(attempts); !os.IsNotExist(err) {
		t.Fatalf("the source extract ran for a window the scan placed past the video: %v", err)
	}
	// The scan's own last picture is where the boundary sits.
	if !scan.noPicturesFrom(scan.Last+0.01) || scan.noPicturesFrom(scan.Last) {
		t.Fatalf("boundary at the last picture %.3f", scan.Last)
	}
}
