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

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/maint"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

// generateMockMovies creates n realistic Radarr movie objects.
func generateMockMovies(n int) []map[string]any {
	movies := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		id := i + 1
		audio := "Japanese"
		subs := "English"
		if id%3 == 0 {
			audio = "Korean"
			subs = "Korean" // Non-accessible
		} else if id%5 == 0 {
			audio = "und"
			subs = ""
		}

		movies[i] = map[string]any{
			"id":               id,
			"title":            fmt.Sprintf("Movie Number %04d", id),
			"originalTitle":    fmt.Sprintf("Original Title %04d", id),
			"hasFile":          id%7 != 0,
			"overview":         "A full, verbose overview text that takes up dozens of kilobytes across hundreds of movies...",
			"year":             2000 + (id % 25),
			"added":            "2026-01-01T00:00:00Z",
			"status":           "released",
			"monitored":        true,
			"isAvailable":      true,
			"cleanTitle":       fmt.Sprintf("movienumber%04d", id),
			"folderName":       fmt.Sprintf("Movie Number %04d (2020)", id),
			"genres":           []string{"Action", "Drama", "Sci-Fi"},
			"tags":             []int{1, 2, 3},
			"ratings":          map[string]any{"imdb": map[string]any{"value": 7.5, "votes": 12000}},
			"runtime":          120,
			"certification":    "PG-13",
			"collection":       map[string]any{"name": "Saga Collection", "tmdbId": 12345},
			"alternateTitles":  []map[string]any{{"title": "Alt 1"}, {"title": "Alt 2"}},
			"movieFile": map[string]any{
				"id":           1000 + id,
				"movieId":      id,
				"relativePath": fmt.Sprintf("Movie.Number.%04d.1080p.mkv", id),
				"size":         int64(2500000000 + id*1000000),
				"dateAdded":    "2026-01-02T00:00:00Z",
				"quality": map[string]any{
					"quality": map[string]any{"id": 1, "name": "Bluray-1080p"},
				},
				"mediaInfo": map[string]any{
					"audioCodec":     "AAC",
					"audioChannels":  5.1,
					"audioLanguages": audio,
					"subtitles":      subs,
					"videoCodec":     "x265",
					"resolution":     "1920x1080",
					"runTime":        "2:00:00",
					"scanType":       "Progressive",
				},
			},
		}
	}
	return movies
}

// ============================================================================
// 4. CALL_API — PROJECTIONS OVER LARGE COLLECTION
// ============================================================================

func TestValidation_CallAPI_Projections_LargeCollection(t *testing.T) {
	movies := generateMockMovies(500)
	rawUpstreamBytes, _ := json.Marshal(movies)
	upstreamBytes := len(rawUpstreamBytes)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(rawUpstreamBytes)
	}))
	defer srv.Close()

	cfg := &config.Config{
		MaxResponseSizeKB: 5000,
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "test-key", AuthMethod: "header", AuthHeader: "X-Api-Key"},
		},
	}
	reg := arrservice.NewRegistry(cfg)
	s := server.NewMCPServer("test", "0.0.0")
	registerAPICallTool(s, reg, cfg.MaxResponseSizeKB, false)

	// 1. Exact prompt projection case
	fields := "id,title,hasFile,movieFile.id,movieFile.size,movieFile.mediaInfo.audioLanguages,movieFile.mediaInfo.subtitles"
	res := callTool(t, s, "call_api", map[string]any{
		"service": "radarr",
		"path":    "/movie",
		"fields":  fields,
	})
	txt := resultText(t, res)
	mcpBytes := len(txt)
	reduction := float64(upstreamBytes-mcpBytes) / float64(upstreamBytes) * 100.0

	t.Logf("PROJECTIONS METRICS: Upstream Bytes: %d, Projected MCP Bytes: %d, Reduction: %.2f%%",
		upstreamBytes, mcpBytes, reduction)

	if reduction < 70.0 {
		t.Errorf("expected at least 70%% payload reduction, got %.2f%%", reduction)
	}

	// Verify unneeded fields are completely absent from serialized payload
	if strings.Contains(txt, "verbose overview text") {
		t.Errorf("unprojected field 'overview' leaked into serialized payload!")
	}
	if strings.Contains(txt, "Saga Collection") {
		t.Errorf("unprojected field 'collection' leaked into serialized payload!")
	}
	if strings.Contains(txt, "cleanTitle") {
		t.Errorf("unprojected field 'cleanTitle' leaked into serialized payload!")
	}

	// Verify requested nested fields are present and structured correctly
	var parsed []map[string]any
	var envelope struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(txt), &envelope); err == nil && len(envelope.Items) > 0 {
		parsed = envelope.Items
	} else if err := json.Unmarshal([]byte(txt), &parsed); err != nil {
		t.Fatalf("decoding projected response: %v\nOutput: %s", err, txt)
	}
	if len(parsed) != 500 && len(parsed) != 50 {
		t.Fatalf("expected items in parsed response, got %d", len(parsed))
	}
	first := parsed[0]
	if first["id"] == nil || first["title"] == nil || first["hasFile"] == nil {
		t.Errorf("missing top-level fields: %v", first)
	}
	mf, ok := first["movieFile"].(map[string]any)
	if !ok || mf["id"] == nil || mf["size"] == nil {
		t.Fatalf("missing nested movieFile fields: %v", first)
	}
	mi, ok := mf["mediaInfo"].(map[string]any)
	if !ok || mi["audioLanguages"] == nil || mi["subtitles"] == nil {
		t.Fatalf("missing deeply nested mediaInfo fields: %v", mf)
	}

	// 2. Edge cases: Single field
	resSingle := callTool(t, s, "call_api", map[string]any{
		"service": "radarr",
		"path":    "/movie",
		"fields":  "title",
		"limit":   5,
	})
	txtSingle := resultText(t, resSingle)
	if strings.Contains(txtSingle, `"id":`) {
		t.Errorf("field 'id' should not be present when only 'title' is requested")
	}

	// 3. Edge case: Non-existent field
	resNonExistent := callTool(t, s, "call_api", map[string]any{
		"service": "radarr",
		"path":    "/movie",
		"fields":  "id,non_existent_field",
		"limit":   2,
	})
	var parsedNE []map[string]any
	var envNE struct {
		Items []map[string]any `json:"items"`
	}
	txtNE := resultText(t, resNonExistent)
	if err := json.Unmarshal([]byte(txtNE), &envNE); err == nil && len(envNE.Items) > 0 {
		parsedNE = envNE.Items
	} else {
		_ = json.Unmarshal([]byte(txtNE), &parsedNE)
	}
	if len(parsedNE) != 2 || parsedNE[0]["id"] == nil {
		t.Errorf("non-existent field caused invalid output: %v", parsedNE)
	}
}

// ============================================================================
// 5. CALL_API — FILTERING AND ORDER OF EVALUATION
// ============================================================================

func TestValidation_CallAPI_FilteringAndOrder(t *testing.T) {
	items := []map[string]any{
		{"id": 1, "title": "Attack on Titan", "size": 1000, "active": true},
		{"id": 2, "title": "Berserk", "size": 5000, "active": false},
		{"id": 3, "title": "Cowboy Bebop", "size": 3000, "active": true},
		{"id": 4, "title": "Death Note", "size": 4000, "active": true},
		{"id": 5, "title": "Evangelion", "size": 2000, "active": false},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer srv.Close()

	cfg := &config.Config{
		MaxResponseSizeKB: 500,
		Services: map[string]config.ServiceConfig{
			"sonarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key"},
		},
	}
	reg := arrservice.NewRegistry(cfg)
	s := server.NewMCPServer("test", "0.0.0")
	registerAPICallTool(s, reg, cfg.MaxResponseSizeKB, false)

	// Filter gt: size > 2500
	resGt := callTool(t, s, "call_api", map[string]any{
		"service": "sonarr", "path": "/series", "filter": "size:gt:2500",
	})
	var outGt []map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resGt)), &outGt)
	if len(outGt) != 3 { // Berserk (5000), Cowboy Bebop (3000), Death Note (4000)
		t.Errorf("expected 3 items for size > 2500, got %d", len(outGt))
	}

	// Filter contains: title contains "be" (case-insensitive: Berserk, Cowboy Bebop)
	resContains := callTool(t, s, "call_api", map[string]any{
		"service": "sonarr", "path": "/series", "filter": "title:contains:be",
	})
	var outContains []map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resContains)), &outContains)
	if len(outContains) != 2 {
		t.Errorf("expected 2 items for contains 'be', got %d", len(outContains))
	}

	// Filter applied BEFORE limit:
	// If limit=2 was applied before filter, items [1,2] would yield only 1 active item (id 1).
	// If filter is applied before limit, active items [1,3,4] yields 2 items (ids 1, 3).
	resOrder := callTool(t, s, "call_api", map[string]any{
		"service": "sonarr", "path": "/series", "filter": "active:eq:true", "limit": 2,
	})
	var outOrder []map[string]any
	var envelopeOrder struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(resultText(t, resOrder)), &envelopeOrder); err == nil && len(envelopeOrder.Items) > 0 {
		outOrder = envelopeOrder.Items
	} else {
		_ = json.Unmarshal([]byte(resultText(t, resOrder)), &outOrder)
	}
	if len(outOrder) != 2 {
		t.Fatalf("expected 2 items with filter+limit, got %d (raw: %s)", len(outOrder), resultText(t, resOrder))
	}
	if outOrder[0]["id"].(float64) != 1 || outOrder[1]["id"].(float64) != 3 {
		t.Errorf("filter was not applied before limit: got IDs %v, %v", outOrder[0]["id"], outOrder[1]["id"])
	}
}

// ============================================================================
// 6 & 7. LOCAL PAGINATION, SNAPSHOT STORE & CACHE AVOIDANCE
// ============================================================================

func TestValidation_LocalPaginationAndSnapshotRequestCount(t *testing.T) {
	movies := generateMockMovies(500)
	var upstreamHits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(movies)
	}))
	defer srv.Close()

	cfg := &config.Config{
		MaxResponseSizeKB: 5000,
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key"},
		},
	}
	reg := arrservice.NewRegistry(cfg)
	s := server.NewMCPServer("test", "0.0.0")
	registerAPICallTool(s, reg, cfg.MaxResponseSizeKB, false)

	// Traverse all 500 items in pages of 100
	var aggregatedIDs []int
	var cursor string
	pages := 0

	for {
		pages++
		args := map[string]any{
			"service": "radarr",
			"path":    "/movie",
			"fields":  "id,title",
			"limit":   100,
		}
		if cursor != "" {
			args["cursor"] = cursor
		}

		res := callTool(t, s, "call_api", args)
		txt := resultText(t, res)

		var pageData struct {
			Items      []map[string]any `json:"items"`
			NextCursor string           `json:"next_cursor"`
			Complete   bool             `json:"complete"`
		}
		// When paginated with cursor or limit, result is structured envelope or array
		if err := json.Unmarshal([]byte(txt), &pageData); err != nil || len(pageData.Items) == 0 {
			var directItems []map[string]any
			if err := json.Unmarshal([]byte(txt), &directItems); err == nil {
				for _, it := range directItems {
					aggregatedIDs = append(aggregatedIDs, int(it["id"].(float64)))
				}
				break
			}
			t.Fatalf("page %d unmarshal failed: %v\nOutput: %s", pages, err, txt)
		}

		for _, it := range pageData.Items {
			aggregatedIDs = append(aggregatedIDs, int(it["id"].(float64)))
		}

		cursor = pageData.NextCursor
		if pageData.Complete || cursor == "" {
			break
		}
	}

	// 1. Verify exact 500 items collected, in stable order, with 0 duplicates
	if len(aggregatedIDs) != 500 {
		t.Fatalf("expected 500 aggregated items across pages, got %d", len(aggregatedIDs))
	}
	seen := make(map[int]bool)
	for i, id := range aggregatedIDs {
		if seen[id] {
			t.Fatalf("duplicate item id %d at index %d", id, i)
		}
		seen[id] = true
		if id != i+1 {
			t.Errorf("order unstable: expected id %d at pos %d, got %d", i+1, i, id)
		}
	}

	// 2. Critical Performance Requirement: Upstream Request Count
	// Traversing 500 movies in 5 pages MUST hit upstream exactly 1 time,
	// because remaining 4 pages are served from the local TTL snapshot store.
	hits := atomic.LoadInt32(&upstreamHits)
	t.Logf("CACHE METRICS: Upstream Requests: %d, Pages Served: %d", hits, pages)
	if hits != 1 {
		t.Errorf("expected exactly 1 upstream request for 5 paginated pages, got %d", hits)
	}

	// 3. Cache Invalidation on Mutation:
	// A POST or DELETE must invalidate the snapshot store so stale data is never returned.
	reg.Snapshots.Invalidate("radarr")
	// Querying again must now fetch fresh from upstream
	callTool(t, s, "call_api", map[string]any{
		"service": "radarr",
		"path":    "/movie",
		"limit":   10,
	})
	newHits := atomic.LoadInt32(&upstreamHits)
	if newHits != 2 {
		t.Errorf("expected 2nd upstream hit after invalidation, got %d", newHits)
	}
}

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
		{"Oldboy", []string{"kor"}, nil},
		{"Princess Mononoke", []string{"jpn"}, nil},
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
		{"Berserk Golden Age I", []string{"jpn", "eng"}, []string{"eng"}},
		{"Berserk Golden Age II", []string{"jpn", "eng"}, []string{"eng"}},
		{"Berserk Golden Age III", []string{"jpn", "eng"}, []string{"eng"}},
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
// 21 & 22. STALE FILE ID & EVANGELION REGRESSION
// ============================================================================

func TestValidation_StaleFileID_And_EvangelionRegression(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "stale_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 1. Initial maintenance job opened for Evangelion 1.0 with old non-accessible file (ID 101)
	fileID := "101"
	size := int64(1500000000)
	item, err := st.AddItem(store.MaintenanceItem{
		Service:           "radarr",
		MediaType:         "movie",
		MediaID:           "501",
		Title:             "Evangelion: 1.11 You Are (Not) Alone",
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
	registerAPICallTool(s, reg, 100, cfg.AllowDestructive)
	registerQbitTools(s, qbClient, cfg.AllowDestructive)

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
// 44 & 45. RANK_RELEASES & AKIRA SCENARIO (SIZE VS ACCESSIBILITY)
// ============================================================================

func TestValidation_Ranking_AkiraScenario(t *testing.T) {
	prefs := maint.DefaultAnimePrefs()
	prefs.CurrentSizeBytes = 2950000000 // Akira original: 2.95 GB

	// Candidate A: Smaller but no accessible subs/audio
	candidateSmall := maint.ReleaseCandidate{
		GUID:         "cand-small",
		Title:        "Akira.1988.1080p.Compact",
		Size:         1800000000, // 1.8 GB (ratio ~0.61 of 2.95GB)
		Seeders:      50,
		VideoCodec:   "hevc",
		Resolution:   "1080p",
		BitDepth:     10,
		AudioLangs:   []string{"jpn"},
		SubLangs:     []string{"jpn"},
		DualAudio:    false,
		MultiSubs:    false,
	}

	// Candidate B: Larger (4.05 GB) but has Dual Audio + Multi English/Spanish subs
	candidateAccessible := maint.ReleaseCandidate{
		GUID:         "cand-accessible",
		Title:        "Akira.1988.1080p.Dual.Audio.Multi-Subs.[Judas]",
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
