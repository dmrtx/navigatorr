package maintenanceui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcodeapi"
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

// This version only reads: every other method is refused, with or without
// the key, and nothing changes.
func TestTranscodeV1RejectsWrites(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "job1", "transcode_media", "running", map[string]any{"path": "/media/Show S01E01.mkv"}, nil, nil)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, p := range []string{"/transcode/v1/actions/job1", "/transcode/v1/pauses/m1", "/transcode/v1/drain", "/transcode/v1/recipes/x"} {
			w := v1(h, m, p, `{}`, keyHdr())
			var e map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			if w.Code != 405 || e["code"] != "method_not_allowed" || !strings.Contains(w.Header().Get("Allow"), "GET") {
				t.Fatalf("%s %s: %d %s", m, p, w.Code, w.Body.String())
			}
		}
		// Not signed in at all: 401 first.
		if w := v1(h, m, "/transcode/v1/actions/job1", `{}`, nil); w.Code != 401 {
			t.Fatal(m, w.Code)
		}
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
	rep := strings.NewReplacer("{name}", "nodeZ", "{id}", "nope", "{section}", "inputs", "{block_id}", "b1", "{artifact}", "transcript")
	for _, r := range transcodeapi.Routes() {
		p := rep.Replace(r.Path)
		w := v1(h, "GET", p, "", keyHdr())
		if w.Code == 401 || w.Code == 405 || w.Code >= 500 && w.Code != 503 {
			t.Errorf("%s: %d %s", p, w.Code, w.Body.String())
		}
		var raw any
		if json.Unmarshal(w.Body.Bytes(), &raw) != nil {
			t.Errorf("%s: not JSON: %q", p, w.Body.String())
		}
		var e struct{ Code string }
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code == 404 && e.Code == "not_found" && !strings.Contains(r.Path, "{section}") && !strings.Contains(r.Path, "podcast/") {
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
		"/transcode/v1/podcasts/nope/ad-library":   "unknown_podcast",
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

func TestTranscodeV1UIAuthWriteNeedsTheSameOriginHeader(t *testing.T) {
	_, h := transcodeUI(t)
	// A cookie-less browser POST without the header is 401; with it, it is the
	// method that is refused. Either way nothing is written.
	w := v1(h, "POST", "/transcode/v1/drain", `{}`, map[string]string{"Authorization": "Bearer " + testToken})
	if w.Code != 401 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = v1(h, "POST", "/transcode/v1/drain", `{}`, map[string]string{"Authorization": "Bearer " + testToken, "X-Navigatorr-Request": "1"})
	if w.Code != 405 {
		t.Fatal(w.Code, w.Body.String())
	}
}
