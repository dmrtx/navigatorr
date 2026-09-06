package action

import (
	"context"
	"encoding/json"
	"fmt"
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
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/1":
			callCount++
			fileID := 100
			filePath := origPath
			if callCount > 2 {
				fileID = 200
				filePath = newPath
			}
			resp := map[string]any{
				"id":      1,
				"title":   "The Movie",
				"hasFile": true,
				"movieFile": map[string]any{
					"id":   fileID,
					"path": filePath,
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
		"service":   "radarr",
		"media_id":  "1",
		"path":      newPath,
		"objective": "accessibility_repair",
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
	if runRes.Outputs["cleanup_performed"] != false {
		t.Errorf("expected cleanup_performed false, got %v", runRes.Outputs["cleanup_performed"])
	}

	// Verify original file still exists on disk
	if _, err := os.Stat(origPath); os.IsNotExist(err) {
		t.Errorf("original file was deleted despite AllowDestructive=false!")
	}
}

func TestSafeMediaReplacementDestructiveCleanupWhenAllowedAndInvariantsMet(t *testing.T) {
	st := setupTestStore(t)
	tempDir := t.TempDir()

	origPath := filepath.Join(tempDir, "Original.Movie.720p.mkv")
	newPath := filepath.Join(tempDir, "Replacement.Movie.1080p.mkv")
	_ = os.WriteFile(origPath, []byte("old media"), 0o644)
	_ = os.WriteFile(newPath, []byte("new media"), 0o644)

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/1":
			callCount++
			fileID := 1
			filePath := origPath
			if callCount > 1 {
				fileID = 2
				filePath = newPath
			}
			resp := map[string]any{
				"id":      1,
				"title":   "Test Movie",
				"hasFile": true,
				"movieFile": map[string]any{
					"id":   fileID,
					"path": filePath,
					"size": 2000000000,
					"mediaInfo": map[string]any{
						"audioLanguages": "English/Japanese",
						"subtitles":      "English/Spanish",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/api/v3/command":
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		AllowDestructive: true, // Centralized safety: destructive operations authorized
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

	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":   "radarr",
		"media_id":  "1",
		"path":      newPath,
		"objective": "accessibility_repair",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runRes.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %s (error: %s)", runRes.Status, runRes.Error)
	}

	if runRes.Outputs["cleanup_status"] != "performed" {
		t.Errorf("expected cleanup_status performed, got %v", runRes.Outputs["cleanup_status"])
	}
	if runRes.Outputs["cleanup_performed"] != true {
		t.Errorf("expected cleanup_performed true, got %v", runRes.Outputs["cleanup_performed"])
	}

	// Verify original file was deleted from disk
	if _, err := os.Stat(origPath); !os.IsNotExist(err) {
		t.Errorf("expected original file to be deleted, but it still exists!")
	}

	// Verify replacement file is intact
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		t.Errorf("replacement file was unexpectedly deleted!")
	}
}

func TestSafeMediaReplacementDestructiveCleanupSkippedWhenInvariantsUnmet(t *testing.T) {
	st := setupTestStore(t)
	tempDir := t.TempDir()

	origPath := filepath.Join(tempDir, "Original.Movie.720p.mkv")
	newPath := filepath.Join(tempDir, "Replacement.Movie.1080p.mkv")
	_ = os.WriteFile(origPath, []byte("old media"), 0o644)
	_ = os.WriteFile(newPath, []byte("new media"), 0o644)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/1":
			// Library verification invariant fails (hasFile = false)
			resp := map[string]any{
				"id":      1,
				"title":   "Test Movie",
				"hasFile": false,
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		AllowDestructive: true, // destructive allowed, but invariants will fail!
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

	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":  "radarr",
		"media_id": "1",
		"path":     newPath,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runRes.Status != StatusFailed {
		t.Fatalf("expected action to fail due to unverified library, got status: %s", runRes.Status)
	}

	// Original file MUST remain intact
	if _, err := os.Stat(origPath); os.IsNotExist(err) {
		t.Errorf("original file was deleted despite unmet library invariants!")
	}
}

func TestSafeMediaReplacementCleanupAlreadyAbsent(t *testing.T) {
	st := setupTestStore(t)
	tempDir := t.TempDir()

	origPath := filepath.Join(tempDir, "Original.Movie.720p.mkv")
	newPath := filepath.Join(tempDir, "Replacement.Movie.1080p.mkv")
	// Notice: origPath does NOT exist on disk (simulating Arr service already removing it on upgrade)
	_ = os.WriteFile(newPath, []byte("new media content"), 0o644)

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/1":
			callCount++
			fileID := 1
			filePath := origPath
			if callCount > 1 {
				fileID = 2
				filePath = newPath
			}
			resp := map[string]any{
				"id":      1,
				"title":   "Test Movie",
				"hasFile": true,
				"movieFile": map[string]any{
					"id":   fileID,
					"path": filePath,
					"size": 2000000000,
					"mediaInfo": map[string]any{
						"audioLanguages": "English",
						"subtitles":      "English",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/api/v3/command":
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		AllowDestructive: true,
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

	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":  "radarr",
		"media_id": "1",
		"path":     newPath,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runRes.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %s: %s", runRes.Status, runRes.Error)
	}
	if runRes.Outputs["cleanup_status"] != "already_absent" {
		t.Errorf("expected cleanup_status already_absent, got %v", runRes.Outputs["cleanup_status"])
	}
	if runRes.Outputs["cleanup_performed"] != false {
		t.Errorf("expected cleanup_performed false, got %v", runRes.Outputs["cleanup_performed"])
	}
	if runRes.Outputs["replacement_verified"] != true {
		t.Errorf("expected replacement_verified true, got %v", runRes.Outputs["replacement_verified"])
	}
}

func TestSafeMediaReplacementCleanupFailurePreservesReplacementSuccess(t *testing.T) {
	st := setupTestStore(t)
	tempDir := t.TempDir()

	origPath := filepath.Join(tempDir, "Original.Movie.720p.mkv")
	newPath := filepath.Join(tempDir, "Replacement.Movie.1080p.mkv")
	_ = os.WriteFile(origPath, []byte("old media"), 0o644)
	_ = os.WriteFile(newPath, []byte("new media"), 0o644)

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/1":
			callCount++
			fileID := 1
			filePath := origPath
			if callCount > 1 {
				fileID = 2
				filePath = newPath
			}
			resp := map[string]any{
				"id":      1,
				"title":   "Test Movie",
				"hasFile": true,
				"movieFile": map[string]any{
					"id":   fileID,
					"path": filePath,
					"size": 2000000000,
					"mediaInfo": map[string]any{
						"audioLanguages": "English",
						"subtitles":      "English",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/api/v3/command":
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		AllowDestructive: true,
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

	// Resolver has write roots pointing elsewhere, so deleting origPath will fail with "outside write roots"
	otherDir := t.TempDir()
	res, err := fsop.NewResolver([]string{tempDir}, []string{otherDir})
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

	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":  "radarr",
		"media_id": "1",
		"path":     newPath,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Action should fail ONLY at the cleanup step
	if runRes.Status != StatusFailed {
		t.Fatalf("expected action to fail on cleanup error, got %s", runRes.Status)
	}
	if !strings.Contains(runRes.Error, "cleanup failed") {
		t.Errorf("expected error to mention cleanup failed, got: %s", runRes.Error)
	}
	if !strings.Contains(runRes.Error, "media replacement and library verification succeeded") {
		t.Errorf("expected error to preserve media replacement success, got: %s", runRes.Error)
	}
	if runRes.Outputs["replacement_verified"] != true {
		t.Errorf("expected replacement_verified true in outputs, got %v", runRes.Outputs["replacement_verified"])
	}
	if runRes.Outputs["cleanup_status"] != "failed" {
		t.Errorf("expected cleanup_status failed, got %v", runRes.Outputs["cleanup_status"])
	}

	// Now verify retry: action_retry should resume directly from step 9 (update_maintenance_and_cleanup)
	// without repeating downloads or imports!
	stepsLog, _ := st.GetActionSteps(runRes.ID)
	var step9 *store.ActionStepLog
	for _, s := range stepsLog {
		if s.StepName == "update_maintenance_and_cleanup" {
			step9 = &s
			break
		}
	}
	if step9 == nil {
		t.Fatalf("step update_maintenance_and_cleanup not logged in store")
	}
	if step9.Status != string(StepFailed) {
		t.Errorf("expected step9 status failed in store, got %s", step9.Status)
	}
}

func TestSafeMediaReplacementLocalPathWorkflow(t *testing.T) {
	st := setupTestStore(t)
	tempDir := t.TempDir()

	localFilePath := filepath.Join(tempDir, "Local.Rip.1080p.mkv")
	_ = os.WriteFile(localFilePath, []byte("local media content"), 0o644)

	importCommandReceived := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/1":
			resp := map[string]any{
				"id":      1,
				"title":   "Local Movie",
				"hasFile": true,
				"movieFile": map[string]any{
					"id":   50,
					"path": localFilePath,
					"size": 1200000000,
					"mediaInfo": map[string]any{
						"audioLanguages": "English",
						"subtitles":      "English",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/api/v3/command":
			importCommandReceived = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["path"] != localFilePath {
				t.Errorf("expected command path %s, got %v", localFilePath, body["path"])
			}
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 10})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{AllowDestructive: false}
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
	res, _ := fsop.NewResolver([]string{tempDir}, []string{tempDir})

	// Notice: Deps.Qbit is nil! No download client configured!
	engine := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: reg,
		Fs:       res,
	})

	ctx := context.Background()
	// Run with only path (no hash, no url)
	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":  "radarr",
		"media_id": "1",
		"path":     localFilePath,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runRes.Status != StatusCompleted {
		t.Fatalf("expected completed status for local path without download client, got: %s (error: %s)", runRes.Status, runRes.Error)
	}
	if !importCommandReceived {
		t.Errorf("expected import command to be issued for local path replacement")
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
	if smr.Destructive != true {
		t.Errorf("expected destructive=true, got %v", smr.Destructive)
	}
	for _, opt := range smr.OptionalInputs {
		if opt == "allow_cleanup" || opt == "allow_destructive" {
			t.Errorf("expected neither allow_cleanup nor allow_destructive in optional inputs, found: %s", opt)
		}
	}
	if len(smr.Steps) != 10 {
		t.Errorf("expected 10 steps for safe_media_replacement, got %d", len(smr.Steps))
	}
	if len(smr.Steps) > 1 && smr.Steps[1] != "find_and_rank_candidate" {
		t.Errorf("expected step 1 to be find_and_rank_candidate, got %s", smr.Steps[1])
	}

	if vt == nil {
		t.Fatalf("validate_torrent missing from catalog")
	}
	if vt.Version != 1 {
		t.Errorf("expected version 1, got %d", vt.Version)
	}
	if vt.Destructive != false {
		t.Errorf("expected destructive=false for validate_torrent, got %v", vt.Destructive)
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

func TestValidateTorrentTotalSizeDoesNotDouble(t *testing.T) {
	st := setupTestStore(t)
	tempDir := t.TempDir()

	// 1. Create mock media files
	mediaDir := filepath.Join(tempDir, "Fate_strange_Fake")
	_ = os.MkdirAll(mediaDir, 0755)
	mediaFile := filepath.Join(mediaDir, "Fate_strange_Fake_EP01.mkv")
	_ = os.WriteFile(mediaFile, []byte("media-bytes-for-inspection"), 0644)

	// 2. Mock ffprobe
	mockFfprobe := filepath.Join(tempDir, "mock_ffprobe.sh")
	ffprobeScript := `#!/bin/sh
cat << 'EOF'
{
  "streams": [
    {
      "codec_type": "video",
      "codec_name": "hevc",
      "profile": "Main 10",
      "pix_fmt": "yuv420p10le",
      "width": 1920,
      "height": 1080
    },
    {
      "codec_type": "audio",
      "codec_name": "aac",
      "tags": { "language": "jpn" }
    },
    {
      "codec_type": "subtitle",
      "codec_name": "subrip",
      "tags": { "language": "eng" }
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

	// 3. Mock qBittorrent server with 14 files totaling exactly 6,120,930,408 bytes
	// (Real production case Fate/strange Fake)
	torHash := "f47e574a9ef41e0123456789abcdef0123456789"
	const expectedLogicalTotal int64 = 6120930408
	fileSizes := []int64{
		450000000, 435000000, 440000000, 432000000, 441000000,
		439000000, 442000000, 438000000, 440000000, 436000000,
		443000000, 437000000, 441000000, 406930408,
	}
	var sumCheck int64
	for _, sz := range fileSizes {
		sumCheck += sz
	}
	if sumCheck != expectedLogicalTotal {
		t.Fatalf("test setup error: file sizes sum to %d, want %d", sumCheck, expectedLogicalTotal)
	}

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
					SavePath:    tempDir,
					Size:        expectedLogicalTotal,
					Progress:    1.0,
					State:       "uploading",
				},
			})
		case strings.HasPrefix(r.URL.Path, "/api/v2/torrents/files"):
			var qbFiles []qbit.TorrentFile
			for i, sz := range fileSizes {
				qbFiles = append(qbFiles, qbit.TorrentFile{
					Name: fmt.Sprintf("Fate_strange_Fake/Fate_strange_Fake_EP%02d.mkv", i+1),
					Size: sz,
				})
			}
			_ = json.NewEncoder(w).Encode(qbFiles)
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

	// Verify total_size: must be 6,120,930,408 bytes, NOT doubled (12,241,860,816)
	totalSize, ok := runRes.Outputs["total_size"].(int64)
	if !ok {
		totalSize = int64(numVal(runRes.Outputs["total_size"]))
	}

	const doubledTotal int64 = 12241860816
	if totalSize == doubledTotal {
		t.Fatalf("BUG REPRODUCED: total_size was doubled to %d! Expected %d", totalSize, expectedLogicalTotal)
	}
	if totalSize != expectedLogicalTotal {
		t.Errorf("expected total_size %d, got %d", expectedLogicalTotal, totalSize)
	}

	// Verify bit_depth is detected as 10 from the HEVC 10-bit stream
	bitDepth, _ := runRes.Outputs["bit_depth"].(int)
	if bitDepth == 0 {
		bitDepth = int(numVal(runRes.Outputs["bit_depth"]))
	}
	if bitDepth != 10 {
		t.Errorf("expected bit_depth 10 for HEVC 10-bit stream, got %d", bitDepth)
	}
}

func TestSafeMediaReplacementWithDirectHashOrUrl(t *testing.T) {
	st := setupTestStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/327":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    327,
				"title": "Direct Movie",
			})
		case r.URL.Path == "/api/v3/release":
			t.Errorf("unexpected call to /api/v3/release when hash/url is provided directly!")
			w.WriteHeader(500)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
		},
	}
	reg := arrservice.NewRegistry(cfg)

	directHash := "11223344556677889900aabbccddeeff00112233"
	// Mock qbittorrent
	qbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v2/auth/login":
			w.Write([]byte("Ok."))
		case strings.HasPrefix(r.URL.Path, "/api/v2/torrents/info"):
			_ = json.NewEncoder(w).Encode([]qbit.TorrentInfo{
				{
					Hash:     directHash,
					Progress: 0.5,
					State:    "downloading",
				},
			})
		default:
			w.Write([]byte("Ok."))
		}
	}))
	defer qbSrv.Close()
	qbClient := qbit.NewClient(qbSrv.URL, "", "")

	engine := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: reg,
		Qbit:     qbClient,
	})

	ctx := context.Background()
	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":  "radarr",
		"media_id": "327",
		"hash":     directHash,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Should transition to wait_for_download (or waiting_external since qbit mock doesn't finish download)
	if runRes.Status != StatusWaitingExternal && runRes.Status != StatusCompleted {
		t.Errorf("expected running or waiting_external, got %s: %s", runRes.Status, runRes.Error)
	}
	if runRes.State["candidate_source"] != "user_input" {
		t.Errorf("expected candidate_source user_input, got %v", runRes.State["candidate_source"])
	}
}

func TestSafeMediaReplacementAutonomousSelection(t *testing.T) {
	st := setupTestStore(t)

	// Mock Radarr with /release endpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/327":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    327,
				"title": "Fate/strange Fake",
				"movieFile": map[string]any{
					"id":   100,
					"size": 10000000000, // 10 GB current
				},
			})
		case r.URL.Path == "/api/v3/release":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"guid":         "bad-release-720p",
					"title":        "Fate.strange.Fake.720p",
					"size":         15000000000,
					"seeders":      1,
					"downloadUrl":  "magnet:?xt=urn:btih:bad0000000000000000000000000000000000000",
					"releaseGroup": "RandomGroup",
				},
				{
					"guid":         "judas-1080p-hevc",
					"title":        "[Judas] Fate/strange Fake 1080p HEVC x265 10bit Dual Audio Multi-Subs",
					"size":         6120930408, // size reduction (ratio ~0.61 -> bonus)
					"seeders":      25,         // healthy seeders -> bonus
					"downloadUrl":  "magnet:?xt=urn:btih:c001000000000000000000000000000000000001",
					"releaseGroup": "Judas", // preferred group -> bonus
					"videoCodec":   "hevc",
					"resolution":   "1080p",
					"bitDepth":     10,
					"dualAudio":    true,
					"multiSubs":    true,
				},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
		},
	}
	cfg.Maintenance.PreferredGroups = []string{"Judas", "EMBER"}
	reg := arrservice.NewRegistry(cfg)

	// Mock qbittorrent
	qbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("Ok."))
	}))
	defer qbSrv.Close()
	qbClient := qbit.NewClient(qbSrv.URL, "", "")

	engine := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: reg,
		Qbit:     qbClient,
	})

	ctx := context.Background()
	// Run WITHOUT hash or url: should autonomously find, rank, and select Judas
	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":   "radarr",
		"media_id":  "327",
		"objective": "size_optimization",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Should have selected Judas candidate
	expectedHash := "c001000000000000000000000000000000000001"
	if runRes.State["hash"] != expectedHash {
		t.Errorf("expected hash %s selected, got %v", expectedHash, runRes.State["hash"])
	}
	if runRes.State["candidate_source"] != "auto_ranked" {
		t.Errorf("expected candidate_source auto_ranked, got %v", runRes.State["candidate_source"])
	}
}

func TestSafeMediaReplacementBlocklistExclusion(t *testing.T) {
	st := setupTestStore(t)

	// Pre-block the malicious or undesirable release
	blockedHash := "badb000000000000000000000000000000000000"
	_ = st.BlockRelease(blockedHash, "Known bad release", "admin")

	goodHash := "e00d000000000000000000000000000000000001"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/10":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    10,
				"title": "Movie 10",
			})
		case r.URL.Path == "/api/v3/release":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"guid":         "blocked-guid",
					"title":        "[EMBER] Movie 10 1080p HEVC",
					"size":         2000000000,
					"seeders":      100,
					"downloadUrl":  "magnet:?xt=urn:btih:" + blockedHash,
					"releaseGroup": "EMBER",
					"audio_langs":  []string{"eng"},
					"sub_langs":    []string{"eng"},
				},
				{
					"guid":         "valid-guid",
					"title":        "[Judas] Movie 10 1080p HEVC",
					"size":         2100000000,
					"seeders":      20,
					"downloadUrl":  "magnet:?xt=urn:btih:" + goodHash,
					"releaseGroup": "Judas",
					"videoCodec":   "hevc",
					"resolution":   "1080p",
					"audio_langs":  []string{"eng"},
					"sub_langs":    []string{"eng"},
				},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
		},
	}
	reg := arrservice.NewRegistry(cfg)

	qbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v2/auth/login":
			w.Write([]byte("Ok."))
		case strings.HasPrefix(r.URL.Path, "/api/v2/torrents/info"):
			_ = json.NewEncoder(w).Encode([]qbit.TorrentInfo{
				{
					Hash:     goodHash,
					Progress: 0.5,
					State:    "downloading",
				},
			})
		default:
			w.Write([]byte("Ok."))
		}
	}))
	defer qbSrv.Close()
	qbClient := qbit.NewClient(qbSrv.URL, "", "")

	engine := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: reg,
		Qbit:     qbClient,
	})

	ctx := context.Background()
	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":  "radarr",
		"media_id": "10",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Must NOT select blocked release
	if runRes.State["hash"] == blockedHash {
		t.Fatalf("SECURITY VIOLATION: Blocklisted candidate was selected!")
	}
	if runRes.State["hash"] != goodHash {
		t.Errorf("expected hash %s, got %v", goodHash, runRes.State["hash"])
	}

	// Also verify that if ONLY blocked releases exist, it fails semantically
	allBlockedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/11":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 11, "title": "Movie 11"})
		case r.URL.Path == "/api/v3/release":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"guid":        "only-blocked",
					"title":       "Movie 11",
					"downloadUrl": "magnet:?xt=urn:btih:" + blockedHash,
				},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer allBlockedSrv.Close()

	cfg.Services["radarr"] = config.ServiceConfig{
		URL: allBlockedSrv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3",
	}
	engineAllBlocked := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: arrservice.NewRegistry(cfg),
		Qbit:     qbClient,
	})
	allBlockedRes, err := engineAllBlocked.Run(ctx, "safe_media_replacement", map[string]any{
		"service":  "radarr",
		"media_id": "11",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if allBlockedRes.Status != StatusFailed {
		t.Errorf("expected failed status when all candidates are blocklisted, got %s", allBlockedRes.Status)
	}
	if !strings.Contains(allBlockedRes.Error, "no suitable replacement candidate found") {
		t.Errorf("expected error 'no suitable replacement candidate found', got %q", allBlockedRes.Error)
	}
}

func TestSafeMediaReplacementNoCandidatesFailsSemantically(t *testing.T) {
	st := setupTestStore(t)

	// Mock Radarr returning empty releases
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/99":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    99,
				"title": "Rare Movie",
			})
		case r.URL.Path == "/api/v3/release":
			_ = json.NewEncoder(w).Encode([]map[string]any{}) // No candidates!
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
		},
	}
	reg := arrservice.NewRegistry(cfg)

	engine := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: reg,
	})

	ctx := context.Background()
	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":  "radarr",
		"media_id": "99",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runRes.Status != StatusFailed {
		t.Fatalf("expected status failed, got %s", runRes.Status)
	}

	// Must fail with semantic error, NOT technical error like "could not determine infohash from inputs (provide hash or url)"
	if !strings.Contains(runRes.Error, "no suitable replacement candidate found") {
		t.Errorf("expected semantic error 'no suitable replacement candidate found', got %q", runRes.Error)
	}
	if strings.Contains(runRes.Error, "provide hash or url") {
		t.Errorf("technical error 'provide hash or url' leaked: %q", runRes.Error)
	}
}

func TestSafeMediaReplacementTradeOffWaitsDecision(t *testing.T) {
	st := setupTestStore(t)

	// Candidate with 0 seeders and huge size penalty (score <= 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/20":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    20,
				"title": "TradeOff Movie",
				"movieFile": map[string]any{
					"id":   1,
					"size": 1000000000,
				},
			})
		case r.URL.Path == "/api/v3/release":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"guid":        "trade-off-guid",
					"title":       "TradeOff.Movie.480p",
					"size":        5000000000, // 5x larger than current -> heavy penalties
					"seeders":     0,          // 0 seeds -> heavy penalty
					"downloadUrl": "magnet:?xt=urn:btih:trade000000000000000000000000000000000001",
				},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
		},
	}
	reg := arrservice.NewRegistry(cfg)

	engine := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: reg,
	})

	ctx := context.Background()
	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":  "radarr",
		"media_id": "20",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Must transition to waiting_decision because candidate has trade-offs
	if runRes.Status != StatusWaitingDecision {
		t.Fatalf("expected status waiting_decision, got %s (error: %s)", runRes.Status, runRes.Error)
	}
	if len(runRes.WaitingOptions) == 0 {
		t.Errorf("expected waiting options for decision")
	}

	// Cancel decision
	cancelRes, err := engine.Resume(ctx, runRes.ID, "cancel", nil)
	if err != nil {
		t.Fatalf("Resume cancel failed: %v", err)
	}
	if cancelRes.Status != StatusFailed {
		t.Errorf("expected status failed on cancel, got %s", cancelRes.Status)
	}
}

func TestSafeMediaReplacementRetryDoesNotRepeatSideEffects(t *testing.T) {
	st := setupTestStore(t)

	movieCalls := 0
	releaseCalls := 0
	chosenHash := "a1b2c3d4e5f60123456789abcdef0123456789ab"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/movie/30":
			movieCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    30,
				"title": "Retry Safe Movie",
				"movieFile": map[string]any{
					"id":   1,
					"size": 5000000000,
				},
			})
		case r.URL.Path == "/api/v3/release":
			releaseCalls++
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"guid":         "retry-guid",
					"title":        "[Judas] Retry Safe Movie 1080p HEVC",
					"size":         2500000000,
					"seeders":      30,
					"downloadUrl":  "magnet:?xt=urn:btih:" + chosenHash,
					"releaseGroup": "Judas",
					"videoCodec":   "hevc",
					"resolution":   "1080p",
					"audio_langs":  []string{"eng"},
					"sub_langs":    []string{"eng"},
				},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
		},
	}
	reg := arrservice.NewRegistry(cfg)

	// Engine without qbit initially -> step 0 (plan) and step 1 (find_and_rank) will complete,
	// but step 2 (add_or_track_download) will fail because Qbit is nil and no path provided!
	engine := NewEngine(EngineDeps{
		Store:    st,
		Config:   cfg,
		Registry: reg,
		Qbit:     nil, // triggers failure at step 2
	})

	ctx := context.Background()
	runRes, err := engine.Run(ctx, "safe_media_replacement", map[string]any{
		"service":  "radarr",
		"media_id": "30",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runRes.Status != StatusFailed {
		t.Fatalf("expected status failed at step 2, got %s", runRes.Status)
	}
	if !strings.Contains(runRes.Error, "qBittorrent client is not configured") {
		t.Errorf("expected qBittorrent failure, got %s", runRes.Error)
	}

	if movieCalls != 1 {
		t.Fatalf("expected movieCalls=1, got %d", movieCalls)
	}
	if releaseCalls != 1 {
		t.Fatalf("expected releaseCalls=1, got %d", releaseCalls)
	}

	// Now configure working qBittorrent mock
	qbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v2/auth/login":
			w.Write([]byte("Ok."))
		case strings.HasPrefix(r.URL.Path, "/api/v2/torrents/info"):
			_ = json.NewEncoder(w).Encode([]qbit.TorrentInfo{
				{
					Hash:     chosenHash,
					Progress: 0.5,
					State:    "downloading",
				},
			})
		default:
			w.Write([]byte("Ok."))
		}
	}))
	defer qbSrv.Close()
	engine.deps.Qbit = qbit.NewClient(qbSrv.URL, "", "")

	// Retry the failed action!
	retryRes, err := engine.Retry(ctx, runRes.ID)
	if err != nil {
		t.Fatalf("Retry failed: %v", err)
	}

	// Step 0 and step 1 must NOT have been called again!
	if movieCalls != 1 {
		t.Errorf("SIDE EFFECT REPEATED: expected movieCalls to remain 1, got %d", movieCalls)
	}
	if releaseCalls != 1 {
		t.Errorf("SIDE EFFECT REPEATED: expected releaseCalls to remain 1, got %d", releaseCalls)
	}

	// The retried action should have successfully resumed from step 2 (add_or_track_download)
	// and advanced to step 3 (wait_for_download -> waiting_external)
	if retryRes.Status != StatusWaitingExternal {
		t.Errorf("expected retried status waiting_external, got %s (error: %s)", retryRes.Status, retryRes.Error)
	}
	if retryRes.State["hash"] != chosenHash {
		t.Errorf("expected preserved hash %s, got %v", chosenHash, retryRes.State["hash"])
	}
}
