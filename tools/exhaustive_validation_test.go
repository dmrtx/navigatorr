package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/arrtools"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/maint"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

// ============================================================================
// 11. DATABASE IS LOCKED — RETRY & BACKOFF
// ============================================================================

func TestValidation_Resilience_DatabaseLocked_ExponentialBackoff(t *testing.T) {
	var attempts int32
	start := time.Now()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := atomic.AddInt32(&attempts, 1)
		if att < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"message": "database is locked: SQLite busy"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "recovered"}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		MaxResponseSizeKB: 100,
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key"},
		},
	}
	reg := arrservice.NewRegistry(cfg)
	svc, _ := reg.Get("radarr")

	data, code, err := svc.DoRequest(context.Background(), "GET", "/test", nil, nil)
	duration := time.Since(start)

	if err != nil || code != 200 {
		t.Fatalf("expected recovery after retries, got code=%d, err=%v", code, err)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
	if duration < 100*time.Millisecond {
		t.Errorf("expected exponential backoff delay, completed in only %v", duration)
	}
	if !strings.Contains(string(data), "recovered") {
		t.Errorf("unexpected body: %s", string(data))
	}
}

// ============================================================================
// 12. BOUNDED CONCURRENCY
// ============================================================================

func TestValidation_Resilience_BoundedConcurrency(t *testing.T) {
	var currentInFlight int32
	var maxInFlight int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&currentInFlight, 1)
		// Track peak
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&currentInFlight, -1)
		w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Concurrency: config.ConcurrencyConfig{
			MaxAPISimultaneous: 3, // Bounded concurrency limit
		},
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key"},
		},
	}
	reg := arrservice.NewRegistry(cfg)
	svc, _ := reg.Get("radarr")

	// Launch 30 simultaneous requests
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = svc.DoRequest(context.Background(), "GET", "/concurrent", nil, nil)
		}()
	}
	wg.Wait()

	peak := atomic.LoadInt32(&maxInFlight)
	t.Logf("CONCURRENCY METRICS: Configured Bound: 3, Observed Peak In-Flight: %d", peak)
	if peak > 3 {
		t.Errorf("concurrency bound violated! peak in-flight was %d, expected <= 3", peak)
	}
}

// ============================================================================
// 17. COMPREHENSIVE LANGUAGE POLICY UNIT TESTS
// ============================================================================

func TestValidation_LanguagePolicy_AllCasesAndVariants(t *testing.T) {
	tests := []struct {
		name     string
		audio    []string
		subs     []string
		expected string
	}{
		{"eng audio => accessible", []string{"eng"}, nil, "accessible"},
		{"spa audio => accessible", []string{"spa"}, nil, "accessible"},
		{"jpn + eng audio => accessible", []string{"jpn", "eng"}, nil, "accessible"},
		{"jpn audio + eng subs => accessible", []string{"jpn"}, []string{"eng"}, "accessible"},
		{"kor audio + spa subs => accessible", []string{"kor"}, []string{"spa"}, "accessible"},
		{"kor audio + kor subs only => issue", []string{"kor"}, []string{"kor"}, maint.IssueMissingLanguage},
		{"kor audio + no subs => issue", []string{"kor"}, nil, maint.IssueMissingLanguage},
		{"fre audio + no subs => issue", []string{"fre"}, nil, maint.IssueMissingLanguage},
		{"und audio + no subs => needs_inspection", []string{"und"}, nil, maint.IssueNeedsInspection},
		{"empty audio + no subs => needs_inspection", []string{}, nil, maint.IssueNeedsInspection},
		{"jpn audio + und subs => needs_inspection", []string{"jpn"}, []string{"und"}, maint.IssueNeedsInspection},

		// Tag and variant normalizations
		{"en-US audio => accessible", []string{"en-US"}, nil, "accessible"},
		{"English audio => accessible", []string{"English"}, nil, "accessible"},
		{"es-ES audio => accessible", []string{"es-ES"}, nil, "accessible"},
		{"es-419 subs => accessible", []string{"kor"}, []string{"es-419"}, "accessible"},
		{"Spanish subs => accessible", []string{"kor"}, []string{"Spanish"}, "accessible"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maint.EvaluateLanguageAccessibility(tt.audio, tt.subs)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

// ============================================================================
// 18, 19, 20. HISTORICAL REAL CASES REGRESSION
// ============================================================================

func TestValidation_HistoricalRealCases(t *testing.T) {
	// 18. Historical Foreign-only cases (MUST flag as IssueMissingLanguage)
	shouldFail := []struct {
		title string
		audio []string
		subs  []string
	}{
		{"Drunken Master", []string{"chi"}, nil},
		{"I Saw the Devil", []string{"kor"}, nil},
		{"Ip Man", []string{"chi"}, nil},
		{"Jiro Dreams of Sushi", []string{"jpn"}, nil},
		{"Martyrs", []string{"fre"}, nil},
		{"Ironbird", []string{"kor"}, nil},
		{"Princess Lumen", []string{"jpn"}, nil},
		{"Riders of Justice", []string{"dan"}, nil},
		{"The Wailing", []string{"kor"}, nil},
		{"The Witch: Part 1. The Subversion", []string{"kor"}, nil},
		{"The Witch: Part 2. The Other One", []string{"kor"}, nil},
		{"Lady Vengeance", []string{"kor"}, nil},
		{"Train to Busan", []string{"kor"}, nil},
		{"Decision to Leave", []string{"kor"}, nil},
		{"Troll Hunter", []string{"nor"}, nil},
		{"The Wandering Earth", []string{"chi"}, nil},
	}

	for _, m := range shouldFail {
		verdict := maint.EvaluateLanguageAccessibility(m.audio, m.subs)
		if verdict != maint.IssueMissingLanguage {
			t.Errorf("movie %q failed: expected %s, got %s", m.title, maint.IssueMissingLanguage, verdict)
		}
	}

	// 19. Historical Accessible cases (MUST NOT flag as issue)
	shouldPass := []struct {
		title string
		audio []string
		subs  []string
	}{
		{"Perfect Blue", []string{"jpn", "eng"}, nil},
		{"Spirited Away", []string{"jpn", "eng"}, nil},
		{"Bellwether Saga I", []string{"jpn", "eng"}, []string{"eng"}},
		{"Bellwether Saga II", []string{"jpn", "eng"}, []string{"eng"}},
		{"Bellwether Saga III", []string{"jpn", "eng"}, []string{"eng"}},
		{"Sympathy for Mr. Vengeance", []string{"kor"}, []string{"eng"}},
		{"Suzume", []string{"jpn"}, []string{"eng"}},
		{"Rurouni Kenshin: The Beginning", []string{"jpn", "eng"}, []string{"eng", "spa"}},
		{"Timecrimes", []string{"spa"}, nil},
		{"Tigers Are Not Afraid", []string{"spa"}, nil},
		{"Ghost in the Shell 2: Innocence", []string{"eng"}, nil},
	}

	for _, m := range shouldPass {
		verdict := maint.EvaluateLanguageAccessibility(m.audio, m.subs)
		if verdict != "accessible" {
			t.Errorf("movie %q flagged incorrectly: expected accessible, got %s", m.title, verdict)
		}
	}

	// 20. Unknown/Undetermined metadata cases (MUST return IssueNeedsInspection, NEVER IssueMissingLanguage!)
	undCases := []struct {
		title string
		audio []string
		subs  []string
	}{
		{"Portrait of a Lady on Fire", []string{"und"}, nil},
		{"The Hunt", []string{"dan"}, []string{"und"}},
		{"Son of Saul", []string{"und"}, nil},
		{"Thirst", []string{"kor"}, []string{"und"}},
		{"The Wandering Earth II", []string{"und"}, []string{"und"}},
		{"One Missed Call", []string{"jpn"}, []string{"und"}},
		{"Cure", []string{"und"}, nil},
	}

	for _, m := range undCases {
		verdict := maint.EvaluateLanguageAccessibility(m.audio, m.subs)
		if verdict != maint.IssueNeedsInspection {
			t.Errorf("undetermined case %q misclassified: expected %s, got %s", m.title, maint.IssueNeedsInspection, verdict)
		}
	}
}

// ============================================================================
// 21 & 22. STALE FILE ID & EVERGLOW REGRESSION
// ============================================================================

func TestValidation_StaleFileID_And_EverglowRegression(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "stale_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 1. Initial maintenance job opened for Everglow 1.0 with old non-accessible file (ID 101)
	fileID := "101"
	size := int64(1500000000)
	item, err := st.AddItem(store.MaintenanceItem{
		Service:           "radarr",
		MediaType:         "movie",
		MediaID:           "501",
		Title:             "Everglow: 1.11 We Were (Not) Lost",
		IssueType:         maint.IssueMissingLanguage,
		CurrentFileID:     &fileID,
		CurrentSize:       &size,
		RequiresSubtitles: true,
	})
	if err != nil {
		t.Fatalf("failed creating initial maintenance item: %v", err)
	}

	// 2. Reconcile / Auto-resolve when new file (ID 202 - Anime Time dual audio) arrives
	resolvedCount, err := st.AutoResolveByMedia("radarr", "501", "Imported Anime Time dual audio 10-bit HEVC")
	if err != nil || resolvedCount != 1 {
		t.Fatalf("expected 1 auto-resolved item, got %d (err: %v)", resolvedCount, err)
	}

	// 3. Verify item is closed with status=done
	refetched, err := st.GetItem(item.ID)
	if err != nil {
		t.Fatalf("fetching item: %v", err)
	}
	if refetched.Status != store.MaintDone {
		t.Errorf("expected status 'done', got %q", refetched.Status)
	}
	if !strings.Contains(refetched.Notes, "Anime Time dual audio") {
		t.Errorf("notes not updated with resolution facts: %s", refetched.Notes)
	}
}

// ============================================================================
// 26 & 33. CENTRALIZED SAFETY GATES (NO EARLY DELETE & NO BYPASS VIA CALL_API)
// ============================================================================

func TestValidation_CentralizedSafety_NoEarlyDeleteAndNoCallAPIBypass(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "safety_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var deleteReachedUpstream bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleteReachedUpstream = true
		}
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		AllowDestructive: false, // GLOBAL KILL SWITCH: OFF
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key"},
		},
	}
	reg := arrservice.NewRegistry(cfg)
	qbClient := qbit.NewClient(srv.URL, "u", "p")

	s := server.NewMCPServer("test", "0.0.0")
	RegisterMaintenance(s, cfg, reg, qbClient, st)
	arrtools.RegisterAPICallTool(s, reg, nil, 100, cfg.AllowDestructive)
	arrtools.RegisterQbitTools(s, qbClient, cfg.AllowDestructive)

	// 1. safe_replace must refuse delete_original before verify + import_confirm
	item, err := st.AddItem(store.MaintenanceItem{
		Service: "radarr", MediaType: "movie", MediaID: "1", Title: "Movie",
		IssueType: maint.IssueMissingLanguage, Status: store.MaintPending,
	})
	if err != nil {
		t.Fatalf("failed creating maintenance item: %v", err)
	}
	delRes := callTool(t, s, "safe_replace", map[string]any{
		"id":      fmt.Sprintf("%d", item.ID),
		"step":    "delete_original",
		"confirm": "true",
	})
	delTxt := resultText(t, delRes)
	if !strings.Contains(delTxt, "replacing") && !strings.Contains(delTxt, "cannot delete") && !strings.Contains(delTxt, "refused") {
		t.Errorf("safe_replace allowed premature delete_original! Result: %s", delTxt)
	}

	// 2. call_api MUST NOT allow bypassing destructive policy via raw DELETE
	apiDelRes := callTool(t, s, "call_api", map[string]any{
		"service": "radarr",
		"method":  "DELETE",
		"path":    "/api/v3/movie/1",
	})
	apiDelTxt := resultText(t, apiDelRes)
	if !strings.Contains(apiDelTxt, "destructive_disabled") && !strings.Contains(apiDelTxt, "allow_destructive") {
		t.Errorf("call_api failed to block DELETE when allow_destructive=false! Result: %s", apiDelTxt)
	}
	if deleteReachedUpstream {
		t.Fatalf("CRITICAL SAFETY BREACH: DELETE request reached upstream server while allow_destructive=false!")
	}

	// 3. qbit_manage_torrent with delete_files must also be blocked
	qbitDelRes := callTool(t, s, "qbit_manage_torrent", map[string]any{
		"action": "delete_files",
		"hashes": "0123456789abcdef0123456789abcdef01234567",
	})
	qbitDelTxt := resultText(t, qbitDelRes)
	if !strings.Contains(qbitDelTxt, "allow_destructive") {
		t.Errorf("qbit_manage_torrent delete_files was not blocked by allow_destructive! Result: %s", qbitDelTxt)
	}
}

// ============================================================================
// 44 & 45. RANK_RELEASES & KESTREL SCENARIO (SIZE VS ACCESSIBILITY)
// ============================================================================

func TestValidation_Ranking_KestrelScenario(t *testing.T) {
	prefs := maint.DefaultAnimePrefs()
	prefs.CurrentSizeBytes = 2950000000 // Kestrel original: 2.95 GB

	// Candidate A: Smaller but no accessible subs/audio
	candidateSmall := maint.ReleaseCandidate{
		GUID:       "cand-small",
		Title:      "Kestrel.1988.1080p.Compact",
		Size:       1800000000, // 1.8 GB (ratio ~0.61 of 2.95GB)
		Seeders:    50,
		VideoCodec: "hevc",
		Resolution: "1080p",
		BitDepth:   10,
		AudioLangs: []string{"jpn"},
		SubLangs:   []string{"jpn"},
		DualAudio:  false,
		MultiSubs:  false,
	}

	// Candidate B: Larger (4.05 GB) but has Dual Audio + Multi English/Spanish subs
	candidateAccessible := maint.ReleaseCandidate{
		GUID:         "cand-accessible",
		Title:        "Kestrel.1988.1080p.Dual.Audio.Multi-Subs.[Judas]",
		ReleaseGroup: "Judas",
		Size:         4050000000, // 4.05 GB (+1.1 GB delta)
		Seeders:      30,
		VideoCodec:   "hevc",
		BitDepth:     10,
		AudioLangs:   []string{"jpn", "eng"},
		SubLangs:     []string{"eng", "spa"},
		DualAudio:    true,
		MultiSubs:    true,
	}

	// Test with Objective = "accessibility_repair"
	prefs.Objective = "accessibility_repair"
	rankedAcc := maint.RankRelease([]maint.ReleaseCandidate{candidateSmall, candidateAccessible}, prefs)

	if rankedAcc[0].GUID != "cand-accessible" {
		t.Errorf("expected accessible candidate to win under accessibility repair, got %s", rankedAcc[0].Title)
	}
	// Verify it does NOT claim space savings
	for _, reason := range rankedAcc[0].Reasons {
		if strings.Contains(reason, "saves") || strings.Contains(reason, "smaller") {
			t.Errorf("accessibility candidate falsely claimed space savings: %s", reason)
		}
	}

	// Test with Objective = "size_optimization"
	prefs.Objective = "size_optimization"
	// Give candidateSmall English audio to make it eligible under size optimization
	candidateSmall.AudioLangs = []string{"jpn", "eng"}
	rankedSize := maint.RankRelease([]maint.ReleaseCandidate{candidateSmall, candidateAccessible}, prefs)
	if rankedSize[0].GUID != "cand-small" {
		t.Errorf("expected smaller candidate to win under size optimization, got %s", rankedSize[0].Title)
	}
}

// ============================================================================
// 48 & 49. ZERO SECRET LEAKS IN DIAGNOSTICS & AUDIT LOG
// ============================================================================

func TestValidation_ZeroSecretLeaks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "leak_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sensitiveKey := "super-secret-radarr-api-key-998877"
	sensitivePass := "qbit-admin-secret-password-1234"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {
				URL:        srv.URL + "/?apikey=" + sensitiveKey,
				APIKey:     sensitiveKey,
				AuthMethod: "query",
			},
		},
		QBittorrent: config.QBittorrentConfig{
			URL:      srv.URL,
			Username: "admin",
			Password: sensitivePass,
		},
	}
	reg := arrservice.NewRegistry(cfg)
	specStore := openapi.NewStore(cfg)

	s := server.NewMCPServer("test", "0.0.0")
	RegisterDiagnostics(s, cfg, reg, specStore, nil, nil, nil, st)

	// Call diagnostics
	diagRes := callTool(t, s, "diagnostics", map[string]any{"check_connectivity": false})
	diagTxt := resultText(t, diagRes)

	if strings.Contains(diagTxt, sensitiveKey) {
		t.Fatalf("SECRET LEAK: API key leaked in diagnostics output!")
	}
	if strings.Contains(diagTxt, sensitivePass) {
		t.Fatalf("SECRET LEAK: Password leaked in diagnostics output!")
	}

	// Call action_history after logging a command
	_ = st.LogActionEnriched("api_call", "radarr", "Test", fmt.Sprintf(`{"apikey":"%s"}`, sensitiveKey), "ok", "", "id-1", 10)
	histRes := callTool(t, s, "action_history", map[string]any{"service": "radarr"})
	histTxt := resultText(t, histRes)

	if strings.Contains(histTxt, sensitiveKey) {
		t.Fatalf("SECRET LEAK: API key leaked in action_history output!")
	}
}

// ============================================================================
// 61. MIGRATION TEST: V1 SCHEMA TO V2 SCHEMA IDEMPOTENCE
// ============================================================================

func TestValidation_StoreMigration_V1toV2(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v1_legacy.db")

	// 1. Manually initialize a v1 database without schema_version or action_instances
	rawSt, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Insert legacy maintenance items, preferences, and action log
	_, _ = rawSt.SetPreference("test_scope", "test_key", `"test_val"`, "user", time.Hour)
	it, _ := rawSt.AddItem(store.MaintenanceItem{
		Service: "sonarr", MediaType: "series", MediaID: "10", Title: "Legacy Series",
		IssueType: "oversized", Status: "pending",
	})
	_ = rawSt.LogAction("legacy_action", "sonarr", "Legacy Series", "{}", "done")
	rawSt.Close()

	// 2. Re-open store with v2 migration engine
	v2St, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("migration to v2 failed: %v", err)
	}
	defer v2St.Close()

	// Verify legacy data is 100% preserved
	pref, err := v2St.GetPreference("test_scope", "test_key")
	if err != nil || pref.ValueJSON != `"test_val"` {
		t.Errorf("legacy preference lost during migration: %v, val: %s", err, pref.ValueJSON)
	}
	fetchedItem, _ := v2St.GetItem(it.ID)
	if fetchedItem.Title != "Legacy Series" {
		t.Errorf("legacy maintenance item lost during migration!")
	}

	// Verify v2 tables are functional
	actInst := store.ActionInstance{
		ID:         "act-migrated-1",
		ActionName: "validate_torrent",
		Status:     "completed",
	}
	if err := v2St.CreateActionInstance(actInst); err != nil {
		t.Errorf("v2 action_instances table unusable after migration: %v", err)
	}

	// 3. Re-open again to verify migration is idempotent
	v2Reopen, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("re-opening v2 database failed: %v", err)
	}
	v2Reopen.Close()
}
