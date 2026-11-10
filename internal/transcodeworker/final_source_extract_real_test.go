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

// A fresh window the plan does not gate still fails when the candidate's
// pictures there cannot be read twice: the candidate is the encode's own
// file, and one that cannot be extracted is a bad candidate, not an
// unmeasured window. One failed read is retried, as the source's is.
// A source extract that comes out empty where the packet scan read pictures
// failed (it is not a window past the video): it fails a gated plan and
// leaves an ungated window unmeasured. A window with no extent is the plan's
// error and fails, gated or not.
func TestRealFreshWindowExtractFailures(t *testing.T) {
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
		{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=4", "-c:v", "ffv1", "-pix_fmt", "yuv420p", source},
		{"-nostdin", "-v", "error", "-i", source, "-map", "0:v", "-c:v", "mpeg4", "-q:v", "2", candidate},
	} {
		if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
	}
	// The wrapper fails every candidate extract of window 8 and counts them,
	// fails only the first candidate extract of window 11, and makes the
	// source's extract of window 9 say it holds nothing.
	wrapper := filepath.Join(dir, "ffmpeg-wrapper")
	attempts, flaked := filepath.Join(dir, "attempts"), filepath.Join(dir, "flaked")
	script := fmt.Sprintf("#!/bin/sh\nfor a; do last=$a; done\ncase \"$last\" in */candidate_8.mkv) echo x >> %q; exit 1;; */candidate_11.mkv) if [ ! -e %q ]; then : > %q; exit 1; fi;; */reference_9.mkv) echo 'Output file is empty, nothing was encoded' >&2; exit 0;; esac\nexec %q \"$@\"\n", attempts, flaked, flaked, ffmpeg)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	packets, err := scanFilePackets(ctx, ffprobe, source, packetScanRequest{video: 0})
	if err != nil {
		t.Fatal(err)
	}
	grid := []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 1}}
	run := func(name string, gated bool, fresh ...transcode.BenchmarkSampleWindow) (*transcode.FinalQualityEvidence, error) {
		jobDir := filepath.Join(dir, name)
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		plan := &transcode.Plan{QualityValidation: &transcode.QualityValidationPlan{
			Metric:                 "ssim",
			Samples:                grid,
			FreshSamples:           fresh,
			GateFreshSamples:       gated,
			BenchmarkRequestDigest: "sha256:" + strings.Repeat("3", 64),
			Quality:                transcode.BenchmarkQualityConfig{FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}},
		}}
		w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state-"+name), AllowedRoots: []string{dir}, FFmpeg: wrapper, FFprobe: ffprobe, MaxParallelJobs: 1})
		return w.validateFinalPerceptualScanned(ctx, jobDir, source, candidate, plan, "sha256:candidate", 1, &packets.video)
	}
	unreadable := transcode.BenchmarkSampleWindow{Index: 8, StartSeconds: 2, DurationSeconds: 1}
	if e, err := run("candidate", false, unreadable); err == nil || !strings.Contains(err.Error(), "quality_final_candidate_extract_failed") || !strings.Contains(err.Error(), "2 attempts") {
		t.Fatalf("an ungated window whose candidate extract failed passed: %+v %v", e, err)
	}
	if data, _ := os.ReadFile(attempts); strings.Count(string(data), "x") != 2 {
		t.Fatalf("candidate extract attempts = %q, want 2", data)
	}
	// One failed read of the candidate is retried, as the source's is.
	flaky := transcode.BenchmarkSampleWindow{Index: 11, StartSeconds: 2, DurationSeconds: 1}
	if e, err := run("candidate-flaky", true, flaky); err != nil || e.Verdict != "pass" || len(e.FreshUnmeasured) != 0 {
		t.Fatalf("a candidate extract that failed once was not retried: %+v %v", e, err)
	}
	if _, err := os.Stat(flaked); err != nil {
		t.Fatalf("the candidate extract of window 11 never failed: %v", err)
	}
	empty := transcode.BenchmarkSampleWindow{Index: 9, StartSeconds: 2, DurationSeconds: 1}
	if e, err := run("empty-gated", true, empty); err == nil || !strings.Contains(err.Error(), "quality_final_sample_extract_failed") {
		t.Fatalf("a gated window whose source extract was empty inside the video passed: %+v %v", e, err)
	}
	e, err := run("empty-ungated", false, empty)
	if err != nil || e.Verdict != "pass" || len(e.FreshUnmeasured) != 1 || e.FreshUnmeasured[0].ReasonCode != "quality_final_sample_extract_failed" {
		t.Fatalf("an ungated window whose source extract failed must be unmeasured: %+v %v", e, err)
	}
	if e, err := run("no-extent", false, transcode.BenchmarkSampleWindow{Index: 10, StartSeconds: -1, DurationSeconds: 1}); err == nil || !strings.Contains(err.Error(), "quality_final_sample_misaligned") {
		t.Fatalf("an ungated window with no extent passed: %+v %v", e, err)
	}
}

// A variable-rate source with a still section longer than a window (a slide
// shown for four seconds, as a screen capture or a frozen frame holds one)
// has pictures after a window inside the still but none in it: the source's
// extract of that window is rightly empty, and the window is unmeasured,
// gated or not. The packet scan reads every picture's time, so it does not
// count the pictures after the window as the window's own. Windows with
// pictures, before and after the still, are measured as before.
func TestRealFreshWindowInsideStill(t *testing.T) {
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
	// Pictures every 1/24 s up to 2 s, the picture at 2 s held until 6 s,
	// then pictures every 1/24 s again until 8 s.
	source := filepath.Join(dir, "source.mkv")
	candidate := filepath.Join(dir, "candidate.mkv")
	for _, args := range [][]string{
		{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=8", "-vf", "select='not(between(t,2.01,5.99))'", "-fps_mode", "vfr", "-c:v", "ffv1", "-pix_fmt", "yuv420p", source},
		{"-nostdin", "-v", "error", "-i", source, "-map", "0:v", "-fps_mode", "passthrough", "-c:v", "mpeg4", "-q:v", "2", candidate},
	} {
		if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
	}
	packets, err := scanFilePackets(ctx, ffprobe, source, packetScanRequest{video: 0})
	if err != nil {
		t.Fatal(err)
	}
	scan := &packets.video
	if !scan.picturesIn(scan.First+0.5, scan.First+1.5) || scan.picturesIn(scan.First+3, scan.First+4) || !scan.picturesIn(scan.First+6.5, scan.First+7.5) {
		t.Fatalf("pictures read in [0.5,1.5) %v, [3,4) %v, [6.5,7.5) %v", scan.picturesIn(scan.First+0.5, scan.First+1.5), scan.picturesIn(scan.First+3, scan.First+4), scan.picturesIn(scan.First+6.5, scan.First+7.5))
	}
	grid := []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 1}}
	still := transcode.BenchmarkSampleWindow{Index: 8, StartSeconds: 3, DurationSeconds: 1}
	after := transcode.BenchmarkSampleWindow{Index: 9, StartSeconds: 6.5, DurationSeconds: 1}
	for _, gated := range []bool{true, false} {
		name := fmt.Sprintf("gated-%v", gated)
		jobDir := filepath.Join(dir, name)
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		plan := &transcode.Plan{QualityValidation: &transcode.QualityValidationPlan{
			Metric:                 "ssim",
			Samples:                grid,
			FreshSamples:           []transcode.BenchmarkSampleWindow{still, after},
			GateFreshSamples:       gated,
			BenchmarkRequestDigest: "sha256:" + strings.Repeat("3", 64),
			Quality:                transcode.BenchmarkQualityConfig{FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}},
		}}
		w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state-"+name), AllowedRoots: []string{dir}, FFmpeg: ffmpeg, FFprobe: ffprobe, MaxParallelJobs: 1})
		e, err := w.validateFinalPerceptualScanned(ctx, jobDir, source, candidate, plan, "sha256:candidate", 1, scan)
		if err != nil || e.Verdict != "pass" || len(e.FreshUnmeasured) != 1 || e.FreshUnmeasured[0].StartSeconds != 3 || e.FreshUnmeasured[0].ReasonCode != "quality_final_sample_misaligned" {
			t.Fatalf("gated %v: a window inside the still must be unmeasured, the others measured: %+v %v", gated, e, err)
		}
	}
}
