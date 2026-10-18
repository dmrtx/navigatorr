package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// Opt-in acceptance experiment on copied source fragments. This never reads
// production worker configuration/state, submits jobs, publishes candidates,
// or replaces media. Keep quality rejection as evidence, not a transport fault.
func TestJoyRealSourceSamples(t *testing.T) {
	sources := os.Getenv("NAVIGATORR_JOY_SAMPLES_DIR")
	if sources == "" {
		t.Skip("set NAVIGATORR_JOY_SAMPLES_DIR for the explicit source experiment")
	}
	out := os.Getenv("NAVIGATORR_JOY_RESULT_DIR")
	if out == "" {
		t.Fatal("explicit isolated result directory required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	ffmpeg := ResolveToolPath("", "ffmpeg")
	ffprobe := ResolveToolPath("", "ffprobe")
	caps := probeQualityCapabilities(ctx, ffmpeg, out)
	if !caps.Models["v1_1080p_3h"].Available || !caps.CAMBIFullRef {
		t.Fatalf("required native metrics unavailable: %+v", caps)
	}
	profileFile := os.Getenv("NAVIGATORR_JOY_PROFILE_FILE")
	if profileFile == "" {
		profileFile = "../../docs/transcode/examples/transcode-profile-joy.json"
	}
	p := joyPlanFromFile(t, profileFile)
	w := NewWorker(&WorkerConfig{FFmpeg: ffmpeg, FFprobe: ffprobe, StateDir: filepath.Join(out, "isolated-state"), AllowedRoots: []string{sources, out}, MaxParallelJobs: 1})
	var samples []map[string]any
	var vmafs []quality.VMAFStats
	var cambis []quality.CAMBIStats
	model, _ := quality.Model("v1_1080p_3h")
	write := func(report any) {
		b, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(filepath.Join(out, "report.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i, start := range []float64{826, 2067, 3308} {
		source := filepath.Join(sources, fmt.Sprintf("joy-source-%d.mkv", i))
		before, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(before)
		details, err := mediainspect.InspectDetailed(ctx, ffprobe, source)
		if err != nil || len(details.Video) != 1 || details.Video[0].FPS != 25 {
			t.Fatal("this reference experiment requires a verified 25 fps source fragment")
		}
		probe, err := ProbeSourceDetails(ctx, ffprobe, source)
		if err != nil {
			t.Fatal(err)
		}
		ep, err := BuildExecutionPlan(p, probe.Streams, probe.DurationSec)
		if err != nil {
			t.Fatal(err)
		}
		candidate := filepath.Join(out, fmt.Sprintf("candidate-%d.mkv", i))
		t.Logf("sample %d: two-pass video and HE-AAC encoding", i)
		began := time.Now()
		if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(out, fmt.Sprintf("progress-%d.txt", i)), filepath.Join(out, fmt.Sprintf("encode-%d.log", i))); err != nil {
			t.Fatal(err)
		}
		attest, err := w.validateEncodedCandidateFull(ctx, candidate, ep, probe, 0)
		if err != nil {
			t.Fatal(err)
		}
		encodeSeconds := time.Since(began).Seconds()
		if b, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", candidate, "-f", "null", "-").CombinedOutput(); err != nil {
			t.Fatalf("candidate decode: %v %s", err, b)
		}
		vlog := filepath.Join(out, fmt.Sprintf("vmaf-%d.json", i))
		args, err := BuildVMAFModelArgs(candidate, source, vlog, model, 10, w.metricThreads(1))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("sample %d: VMAF v1 and CAMBI", i)
		if b, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("VMAF: %v %s", err, b)
		}
		b, err := os.ReadFile(vlog)
		if err != nil {
			t.Fatal(err)
		}
		v, err := quality.ParseVMAF(b, MaxMetricLogSizeBytes, model, i, 0, 25, 5, nil)
		if err != nil {
			t.Fatal(err)
		}
		clog := filepath.Join(out, fmt.Sprintf("cambi-%d.json", i))
		if b, err := exec.CommandContext(ctx, ffmpeg, BuildCAMBIArgs(candidate, source, clog, 10, w.metricThreads(1))...).CombinedOutput(); err != nil {
			t.Fatalf("CAMBI: %v %s", err, b)
		}
		b, err = os.ReadFile(clog)
		if err != nil {
			t.Fatal(err)
		}
		c, err := quality.ParseCAMBIFullReference(b, MaxMetricLogSizeBytes, caps.CAMBIOutput, i, 0, 25)
		if err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(source)
		if err != nil || sha256.Sum256(after) != hash {
			t.Fatal("source fragment changed")
		}
		vmafs = append(vmafs, v)
		cambis = append(cambis, c)
		passed := v.Mean >= 93 && v.P5 >= 88 && v.WorstWindowMean >= 90 && c.Mean <= 6 && c.Max <= 16
		samples = append(samples, map[string]any{"sample": i, "requested_source_seek_seconds": start, "source": source, "source_sha256": hex.EncodeToString(hash[:]), "source_bytes": len(before), "duration_seconds": probe.DurationSec, "candidate": candidate, "candidate_bytes": attest.SizeBytes, "candidate_sha256": attest.SHA256, "encode_seconds": encodeSeconds, "vmaf": v, "cambi": c, "quality_accepted": passed, "structural_audio_timing_decode_validated": true})
		write(map[string]any{"status": "running", "samples": samples})
		t.Logf("sample %d: VMAF %.3f p5 %.3f worst %.3f; CAMBI %.3f/%.3f; accepted=%v", i, v.Mean, v.P5, v.WorstWindowMean, c.Mean, c.Max, passed)
	}
	v, err := quality.CombineVMAF(vmafs)
	if err != nil {
		t.Fatal(err)
	}
	c, err := quality.CombineCAMBI(cambis)
	if err != nil {
		t.Fatal(err)
	}
	accepted := true
	for _, s := range samples {
		accepted = accepted && s["quality_accepted"].(bool)
	}
	write(map[string]any{"status": "completed", "profile": filepath.Base(profileFile), "profile_plan_digest": p.PlanDigest, "reference": "Road Notes S03E01 QOQ H264 8-bit + EAC3 5.1; stream-copy fragments; seek may include keyframe lead-in", "sampling_note": "Metric locations are local to each verified 25 fps fragment; requested seeks are not exact cuts on the full episode.", "samples": samples, "vmaf_combined": v, "cambi_combined": c, "quality_accepted": accepted, "video_target_kbps": p.X265.BitrateKbps, "audio_target_kbps": 160, "production_jobs_submitted": 0})
	t.Logf("completed isolated source experiment: quality accepted=%v", accepted)
}
