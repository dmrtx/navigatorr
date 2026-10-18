package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// coverArtReport puts the cover first so a gate that reads Video[0] or counts
// every video stream fails.
func coverArtReport() *mediainspect.DetailedReport {
	return &mediainspect.DetailedReport{Probed: true, Container: "matroska", DurationSec: 1420, SizeBytes: 1 << 30, Video: []mediainspect.DetailedStream{
		{Index: 0, Kind: "video", Codec: "mjpeg", PixelFormat: "yuvj420p", Width: 600, Height: 900, Disposition: map[string]int{"attached_pic": 1}},
		{Index: 1, Kind: "video", Codec: "h264", PixelFormat: "yuv420p", Width: 1920, Height: 1080, BitDepth: 8, FPS: 23.976, Disposition: map[string]int{"default": 1}},
	}}
}

func TestModeAndCalibrationGatesIgnoreCoverArt(t *testing.T) {
	rep := coverArtReport()
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		if reason := modeSourceReason(mode, rep); reason != "" {
			t.Fatalf("%s: cover art made the source %q", mode, reason)
		}
	}
	if !sharedCalibrationSourceSupported(rep) {
		t.Fatal("shared calibration rejected a source with cover art")
	}
	rep.Video[1].Codec = "hevc"
	if modeSourceReason("x265_preserve", rep) != "already_target_codec" {
		t.Fatal("codec gate read the cover instead of the main video")
	}
	rep.Video = append(rep.Video, rep.Video[1])
	if modeSourceReason("size", rep) != "unsupported_source" || sharedCalibrationSourceSupported(rep) {
		t.Fatal("two programme video streams accepted")
	}
	rep = coverArtReport()
	rep.Video[1].BitDepth = 10
	if sharedCalibrationSourceSupported(rep) || sharedCalibrationSourceSupported(nil) {
		t.Fatal("calibration class widened")
	}
}

func TestBuildBenchmarkRequestUsesMainVideoBesideCoverArt(t *testing.T) {
	ec := &ExecutionContext{InstanceID: "cover-art", Inputs: map[string]any{}, State: map[string]any{"plan": &transcode.Plan{VideoCodec: "libx265", Quality: 20, PreserveAttachments: true}}}
	opt := &recipe.OptimizationPolicy{Sampling: &recipe.SamplingPolicy{SampleCount: 1, SampleSeconds: 5, Positions: []float64{0.5}}, Search: &recipe.SearchPolicy{MaxCandidates: 1, QualityValues: []int{20}}, Quality: &recipe.QualityPolicy{PreferredMetric: "ssim"}}
	caps := transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, Encoders: map[string]bool{"libx265": true}, Filters: map[string]bool{"ssim": true}}
	if _, err := buildBenchmarkRequest(ec, "/media/series/Show/S01E01.mkv", coverArtReport(), opt, caps); err != nil {
		t.Fatalf("benchmark request read the cover as the source video: %v", err)
	}
}

func TestValidateCandidateDetailedRequiresCoverArtKept(t *testing.T) {
	original := coverArtReport()
	for _, tc := range []struct {
		name, coverStream string
		wantStatus        StepStatus
	}{
		{"kept", `,{"index": 2, "codec_type": "video", "codec_name": "mjpeg", "width": 600, "height": 900, "disposition": {"attached_pic": 1}, "tags": {"filename": "cover.jpg", "mimetype": "image/jpeg"}}`, StepCompleted},
		{"lost", "", StepWaitingDecision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			candidate := filepath.Join(dir, "candidate.mkv")
			if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
				t.Fatal(err)
			}
			probe := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bits_per_raw_sample": "8"},{"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2}` + tc.coverStream + `], "format": {"format_name": "matroska,webm", "duration": "1420"}}`
			ffprobe := filepath.Join(dir, "ffprobe")
			if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\ncat <<'JSON'\n"+probe+"\nJSON\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			e := NewEngine(EngineDeps{Ffprobe: ffprobe})
			ec := &ExecutionContext{Inputs: map[string]any{}, State: map[string]any{
				"candidate_path": candidate,
				"plan":           &transcode.Plan{VideoCodec: "libx265", AudioMode: "copy", PreserveAttachments: true},
				"original":       map[string]any{"size_bytes": int64(1 << 30), "duration_sec": 1420.0, "video": original.Video, "audio": []mediainspect.DetailedStream{{Index: 2, Kind: "audio", Codec: "aac", Channels: 2}}},
			}}
			result, err := e.validateCandidateDetailed(context.Background(), ec)
			if err != nil || result.Status != tc.wantStatus {
				t.Fatalf("status %s (%s%s): %v", result.Status, result.Error, result.WaitingReason, err)
			}
			if tc.wantStatus == StepWaitingDecision && !strings.Contains(result.WaitingReason, "Cover art count mismatch") {
				t.Fatalf("waiting reason: %s", result.WaitingReason)
			}
		})
	}
}
