package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
	"github.com/jakenesler/navigatorr/transcode/selector"
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
		if reason := modeSourceReason(mode, rep, false); reason != "" {
			t.Fatalf("%s: cover art made the source %q", mode, reason)
		}
	}
	if !sharedCalibrationSourceSupported(rep) {
		t.Fatal("shared calibration rejected a source with cover art")
	}
	rep.Video[1].Codec = "hevc"
	if modeSourceReason("x265_preserve", rep, false) != "already_target_codec" {
		t.Fatal("codec gate read the cover instead of the main video")
	}
	rep.Video = append(rep.Video, rep.Video[1])
	if modeSourceReason("size", rep, false) != "unsupported_source" || sharedCalibrationSourceSupported(rep) {
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
	caps := transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, CoverArt: true, Encoders: map[string]bool{"libx265": true}, Filters: map[string]bool{"ssim": true}}
	if _, err := buildBenchmarkRequest(ec, "/media/series/Show/S01E01.mkv", coverArtReport(), opt, caps); err != nil {
		t.Fatalf("benchmark request read the cover as the source video: %v", err)
	}
	caps.CoverArt = false
	if _, err := buildBenchmarkRequest(ec, "/media/series/Show/S01E01.mkv", coverArtReport(), opt, caps); err == nil || !strings.Contains(err.Error(), "cover art") {
		t.Fatalf("old worker benchmarked a cover-art source: %v", err)
	}
	plain := coverArtReport()
	plain.Video = plain.Video[1:]
	if _, err := buildBenchmarkRequest(ec, "/media/series/Show/S01E01.mkv", plain, opt, caps); err != nil {
		t.Fatalf("cover-art capability required for a source without cover: %v", err)
	}
}

// Against a worker without the cover_art capability every source-class gate
// must keep the old behaviour: the cover counts as a second video stream.
func TestCoverArtGatesFallBackWithoutWorkerCapability(t *testing.T) {
	unsupported := func() bool { return false }
	legacy := coverArtGateReport(coverArtReport(), unsupported)
	if len(legacy.MainVideo()) != 2 || len(legacy.CoverArt()) != 0 {
		t.Fatalf("legacy view: main=%d cover=%d", len(legacy.MainVideo()), len(legacy.CoverArt()))
	}
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		if reason := modeSourceReason(mode, legacy, false); reason != "unsupported_source" {
			t.Fatalf("%s against an old worker: %q", mode, reason)
		}
	}
	if sharedCalibrationSourceSupported(legacy) {
		t.Fatal("shared calibration admitted cover art against an old worker")
	}
	if res := selector.Select(selector.Input{Report: *legacy, MinSavingsPercent: 15}); res.Decision != selector.DecisionReview {
		t.Fatalf("selector against an old worker: %s %v", res.Decision, res.Reasons)
	}
	original := coverArtReport()
	coverArtGateReport(original, unsupported)
	if len(original.CoverArt()) != 1 {
		t.Fatal("legacy view mutated the source report")
	}
	if rep := coverArtGateReport(original, func() bool { return true }); rep != original {
		t.Fatal("capable worker did not get the cover-aware report")
	}
	plain := coverArtReport()
	plain.Video = plain.Video[1:]
	if rep := coverArtGateReport(plain, func() bool { t.Fatal("worker asked for a source without cover art"); return false }); rep != plain {
		t.Fatal("source without cover art changed")
	}
}

func TestWorkerSupportsCoverArtFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps func(context.Context) (transcode.WorkerCapabilities, error)
		want bool
	}{
		{"advertised", func(context.Context) (transcode.WorkerCapabilities, error) {
			return transcode.WorkerCapabilities{CoverArt: true}, nil
		}, true},
		{"old worker", func(context.Context) (transcode.WorkerCapabilities, error) {
			return transcode.WorkerCapabilities{}, nil
		}, false},
		{"unreachable", func(context.Context) (transcode.WorkerCapabilities, error) {
			return transcode.WorkerCapabilities{CoverArt: true}, errors.New("offline")
		}, false},
	} {
		e := NewEngine(EngineDeps{Transcode: &mockTranscodeExecutor{capabilitiesFunc: tc.caps}})
		if got := e.workerSupportsCoverArt(context.Background()); got != tc.want {
			t.Fatalf("%s: got %v", tc.name, got)
		}
	}
	if NewEngine(EngineDeps{}).workerSupportsCoverArt(context.Background()) {
		t.Fatal("no worker configured but cover art supported")
	}
	calls := 0
	e := NewEngine(EngineDeps{Transcode: &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		calls++
		return transcode.WorkerCapabilities{CoverArt: true}, nil
	}}})
	supported := e.cachedCoverArtSupport(context.Background())
	if !supported() || !supported() || calls != 1 {
		t.Fatalf("cached support asked the worker %d times", calls)
	}
}

func TestSubmitRefusesCoverArtForOldWorker(t *testing.T) {
	for _, tc := range []struct {
		name       string
		coverArt   bool
		wantSubmit bool
	}{
		{"old worker", false, false},
		{"cover-art worker", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
				return transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, CoverArt: tc.coverArt}, nil
			}}
			e := NewEngine(EngineDeps{Transcode: mock})
			ec := testReconcileEC("job-cover-art")
			ec.State["resolved_path"] = "/media/series/Show/S01E01.mkv"
			ec.State["original"] = map[string]any{"video": coverArtReport().Video}
			res, err := e.stepTranscodeSubmit(context.Background(), ec)
			if err != nil {
				t.Fatal(err)
			}
			submitted := atomic.LoadInt32(&mock.submitCalls) > 0
			if submitted != tc.wantSubmit {
				t.Fatalf("submitted=%v status=%s error=%s", submitted, res.Status, res.Error)
			}
			if !tc.wantSubmit && (res.Status != StepFailed || !strings.Contains(res.Error, "cover art")) {
				t.Fatalf("status=%s error=%s", res.Status, res.Error)
			}
		})
	}
}

func TestValidateCandidateDetailedRequiresCoverArtKept(t *testing.T) {
	original := coverArtReport()
	for _, tc := range []struct {
		name, coverStream, decision string
		wantStatus                  StepStatus
		wantReason                  string
	}{
		{"kept", `,{"index": 2, "codec_type": "video", "codec_name": "mjpeg", "width": 600, "height": 900, "disposition": {"attached_pic": 1}, "tags": {"filename": "cover.jpg", "mimetype": "image/jpeg"}}`, "", StepCompleted, ""},
		{"lost", "", "", StepWaitingDecision, "Cover art count mismatch"},
		// An old worker that maps 0:v? re-encodes the cover into a second
		// programme video track; that must fail even when the loss is accepted.
		{"re-encoded as video", `,{"index": 2, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 600, "height": 900}`, "", StepFailed, "Video stream count mismatch"},
		{"re-encoded as video, loss accepted", `,{"index": 2, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 600, "height": 900}`, "accept_loss", StepFailed, "Video stream count mismatch"},
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
			ec := &ExecutionContext{Decision: tc.decision, Inputs: map[string]any{}, State: map[string]any{
				"candidate_path": candidate,
				"plan":           &transcode.Plan{VideoCodec: "libx265", AudioMode: "copy", PreserveAttachments: true},
				"original":       map[string]any{"size_bytes": int64(1 << 30), "duration_sec": 1420.0, "video": original.Video, "audio": []mediainspect.DetailedStream{{Index: 2, Kind: "audio", Codec: "aac", Channels: 2}}},
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

func TestModePreflightAdmitsCoverArtOnlyForCapableWorker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		coverArt bool
		wantSkip bool
	}{
		{"old worker", false, true},
		{"cover-art worker", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "S01E01.mkv")
			probe := filepath.Join(dir, "ffprobe")
			if err := os.WriteFile(file, make([]byte, 100), 0o600); err != nil {
				t.Fatal(err)
			}
			script := `#!/bin/sh
cat <<'JSON'
{"streams":[{"index":0,"codec_type":"video","codec_name":"mjpeg","pix_fmt":"yuvj420p","width":600,"height":900,"disposition":{"attached_pic":1},"tags":{"filename":"cover.jpg","mimetype":"image/jpeg"}},{"index":1,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080,"avg_frame_rate":"24/1","r_frame_rate":"24/1","bits_per_raw_sample":"8"}],"format":{"format_name":"matroska","duration":"100","size":"100"},"chapters":[]}
JSON
`
			if err := os.WriteFile(probe, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			caps, _ := (&mockTranscodeExecutor{}).Capabilities(context.Background())
			caps.CoverArt = tc.coverArt
			worker := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) { return caps, nil }}
			e, st := setupTranscodeEngine(t, worker, probe, []string{dir}, []string{dir}, false)
			defer st.Close()
			ec := &ExecutionContext{InstanceID: "cover-art-mode", ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "size"}, State: map[string]any{}}
			seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
			result, err := e.stepTranscodePreflight(context.Background(), ec)
			if err != nil || result.Status != StepCompleted {
				t.Fatalf("preflight %+v %v", result, err)
			}
			skipped := getBool(ec.State, "skip_transcode") && getString(ec.State, "reason_code") == "unsupported_source"
			if skipped != tc.wantSkip {
				t.Fatalf("skipped=%v reason=%q", skipped, getString(ec.State, "reason_code"))
			}
		})
	}
}
