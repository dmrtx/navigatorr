package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/mark3labs/mcp-go/mcp"
)

// callAPI drives handleCallAPI against a stub *arr service.
func callAPI(t *testing.T, handler http.HandlerFunc, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"sonarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
		},
	}
	registry := arrservice.NewRegistry(cfg)

	if _, ok := args["service"]; !ok {
		args["service"] = "sonarr"
	}
	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "call_api", Arguments: args}}

	res, err := handleCallAPI(context.Background(), req, registry, nil, 50, false)
	if err != nil {
		t.Fatalf("handleCallAPI returned a transport error: %v", err)
	}
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// A non-2xx response carrying a JSON body must not read as success. *arr
// services return JSON on failure, so this previously parsed cleanly and the
// caller could not tell an auth failure from a real result.
func TestCallAPIStatusErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"unauthorized with json body", 401, `{"message":"Unauthorized"}`, true},
		{"unprocessable with json body", 422, `[{"errorMessage":"bad title"}]`, true},
		{"server error with json body", 500, `{"message":"boom"}`, true},
		{"not found with empty body", 404, ``, true},
		{"ok", 200, `{"id":1}`, false},
		{"created", 201, `{"id":2}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := callAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}, map[string]any{"path": "/series"})

			if res.IsError != tt.wantErr {
				t.Fatalf("status %d: IsError = %v, want %v (body: %s)",
					tt.status, res.IsError, tt.wantErr, resultText(t, res))
			}
			if tt.wantErr && !strings.Contains(resultText(t, res), "HTTP") {
				t.Errorf("error result should name the status, got: %s", resultText(t, res))
			}
		})
	}
}

// The API key must reach the service, and a lowercase method must still be
// recognized by the DELETE guard.
func TestCallAPIAuthAndDeleteGuard(t *testing.T) {
	var gotKey, gotMethod string
	h := func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotMethod = r.Method
		w.Write([]byte(`{"ok":true}`))
	}

	res := callAPI(t, h, map[string]any{"path": "/series"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	if gotKey != "k" {
		t.Errorf("X-Api-Key = %q, want %q", gotKey, "k")
	}
	if gotMethod != "GET" {
		t.Errorf("method = %q, want GET", gotMethod)
	}

	for _, m := range []string{"DELETE", "delete", " delete "} {
		res := callAPI(t, h, map[string]any{"path": "/series/1", "method": m})
		if !res.IsError {
			t.Errorf("method %q: DELETE guard did not block it", m)
			continue
		}
		// Must be stopped by the guard, not by the HTTP layer rejecting a
		// malformed method — otherwise the guard is failing open.
		if !strings.Contains(resultText(t, res), "allow_destructive") {
			t.Errorf("method %q: blocked by something other than the guard: %s", m, resultText(t, res))
		}
	}
}

func TestApplyFilter(t *testing.T) {
	items := []any{
		map[string]any{"title": "Alpha", "year": float64(2001), "hasFile": true},
		map[string]any{"title": "Beta", "year": float64(2020), "hasFile": false},
		map[string]any{"title": "Gamma", "year": float64(2015)}, // hasFile absent
	}

	tests := []struct {
		name   string
		filter string
		want   []string
	}{
		{"contains is case-insensitive", "title:contains:alp", []string{"Alpha"}},
		{"gt compares numerically", "year:gt:2010", []string{"Beta", "Gamma"}},
		{"lt compares numerically", "year:lt:2010", []string{"Alpha"}},
		{"eq matches", "hasFile:eq:true", []string{"Alpha"}},
		{"ne includes items missing the field", "hasFile:ne:true", []string{"Beta", "Gamma"}},
		{"no matches yields empty, not null", "title:contains:zzz", []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyFilter(items, tt.filter)
			if got == nil {
				t.Fatal("applyFilter returned nil; it must marshal as [] not null")
			}
			var titles []string
			for _, it := range got {
				titles = append(titles, it.(map[string]any)["title"].(string))
			}
			if len(titles) != len(tt.want) {
				t.Fatalf("got %v, want %v", titles, tt.want)
			}
			for i := range titles {
				if titles[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", titles, tt.want)
				}
			}

			data, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) == "null" {
				t.Error("empty filter result marshaled to null")
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("truncate should leave short strings alone, got %q", got)
	}
	long := strings.Repeat("x", 300)
	got := truncate(long, 100)
	if len(got) <= 100 || !strings.Contains(got, "truncated") {
		t.Errorf("truncate(300 chars, 100) = %d chars, want a truncation marker", len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 100)) {
		t.Error("truncate should keep the first max bytes")
	}
}

// Some APIs wrap their list one level deeper than the *arr convention, e.g.
// SABnzbd returns {"history": {"slots": [...]}} rather than {"records": [...]}.
// Field selection used to return an empty object for those: the drilling check
// only looked for an array directly under the requested key, and pickFields had
// no branch for arrays, so "history.slots.name" was dropped without an error.
func TestCallAPINestedArrayFields(t *testing.T) {
	body := `{"history":{"noofslots":2,"slots":[
		{"name":"first release","size":"1.2 GB","fail_message":""},
		{"name":"second release","size":"3.4 GB","fail_message":"bad par2"}
	]}}`

	res := callAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}, map[string]any{"path": "/api", "fields": "history.slots.name"})

	got := resultText(t, res)
	for _, want := range []string{"first release", "second release"} {
		if !strings.Contains(got, want) {
			t.Errorf("field selection dropped %q from a two-level response:\n%s", want, got)
		}
	}
	if strings.Contains(got, "3.4 GB") {
		t.Errorf("field selection returned unrequested fields:\n%s", got)
	}
}

// The one-level *arr shape has to keep working unchanged.
func TestCallAPISingleLevelArrayFieldsStillWork(t *testing.T) {
	body := `{"page":1,"records":[{"title":"a","year":2001},{"title":"b","year":2002}]}`

	res := callAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}, map[string]any{"path": "/wanted/missing", "fields": "records.title"})

	got := resultText(t, res)
	if !strings.Contains(got, `"a"`) || !strings.Contains(got, `"b"`) {
		t.Errorf("records.title stopped working:\n%s", got)
	}
	if strings.Contains(got, "2001") {
		t.Errorf("records.title returned unrequested fields:\n%s", got)
	}
}

func TestCallAPIDeepProjectionsAndSnapshotCursor(t *testing.T) {
	// Build 120 movie items
	type movieStub struct {
		ID        int            `json:"id"`
		Title     string         `json:"title"`
		Extra     string         `json:"extra"`
		MovieFile map[string]any `json:"movieFile"`
	}

	movies := make([]movieStub, 120)
	for i := 0; i < 120; i++ {
		movies[i] = movieStub{
			ID:    i + 1,
			Title: fmt.Sprintf("Movie %d", i+1),
			Extra: "should be filtered out by projection",
			MovieFile: map[string]any{
				"id":   (i + 1) * 10,
				"size": 4000000000 + int64(i),
				"mediaInfo": map[string]any{
					"audioLanguages": []string{"jpn", "eng"},
					"subtitles":      []string{"eng", "spa"},
					"videoCodec":     "hevc",
				},
			},
		}
	}

	upstreamCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		b, _ := json.Marshal(movies)
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
		},
	}
	registry := arrservice.NewRegistry(cfg)

	// Page 1: Request limit 50 with deep fields projection
	fields := "id,title,movieFile.id,movieFile.size,movieFile.mediaInfo.audioLanguages,movieFile.mediaInfo.subtitles"
	req1 := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service": "radarr",
			"path":    "/movie",
			"limit":   "50",
			"fields":  fields,
		},
	}}

	res1, err := handleCallAPI(context.Background(), req1, registry, nil, 50, false)
	if err != nil || res1.IsError {
		t.Fatalf("call 1 failed: %v, text: %s", err, resultText(t, res1))
	}

	text1 := resultText(t, res1)
	if strings.Contains(text1, "should be filtered out") || strings.Contains(text1, "hevc") {
		t.Errorf("projection leaked unrequested fields: %s", text1)
	}

	var page1 struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
		Complete   bool             `json:"complete"`
		Total      int              `json:"total"`
		Offset     int              `json:"offset"`
	}
	if err := json.Unmarshal([]byte(text1), &page1); err != nil {
		t.Fatalf("unmarshal page 1: %v", err)
	}

	if len(page1.Items) != 50 || page1.Complete || page1.Total != 120 || page1.NextCursor == "" {
		t.Fatalf("unexpected page 1: len=%d complete=%v total=%d cursor=%s",
			len(page1.Items), page1.Complete, page1.Total, page1.NextCursor)
	}

	// Verify deep projection fields on first item
	first := page1.Items[0]
	mf, ok := first["movieFile"].(map[string]any)
	if !ok {
		t.Fatalf("movieFile missing in projection: %+v", first)
	}
	mi, ok := mf["mediaInfo"].(map[string]any)
	if !ok {
		t.Fatalf("mediaInfo missing in projection: %+v", mf)
	}
	if _, hasAudio := mi["audioLanguages"]; !hasAudio {
		t.Errorf("audioLanguages missing from mediaInfo: %+v", mi)
	}
	if _, hasSubs := mi["subtitles"]; !hasSubs {
		t.Errorf("subtitles missing from mediaInfo: %+v", mi)
	}

	// Page 2: Request using cursor only (no upstream call should happen!)
	req2 := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"cursor": page1.NextCursor,
			"fields": fields,
		},
	}}

	res2, err := handleCallAPI(context.Background(), req2, registry, nil, 50, false)
	if err != nil || res2.IsError {
		t.Fatalf("call 2 failed: %v, text: %s", err, resultText(t, res2))
	}

	var page2 struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
		Complete   bool             `json:"complete"`
		Total      int              `json:"total"`
		Offset     int              `json:"offset"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res2)), &page2); err != nil {
		t.Fatalf("unmarshal page 2: %v", err)
	}

	if len(page2.Items) != 50 || page2.Complete || page2.Offset != 50 {
		t.Fatalf("unexpected page 2: len=%d complete=%v off=%d", len(page2.Items), page2.Complete, page2.Offset)
	}

	// Page 3: Request final page
	req3 := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"cursor": page2.NextCursor,
			"fields": fields,
		},
	}}

	res3, err := handleCallAPI(context.Background(), req3, registry, nil, 50, false)
	if err != nil || res3.IsError {
		t.Fatalf("call 3 failed: %v, text: %s", err, resultText(t, res3))
	}

	var page3 struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
		Complete   bool             `json:"complete"`
		Total      int              `json:"total"`
		Offset     int              `json:"offset"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res3)), &page3); err != nil {
		t.Fatalf("unmarshal page 3: %v", err)
	}

	if len(page3.Items) != 20 || !page3.Complete || page3.NextCursor != "" || page3.Offset != 100 {
		t.Fatalf("unexpected page 3: len=%d complete=%v next=%s off=%d", len(page3.Items), page3.Complete, page3.NextCursor, page3.Offset)
	}

	// Upstream must have been called EXACTLY ONCE!
	if upstreamCalls != 1 {
		t.Errorf("expected upstream to be called exactly 1 time, got %d", upstreamCalls)
	}
}

// 1. Unit test: JSON requests (legacy and explicit content_type)
func TestCallAPIJSONLegacyAndExplicit(t *testing.T) {
	var gotCT, gotAuth, gotBody string
	h := func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("X-Api-Key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id": 101, "title": "Inception"}`))
	}

	// Legacy call: body supplied, no content_type specified
	resLegacy := callAPI(t, h, map[string]any{
		"method": "POST",
		"path":   "/movie",
		"body":   `{"title": "Inception"}`,
	})
	if resLegacy.IsError {
		t.Fatalf("legacy call failed: %s", resultText(t, resLegacy))
	}
	if gotCT != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", gotCT)
	}
	if gotAuth != "k" {
		t.Errorf("expected auth header 'k', got %q", gotAuth)
	}
	if gotBody != `{"title": "Inception"}` {
		t.Errorf("expected body to be preserved, got %q", gotBody)
	}

	// Explicit call: content_type="application/json"
	resExplicit := callAPI(t, h, map[string]any{
		"method":       "POST",
		"path":         "/movie",
		"content_type": "application/json",
		"body":         `{"title": "Inception"}`,
	})
	if resExplicit.IsError {
		t.Fatalf("explicit call failed: %s", resultText(t, resExplicit))
	}
	if gotCT != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", gotCT)
	}
}

// 2. Unit test: Form-urlencoded with all required data types
func TestCallAPIFormURLEncoded(t *testing.T) {
	var gotCT, gotRawBody string
	var gotForm url.Values

	h := func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotRawBody = string(b)
		r.Body = io.NopCloser(bytes.NewReader(b))
		_ = r.ParseForm()
		gotForm = r.Form
		w.WriteHeader(http.StatusNoContent)
	}

	formArgs := map[string]any{
		"name":    "English Subtitles",
		"active":  true,
		"score":   85,
		"spaces":  "hello world",
		"unicode": "Película Español 🎬",
		"special": "a=1&b=2+3",
		"tags":    []any{"en", "es"},
	}

	res := callAPI(t, h, map[string]any{
		"method":       "POST",
		"path":         "/settings",
		"content_type": "application/x-www-form-urlencoded",
		"form":         formArgs,
	})
	if res.IsError {
		t.Fatalf("form call failed: %s", resultText(t, res))
	}

	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("expected Content-Type application/x-www-form-urlencoded, got %q", gotCT)
	}

	// Validate parsed values
	if gotForm.Get("name") != "English Subtitles" {
		t.Errorf("name = %q, want 'English Subtitles'", gotForm.Get("name"))
	}
	if gotForm.Get("active") != "true" {
		t.Errorf("active = %q, want 'true'", gotForm.Get("active"))
	}
	if gotForm.Get("score") != "85" {
		t.Errorf("score = %q, want '85'", gotForm.Get("score"))
	}
	if gotForm.Get("spaces") != "hello world" {
		t.Errorf("spaces = %q, want 'hello world'", gotForm.Get("spaces"))
	}
	if gotForm.Get("unicode") != "Película Español 🎬" {
		t.Errorf("unicode = %q, want 'Película Español 🎬'", gotForm.Get("unicode"))
	}
	if gotForm.Get("special") != "a=1&b=2+3" {
		t.Errorf("special = %q, want 'a=1&b=2+3'", gotForm.Get("special"))
	}

	// Verify repeated keys for array
	tags := gotForm["tags"]
	if len(tags) != 2 || tags[0] != "en" || tags[1] != "es" {
		t.Errorf("tags = %v, want ['en', 'es']", tags)
	}

	// Ensure no raw JSON array leakage like tags=["en","es"]
	if strings.Contains(gotRawBody, `["en"`) {
		t.Errorf("raw body leaked json array formatting: %s", gotRawBody)
	}
}

// 3. Unit test: Complex JSON field inside form (Bazarr languages-profiles pattern)
func TestCallAPIFormComplexJSONField(t *testing.T) {
	var gotForm url.Values
	h := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.Form
		w.WriteHeader(http.StatusNoContent)
	}

	profilesData := []map[string]any{
		{
			"profileId": 1,
			"name":      "Accessible EN+ES",
			"cutoff":    65535,
			"items": []map[string]any{
				{"id": 1, "language": "en"},
				{"id": 2, "language": "es"},
			},
		},
	}

	res := callAPI(t, h, map[string]any{
		"method": "POST",
		"path":   "/system/settings",
		"form": map[string]any{
			"languages-profiles": profilesData,
			"languages-enabled":  []any{"en", "es"},
		},
	})
	if res.IsError {
		t.Fatalf("complex form call failed: %s", resultText(t, res))
	}

	lp := gotForm.Get("languages-profiles")
	if lp == "" {
		t.Fatal("languages-profiles field is empty")
	}

	var parsedProfiles []map[string]any
	if err := json.Unmarshal([]byte(lp), &parsedProfiles); err != nil {
		t.Fatalf("languages-profiles is not valid json: %v", err)
	}
	if len(parsedProfiles) != 1 || parsedProfiles[0]["name"] != "Accessible EN+ES" {
		t.Errorf("unexpected parsed profiles: %+v", parsedProfiles)
	}

	// Repeated keys for languages-enabled
	enabled := gotForm["languages-enabled"]
	if len(enabled) != 2 || enabled[0] != "en" || enabled[1] != "es" {
		t.Errorf("languages-enabled = %v, want ['en', 'es']", enabled)
	}
}

// 4. Unit test: Error handling and validation
func TestCallAPIErrors(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}

	// Simultaneous body and form
	resBoth := callAPI(t, h, map[string]any{
		"method": "POST",
		"path":   "/test",
		"body":   `{"foo":"bar"}`,
		"form":   map[string]any{"baz": "qux"},
	})
	if !resBoth.IsError || !strings.Contains(resultText(t, resBoth), "cannot provide both body and form") {
		t.Errorf("expected error for simultaneous body and form, got: %s", resultText(t, resBoth))
	}

	// Form with GET method
	resFormGet := callAPI(t, h, map[string]any{
		"method": "GET",
		"path":   "/test",
		"form":   map[string]any{"foo": "bar"},
	})
	if !resFormGet.IsError || !strings.Contains(resultText(t, resFormGet), "cannot use form with HTTP method GET") {
		t.Errorf("expected error for form with GET method, got: %s", resultText(t, resFormGet))
	}

	// Unsupported content type
	resBadCT := callAPI(t, h, map[string]any{
		"method":       "POST",
		"path":         "/test",
		"content_type": "text/xml",
		"body":         "<xml/>",
	})
	if !resBadCT.IsError || !strings.Contains(resultText(t, resBadCT), "unsupported content_type") {
		t.Errorf("expected error for unsupported content type, got: %s", resultText(t, resBadCT))
	}

	// Form with content_type application/json
	resFormJSON := callAPI(t, h, map[string]any{
		"method":       "POST",
		"path":         "/test",
		"content_type": "application/json",
		"form":         map[string]any{"foo": "bar"},
	})
	if !resFormJSON.IsError || !strings.Contains(resultText(t, resFormJSON), "cannot use form with content_type application/json") {
		t.Errorf("expected error for form with application/json, got: %s", resultText(t, resFormJSON))
	}

	// Body with content_type form-urlencoded
	resBodyForm := callAPI(t, h, map[string]any{
		"method":       "POST",
		"path":         "/test",
		"content_type": "application/x-www-form-urlencoded",
		"body":         `{"foo":"bar"}`,
	})
	if !resBodyForm.IsError || !strings.Contains(resultText(t, resBodyForm), "cannot use body with content_type application/x-www-form-urlencoded") {
		t.Errorf("expected error for body with form-urlencoded, got: %s", resultText(t, resBodyForm))
	}
}

// 5. Unit test: Zero-leak diagnostics and secret sanitization in errors
func TestCallAPIZeroLeakDiagnosticsAndErrorSanitization(t *testing.T) {
	secretKey := "super-secret-api-key-xyz123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// Server echoes back bad credentials in error body
		w.Write([]byte(fmt.Sprintf("Invalid request with apikey=%s and auth token %s", secretKey, secretKey)))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"bazarr": {URL: srv.URL, APIKey: secretKey, AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api"},
		},
	}
	registry := arrservice.NewRegistry(cfg)

	req := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service": "bazarr",
			"method":  "POST",
			"path":    "/system/settings",
			"form": map[string]any{
				"password": "my-secret-password",
			},
		},
	}}

	res, err := handleCallAPI(context.Background(), req, registry, nil, 50, false)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result for 400 status")
	}

	errText := resultText(t, res)
	if strings.Contains(errText, secretKey) {
		t.Errorf("SECRET LEAK: error text contains API key: %s", errText)
	}
	if strings.Contains(errText, "my-secret-password") {
		t.Errorf("SECRET LEAK: error text contains form password: %s", errText)
	}
	if !strings.Contains(errText, "***REDACTED***") {
		t.Errorf("expected ***REDACTED*** placeholder in error text, got: %s", errText)
	}
}

// 6. Unit test: Response metadata and mutation verification
func TestCallAPIIncludeMetadata(t *testing.T) {
	// A. Status 200 OK with JSON
	h200 := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":"ok"}`))
	}
	res200 := callAPI(t, h200, map[string]any{
		"method":           "POST",
		"path":             "/movie",
		"body":             `{"name":"test"}`,
		"include_metadata": true,
	})
	if res200.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res200))
	}
	var meta200 map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res200)), &meta200); err != nil {
		t.Fatalf("failed to unmarshal metadata envelope: %v", err)
	}
	if meta200["status_code"] != float64(200) {
		t.Errorf("expected status_code 200, got %v", meta200["status_code"])
	}
	if meta200["mutating"] != true {
		t.Errorf("expected mutating=true for POST, got %v", meta200["mutating"])
	}

	// B. Status 204 No Content
	h204 := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}
	res204 := callAPI(t, h204, map[string]any{
		"method":           "POST",
		"path":             "/system/settings",
		"form":             map[string]any{"foo": "bar"},
		"include_metadata": true,
	})
	if res204.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res204))
	}
	var meta204 map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res204)), &meta204); err != nil {
		t.Fatalf("failed to unmarshal metadata envelope for 204: %v", err)
	}
	if meta204["status_code"] != float64(204) {
		t.Errorf("expected status_code 204, got %v", meta204["status_code"])
	}
	if meta204["mutating"] != true {
		t.Errorf("expected mutating=true for 204 POST, got %v", meta204["mutating"])
	}

	// C. Without include_metadata: legacy 204 behavior
	resLegacy204 := callAPI(t, h204, map[string]any{
		"method": "POST",
		"path":   "/system/settings",
		"form":   map[string]any{"foo": "bar"},
	})
	if resLegacy204.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, resLegacy204))
	}
	txtLegacy := resultText(t, resLegacy204)
	if !strings.HasPrefix(txtLegacy, "status: 204") {
		t.Errorf("expected legacy 204 output to start with 'status: 204', got %q", txtLegacy)
	}
}

// 7. Simulation test: Bazarr 1.6.0 language profiles workflow
func TestCallAPIBazarrSimulation(t *testing.T) {
	var currentProfiles []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/system/languages/profiles":
			w.Header().Set("Content-Type", "application/json")
			if currentProfiles == nil {
				w.Write([]byte("[]"))
			} else {
				data, _ := json.Marshal(currentProfiles)
				w.Write(data)
			}

		case r.Method == "POST" && r.URL.Path == "/api/system/settings":
			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
				// Bazarr bug simulation: if JSON is sent, body is ignored and 204 returned without updating
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = r.ParseForm()
			lp := r.Form.Get("languages-profiles")
			if lp != "" {
				var profiles []map[string]any
				if err := json.Unmarshal([]byte(lp), &profiles); err == nil {
					currentProfiles = profiles
				}
			}
			w.WriteHeader(http.StatusNoContent)

		case r.Method == "POST" && r.URL.Path == "/api/movies":
			// Query parameter profile assignment: POST /api/movies?radarrid=...&profileid=...
			w.WriteHeader(http.StatusNoContent)

		case r.Method == "GET" && r.URL.Path == "/api/movies":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data": [{"title": "Test Movie", "radarrId": 100, "profileId": 1}]}`))

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"bazarr": {URL: srv.URL, APIKey: "test-mock-key", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api"},
		},
	}
	registry := arrservice.NewRegistry(cfg)

	// Step A: Read initial profiles -> []
	reqGet1 := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service": "bazarr",
			"method":  "GET",
			"path":    "/system/languages/profiles",
		},
	}}
	resGet1, _ := handleCallAPI(context.Background(), reqGet1, registry, nil, 50, false)
	if resultText(t, resGet1) != "[]" {
		t.Fatalf("expected [], got %s", resultText(t, resGet1))
	}

	// Step B: Create language profile using form-urlencoded
	reqPost := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service":      "bazarr",
			"method":       "POST",
			"path":         "/system/settings",
			"content_type": "application/x-www-form-urlencoded",
			"form": map[string]any{
				"languages-profiles": []map[string]any{
					{
						"profileId": 1,
						"name":      "Accessible EN+ES",
						"cutoff":    65535,
						"items": []map[string]any{
							{"id": 1, "language": "en"},
							{"id": 2, "language": "es"},
						},
					},
				},
			},
		},
	}}
	resPost, _ := handleCallAPI(context.Background(), reqPost, registry, nil, 50, false)
	if resPost.IsError {
		t.Fatalf("POST /system/settings failed: %s", resultText(t, resPost))
	}

	// Step C: Verify with GET
	reqGet2 := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service": "bazarr",
			"method":  "GET",
			"path":    "/system/languages/profiles",
		},
	}}
	resGet2, _ := handleCallAPI(context.Background(), reqGet2, registry, nil, 50, false)
	if resGet2.IsError {
		t.Fatalf("GET after POST failed: %s", resultText(t, resGet2))
	}

	var verifiedProfiles []map[string]any
	if err := json.Unmarshal([]byte(resultText(t, resGet2)), &verifiedProfiles); err != nil {
		t.Fatalf("failed to unmarshal verified profiles: %v", err)
	}
	if len(verifiedProfiles) != 1 || verifiedProfiles[0]["name"] != "Accessible EN+ES" {
		t.Fatalf("profile verification failed: %+v", verifiedProfiles)
	}

	// Step D: Assign profile to movie via POST /movies with query params
	reqAssign := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service": "bazarr",
			"method":  "POST",
			"path":    "/movies",
			"query": map[string]any{
				"radarrid":  "100",
				"profileid": "1",
			},
			"include_metadata": true,
		},
	}}
	resAssign, err := handleCallAPI(context.Background(), reqAssign, registry, nil, 50, true)
	if err != nil || resAssign.IsError {
		t.Fatalf("assign movie profile failed: %v, text: %s", err, resultText(t, resAssign))
	}

	// Step E: Verify movie assignment with GET /movies
	reqGetMovie := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service": "bazarr",
			"method":  "GET",
			"path":    "/movies",
			"query": map[string]any{
				"radarrid[]": "100",
			},
		},
	}}
	resGetMovie, err := handleCallAPI(context.Background(), reqGetMovie, registry, nil, 50, true)
	if err != nil || resGetMovie.IsError {
		t.Fatalf("get movie failed: %v, text: %s", err, resultText(t, resGetMovie))
	}
	movieText := resultText(t, resGetMovie)
	if !strings.Contains(movieText, `"profileId": 1`) && !strings.Contains(movieText, `"profileId":1`) {
		t.Errorf("expected movie to have profileId 1, got: %s", movieText)
	}
}

// 8. Optional integration test against real Bazarr instance configured via environment variables.
// Skipped by default; set BAZARR_TEST_URL and BAZARR_TEST_API_KEY to run.
func TestCallAPIRealBazarrIntegration(t *testing.T) {
	bazarrURL := os.Getenv("BAZARR_TEST_URL")
	bazarrKey := os.Getenv("BAZARR_TEST_API_KEY")
	movieID := os.Getenv("BAZARR_TEST_MOVIE_ID")
	if movieID == "" {
		movieID = "1"
	}
	if bazarrURL == "" || bazarrKey == "" {
		t.Skip("skipping real Bazarr integration test: set BAZARR_TEST_URL and BAZARR_TEST_API_KEY to enable")
	}

	client := &http.Client{Timeout: 5 * time.Second}
	pingReq, err := http.NewRequest("GET", strings.TrimRight(bazarrURL, "/")+"/api/system/status", nil)
	if err != nil {
		t.Fatalf("failed to build ping request: %v", err)
	}
	pingReq.Header.Set("X-API-KEY", bazarrKey)
	pingResp, err := client.Do(pingReq)
	if err != nil || pingResp.StatusCode != 200 {
		t.Fatalf("Bazarr instance at %s not reachable: %v", bazarrURL, err)
	}
	pingResp.Body.Close()

	cfg := &config.Config{
		AllowDestructive: true,
		Services: map[string]config.ServiceConfig{
			"bazarr": {URL: bazarrURL, APIKey: bazarrKey, AuthMethod: "header", AuthHeader: "X-API-KEY", APIVersion: "/api"},
		},
	}
	registry := arrservice.NewRegistry(cfg)
	ctx := context.Background()

	// A. Read initial profiles
	reqGetInitial := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service": "bazarr",
			"method":  "GET",
			"path":    "/system/languages/profiles",
		},
	}}
	resGetInitial, err := handleCallAPI(ctx, reqGetInitial, registry, nil, 50, true)
	if err != nil || resGetInitial.IsError {
		t.Fatalf("initial GET failed: %v, text: %s", err, resultText(t, resGetInitial))
	}

	// B. Create language profile "Accessible EN+ES" with English and Spanish
	profilesPayload := `[{"profileId": 1, "name": "Accessible EN+ES", "cutoff": 65535, "items": [{"id": 1, "language": "en", "audio_exclude": "False", "audio_only_include": "False", "hi": "False", "forced": "False"}, {"id": 2, "language": "es", "audio_exclude": "False", "audio_only_include": "False", "hi": "False", "forced": "False"}], "mustContain": [], "mustNotContain": [], "originalFormat": null}]`

	reqCreate := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service":          "bazarr",
			"method":           "POST",
			"path":             "/system/settings",
			"content_type":     "application/x-www-form-urlencoded",
			"include_metadata": true,
			"form": map[string]any{
				"languages-profiles": profilesPayload,
			},
		},
	}}
	resCreate, err := handleCallAPI(ctx, reqCreate, registry, nil, 50, true)
	if err != nil || resCreate.IsError {
		t.Fatalf("create profile failed: %v, text: %s", err, resultText(t, resCreate))
	}

	// C. Read again and verify profile exists with English and Spanish
	reqGetVerify := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service": "bazarr",
			"method":  "GET",
			"path":    "/system/languages/profiles",
		},
	}}
	resGetVerify, err := handleCallAPI(ctx, reqGetVerify, registry, nil, 50, true)
	if err != nil || resGetVerify.IsError {
		t.Fatalf("verification GET failed: %v, text: %s", err, resultText(t, resGetVerify))
	}
	verifyText := resultText(t, resGetVerify)
	if !strings.Contains(verifyText, "Accessible EN+ES") {
		t.Fatalf("expected profile 'Accessible EN+ES' in Bazarr, got: %s", verifyText)
	}

	// D. Assign profile to a test movie via POST /movies with query params
	reqAssign := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service": "bazarr",
			"method":  "POST",
			"path":    "/movies",
			"query": map[string]any{
				"radarrid":  movieID,
				"profileid": "1",
			},
			"include_metadata": true,
		},
	}}
	resAssign, err := handleCallAPI(ctx, reqAssign, registry, nil, 50, true)
	if err != nil || resAssign.IsError {
		t.Fatalf("assign movie profile failed: %v, text: %s", err, resultText(t, resAssign))
	}

	// E. Verify movie has profileId: 1
	reqGetMovie := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "call_api",
		Arguments: map[string]any{
			"service": "bazarr",
			"method":  "GET",
			"path":    "/movies",
			"query": map[string]any{
				"radarrid[]": movieID,
			},
		},
	}}
	resGetMovie, err := handleCallAPI(ctx, reqGetMovie, registry, nil, 50, true)
	if err != nil || resGetMovie.IsError {
		t.Fatalf("get movie failed: %v, text: %s", err, resultText(t, resGetMovie))
	}
	movieText := resultText(t, resGetMovie)
	if !strings.Contains(movieText, `"profileId": 1`) && !strings.Contains(movieText, `"profileId":1`) {
		t.Errorf("expected movie to have profileId 1, got: %s", movieText)
	}
}
