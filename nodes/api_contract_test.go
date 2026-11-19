package nodes

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The numbers of a build log restart whenever its buffer is recreated. A reader
// that sends back the generation it saw, or an `after` past the end, gets the
// log from the start with reset:true instead of silence.
func TestAPILogGenerationAndReset(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	call(t, h, "POST", "/nodes/v1/builds", `{"client_id":"bld_job00001","target":"remote-box","source":"ci","title":"t"}`)
	for _, l := range []string{"one", "two", "three"} {
		call(t, h, "POST", "/nodes/v1/builds/bld_job00001/heartbeat", `{"log":["`+l+`"]}`)
	}
	const url = "/nodes/v1/builds/bld_job00001/log"
	n := func(m map[string]any) int { return len(m["lines"].([]any)) }

	_, first := call(t, h, "GET", url, "")
	g1, _ := first["generation"].(string)
	if g1 == "" || first["reset"] != false || n(first) != 3 || first["next_after"].(float64) != 3 {
		t.Fatal(first)
	}
	// Up to date with the right generation: nothing new, no reset.
	if _, m := call(t, h, "GET", url+"?after=3&generation="+g1, ""); n(m) != 0 || m["reset"] != false || m["next_after"].(float64) != 3 {
		t.Fatal(m)
	}
	// Past the end of this buffer: reset, from the start.
	if _, m := call(t, h, "GET", url+"?after=10&generation="+g1, ""); n(m) != 3 || m["reset"] != true || m["next_after"].(float64) != 3 {
		t.Fatal("after beyond the total", m)
	}
	if _, m := call(t, h, "GET", url+"?after=10", ""); n(m) != 3 || m["reset"] != true {
		t.Fatal("after beyond the total, no generation", m)
	}
	// Another generation: reset even if the position looks fine.
	if _, m := call(t, h, "GET", url+"?after=2&generation=not-this-one", ""); n(m) != 3 || m["reset"] != true {
		t.Fatal("generation differs", m)
	}

	// The buffer is dropped (restart, eviction, retention) and nothing is
	// written again: the old position resets to an empty, honest answer.
	s.logs.expire("bld_job00001")
	if _, m := call(t, h, "GET", url+"?after=3&generation="+g1, ""); n(m) != 0 || m["reset"] != true || m["next_after"].(float64) != 0 || m["generation"] != "" {
		t.Fatal("dropped buffer", m)
	}
	// The build writes again: a new buffer, a new generation, numbers from 1.
	call(t, h, "POST", "/nodes/v1/builds/bld_job00001/heartbeat", `{"log":["again"]}`)
	_, m := call(t, h, "GET", url+"?after=3&generation="+g1, "")
	g2, _ := m["generation"].(string)
	if g2 == "" || g2 == g1 || m["reset"] != true || n(m) != 1 || m["next_after"].(float64) != 1 {
		t.Fatal("recreated buffer", m)
	}
	if first := m["lines"].([]any)[0].(map[string]any); first["n"].(float64) != 1 || first["text"] != "again" {
		t.Fatal(first)
	}
	// Following the new numbers from there is quiet again.
	if _, m := call(t, h, "GET", url+"?after=1&generation="+g2, ""); n(m) != 0 || m["reset"] != false {
		t.Fatal(m)
	}
}

// A restart forgets the buffers: a new Service on the same database starts with
// no buffer, so a reader holding a generation is reset.
func TestAPILogResetsAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.db")
	s, _ := buildSvcAt(t, path)
	h := s.Handler()
	call(t, h, "POST", "/nodes/v1/builds", `{"client_id":"bld_job00001","target":"remote-box","source":"ci","title":"t"}`)
	call(t, h, "POST", "/nodes/v1/builds/bld_job00001/heartbeat", `{"log":["one","two"]}`)
	_, m := call(t, h, "GET", "/nodes/v1/builds/bld_job00001/log", "")
	g1, _ := m["generation"].(string)
	s.Close()

	s2, _ := buildSvcAt(t, path)
	h2 := s2.Handler()
	_, m = call(t, h2, "GET", "/nodes/v1/builds/bld_job00001/log?after=2&generation="+g1, "")
	if m["reset"] != true || len(m["lines"].([]any)) != 0 || m["next_after"].(float64) != 0 {
		t.Fatal(m)
	}
}

// builds-pause: the body must say paused, as a JSON boolean. {} is no longer
// read as false (which would resume the node).
func TestAPIBuildsPauseNeedsABoolean(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	if code, e := call(t, h, "POST", "/nodes/v1/nodes/nodeA/builds-pause", `{"paused":true}`); code != 200 {
		t.Fatal(code, e)
	}
	for _, body := range []string{``, `{}`, `{"paused":null}`, `{"paused":"yes"}`, `{"paused":"false"}`, `{"paused":1}`, `{"paused":0}`, `{"pause":false}`, `{"paused":false,"extra":1}`, `[]`, `not json`} {
		code, e := call(t, h, "POST", "/nodes/v1/nodes/nodeA/builds-pause", body)
		if code != 400 || e["code"] != "invalid_request" || e["hint"] == "" {
			t.Fatalf("body %q: %d %v", body, code, e)
		}
	}
	if v, _ := s.Node(context.Background(), "nodeA"); !v.BuildsPaused {
		t.Fatal("none of the bad bodies may resume the node")
	}
	if code, _ := call(t, h, "POST", "/nodes/v1/nodes/nodeA/builds-pause", `{"paused":false}`); code != 200 {
		t.Fatal(code)
	}
	if v, _ := s.Node(context.Background(), "nodeA"); v.BuildsPaused {
		t.Fatal("an explicit false resumes")
	}
}

// caseTopology resolves node names without regard to case, answering the
// canonical spelling, to prove every path uses the name the lookup returns.
type caseTopology struct{}

func (caseTopology) Node(target string) (string, bool) {
	for _, n := range []string{"nodeA", "nodeB"} {
		if strings.EqualFold(n, target) {
			return n, true
		}
	}
	return "", false
}
func (caseTopology) Nodes() []string { return []string{"nodeA", "nodeB"} }

// WorkerSet.Get (what the real topology wraps) matches names exactly, so a
// non-canonical name never reaches the registry in production; the registry
// still keys everything by the name the lookup returns, which this proves with
// a topology that does accept other spellings.
func TestNodeNamesUseTheCanonicalSpelling(t *testing.T) {
	clock := &buildClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	s, err := Open(Options{Path: filepath.Join(t.TempDir(), "nodes.db"), Topology: caseTopology{}, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := s.Handler()

	code, n := call(t, h, "POST", "/nodes/v1/nodes/NODEa/builds-pause", `{"paused":true}`)
	if code != 200 || n["name"] != "nodeA" || n["builds_paused"] != true {
		t.Fatal("answer is the canonical node", code, n)
	}
	if p, _ := s.BuildsPauses(); len(p) != 1 || p["nodeA"].Since == "" {
		t.Fatalf("the pause is keyed by the canonical name: %+v", p)
	}
	if _, n := call(t, h, "GET", "/nodes/v1/nodes/NoDeA", ""); n["name"] != "nodeA" || n["builds_paused"] != true {
		t.Fatal(n)
	}
	// A create through another spelling is refused by the same pause.
	if code, e := call(t, h, "POST", "/nodes/v1/builds", `{"target":"NODEA","via":"node_lease","source":"ci","title":"t"}`); code != 409 || e["code"] != BuildsPausedCode {
		t.Fatal(code, e)
	}
	// ?node= filters by the canonical name.
	call(t, h, "POST", "/nodes/v1/nodes/nodeA/builds-pause", `{"paused":false}`)
	if code, e := call(t, h, "POST", "/nodes/v1/builds", `{"target":"nodeA","via":"node_lease","source":"ci","title":"t"}`); code != 200 {
		t.Fatal(code, e)
	}
	for _, spelling := range []string{"nodeA", "NODEA", "nodea"} {
		_, page := call(t, h, "GET", "/nodes/v1/builds?node="+spelling, "")
		if got := idsOf(t, page); len(got) != 1 {
			t.Fatalf("?node=%s: %v", spelling, got)
		}
	}
	if _, page := call(t, h, "GET", "/nodes/v1/builds?node=nodeB", ""); len(idsOf(t, page)) != 0 {
		t.Fatal("another node's filter is empty", page)
	}
	if code, e := call(t, h, "POST", "/nodes/v1/nodes/nowhere/builds-pause", `{"paused":true}`); code != 404 || e["code"] != "unknown_node" {
		t.Fatal(code, e)
	}
}

// A heartbeat on a build that ended is 409 build_ended with the final record.
func TestAPIHeartbeatOnAnEndedBuild(t *testing.T) {
	s, clock := buildSvc(t)
	h := s.Handler()
	call(t, h, "POST", "/nodes/v1/builds", `{"client_id":"bld_job00001","target":"remote-box","source":"ci","title":"t"}`)
	call(t, h, "POST", "/nodes/v1/builds/bld_job00001/finish", `{"status":"failed","exit_code":2}`)
	code, e := call(t, h, "POST", "/nodes/v1/builds/bld_job00001/heartbeat", `{"progress":"late"}`)
	b, _ := e["build"].(map[string]any)
	if code != 409 || e["code"] != "build_ended" || e["hint"] == "" || e["error"] == "" || b["id"] != "bld_job00001" || b["status"] != "failed" || b["exit_code"].(float64) != 2 {
		t.Fatal(code, e)
	}
	// An empty body is the same answer.
	if code, e := call(t, h, "POST", "/nodes/v1/builds/bld_job00001/heartbeat", ``); code != 409 || e["code"] != "build_ended" {
		t.Fatal(code, e)
	}
	// So is one past its expiry: marked lost, still build_ended.
	call(t, h, "POST", "/nodes/v1/builds", `{"client_id":"bld_job00002","target":"remote-box","source":"ci","title":"t","ttl_seconds":30}`)
	clock.advance(time.Minute)
	code, e = call(t, h, "POST", "/nodes/v1/builds/bld_job00002/heartbeat", `{}`)
	if b, _ := e["build"].(map[string]any); code != 409 || e["code"] != "build_ended" || b["status"] != "lost" {
		t.Fatal(code, e)
	}
	// Cancel of a cancelled build is 200 and unchanged; unknown is 404.
	call(t, h, "POST", "/nodes/v1/builds", `{"client_id":"bld_job00003","target":"remote-box","source":"ci","title":"t"}`)
	call(t, h, "POST", "/nodes/v1/builds/bld_job00003/cancel", "")
	if code, e := call(t, h, "POST", "/nodes/v1/builds/bld_job00003/heartbeat", `{}`); code != 409 || e["build"].(map[string]any)["status"] != "cancelled" {
		t.Fatal(code, e)
	}
	if code, e := call(t, h, "POST", "/nodes/v1/builds/nope/heartbeat", `{}`); code != 404 || e["code"] != "build_not_found" {
		t.Fatal(code, e)
	}
}

func TestAPICreateIDTakenAndClientIDValidation(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	body := func(id, target, source string) string {
		return `{"client_id":"` + id + `","target":"` + target + `","source":"` + source + `","title":"t"}`
	}
	if code, e := call(t, h, "POST", "/nodes/v1/builds", body("bld_taken001", "remote-box", "ci")); code != 200 {
		t.Fatal(code, e)
	}
	for _, other := range []string{body("bld_taken001", "other-box", "ci"), body("bld_taken001", "remote-box", "other-tool")} {
		code, e := call(t, h, "POST", "/nodes/v1/builds", other)
		if code != 409 || e["code"] != BuildIDTakenCode || e["hint"] == "" {
			t.Fatal(other, code, e)
		}
	}
	// The same id after the build ended: the same build back, unchanged.
	call(t, h, "POST", "/nodes/v1/builds/bld_taken001/finish", `{"status":"succeeded"}`)
	if code, e := call(t, h, "POST", "/nodes/v1/builds", body("bld_taken001", "remote-box", "ci")); code != 200 || e["status"] != "succeeded" {
		t.Fatal(code, e)
	}

	long59 := "bld_" + strings.Repeat("a", 59)
	if code, e := call(t, h, "POST", "/nodes/v1/builds", body(long59, "remote-box", "ci")); code != 200 {
		t.Fatal("59 characters is the longest id", code, e)
	}
	if code, e := call(t, h, "POST", "/nodes/v1/builds", body("bld_"+strings.Repeat("b", 8), "remote-box", "ci")); code != 200 {
		t.Fatal("8 characters is the shortest id", code, e)
	}
	for name, id := range map[string]string{
		"too short":   "bld_1234567",
		"too long":    "bld_" + strings.Repeat("a", 60),
		"no prefix":   "job_12345678",
		"bare":        "12345678",
		"space":       "bld_1234 5678",
		"slash":       "bld_1234/5678",
		"dot":         "bld_1234.5678",
		"unicode":     "bld_1234567é",
		"only prefix": "bld_",
	} {
		code, e := call(t, h, "POST", "/nodes/v1/builds", body(id, "remote-box", "ci"))
		if code != 400 || e["code"] != "invalid_request" || e["hint"] == "" {
			t.Fatalf("%s (%q): %d %v", name, id, code, e)
		}
	}
	if _, page := call(t, h, "GET", "/nodes/v1/builds", ""); len(idsOf(t, page)) != 3 {
		t.Fatal("refused creates record nothing", page)
	}
}

// ttl_seconds as the code does it: 0 is the default (900), a positive value is
// used as given (no floor), more than 7200 is clamped, negative is refused.
func TestAPITTLClampingMatchesTheSpec(t *testing.T) {
	s, _ := buildSvc(t)
	h := s.Handler()
	window := func(ttl string) (int, time.Duration) {
		_, b := call(t, h, "POST", "/nodes/v1/builds", `{"target":"remote-box","source":"ci","title":"t"`+ttl+`}`)
		if b["created_at"] == nil {
			return 0, 0
		}
		c, _ := time.Parse(time.RFC3339, b["created_at"].(string))
		e, _ := time.Parse(time.RFC3339, b["expires_at"].(string))
		return 200, e.Sub(c)
	}
	for ttl, want := range map[string]time.Duration{
		``:                       15 * time.Minute,
		`,"ttl_seconds":0`:       15 * time.Minute,
		`,"ttl_seconds":30`:      30 * time.Second,
		`,"ttl_seconds":60`:      time.Minute,
		`,"ttl_seconds":7200`:    2 * time.Hour,
		`,"ttl_seconds":7201`:    2 * time.Hour,
		`,"ttl_seconds":9999999`: 2 * time.Hour,
	} {
		if code, got := window(ttl); code != 200 || got != want {
			t.Fatalf("%q: %d %v, want %v", ttl, code, got, want)
		}
	}
	if code, e := call(t, h, "POST", "/nodes/v1/builds", `{"target":"remote-box","source":"ci","title":"t","ttl_seconds":-5}`); code != 400 || e["code"] != "invalid_request" {
		t.Fatal(code, e)
	}
}

// The spec says what the code does: 401 and the Error schema on every
// operation, waiting among the Build statuses, and one 200 for create.
func TestSpecDeclaresAuthErrorsAndStatuses(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas struct {
				Build struct {
					Properties struct {
						Status struct {
							Enum []string `json:"enum"`
						} `json:"status"`
					} `json:"properties"`
				} `json:"Build"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(Spec(), &doc); err != nil {
		t.Fatal(err)
	}
	for p, ops := range doc.Paths {
		for m, op := range ops {
			r, ok := op.Responses["401"]
			if !ok || r.Content["application/json"].Schema.Ref != "#/components/schemas/Error" {
				t.Errorf("%s %s must declare 401 with the Error schema", strings.ToUpper(m), p)
			}
		}
	}
	has := map[string]bool{}
	for _, v := range doc.Components.Schemas.Build.Properties.Status.Enum {
		has[v] = true
	}
	for _, v := range []string{BuildWaiting, BuildRunning, BuildSucceeded, BuildFailed, BuildCancelled, BuildLost} {
		if !has[v] {
			t.Errorf("Build.status enum lacks %s", v)
		}
	}
	create := doc.Paths["/nodes/v1/builds"]["post"].Responses
	if _, ok := create["200"]; !ok {
		t.Error("create answers 200")
	}
	if _, ok := create["201"]; ok {
		t.Error("create never answers 201")
	}
}
