package maintenanceui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcodeapi"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// recorder replaces a tool with a fake that stores the arguments of each call
// and answers text (or, when isErr, a refusal).
type toolCalls struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (c *toolCalls) n() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.calls) }
func (c *toolCalls) last() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return nil
	}
	return c.calls[len(c.calls)-1]
}

func fakeWrite(s *Server, name string, answer func(args map[string]any) (string, bool)) *toolCalls {
	rec := &toolCalls{}
	fakeTool(s, name, func(args map[string]any) (*mcp.CallToolResult, error) {
		rec.mu.Lock()
		rec.calls = append(rec.calls, args)
		rec.mu.Unlock()
		text, isErr := answer(args)
		if isErr {
			return mcp.NewToolResultError(text), nil
		}
		return mcp.NewToolResultText(text), nil
	})
	return rec
}

func ok(text string) func(map[string]any) (string, bool) {
	return func(map[string]any) (string, bool) { return text, false }
}

func jsonHdr() map[string]string {
	h := keyHdr()
	h["Content-Type"] = "application/json"
	return h
}

func write(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	return v1(h, method, path, body, jsonHdr())
}

func code(t *testing.T, w *httptest.ResponseRecorder) (int, string) {
	t.Helper()
	var e map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	c, _ := e["code"].(string)
	return w.Code, c
}

// Body validation, for every write that takes a body: bad JSON, an unknown
// field, a second value, a wrong type, a body over 1 MiB and a wrong
// Content-Type are refused before any tool is called.
func TestTranscodeV1WriteBodyValidation(t *testing.T) {
	s, h := transcodeUI(t)
	run := fakeWrite(s, "action_run", ok(`{"id":"a1"}`))
	for name, tc := range map[string]struct {
		body string
		ct   string
		code int
		kind string
	}{
		"bad json":      {`{"action":`, "application/json", 400, "invalid_request"},
		"unknown field": {`{"action":"transcode_media","surprise":1}`, "application/json", 400, "invalid_request"},
		"two values":    {`{"action":"transcode_media"}{"x":1}`, "application/json", 400, "invalid_request"},
		"wrong type":    {`{"action":5}`, "application/json", 400, "invalid_request"},
		"empty":         {``, "application/json", 400, "invalid_request"},
		"too large":     {`{"action":"transcode_media","inputs":{"x":"` + strings.Repeat("a", 1<<20) + `"}}`, "application/json", 413, "payload_too_large"},
		"wrong type ct": {`{"action":"transcode_media"}`, "text/plain", 415, "unsupported_media_type"},
		"no ct":         {`{"action":"transcode_media"}`, "", 415, "unsupported_media_type"},
	} {
		hd := keyHdr()
		if tc.ct != "" {
			hd["Content-Type"] = tc.ct
		}
		c, k := code(t, v1(h, "POST", "/transcode/v1/actions", tc.body, hd))
		if c != tc.code || k != tc.kind {
			t.Errorf("%s: %d %s, want %d %s", name, c, k, tc.code, tc.kind)
		}
	}
	if run.n() != 0 {
		t.Fatal("a refused body reached the tool")
	}
	// The same rules on a route that has the other kind of body.
	if c, k := code(t, write(h, "PUT", "/transcode/v1/drain", `{"enabled":true,"x":1}`)); c != 400 || k != "invalid_request" {
		t.Error("drain unknown field", c, k)
	}
	if c, _ := code(t, write(h, "PUT", "/transcode/v1/drain", `{}`)); c != 400 {
		t.Error("drain without enabled", c)
	}
}

// action_run takes only the transcode workflows, and passes its idempotency
// key through from the header or the body.
func TestTranscodeV1RunScopeAndIdempotency(t *testing.T) {
	s, h := transcodeUI(t)
	run := fakeWrite(s, "action_run", ok(`{"id":"a1","status":"running"}`))
	// Scope guard: another workflow is a 400 and the tool is not called.
	if c, k := code(t, write(h, "POST", "/transcode/v1/actions", `{"action":"sync_something"}`)); c != 400 || k != "invalid_request" {
		t.Fatal(c, k)
	}
	if run.n() != 0 {
		t.Fatal("a non-transcode workflow reached action_run")
	}
	w := write(h, "POST", "/transcode/v1/actions", `{"action":"transcode_media","inputs":{"path":"/media/Example Show S01E01.mkv"},"idempotency_key":"k-body"}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	a := run.last()
	if a["action"] != "transcode_media" || a["idempotency_key"] != "k-body" || !strings.Contains(a["inputs"].(string), "Example Show") {
		t.Fatal(a)
	}
	// The header works too, and the same key twice is fine.
	hd := jsonHdr()
	hd["Idempotency-Key"] = "k-header"
	if w := v1(h, "POST", "/transcode/v1/actions", `{"action":"transcode_batch"}`, hd); w.Code != 202 || run.last()["idempotency_key"] != "k-header" {
		t.Fatal(w.Code, run.last())
	}
	hd["Idempotency-Key"] = "same"
	if w := v1(h, "POST", "/transcode/v1/actions", `{"action":"transcode_batch","idempotency_key":"same"}`, hd); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Two different keys are refused, not guessed between.
	before := run.n()
	hd["Idempotency-Key"] = "one"
	if c, k := code(t, v1(h, "POST", "/transcode/v1/actions", `{"action":"transcode_batch","idempotency_key":"two"}`, hd)); c != 400 || k != "invalid_request" || run.n() != before {
		t.Fatal(c, k)
	}
	// A refusal of the inputs by the tool is a 400 with its field named.
	fakeWrite(s, "action_run", func(map[string]any) (string, bool) {
		return `{"error":"path is required","code":"invalid_params","field":"path"}`, true
	})
	w = write(h, "POST", "/transcode/v1/actions", `{"action":"transcode_media"}`)
	if c, k := code(t, w); c != 400 || k != "invalid_request" || !strings.Contains(w.Body.String(), `\"path\"`) {
		t.Fatal(c, k, w.Body.String())
	}
}

// Writes under /actions/{id} answer 404 for an action that is not a transcode
// one, exactly as the reads do, and never reach the tool.
func TestTranscodeV1ActionWritesAreScopedToTranscode(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "other1", "sync_something", "running", nil, nil, nil)
	seedOperation(t, s.engine.Deps().Store, "job1", "transcode_media", "running", nil, nil, nil)
	cancel := fakeWrite(s, "action_cancel", ok(`{"id":"job1","status":"cancelled"}`))
	resume := fakeWrite(s, "action_resume", ok(`{"id":"job1"}`))
	retry := fakeWrite(s, "action_retry", ok(`{"id":"job1"}`))
	cls := fakeWrite(s, "podcast_classify", ok(`{}`))
	rev := fakeWrite(s, "podcast_review", ok(`{}`))
	bk := fakeWrite(s, "transcode_backups", ok(`{}`))
	for _, p := range []string{"cancel", "resume", "retry", "podcast/classify", "podcast/review"} {
		for _, id := range []string{"other1", "nope"} {
			c, k := code(t, write(h, "POST", "/transcode/v1/actions/"+id+"/"+p, `{}`))
			if c != 404 || k != "action_not_found" {
				t.Errorf("%s %s: %d %s", id, p, c, k)
			}
		}
	}
	for _, p := range []string{"clean", "restore", "discard", "discard-duplicate"} {
		if c, _ := code(t, write(h, "POST", "/transcode/v1/backups/other1/"+p, `{"confirm":true}`)); c != 404 {
			t.Errorf("backups %s: %d", p, c)
		}
	}
	if c, _ := code(t, write(h, "POST", "/transcode/v1/backups/candidates/other1/discard", `{"confirm":true}`)); c != 404 {
		t.Error("candidate", c)
	}
	if cancel.n()+resume.n()+retry.n()+cls.n()+rev.n()+bk.n() != 0 {
		t.Fatal("a non-transcode action reached a tool")
	}
	// A transcode action does reach it, with what the body said.
	if w := write(h, "POST", "/transcode/v1/actions/job1/cancel", `{"reason":"not needed"}`); w.Code != 200 || cancel.last()["id"] != "job1" || cancel.last()["reason"] != "not needed" {
		t.Fatal(w.Code, cancel.last())
	}
	if w := write(h, "POST", "/transcode/v1/actions/job1/resume", `{"decision":"promote","plan_digest":"d1","inputs":{"a":1}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if a := resume.last(); a["plan_digest"] != "d1" || a["decision"] != "promote" || a["inputs"] != `{"a":1}` {
		t.Fatal(a)
	}
	if w := write(h, "POST", "/transcode/v1/actions/job1/retry", ``); w.Code != 200 || retry.n() != 1 {
		t.Fatal(w.Code)
	}
}

// The confirmations of the tools cannot be skipped through the API: a stale
// plan digest or review digest is a 409 and nothing is applied; the right one
// commits. The stale/right decision is the tool's, the API passes the value.
func TestTranscodeV1ConfirmationsArePassedThrough(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "job1", "promote_transcode_candidate", "waiting_decision", nil, nil, nil)
	applied := 0
	fakeWrite(s, "action_resume", func(a map[string]any) (string, bool) {
		if a["plan_digest"] != "right" {
			return "plan digest mismatch: the plan changed since it was shown", true
		}
		applied++
		return `{"id":"job1"}`, false
	})
	for _, body := range []string{`{"decision":"promote"}`, `{"decision":"promote","plan_digest":"wrong"}`} {
		if c, k := code(t, write(h, "POST", "/transcode/v1/actions/job1/resume", body)); c != 409 || k != "conflict" {
			t.Fatalf("%s: %d %s", body, c, k)
		}
	}
	if applied != 0 {
		t.Fatal("a missing or wrong digest committed")
	}
	if w := write(h, "POST", "/transcode/v1/actions/job1/resume", `{"decision":"promote","plan_digest":"right"}`); w.Code != 200 || applied != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	// podcast_review with a stale digest is a 409 as well.
	seedOperation(t, s.engine.Deps().Store, "pod1", "clean_podcast_ads", "waiting_decision", nil, nil, nil)
	fakeWrite(s, "podcast_review", func(a map[string]any) (string, bool) {
		if a["digest"] != "fresh" {
			return "review digest is stale: read the review again", true
		}
		return `{"approved":true}`, false
	})
	if c, k := code(t, write(h, "POST", "/transcode/v1/actions/pod1/podcast/review", `{"digest":"old","approve":true}`)); c != 409 || k != "conflict" {
		t.Fatal(c, k)
	}
	if w := write(h, "POST", "/transcode/v1/actions/pod1/podcast/review", `{"digest":"fresh","approve":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

// The goal change: GET (or POST with no mode) is the preview, POST with a mode
// needs the selection_version, and the idempotency key travels through.
func TestTranscodeV1GoalChangeFlow(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "batch1", "transcode_batch", "running", nil, nil, nil)
	seedOperation(t, s.engine.Deps().Store, "other1", "sync_something", "running", nil, nil, nil)
	rec := fakeWrite(s, "transcode_goal_change", func(a map[string]any) (string, bool) {
		if _, has := a["mode"]; !has {
			return `{"selection_version":"v7","preview":true}`, false
		}
		if a["selection_version"] != "v7" {
			return "selection_version mismatch: the selection changed, preview again", true
		}
		return `{"changed":true}`, false
	})
	if w := v1(h, "GET", "/transcode/v1/batches/batch1/goal?scope=unfinished", "", keyHdr()); w.Code != 200 || !strings.Contains(w.Body.String(), "v7") || rec.last()["scope"] != "unfinished" {
		t.Fatal(w.Code, w.Body.String(), rec.last())
	}
	if c, k := code(t, write(h, "POST", "/transcode/v1/batches/batch1/goal", `{"selection_version":"v7"}`)); c != 400 || k != "invalid_request" {
		t.Fatal("no mode", c, k)
	}
	if c, k := code(t, write(h, "POST", "/transcode/v1/batches/batch1/goal", `{"mode":"size","selection_version":"v1"}`)); c != 409 || k != "conflict" {
		t.Fatal("stale version", c, k)
	}
	hd := jsonHdr()
	hd["Idempotency-Key"] = "goal-1"
	w := v1(h, "POST", "/transcode/v1/batches/batch1/goal", `{"mode":"size","selection_version":"v7","allow_hardware":true,"size_budget_percent":60}`, hd)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	if a := rec.last(); a["idempotency_key"] != "goal-1" || a["selection_version"] != "v7" || a["allow_hardware"] != true || a["size_budget_percent"] != 60.0 || a["id"] != "batch1" {
		t.Fatal(a)
	}
	for _, m := range []string{"GET", "POST"} {
		if w := v1(h, m, "/transcode/v1/batches/other1/goal", `{"mode":"size"}`, jsonHdr()); w.Code != 404 {
			t.Errorf("%s non-transcode batch: %d", m, w.Code)
		}
	}
}

// Backups: discard (no verification of the replacement) and the leftover
// candidate need confirm true in the body; the rest pass straight through.
func TestTranscodeV1BackupsConfirmation(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "job1", "transcode_media", "completed", nil, nil, nil)
	rec := fakeWrite(s, "transcode_backups", func(a map[string]any) (string, bool) {
		if a["mode"] == "discard_candidate" && a["confirm"] != true {
			return "discard_candidate needs confirm=true; the candidate stays", true
		}
		return `{"done":true}`, false
	})
	for _, body := range []string{``, `{}`, `{"confirm":false}`} {
		if c, k := code(t, write(h, "POST", "/transcode/v1/backups/job1/discard", body)); c != 400 || k != "invalid_request" {
			t.Fatalf("discard %q: %d %s", body, c, k)
		}
	}
	if rec.n() != 0 {
		t.Fatal("discard without confirm reached the tool")
	}
	if w := write(h, "POST", "/transcode/v1/backups/job1/discard", `{"confirm":true}`); w.Code != 200 || rec.last()["mode"] != "discard" || rec.last()["action_id"] != "job1" {
		t.Fatal(w.Code, rec.last())
	}
	// The candidate flow: the tool itself judges the missing confirm, and its
	// refusal is a bad parameter (400), never a conflict.
	if c, k := code(t, write(h, "POST", "/transcode/v1/backups/candidates/job1/discard", `{"confirm":false}`)); c != 400 || k != "invalid_request" || rec.last()["confirm"] != false {
		t.Fatal("candidate without confirm", c, k, rec.last())
	}
	// clean, restore and discard-duplicate take no body fields.
	for _, seg := range []string{"clean", "restore", "discard-duplicate"} {
		if c, k := code(t, write(h, "POST", "/transcode/v1/backups/job1/"+seg, `{"confirm":true}`)); c != 400 || k != "invalid_request" {
			t.Errorf("%s with a confirm field: %d %s", seg, c, k)
		}
	}
	if w := write(h, "POST", "/transcode/v1/backups/candidates/job1/discard", `{"confirm":true}`); w.Code != 200 || rec.last()["mode"] != "discard_candidate" || rec.last()["confirm"] != true {
		t.Fatal(w.Code, rec.last())
	}
	for seg, mode := range map[string]string{"clean": "clean", "restore": "restore", "discard-duplicate": "discard_duplicate"} {
		if w := write(h, "POST", "/transcode/v1/backups/job1/"+seg, ``); w.Code != 200 || rec.last()["mode"] != mode {
			t.Errorf("%s: %d %v", seg, w.Code, rec.last())
		}
	}
	// A switch the server keeps off is a 403, not a 400.
	fakeWrite(s, "transcode_backups", func(map[string]any) (string, bool) {
		return "destructive changes need allow_destructive on this server", true
	})
	if c, k := code(t, write(h, "POST", "/transcode/v1/backups/job1/discard", `{"confirm":true}`)); c != 403 || k != "forbidden" {
		t.Fatal(c, k)
	}
}

// Recipes: the expected generation and digest are required where the tool
// requires them, and a stale pair is the tool's 409.
func TestTranscodeV1RecipeWritesKeepTheirGuards(t *testing.T) {
	s, h := transcodeUI(t)
	save := fakeWrite(s, "recipe_save", func(a map[string]any) (string, bool) {
		if a["expected_digest"] != "d1" {
			return "expected_digest does not match the current recipe", true
		}
		return `{"saved":true}`, false
	})
	del := fakeWrite(s, "recipe_delete", ok(`{"deleted":true}`))
	if c, _ := code(t, write(h, "DELETE", "/transcode/v1/recipes/x", `{"expected_digest":"d1"}`)); c != 400 {
		t.Fatal("delete without generation", c)
	}
	if c, _ := code(t, write(h, "DELETE", "/transcode/v1/recipes/x", ``)); c != 400 {
		t.Fatal("delete without body", c)
	}
	if del.n() != 0 {
		t.Fatal("an unguarded delete reached the tool")
	}
	if w := write(h, "DELETE", "/transcode/v1/recipes/x", `{"expected_generation":3,"expected_digest":"d1"}`); w.Code != 200 || del.last()["expected_generation"] != 3.0 || del.last()["name"] != "x" {
		t.Fatal(w.Code, del.last())
	}
	if c, _ := code(t, write(h, "PUT", "/transcode/v1/recipes/x", `{"description":"d"}`)); c != 400 {
		t.Fatal("save without profile", c)
	}
	if c, k := code(t, write(h, "PUT", "/transcode/v1/recipes/x", `{"profile":{"a":1},"expected_digest":"old"}`)); c != 409 || k != "conflict" {
		t.Fatal(c, k)
	}
	if w := write(h, "PUT", "/transcode/v1/recipes/x", `{"profile":{"a":1},"expected_generation":2,"expected_digest":"d1"}`); w.Code != 200 || save.last()["profile"] != `{"a":1}` || save.last()["expected_generation"] != 2.0 {
		t.Fatal(w.Code, save.last())
	}
	// A recipe the tool does not know is the recipe 404.
	fakeWrite(s, "recipe_save", func(map[string]any) (string, bool) { return "recipe not found", true })
	if c, k := code(t, write(h, "PUT", "/transcode/v1/recipes/x", `{"profile":{}}`)); c != 404 || k != "recipe_not_found" {
		t.Fatal(c, k)
	}
}

// The bundle tools answer 200 with a flag; a refusal must not look like a
// success, and a rollback without both digests never reaches the tool.
func TestTranscodeV1RecipeBundle(t *testing.T) {
	s, h := transcodeUI(t)
	rb := fakeWrite(s, "recipe_rollback", func(a map[string]any) (string, bool) {
		if a["expected_active_digest"] == "a1" && a["expected_previous_digest"] == "p1" {
			return `{"rolled_back":true}`, false
		}
		return `{"rolled_back":false,"error":"the active digest changed"}`, false
	})
	for _, body := range []string{``, `{}`, `{"expected_active_digest":"a1"}`, `{"expected_previous_digest":"p1"}`} {
		if c, k := code(t, write(h, "POST", "/transcode/v1/recipe-bundle/rollback", body)); c != 400 || k != "invalid_request" {
			t.Fatalf("%q: %d %s", body, c, k)
		}
	}
	if rb.n() != 0 {
		t.Fatal("a rollback without both digests reached the tool")
	}
	if c, k := code(t, write(h, "POST", "/transcode/v1/recipe-bundle/rollback", `{"expected_active_digest":"zz","expected_previous_digest":"p1"}`)); c != 409 || k != "conflict" {
		t.Fatal(c, k)
	}
	if w := write(h, "POST", "/transcode/v1/recipe-bundle/rollback", `{"expected_active_digest":"a1","expected_previous_digest":"p1"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	fakeWrite(s, "recipe_update", ok(`{"activated":false,"error":"recipe bundle has no profiles"}`))
	if c, k := code(t, write(h, "POST", "/transcode/v1/recipe-bundle/update", ``)); c != 422 || k != "rejected" {
		t.Fatal(c, k)
	}
	fakeWrite(s, "recipe_reload", ok(`{"activated":true}`))
	if w := write(h, "POST", "/transcode/v1/recipe-bundle/reload", ``); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

// Workers: pause, drain and slots go through the console's handlers.
func TestTranscodeV1WorkerWrites(t *testing.T) {
	_, h := transcodeUI(t)
	const name = transcode.DefaultWorkerName
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"PUT", "/transcode/v1/pauses/nodeZ", `{"mode":"graceful"}`, 404},
		{"DELETE", "/transcode/v1/pauses/nodeZ", ``, 404},
		{"PUT", "/transcode/v1/workers/nodeZ/slots", `{}`, 404},
		{"PUT", "/transcode/v1/pauses/" + name, `{}`, 400},
		{"PUT", "/transcode/v1/pauses/" + name, `{"mode":"graceful","builds_paused":true}`, 400},
		{"PUT", "/transcode/v1/pauses/" + name, `{"mode":"sideways"}`, 400},
		{"PUT", "/transcode/v1/pauses/" + name, `{"mode":"graceful","x":1}`, 400},
		{"PUT", "/transcode/v1/drain", `{"enabled":true}`, 200},
		{"PUT", "/transcode/v1/drain", `{"enabled":false}`, 200},
	} {
		if w := write(h, tc.method, tc.path, tc.body); w.Code != tc.code {
			t.Errorf("%s %s %s: %d %s, want %d", tc.method, tc.path, tc.body, w.Code, w.Body.String(), tc.code)
		}
	}
	// The drain really changed: the read says so.
	write(h, "PUT", "/transcode/v1/drain", `{"enabled":true}`)
	if c, e := get(t, h, "/transcode/v1/drain", keyHdr()); c != 200 || e["enabled"] != true && e["drain"] != true {
		t.Fatal(c, e)
	}
	write(h, "PUT", "/transcode/v1/drain", `{"enabled":false}`)
}

// Review queue: closing validates the outcome, the decision goes to
// decision_record with the entry id and answers 201.
func TestTranscodeV1ReviewQueueWrites(t *testing.T) {
	s, h := transcodeUI(t)
	rec := fakeWrite(s, "decision_record", ok(`{"recorded":true}`))
	for _, tc := range []struct{ path, body string }{
		{"/transcode/v1/review-queue/abc/close", `{"outcome":"processed"}`},
		{"/transcode/v1/review-queue/0/close", `{"outcome":"processed"}`},
		{"/transcode/v1/review-queue/7/close", `{"outcome":"sideways"}`},
		{"/transcode/v1/review-queue/7/close", `{}`},
		{"/transcode/v1/review-queue/abc/decisions", `{"title":"Example Show","decision":"approved"}`},
	} {
		if c, k := code(t, write(h, "POST", tc.path, tc.body)); c != 400 || k != "invalid_request" {
			t.Errorf("%s %s: %d %s", tc.path, tc.body, c, k)
		}
	}
	// An id that is not in the queue is the queue's own 404.
	if c, _ := code(t, write(h, "POST", "/transcode/v1/review-queue/999999/close", `{"outcome":"dismissed"}`)); c != 404 {
		t.Error("unknown entry", c)
	}
	w := write(h, "POST", "/transcode/v1/review-queue/7/decisions", `{"title":"Example Show","decision":"selected","release_group":"GroupA","size":1234,"reasons":["fits"]}`)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	a := rec.last()
	if a["maintenance_item_id"] != "7" || a["decision"] != "selected" || a["release_group"] != "GroupA" || a["size"] != 1234.0 || len(a["reasons"].([]any)) != 1 {
		t.Fatal(a)
	}
}

// Podcast writes: classify takes the classification as the body, the ad
// reference revoke names the entry.
func TestTranscodeV1PodcastWrites(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "pod1", "clean_podcast_ads", "waiting_decision", nil, nil, nil)
	cls := fakeWrite(s, "podcast_classify", ok(`{"accepted":true}`))
	if c, k := code(t, write(h, "POST", "/transcode/v1/actions/pod1/podcast/classify", `[1,2]`)); c != 400 || k != "invalid_request" {
		t.Fatal(c, k)
	}
	if w := write(h, "POST", "/transcode/v1/actions/pod1/podcast/classify", `{"blocks":[{"id":"b1","label":"ad"}]}`); w.Code != 200 || !strings.Contains(cls.last()["classification"].(string), "b1") {
		t.Fatal(w.Code, cls.last())
	}
	lib := fakeWrite(s, "podcast_ad_library", ok(`{"revoked":true}`))
	s.cfg.Podcasts.Enabled = true
	s.cfg.Podcasts.Podcasts = map[string]config.PodcastSettings{"p1": {Enabled: true}}
	if c, k := code(t, write(h, "DELETE", "/transcode/v1/podcasts/nope/ad-library/e1", ``)); c != 404 || k != "unknown_podcast" {
		t.Fatal(c, k)
	}
	if w := write(h, "DELETE", "/transcode/v1/podcasts/p1/ad-library/e1", ``); w.Code != 200 || lib.last()["revoke"] != "e1" || lib.last()["podcast_id"] != "p1" {
		t.Fatal(w.Code, w.Body.String(), lib.last())
	}
	s.cfg.Podcasts.Enabled = false
	if c, k := code(t, write(h, "DELETE", "/transcode/v1/podcasts/p1/ad-library/e1", ``)); c != 503 || k != "unavailable" {
		t.Fatal(c, k)
	}
	// podcast_review: the offset reaches the tool as the number every tool reads.
	rev := fakeWrite(s, "podcast_review", ok(`{}`))
	seedOperation(t, s.engine.Deps().Store, "pod2", "clean_podcast_ads", "waiting_decision", nil, nil, nil)
	if w := write(h, "POST", "/transcode/v1/actions/pod2/podcast/review", `{"digest":"d","offset":32}`); w.Code != 200 || rev.last()["offset"] != 32.0 {
		t.Fatal(w.Code, rev.last())
	}
	if c, _ := code(t, write(h, "POST", "/transcode/v1/actions/pod2/podcast/review", `{"offset":-1}`)); c != 400 {
		t.Fatal("negative offset", c)
	}
}

// Every write takes a JSON object: null, an array or a scalar is a 400, even
// for the writes that otherwise take no body.
func TestTranscodeV1WriteBodiesMustBeObjects(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "job1", "transcode_media", "running", nil, nil, nil)
	cancel := fakeWrite(s, "action_cancel", ok(`{}`))
	retry := fakeWrite(s, "action_retry", ok(`{}`))
	for _, body := range []string{`null`, `[]`, `[{}]`, `"x"`, `5`, `true`} {
		for _, rt := range [][2]string{
			{"POST", "/transcode/v1/actions"}, {"POST", "/transcode/v1/actions/job1/cancel"}, {"POST", "/transcode/v1/actions/job1/retry"},
			{"PUT", "/transcode/v1/drain"}, {"PUT", "/transcode/v1/pauses/" + transcode.DefaultWorkerName}, {"DELETE", "/transcode/v1/pauses/" + transcode.DefaultWorkerName},
			{"POST", "/transcode/v1/recipe-bundle/reload"}, {"DELETE", "/transcode/v1/recipes/x"}, {"PUT", "/transcode/v1/workers/" + transcode.DefaultWorkerName + "/slots"},
		} {
			if c, k := code(t, write(h, rt[0], rt[1], body)); c != 400 || k != "invalid_request" {
				t.Errorf("%s %s %s: %d %s", rt[0], rt[1], body, c, k)
			}
		}
	}
	if cancel.n()+retry.n() != 0 {
		t.Fatal("a non-object body reached a tool")
	}
}

// transcodeSlotUI is the transcode API over a worker that can divide its
// slots (the fake the console's own slot tests use), so a slot write reaches
// the console handler.
func transcodeSlotUI(t *testing.T, worker transcode.Executor) (*Server, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	path := filepath.Join(t.TempDir(), "transcode-token")
	if err := os.WriteFile(path, []byte(testTranscodeKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Web: config.WebConfig{Enabled: true, Token: testToken, TranscodeTokenFile: path}}
	engine := action.NewEngine(action.EngineDeps{Store: st, Config: cfg, Transcode: worker})
	s, err := New(cfg, arrservice.NewRegistry(cfg), engine, server.NewMCPServer("test", "1"))
	if err != nil {
		t.Fatal(err)
	}
	return s, s.Handler()
}

// The slot write needs all three fields; with a worker that can divide its
// slots the request then reaches the console handler and its own rules (the
// stale-revision 409, the stored settings, 0/0 back to shared).
func TestTranscodeV1SlotsRequireTheirFields(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 3, cpus: 10}
	s, h := transcodeSlotUI(t, worker)
	const p = "/transcode/v1/workers/" + transcode.DefaultWorkerName + "/slots"
	st := s.engine.Deps().Store
	stored := func() *store.WorkerSlotSettings {
		row, err := st.GetWorkerSlotSettings(transcode.DefaultWorkerName)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	for _, body := range []string{`{}`, `{"revision":0,"x265_slots":2}`, `{"video_slots":2,"podcast_slots":1}`, `{"video_slots":2,"revision":0}`, `{"video_slots":null,"podcast_slots":null,"revision":null}`} {
		if c, k := code(t, write(h, "PUT", p, body)); c != 400 || k != "invalid_request" {
			t.Errorf("%s: %d %s", body, c, k)
		}
	}
	if worker.puts != 0 || stored() != nil {
		t.Fatal("a refused request reached the worker or the store", worker.puts, stored())
	}
	// A stale revision is the console handler's own 409, with its message.
	w := write(h, "PUT", p, `{"video_slots":2,"podcast_slots":1,"revision":99}`)
	if c, k := code(t, w); c != 409 || k != "conflict" || !strings.Contains(w.Body.String(), "The worker slot settings changed; refresh and retry.") {
		t.Fatal("stale revision", w.Code, w.Body.String())
	}
	if worker.puts != 0 || stored() != nil {
		t.Fatal("a stale slot change was stored or pushed", worker.puts, stored())
	}
	// A valid body is stored and pushed.
	if w := write(h, "PUT", p, `{"video_slots":2,"podcast_slots":1,"revision":0}`); w.Code != 200 {
		t.Fatal("valid slots", w.Code, w.Body.String())
	}
	row := stored()
	if row == nil || row.VideoSlots != 2 || row.PodcastSlots != 1 || row.Revision != 1 || worker.puts != 1 {
		t.Fatal("slots not stored", row, worker.puts)
	}
	// 0 and 0 return the worker to shared slots; the old revision now fails.
	if c, _ := code(t, write(h, "PUT", p, `{"video_slots":0,"podcast_slots":0,"revision":0}`)); c != 409 {
		t.Fatal("old revision after a change", c)
	}
	if w := write(h, "PUT", p, `{"video_slots":0,"podcast_slots":0,"revision":1}`); w.Code != 200 {
		t.Fatal("shared slots", w.Code, w.Body.String())
	}
	if row := stored(); row == nil || !row.Shared() || row.Revision != 2 {
		t.Fatal("not shared again", row)
	}
}

// A worker that cannot divide its slots is refused by the API before the
// console handler, with its own message.
func TestTranscodeV1SlotsRefuseAWorkerThatCannotDivideThem(t *testing.T) {
	_, h := transcodeSlotUI(t, struct{ transcode.Executor }{})
	const p = "/transcode/v1/workers/" + transcode.DefaultWorkerName + "/slots"
	w := write(h, "PUT", p, `{"video_slots":0,"podcast_slots":0,"revision":0}`)
	if c, k := code(t, w); c != 409 || k != "conflict" || !strings.Contains(w.Body.String(), "slot assignment requires the HTTP video worker") {
		t.Fatal(w.Code, w.Body.String())
	}
}

// Pausing a worker through the API changes what the read reports.
func TestTranscodeV1PauseWritesChangeTheState(t *testing.T) {
	_, h := transcodeUI(t)
	p := "/transcode/v1/pauses/" + transcode.DefaultWorkerName
	mode := func() string {
		_, e := get(t, h, p, keyHdr())
		m, _ := e["mode"].(string)
		return m
	}
	if mode() != "" {
		t.Fatal("starts paused", mode())
	}
	if w := write(h, "PUT", p, `{"mode":"graceful"}`); w.Code != 200 || mode() != "graceful" {
		t.Fatal(w.Code, w.Body.String(), mode())
	}
	if w := write(h, "DELETE", p, ``); w.Code != 200 || mode() != "" {
		t.Fatal(w.Code, w.Body.String(), mode())
	}
	if w := write(h, "PUT", p, `{"mode":"immediate"}`); w.Code != 200 || mode() != "immediate" {
		t.Fatal(w.Code, w.Body.String(), mode())
	}
	if w := write(h, "PUT", p, `{"mode":"off"}`); w.Code != 200 || mode() != "" {
		t.Fatal(w.Code, w.Body.String(), mode())
	}
	if w := write(h, "PUT", p, `{"builds_paused":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	write(h, "PUT", p, `{"builds_paused":false}`)
}

// The goal change with the REAL tool: a mode without the reviewed
// selection_version (and idempotency key) is refused as a bad request and
// nothing is applied.
func TestTranscodeV1GoalChangeRealToolRefusesWithoutAVersion(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "batch1", "transcode_batch", "running", map[string]any{"mode": "size"}, nil, nil)
	for _, body := range []string{`{"mode":"size"}`, `{"mode":"size","idempotency_key":"k1"}`, `{"mode":"size","selection_version":"v1"}`} {
		w := write(h, "POST", "/transcode/v1/batches/batch1/goal", body)
		c, k := code(t, w)
		if c != 400 || k != "invalid_request" || !strings.Contains(w.Body.String(), "selection_version") || !strings.Contains(w.Body.String(), "idempotency_key") {
			t.Errorf("%s: %d %s %s", body, c, k, w.Body.String())
		}
	}
	inst, _ := s.engine.Deps().Store.GetActionInstance("batch1")
	if inst == nil || inst.Status != "running" {
		t.Fatal("a refused goal change touched the batch", inst)
	}
}

// Only an authorized write that succeeded empties the caches; an anonymous or
// refused one, and the spec path, do not.
func TestTranscodeV1CachesAreInvalidatedOnlyAfterASuccessfulWrite(t *testing.T) {
	s, h := transcodeUI(t)
	gen := func() uint64 { return s.arrLists.generation.Load() }
	g := gen()
	v1(h, "POST", transcodeapi.SpecPath, `{}`, nil)
	v1(h, "POST", transcodeapi.SpecPath, `{}`, keyHdr())
	v1(h, "PUT", "/transcode/v1/drain", `{"enabled":true}`, nil)
	write(h, "PUT", "/transcode/v1/drain", `{"bad":1}`)
	if gen() != g {
		t.Fatal("a refused request emptied the caches")
	}
	if w := write(h, "PUT", "/transcode/v1/drain", `{"enabled":false}`); w.Code != 200 || gen() == g {
		t.Fatal(w.Code, "a successful write left the caches")
	}
}

// A write refusal keeps its meaning: stale state is 409, a bad or missing
// parameter 400, a missing thing 404, an unreachable service 503.
// TestWriteToolWriteErrorClassification pins the status of the real refusal
// texts the write tools and the engine produce (file:line of each source in
// the comment). A refusal due to the CURRENT STATE of the thing is 409; a
// missing or bad parameter is 400; a worker that cannot take the job is 503.
func TestWriteToolWriteErrorClassification(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want int
		src  string
	}{
		// 409: revision, digest and version guards.
		{`managed profile "x" changed: expected generation 2, current 3`, 409, "transcode/recipe/managed.go:287"},
		{`managed profile "x" already exists; expected_generation is required to update it`, 409, "transcode/recipe/managed.go:284"},
		{`managed profile "x" does not exist; expected_generation/expected_digest cannot create it`, 409, "transcode/recipe/managed.go:296"},
		{`recipe version conflict: reviewed active or previous bundle changed; review current versions before restoring`, 409, "transcode/recipe/manager.go:124"},
		{`recipe version 1 already exists with a different digest (immutable version collision)`, 409, "transcode/recipe/manager.go:189"},
		{`no previous valid cached recipe bundle available`, 409, "transcode/recipe/manager.go:161"},
		{`batch selection changed; reopen the settings`, 409, "action/batch_revision.go:220"},
		{`cuts approval is missing or stale`, 409, "action/podcast.go:261"},
		{`approval requires current waiting review and exact cuts digest`, 409, "action/podcast_tools.go:212"},
		{`reclassification requires current waiting podcast review`, 409, "action/podcast_tools.go:310"},
		{`reclassification requires the exact current cuts digest`, 409, "action/podcast_tools.go:319"},
		{`podcast classification/review requires a waiting podcast action`, 409, "action/podcast_tools.go:34"},
		{`classification is frozen; start a new action to change policy/transcript`, 409, "action/podcast_tools.go:135"},
		{`read every podcast_block page before classifying (b1 unit 3 unread)`, 409, "action/podcast_tools.go:159"},
		{`cuts are not planned yet`, 409, "action/podcast_tools.go:201"},
		{`read every cut review page before approving`, 409, "action/podcast_tools.go:216"},
		// 409: the action or the recovery copy is in the wrong state.
		{`action already completed; cancellation was not applied`, 409, "action/engine.go:816"},
		{`action_resume blocked: candidate validation changed; review it again`, 409, "tools/action_tools.go:496"},
		{`action_retry blocked: only failed actions can be retried (current status: running)`, 409, "tools/action_tools.go:437, action/engine.go:500"},
		{`only failed actions can be retried (current status: running)`, 409, "action/engine.go:500"},
		{`this attempt's candidate was removed after it was kept; start a new transcode instead`, 409, "action/engine.go:519"},
		{`this replacement belongs to batch b1, which has failed; retry the batch instead`, 409, "action/engine.go:611"},
		{`transcode inputs are immutable after action creation; resume accepts only a decision`, 409, "action/engine.go:671"},
		{`idempotency key "k" already belongs to active transcode action a1 with different immutable inputs`, 409, "action/engine.go:277"},
		{`submission key already belongs to different immutable inputs`, 409, "action/enqueue.go:113"},
		{`only an automatic processing batch can change goals`, 409, "tools/transcode_goal_tools.go:46"},
		{`only a finished promotion's recovery copy can be removed`, 409, "action/backup_discard.go:25"},
		{`recovery copy changed before removal; stopped`, 409, "action/backup_discard.go:82"},
		{`recovery artifact is not a regular file`, 409, "action/backup_discard.go:56"},
		{`only a failed or cancelled promotion's original can be moved back`, 409, "action/backup_restore.go:59"},
		{`the original was never moved aside; nothing to move back`, 409, "action/backup_restore.go:19"},
		{`the library path is not empty; the original stays at its recovery path`, 409, "action/backup_restore.go:32"},
		{`only failed promotions can discard a duplicate recovery copy`, 409, "action/backup_duplicate.go:15"},
		{`library command outcome is unresolved; recovery retained`, 409, "action/backup_duplicate.go:29"},
		{`this attempt's candidate can still be promoted or the action is not finished; nothing was removed`, 409, "action/candidate_cleanup.go:270"},
		{`candidate file kept: the worker did not confirm that the job stopped`, 409, "action/candidate_cleanup.go:273 (worker-stop refusal)"},
		// 500: a fault of the server (I/O, database); the cause stays hidden.
		{`could not remove candidate file: remove /x/y: permission denied`, 500, "action/candidate_cleanup.go:295 (candidateKeptError)"},
		{`candidate file kept: x is not a regular file`, 409, "action/candidate_cleanup.go:295, :136 (a guard, not an I/O fault)"},
		{`write state: database is locked`, 500, "store (SQLite)"},
		{`write /x: no space left on device`, 500, "operating system"},
		{`only a finished transcode attempt's own candidate can be dropped`, 409, "action/candidate_cleanup.go:310"},
		// 400: a missing or bad parameter.
		{`discard_candidate needs confirm=true; the candidate stays`, 400, "tools/transcode_backup_tools.go:40"},
		{`action_id from transcode_backups list is required for clean`, 400, "tools/transcode_backup_tools.go:56"},
		{`mode must be list, candidates, discard_candidate, clean, discard_duplicate, discard or restore`, 400, "tools/transcode_backup_tools.go:74"},
		{`both expected_active_digest and expected_previous_digest are required for reviewed restoration`, 400, "transcode/recipe/manager.go:116"},
		{`approval must carry the digest of the reviewed replacement plan (plan_digest)`, 400, "action/promote_plan_digest.go:73"},
		{`reviewed selection_version and idempotency_key are required`, 400, "tools/transcode_goal_tools.go"},
		{`expected_generation is required to delete managed profile "x"`, 400, "transcode/recipe/managed.go:363"},
		{`expected_digest is required to delete managed profile "x"`, 400, "transcode/recipe/managed.go:366"},
		{`maintenance_item_id and title are required`, 400, "store/store.go:986"},
		{`decision must be selected, rejected or alternate, got "x"`, 400, "store/store.go:991"},
		{`id is required`, 400, "tools/action_tools.go:432"},
		{`invalid action_run inputs: bad input`, 400, "tools/action_tools.go:374"},
		{`invalid block offset`, 400, "action/podcast_tools.go:49"},
		{`invalid cut offset`, 400, "action/podcast_tools.go:204"},
		// 404, 403 and 503.
		{`action instance "a" not found`, 404, "tools/action_tools.go:592"},
		{`managed profile "x" not found`, 404, "transcode/recipe/managed.go:362"},
		{`discard needs allow_destructive on this server`, 403, "action/backup_duplicate.go:44"},
		{`allow_destructive must be enabled for backup removal`, 403, "action/backup_discard.go:13"},
		{`node m1 unreachable`, 503, "maintenanceui"},
		{`video worker is offline or not ready; no job was submitted`, 503, "action/worker_admission.go:170"},
		{`this job's worker "w" is not configured on this server; nothing was sent`, 503, "action/worker_admission.go:113"},
	} {
		w := httptest.NewRecorder()
		writeToolWriteError(w, c.msg, v1WriteErr{})
		if w.Code != c.want {
			t.Errorf("%q (%s): %d, want %d", c.msg, c.src, w.Code, c.want)
		}
	}
}

// A value the caller sent and a tool echoes back must not pick the status, and
// a word inside a longer one ("unchanged", "unblocked") is not the word.
func TestWriteToolWriteErrorIgnoresEchoedValuesAndInnerWords(t *testing.T) {
	for msg, want := range map[string]int{
		`decision must be selected, rejected or alternate, got "uncertain"`:         400,
		`decision must be selected, rejected or alternate, got "already"`:           400,
		`decision must be selected, rejected or alternate, got "blocked"`:           400,
		`decision must be selected, rejected or alternate, got "allow_destructive"`: 400,
		`decision must be selected, rejected or alternate, got "unreachable"`:       400,
		`decision must be selected, rejected or alternate, got 'stale'`:             400,
		"decision must be selected, rejected or alternate, got `conflict`":          400,
		`decision must be selected, rejected or alternate, got "not found"`:         400,
		`the profile is unchanged and the worker unblocked`:                         400,
		`this attempt's candidate was removed after it was kept; that's it`:         409,
		`managed profile "a b" changed: expected generation 2, current 3`:           409,
	} {
		w := httptest.NewRecorder()
		writeToolWriteError(w, msg, v1WriteErr{})
		if w.Code != want {
			t.Errorf("%q: %d, want %d", msg, w.Code, want)
		}
	}
}

// The decision is checked before the tool is called.
func TestTranscodeV1DecisionIsValidatedBeforeTheTool(t *testing.T) {
	_, h := transcodeUI(t)
	for _, d := range []string{"uncertain", "already", "blocked", "allow_destructive", "unreachable", ""} {
		w := write(h, "POST", "/transcode/v1/review-queue/1/decisions", `{"title":"Example Show","decision":"`+d+`"}`)
		if c, k := code(t, w); c != 400 || k != "invalid_request" {
			t.Errorf("decision %q: %d %s %s", d, c, k, w.Body.String())
		}
	}
}

// What recipe_reload and recipe_update could not activate is presented by its
// cause: a fault of the server is a hidden 500, an unreachable source 503, a
// version that exists with other content 409, and only what the bundle itself
// got wrong is the 422 rejection.
func TestBundleRefusalClassification(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want int
		code string
	}{
		{`recipe version 1 already exists with a different digest (immutable version collision)`, 409, "conflict"},
		{`cached recipe version 1 is corrupt: parsing recipe bundle: x`, 500, "internal"},
		{`rename /cache/a /other/b: invalid cross-device link`, 500, "internal"},
		{`open /cache/x: too many open files`, 500, "internal"},
		{`bundle archive is corrupt`, 500, "internal"},
		{`something nobody anticipated`, 500, "internal"},
		{`profile "x": unsupported container "avi"`, 422, "rejected"},
		{`container mkv: invalid conversion reason "q"`, 422, "rejected"},
		{`open /cache/1/bundle.yaml: permission denied`, 500, "internal"},
		{`mkdir /cache/1: no space left on device`, 500, "internal"},
		{`reading recipe file: open /x: no such file or directory`, 503, "unavailable"},
		{`fetching recipe manifest: HTTP 502`, 503, "unavailable"},
		{`fetching recipe bundle: Get "https://example.invalid/b": timeout`, 503, "unavailable"},
		{`recipe bundle sha256 mismatch: expected a got b`, 422, "rejected"},
		{`unsupported recipe schema_version 9 (supported: 1-2)`, 422, "rejected"},
		{`parsing recipe bundle: bad yaml`, 422, "rejected"},
	} {
		w := httptest.NewRecorder()
		bundleRefusal(w, c.msg)
		var e struct{ Code, Error string }
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != c.want || e.Code != c.code {
			t.Errorf("%q: %d %s, want %d %s", c.msg, w.Code, e.Code, c.want, c.code)
		}
		if c.want == 500 && (strings.Contains(w.Body.String(), "/cache") || strings.Contains(w.Body.String(), "permission")) {
			t.Errorf("%q: a 500 carries its cause: %s", c.msg, w.Body.String())
		}
	}
}

// Escaped quotes of %q must not end a quoted value early or swallow what
// follows it.
func TestWriteToolWriteErrorEscapedQuotes(t *testing.T) {
	for msg, want := range map[string]int{
		`decision must be selected, got "a\" permission denied \"b"`:        400,
		`decision must be selected, got "a\" already \" unreachable"`:       400,
		`managed profile "a\"" changed: expected digest "v1", current "v2"`: 409,
		`managed profile "a\\" changed: expected generation 2, current 3`:   409,
		`write /x: permission denied`:                                       500,
	} {
		w := httptest.NewRecorder()
		writeToolWriteError(w, msg, v1WriteErr{})
		if w.Code != want {
			t.Errorf("%q: %d, want %d", msg, w.Code, want)
		}
	}
}

// A 5xx never carries its cause, a 503 included: a path, a worker name or a
// URL in the refusal stays in the log.
func TestWriteToolWriteErrorFiveHundredsHideTheCause(t *testing.T) {
	for _, msg := range []string{
		`this job's worker "nodeA" is not configured on this server; nothing was sent`,
		`video worker is offline or not ready; no job was submitted`,
		`node m1 unreachable at https://worker.example.invalid/v1/x`,
		`podcast template unavailable`,
		`could not remove candidate file: remove /media/x/secret.mkv: permission denied`,
	} {
		w := httptest.NewRecorder()
		writeToolWriteError(w, msg, v1WriteErr{})
		if w.Code < 500 {
			t.Errorf("%q: %d", msg, w.Code)
		}
		for _, leak := range []string{"nodeA", "worker.example", "/media", "secret", "m1", "nothing was sent", "job was submitted"} {
			if strings.Contains(w.Body.String(), leak) {
				t.Errorf("%q: the %d carries %q: %s", msg, w.Code, leak, w.Body.String())
			}
		}
	}
	for _, msg := range []string{`reading recipe file: open /etc/recipes/x.yaml: no such file or directory`, `fetching recipe manifest: HTTP 502 from https://example.invalid/manifest.json`} {
		w := httptest.NewRecorder()
		bundleRefusal(w, msg)
		if w.Code != 503 || strings.Contains(w.Body.String(), "/etc") || strings.Contains(w.Body.String(), "example.invalid") {
			t.Errorf("%q: %d %s", msg, w.Code, w.Body.String())
		}
	}
}

// Words match whole: a word that merely starts like one is not it, and an
// operating-system fault is never a 404 or a 409 with its cause.
func TestWriteToolWriteErrorWholeWordsAndOrder(t *testing.T) {
	for msg, want := range map[string]int{
		`the selection has blockedness`:                        400,
		`the digests are mismatched in spirit`:                 400,
		`it is staleness itself`:                               400,
		`upstream said http 5030 oddly`:                        400,
		`query failed: no such table: transcode_batch_items`:   500,
		`open /x: no such file or directory`:                   500,
		`the thing was not found`:                              404,
		`discard needs allow_destructive on this server`:       403,
		`"allow_destructive" must be enabled for removal`:      403,
		`allow_destructive must be enabled for backup removal`: 403,
		`bad value got "allow_destructive"`:                    400,
	} {
		w := httptest.NewRecorder()
		writeToolWriteError(w, msg, v1WriteErr{})
		if w.Code != want {
			t.Errorf("%q: %d, want %d", msg, w.Code, want)
		}
	}
	w := httptest.NewRecorder()
	bundleRefusal(w, `odd failure http 5030`)
	if w.Code != 500 {
		t.Errorf("http 5030 is no http status: %d", w.Code)
	}
}

// The enumerated fields of the goal and the resume are checked before the tool.
func TestTranscodeV1EnumeratedFieldsAreCheckedBeforeTheTool(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "batch1", "transcode_batch", "running", map[string]any{"mode": "size"}, nil, nil)
	for _, body := range []string{`{"mode":"unreachable","selection_version":"v","idempotency_key":"k"}`, `{"mode":"size","scope":"already","selection_version":"v","idempotency_key":"k"}`, `{"mode":"Size","selection_version":"v","idempotency_key":"k"}`} {
		w := write(h, "POST", "/transcode/v1/batches/batch1/goal", body)
		if c, k := code(t, w); c != 400 || k != "invalid_request" {
			t.Errorf("%s: %d %s %s", body, c, k, w.Body.String())
		}
	}
	for _, d := range []string{`blocked now`, `Resume`, `already"x`} {
		w := write(h, "POST", "/transcode/v1/actions/batch1/resume", `{"decision":"`+strings.ReplaceAll(d, `"`, `\"`)+`"}`)
		if c, k := code(t, w); c != 400 || k != "invalid_request" {
			t.Errorf("decision %q: %d %s %s", d, c, k, w.Body.String())
		}
	}
}

// The destructive switch is matched in its own phrasings only: an internal
// fault wins over it, a quoted bare switch name still counts, a phrase the
// caller sent never does.
func TestWriteToolWriteErrorDestructiveSwitchAndOrder(t *testing.T) {
	for msg, want := range map[string]int{
		`bad value got "requires allow_destructive"`:                          400,
		`bad value got "needs allow_destructive" and more`:                    400,
		`discard requires "allow_destructive"`:                                403,
		"discard requires `allow_destructive`":                                403,
		`allow_destructive must be enabled for backup removal`:                403,
		`write /x: permission denied; needs allow_destructive on this server`: 500,
		`allow_destructive_mode must be enabled`:                              400,
	} {
		w := httptest.NewRecorder()
		writeToolWriteError(w, msg, v1WriteErr{})
		if w.Code != want {
			t.Errorf("%q: %d, want %d", msg, w.Code, want)
		}
	}
	w := httptest.NewRecorder()
	writeToolWriteError(w, `recipe_rollback failed: cached recipe version 3 is corrupt: parsing recipe bundle: x`, v1WriteErr{})
	if w.Code != 500 || strings.Contains(w.Body.String(), "corrupt") {
		t.Error("a corrupt cache on rollback", w.Code, w.Body.String())
	}
}

// The goal preview and the change take the same scope, and the size budget
// source is the one value the tool knows.
func TestTranscodeV1GoalScopeAndBudgetSourceAreChecked(t *testing.T) {
	s, h := transcodeUI(t)
	seedOperation(t, s.engine.Deps().Store, "batch1", "transcode_batch", "running", map[string]any{"mode": "size"}, nil, nil)
	for _, q := range []string{"?scope=already", "?scope=ALL"} {
		if c, k := get(t, h, "/transcode/v1/batches/batch1/goal"+q, keyHdr()); c != 400 || k["code"] != "invalid_request" {
			t.Errorf("GET %s: %d %v", q, c, k)
		}
	}
	for _, q := range []string{"", "?scope=all", "?scope=unfinished"} {
		if c, _ := get(t, h, "/transcode/v1/batches/batch1/goal"+q, keyHdr()); c != 200 {
			t.Errorf("GET %s: %d", q, c)
		}
	}
	w := write(h, "POST", "/transcode/v1/batches/batch1/goal", `{"mode":"size","selection_version":"v","idempotency_key":"k","size_budget_source":"already"}`)
	if c, k := code(t, w); c != 400 || k != "invalid_request" {
		t.Error("size_budget_source", c, k, w.Body.String())
	}
}
