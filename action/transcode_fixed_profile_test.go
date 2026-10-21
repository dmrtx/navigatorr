package action

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
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

func setupProbeTestEnv(t *testing.T, mock *mockTranscodeExecutor, ffprobeJSON string) (*Engine, *store.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	probePath := createSmartFakeFFprobeScript(t, ffprobeJSON)

	mediaFile := filepath.Join(dir, "TestEpisode.mkv")
	if err := os.WriteFile(mediaFile, []byte("fake-source-media-bytes-for-transcode-testing"), 0644); err != nil {
		t.Fatalf("writing fake media file: %v", err)
	}

	engine, st := setupTranscodeEngine(t, mock, probePath, []string{dir}, []string{dir}, false)
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
func TestTranscodeMedia_FixedProfileSubmitsOneEncode(t *testing.T) {
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

	engine, _, mediaFile, _ := setupProbeTestEnv(t, mock, standard8BitProbeJSON)

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
		t.Errorf("a fixed profile must not submit a sample job, got %d", mock.benchmarkSubmitCalls)
	}
	if mock.submitCalls != 1 {
		t.Errorf("expected exactly 1 full transcode Submit call, got %d", mock.submitCalls)
	}
}

func TestTranscode_ReplaceOriginal_RejectedBeforeSubmit(t *testing.T) {
	mock := &mockTranscodeExecutor{}
	engine, _, mediaFile, _ := setupProbeTestEnv(t, mock, standard8BitProbeJSON)

	res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
		"path":             mediaFile,
		"profile":          "hevc-vt",
		"replace_original": true,
	})
	if invalid, ok := AsInvalidParams(err); !ok || res != nil || invalid.Field != "replace_original" {
		t.Fatalf("transcode_media with replace_original=true must be refused before admission, got %+v %v", res, err)
	}
	if mock.submitCalls != 0 {
		t.Errorf("transcode submit must not be called when replace_original=true")
	}
}

func TestTranscode_HDRSourcesNeedTheCompactGoal(t *testing.T) {
	cases := []struct {
		name  string
		probe string
	}{
		{"HDR10", hdr10ProbeJSON},
		{"DV side data only", dvSideDataOnlyProbeJSON},
		{"DOVI tag only", doviTagOnlyProbeJSON},
		{"HDR mastering side data only", hdrMasteringSideDataOnlyProbeJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockTranscodeExecutor{}
			engine, _, mediaFile, _ := setupProbeTestEnv(t, mock, tc.probe)
			res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
				"path": mediaFile,
				"mode": "quality",
			})
			if err != nil {
				t.Fatalf("unexpected engine error: %v", err)
			}
			if res.Status != StatusCompleted || getBool(res.Outputs, "original_intact") != true || !strings.Contains(getString(res.Outputs, "message"), "hdr_requires_compact") {
				t.Fatalf("expected the original to be preserved as HDR needing compact, got %s %v", res.Status, res.Outputs["message"])
			}
			if mock.submitCalls != 0 {
				t.Errorf("nothing may be submitted for this source, got %d submits", mock.submitCalls)
			}
		})
	}

	t.Run("ordinary 10-bit SDR still encodes with a fixed profile", func(t *testing.T) {
		mock := &mockTranscodeExecutor{
			submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
				_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
				_ = os.WriteFile(req.CandidatePath, []byte("transcoded-media"), 0644)
				return transcode.Job{ID: req.ID}, nil
			},
			statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
				return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted, Progress: 100}, nil
			},
		}
		engine, _, mediaFile, _ := setupProbeTestEnv(t, mock, standard10BitSDRProbeJSON)
		res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
			"path":    mediaFile,
			"profile": "hevc-vt",
		})
		if err != nil {
			t.Fatalf("unexpected engine error: %v", err)
		}
		if res.Status != StatusCompleted {
			t.Fatalf("expected ordinary 10-bit SDR to succeed, got %s (error: %s)", res.Status, res.Error)
		}
		if mock.submitCalls != 1 {
			t.Errorf("expected exactly 1 submit call for ordinary SDR, got %d", mock.submitCalls)
		}
	})
}

func TestTranscode_PersistedOutputsSurviveActionReload(t *testing.T) {
	var statusCalls int32
	mock := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			_ = os.MkdirAll(filepath.Dir(req.CandidatePath), 0755)
			_ = os.WriteFile(req.CandidatePath, []byte("transcoded-media"), 0644)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			if atomic.AddInt32(&statusCalls, 1) == 1 {
				return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning, Progress: 40}, nil
			}
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted, Progress: 100}, nil
		},
	}

	engine1, _, mediaFile, _ := setupProbeTestEnv(t, mock, standard8BitProbeJSON)
	res1, err := engine1.Run(context.Background(), "transcode_media", map[string]any{
		"path":    mediaFile,
		"profile": "hevc-vt",
	})
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if res1.Status != StatusWaitingExternal {
		t.Fatalf("expected waiting_external, got %s", res1.Status)
	}

	// A new engine over the same store is a process restart.
	engine2 := NewEngine(engine1.Deps())
	stRes, err := engine2.Status(context.Background(), res1.ID)
	if err != nil {
		t.Fatalf("status error on reload: %v", err)
	}
	if stRes.Status != StatusWaitingExternal {
		t.Errorf("status on reload = %s, want waiting_external", stRes.Status)
	}
	resumeRes, err := engine2.Resume(context.Background(), res1.ID, "", nil)
	if err != nil {
		t.Fatalf("resume error on reload: %v", err)
	}
	if resumeRes.Status != StatusCompleted {
		t.Fatalf("resume status = %s, want completed (error: %s)", resumeRes.Status, resumeRes.Error)
	}
}

func TestTranscode_IdempotentResumeAndWorkerBusy(t *testing.T) {
	t.Run("worker busy handling", func(t *testing.T) {
		mock := &mockTranscodeExecutor{
			submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
				return transcode.Job{}, fmt.Errorf("worker busy: active job slot limit reached")
			},
		}
		engine, _, mediaFile, _ := setupProbeTestEnv(t, mock, standard8BitProbeJSON)
		res, err := engine.Run(context.Background(), "transcode_media", map[string]any{
			"path":                mediaFile,
			"profile":             "hevc-vt",
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
		if res.Outputs["attempt"] == nil || res.Outputs["retry_count"] == nil {
			t.Errorf("expected attempt and retry_count in outputs, got %v", res.Outputs)
		}
	})

	t.Run("idempotent submission reuse", func(t *testing.T) {
		var submitCount int32
		mock := &mockTranscodeExecutor{
			submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
				atomic.AddInt32(&submitCount, 1)
				return transcode.Job{ID: req.ID}, nil
			},
			statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
				return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning, Progress: 50}, nil
			},
		}
		engine, _, mediaFile, _ := setupProbeTestEnv(t, mock, standard8BitProbeJSON)
		inputs := map[string]any{"path": mediaFile, "profile": "hevc-vt"}

		res1, err := engine.Run(context.Background(), "transcode_media", inputs, "idem-key-1")
		if err != nil || res1.Status != StatusWaitingExternal {
			t.Fatalf("initial run failed: %v, status: %s", err, res1.Status)
		}
		res2, err := engine.Run(context.Background(), "transcode_media", inputs, "idem-key-1")
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
