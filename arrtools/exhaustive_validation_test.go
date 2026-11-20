package arrtools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
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
			"id":              id,
			"title":           fmt.Sprintf("Movie Number %04d", id),
			"originalTitle":   fmt.Sprintf("Original Title %04d", id),
			"hasFile":         id%7 != 0,
			"overview":        "A full, verbose overview text that takes up dozens of kilobytes across hundreds of movies...",
			"year":            2000 + (id % 25),
			"added":           "2026-01-01T00:00:00Z",
			"status":          "released",
			"monitored":       true,
			"isAvailable":     true,
			"cleanTitle":      fmt.Sprintf("movienumber%04d", id),
			"folderName":      fmt.Sprintf("Movie Number %04d (2020)", id),
			"genres":          []string{"Action", "Drama", "Sci-Fi"},
			"tags":            []int{1, 2, 3},
			"ratings":         map[string]any{"imdb": map[string]any{"value": 7.5, "votes": 12000}},
			"runtime":         120,
			"certification":   "PG-13",
			"collection":      map[string]any{"name": "Saga Collection", "tmdbId": 12345},
			"alternateTitles": []map[string]any{{"title": "Alt 1"}, {"title": "Alt 2"}},
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
	RegisterAPICallTool(s, reg, nil, cfg.MaxResponseSizeKB, false)

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
		{"id": 1, "title": "Atlas of Stone", "size": 1000, "active": true},
		{"id": 2, "title": "Bellwether", "size": 5000, "active": false},
		{"id": 3, "title": "Comet Beacon", "size": 3000, "active": true},
		{"id": 4, "title": "Drift Signal", "size": 4000, "active": true},
		{"id": 5, "title": "Everglow", "size": 2000, "active": false},
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
	RegisterAPICallTool(s, reg, nil, cfg.MaxResponseSizeKB, false)

	// Filter gt: size > 2500
	resGt := callTool(t, s, "call_api", map[string]any{
		"service": "sonarr", "path": "/series", "filter": "size:gt:2500",
	})
	var outGt []map[string]any
	_ = json.Unmarshal([]byte(resultText(t, resGt)), &outGt)
	if len(outGt) != 3 { // Bellwether (5000), Comet Beacon (3000), Drift Signal (4000)
		t.Errorf("expected 3 items for size > 2500, got %d", len(outGt))
	}

	// Filter contains: title contains "be" (case-insensitive: Bellwether, Comet Beacon)
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
	RegisterAPICallTool(s, reg, nil, cfg.MaxResponseSizeKB, false)

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
