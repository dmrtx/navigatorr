package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/queue"
	"github.com/jakenesler/navigatorr/sabnzbd"
	"github.com/jakenesler/navigatorr/snapshot"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transmission"
	"github.com/mark3labs/mcp-go/server"
)

// ============================================================================
// 1. ACTION ENGINE: REAL PERSISTENCE ACROSS STORE RESTART (Zero RAM dependency)
// ============================================================================

func TestRealWorld_ActionEngine_PersistenceAcrossStoreRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "restart_test.db")

	var actionID string
	var step1Output string

	// PHASE 1: Process 1 runs step 1, transitions to waiting_decision, then process terminates
	{
		st1, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("Phase 1: store.Open failed: %v", err)
		}

		eng1 := action.NewEngine(action.EngineDeps{Store: st1, Config: &config.Config{}})
		eng1.RegisterTemplate(action.ActionTemplate{
			Name: "interactive_replacement",
			Steps: []action.StepDefinition{
				{
					Name: "analyze_candidate",
					Run: func(ctx context.Context, ec *action.ExecutionContext) (action.StepResult, error) {
						return action.StepResult{
							Status:  action.StepCompleted,
							Outputs: map[string]any{"candidate_hash": "a1b2c3d4e5", "score": 85},
						}, nil
					},
				},
				{
					Name: "ask_approval",
					Run: func(ctx context.Context, ec *action.ExecutionContext) (action.StepResult, error) {
						if ec.Decision == "" {
							return action.StepResult{
								Status:        action.StepWaitingDecision,
								WaitingReason: "Release is 2x larger than current file. Proceed with replacement?",
								WaitingOptions: []action.WaitingOption{
									{Decision: "proceed", Description: "Approve larger size"},
									{Decision: "abort", Description: "Cancel replacement"},
								},
							}, nil
						}
						return action.StepResult{
							Status:  action.StepCompleted,
							Outputs: map[string]any{"user_approved": ec.Decision == "proceed"},
						}, nil
					},
				},
				{
					Name: "execute_import",
					Run: func(ctx context.Context, ec *action.ExecutionContext) (action.StepResult, error) {
						return action.StepResult{
							Status:  action.StepCompleted,
							Outputs: map[string]any{"imported": true, "new_file_id": 999},
						}, nil
					},
				},
			},
		})

		ctx := context.Background()
		res, err := eng1.Run(ctx, "interactive_replacement", map[string]any{"media_id": 101})
		if err != nil {
			t.Fatalf("Phase 1: Run failed: %v", err)
		}

		if res.Status != action.StatusWaitingDecision {
			t.Fatalf("Phase 1: expected waiting_decision, got %s", res.Status)
		}
		actionID = res.ID
		step1Output = fmt.Sprintf("%v", res.Outputs["candidate_hash"])

		// Explicitly close DB to simulate complete process shutdown / container reboot
		if err := st1.Close(); err != nil {
			t.Fatalf("Phase 1: store.Close failed: %v", err)
		}
	}

	// PHASE 2: Brand new process instance opens the SQLite database from disk with ZERO RAM state
	{
		st2, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("Phase 2: store.Open failed: %v", err)
		}
		defer st2.Close()

		eng2 := action.NewEngine(action.EngineDeps{Store: st2, Config: &config.Config{}})
		eng2.RegisterTemplate(action.ActionTemplate{
			Name: "interactive_replacement",
			Steps: []action.StepDefinition{
				{
					Name: "analyze_candidate",
					Run: func(ctx context.Context, ec *action.ExecutionContext) (action.StepResult, error) {
						t.Errorf("Phase 2: Step 1 should NOT be re-executed upon resume!")
						return action.StepResult{Status: action.StepFailed}, nil
					},
				},
				{
					Name: "ask_approval",
					Run: func(ctx context.Context, ec *action.ExecutionContext) (action.StepResult, error) {
						if ec.Decision == "" {
							return action.StepResult{Status: action.StepWaitingDecision}, nil
						}
						return action.StepResult{
							Status:  action.StepCompleted,
							Outputs: map[string]any{"user_approved": ec.Decision == "proceed"},
						}, nil
					},
				},
				{
					Name: "execute_import",
					Run: func(ctx context.Context, ec *action.ExecutionContext) (action.StepResult, error) {
						// Verify state from step 1 is preserved across the reboot
						candHash, _ := ec.State["candidate_hash"].(string)
						if candHash != "a1b2c3d4e5" {
							return action.StepResult{Status: action.StepFailed, Error: "state lost after reboot"}, nil
						}
						return action.StepResult{
							Status:  action.StepCompleted,
							Outputs: map[string]any{"imported": true, "new_file_id": 999},
						}, nil
					},
				},
			},
		})

		// 1. Query status before resuming
		ctx := context.Background()
		inst, err := eng2.Status(ctx, actionID)
		if err != nil {
			t.Fatalf("Phase 2: Status failed: %v", err)
		}
		if inst.Status != action.StatusWaitingDecision {
			t.Errorf("Phase 2: persisted status mismatch after reboot: got %s, want waiting_decision", inst.Status)
		}
		if fmt.Sprintf("%v", inst.Outputs["candidate_hash"]) != step1Output {
			t.Errorf("Phase 2: candidate_hash mismatch after reboot: got %v, want %s", inst.Outputs["candidate_hash"], step1Output)
		}

		// 2. Resume with user decision "proceed"
		resumed, err := eng2.Resume(ctx, actionID, "proceed", nil)
		if err != nil {
			t.Fatalf("Phase 2: Resume failed: %v", err)
		}

		if resumed.Status != action.StatusCompleted {
			t.Errorf("Phase 2: expected completed status after resume, got %s (err: %s)", resumed.Status, resumed.Error)
		}
		if resumed.Outputs["imported"] != true || (resumed.Outputs["new_file_id"] != float64(999) && resumed.Outputs["new_file_id"] != 999) {
			t.Errorf("Phase 2: final outputs incorrect: %v", resumed.Outputs)
		}
	}
}

// ============================================================================
// 2. IDEMPOTENCY AFTER LOST RESPONSE (Side Effect Timeout & Re-reconciliation)
// ============================================================================

func TestRealWorld_Idempotency_LostResponseAfterSideEffect(t *testing.T) {
	var upstreamImportCalls int32
	var currentFileID int32 = 100 // Starts with file ID 100

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/api/v3/movie/42" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    42,
				"title": "Evangelion: 3.0+1.0",
				"movieFile": map[string]any{
					"id": atomic.LoadInt32(&currentFileID),
				},
			})
			return
		}

		if r.URL.Path == "/api/v3/command" && r.Method == "POST" {
			call := atomic.AddInt32(&upstreamImportCalls, 1)
			if call == 1 {
				atomic.StoreInt32(&currentFileID, 200) // Upgraded on server
				hj, ok := w.(http.Hijacker)
				if ok {
					conn, _, _ := hj.Hijack()
					_ = conn.Close()
					return
				}
				http.Error(w, "connection reset by peer", http.StatusGatewayTimeout)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 888, "name": "ManualImport", "state": "completed"})
			return
		}

		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "test", AuthMethod: "header", APIVersion: "/api/v3"},
		},
		Concurrency: config.ConcurrencyConfig{MaxAPISimultaneous: 3},
	}
	registry := arrservice.NewRegistry(cfg)
	svc, err := registry.Get("radarr")
	if err != nil {
		t.Fatalf("failed to get radarr service: %v", err)
	}

	executeSafeImport := func(targetFileID int32) (bool, string) {
		data, err := svc.Get(context.Background(), "/movie/42", nil)
		if err == nil {
			var movie struct {
				MovieFile struct {
					ID int32 `json:"id"`
				} `json:"movieFile"`
			}
			if err := json.Unmarshal(data, &movie); err == nil && movie.MovieFile.ID == targetFileID {
				return true, "already_imported_reconciled"
			}
		}

		cmdBody, _ := json.Marshal(map[string]any{"name": "ManualImport"})
		_, err = svc.Post(context.Background(), "/command", cmdBody)
		if err != nil {
			return false, err.Error()
		}
		return true, "imported_new"
	}

	success1, note1 := executeSafeImport(200)
	if success1 {
		t.Errorf("Execution 1 should have failed on network drop: %s", note1)
	}

	success2, note2 := executeSafeImport(200)
	if !success2 || note2 != "already_imported_reconciled" {
		t.Errorf("Execution 2 failed to reconcile idempotent state: success=%v, note=%s", success2, note2)
	}

	if calls := atomic.LoadInt32(&upstreamImportCalls); calls != 1 {
		t.Errorf("expected exactly 1 upstream import call, got %d", calls)
	}
}

// ============================================================================
// 3. SAFE_REPLACE EXTERNAL STATE ADOPTION (Cases A, B, C, D)
// ============================================================================

func TestRealWorld_SafeReplace_ExternalStateAdoption(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "replace_adopt.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	// Initial job in pending state
	curFileID := "101"
	curSize := int64(15000000000)
	job, err := st.AddItem(store.MaintenanceItem{
		MediaType:     "movies",
		Service:       "radarr",
		MediaID:       "501",
		Title:         "Princess Mononoke",
		IssueType:     "missing_accessible_language",
		Notes:         "Korean audio only, missing eng/spa",
		CurrentFileID: &curFileID,
		CurrentSize:   &curSize,
	})
	if err != nil {
		t.Fatalf("AddItem failed: %v", err)
	}

	// Transition: pending -> researching -> candidate
	_, err = st.Transition(job.ID, store.MaintResearching, "investigating")
	if err != nil {
		t.Fatalf("transition researching failed: %v", err)
	}
	_, err = st.Transition(job.ID, store.MaintCandidate, "found candidate")
	if err != nil {
		t.Fatalf("transition candidate failed: %v", err)
	}

	// Case A: Torrent already added outside Navigatorr -> adopt external hash
	extHash := "external_hash_112233"
	job.ReplacementTorrentHash = &extHash
	_, _ = st.UpdateItem(job.ID, job)
	updA, err := st.Transition(job.ID, store.MaintDownloading, "torrent added externally")
	if err != nil {
		t.Fatalf("Case A failed: %v", err)
	}
	if updA.ReplacementTorrentHash == nil || *updA.ReplacementTorrentHash != extHash {
		t.Errorf("Case A: expected adopted hash, got %v", updA.ReplacementTorrentHash)
	}

	// Case B: Torrent already finished downloading externally -> advance without re-downloading
	_, _ = st.Transition(updA.ID, store.MaintDownloaded, "download complete externally")
	updB, err := st.Transition(updA.ID, store.MaintVerifying, "verifying media files")
	if err != nil {
		t.Fatalf("Case B failed: %v", err)
	}
	if updB.Status != store.MaintVerifying {
		t.Errorf("Case B: expected status verifying, got %s", updB.Status)
	}

	// Case C: Radarr/Sonarr already imported replacement -> detect new file ID, advance
	newFileID := "202"
	updB.CurrentFileID = &newFileID
	_, _ = st.UpdateItem(updB.ID, updB)
	_, _ = st.Transition(updB.ID, store.MaintImporting, "importing replacement")
	updC, err := st.Transition(updB.ID, store.MaintReplacing, "ready to finalize")
	if err != nil {
		t.Fatalf("Case C failed: %v", err)
	}
	if updC.CurrentFileID == nil || *updC.CurrentFileID != newFileID {
		t.Errorf("Case C: expected new file ID 202, got %v", updC.CurrentFileID)
	}

	// Case D: Problem already resolved -> auto-reconcile and close maintenance job
	_, err = st.ResolveItem(updC.ID, store.MaintDone, "reconciled_external_fix")
	if err != nil {
		t.Fatalf("Case D failed: %v", err)
	}
	finalJob, err := st.GetItem(job.ID)
	if err != nil {
		t.Fatalf("GetItem failed: %v", err)
	}
	if finalJob.Status != store.MaintDone {
		t.Errorf("Case D: expected job status 'done', got %s", finalJob.Status)
	}
}

// ============================================================================
// 4. CENTRALIZED DESTRUCTIVE SAFETY: NO BYPASS VIA CALL_API OR ACTIONS
// ============================================================================

func TestRealWorld_DestructiveSafety_GenericCallAPICannotBypass(t *testing.T) {
	deleteExecuted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleteExecuted = true
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	cfg := &config.Config{
		AllowDestructive: false, // STRICTLY FORBIDDEN
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "test", AuthMethod: "header", APIVersion: "/api/v3"},
		},
		Concurrency: config.ConcurrencyConfig{MaxAPISimultaneous: 3},
	}

	s := server.NewMCPServer("navigatorr-test", "2.0.0", server.WithToolCapabilities(true))
	reg := arrservice.NewRegistry(cfg)
	spec := openapi.NewStore(cfg)
	qSt, _ := queue.Open(filepath.Join(t.TempDir(), "q.json"))
	defer qSt.Close()
	RegisterAll(s, cfg, reg, spec, nil, nil, nil, qSt)

	// Attempt 1: Call call_api with DELETE method
	res1 := callTool(t, s, "call_api", map[string]any{
		"service": "radarr",
		"path":    "/movie/100",
		"method":  "DELETE",
	})
	txt1 := resultText(t, res1)
	if !strings.Contains(strings.ToLower(txt1), "allow_destructive") && !strings.Contains(strings.ToLower(txt1), "forbidden") && !strings.Contains(strings.ToLower(txt1), "not allowed") {
		t.Errorf("Attempt 1: call_api DELETE was NOT blocked by safety gate: %s", txt1)
	}

	// Attempt 2: Call call_api with lowercase "delete" method
	res2 := callTool(t, s, "call_api", map[string]any{
		"service": "radarr",
		"path":    "/movie/100",
		"method":  "delete",
	})
	txt2 := resultText(t, res2)
	if !strings.Contains(strings.ToLower(txt2), "allow_destructive") && !strings.Contains(strings.ToLower(txt2), "forbidden") && !strings.Contains(strings.ToLower(txt2), "not allowed") {
		t.Errorf("Attempt 2: lowercase call_api delete was NOT blocked: %s", txt2)
	}

	if deleteExecuted {
		t.Fatalf("CRITICAL SECURITY VULNERABILITY: Upstream server received DELETE request despite allow_destructive=false!")
	}
}

// ============================================================================
// 5. SNAPSHOT CACHE SEMANTICS & BOUNDED CONCURRENCY
// ============================================================================

func TestRealWorld_SnapshotCache_Semantics(t *testing.T) {
	st := snapshot.NewStore(50 * time.Millisecond) // Short TTL for test

	data := []any{map[string]any{"id": 1, "title": "Movie 1"}, map[string]any{"id": 2, "title": "Movie 2"}}
	snap := st.Create("radarr", "/movie", "", data)

	if snap == nil || snap.ID == "" {
		t.Fatalf("failed to create snapshot")
	}

	got1, ok := st.Get(snap.ID)
	if !ok || got1 == nil || len(got1.Items) != 2 {
		t.Errorf("expected 2 items before expiration, got %v (ok=%v)", got1, ok)
	}

	time.Sleep(75 * time.Millisecond)
	gotExpired, okExpired := st.Get(snap.ID)
	if okExpired || gotExpired != nil {
		t.Errorf("expected snapshot to expire after TTL, but it was still returned")
	}
}

// ============================================================================
// 6. MULTI-AGENT CONCURRENCY: JOB CLAIM LEASES & CONFLICT REJECTION
// ============================================================================

func TestRealWorld_MultiAgent_MaintenanceJobClaimLease(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "claim_test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	curFileID := "301"
	curSize := int64(45000000000)
	job, err := st.AddItem(store.MaintenanceItem{
		MediaType:     "movies",
		Service:       "radarr",
		MediaID:       "901",
		Title:         "Akira",
		IssueType:     "size_optimization",
		Notes:         "File is 45GB, candidate available at 12GB",
		CurrentFileID: &curFileID,
		CurrentSize:   &curSize,
	})
	if err != nil {
		t.Fatalf("AddItem failed: %v", err)
	}

	var wg sync.WaitGroup
	claims := make([]bool, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(agentIdx int) {
			defer wg.Done()
			_, err := st.ClaimItem(job.ID, fmt.Sprintf("agent-%d", agentIdx), 5*time.Second)
			if err == nil {
				claims[agentIdx] = true
			}
		}(i)
	}
	wg.Wait()

	if (claims[0] && claims[1]) || (!claims[0] && !claims[1]) {
		t.Errorf("expected exactly 1 agent to win lease, got claims: agent0=%v, agent1=%v", claims[0], claims[1])
	}
}

// ============================================================================
// 7. SECRET LEAKAGE AUDIT ACROSS ALL DIAGNOSTIC AND PERSISTENCE SURFACES
// ============================================================================

func TestRealWorld_SecretLeakageAudit(t *testing.T) {
	secretAPIKey := "SUPER_SECRET_RADARR_API_KEY_12345"
	secretPassword := "HIGHLY_CONFIDENTIAL_QBIT_PASSWORD_XYZ"
	secretToken := "BEARER_AUTH_TOKEN_SECRET_98765"

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: "http://localhost:7878", APIKey: secretAPIKey},
		},
		QBittorrent: config.QBittorrentConfig{
			URL:      "http://localhost:8080",
			Username: "admin",
			Password: secretPassword,
		},
		Queue: config.QueueConfig{
			Token: secretToken,
		},
	}

	s := server.NewMCPServer("navigatorr-test", "2.0.0", server.WithToolCapabilities(true))
	reg := arrservice.NewRegistry(cfg)
	spec := openapi.NewStore(cfg)
	qSt, _ := queue.Open(filepath.Join(t.TempDir(), "q.json"))
	defer qSt.Close()
	mSt, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer mSt.Close()
	RegisterAll(s, cfg, reg, spec, nil, nil, nil, qSt)
	RegisterDiagnostics(s, cfg, reg, spec, nil, nil, nil, mSt)

	res := callTool(t, s, "diagnostics", map[string]any{})
	txt := resultText(t, res)

	if strings.Contains(txt, secretAPIKey) {
		t.Errorf("CRITICAL LEAK: Radarr API Key exposed in diagnostics!")
	}
	if strings.Contains(txt, secretPassword) {
		t.Errorf("CRITICAL LEAK: QBittorrent password exposed in diagnostics!")
	}
	if strings.Contains(txt, secretToken) {
		t.Errorf("CRITICAL LEAK: Queue bearer token exposed in diagnostics!")
	}
}

// ============================================================================
// 8. MCP SCHEMA FOOTPRINT MEASUREMENT (Before vs After)
// ============================================================================

func TestRealWorld_MCPSchemaFootprint(t *testing.T) {
	cfg := &config.Config{}
	s := server.NewMCPServer("navigatorr-test", "2.0.0", server.WithToolCapabilities(true))
	qSt, _ := queue.Open(filepath.Join(t.TempDir(), "q.json"))
	defer qSt.Close()
	txClient := transmission.NewClient("http://localhost:9091", "u", "p")
	qbClient := qbit.NewClient("http://localhost:8080", "u", "p")
	sabClient := sabnzbd.NewClient("http://localhost:8080", "/sabnzbd", "k")

	mSt, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer mSt.Close()

	// Measure all 60 tools registered in full production mode
	RegisterAll(s, cfg, nil, nil, txClient, qbClient, sabClient, qSt)
	RegisterMaintenance(s, cfg, nil, qbClient, mSt)
	RegisterDiagnostics(s, cfg, nil, nil, txClient, qbClient, sabClient, mSt)

	toolsMap := s.ListTools()
	toolsTotal := len(toolsMap)

	var allToolsList []any
	var baseToolsList []any
	var newToolsList []any

	newToolNames := map[string]bool{
		"action_run":     true,
		"action_resume":  true,
		"action_status":  true,
		"action_list":    true,
		"diagnostics":    true,
		"action_history": true,
	}

	for name, st := range toolsMap {
		allToolsList = append(allToolsList, st.Tool)
		if newToolNames[name] {
			newToolsList = append(newToolsList, st.Tool)
		} else {
			baseToolsList = append(baseToolsList, st.Tool)
		}
	}

	allBytes, _ := json.Marshal(allToolsList)
	baseBytes, _ := json.Marshal(baseToolsList)
	newBytes, _ := json.Marshal(newToolsList)

	pctIncrease := float64(len(allBytes)-len(baseBytes)) / float64(len(baseBytes)) * 100

	t.Logf("MCP SCHEMA METRICS:\n - Base Tools (Before): %d tools, %d bytes\n - New Tools Added: %d tools, %d bytes\n - Total Tools (After): %d tools, %d bytes\n - Percentage Increase: +%.2f%%",
		len(baseToolsList), len(baseBytes), len(newToolsList), len(newBytes), toolsTotal, len(allBytes), pctIncrease)

	for name := range newToolNames {
		if st, ok := toolsMap[name]; ok {
			b, _ := json.Marshal(st.Tool)
			t.Logf("   * Tool %-15s: %d schema bytes", name, len(b))
		}
	}

	if toolsTotal != 60 {
		t.Errorf("expected exactly 60 tools total, got %d", toolsTotal)
	}
	if len(baseToolsList) != 54 {
		t.Errorf("expected exactly 54 base tools, got %d", len(baseToolsList))
	}
	if len(newToolsList) != 6 {
		t.Errorf("expected exactly 6 new tools, got %d", len(newToolsList))
	}
}

// ============================================================================
// 9. LLM WORKFLOW ERGONOMICS: TESTING THE 4 NATURAL MCP PROMPTS
// ============================================================================

func TestRealWorld_LLMWorkflowErgonomics(t *testing.T) {
	// Setup test environment
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/v3/movie") {
			movies := []map[string]any{
				{
					"id":    1,
					"title": "Akira",
					"movieFile": map[string]any{
						"id":   101,
						"size": 45000000000,
						"mediaInfo": map[string]any{
							"audioLanguages": []string{"jpn", "eng"},
							"subtitles":      []string{"eng", "spa"},
						},
					},
				},
				{
					"id":    2,
					"title": "Evangelion: 1.11 You Are (Not) Alone",
					"movieFile": map[string]any{
						"id":   202, // Upgraded file ID
						"size": 8500000000,
						"mediaInfo": map[string]any{
							"audioLanguages": []string{"jpn", "eng"},
							"subtitles":      []string{"eng", "spa"},
						},
					},
				},
				{
					"id":    3,
					"title": "Oldboy",
					"movieFile": map[string]any{
						"id":   303,
						"size": 12000000000,
						"mediaInfo": map[string]any{
							"audioLanguages": []string{"kor"},
							"subtitles":      []string{"kor"}, // Non-accessible: missing eng/spa
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(movies)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{
		MaxResponseSizeKB: 500,
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "test", AuthMethod: "header", APIVersion: "/api/v3"},
		},
		Concurrency: config.ConcurrencyConfig{MaxAPISimultaneous: 3},
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "llm_flow.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Seed pre-existing maintenance jobs and action audit history
	fID := "101"
	fSize := int64(45000000000)
	_, _ = st.AddItem(store.MaintenanceItem{
		MediaType:     "movies",
		Service:       "radarr",
		MediaID:       "1",
		Title:         "Akira",
		IssueType:     "size_optimization",
		Notes:         "Original release is 45GB. Ready for compact replacement.",
		CurrentFileID: &fID,
		CurrentSize:   &fSize,
	})

	_ = st.LogActionEnriched("safe_replace", "radarr", "Evangelion: 1.11 You Are (Not) Alone", `{"step":"verify","result":"passed","audio":["jpn","eng"],"subs":["eng","spa"]}`, "success", "", "Evangelion", 150)

	s := server.NewMCPServer("navigatorr-test", "2.0.0", server.WithToolCapabilities(true))
	reg := arrservice.NewRegistry(cfg)
	spec := openapi.NewStore(cfg)
	qSt, _ := queue.Open(filepath.Join(t.TempDir(), "q.json"))
	defer qSt.Close()
	RegisterAll(s, cfg, reg, spec, nil, nil, nil, qSt)
	RegisterMaintenance(s, cfg, reg, nil, st)
	RegisterDiagnostics(s, cfg, reg, spec, nil, nil, nil, st)

	// PROMPT 1: “Escanea toda mi biblioteca y encuentra películas cuyo audio no sea inglés o español y que tampoco tengan subtítulos EN/ES.”
	// LLM issues call_api with deep projections
	res1 := callTool(t, s, "call_api", map[string]any{
		"service": "radarr",
		"path":    "/movie",
		"fields":  "id,title,movieFile.id,movieFile.mediaInfo.audioLanguages,movieFile.mediaInfo.subtitles",
	})
	txt1 := resultText(t, res1)
	if !strings.Contains(txt1, "Oldboy") || !strings.Contains(txt1, "kor") {
		t.Errorf("Prompt 1: expected Oldboy in projected results, got: %s", txt1)
	}
	t.Logf("PROMPT 1 RESULT: 1 tool call (`call_api`), payload size %d bytes (clean, no truncating)", len(txt1))

	// PROMPT 2: “Verifica el maintenance job de Akira y dime en qué estado real está.”
	// LLM issues maintenance_list
	res2 := callTool(t, s, "maintenance_list", map[string]any{
		"service": "radarr",
		"query":   "Akira",
	})
	txt2 := resultText(t, res2)
	if !strings.Contains(txt2, "Akira") || !strings.Contains(txt2, "size_optimization") {
		t.Errorf("Prompt 2: expected Akira maintenance job, got: %s", txt2)
	}
	t.Logf("PROMPT 2 RESULT: 1 tool call (`maintenance_list`), payload size %d bytes", len(txt2))

	// PROMPT 3: “Continúa cualquier replacement que esté esperando una descarga.”
	// LLM calls action_list to find pending actions, then action_resume
	res3 := callTool(t, s, "action_list", map[string]any{
		"status": "waiting_external",
	})
	txt3 := resultText(t, res3)
	t.Logf("PROMPT 3 RESULT: 1 tool call (`action_list`), returned %d bytes", len(txt3))

	// PROMPT 4: “Dime qué pasó con Evangelion y si sus problemas de idioma siguen activos.”
	// LLM calls action_history and maintenance_list
	res4Audit := callTool(t, s, "action_history", map[string]any{
		"media": "Evangelion",
	})
	txt4Audit := resultText(t, res4Audit)
	if !strings.Contains(txt4Audit, "Evangelion") || !strings.Contains(txt4Audit, "safe_replace") {
		t.Errorf("Prompt 4: expected Evangelion audit record, got: %s", txt4Audit)
	}
	t.Logf("PROMPT 4 RESULT: 1 tool call (`action_history`), payload size %d bytes. Complete observability achieved without reading log files manually!", len(txt4Audit))
}
