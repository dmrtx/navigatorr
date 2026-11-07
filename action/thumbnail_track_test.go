package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// previewTrackReport is a source whose chapter thumbnails are a second mjpeg
// video track at well under one frame per second, listed after the programme.
func previewTrackReport() *mediainspect.DetailedReport {
	return &mediainspect.DetailedReport{Probed: true, Container: "mov", DurationSec: 8682, SizeBytes: 12 << 30, Video: []mediainspect.DetailedStream{
		{Index: 0, Kind: "video", Codec: "h264", PixelFormat: "yuv420p", Width: 1920, Height: 804, BitDepth: 8, FPS: 23.976, Disposition: map[string]int{"default": 1}},
		{Index: 11, Kind: "video", Codec: "mjpeg", PixelFormat: "yuvj420p", Width: 640, Height: 268, BitDepth: 8, FPS: 0.0056, Disposition: map[string]int{}},
	}}
}

func TestModeGatesIgnorePreviewTrack(t *testing.T) {
	rep := previewTrackReport()
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		if reason := modeSourceReason(mode, rep, false); reason != "" {
			t.Fatalf("%s: a preview track made the source %q", mode, reason)
		}
	}
	if reason := audioOnlySourceReason(rep); reason != "" {
		t.Fatalf("audio only: a preview track made the source %q", reason)
	}
	// A second real video stream is still refused.
	rep.Video = append(rep.Video, mediainspect.DetailedStream{Index: 12, Kind: "video", Codec: "h264", BitDepth: 8, FPS: 24})
	if modeSourceReason("size", rep, false) != "unsupported_source" {
		t.Fatal("two programme video streams accepted")
	}
}

func TestFinalValidationRequestRefusesPreviewTrackForOldWorker(t *testing.T) {
	ec := &ExecutionContext{InstanceID: "preview-track", Inputs: map[string]any{}, State: map[string]any{"plan": &transcode.Plan{VideoCodec: "libx265", Quality: 20, PreserveAttachments: true}}}
	opt := &recipe.OptimizationPolicy{Sampling: &recipe.SamplingPolicy{SampleCount: 3, SampleSeconds: 5, Positions: []float64{0.2, 0.5, 0.8}}, Quality: &recipe.QualityPolicy{PreferredMetric: "ssim"}}
	caps := transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, ThumbnailTracks: true, Encoders: map[string]bool{"libx265": true}, Filters: map[string]bool{"ssim": true}}
	if _, err := finalRequestForPlan(ec, "/media/movies/Movie/Movie.mp4", previewTrackReport(), opt, caps); err != nil {
		t.Fatalf("final validation request read the preview track as the source video: %v", err)
	}
	caps.ThumbnailTracks = false
	if _, err := finalRequestForPlan(ec, "/media/movies/Movie/Movie.mp4", previewTrackReport(), opt, caps); err == nil || !strings.Contains(err.Error(), "preview image") {
		t.Fatalf("old worker was sent a preview-track source: %v", err)
	}
	plain := previewTrackReport()
	plain.Video = plain.Video[:1]
	if _, err := finalRequestForPlan(ec, "/media/movies/Movie/Movie.mp4", plain, opt, caps); err != nil {
		t.Fatalf("thumbnail_tracks capability required for a source without a preview track: %v", err)
	}
}

func TestSubmitRefusesPreviewTrackForOldWorker(t *testing.T) {
	for _, tc := range []struct {
		name       string
		capability bool
		wantSubmit bool
	}{
		{"old worker", false, false},
		{"thumbnail-track worker", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
				return transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, ThumbnailTracks: tc.capability}, nil
			}}
			e := NewEngine(EngineDeps{Transcode: mock})
			ec := testReconcileEC("job-preview-track")
			ec.State["resolved_path"] = "/media/movies/Movie/Movie.mp4"
			ec.State["original"] = map[string]any{"video": previewTrackReport().Video}
			res, err := e.stepTranscodeSubmit(context.Background(), ec)
			if err != nil {
				t.Fatal(err)
			}
			submitted := atomic.LoadInt32(&mock.submitCalls) > 0
			if submitted != tc.wantSubmit {
				t.Fatalf("submitted=%v status=%s error=%s", submitted, res.Status, res.Error)
			}
			if !tc.wantSubmit && (res.Status != StepFailed || !strings.Contains(res.Error, "preview image")) {
				t.Fatalf("status=%s error=%s", res.Status, res.Error)
			}
		})
	}
}

// The candidate is held to the source's programme video count, which does not
// include the preview track the output leaves out. An old worker that encoded
// the track as a second video stream fails even when the loss is accepted.
func TestValidateCandidateDetailedLeavesPreviewTrackOut(t *testing.T) {
	original := previewTrackReport()
	for _, tc := range []struct {
		name, extraStream, decision string
		wantStatus                  StepStatus
		wantReason                  string
	}{
		{"left out", "", "", StepCompleted, ""},
		{"re-encoded as video", `,{"index": 2, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 640, "height": 268}`, "", StepFailed, "Video stream count mismatch"},
		{"re-encoded as video, loss accepted", `,{"index": 2, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 640, "height": 268}`, "accept_loss", StepFailed, "Video stream count mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			candidate := filepath.Join(dir, "candidate.mkv")
			if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
				t.Fatal(err)
			}
			probe := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 1920, "height": 804, "bits_per_raw_sample": "8", "avg_frame_rate": "24000/1001"},{"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2}` + tc.extraStream + `], "format": {"format_name": "matroska,webm", "duration": "8682"}}`
			ffprobe := filepath.Join(dir, "ffprobe")
			if err := os.WriteFile(ffprobe, []byte(probeScriptWithPackets(t, probe)), 0o700); err != nil {
				t.Fatal(err)
			}
			e := NewEngine(EngineDeps{Ffprobe: ffprobe})
			ec := &ExecutionContext{Decision: tc.decision, Inputs: map[string]any{}, State: map[string]any{
				"candidate_path": candidate,
				"plan":           &transcode.Plan{VideoCodec: "libx265", AudioMode: "copy", PreserveAttachments: true},
				"original":       map[string]any{"size_bytes": int64(12 << 30), "duration_sec": 8682.0, "video": original.Video, "audio": []mediainspect.DetailedStream{{Index: 1, Kind: "audio", Codec: "aac", Channels: 2}}},
			}}
			result, err := e.validateCandidateDetailed(context.Background(), ec)
			if err != nil || result.Status != tc.wantStatus {
				t.Fatalf("status %s (%s%s): %v", result.Status, result.Error, result.WaitingReason, err)
			}
			if !strings.Contains(result.WaitingReason+result.Error, tc.wantReason) {
				t.Fatalf("reason %q, want %q", result.WaitingReason+result.Error, tc.wantReason)
			}
		})
	}
}
