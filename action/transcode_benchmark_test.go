package action

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

const standard8BitCandidateJSON = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bit_rate": "3000000"},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "subtitle", "codec_name": "subrip", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "350000000", "bit_rate": "3000000"},
  "chapters": []
}`

const standard8BitNoSubCandidateJSON = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bit_rate": "3000000"},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "jpn"}}
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "350000000", "bit_rate": "3000000"},
  "chapters": []
}`

const standard10BitCandidateJSON = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main 10", "pix_fmt": "yuv420p10le", "width": 1920, "height": 1080, "bit_rate": "3000000"},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "subtitle", "codec_name": "subrip", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "350000000", "bit_rate": "3000000"},
  "chapters": []
}`

func createSmartFakeFFprobeScript(t *testing.T, sourceJSON string) string {
	t.Helper()
	dir := t.TempDir()
	probePath := filepath.Join(dir, "ffprobe")
	candJSON := standard8BitCandidateJSON
	if strings.Contains(sourceJSON, "yuv420p10le") || strings.Contains(sourceJSON, "Main 10") {
		candJSON = standard10BitCandidateJSON
	} else if !strings.Contains(sourceJSON, `"codec_type": "subtitle"`) {
		candJSON = standard8BitNoSubCandidateJSON
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *candidate*)
cat << 'JSON'
%s
JSON
  ;;
  *)
cat << 'JSON'
%s
JSON
  ;;
esac
`, candJSON, sourceJSON)
	if err := os.WriteFile(probePath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fake ffprobe: %v", err)
	}
	return probePath
}

func setupBenchmarkTestEnv(t *testing.T, mock *mockTranscodeExecutor, ffprobeJSON string, optPolicy *recipe.OptimizationPolicy) (*Engine, *store.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	probePath := createSmartFakeFFprobeScript(t, ffprobeJSON)

	mediaFile := filepath.Join(dir, "TestEpisode.mkv")
	if err := os.WriteFile(mediaFile, []byte("fake-source-media-bytes-for-benchmark-testing"), 0644); err != nil {
		t.Fatalf("writing fake media file: %v", err)
	}

	engine, st := setupTranscodeEngine(t, mock, probePath, []string{dir}, []string{dir}, false)

	if optPolicy != nil {
		engine.Deps().Config.Transcode.Profiles = map[string]config.TranscodeProfileConfig{
			"opt-vt": {
				Container: "mkv",
				Video: config.VideoProfileConfig{
					Codec:   "hevc_videotoolbox",
					Quality: 65,
				},
				Audio:        config.AudioProfileConfig{Mode: "copy"},
				Subtitles:    config.SubtitleProfileConfig{Mode: "preserve", ConvertIncompatible: true},
				Preserve:     config.PreserveProfileConfig{Metadata: true, Chapters: true, Attachments: true},
				Optimization: optPolicy,
			},
		}
	}

	return engine, st, mediaFile, dir
}

const standard8BitProbeJSON = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "profile": "High", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bit_rate": "5000000"},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "subtitle", "codec_name": "subrip", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "750000000", "bit_rate": "5000000"},
  "chapters": []
}`

const standard10BitProbeJSON = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main 10", "pix_fmt": "yuv420p10le", "width": 1920, "height": 1080, "bit_rate": "6000000"},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "subtitle", "codec_name": "subrip", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "900000000", "bit_rate": "6000000"},
  "chapters": []
}`

const hdr10ProbeJSON = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main 10", "pix_fmt": "yuv420p10le", "width": 3840, "height": 2160, "color_transfer": "smpte2084", "color_primaries": "bt2020", "color_space": "bt2020nc"}
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "1500000000"},
  "chapters": []
}`

const dvSideDataOnlyProbeJSON = `{
  "streams": [
    {
      "index": 0,
      "codec_type": "video",
      "codec_name": "hevc",
      "profile": "Main 10",
      "pix_fmt": "yuv420p10le",
      "width": 1920,
      "height": 1080,
      "color_transfer": "bt709",
      "color_primaries": "bt709",
      "color_space": "bt709",
      "side_data_list": [
        {"side_data_type": "DOVI configuration record", "dv_version_major": 1, "dv_profile": 8}
      ]
    }
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "900000000"},
  "chapters": []
}`

const doviTagOnlyProbeJSON = `{
  "streams": [
    {
      "index": 0,
      "codec_type": "video",
      "codec_name": "hevc",
      "profile": "Main 10",
      "pix_fmt": "yuv420p10le",
      "width": 1920,
      "height": 1080,
      "color_transfer": "bt709",
      "color_primaries": "bt709",
      "color_space": "bt709",
      "tags": {"dovi_profile": "5"}
    }
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "900000000"},
  "chapters": []
}`

const hdrMasteringSideDataOnlyProbeJSON = `{
  "streams": [
    {
      "index": 0,
      "codec_type": "video",
      "codec_name": "hevc",
      "profile": "Main 10",
      "pix_fmt": "yuv420p10le",
      "width": 1920,
      "height": 1080,
      "color_transfer": "bt709",
      "color_primaries": "bt709",
      "color_space": "bt709",
      "side_data_list": [
        {"side_data_type": "Mastering display metadata", "red_x": "34000/50000"}
      ]
    }
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "900000000"},
  "chapters": []
}`

const standard10BitSDRProbeJSON = `{
  "streams": [
    {
      "index": 0,
      "codec_type": "video",
      "codec_name": "hevc",
      "profile": "Main 10",
      "pix_fmt": "yuv420p10le",
      "width": 1920,
      "height": 1080,
      "color_transfer": "bt709",
      "color_primaries": "bt709",
      "color_space": "bt709"
    },
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "subtitle", "codec_name": "subrip", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "900000000"},
  "chapters": []
}`

// 1. benchmark_transcode success with winner
func TestBenchmarkTranscode_SuccessWithWinner(t *testing.T) {
	var capturedReq transcode.BenchmarkRequest
	mock := &mockTranscodeExecutor{
		benchmarkSubmitFunc: func(ctx context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
			capturedReq = req
			return transcode.BenchmarkJob{ID: req.ID}, nil
		},
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner: &transcode.BenchmarkWinner{
						CandidateID:         "cand_q70",
						CandidateIndex:      2,
						Quality:             70,
						VideoProfile:        "main",
						PixelFormat:         "yuv420p",
						ExpectedBitDepth:    8,
						MetricType:          "vmaf",
						Score:               96.8,
						TargetReached:       true,
						MinimumMet:          true,
						EstimatedVideoBytes: 200000000,
						EstimatedTotalBytes: 250000000,
						EstimatedTotalMB:    250.0,
						SavingsPercent:      50.0,
					},
					DecisionReason: "candidate cand_q70 reached target quality 96.0 with smallest size",
				},
			}, nil
		},
	}

	engine, _, mediaFile, dir := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s (error: %s)", res.Status, res.Error)
	}
	if getBool(res.Outputs, "has_winner") != true {
		t.Errorf("expected has_winner=true in outputs")
	}
	if getString(res.Outputs, "winner_candidate_id") != "cand_q70" {
		t.Errorf("expected winner_candidate_id=cand_q70, got %s", getString(res.Outputs, "winner_candidate_id"))
	}
	if getInt(res.Outputs, "winner_quality") != 70 {
		t.Errorf("expected winner_quality=70, got %d", getInt(res.Outputs, "winner_quality"))
	}
	if getString(res.Outputs, "plan_digest") == "" {
		t.Errorf("expected non-empty plan_digest in outputs")
	}

	// Invariants: candidate-only, never submits full transcode, never creates permanent candidate
	if mock.submitCalls != 0 {
		t.Errorf("benchmark_transcode must never submit full transcode, got submitCalls=%d", mock.submitCalls)
	}
	candidatesDir := filepath.Join(dir, ".navigatorr-candidates")
	if _, err := os.Stat(candidatesDir); !os.IsNotExist(err) {
		t.Errorf("benchmark_transcode must never create .navigatorr-candidates directory")
	}
	// Source media must remain untouched
	sourceBytes, _ := os.ReadFile(mediaFile)
	if string(sourceBytes) != "fake-source-media-bytes-for-benchmark-testing" {
		t.Errorf("source media was unexpectedly mutated")
	}
	// Verify captured request
	if capturedReq.ID == "" || capturedReq.Metric != "vmaf" {
		t.Errorf("unexpected captured benchmark request: %+v", capturedReq)
	}
}

// 2. benchmark_transcode no-winner
func TestBenchmarkTranscode_NoWinner(t *testing.T) {
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner:         nil,
					DecisionReason: "no candidate met minimum quality threshold 95.0",
				},
			}, nil
		},
	}

	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted for benchmark reporting no winner, got %s", res.Status)
	}
	if getBool(res.Outputs, "has_winner") != false {
		t.Errorf("expected has_winner=false in outputs")
	}
	if res.Outputs["winner"] != nil {
		t.Errorf("expected winner=nil in outputs, got %+v", res.Outputs["winner"])
	}
	if getString(res.Outputs, "decision_reason") != "no candidate met minimum quality threshold 95.0" {
		t.Errorf("unexpected decision_reason: %s", getString(res.Outputs, "decision_reason"))
	}
	if mock.submitCalls != 0 {
		t.Errorf("benchmark_transcode must never submit full transcode")
	}
}

// 3. benchmark worker failure
func TestBenchmarkTranscode_WorkerFailure(t *testing.T) {
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusFailed,
				Error:           "ffmpeg exited with code 1: hardware encoder initialization failed",
			}, nil
		},
	}

	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected engine error: %v", err)
	}

	if res.Status != StatusFailed {
		t.Fatalf("expected StatusFailed when benchmark fails, got %s", res.Status)
	}
	if mock.submitCalls != 0 {
		t.Errorf("no full transcode submit should happen on benchmark failure")
	}
}

// 4. cancellation propagation
func TestBenchmarkTranscode_CancellationPropagation(t *testing.T) {
	var cancelCalled int32
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusRunning,
				Progress:        25.0,
			}, nil
		},
		benchmarkCancelFunc: func(ctx context.Context, jobID string) error {
			atomic.AddInt32(&cancelCalled, 1)
			return nil
		},
	}

	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	// Run initially -> enters waiting_external
	res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected StatusWaitingExternal, got %s", res.Status)
	}

	// Cancel via Engine.Cancel -> should propagate to BenchmarkCancel
	resCancel, err := engine.Cancel(context.Background(), res.ID, "user cancelled benchmark")
	if err != nil {
		t.Fatalf("Cancel error: %v", err)
	}
	if resCancel.Status != StatusCancelled {
		t.Errorf("expected StatusCancelled, got %s", resCancel.Status)
	}
	if atomic.LoadInt32(&cancelCalled) == 0 {
		t.Errorf("expected BenchmarkCancel to be called upon action cancellation")
	}
}

// 5. optimization-disabled transcode_media unchanged regression
func TestTranscodeMedia_OptimizationDisabled_UnchangedRegression(t *testing.T) {
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			// Fake write candidate output
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-media"), 0644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusCompleted,
				Progress: 100,
			}, nil
		},
	}

	// Optimization is nil (disabled)
	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, nil)

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "hevc-vt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s (error: %s)", res.Status, res.Error)
	}
	if mock.benchmarkSubmitCalls != 0 {
		t.Errorf("optimization-disabled transcode_media must NOT call BenchmarkSubmit, got %d", mock.benchmarkSubmitCalls)
	}
	if mock.submitCalls != 1 {
		t.Errorf("expected exactly 1 full transcode Submit call, got %d", mock.submitCalls)
	}
}

// 6. optimization-enabled transcode_media uses winner concrete knobs and concrete PlanDigest
func TestTranscodeMedia_OptimizationEnabled_UsesWinnerConcreteKnobsAndPlanDigest(t *testing.T) {
	var fullTranscodeReq transcode.Request
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner: &transcode.BenchmarkWinner{
						CandidateID:         "cand_q75",
						CandidateIndex:      3,
						Quality:             75,
						VideoProfile:        "main",
						PixelFormat:         "yuv420p",
						ExpectedBitDepth:    8,
						MetricType:          "vmaf",
						Score:               97.2,
						TargetReached:       true,
						MinimumMet:          true,
						EstimatedVideoBytes: 300000000,
						EstimatedTotalBytes: 350000000,
						EstimatedTotalMB:    350.0,
						SavingsPercent:      40.0,
					},
					DecisionReason: "winner cand_q75 optimal",
				},
			}, nil
		},
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			fullTranscodeReq = req
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-output"), 0644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusCompleted,
				Progress: 100,
			}, nil
		},
	}

	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s (error: %s)", res.Status, res.Error)
	}

	// Verify both benchmark and full transcode were executed
	if mock.benchmarkSubmitCalls != 1 {
		t.Errorf("expected 1 benchmark submit call, got %d", mock.benchmarkSubmitCalls)
	}
	if mock.submitCalls != 1 {
		t.Errorf("expected 1 full transcode submit call, got %d", mock.submitCalls)
	}

	// Concrete winning plan verification
	if fullTranscodeReq.Plan == nil {
		t.Fatalf("submitted transcode request has nil Plan")
	}
	if fullTranscodeReq.Plan.Quality != 75 {
		t.Errorf("submitted transcode plan quality = %d, want winner's quality 75", fullTranscodeReq.Plan.Quality)
	}
	if fullTranscodeReq.Plan.VideoProfile != "main" {
		t.Errorf("submitted transcode plan video profile = %s, want main", fullTranscodeReq.Plan.VideoProfile)
	}
	if fullTranscodeReq.Plan.PixelFormat != "yuv420p" {
		t.Errorf("submitted transcode plan pixel format = %s, want yuv420p", fullTranscodeReq.Plan.PixelFormat)
	}
	if fullTranscodeReq.Plan.ExpectedBitDepth != 8 {
		t.Errorf("submitted transcode plan expected bit depth = %d, want 8", fullTranscodeReq.Plan.ExpectedBitDepth)
	}

	// Verify PlanDigest matches the concrete winning plan
	expectedDigest, err := transcode.DigestPlan(fullTranscodeReq.Plan)
	if err != nil {
		t.Fatalf("DigestPlan error: %v", err)
	}
	if fullTranscodeReq.Plan.PlanDigest != expectedDigest {
		t.Errorf("PlanDigest %s does not match expected %s", fullTranscodeReq.Plan.PlanDigest, expectedDigest)
	}
}

// 7. no-winner prevents full transcode submit
func TestTranscodeMedia_OptimizationEnabled_NoWinner_PreventsFullTranscodeSubmit(t *testing.T) {
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner:         nil,
					DecisionReason: "all candidates failed quality gate",
				},
			}, nil
		},
	}

	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected engine error: %v", err)
	}

	if res.Status != StatusFailed {
		t.Fatalf("expected StatusFailed when no winner is found, got %s", res.Status)
	}
	if mock.submitCalls != 0 {
		t.Errorf("no-winner must strictly prevent full transcode submit, got %d submit calls", mock.submitCalls)
	}
}

// 8. replace_original=true rejected before benchmark/full submit
func TestTranscode_ReplaceOriginal_RejectedBeforeBenchmarkOrFullSubmit(t *testing.T) {
	mock := &mockTranscodeExecutor{}
	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	// Benchmark action
	resBench, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
		"path":             mediaFile,
		"profile":          "opt-vt",
		"replace_original": true,
	})
	if err != nil {
		t.Fatalf("unexpected engine error: %v", err)
	}
	if resBench.Status != StatusFailed {
		t.Errorf("benchmark_transcode with replace_original=true must fail, got %s", resBench.Status)
	}
	if mock.benchmarkSubmitCalls != 0 {
		t.Errorf("benchmark_submit must not be called when replace_original=true")
	}

	// Transcode media action
	resMedia, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":             mediaFile,
		"profile":          "opt-vt",
		"replace_original": true,
	})
	if err != nil {
		t.Fatalf("unexpected engine error: %v", err)
	}
	if resMedia.Status != StatusFailed {
		t.Errorf("transcode_media with replace_original=true must fail, got %s", resMedia.Status)
	}
	if mock.benchmarkSubmitCalls != 0 || mock.submitCalls != 0 {
		t.Errorf("neither benchmark nor transcode submit may be called when replace_original=true")
	}
}

// 9. invalid/missing capabilities
func TestTranscode_InvalidOrMissingCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		metric     string
		bitDepth   string // "8" or "10"
		capsMod    func(*transcode.WorkerCapabilities)
		wantErrSub string
	}{
		{
			name: "protocol version mismatch",
			capsMod: func(c *transcode.WorkerCapabilities) {
				c.ProtocolVersion = 999
			},
			wantErrSub: "protocol version",
		},
		{
			name: "missing hevc_videotoolbox encoder",
			capsMod: func(c *transcode.WorkerCapabilities) {
				c.Encoders["hevc_videotoolbox"] = false
			},
			wantErrSub: "hevc_videotoolbox",
		},
		{
			name:   "missing libvmaf filter for vmaf metric",
			metric: "vmaf",
			capsMod: func(c *transcode.WorkerCapabilities) {
				c.Filters["libvmaf"] = false
			},
			wantErrSub: "libvmaf",
		},
		{
			name:   "missing ssim filter for ssim metric",
			metric: "ssim",
			capsMod: func(c *transcode.WorkerCapabilities) {
				c.Filters["ssim"] = false
			},
			wantErrSub: "ssim",
		},
		{
			name:       "10-bit source with vmaf metric fails closed",
			metric:     "vmaf",
			bitDepth:   "10",
			capsMod:    func(c *transcode.WorkerCapabilities) {},
			wantErrSub: "10-bit VMAF",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockTranscodeExecutor{
				capabilitiesFunc: func(ctx context.Context) (transcode.WorkerCapabilities, error) {
					caps := transcode.WorkerCapabilities{
						ProtocolVersion: transcode.WorkerProtocolVersion,
						Encoders:        map[string]bool{"hevc_videotoolbox": true},
						Filters:         map[string]bool{"scale": true, "libvmaf": true, "ssim": true},
					}
					tc.capsMod(&caps)
					return caps, nil
				},
			}

			probeJSON := standard8BitProbeJSON
			if tc.bitDepth == "10" {
				probeJSON = standard10BitProbeJSON
			}

			engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, probeJSON, &recipe.OptimizationPolicy{Enabled: true})

			inputs := map[string]any{
				"path":    mediaFile,
				"profile": "opt-vt",
			}
			if tc.metric != "" {
				inputs["metric"] = tc.metric
			}

			res, err := engine.Run(context.Background(), "benchmark_transcode", inputs)
			if err != nil {
				t.Fatalf("unexpected engine error: %v", err)
			}
			if res.Status != StatusFailed {
				t.Fatalf("expected StatusFailed for %s, got %s", tc.name, res.Status)
			}
			if mock.benchmarkSubmitCalls != 0 {
				t.Errorf("benchmark submit must not be called when capability validation fails")
			}
		})
	}
}

// 10. vmaf/ssim/both request mapping
func TestTranscode_MetricRequestMapping(t *testing.T) {
	metrics := []struct {
		inputMetric   string
		expectedReq   string
		shouldSucceed bool
	}{
		{inputMetric: "vmaf", expectedReq: "vmaf", shouldSucceed: true},
		{inputMetric: "ssim", expectedReq: "ssim", shouldSucceed: true},
		{inputMetric: "both", expectedReq: "both", shouldSucceed: true},
		{inputMetric: "invalid_metric", expectedReq: "", shouldSucceed: false},
	}

	for _, tc := range metrics {
		t.Run(tc.inputMetric, func(t *testing.T) {
			var recordedReq transcode.BenchmarkRequest
			mock := &mockTranscodeExecutor{
				benchmarkSubmitFunc: func(ctx context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
					recordedReq = req
					return transcode.BenchmarkJob{ID: req.ID}, nil
				},
				benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
					return transcode.BenchmarkStatus{
						ProtocolVersion: transcode.WorkerProtocolVersion,
						ID:              jobID,
						Status:          transcode.StatusCompleted,
						Decision: &transcode.BenchmarkDecision{
							Winner: &transcode.BenchmarkWinner{
								CandidateID: "c1", Quality: 65, Score: 96.0,
							},
						},
					}, nil
				},
			}

			engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

			res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
				"path":    mediaFile,
				"profile": "opt-vt",
				"metric":  tc.inputMetric,
			})
			if err != nil {
				t.Fatalf("unexpected engine error: %v", err)
			}

			if tc.shouldSucceed {
				if res.Status != StatusCompleted {
					t.Fatalf("expected completed for metric %s, got %s (error: %s)", tc.inputMetric, res.Status, res.Error)
				}
				if recordedReq.Metric != tc.expectedReq {
					t.Errorf("recordedReq.Metric = %s, want %s", recordedReq.Metric, tc.expectedReq)
				}
			} else {
				if res.Status != StatusFailed {
					t.Fatalf("expected failed for invalid metric %s, got %s", tc.inputMetric, res.Status)
				}
			}
		})
	}
}

// 11. 8/10-bit/HDR safety propagation
func TestTranscode_BitDepthAndHDRSafety(t *testing.T) {
	t.Run("8-bit source generates only 8-bit candidates", func(t *testing.T) {
		var recordedReq transcode.BenchmarkRequest
		mock := &mockTranscodeExecutor{
			benchmarkSubmitFunc: func(ctx context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
				recordedReq = req
				return transcode.BenchmarkJob{ID: req.ID}, nil
			},
			benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
				return transcode.BenchmarkStatus{
					ProtocolVersion: transcode.WorkerProtocolVersion,
					ID:              jobID,
					Status:          transcode.StatusCompleted,
					Decision:        &transcode.BenchmarkDecision{Winner: &transcode.BenchmarkWinner{CandidateID: "c1", Quality: 65}},
				}, nil
			},
		}

		engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
		res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
			"path":    mediaFile,
			"profile": "opt-vt",
		})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("run failed: %v, status: %s", err, res.Status)
		}
		for _, cand := range recordedReq.Candidates {
			if cand.VideoProfile != "main" || cand.PixelFormat != "yuv420p" {
				t.Errorf("8-bit candidate has illegal profile/pixfmt: %+v", cand)
			}
		}
	})

	t.Run("10-bit source with SSIM generates only 10-bit candidates", func(t *testing.T) {
		var recordedReq transcode.BenchmarkRequest
		mock := &mockTranscodeExecutor{
			benchmarkSubmitFunc: func(ctx context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
				recordedReq = req
				return transcode.BenchmarkJob{ID: req.ID}, nil
			},
			benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
				return transcode.BenchmarkStatus{
					ProtocolVersion: transcode.WorkerProtocolVersion,
					ID:              jobID,
					Status:          transcode.StatusCompleted,
					Decision:        &transcode.BenchmarkDecision{Winner: &transcode.BenchmarkWinner{CandidateID: "c1", Quality: 65, ExpectedBitDepth: 10}},
				}, nil
			},
		}

		engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard10BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
		res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
			"path":    mediaFile,
			"profile": "opt-vt",
			"metric":  "ssim",
		})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("run failed: %v, status: %s", err, res.Status)
		}
		for _, cand := range recordedReq.Candidates {
			if cand.VideoProfile != "main10" || cand.PixelFormat != "p010le" {
				t.Errorf("10-bit candidate has illegal profile/pixfmt: %+v", cand)
			}
		}
	})

	t.Run("HDR10 source fails closed", func(t *testing.T) {
		mock := &mockTranscodeExecutor{}
		engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, hdr10ProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
		res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
			"path":    mediaFile,
			"profile": "opt-vt",
		})
		if err != nil {
			t.Fatalf("unexpected engine error: %v", err)
		}
		if res.Status != StatusFailed {
			t.Fatalf("expected HDR source to fail closed, got %s", res.Status)
		}
		if mock.benchmarkSubmitCalls != 0 {
			t.Errorf("benchmark must not be submitted for HDR source")
		}
	})

	t.Run("DV side data only fails closed", func(t *testing.T) {
		mock := &mockTranscodeExecutor{}
		engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, dvSideDataOnlyProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
		res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
			"path":    mediaFile,
			"profile": "opt-vt",
		})
		if err != nil {
			t.Fatalf("unexpected engine error: %v", err)
		}
		if res.Status != StatusFailed {
			t.Fatalf("expected DV side-data-only to fail closed, got %s", res.Status)
		}
		if mock.benchmarkSubmitCalls != 0 {
			t.Errorf("benchmark must not be submitted for DV side data")
		}
	})

	t.Run("DOVI tag only fails closed", func(t *testing.T) {
		mock := &mockTranscodeExecutor{}
		engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, doviTagOnlyProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
		res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
			"path":    mediaFile,
			"profile": "opt-vt",
		})
		if err != nil {
			t.Fatalf("unexpected engine error: %v", err)
		}
		if res.Status != StatusFailed {
			t.Fatalf("expected DOVI tag-only to fail closed, got %s", res.Status)
		}
		if mock.benchmarkSubmitCalls != 0 {
			t.Errorf("benchmark must not be submitted for DOVI tags")
		}
	})

	t.Run("HDR mastering side data only fails closed", func(t *testing.T) {
		mock := &mockTranscodeExecutor{}
		engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, hdrMasteringSideDataOnlyProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
		res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
			"path":    mediaFile,
			"profile": "opt-vt",
		})
		if err != nil {
			t.Fatalf("unexpected engine error: %v", err)
		}
		if res.Status != StatusFailed {
			t.Fatalf("expected HDR mastering side-data to fail closed, got %s", res.Status)
		}
		if mock.benchmarkSubmitCalls != 0 {
			t.Errorf("benchmark must not be submitted for HDR mastering metadata")
		}
	})

	t.Run("ordinary SDR false positive regression passes", func(t *testing.T) {
		mock := &mockTranscodeExecutor{
			benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
				return transcode.BenchmarkStatus{
					ProtocolVersion: transcode.WorkerProtocolVersion,
					ID:              jobID,
					Status:          transcode.StatusCompleted,
					Decision:        &transcode.BenchmarkDecision{Winner: &transcode.BenchmarkWinner{CandidateID: "c1", Quality: 65, ExpectedBitDepth: 10}},
				}, nil
			},
		}
		engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard10BitSDRProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
		res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
			"path":    mediaFile,
			"profile": "opt-vt",
			"metric":  "ssim",
		})
		if err != nil {
			t.Fatalf("unexpected engine error: %v", err)
		}
		if res.Status != StatusCompleted {
			t.Fatalf("expected ordinary 10-bit SDR to succeed, got %s (error: %s)", res.Status, res.Error)
		}
		if mock.benchmarkSubmitCalls != 1 {
			t.Errorf("expected exactly 1 benchmark submit call for ordinary SDR, got %d", mock.benchmarkSubmitCalls)
		}
	})
}

// 12. persisted outputs survive action reload
func TestTranscode_PersistedOutputsSurviveActionReload(t *testing.T) {
	var statusCalls int32
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			call := atomic.AddInt32(&statusCalls, 1)
			if call == 1 {
				return transcode.BenchmarkStatus{
					ProtocolVersion: transcode.WorkerProtocolVersion,
					ID:              jobID,
					Status:          transcode.StatusRunning,
					Progress:        40.0,
				}, nil
			}
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner: &transcode.BenchmarkWinner{
						CandidateID:      "cand_q65",
						Quality:          65,
						ExpectedBitDepth: 8,
						Score:            96.2,
						MetricType:       "vmaf",
						EstimatedTotalMB: 400.0,
						SavingsPercent:   30.0,
					},
					DecisionReason: "winner selected",
				},
			}, nil
		},
	}

	mediaDir := t.TempDir()
	probePath := createFakeFFprobeScript(t, standard8BitProbeJSON)
	mediaFile := filepath.Join(mediaDir, "EpisodeReload.mkv")
	_ = os.WriteFile(mediaFile, []byte("reload-test-media"), 0644)

	dbPath := filepath.Join(t.TempDir(), "reload_action.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	resResolver, _ := fsop.NewResolver([]string{mediaDir}, []string{mediaDir})
	cfg := &config.Config{
		Media: config.MediaConfig{
			AllowedReadRoots:  []string{mediaDir},
			AllowedWriteRoots: []string{mediaDir},
			FfprobePath:       probePath,
		},
		Transcode: config.TranscodeConfig{
			Enabled: true,
			Profiles: map[string]config.TranscodeProfileConfig{
				"opt-vt": {
					Container:    "mkv",
					Video:        config.VideoProfileConfig{Codec: "hevc_videotoolbox", Quality: 65},
					Audio:        config.AudioProfileConfig{Mode: "copy"},
					Subtitles:    config.SubtitleProfileConfig{Mode: "preserve", ConvertIncompatible: true},
					Preserve:     config.PreserveProfileConfig{Metadata: true, Chapters: true, Attachments: true},
					Optimization: &recipe.OptimizationPolicy{Enabled: true},
				},
			},
		},
	}

	engine1 := NewEngine(EngineDeps{
		Store:     st,
		Config:    cfg,
		Fs:        resResolver,
		Ffprobe:   probePath,
		Transcode: mock,
	})

	res1, err := engine1.Run(context.Background(), "benchmark_transcode", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res1.Status != StatusWaitingExternal {
		t.Fatalf("expected waiting_external, got %s", res1.Status)
	}

	// Create new Engine instance pointing to the same store (simulating process restart)
	engine2 := NewEngine(EngineDeps{
		Store:     st,
		Config:    cfg,
		Fs:        resResolver,
		Ffprobe:   probePath,
		Transcode: mock,
	})

	// Verify status from reloaded engine
	stRes, err := engine2.Status(context.Background(), res1.ID)
	if err != nil {
		t.Fatalf("status error on reload: %v", err)
	}
	if stRes.Status != StatusWaitingExternal {
		t.Errorf("status on reload = %s, want waiting_external", stRes.Status)
	}

	// Resume on reloaded engine
	resumeRes, err := engine2.Resume(context.Background(), res1.ID, "", nil)
	if err != nil {
		t.Fatalf("resume error on reload: %v", err)
	}
	if resumeRes.Status != StatusCompleted {
		t.Fatalf("resume status = %s, want completed", resumeRes.Status)
	}
	if getBool(resumeRes.Outputs, "has_winner") != true {
		t.Errorf("expected has_winner=true after reload resume")
	}
	if getString(resumeRes.Outputs, "winner_candidate_id") != "cand_q65" {
		t.Errorf("expected winner cand_q65, got %s", getString(resumeRes.Outputs, "winner_candidate_id"))
	}
}

// 13. idempotent/retry behavior and worker busy
func TestTranscode_IdempotentResumeAndWorkerBusy(t *testing.T) {
	t.Run("worker busy handling", func(t *testing.T) {
		mock := &mockTranscodeExecutor{
			benchmarkSubmitFunc: func(ctx context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
				return transcode.BenchmarkJob{}, fmt.Errorf("worker busy: active job slot limit reached")
			},
		}

		engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})
		res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
			"path":                mediaFile,
			"profile":             "opt-vt",
			"surface_worker_busy": true,
		})
		if err != nil {
			t.Fatalf("unexpected engine error: %v", err)
		}
		if res.Status != StatusWaitingExternal {
			t.Fatalf("expected StatusWaitingExternal on worker_busy, got %s", res.Status)
		}
		if res.WaitingCondition != "worker_busy" {
			t.Errorf("expected WaitingCondition=worker_busy, got %s", res.WaitingCondition)
		}
		if res.Outputs["benchmark_attempt"] == nil {
			t.Errorf("expected benchmark_attempt in outputs, got nil")
		}
		if res.Outputs["benchmark_retry_count"] == nil {
			t.Errorf("expected benchmark_retry_count in outputs, got nil")
		}
		if res.Outputs["attempt"] != nil {
			t.Errorf("did not expect transcode 'attempt' in benchmark worker_busy outputs, got %v", res.Outputs["attempt"])
		}
		if res.Outputs["retry_count"] != nil {
			t.Errorf("did not expect transcode 'retry_count' in benchmark worker_busy outputs, got %v", res.Outputs["retry_count"])
		}
	})

	t.Run("idempotent submission reuse", func(t *testing.T) {
		var submitCount int32
		mock := &mockTranscodeExecutor{
			benchmarkSubmitFunc: func(ctx context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
				atomic.AddInt32(&submitCount, 1)
				return transcode.BenchmarkJob{ID: req.ID}, nil
			},
			benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
				return transcode.BenchmarkStatus{
					ProtocolVersion: transcode.WorkerProtocolVersion,
					ID:              jobID,
					Status:          transcode.StatusRunning,
					Progress:        50.0,
				}, nil
			},
		}

		engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

		res1, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
			"path":    mediaFile,
			"profile": "opt-vt",
		}, "idem-key-1")
		if err != nil || res1.Status != StatusWaitingExternal {
			t.Fatalf("initial run failed: %v, status: %s", err, res1.Status)
		}

		// Re-running with same idempotency key should return existing action without re-submitting
		res2, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
			"path":    mediaFile,
			"profile": "opt-vt",
		}, "idem-key-1")
		if err != nil {
			t.Fatalf("second run error: %v", err)
		}
		if res2.ID != res1.ID {
			t.Errorf("expected same action ID %s, got %s", res1.ID, res2.ID)
		}
		if atomic.LoadInt32(&submitCount) != 1 {
			t.Errorf("expected exactly 1 submit call, got %d", atomic.LoadInt32(&submitCount))
		}
	})
}

// 14. cancellation of transcode_media after benchmark completed while full transcode is active
func TestTranscodeMedia_Optimized_CancellationAfterBenchmarkDone_OnlyCancelsFullTranscode(t *testing.T) {
	var benchCancelCalls int32
	var fullCancelCalls int32
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner: &transcode.BenchmarkWinner{
						CandidateID:      "cand_q75",
						CandidateIndex:   3,
						Quality:          75,
						VideoProfile:     "main",
						PixelFormat:      "yuv420p",
						ExpectedBitDepth: 8,
						MetricType:       "vmaf",
						Score:            97.0,
						TargetReached:    true,
						MinimumMet:       true,
						SavingsPercent:   40.0,
					},
					DecisionReason: "optimal",
				},
			}, nil
		},
		benchmarkCancelFunc: func(ctx context.Context, jobID string) error {
			atomic.AddInt32(&benchCancelCalls, 1)
			return nil
		},
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			// In-progress full transcode keeps action in waiting_external
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusRunning,
				Progress: 40.0,
			}, nil
		},
		cancelFunc: func(ctx context.Context, jobID string) error {
			atomic.AddInt32(&fullCancelCalls, 1)
			return nil
		},
	}

	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	// Run initially -> completes benchmark, submits full transcode, enters waiting_external on wait_transcode
	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected run error: %v", err)
	}
	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected StatusWaitingExternal, got %s", res.Status)
	}

	// Verify benchmark is done and full transcode was submitted
	if mock.benchmarkSubmitCalls != 1 {
		t.Errorf("expected 1 benchmark submit call, got %d", mock.benchmarkSubmitCalls)
	}
	if mock.submitCalls != 1 {
		t.Errorf("expected 1 full transcode submit call, got %d", mock.submitCalls)
	}

	// Cancel the action while full transcode is active
	resCancel, err := engine.Cancel(context.Background(), res.ID, "user cancelled active transcode")
	if err != nil {
		t.Fatalf("Cancel error: %v", err)
	}
	if resCancel.Status != StatusCancelled {
		t.Errorf("expected StatusCancelled, got %s", resCancel.Status)
	}

	// BenchmarkCancel must NOT be called since benchmark_done was true
	if atomic.LoadInt32(&benchCancelCalls) != 0 {
		t.Errorf("BenchmarkCancel should NOT be called after benchmark is done, got %d calls", atomic.LoadInt32(&benchCancelCalls))
	}
	// Normal transcode Cancel MUST be called
	if atomic.LoadInt32(&fullCancelCalls) != 1 {
		t.Errorf("expected Cancel to be called exactly once for full transcode, got %d calls", atomic.LoadInt32(&fullCancelCalls))
	}
}

// 15. stepBenchmarkWait honors persisted benchmark_retry_not_before backoff
func TestBenchmarkWait_HonorsRetryNotBeforeBackoff(t *testing.T) {
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusRunning,
				Progress:        10.0,
			}, nil
		},
	}

	engine, _, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	// Initial run: starts benchmark, enters waiting_external
	res, err := engine.Run(context.Background(), "benchmark_transcode", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil || res.Status != StatusWaitingExternal {
		t.Fatalf("initial run error: %v, status: %s", err, res.Status)
	}

	// Persist benchmark_retry_not_before in action state 30 seconds into the future
	inst, err := engine.deps.Store.GetActionInstance(res.ID)
	if err != nil {
		t.Fatalf("getting action instance: %v", err)
	}
	ec := parseExecutionContext(inst, engine)
	ec.State["benchmark_retry_not_before"] = time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339Nano)
	stateBytes, _ := json.Marshal(ec.State)
	inst.StateJSON = string(stateBytes)
	if err := engine.deps.Store.UpdateActionInstance(*inst); err != nil {
		t.Fatalf("updating action instance: %v", err)
	}

	// Reset status calls counter
	atomic.StoreInt32(&mock.benchmarkStatusCalls, 0)

	// Resume action: backoff is active, must NOT call BenchmarkStatus
	resumeRes, err := engine.Resume(context.Background(), res.ID, "", nil)
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if resumeRes.Status != StatusWaitingExternal {
		t.Errorf("expected StatusWaitingExternal during backoff, got %s", resumeRes.Status)
	}
	if resumeRes.WaitingCondition != "benchmark_retry" {
		t.Errorf("expected waiting_condition=benchmark_retry, got %s", resumeRes.WaitingCondition)
	}
	if atomic.LoadInt32(&mock.benchmarkStatusCalls) != 0 {
		t.Errorf("BenchmarkStatus should NOT be called while backoff is active, got %d calls", atomic.LoadInt32(&mock.benchmarkStatusCalls))
	}

	// Now update backoff to the past (expired)
	inst, _ = engine.deps.Store.GetActionInstance(res.ID)
	ec = parseExecutionContext(inst, engine)
	ec.State["benchmark_retry_not_before"] = time.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339Nano)
	stateBytes, _ = json.Marshal(ec.State)
	inst.StateJSON = string(stateBytes)
	_ = engine.deps.Store.UpdateActionInstance(*inst)

	// Resume again: backoff expired, should call BenchmarkStatus
	resumeRes2, err := engine.Resume(context.Background(), res.ID, "", nil)
	if err != nil {
		t.Fatalf("resume2 error: %v", err)
	}
	if atomic.LoadInt32(&mock.benchmarkStatusCalls) != 1 {
		t.Errorf("BenchmarkStatus should be called after backoff expired, got %d calls", atomic.LoadInt32(&mock.benchmarkStatusCalls))
	}
	_ = resumeRes2
}

// 16. reload/resume coverage for optimized transcode_media across benchmark-completed boundary
func TestTranscodeMedia_Optimized_ReloadResumeAcrossBenchmarkCompletedBoundary(t *testing.T) {
	var fullTranscodeReq transcode.Request
	mock := &mockTranscodeExecutor{
		benchmarkStatusFunc: func(ctx context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				ID:              jobID,
				Status:          transcode.StatusCompleted,
				Progress:        100,
				Decision: &transcode.BenchmarkDecision{
					Winner: &transcode.BenchmarkWinner{
						CandidateID:         "cand_q72",
						CandidateIndex:      2,
						Quality:             72,
						VideoProfile:        "main",
						PixelFormat:         "yuv420p",
						ExpectedBitDepth:    8,
						MetricType:          "vmaf",
						Score:               96.5,
						TargetReached:       true,
						MinimumMet:          true,
						EstimatedVideoBytes: 250000000,
						EstimatedTotalBytes: 300000000,
						EstimatedTotalMB:    300.0,
						SavingsPercent:      45.0,
					},
					DecisionReason: "winner cand_q72 reached target",
				},
			}, nil
		},
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			fullTranscodeReq = req
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-output"), 0644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:       jobID,
				Status:   transcode.StatusCompleted,
				Progress: 100,
			}, nil
		},
	}

	engine1, st, mediaFile, _ := setupBenchmarkTestEnv(t, mock, standard8BitProbeJSON, &recipe.OptimizationPolicy{Enabled: true})

	// Run initial execution -> completes benchmark, submits full transcode
	res1, err := engine1.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "opt-vt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res1.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted, got %s (error: %s)", res1.Status, res1.Error)
	}

	// Capture outputs from first run
	winnerCandID := getString(res1.Outputs, "winner_candidate_id")
	planDigest := getString(res1.Outputs, "plan_digest")
	if winnerCandID != "cand_q72" {
		t.Errorf("expected winner cand_q72, got %s", winnerCandID)
	}
	if planDigest == "" {
		t.Errorf("expected non-empty plan_digest")
	}

	// Now simulate reloaded engine with the same store
	engine2 := NewEngine(EngineDeps{
		Store:     st,
		Config:    engine1.deps.Config,
		Fs:        engine1.deps.Fs,
		Ffprobe:   engine1.deps.Ffprobe,
		Transcode: mock,
	})

	// Resuming completed action
	resumeRes, err := engine2.Resume(context.Background(), res1.ID, "", nil)
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if resumeRes.Status != StatusCompleted {
		t.Errorf("expected StatusCompleted on reload, got %s", resumeRes.Status)
	}

	// Assertions:
	// 1. No second benchmark submit
	if mock.benchmarkSubmitCalls != 1 {
		t.Errorf("expected exactly 1 benchmark submit call, got %d", mock.benchmarkSubmitCalls)
	}
	// 2. Same persisted winner and concrete PlanDigest
	if getString(resumeRes.Outputs, "winner_candidate_id") != winnerCandID {
		t.Errorf("winner candidate changed on reload: %s vs %s", getString(resumeRes.Outputs, "winner_candidate_id"), winnerCandID)
	}
	if getString(resumeRes.Outputs, "plan_digest") != planDigest {
		t.Errorf("plan digest changed on reload: %s vs %s", getString(resumeRes.Outputs, "plan_digest"), planDigest)
	}
	// 3. Exactly one full submit
	if mock.submitCalls != 1 {
		t.Errorf("expected exactly 1 full transcode submit call, got %d", mock.submitCalls)
	}
	// 4. Concrete Plan properties match winner
	if fullTranscodeReq.Plan == nil || fullTranscodeReq.Plan.Quality != 72 {
		t.Errorf("submitted transcode request does not have winner's quality 72")
	}
}
