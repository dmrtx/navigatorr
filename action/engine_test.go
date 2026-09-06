package action

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/store"
)

func setupTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "action_test.db"))
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestEngineSequentialExecutionAndPersistence(t *testing.T) {
	st := setupTestStore(t)
	cfg := &config.Config{}
	engine := NewEngine(EngineDeps{Store: st, Config: cfg})

	executedSteps := []string{}
	engine.RegisterTemplate(ActionTemplate{
		Name:        "test_flow",
		Description: "A simple sequential test workflow",
		Steps: []StepDefinition{
			{
				Name: "step_one",
				Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
					executedSteps = append(executedSteps, "step_one")
					return StepResult{
						Status: StepCompleted,
						Outputs: map[string]any{
							"step1_out": "val1",
						},
					}, nil
				},
			},
			{
				Name: "step_two",
				Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
					executedSteps = append(executedSteps, "step_two")
					if ec.State["step1_out"] != "val1" {
						return StepResult{Status: StepFailed, Error: "missing state from step 1"}, nil
					}
					return StepResult{
						Status: StepCompleted,
						Outputs: map[string]any{
							"step2_out": "val2",
						},
					}, nil
				},
			},
		},
	})

	ctx := context.Background()
	res, err := engine.Run(ctx, "test_flow", map[string]any{"init_key": "init_val"})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if res.Status != StatusCompleted {
		t.Errorf("expected completed status, got %s (err: %s)", res.Status, res.Error)
	}
	if res.CurrentStep != 2 {
		t.Errorf("expected current step 2, got %d", res.CurrentStep)
	}
	if len(executedSteps) != 2 || executedSteps[0] != "step_one" || executedSteps[1] != "step_two" {
		t.Errorf("unexpected executed steps: %v", executedSteps)
	}

	// Verify persistence in SQLite
	inst, err := st.GetActionInstance(res.ID)
	if err != nil || inst == nil {
		t.Fatalf("failed to retrieve action from store: %v", err)
	}
	if inst.Status != StatusCompleted {
		t.Errorf("persisted status mismatch: got %s", inst.Status)
	}

	stepsLog, err := st.GetActionSteps(res.ID)
	if err != nil || len(stepsLog) != 2 {
		t.Errorf("expected 2 logged steps, got %d: %v", len(stepsLog), err)
	}
}

func TestEngineWaitingExternalAndResume(t *testing.T) {
	st := setupTestStore(t)
	cfg := &config.Config{}
	engine := NewEngine(EngineDeps{Store: st, Config: cfg})

	externalReady := false
	engine.RegisterTemplate(ActionTemplate{
		Name: "async_download_flow",
		Steps: []StepDefinition{
			{
				Name: "start_download",
				Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
					return StepResult{
						Status:  StepCompleted,
						Outputs: map[string]any{"download_id": "dl-42"},
					}, nil
				},
			},
			{
				Name: "wait_download",
				Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
					if !externalReady {
						return StepResult{
							Status:           StepWaitingExternal,
							WaitingCondition: "download_complete",
							WaitingReason:    "waiting for external downloader",
							Outputs:          map[string]any{"progress": 0.45},
						}, nil
					}
					return StepResult{
						Status:  StepCompleted,
						Outputs: map[string]any{"file_path": "/media/downloaded.mkv"},
					}, nil
				},
			},
			{
				Name: "finalize",
				Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
					return StepResult{
						Status:  StepCompleted,
						Outputs: map[string]any{"finalized": true},
					}, nil
				},
			},
		},
	})

	ctx := context.Background()
	res, err := engine.Run(ctx, "async_download_flow", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected waiting_external status, got %s", res.Status)
	}
	if res.WaitingCondition != "download_complete" {
		t.Errorf("expected condition 'download_complete', got %s", res.WaitingCondition)
	}

	// External condition becomes true
	externalReady = true

	// Resume action
	resumed, err := engine.Resume(ctx, res.ID, "", nil)
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}

	if resumed.Status != StatusCompleted {
		t.Errorf("expected completed after resume, got %s", resumed.Status)
	}
	if resumed.Outputs["finalized"] != true {
		t.Errorf("expected finalized output true, got %v", resumed.Outputs)
	}
}

func TestEngineWaitingDecisionApproveAndReject(t *testing.T) {
	st := setupTestStore(t)
	cfg := &config.Config{}
	engine := NewEngine(EngineDeps{Store: st, Config: cfg})

	engine.RegisterTemplate(ActionTemplate{
		Name: "decision_flow",
		Steps: []StepDefinition{
			{
				Name: "check_tradeoff",
				Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
					if ec.Decision == "" {
						return StepResult{
							Status:        StepWaitingDecision,
							WaitingReason: "Replacement is 3x larger. Approve?",
							WaitingOptions: []WaitingOption{
								{Decision: "approve", Description: "Accept larger file"},
								{Decision: "reject", Description: "Decline and abort"},
							},
						}, nil
					}
					if ec.Decision == "reject" {
						return StepResult{
							Status: StepFailed,
							Error:  "replacement rejected by user",
						}, nil
					}
					return StepResult{
						Status:  StepCompleted,
						Outputs: map[string]any{"approved": true},
					}, nil
				},
			},
		},
	})

	ctx := context.Background()
	// 1. Run pauses at waiting_decision
	res, err := engine.Run(ctx, "decision_flow", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected waiting_decision, got %s", res.Status)
	}
	if len(res.WaitingOptions) != 2 {
		t.Errorf("expected 2 waiting options, got %d", len(res.WaitingOptions))
	}

	// 2. Resume with approve
	resumedApprove, err := engine.Resume(ctx, res.ID, "approve", nil)
	if err != nil {
		t.Fatalf("Resume approve failed: %v", err)
	}
	if resumedApprove.Status != StatusCompleted {
		t.Errorf("expected completed, got %s", resumedApprove.Status)
	}

	// 3. Test reject path
	resReject, _ := engine.Run(ctx, "decision_flow", nil)
	resumedReject, err := engine.Resume(ctx, resReject.ID, "reject", nil)
	if err != nil {
		t.Fatalf("Resume reject failed: %v", err)
	}
	if resumedReject.Status != StatusFailed {
		t.Errorf("expected failed, got %s", resumedReject.Status)
	}
}

func TestValidateTorrentTemplate(t *testing.T) {
	st := setupTestStore(t)
	cfg := &config.Config{}
	engine := NewEngine(EngineDeps{Store: st, Config: cfg})
	ctx := context.Background()

	// 1. Torrent with dangerous executable -> must fail
	badRes, err := engine.Run(ctx, "validate_torrent", map[string]any{
		"files": []string{"Movie.2024.1080p.mkv", "setup.exe", "codec.scr"},
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if badRes.Status != StatusFailed {
		t.Errorf("dangerous torrent was not rejected: status=%s", badRes.Status)
	}

	// 2. Safe torrent
	goodRes, err := engine.Run(ctx, "validate_torrent", map[string]any{
		"files": []string{"Show.S01E01.1080p.mkv", "Show.S01E01.eng.srt"},
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if goodRes.Status != StatusCompleted {
		t.Errorf("safe torrent failed: status=%s, err=%s", goodRes.Status, goodRes.Error)
	}
	if goodRes.Outputs["is_safe"] != true {
		t.Errorf("expected is_safe=true, got %v", goodRes.Outputs["is_safe"])
	}
}

func TestSafeMediaReplacementExternalReconciliationAndSafety(t *testing.T) {
	st := setupTestStore(t)
	tempDir := t.TempDir()

	// Create test files
	origPath := filepath.Join(tempDir, "Original.Movie.1080p.mkv")
	_ = os.WriteFile(origPath, []byte("original media"), 0o644)
	newPath := filepath.Join(tempDir, "New.Movie.1080p.mkv")
	_ = os.WriteFile(newPath, []byte("new media"), 0o644)

	// Mock Radarr server
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/1":
			// Return movie with active file id 100
			resp := map[string]any{
				"id":      1,
				"title":   "The Movie",
				"hasFile": true,
				"movieFile": map[string]any{
					"id":   100,
					"path": origPath,
					"size": 2000000000,
					"mediaInfo": map[string]any{
						"audioLanguages": "Japanese",
						"subtitles":      "Japanese",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/api/v3/command" && r.Method == "POST":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 12, "name": "DownloadedMoviesScan", "state": "completed"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		AllowDestructive: false, // Centralized safety: destructive operations disabled
	}
	cfg.Services = map[string]config.ServiceConfig{
		"radarr": {
			URL:        srv.URL,
			APIKey:     "fake-key",
			AuthMethod: "header",
			AuthHeader: "X-Api-Key",
			APIVersion: "/api/v3",
		},
	}
	reg := arrservice.NewRegistry(cfg)
	res, err := fsop.NewResolver([]string{tempDir}, []string{tempDir})
	if err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: reg,
		Fs:       res,
	})

	ctx := context.Background()

	// Run safe_media_replacement with local replacement path
	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":       "radarr",
		"media_id":      "1",
		"path":          newPath,
		"objective":     "accessibility_repair",
		"allow_cleanup": true, // Requested cleanup, but cfg.AllowDestructive is false!
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runRes.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %s (error: %s)", runRes.Status, runRes.Error)
	}

	// Centralized safety check: cleanup should be skipped because AllowDestructive=false
	if runRes.Outputs["cleanup_status"] != "skipped_destructive_disabled" {
		t.Errorf("expected cleanup_status skipped_destructive_disabled, got %v", runRes.Outputs["cleanup_status"])
	}

	// Verify original file still exists on disk
	if _, err := os.Stat(origPath); os.IsNotExist(err) {
		t.Errorf("original file was deleted despite AllowDestructive=false!")
	}
}

func TestSafeMediaReplacementExternalReconciliation(t *testing.T) {
	st := setupTestStore(t)
	tempDir := t.TempDir()

	newPath := filepath.Join(tempDir, "AlreadyImported.1080p.mkv")
	_ = os.WriteFile(newPath, []byte("media content"), 0o644)

	commandCalled := false
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/1":
			callCount++
			fileID := 100
			if callCount > 1 {
				fileID = 200 // simulated external import!
			}
			resp := map[string]any{
				"id":      1,
				"title":   "Reconciled Movie",
				"hasFile": true,
				"movieFile": map[string]any{
					"id":   fileID,
					"path": newPath,
					"size": 1500000000,
					"mediaInfo": map[string]any{
						"audioLanguages": "Japanese",
						"subtitles":      "English",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/api/v3/command":
			commandCalled = true
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{AllowDestructive: false}
	cfg.Services = map[string]config.ServiceConfig{
		"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
	}
	reg := arrservice.NewRegistry(cfg)
	res, _ := fsop.NewResolver([]string{tempDir}, []string{tempDir})

	engine := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: reg,
		Fs:       res,
	})

	ctx := context.Background()
	// Pass file_id=100 as input. The mock returns file id 200 on initial fetch,
	// but let's test when state was set to 100 and then external import gave 200.
	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":  "radarr",
		"media_id": "1",
		"file_id":  "100",
		"path":     newPath,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runRes.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %s: %s", runRes.Status, runRes.Error)
	}
	if commandCalled {
		t.Errorf("expected import command to be skipped when already reconciled externally!")
	}
}

func TestSafeMediaReplacementRadarrMovieResolution(t *testing.T) {
	st := setupTestStore(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v3/movie/327" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      327,
				"title":   "The Big Lebowski",
				"hasFile": true,
				"movieFile": map[string]any{
					"id":   888,
					"path": "/volume1/Media/Movies/The.Big.Lebowski.1080p.mkv",
					"size": 4500000000,
					"mediaInfo": map[string]any{
						"audioLanguages": "English/Spanish",
						"subtitles":      "English/Spanish",
					},
				},
			})
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Services = map[string]config.ServiceConfig{
		"radarr": {
			URL:        srv.URL,
			APIKey:     "test-key",
			AuthMethod: "header",
			AuthHeader: "X-Api-Key",
			APIVersion: "/api/v3",
		},
	}
	reg := arrservice.NewRegistry(cfg)

	engine := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: reg,
	})

	ctx := context.Background()
	ec := &ExecutionContext{
		InstanceID: "test-inst",
		ActionName: "safe_media_replacement",
		Inputs: map[string]any{
			"service":   "radarr",
			"media_id":  "327",
			"objective": "size_optimization",
		},
		State:   make(map[string]any),
		Outputs: make(map[string]any),
		Engine:  engine,
	}

	stepRes, err := engine.stepPlanAndCheckCurrent(ctx, ec)
	if err != nil {
		t.Fatalf("stepPlanAndCheckCurrent failed: %v", err)
	}
	if stepRes.Status != StepCompleted {
		t.Fatalf("expected stepPlanAndCheckCurrent completed, got %s: %s", stepRes.Status, stepRes.Error)
	}
	if stepRes.Outputs["media_title"] != "The Big Lebowski" {
		t.Errorf("expected media_title 'The Big Lebowski', got %v", stepRes.Outputs["media_title"])
	}
	if stepRes.Outputs["current_file_id"] != "888" {
		t.Errorf("expected current_file_id '888', got %v", stepRes.Outputs["current_file_id"])
	}
	if stepRes.Outputs["current_size"] != int64(4500000000) {
		t.Errorf("expected current_size 4500000000, got %v", stepRes.Outputs["current_size"])
	}
}

func TestSafeMediaReplacementSonarrSeriesResolution(t *testing.T) {
	st := setupTestStore(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/series/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    42,
				"title": "Breaking Bad",
				"path":  "/volume1/Media/TvSeries/Breaking Bad",
				"statistics": map[string]any{
					"sizeOnDisk":       12000000000,
					"episodeFileCount": 62,
				},
			})
		case r.URL.Path == "/api/v3/episodefile" && r.URL.Query().Get("seriesId") == "42":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id":   999,
					"path": "/volume1/Media/TvSeries/Breaking Bad/S01E01.mkv",
					"size": 1500000000,
					"mediaInfo": map[string]any{
						"audioLanguages": "English",
						"subtitles":      "English/Spanish",
					},
				},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Services = map[string]config.ServiceConfig{
		"sonarr": {
			URL:        srv.URL,
			APIKey:     "test-key",
			AuthMethod: "header",
			AuthHeader: "X-Api-Key",
			APIVersion: "/api/v3",
		},
	}
	reg := arrservice.NewRegistry(cfg)

	engine := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: reg,
	})

	ctx := context.Background()
	ec := &ExecutionContext{
		InstanceID: "test-inst-sonarr",
		ActionName: "safe_media_replacement",
		Inputs: map[string]any{
			"service":   "sonarr",
			"media_id":  "42",
			"objective": "accessibility_repair",
		},
		State:   make(map[string]any),
		Outputs: make(map[string]any),
		Engine:  engine,
	}

	stepRes, err := engine.stepPlanAndCheckCurrent(ctx, ec)
	if err != nil {
		t.Fatalf("stepPlanAndCheckCurrent failed: %v", err)
	}
	if stepRes.Status != StepCompleted {
		t.Fatalf("expected stepPlanAndCheckCurrent completed, got %s: %s", stepRes.Status, stepRes.Error)
	}
	if stepRes.Outputs["media_title"] != "Breaking Bad" {
		t.Errorf("expected media_title 'Breaking Bad', got %v", stepRes.Outputs["media_title"])
	}
	if stepRes.Outputs["current_file_id"] != "999" {
		t.Errorf("expected current_file_id '999', got %v", stepRes.Outputs["current_file_id"])
	}
	if stepRes.Outputs["current_size"] != int64(12000000000) {
		t.Errorf("expected current_size 12000000000, got %v", stepRes.Outputs["current_size"])
	}
}

func TestValidateTorrentAccessibleMediaAndInspection(t *testing.T) {
	st := setupTestStore(t)
	tempDir := t.TempDir()

	// 1. Create a media file on disk
	mediaDir := filepath.Join(tempDir, "downloads", "Fate_strange_Fake")
	_ = os.MkdirAll(mediaDir, 0755)
	mediaFile := filepath.Join(mediaDir, "Fate_strange_Fake_EP01.mkv")
	_ = os.WriteFile(mediaFile, []byte("dummy video stream content"), 0644)

	// 2. Create mock ffprobe script
	mockFfprobe := filepath.Join(tempDir, "mock_ffprobe.sh")
	ffprobeScript := `#!/bin/sh
cat << 'EOF'
{
  "streams": [
    {
      "codec_type": "video",
      "codec_name": "hevc",
      "height": 1080,
      "bits_per_raw_sample": 10
    },
    {
      "codec_type": "audio",
      "tags": {
        "language": "jpn"
      }
    },
    {
      "codec_type": "subtitle",
      "tags": {
        "language": "eng"
      }
    }
  ],
  "format": {
    "format_name": "matroska",
    "duration": "1420.000000"
  }
}
EOF
`
	_ = os.WriteFile(mockFfprobe, []byte(ffprobeScript), 0755)

	// 3. Mock qBittorrent server
	torHash := "4a5b6c7d8e9f0123456789abcdef0123456789ab"
	qbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v2/auth/login":
			w.Write([]byte("Ok."))
		case strings.HasPrefix(r.URL.Path, "/api/v2/torrents/info"):
			_ = json.NewEncoder(w).Encode([]qbit.TorrentInfo{
				{
					Hash:        torHash,
					Name:        "Fate_strange_Fake",
					ContentPath: mediaDir,
					SavePath:    filepath.Join(tempDir, "downloads"),
					Size:        1500000000,
					Progress:    1.0,
					State:       "uploading",
				},
			})
		case strings.HasPrefix(r.URL.Path, "/api/v2/torrents/files"):
			_ = json.NewEncoder(w).Encode([]qbit.TorrentFile{
				{
					Name: "Fate_strange_Fake/Fate_strange_Fake_EP01.mkv",
					Size: 1500000000,
				},
			})
		default:
			w.WriteHeader(200)
		}
	}))
	defer qbSrv.Close()

	qbClient := qbit.NewClient(qbSrv.URL, "", "")
	res, _ := fsop.NewResolver([]string{tempDir}, []string{tempDir})

	engine := NewEngine(EngineDeps{
		Store:   st,
		Config:  &config.Config{},
		Qbit:    qbClient,
		Fs:      res,
		Ffprobe: mockFfprobe,
	})

	ctx := context.Background()
	runRes, err := engine.Run(ctx, "validate_torrent", map[string]any{
		"hash": torHash,
	})
	if err != nil {
		t.Fatalf("validate_torrent Run failed: %v", err)
	}

	if runRes.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %s: %s", runRes.Status, runRes.Error)
	}

	// Verify outputs
	if runRes.Outputs["valid"] != true {
		t.Errorf("expected valid=true, got %v", runRes.Outputs["valid"])
	}
	if runRes.Outputs["validation_incomplete"] != false {
		t.Errorf("expected validation_incomplete=false, got %v", runRes.Outputs["validation_incomplete"])
	}
	if runRes.Outputs["is_safe"] != true {
		t.Errorf("expected is_safe=true, got %v", runRes.Outputs["is_safe"])
	}
	if runRes.Outputs["video_codec"] != "hevc" {
		t.Errorf("expected video_codec=hevc, got %v", runRes.Outputs["video_codec"])
	}
	if runRes.Outputs["resolution"] != "1080p" {
		t.Errorf("expected resolution=1080p, got %v", runRes.Outputs["resolution"])
	}

	audios, _ := runRes.Outputs["audio_languages"].([]string)
	if len(audios) == 0 || audios[0] != "jpn" {
		t.Errorf("expected audio languages [jpn], got %v", audios)
	}
	subs, _ := runRes.Outputs["subtitle_languages"].([]string)
	if len(subs) == 0 || subs[0] != "eng" {
		t.Errorf("expected subtitle languages [eng], got %v", subs)
	}
}

func TestValidateTorrentIncompleteValidationWhenInspectionFails(t *testing.T) {
	st := setupTestStore(t)
	engine := NewEngine(EngineDeps{
		Store:   st,
		Config:  &config.Config{},
		Ffprobe: "/nonexistent/path/to/ffprobe",
	})

	ctx := context.Background()
	// Torrent safe by file extension screening, but with no local files / inaccessible path
	runRes, err := engine.Run(ctx, "validate_torrent", map[string]any{
		"files": []string{"Fate_strange_Fake_EP01.mkv"},
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runRes.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %s", runRes.Status)
	}

	// It must NOT falsely report valid=true!
	if runRes.Outputs["valid"] == true {
		t.Errorf("CRITICAL BUG: validate_torrent returned valid=true without inspecting streams!")
	}
	if runRes.Outputs["valid"] != false {
		t.Errorf("expected valid=false, got %v", runRes.Outputs["valid"])
	}
	if runRes.Outputs["validation_incomplete"] != true {
		t.Errorf("expected validation_incomplete=true, got %v", runRes.Outputs["validation_incomplete"])
	}
	if runRes.Outputs["is_safe"] != true {
		t.Errorf("expected is_safe=true for clean filename, got %v", runRes.Outputs["is_safe"])
	}
}

func TestEngineActionRetrySkipsCompletedSteps(t *testing.T) {
	st := setupTestStore(t)
	engine := NewEngine(EngineDeps{Store: st, Config: &config.Config{}})

	step1Calls := 0
	step2Calls := 0
	step2Allowed := false

	engine.RegisterTemplate(ActionTemplate{
		Name: "retry_flow",
		Steps: []StepDefinition{
			{
				Name: "step_side_effect",
				Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
					step1Calls++
					return StepResult{
						Status:  StepCompleted,
						Outputs: map[string]any{"step1_done": true},
					}, nil
				},
			},
			{
				Name: "step_fallible",
				Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
					step2Calls++
					if !step2Allowed {
						return StepResult{
							Status: StepFailed,
							Error:  "transient external failure",
						}, nil
					}
					return StepResult{
						Status:  StepCompleted,
						Outputs: map[string]any{"step2_done": true},
					}, nil
				},
			},
		},
	})

	ctx := context.Background()
	// 1. Initial run fails at step 2
	runRes, err := engine.Run(ctx, "retry_flow", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if runRes.Status != StatusFailed {
		t.Fatalf("expected status failed, got %s", runRes.Status)
	}
	if step1Calls != 1 {
		t.Fatalf("expected step 1 to have run once, got %d", step1Calls)
	}
	if step2Calls != 1 {
		t.Fatalf("expected step 2 to have run once, got %d", step2Calls)
	}

	// 2. Cannot retry non-failed action
	_, err = engine.Retry(ctx, "nonexistent-id")
	if err == nil {
		t.Errorf("expected error retrying nonexistent action")
	}

	// 3. Enable step 2 and retry the failed action
	step2Allowed = true
	retryRes, err := engine.Retry(ctx, runRes.ID)
	if err != nil {
		t.Fatalf("Retry failed: %v", err)
	}

	if retryRes.ID != runRes.ID {
		t.Errorf("expected retry to reuse same instance ID %s, got %s", runRes.ID, retryRes.ID)
	}
	if retryRes.Status != StatusCompleted {
		t.Fatalf("expected retried action to be completed, got %s: %s", retryRes.Status, retryRes.Error)
	}

	// 4. Cannot retry an already completed action (strictly failed actions only)
	_, err = engine.Retry(ctx, runRes.ID)
	if err == nil || !strings.Contains(err.Error(), "only failed actions can be retried") {
		t.Errorf("expected error retrying completed action, got: %v", err)
	}

	// Step 1 must NOT have run again!
	if step1Calls != 1 {
		t.Errorf("SIDE EFFECT REPEATED: expected step 1 calls to stay 1, but got %d", step1Calls)
	}
	if step2Calls != 2 {
		t.Errorf("expected step 2 calls to be 2, got %d", step2Calls)
	}

	// Audit log verification
	logs, err := st.QueryActionLog("", "", "action_retry", 10)
	if err != nil {
		t.Fatalf("querying action log: %v", err)
	}
	foundRetryAudit := false
	for _, l := range logs {
		if idStr, ok := l["identifiers"].(string); ok && strings.Contains(idStr, runRes.ID) {
			foundRetryAudit = true
			break
		}
	}
	if !foundRetryAudit {
		t.Errorf("expected action_retry in audit log for instance %s", runRes.ID)
	}
}

func TestEngineIdempotencyKeyDeduplication(t *testing.T) {
	st := setupTestStore(t)
	engine := NewEngine(EngineDeps{Store: st, Config: &config.Config{}})

	engine.RegisterTemplate(ActionTemplate{
		Name: "idempotent_flow",
		Steps: []StepDefinition{
			{
				Name: "wait_step",
				Run: func(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
					if ec.Decision == "" {
						return StepResult{
							Status:        StepWaitingDecision,
							WaitingReason: "Waiting for user confirmation",
						}, nil
					}
					return StepResult{
						Status:  StepCompleted,
						Outputs: map[string]any{"confirmed": true},
					}, nil
				},
			},
		},
	})

	ctx := context.Background()
	key := "radarr:327:size_optimization"

	// 1. First run creates action
	res1, err := engine.Run(ctx, "idempotent_flow", map[string]any{"target": 327}, key)
	if err != nil {
		t.Fatalf("first Run failed: %v", err)
	}
	if res1.Status != StatusWaitingDecision {
		t.Fatalf("expected waiting_decision, got %s", res1.Status)
	}

	// 2. Second run with same idempotency key returns existing action
	res2, err := engine.Run(ctx, "idempotent_flow", map[string]any{"target": 327}, key)
	if err != nil {
		t.Fatalf("second Run failed: %v", err)
	}
	if res2.ID != res1.ID {
		t.Errorf("expected duplicate run to return existing ID %s, got %s", res1.ID, res2.ID)
	}

	// Verify only 1 instance exists in store
	instances, err := st.ListActionInstances("all", 10)
	if err != nil {
		t.Fatalf("listing instances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected exactly 1 instance in store, got %d", len(instances))
	}

	// 3. Complete the action
	resumed, err := engine.Resume(ctx, res1.ID, "confirm", nil)
	if err != nil || resumed.Status != StatusCompleted {
		t.Fatalf("resume failed: %v (status=%s)", err, resumed.Status)
	}

	// 4. Once terminal, a new run with the same key is permitted
	res3, err := engine.Run(ctx, "idempotent_flow", map[string]any{"target": 327}, key)
	if err != nil {
		t.Fatalf("third Run failed: %v", err)
	}
	if res3.ID == res1.ID {
		t.Errorf("expected new instance after previous terminal completion, but got old ID %s", res1.ID)
	}
}

func TestActionCatalogDiscovery(t *testing.T) {
	st := setupTestStore(t)
	engine := NewEngine(EngineDeps{Store: st, Config: &config.Config{}})

	catalog := engine.Catalog()
	if len(catalog) < 2 {
		t.Fatalf("expected at least 2 catalog entries, got %d", len(catalog))
	}

	var smr, vt *ActionCatalogEntry
	for i := range catalog {
		if catalog[i].Name == "safe_media_replacement" {
			smr = &catalog[i]
		}
		if catalog[i].Name == "validate_torrent" {
			vt = &catalog[i]
		}
	}

	if smr == nil {
		t.Fatalf("safe_media_replacement missing from catalog")
	}
	if smr.Version != 1 {
		t.Errorf("expected version 1, got %d", smr.Version)
	}
	if len(smr.RequiredInputs) < 2 || smr.RequiredInputs[0] != "service" || smr.RequiredInputs[1] != "media_id" {
		t.Errorf("expected required inputs [service, media_id], got %v", smr.RequiredInputs)
	}
	if smr.Destructive != false {
		t.Errorf("expected destructive=false, got %v", smr.Destructive)
	}
	if len(smr.Steps) != 9 {
		t.Errorf("expected 9 steps for safe_media_replacement, got %d", len(smr.Steps))
	}

	if vt == nil {
		t.Fatalf("validate_torrent missing from catalog")
	}
	if vt.Version != 1 {
		t.Errorf("expected version 1, got %d", vt.Version)
	}
	if len(vt.Steps) != 4 {
		t.Errorf("expected 4 steps for validate_torrent, got %d", len(vt.Steps))
	}
}

func TestActionPersistenceAfterReopeningStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "persist_test.db")
	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("opening store 1: %v", err)
	}

	engine1 := NewEngine(EngineDeps{Store: st1, Config: &config.Config{}})

	ctx := context.Background()
	runRes, err := engine1.Run(ctx, "validate_torrent", map[string]any{
		"files": []string{"Sample.Movie.1080p.mkv"},
	}, "test-persist-key")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	actionID := runRes.ID

	// Close the first store
	st1.Close()

	// Reopen the exact same database file in a new Store instance
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	defer st2.Close()

	// 1. Verify action instance persisted
	inst, err := st2.GetActionInstance(actionID)
	if err != nil || inst == nil {
		t.Fatalf("failed to retrieve action after reopen: %v", err)
	}
	if inst.ID != actionID {
		t.Errorf("expected ID %s, got %s", actionID, inst.ID)
	}
	if inst.ActionName != "validate_torrent" {
		t.Errorf("expected ActionName validate_torrent, got %s", inst.ActionName)
	}
	if inst.IdempotencyKey != "test-persist-key" {
		t.Errorf("expected IdempotencyKey test-persist-key, got %s", inst.IdempotencyKey)
	}
	if inst.Status != StatusCompleted {
		t.Errorf("expected Status completed, got %s", inst.Status)
	}

	// 2. Verify steps log persisted
	steps, err := st2.GetActionSteps(actionID)
	if err != nil {
		t.Fatalf("failed to retrieve steps after reopen: %v", err)
	}
	if len(steps) != 4 {
		t.Fatalf("expected 4 logged steps, got %d", len(steps))
	}
	expectedStepNames := []string{"resolve_torrent_files", "security_scan", "inspect_streams", "summarize"}
	for i, s := range steps {
		if s.StepName != expectedStepNames[i] {
			t.Errorf("step %d: expected name %s, got %s", i, expectedStepNames[i], s.StepName)
		}
	}

	// 3. Engine querying against reopened store
	engine2 := NewEngine(EngineDeps{Store: st2, Config: &config.Config{}})
	statRes, err := engine2.Status(ctx, actionID)
	if err != nil {
		t.Fatalf("Status failed after reopen: %v", err)
	}
	if statRes.ID != actionID || statRes.Status != StatusCompleted {
		t.Errorf("unexpected status result after reopen: %+v", statRes)
	}
}
