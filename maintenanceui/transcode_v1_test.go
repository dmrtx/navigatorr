package maintenanceui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/tools"
	"github.com/jakenesler/navigatorr/transcodeapi"
	"github.com/mark3labs/mcp-go/mcp"
)

const testTranscodeKey = "synthetic-transcode-key-0123456789"

// transcodeUI is the test server with a transcode API key configured.
func transcodeUI(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	base, _ := testUI(t)
	path := filepath.Join(t.TempDir(), "transcode-token")
	if err := os.WriteFile(path, []byte(testTranscodeKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := *base.cfg
	cfg.Web.TranscodeTokenFile = path
	s, err := New(&cfg, base.registry, base.engine, base.mcp)
	if err != nil {
		t.Fatal(err)
	}
	s.SetNodes(base.nodes)
	return s, s.Handler()
}

// apiKeyHeader is the header the v1 key travels in (Bearer works too).
const apiKeyHeader = "X-Api-Key"

func keyHdr() map[string]string { return map[string]string{apiKeyHeader: testTranscodeKey} }

func get(t *testing.T, h http.Handler, path string, hd map[string]string) (int, map[string]any) {
	t.Helper()
	w := v1(h, "GET", path, "", hd)
	var out map[string]any
	if w.Body.Len() > 0 && json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("GET %s: not a JSON object: %q", path, w.Body.String())
	}
	return w.Code, out
}

func TestTranscodeV1Auth(t *testing.T) {
	_, h := transcodeUI(t)
	if w := v1(h, "GET", transcodeapi.SpecPath, "", nil); w.Code != 200 || !json.Valid(w.Body.Bytes()) {
		t.Fatal("the spec is public", w.Code)
	}
	for _, hd := range []map[string]string{nil, {apiKeyHeader: "wrong"}, {"Authorization": "Bearer wrong"}, {apiKeyHeader: testBuildsToken}} {
		code, e := get(t, h, "/transcode/v1/workers", hd)
		if code != 401 || e["code"] != "unauthorized" || e["hint"] == "" {
			t.Fatal(code, e)
		}
	}
	// The key, as X-Api-Key and as Bearer.
	if code, _ := get(t, h, "/transcode/v1/workers", keyHdr()); code != 200 {
		t.Fatal(code)
	}
	if code, _ := get(t, h, "/transcode/v1/workers", map[string]string{"Authorization": "Bearer " + testTranscodeKey}); code != 200 {
		t.Fatal(code)
	}
	// The UI's own sign-in (bearer web token) keeps reading too.
	if code, _ := get(t, h, "/transcode/v1/workers", map[string]string{"Authorization": "Bearer " + testToken}); code != 200 {
		t.Fatal("UI auth GET", code)
	}
	// The key opens nothing else.
	for _, p := range []string{"/api/maintenance/bootstrap", "/nodes/v1/builds", "/api/maintenance/builds"} {
		if w := v1(h, "GET", p, "", keyHdr()); w.Code == 200 {
			t.Fatal("the transcode key must not open", p)
		}
		if w := v1(h, "GET", p, "", map[string]string{"Authorization": "Bearer " + testTranscodeKey}); w.Code == 200 {
			t.Fatal("the transcode key must not open", p)
		}
	}
}

// With no key configured only the UI auth path works.
func TestTranscodeV1WithoutAKeyOnlyTheUIAuthWorks(t *testing.T) {
	_, h := testUI(t)
	if code, _ := get(t, h, "/transcode/v1/workers", keyHdr()); code != 401 {
		t.Fatal(code)
	}
	if code, _ := get(t, h, "/transcode/v1/workers", map[string]string{"Authorization": "Bearer " + testToken}); code != 200 {
		t.Fatal(code)
	}
	if w := v1(h, "GET", transcodeapi.SpecPath, "", nil); w.Code != 200 {
		t.Fatal("spec", w.Code)
	}
}

func TestTranscodeKeyIsValidatedAtStartup(t *testing.T) {
	base, _ := testUI(t)
	cfg := *base.cfg
	cfg.Web.TranscodeTokenFile = filepath.Join(t.TempDir(), "missing")
	if _, err := New(&cfg, base.registry, base.engine, base.mcp); err == nil {
		t.Fatal("a missing token file must fail")
	}
	short := filepath.Join(t.TempDir(), "short")
	_ = os.WriteFile(short, []byte("short\n"), 0o600)
	cfg.Web.TranscodeTokenFile = short
	if _, err := New(&cfg, base.registry, base.engine, base.mcp); err == nil {
		t.Fatal("a short token must fail")
	}
}

// A method a path does not serve is a 405 naming the methods it does (with or
// without the key), and a write with no credential at all is a 401 that
// changes nothing.
func TestTranscodeV1RejectsWhatItDoesNotServe(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "job1", "transcode_media", "running", map[string]any{"path": "/media/Example Show S01E01.mkv"}, nil, nil)
	for p, want := range map[string]string{
		"/transcode/v1/actions/job1":        "GET, HEAD",
		"/transcode/v1/actions/job1/cancel": "POST",
		"/transcode/v1/pauses/video-worker": "GET, HEAD, PUT, DELETE",
		"/transcode/v1/drain":               "GET, HEAD, PUT",
		"/transcode/v1/recipes/x":           "GET, HEAD, PUT, DELETE",
	} {
		for _, m := range []string{"PATCH", "TRACE"} {
			w := v1(h, m, p, `{}`, keyHdr())
			var e map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			if w.Code != 405 || e["code"] != "method_not_allowed" || w.Header().Get("Allow") != want {
				t.Fatalf("%s %s: %d Allow %q %s", m, p, w.Code, w.Header().Get("Allow"), w.Body.String())
			}
		}
	}
	for _, p := range []string{"/transcode/v1/actions/job1/cancel", "/transcode/v1/actions", "/transcode/v1/pauses/video-worker", "/transcode/v1/recipes/x"} {
		for _, m := range []string{"POST", "PUT", "DELETE"} {
			if w := v1(h, m, p, `{}`, nil); w.Code != 401 && w.Code != 405 {
				t.Fatal(m, p, w.Code)
			}
		}
	}
	if w := v1(h, "POST", "/transcode/v1/actions/job1/cancel", `{}`, nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	inst, _ := s.engine.Deps().Store.GetActionInstance("job1")
	if inst == nil || inst.Status != "running" {
		t.Fatal("a refused write changed the action", inst)
	}
	// An unknown path under the base is a JSON 404.
	code, e := get(t, h, "/transcode/v1/nope", keyHdr())
	if code != 404 || e["code"] != "not_found" {
		t.Fatal(code, e)
	}
}

// Every route is mounted (never the catch-all) and, with the placeholders
// filled with ids that do not exist, answers a JSON object.
func TestTranscodeV1EveryRouteIsMounted(t *testing.T) {
	_, h := transcodeUI(t)
	rep := strings.NewReplacer("{name}", "nodeZ", "{id}", "nope", "{section}", "inputs", "{block_id}", "b1", "{artifact}", "transcript", "{entry}", "e1")
	for _, r := range transcodeapi.Routes() {
		p := rep.Replace(r.Path)
		hd := keyHdr()
		if r.Method != "GET" {
			hd["Content-Type"] = "application/json"
		}
		w := v1(h, r.Method, p, map[bool]string{true: "", false: `{}`}[r.Method == "GET" || r.Method == "DELETE"], hd)
		if w.Code == 401 || w.Code == 405 || w.Code >= 500 && w.Code != 503 {
			t.Errorf("%s: %d %s", p, w.Code, w.Body.String())
		}
		var raw any
		if json.Unmarshal(w.Body.Bytes(), &raw) != nil {
			t.Errorf("%s: not JSON: %q", p, w.Body.String())
		}
		var e struct{ Code string }
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code == 404 && e.Code == "not_found" && !strings.Contains(r.Path, "{section}") && !strings.Contains(r.Path, "podcast") {
			t.Errorf("%s fell to the catch-all", p)
		}
	}
}

func TestTranscodeV1UnknownIdsAre404(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "other1", "sonarr_thing", "running", nil, nil, nil)
	seedOperation(t, s.engine.Deps().Store, "file1", "transcode_media", "running", nil, nil, nil)
	for p, code := range map[string]string{
		"/transcode/v1/workers/nodeZ":              "unknown_worker",
		"/transcode/v1/workers/nodeZ/slots":        "unknown_worker",
		"/transcode/v1/pauses/nodeZ":               "unknown_worker",
		"/transcode/v1/actions/nope":               "action_not_found",
		"/transcode/v1/actions/other1":             "action_not_found",
		"/transcode/v1/actions/nope/detail/inputs": "action_not_found",
		"/transcode/v1/batches/nope/items":         "batch_not_found",
		"/transcode/v1/batches/file1/items":        "batch_not_found",
		"/transcode/v1/batches/nope/settings":      "batch_not_found",
	} {
		c, e := get(t, h, p, keyHdr())
		if c != 404 || e["code"] != code || e["error"] == "" {
			t.Errorf("%s: %d %v, want 404 %s", p, c, e, code)
		}
	}
}

func TestTranscodeV1WorkersAndState(t *testing.T) {
	_, h := transcodeUI(t)
	code, list := get(t, h, "/transcode/v1/workers", keyHdr())
	rows, _ := list["nodes"].([]any)
	if code != 200 || len(rows) == 0 {
		t.Fatal(code, list)
	}
	name, _ := rows[0].(map[string]any)["worker"].(string)
	if name == "" {
		t.Fatal(rows[0])
	}
	code, one := get(t, h, "/transcode/v1/workers/"+name, keyHdr())
	if code != 200 || one["worker"] != name || one["state"] == nil {
		t.Fatal(code, one)
	}
	for _, p := range []string{"/transcode/v1/state", "/transcode/v1/drain", "/transcode/v1/pauses", "/transcode/v1/pauses/" + name, "/transcode/v1/catalog", "/transcode/v1/activity"} {
		if c, _ := get(t, h, p, keyHdr()); c != 200 {
			t.Errorf("%s: %d", p, c)
		}
	}
	_, cat := get(t, h, "/transcode/v1/catalog", keyHdr())
	acts, _ := cat["actions"].([]any)
	if len(acts) == 0 {
		t.Fatal("catalog is empty")
	}
	for _, a := range acts {
		if n, _ := a.(map[string]any)["name"].(string); n == "" || (!strings.HasPrefix(n, "transcode") && !strings.Contains(n, "podcast") && n != "promote_transcode_candidate" && n != "benchmark_transcode") {
			t.Errorf("not a transcode workflow in the catalog: %q", n)
		}
	}
}

func TestTranscodeV1ActionsPagination(t *testing.T) {
	s, h := transcodeUI(t)
	st := s.engine.Deps().Store
	for i := 0; i < 5; i++ {
		seedOperation(t, st, fmt.Sprintf("job%d", i), "transcode_media", "completed", map[string]any{"path": fmt.Sprintf("/media/Show S01E0%d.mkv", i)}, nil, nil)
	}
	seedOperation(t, st, "foreign", "sonarr_thing", "completed", nil, nil, nil)

	seen := map[string]bool{}
	offset := 0
	for pages := 0; pages < 20; pages++ {
		code, page := get(t, h, fmt.Sprintf("/transcode/v1/actions?status=completed&limit=2&offset=%d", offset), keyHdr())
		items, _ := page["items"].([]any)
		if code != 200 || page["has_more"] == nil || page["next_offset"] == nil || len(items) > 2 {
			t.Fatal(code, page)
		}
		for _, it := range items {
			id, _ := it.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("%s repeated across pages", id)
			}
			seen[id] = true
		}
		next := int(page["next_offset"].(float64))
		if next <= offset && page["has_more"] == true {
			t.Fatalf("next_offset %d does not advance past %d", next, offset)
		}
		offset = next
		if page["has_more"] != true {
			break
		}
	}
	if len(seen) != 5 || seen["foreign"] {
		t.Fatalf("a full walk must find the 5 transcode actions and nothing else: %v", seen)
	}
	for _, q := range []string{"limit=0", "limit=101", "limit=x", "offset=-1", "status=bogus"} {
		if c, e := get(t, h, "/transcode/v1/actions?"+q, keyHdr()); c != 400 || e["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", q, c, e)
		}
	}
	code, one := get(t, h, "/transcode/v1/actions/job1", keyHdr())
	if code != 200 || one["action"] == nil {
		t.Fatal(code, one)
	}
	// The audit log and the recipes are off in this server: a JSON 503, not a 500.
	for _, p := range []string{"/transcode/v1/history?limit=5", "/transcode/v1/recipes/nope", "/transcode/v1/recipe-bundle"} {
		if c, e := get(t, h, p, keyHdr()); c != 503 || e["code"] != "unavailable" {
			t.Errorf("%s: %d %v", p, c, e)
		}
	}
}

func TestTranscodeV1BatchItemsPaginateLikeTheLegacyRoute(t *testing.T) {
	s, h := transcodeUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "completed", nil, nil, nil)
	for i := 0; i < 30; i++ {
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: fmt.Sprintf("file-%02d", i), FilePath: fmt.Sprintf("/media/%02d.mkv", i), Status: "failed", Decision: "transcode"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		q     string
		count int
		more  bool
	}{{"", 25, true}, {"?offset=25", 5, false}, {"?offset=30", 0, false}, {"?limit=10&offset=10", 10, true}} {
		code, page := get(t, h, "/transcode/v1/batches/batch/items"+tc.q, keyHdr())
		items, _ := page["items"].([]any)
		if code != 200 || len(items) != tc.count || page["total"].(float64) != 30 || page["has_more"] != tc.more {
			t.Fatal(tc.q, code, page)
		}
	}
	if c, _ := get(t, h, "/transcode/v1/batches/batch/items?limit=101", keyHdr()); c != 400 {
		t.Fatal("unbounded page accepted")
	}
	// The legacy route and the v1 route answer the same page.
	legacy := request(h, "GET", "/api/maintenance/batch-items?id=batch&limit=10&offset=5", "", true)
	v := v1(h, "GET", "/transcode/v1/batches/batch/items?limit=10&offset=5", "", keyHdr())
	if legacy.Code != 200 || legacy.Body.String() != v.Body.String() {
		t.Fatalf("legacy %d %s\nv1 %d %s", legacy.Code, legacy.Body.String(), v.Code, v.Body.String())
	}
	for _, p := range []string{"settings", "candidates", "promotion-plan", "preview"} {
		c, e := get(t, h, "/transcode/v1/batches/batch/"+p, keyHdr())
		if c == 401 || c == 405 || c >= 500 {
			t.Errorf("%s: %d %v", p, c, e)
		}
	}
}

func TestTranscodeV1ReviewQueueAndLists(t *testing.T) {
	_, h := transcodeUI(t)
	for _, p := range []string{
		"/transcode/v1/review-queue", "/transcode/v1/review-queue?state=all&sort=size&limit=5&offset=0",
		"/transcode/v1/backups", "/transcode/v1/backups/candidates?offset=0", "/transcode/v1/recipes", "/transcode/v1/recipe-bundle", "/transcode/v1/podcasts",
	} {
		w := v1(h, "GET", p, "", keyHdr())
		if w.Code != 200 && w.Code != 503 || !json.Valid(w.Body.Bytes()) {
			t.Errorf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	for _, q := range []string{"state=bogus", "sort=bogus", "limit=0", "offset=-1"} {
		if c, e := get(t, h, "/transcode/v1/review-queue?"+q, keyHdr()); c != 400 || e["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", q, c, e)
		}
	}
}

// The section and the artifact are validated before any tool is called.
func TestTranscodeV1DetailParametersAreValidated(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "job1", "transcode_media", "running", map[string]any{"path": "/media/Show S01E01.mkv"}, nil, nil)
	for _, p := range []string{"/transcode/v1/actions/job1/detail/bogus", "/transcode/v1/actions/job1/podcast/artifacts/bogus"} {
		if c, e := get(t, h, p, keyHdr()); c != 400 || e["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", p, c, e)
		}
	}
	if c, _ := get(t, h, "/transcode/v1/actions/job1/detail/inputs", keyHdr()); c != 200 {
		t.Fatal("inputs", c)
	}
}

// A write made with the web sign-in goes through the console's own write
// checks (see the wrapper in server.go): the X-Navigatorr-Request header, and
// an Origin that names this host. The API key is no ambient credential and
// needs neither.
func TestTranscodeV1UIAuthWriteNeedsTheConsoleWriteChecks(t *testing.T) {
	_, h := transcodeUI(t)
	const drain = "/transcode/v1/drain"
	bearer := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range bearer {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	drainOn := func() bool {
		_, e := get(t, h, drain, bearer)
		b, _ := json.Marshal(e)
		return strings.Contains(string(b), `"enabled":true`) || strings.Contains(string(b), `"drain":true`)
	}
	// No header: 401. A foreign Origin with the header: 403. Neither writes.
	if w := v1(h, "PUT", drain, `{"enabled":true}`, bearer); w.Code != 401 || drainOn() {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := v1(h, "PUT", drain, `{"enabled":true}`, with(map[string]string{"X-Navigatorr-Request": "1", "Origin": "https://elsewhere.example"})); w.Code != 403 || drainOn() {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := v1(h, "PUT", drain, `{"enabled":false}`, bearer); w.Code != 401 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := v1(h, "PUT", drain, `{"enabled":false}`, with(map[string]string{"X-Navigatorr-Request": "1", "Origin": "https://elsewhere.example"})); w.Code != 403 || !strings.Contains(w.Body.String(), `"forbidden"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// Header and a same-host Origin (httptest requests are for example.com): through.
	if w := v1(h, "PUT", drain, `{"enabled":false}`, with(map[string]string{"X-Navigatorr-Request": "1", "Origin": "http://example.com"})); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := v1(h, "PUT", drain, `{"enabled":false}`, with(map[string]string{"X-Navigatorr-Request": "1"})); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// The key needs neither header nor Origin, even with a foreign Origin.
	hd := keyHdr()
	hd["Content-Type"] = "application/json"
	hd["Origin"] = "https://elsewhere.example"
	if w := v1(h, "PUT", drain, `{"enabled":false}`, hd); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Reads need only the credential.
	if c, _ := get(t, h, drain, bearer); c != 200 {
		t.Fatal(c)
	}
}

// fakeTool replaces an MCP tool of the test server with fn.
func fakeTool(s *Server, name string, fn func(args map[string]any) (*mcp.CallToolResult, error)) {
	s.mcp.AddTool(mcp.NewTool(name), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return fn(req.GetArguments())
	})
}

// The history shows only what the engine logged for transcode actions: not the
// entries of other services, nor those of non-transcode actions, whatever the
// filters say.
func TestTranscodeV1HistoryIsLimitedToTranscodeActions(t *testing.T) {
	s, h := transcodeUI(t)
	st := s.engine.Deps().Store
	tools.RegisterDiagnostics(s.mcp, s.cfg, s.registry, nil, nil, nil, nil, st, s.engine.Deps().Workers)
	seedOperation(t, st, "job1", "transcode_media", "completed", nil, nil, nil)
	seedOperation(t, st, "pod1", "clean_podcast_ads", "completed", nil, nil, nil)
	seedOperation(t, st, "other1", "sonarr_thing", "completed", nil, nil, nil)
	log := func(action, service, media, ident string) {
		t.Helper()
		if err := st.LogActionEnriched(action, service, media, `{}`, "ok", "", ident, 1); err != nil {
			t.Fatal(err)
		}
	}
	log("action_completed", "", "job1", "job1")
	log("action_failed", "", "pod1", "pod1")
	log("action_completed", "", "other1", "other1")
	log("scan_library", "radarr", "Show A", "")
	log("safe_replace", "sonarr", "Show A", "")
	log("action_completed", "", "gone1", "gone1") // an instance that no longer exists

	ids := func(q string) (int, []string, map[string]any) {
		code, body := get(t, h, "/transcode/v1/history"+q, keyHdr())
		var out []string
		recs, _ := body["records"].([]any)
		for _, r := range recs {
			rec := r.(map[string]any)
			out = append(out, rec["action"].(string)+":"+rec["identifiers"].(string))
		}
		return code, out, body
	}
	for q, want := range map[string][]string{
		"":                                    {"action_failed:pod1", "action_completed:job1"},
		"?service=radarr":                     nil,
		"?media=Show":                         nil,
		"?media=job1":                         {"action_completed:job1"},
		"?action=transcode_media":             {"action_completed:job1"},
		"?action=clean_podcast_ads":           {"action_failed:pod1"},
		"?action=action_completed":            {"action_completed:job1"},
		"?action=action_failed":               {"action_failed:pod1"},
		"?action=promote_transcode_candidate": nil,
		"?limit=1":                            {"action_failed:pod1"},
	} {
		code, got, body := ids(q)
		if code != 200 || strings.Join(got, ",") != strings.Join(want, ",") || body["count"] != float64(len(want)) {
			t.Errorf("history%s: %d %v (want %v) %v", q, code, got, want, body)
		}
	}
	// Any other action (a non-transcode workflow or another service's entry) is refused.
	for _, q := range []string{"?action=sonarr_thing", "?action=scan_library", "?action=safe_replace", "?action=api_call", "?limit=0", "?limit=101", "?limit=x"} {
		if c, e := get(t, h, "/transcode/v1/history"+q, keyHdr()); c != 400 || e["code"] != "invalid_request" {
			t.Errorf("history%s: %d %v", q, c, e)
		}
	}
}

// HEAD on the block route must not read (reading records coverage).
func TestTranscodeV1HeadDoesNotRecordPodcastCoverage(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "pod1", "clean_podcast_ads", "waiting_decision", nil, nil, nil)
	calls := 0
	fakeTool(s, "podcast_block", func(map[string]any) (*mcp.CallToolResult, error) {
		calls++
		return mcp.NewToolResultText(`{"has_more":false}`), nil
	})
	w := v1(h, "HEAD", "/transcode/v1/actions/pod1/podcast/blocks/b1", "", keyHdr())
	if w.Code != 405 || w.Header().Get("Allow") != "GET" || calls != 0 {
		t.Fatalf("HEAD: %d allow=%q calls=%d", w.Code, w.Header().Get("Allow"), calls)
	}
	if c, _ := get(t, h, "/transcode/v1/actions/pod1/podcast/blocks/b1", keyHdr()); c != 200 || calls != 1 {
		t.Fatalf("GET: %d calls=%d", c, calls)
	}
	// Other reads keep answering HEAD.
	if w := v1(h, "HEAD", "/transcode/v1/workers", "", keyHdr()); w.Code != 200 {
		t.Fatal("HEAD workers", w.Code)
	}
}

func TestWriteToolErrorClassification(t *testing.T) {
	for msg, want := range map[string]int{
		`action instance "job1" not found`:                               404,
		`key "x" not found in section inputs (available keys: [])`:       404,
		`step index 3 not found for action job1 (total logged steps: 2)`: 404,
		`podcast action not found`:                                       404,
		`unknown transcode profile "nope"`:                               404,
		`unknown block`:                                                  404,
		`no such recipe`:                                                 404,
		`open /x: no such file or directory`:                             400,
		`unknown_only must be a boolean`:                                 400,
		`unknown mode "x"`:                                               400,
		`unknown podcast worker status`:                                  400,
		`invalid block/offset`:                                           400,
		`name is required`:                                               400,
		`transcode recipe manager is not initialized`:                    400,
	} {
		w := httptest.NewRecorder()
		writeToolError(w, msg, v1Err{})
		var e map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		wantCode := "invalid_request"
		if want == 404 {
			wantCode = "not_found"
		}
		if w.Code != want || e["code"] != wantCode {
			t.Errorf("%q: %d %v, want %d %s", msg, w.Code, e, want, wantCode)
		}
	}
	w := httptest.NewRecorder()
	writeToolError(w, "recipe not found", v1Err{notFound: "recipe_not_found", hint: "h"})
	if w.Code != 404 || !strings.Contains(w.Body.String(), `"recipe_not_found"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// A Go error of a tool handler is the server's fault (500, cause not sent); a
// refusal the tool reports as a result stays the caller's (400/404).
func TestTranscodeV1ToolGoErrorIs500(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "job1", "transcode_media", "running", nil, nil, nil)
	fakeTool(s, "action_status", func(map[string]any) (*mcp.CallToolResult, error) {
		return nil, errors.New("database exploded at /secret/path")
	})
	w := v1(h, "GET", "/transcode/v1/actions/job1", "", keyHdr())
	if w.Code != 500 || !strings.Contains(w.Body.String(), `"internal"`) || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Code, w.Body.String())
	}
	fakeTool(s, "action_status", func(map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("verbose must be shorter"), nil
	})
	if c, e := get(t, h, "/transcode/v1/actions/job1", keyHdr()); c != 400 || e["code"] != "invalid_request" {
		t.Fatal(c, e)
	}
}

// A not-found inside an existing action is not "action_not_found".
func TestTranscodeV1NotFoundInsideAnActionIsGeneric(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "job1", "transcode_media", "running", map[string]any{"path": "/media/a.mkv"}, nil, nil)
	c, e := get(t, h, "/transcode/v1/actions/job1/detail/inputs?key=nope", keyHdr())
	if c != 404 || e["code"] != "not_found" || e["hint"] == "" || strings.Contains(e["hint"].(string), "lists the actions") {
		t.Fatal(c, e)
	}
	c, e = get(t, h, "/transcode/v1/actions/job1/detail/steps?step_index=9", keyHdr())
	if c != 404 || e["code"] != "not_found" {
		t.Fatal(c, e)
	}
	// The action itself missing keeps its code.
	if c, e = get(t, h, "/transcode/v1/actions/nope/detail/inputs", keyHdr()); c != 404 || e["code"] != "action_not_found" {
		t.Fatal(c, e)
	}
}

// The "response too large" note of a tool is never a 200 page.
func TestTranscodeV1TooLargeAnswersAnError(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "job1", "transcode_media", "running", nil, nil, nil)
	note := `{"error":"response_too_large","message":"too big","size_bytes":99999,"limit_bytes":65536}`
	for _, tool := range []string{"action_list", "action_status", "action_history"} {
		fakeTool(s, tool, func(map[string]any) (*mcp.CallToolResult, error) { return mcp.NewToolResultText(note), nil })
	}
	for p, hint := range map[string]string{
		"/transcode/v1/actions/job1": "narrow",
		"/transcode/v1/history":      "narrow",
	} {
		c, e := get(t, h, p, keyHdr())
		if c != 502 || e["code"] != "response_too_large" || !strings.Contains(e["hint"].(string), hint) || e["items"] != nil {
			t.Errorf("%s: %d %v", p, c, e)
		}
	}
	// History that is not a page ({}, null, a sentence) is an error, never an
	// empty 200.
	for _, out := range []string{`{}`, `null`, `nothing`} {
		fakeTool(s, "action_history", func(map[string]any) (*mcp.CallToolResult, error) { return mcp.NewToolResultText(out), nil })
		if c, e := get(t, h, "/transcode/v1/history", keyHdr()); c != 502 || e["code"] != "internal" {
			t.Errorf("history answering %s: %d %v", out, c, e)
		}
	}
}

// Numbers and booleans are validated here, not left to the tools, which fall
// back to their defaults on a value they cannot read.
func TestTranscodeV1NumericAndBooleanParameters(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "job1", "transcode_media", "running", nil, nil, nil)
	var last map[string]any
	for _, tool := range []string{"action_status", "action_detail", "podcast_blocks", "podcast_block", "podcast_artifact", "recipe_list", "recipe_history", "transcode_backups", "podcast_ad_library"} {
		fakeTool(s, tool, func(a map[string]any) (*mcp.CallToolResult, error) {
			last = a
			return mcp.NewToolResultText(`{"ok":true}`), nil
		})
	}
	bad := []string{
		"/transcode/v1/actions/job1?verbose=maybe",
		"/transcode/v1/actions/job1/detail/steps?step_limit=0",
		"/transcode/v1/actions/job1/detail/steps?step_limit=51",
		"/transcode/v1/actions/job1/detail/steps?step_index=-1",
		"/transcode/v1/actions/job1/detail/steps?step_offset=abc",
		"/transcode/v1/actions/job1/detail/inputs?chunk=-1",
		"/transcode/v1/actions/job1/podcast/blocks?offset=-1",
		"/transcode/v1/actions/job1/podcast/blocks/b1?offset=1.5",
		"/transcode/v1/actions/job1/podcast/blocks/b1?unknown_only=yes",
		"/transcode/v1/actions/job1/podcast/artifacts/cuts?offset=x",
		"/transcode/v1/recipes?limit=0",
		"/transcode/v1/recipes?limit=101",
		"/transcode/v1/recipes?limit=abc",
		"/transcode/v1/recipes?offset=-1",
		"/transcode/v1/recipes?include_selection_details=maybe",
		"/transcode/v1/recipes/x/history?limit=51",
		"/transcode/v1/backups?offset=-1",
		"/transcode/v1/backups/candidates?offset=z",
		"/transcode/v1/podcasts/none/ad-library?offset=-1",
	}
	s.cfg.Podcasts.Enabled = true
	s.cfg.Podcasts.Podcasts = map[string]config.PodcastSettings{"pod": {Enabled: true}}
	bad = append(bad, "/transcode/v1/podcasts/pod/ad-library?offset=-1")
	for _, p := range bad {
		last = nil
		c, e := get(t, h, p, keyHdr())
		if p != "/transcode/v1/podcasts/none/ad-library?offset=-1" && (c != 400 || e["code"] != "invalid_request" || e["hint"] == "" || last != nil) {
			t.Errorf("%s: %d %v (tool called: %v)", p, c, e, last)
		}
	}
	// Valid values reach the tool typed.
	for p, want := range map[string]map[string]any{
		"/transcode/v1/actions/job1?verbose=true":                                 {"verbose": true},
		"/transcode/v1/actions/job1?verbose=0":                                    {"verbose": false},
		"/transcode/v1/actions/job1/detail/steps?step_limit=50&step_offset=2":     {"step_limit": 50, "step_offset": 2},
		"/transcode/v1/actions/job1/podcast/blocks/b1?offset=3&unknown_only=1":    {"offset": 3, "unknown_only": true, "block_id": "b1"},
		"/transcode/v1/recipes?limit=100&offset=0&include_selection_details=true": {"limit": 100, "offset": 0, "include_selection_details": true},
	} {
		last = nil
		if c, e := get(t, h, p, keyHdr()); c != 200 {
			t.Fatalf("%s: %d %v", p, c, e)
		}
		for k, v := range want {
			if last[k] != v {
				t.Errorf("%s: %s = %#v (%T), want %#v", p, k, last[k], last[k], v)
			}
		}
	}
}

// The podcast routes answer like every other switched-off feature: 503.
func TestTranscodeV1PodcastsDisabledIs503(t *testing.T) {
	s, h := transcodeUI(t)
	s.cfg.Podcasts.Enabled = false
	s.cfg.Podcasts.Podcasts = map[string]config.PodcastSettings{"pod": {Enabled: true}}
	for _, p := range []string{"/transcode/v1/podcasts/pod/ad-library", "/transcode/v1/podcasts/pod/local-library", "/transcode/v1/podcasts/nope/ad-library"} {
		if c, e := get(t, h, p, keyHdr()); c != 503 || e["code"] != "unavailable" {
			t.Errorf("%s: %d %v", p, c, e)
		}
	}
	// Enabled: an unknown or disabled profile is a 404.
	s.cfg.Podcasts.Enabled = true
	for _, p := range []string{"/transcode/v1/podcasts/nope/ad-library", "/transcode/v1/podcasts/nope/local-library"} {
		if c, e := get(t, h, p, keyHdr()); c != 404 || e["code"] != "unknown_podcast" {
			t.Errorf("%s: %d %v", p, c, e)
		}
	}
}

// When the tool says the newest 100 entries do not fit, fewer are read (the
// underlying limit is halved down to the client's) so a subset is reachable;
// only when even the client's limit does not fit is it a 502, whose hint names
// filters that really narrow the read.
func TestTranscodeV1HistoryRetriesSmallerWhenTooLarge(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "job1", "transcode_media", "completed", nil, nil, nil)
	var seen []int
	maxFit := 10
	fakeTool(s, "action_history", func(a map[string]any) (*mcp.CallToolResult, error) {
		n, _ := a["limit"].(int)
		seen = append(seen, n)
		if n > maxFit {
			return mcp.NewToolResultText(`{"error":"response_too_large","size_bytes":99999,"limit_bytes":65536}`), nil
		}
		return mcp.NewToolResultText(`{"count":2,"records":[{"action":"action_completed","identifiers":"job1"},{"action":"action_completed","identifiers":"job1"}]}`), nil
	})
	c, body := get(t, h, "/transcode/v1/history?limit=1", keyHdr())
	if c != 200 || body["count"] != float64(1) || fmt.Sprint(seen) != "[100 50 25 12 6]" {
		t.Fatal(c, body, seen)
	}
	// Nothing fits, not even the client's limit: 502 with an accurate hint.
	maxFit, seen = 0, nil
	c, e := get(t, h, "/transcode/v1/history?limit=3", keyHdr())
	hint, _ := e["hint"].(string)
	if c != 502 || e["code"] != "response_too_large" || fmt.Sprint(seen) != "[100 50 25 12 6 3]" || !strings.Contains(hint, "action_completed") || !strings.Contains(hint, "after the read") {
		t.Fatal(c, e, seen)
	}
	// An event name reaches the tool; a workflow name is applied afterwards.
	maxFit = 100
	var gotAction any
	fakeTool(s, "action_history", func(a map[string]any) (*mcp.CallToolResult, error) {
		gotAction = a["action"]
		return mcp.NewToolResultText(`{"count":0,"records":null}`), nil
	})
	if c, _ := get(t, h, "/transcode/v1/history?action=action_failed", keyHdr()); c != 200 || gotAction != "action_failed" {
		t.Fatal(c, gotAction)
	}
}

// With nothing logged the real tool answers an empty page: 200 with [].
func TestTranscodeV1HistoryEmptyIsAnEmptyList(t *testing.T) {
	s, h := transcodeUI(t)
	tools.RegisterDiagnostics(s.mcp, s.cfg, s.registry, nil, nil, nil, nil, s.engine.Deps().Store, s.engine.Deps().Workers)
	w := v1(h, "GET", "/transcode/v1/history?media=no-such-title", "", keyHdr())
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"records":[]`) || !strings.Contains(w.Body.String(), `"count":0`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// The detail of a legacy handler's 500 goes to the log, not to the caller.
func TestTranscodeV1LegacyServerErrorDoesNotLeak(t *testing.T) {
	s, _ := transcodeUI(t)
	var logged strings.Builder
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	for status, wantCode := range map[int]string{500: "internal", 503: "unavailable"} {
		logged.Reset()
		fn := s.v1Legacy(func(w http.ResponseWriter, _ *http.Request) { fail(w, status, "read /secret/path: disk on fire") }, nil, v1Err{})
		w := httptest.NewRecorder()
		fn(w, httptest.NewRequest("GET", "/x", nil))
		var e map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != status || e["code"] != wantCode || strings.Contains(w.Body.String(), "secret") || !strings.Contains(logged.String(), "disk on fire") {
			t.Errorf("%d: %d %s (log %q)", status, w.Code, w.Body.String(), logged.String())
		}
	}
	// A 4xx of the handler is the caller's and keeps its message.
	fn := s.v1Legacy(func(w http.ResponseWriter, _ *http.Request) { fail(w, 400, "invalid limit") }, nil, v1Err{})
	w := httptest.NewRecorder()
	fn(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid limit") || !strings.Contains(w.Body.String(), `"hint"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// The podcast block route refuses HEAD, so its 405 says Allow: GET; the others
// say GET, HEAD, plus the writes they serve. A write to the spec is a 405 too
// (public or not, it only answers GET and HEAD).
func TestTranscodeV1AllowHeaderAndSpecWrites(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "pod1", "clean_podcast_ads", "waiting_decision", nil, nil, nil)
	for p, want := range map[string]string{
		"/transcode/v1/actions/pod1/podcast/blocks/b1": "GET",
		"/transcode/v1/actions/pod1/podcast/blocks":    "GET, HEAD",
		"/transcode/v1/workers":                        "GET, HEAD",
		"/transcode/v1/actions":                        "GET, HEAD, POST",
		"/transcode/v1/batches/pod1/goal":              "GET, HEAD, POST",
		"/transcode/v1/actions/pod1/podcast/review":    "POST",
	} {
		if w := v1(h, "PATCH", p, "{}", keyHdr()); w.Code != 405 || w.Header().Get("Allow") != want {
			t.Errorf("PATCH %s: %d Allow %q, want %q", p, w.Code, w.Header().Get("Allow"), want)
		}
	}
	for _, hd := range []map[string]string{nil, keyHdr()} {
		w := v1(h, "POST", transcodeapi.SpecPath, "{}", hd)
		var e map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != 405 || e["code"] != "method_not_allowed" || w.Header().Get("Allow") != "GET, HEAD" || strings.Contains(w.Body.String(), `"paths"`) {
			t.Errorf("POST spec: %d %s", w.Code, w.Body.String())
		}
	}
	if w := v1(h, "GET", transcodeapi.SpecPath, "", nil); w.Code != 200 {
		t.Fatal("the spec stays public", w.Code)
	}
}

// Paging of the two legacy-backed lists is validated, never defaulted.
func TestTranscodeV1LegacyPagingIsValidated(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "batch", "transcode_batch", "completed", nil, nil, nil)
	for _, p := range []string{
		"/transcode/v1/batches/batch/items?limit=abc", "/transcode/v1/batches/batch/items?offset=-1", "/transcode/v1/batches/batch/items?limit=0",
		"/transcode/v1/review-queue?limit=abc", "/transcode/v1/review-queue?offset=-1", "/transcode/v1/review-queue?limit=0", "/transcode/v1/review-queue?limit=1001",
	} {
		c, e := get(t, h, p, keyHdr())
		if c != 400 || e["code"] != "invalid_request" || e["hint"] == "" {
			t.Errorf("%s: %d %v", p, c, e)
		}
	}
}

// A 404 of a batch sub-resource (the batch exists) is the generic not_found.
func TestTranscodeV1BatchSubResourceNotFoundIsGeneric(t *testing.T) {
	s, _ := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "batch", "transcode_batch", "completed", nil, nil, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /b/{id}", s.v1Batch(func(w http.ResponseWriter, _ *http.Request) { fail(w, 404, "file not found") }, func(*http.Request) url.Values { return url.Values{} }))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/b/batch", nil))
	var e map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != 404 || e["code"] != "not_found" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/b/nope", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != 404 || e["code"] != "batch_not_found" {
		t.Fatal(w.Code, w.Body.String())
	}
}

// GET /podcasts answers 503 when podcasts are off, like the profile routes; a
// profile that is configured but disabled is a 404 unknown_podcast.
func TestTranscodeV1PodcastListFollowsTheSwitch(t *testing.T) {
	s, h := transcodeUI(t)
	s.cfg.Podcasts.Enabled = false
	if c, e := get(t, h, "/transcode/v1/podcasts", keyHdr()); c != 503 || e["code"] != "unavailable" {
		t.Fatal(c, e)
	}
	s.cfg.Podcasts.Enabled = true
	s.cfg.Podcasts.Podcasts = map[string]config.PodcastSettings{"on": {Enabled: true}, "off": {Enabled: false}}
	c, e := get(t, h, "/transcode/v1/podcasts", keyHdr())
	if c != 200 || e["enabled"] != true || fmt.Sprint(e["podcasts"]) != "[on]" {
		t.Fatal(c, e)
	}
	for _, p := range []string{"/transcode/v1/podcasts/off/local-library", "/transcode/v1/podcasts/off/ad-library"} {
		if c, e := get(t, h, p, keyHdr()); c != 404 || e["code"] != "unknown_podcast" {
			t.Errorf("%s: %d %v", p, c, e)
		}
	}
}

// The REST list is a page of verdicts: a default page of finished conversions
// with their full search evidence answers 200, and the evidence and phase
// costs are left to GET /transcode/v1/actions/{id}.
func TestTranscodeV1ActionsListIsLight(t *testing.T) {
	s, h := transcodeUI(t)
	st := s.engine.Deps().Store
	stages := make([]any, 0, 12)
	for i := 0; i < 12; i++ {
		candidates := make([]any, 0, 10)
		for c := 0; c < 10; c++ {
			candidates = append(candidates, fmt.Sprintf("libx265-crf%02d-preset-slow-stage%02d", 18+c, i))
		}
		stages = append(stages, map[string]any{"stage": fmt.Sprintf("stage-%02d", i), "candidates": candidates, "outcome": "measured"})
	}
	heavy := map[string]any{
		"automatic_search":   map[string]any{"stages": stages, "verified_samples": 7},
		"benchmark_decision": map[string]any{"winner": map[string]any{"video_codec": "libx265"}},
		"phase_costs":        map[string]any{"worker_encode": map[string]any{"duration_ms": 900000, "attempts": 1, "provenance": "worker_measured", "timing_scope": strings.Repeat("phase_wall; phases may overlap ", 40)}},
		"work_progress":      map[string]any{"phase": "encode", "bytes_read": 1, "total_bytes": 2},
	}
	for i := 0; i < 100; i++ {
		seedOperation(t, st, fmt.Sprintf("done%03d", i), "transcode_media", "completed", map[string]any{"mode": "size", "path": fmt.Sprintf("/media/Some Long Series Name (2024)/Season 01/Some Long Series Name (2024) - S01E%03d - An Episode Title [WEBDL-2160p][HDR10][EAC3 5.1][h265].mkv", i)}, heavy, heavy)
	}
	seedOperation(t, st, "batch1", "transcode_batch", "running", map[string]any{"mode": "size"}, nil,
		map[string]any{"series_title": "Some Series", "counts": map[string]any{"total": 8, "completed": 3, "running": 1, "queued": 4}, "batch_items": strings.Repeat("x", 20000)})
	allowed := map[string]bool{"id": true, "action_name": true, "status": true, "file": true, "title": true, "result": true, "waiting": true, "error": true, "parent_action_id": true, "created_at": true, "updated_at": true}
	w := v1(h, "GET", "/transcode/v1/actions?limit=100", "", keyHdr())
	var page map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 {
		t.Fatalf("limit=100: %d %.300s", w.Code, w.Body.String())
	}
	if w.Body.Len() > 64<<10 {
		t.Fatalf("a page must fit in 64 KiB: %d bytes", w.Body.Len())
	}
	items, _ := page["items"].([]any)
	if len(items) != 100 || page["has_more"] != true || page["next_offset"] != float64(100) || page["next_before"] != "done001" {
		t.Fatalf("100 light rows fit in one page: %d items, %v %v", len(items), page["has_more"], page["next_offset"])
	}
	for _, it := range items {
		row := it.(map[string]any)
		for k := range row {
			if !allowed[k] {
				t.Fatalf("list row carries %q: %v", k, row)
			}
		}
	}
	first := items[0].(map[string]any)
	if first["id"] != "batch1" || first["title"] != "Some Series" {
		t.Fatalf("newest first, a batch shows its title: %v", first)
	}
	if c, _ := first["result"].(map[string]any)["counts"].(map[string]any); c["total"] != float64(8) || c["queued"] != float64(4) || c["failed"] != nil {
		t.Fatalf("a batch shows its non-zero counts: %v", first)
	}
	row := items[1].(map[string]any)
	res, _ := row["result"].(map[string]any)
	if row["id"] != "done099" || !strings.HasSuffix(row["file"].(string), "[h265].mkv") || res["goal"] != "size" || res["outcome"] == "" || res["message"] == "" || res["encoder"] != "libx265" {
		t.Fatalf("a conversion row keeps id, file and verdict: %v", row)
	}
	// The last page says it is the last: no empty page after it.
	if c, last := get(t, h, "/transcode/v1/actions?limit=100&before=done001", keyHdr()); c != 200 || last["has_more"] != false || len(last["items"].([]any)) != 1 {
		t.Fatalf("exactly the rows left: %d %v", c, last)
	}
	code, one := get(t, h, "/transcode/v1/actions/done000", keyHdr())
	if body, _ := json.Marshal(one); code != 200 || !strings.Contains(string(body), "stage-11") || !strings.Contains(string(body), "phase_costs") {
		t.Fatalf("one action must keep the evidence: %d %.300s", code, body)
	}
}

// A page ends early instead of overflowing, and walking with next_offset still
// sees every action once, even when actions update between pages.
func TestTranscodeV1ActionsPageEndsEarlyAndWalksEveryAction(t *testing.T) {
	s, h := transcodeUI(t)
	st := s.engine.Deps().Store
	for i := 0; i < 100; i++ {
		seedOperation(t, st, fmt.Sprintf("f%03d", i), "transcode_media", "failed", map[string]any{"path": "/media/" + strings.Repeat("p", 700)}, nil, nil)
		touch(t, st, fmt.Sprintf("f%03d", i), func(inst *store.ActionInstance) {
			inst.ErrorJSON = `{"error":"` + strings.Repeat("e", 3000) + `"}`
			inst.WaitingReason = strings.Repeat("w", 3000)
		})
	}
	seen := map[string]bool{}
	offset, pages := 0, 0
	for ; pages < 100; pages++ {
		w := v1(h, "GET", fmt.Sprintf("/transcode/v1/actions?status=failed&limit=100&offset=%d", offset), "", keyHdr())
		var page map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || w.Body.Len() > 64<<10 {
			t.Fatalf("offset %d: %d, %d bytes", offset, w.Code, w.Body.Len())
		}
		items, _ := page["items"].([]any)
		for _, it := range items {
			row := it.(map[string]any)
			if len([]rune(row["error"].(string))) > 200 || len([]rune(row["waiting"].(string))) > 200 || len([]rune(row["file"].(string))) > 512 {
				t.Fatalf("text is not clipped: %d %d %d", len(row["error"].(string)), len(row["waiting"].(string)), len(row["file"].(string)))
			}
			id := row["id"].(string)
			if seen[id] {
				t.Fatalf("%s repeated", id)
			}
			seen[id] = true
		}
		if pages == 0 {
			if len(items) >= 100 || page["has_more"] != true {
				t.Fatalf("a page of big rows must end early with has_more: %d %v", len(items), page["has_more"])
			}
			// An older action updates: the order stays by creation.
			touch(t, st, "f000", func(inst *store.ActionInstance) { inst.WaitingReason = "touched" })
		}
		offset = int(page["next_offset"].(float64))
		if page["has_more"] != true {
			break
		}
	}
	if len(seen) != 100 || pages < 1 {
		t.Fatalf("the walk must see all 100 once: %d in %d pages", len(seen), pages+1)
	}
}

// Walking with before/next_before sees every action once even when new
// actions are created and rows leave the status filter during the walk.
func TestTranscodeV1ActionsBeforeCursorSurvivesChanges(t *testing.T) {
	s, h := transcodeUI(t)
	st := s.engine.Deps().Store
	for i := 0; i < 9; i++ {
		seedOperation(t, st, fmt.Sprintf("r%d", i), "transcode_media", "running", map[string]any{"path": fmt.Sprintf("/media/E%d.mkv", i)}, nil, nil)
	}
	seen := map[string]bool{}
	before := ""
	for pages := 0; pages < 20; pages++ {
		url := "/transcode/v1/actions?status=running&limit=3"
		if before != "" {
			url += "&before=" + before
		}
		code, page := get(t, h, url, keyHdr())
		if code != 200 {
			t.Fatal(code, page)
		}
		for _, it := range page["items"].([]any) {
			id := it.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("%s repeated", id)
			}
			seen[id] = true
		}
		if pages == 0 {
			// A new action arrives at the top, and the cursor row itself and
			// another seen one leave the filter.
			seedOperation(t, st, "zz-new", "transcode_media", "running", nil, nil, nil)
			cursor := page["next_before"].(string)
			touch(t, st, cursor, func(inst *store.ActionInstance) { inst.Status = "completed" })
			touch(t, st, "r8", func(inst *store.ActionInstance) { inst.Status = "completed" })
		}
		if page["has_more"] != true {
			break
		}
		before = page["next_before"].(string)
	}
	for i := 0; i < 9; i++ {
		if !seen[fmt.Sprintf("r%d", i)] {
			t.Fatalf("r%d skipped: %v", i, seen)
		}
	}
	if seen["zz-new"] {
		t.Fatal("an action created after the walk started is not part of it")
	}
	seedOperation(t, st, "foreign", "sonarr_thing", "completed", nil, nil, nil)
	for _, b := range []string{"nope", "foreign", "%20%20"} {
		if c, e := get(t, h, "/transcode/v1/actions?before="+b, keyHdr()); c != 400 || e["code"] != "invalid_request" {
			t.Errorf("before=%s: %d %v", b, c, e)
		}
	}
}

func touch(t *testing.T, st *store.Store, id string, change func(*store.ActionInstance)) {
	t.Helper()
	inst, err := st.GetActionInstanceIfExists(id)
	if err != nil || inst == nil {
		t.Fatal(id, err)
	}
	change(inst)
	if err := st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
}
