package transcodeapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/openapi"
)

func stubHandlers() Handlers {
	h := Handlers{}
	for _, r := range Routes() {
		h[r.Key()] = func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, 200, map[string]any{"ok": true}) }
	}
	return h
}

func do(t *testing.T, h http.Handler, method, path string) (int, map[string]any, http.Header) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: not JSON: %q", method, path, w.Body.String())
		}
	}
	return w.Code, out, w.Header()
}

// Every route is in the spec, every spec operation is a route, and each is
// documented (summary, description, operationId, a 200 and a 401).
func TestSpecAndRoutesAgree(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			Summary     string                      `json:"summary"`
			Description string                      `json:"description"`
			OperationID string                      `json:"operationId"`
			Parameters  []struct{ Name, In string } `json:"parameters"`
			Responses   map[string]any              `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(Spec(), &doc); err != nil {
		t.Fatal(err)
	}
	inSpec := map[string]bool{}
	ids := map[string]bool{}
	for p, ops := range doc.Paths {
		for m, op := range ops {
			key := strings.ToUpper(m) + " " + p
			inSpec[key] = true
			if m != "get" {
				t.Errorf("%s: this version is read-only", key)
			}
			if op.Summary == "" || op.Description == "" || op.OperationID == "" {
				t.Errorf("%s needs a summary, a description and an operationId", key)
			}
			if ids[op.OperationID] {
				t.Errorf("duplicate operationId %s", op.OperationID)
			}
			ids[op.OperationID] = true
			for _, c := range []string{"200", "401"} {
				if _, ok := op.Responses[c]; !ok {
					t.Errorf("%s has no %s response", key, c)
				}
			}
			// every {placeholder} of the path is a declared path parameter
			for _, seg := range strings.Split(p, "/") {
				if strings.HasPrefix(seg, "{") {
					found := false
					for _, par := range op.Parameters {
						if par.In == "path" && "{"+par.Name+"}" == seg {
							found = true
						}
					}
					if !found {
						t.Errorf("%s: path parameter %s is not documented", key, seg)
					}
				}
			}
		}
	}
	inRoutes := map[string]bool{}
	for _, r := range Routes() {
		inRoutes[r.Key()] = true
		if !inSpec[r.Key()] {
			t.Errorf("route %s is not in the spec", r.Key())
		}
	}
	for k := range inSpec {
		if !inRoutes[k] {
			t.Errorf("spec operation %s is not a route", k)
		}
	}
}

// The spec loads with the parser search_api uses and finds the operations by
// the words an LLM would use.
func TestSpecIsFoundBySearchAPI(t *testing.T) {
	idx, err := openapi.Parse(context.Background(), "transcode", Spec())
	if err != nil {
		t.Fatal(err)
	}
	if idx.Count() != len(Routes()) {
		t.Fatalf("index has %d endpoints, want %d", idx.Count(), len(Routes()))
	}
	for query, want := range map[string]string{
		"worker":  "/transcode/v1/workers",
		"drain":   "/transcode/v1/drain",
		"batch":   "/transcode/v1/batches/{id}/items",
		"recipe":  "/transcode/v1/recipes/{name}",
		"backups": "/transcode/v1/backups",
	} {
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
}

func TestNewHandlerRequiresExactlyTheRoutes(t *testing.T) {
	if _, err := NewHandler(stubHandlers()); err != nil {
		t.Fatal(err)
	}
	missing := stubHandlers()
	delete(missing, Routes()[0].Key())
	if _, err := NewHandler(missing); err == nil {
		t.Fatal("a missing handler must be an error")
	}
	extra := stubHandlers()
	extra["GET "+BasePath+"/surprise"] = func(http.ResponseWriter, *http.Request) {}
	if _, err := NewHandler(extra); err == nil {
		t.Fatal("an extra handler must be an error")
	}
}

func TestRoutesAnswerAndTheRestIsJSONErrors(t *testing.T) {
	h, err := NewHandler(stubHandlers())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Routes() {
		p := strings.NewReplacer("{name}", "m1", "{id}", "job1", "{section}", "inputs", "{block_id}", "b1", "{artifact}", "transcript").Replace(r.Path)
		if code, _, _ := do(t, h, "GET", p); code != 200 {
			t.Errorf("GET %s = %d", p, code)
		}
		code, body, hdr := do(t, h, "POST", p)
		if code != 405 || body["code"] != "method_not_allowed" || !strings.Contains(hdr.Get("Allow"), "GET") {
			t.Errorf("POST %s = %d %v %q", p, code, body, hdr.Get("Allow"))
		}
	}
	code, body, _ := do(t, h, "GET", BasePath+"/nope")
	if code != 404 || body["code"] != "not_found" {
		t.Fatalf("unknown path = %d %v", code, body)
	}
}

func TestCodeForStatus(t *testing.T) {
	for st, want := range map[int]string{400: "invalid_request", 401: "unauthorized", 403: "forbidden", 404: "not_found", 405: "method_not_allowed", 409: "conflict", 503: "unavailable", 500: "internal", 502: "internal"} {
		if got := CodeForStatus(st); got != want {
			t.Errorf("%d: %s, want %s", st, got, want)
		}
	}
}
