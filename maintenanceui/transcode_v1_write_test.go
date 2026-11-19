package maintenanceui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/mark3labs/mcp-go/mcp"
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
	if w := v1(h, "GET", "/transcode/v1/batches/batch1/goal?scope=pending", "", keyHdr()); w.Code != 200 || !strings.Contains(w.Body.String(), "v7") || rec.last()["scope"] != "pending" {
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
			return "discard_candidate requires confirm=true", true
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
	// The candidate flow: the tool's own refusal reaches the caller (a 400
	// here because the body had no confirm, and the tool was still the judge).
	if c, _ := code(t, write(h, "POST", "/transcode/v1/backups/candidates/job1/discard", `{"confirm":false}`)); c != 400 && c != 409 {
		t.Fatal("candidate without confirm", c)
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
	fakeWrite(s, "recipe_update", ok(`{"activated":false,"error":"the bundle did not validate"}`))
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
	w := write(h, "POST", "/transcode/v1/review-queue/7/decisions", `{"title":"Example Show","decision":"approved","release_group":"GroupA","size":1234,"reasons":["fits"]}`)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	a := rec.last()
	if a["maintenance_item_id"] != "7" || a["decision"] != "approved" || a["release_group"] != "GroupA" || a["size"] != 1234.0 || len(a["reasons"].([]any)) != 1 {
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
	w := write(h, "DELETE", "/transcode/v1/podcasts/p1/ad-library/e1", ``)
	if w.Code == 200 {
		if lib.last()["revoke"] != "e1" || lib.last()["podcast_id"] != "p1" {
			t.Fatal(lib.last())
		}
	} else if w.Code != 404 && w.Code != 503 {
		// 404 or 503 when the podcast profile is not configured in this test
		// server; the route was reached either way.
		t.Fatal(w.Code, w.Body.String())
	}
}
