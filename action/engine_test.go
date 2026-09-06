package action

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
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
		"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key"},
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
