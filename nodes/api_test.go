package nodes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/openapi"
)

func call(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: not JSON: %q", method, path, w.Body.String())
		}
	}
	return w.Code, out
}

// Every route the handler serves is in the spec and every spec operation is a route.
func TestSpecAndRoutesAgree(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			Summary     string `json:"summary"`
			Description string `json:"description"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(Spec(), &doc); err != nil {
		t.Fatal(err)
	}
	inSpec := map[string]bool{}
	for p, ops := range doc.Paths {
		for m, op := range ops {
			key := strings.ToUpper(m) + " " + p
			inSpec[key] = true
			if op.Summary == "" || op.Description == "" {
				t.Errorf("%s needs a summary and a description", key)
			}
		}
	}
	inRoutes := map[string]bool{}
	for _, r := range Routes() {
		inRoutes[r.Method+" "+r.Path] = true
		if !inSpec[r.Method+" "+r.Path] {
			t.Errorf("route %s %s is not in the spec", r.Method, r.Path)
		}
	}
	for k := range inSpec {
		if !inRoutes[k] {
			t.Errorf("spec operation %s is not a route", k)
		}
	}
	// And the handler really serves each route (a registered pattern, not the catch-all).
	s, _ := buildSvc(t)
	h := s.Handler()
	for _, r := range Routes() {
		p := strings.NewReplacer("{name}", "nodeA", "{id}", "nope").Replace(r.Path)
		code, body := call(t, h, r.Method, p, "{}")
		if code == 404 && body["code"] == "not_found" {
			t.Errorf("%s %s fell to the catch-all", r.Method, p)
		}
	}
}

// The spec loads with the same parser list_services/search_api use, and the
// operations an LLM needs are found by search.
func TestSpecIsFoundBySearchAPI(t *testing.T) {
	idx, err := openapi.Parse(context.Background(), "nodes", Spec())
	if err != nil {
		t.Fatal(err)
	}
	if idx.Count() != len(Routes()) {
		t.Fatalf("index has %d endpoints, want %d", idx.Count(), len(Routes()))
	}
	for query, want := range map[string]string{"cancel": "/nodes/v1/builds/{id}/cancel", "builds": "/nodes/v1/builds", "pause": "/nodes/v1/nodes/{name}/builds-pause"} {
		found := false
		for _, e := range idx.Search(query) {
			if e.Path == want {
				found = true
			}
		}
		if !found {
			t.Errorf("search %q does not find %s", query, want)
		}
	}
	d, err := idx.GetDetail("/nodes/v1/builds", "POST")
	if err != nil || d.RequestBody == nil {
		t.Fatalf("create detail: %+v %v", d, err)
	}
}

func TestAPICreateIsIdempotentByClientID(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	body := `{"client_id":"bld_job00001","target":"remote-box","source":"ci","title":"compile"}`
	c1, b1 := call(t, h, "POST", "/nodes/v1/builds", body)
	c2, b2 := call(t, h, "POST", "/nodes/v1/builds", body)
	if c1 != 201 && c1 != 200 || c2 != 200 && c2 != 201 || b1["id"] != "bld_job00001" || b2["id"] != "bld_job00001" {
		t.Fatal(c1, b1, c2, b2)
	}
	_, list := call(t, h, "GET", "/nodes/v1/builds", "")
	if bs, _ := list["builds"].([]any); len(bs) != 1 {
		t.Fatal("one record", list)
	}
	code, e := call(t, h, "POST", "/nodes/v1/builds", `{"id":"bld_job00001","target":"x","source":"ci","title":"t"}`)
	if code != 400 || e["code"] != "invalid_request" || e["hint"] == "" {
		t.Fatal("id in the body is refused with a hint", code, e)
	}
}

func TestAPIErrorsCarryCodeAndHint(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	code, e := call(t, h, "POST", "/nodes/v1/builds", `{"target":"nodeA","source":"ci","title":"t"}`)
	if code != 409 || e["code"] != NodeLeaseRequiredCode || e["hint"] == "" || e["error"] == "" {
		t.Fatal(code, e)
	}
	if code, e := call(t, h, "POST", "/nodes/v1/builds", `{"target":"nodeA","via":"node_lease","source":"ci","title":"t"}`); code != 201 && code != 200 {
		t.Fatal(code, e)
	}
	if code, _ := call(t, h, "POST", "/nodes/v1/nodes/nodeB/builds-pause", `{"paused":true}`); code != 200 {
		t.Fatal(code)
	}
	code, e = call(t, h, "POST", "/nodes/v1/builds", `{"target":"nodeB","via":"node_lease","source":"ci","title":"t"}`)
	if code != 409 || e["code"] != BuildsPausedCode || e["hint"] == "" {
		t.Fatal(code, e)
	}
	if code, e := call(t, h, "GET", "/nodes/v1/builds/nope", ""); code != 404 || e["code"] != "build_not_found" {
		t.Fatal(code, e)
	}
	if code, e := call(t, h, "POST", "/nodes/v1/nodes/nowhere/builds-pause", `{"paused":true}`); code != 404 || e["code"] != "unknown_node" {
		t.Fatal(code, e)
	}
	if code, e := call(t, h, "GET", "/nodes/v1/other", ""); code != 404 || e["code"] != "not_found" {
		t.Fatal(code, e)
	}
}

func TestAPIPauseIsIdempotent(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	for i := 0; i < 2; i++ {
		if code, e := call(t, h, "POST", "/nodes/v1/nodes/nodeA/builds-pause", `{"paused":true}`); code != 200 {
			t.Fatal(code, e)
		}
	}
	_, n := call(t, h, "GET", "/nodes/v1/nodes/nodeA", "")
	if n["builds_paused"] != true {
		t.Fatal(n)
	}
	call(t, h, "POST", "/nodes/v1/nodes/nodeA/builds-pause", `{"paused":false}`)
	call(t, h, "POST", "/nodes/v1/nodes/nodeA/builds-pause", `{"paused":false}`)
	_, n = call(t, h, "GET", "/nodes/v1/nodes/nodeA", "")
	if n["builds_paused"] == true {
		t.Fatal(n)
	}
}

func TestAPILogNumbersAndCursor(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	call(t, h, "POST", "/nodes/v1/builds", `{"client_id":"bld_job00001","target":"remote-box","source":"ci","title":"t"}`)
	for _, l := range []string{"one", "two", "three"} {
		if code, e := call(t, h, "POST", "/nodes/v1/builds/bld_job00001/heartbeat", `{"log":["`+l+`"]}`); code != 200 {
			t.Fatal(code, e)
		}
	}
	_, all := call(t, h, "GET", "/nodes/v1/builds/bld_job00001/log", "")
	lines, _ := all["lines"].([]any)
	if len(lines) != 3 {
		t.Fatal(all)
	}
	for i, l := range lines {
		if int(l.(map[string]any)["n"].(float64)) != i+1 {
			t.Fatal("lines are numbered from 1", lines)
		}
	}
	_, rest := call(t, h, "GET", "/nodes/v1/builds/bld_job00001/log?after=2", "")
	if ls, _ := rest["lines"].([]any); len(ls) != 1 || ls[0].(map[string]any)["text"] != "three" {
		t.Fatal(rest)
	}
	for _, id := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"} {
		call(t, h, "POST", "/nodes/v1/builds", `{"client_id":"bld_`+id+`","target":"remote-box","source":"ci","title":"t"}`)
	}
	_, page := call(t, h, "GET", "/nodes/v1/builds?limit=2", "")
	next, _ := page["next_cursor"].(string)
	if bs, _ := page["builds"].([]any); len(bs) != 2 || next == "" {
		t.Fatal(page)
	}
	_, page2 := call(t, h, "GET", "/nodes/v1/builds?limit=2&cursor="+next, "")
	if bs, _ := page2["builds"].([]any); len(bs) != 2 {
		t.Fatal(page2)
	}
}
