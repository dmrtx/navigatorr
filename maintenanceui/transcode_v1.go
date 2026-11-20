package maintenanceui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcodeapi"
	"github.com/mark3labs/mcp-go/mcp"
)

// This file serves the reads of /transcode/v1 (package transcodeapi), the
// successor of the transcode reads of /api/maintenance/* and of the MCP tools;
// transcode_v1_write.go serves its writes.
// It holds no business logic of its own: each endpoint calls the maintenance
// handler or the MCP tool that already answers the question, so the two
// surfaces cannot drift apart, and only reshapes the request (a resource path
// instead of a query) and the error ({error, code, hint}).

// transcodeActions are the workflows the Transcode API shows. The list is the
// one the maintenance surface already restricts its actions to.
func transcodeAction(name string) bool { return allowedActions[name] }

// transcodeAccess says whether a /transcode/v1 call may proceed and, when it
// may not, the status to answer: 401 for no or wrong credentials, 403 for a
// change that fails the console's own write checks.
//
// The API key (web.transcode_token_file), as X-Api-Key (what call_api sends) or
// as a bearer, is no ambient credential (a browser never attaches it by itself),
// so it needs nothing more, as the builds token needs nothing more on the build
// routes. The web console's own sign-in (cookie, bearer web token or Cloudflare
// Access) is ambient, so a change made with it passes the same two checks the
// console applies to its /api/ writes (see the handler wrapper in server.go): the
// X-Navigatorr-Request: 1 header, and an Origin, when sent, that names this host.
// Reads need only the credential.
func (s *Server) transcodeAccess(r *http.Request) (ok bool, status int) {
	if s.hasTranscodeToken {
		tok := r.Header.Get("X-Api-Key")
		if auth := r.Header.Get("Authorization"); tok == "" && strings.HasPrefix(auth, "Bearer ") {
			tok = strings.TrimPrefix(auth, "Bearer ")
		}
		if tok != "" {
			h := sha256.Sum256([]byte(tok))
			if subtle.ConstantTimeCompare(h[:], s.transcodeTokenHash[:]) == 1 {
				return true, 0
			}
		}
	}
	if !s.authorized(r) {
		return false, 401
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true, 0
	}
	if r.Header.Get("X-Navigatorr-Request") != "1" {
		return false, 401
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return false, 403
		}
	}
	return true, 0
}

// transcodeV1 builds the handler of /transcode/v1 (auth was checked by
// Handler's wrapper). A mismatch between the handlers and transcodeapi.Routes
// is a programming error; a test fails on it, and here it is logged and every
// call answers 500 rather than a half-mounted API.
func (s *Server) transcodeV1() http.Handler {
	h, err := transcodeapi.NewHandler(s.transcodeHandlers())
	if err != nil {
		log.Printf("transcode v1 API not mounted: %v", err)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			transcodeapi.WriteError(w, 500, "internal", "the transcode API is not available", "")
		})
	}
	return h
}

const (
	hintWorkers = "GET " + transcodeapi.BasePath + "/workers lists the workers"
	hintActions = "GET " + transcodeapi.BasePath + "/actions lists the actions"
	hintRecipes = "GET " + transcodeapi.BasePath + "/recipes lists the recipes"
)

func (s *Server) transcodeHandlers() transcodeapi.Handlers {
	const b = transcodeapi.BasePath
	h := transcodeapi.Handlers{}
	get := func(path string, fn http.HandlerFunc) { h["GET "+b+path] = fn }

	// Workers.
	get("/workers", s.v1Legacy(s.workers, nil, v1Err{}))
	get("/workers/{name}", s.v1Worker)
	get("/workers/{name}/slots", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, ok := s.engine.Deps().Workers.Get(name); !ok {
			transcodeapi.WriteError(w, 404, "unknown_worker", "no such worker", hintWorkers)
			return
		}
		s.v1Legacy(s.workerSlots, func(*http.Request) url.Values { return url.Values{"node": {name}} }, v1Err{notFound: "unknown_worker", hint: hintWorkers})(w, r)
	})
	get("/activity", s.v1Legacy(s.workerActivity, nil, v1Err{}))
	get("/state", s.v1Legacy(s.workerNodeState, nil, v1Err{}))
	get("/drain", s.v1Legacy(s.workerDrain, nil, v1Err{}))
	get("/pauses", s.v1Legacy(s.workerPause, nil, v1Err{}))
	get("/pauses/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		s.v1Legacy(s.workerPause, func(*http.Request) url.Values { return url.Values{"node": {name}} }, v1Err{notFound: "unknown_worker", hint: hintWorkers})(w, r)
	})

	// Actions.
	get("/actions", s.v1Actions)
	detailHint := "GET " + b + "/actions/{id}/detail/{section} without key lists the keys of a section; steps pages through the step log"
	podcastHint := "podcast reads need a clean_podcast_ads action: GET " + b + "/actions lists the actions"
	get("/actions/{id}", s.v1ActionTool("action_status", func(r *http.Request) (map[string]any, string) {
		return v1Args(r, nil, pBool("verbose"))
	}, v1Err{}))
	get("/actions/{id}/detail/{section}", s.v1ActionTool("action_detail", func(r *http.Request) (map[string]any, string) {
		section := r.PathValue("section")
		if !slices.Contains([]string{"inputs", "outputs", "state", "steps"}, section) {
			return nil, "section must be inputs, outputs, state or steps"
		}
		return v1Args(r, map[string]any{"section": section, "key": r.URL.Query().Get("key")},
			pInt("chunk", 0, 0), pInt("step_index", 0, 0), pInt("step_offset", 0, 0), pInt("step_limit", 1, 50))
	}, v1Err{notFound: "not_found", hint: detailHint}))
	get("/actions/{id}/podcast/blocks", s.v1ActionTool("podcast_blocks", func(r *http.Request) (map[string]any, string) {
		return v1Args(r, nil, pInt("offset", 0, 0))
	}, v1Err{notFound: "not_found", hint: podcastHint}))
	// Reading a block records read coverage, a side effect that only a GET may
	// have: HEAD would record it without delivering the body. The route is
	// NoHead in transcodeapi, whose NewHandler refuses HEAD before this runs.
	get("/actions/{id}/podcast/blocks/{block_id}", s.v1ActionTool("podcast_block", func(r *http.Request) (map[string]any, string) {
		return v1Args(r, map[string]any{"block_id": r.PathValue("block_id")}, pInt("offset", 0, 0), pBool("unknown_only"))
	}, v1Err{notFound: "not_found", hint: podcastHint}))
	get("/actions/{id}/podcast/artifacts/{artifact}", s.v1ActionTool("podcast_artifact", func(r *http.Request) (map[string]any, string) {
		artifact := r.PathValue("artifact")
		if !slices.Contains([]string{"transcript", "classifications", "cuts"}, artifact) {
			return nil, "artifact must be transcript, classifications or cuts"
		}
		return v1Args(r, map[string]any{"artifact": artifact}, pInt("offset", 0, 0))
	}, v1Err{notFound: "not_found", hint: podcastHint}))
	get("/history", s.v1History)
	get("/catalog", func(w http.ResponseWriter, r *http.Request) {
		out := []any{}
		for _, entry := range s.engine.Catalog() {
			if transcodeAction(entry.Name) {
				out = append(out, entry)
			}
		}
		transcodeapi.WriteJSON(w, 200, map[string]any{"actions": out})
	})

	// Batches (a batch is an action of kind transcode_batch).
	get("/batches/{id}/items", s.v1Batch(s.batchItems, func(r *http.Request) url.Values { return pick(r, "offset", "limit", "child") }))
	get("/batches/{id}/settings", s.v1Batch(s.batchSettings, func(r *http.Request) url.Values { return pick(r, "scope", "candidate_id", "item_key") }))
	get("/batches/{id}/candidates", s.v1Batch(s.batchCandidates, func(r *http.Request) url.Values { return url.Values{} }))
	get("/batches/{id}/promotion-plan", s.v1Batch(s.batchCandidates, func(r *http.Request) url.Values { return url.Values{"plan": {"1"}} }))
	get("/batches/{id}/preview", s.v1Batch(s.batchPreview, func(r *http.Request) url.Values { return url.Values{} }))

	// Backups and leftover candidates.
	get("/backups", s.v1Tool("transcode_backups", func(r *http.Request) (map[string]any, string) {
		return v1Args(r, map[string]any{"mode": "list"}, pInt("offset", 0, 0))
	}, v1Err{}))
	get("/backups/candidates", s.v1Tool("transcode_backups", func(r *http.Request) (map[string]any, string) {
		return v1Args(r, map[string]any{"mode": "candidates"}, pInt("offset", 0, 0))
	}, v1Err{}))

	// Recipes.
	get("/recipes", s.v1Tool("recipe_list", func(r *http.Request) (map[string]any, string) {
		return v1Args(r, nil, pInt("limit", 1, 100), pInt("offset", 0, 0), pBool("include_selection_details"))
	}, v1Err{}))
	get("/recipes/{name}", s.v1Tool("recipe_get", func(r *http.Request) (map[string]any, string) {
		return map[string]any{"name": r.PathValue("name")}, ""
	}, v1Err{notFound: "recipe_not_found", hint: hintRecipes}))
	get("/recipes/{name}/history", s.v1Tool("recipe_history", func(r *http.Request) (map[string]any, string) {
		return v1Args(r, map[string]any{"name": r.PathValue("name")}, pInt("limit", 1, 50))
	}, v1Err{notFound: "recipe_not_found", hint: hintRecipes}))
	get("/recipe-bundle", s.v1Tool("recipe_status", func(*http.Request) (map[string]any, string) { return map[string]any{}, "" }, v1Err{}))

	// Podcasts.
	get("/podcasts", func(w http.ResponseWriter, r *http.Request) {
		if !s.podcastsOff(w) {
			s.v1Legacy(s.podcasts, nil, v1Err{})(w, r)
		}
	})
	get("/podcasts/{id}/local-library", s.v1Podcast(s.v1Legacy(s.podcastLocalLibrary, func(r *http.Request) url.Values {
		return url.Values{"podcast_id": {r.PathValue("id")}}
	}, v1Err{notFound: "unknown_podcast"})))
	get("/podcasts/{id}/ad-library", s.v1Podcast(s.v1Tool("podcast_ad_library", func(r *http.Request) (map[string]any, string) {
		return v1Args(r, map[string]any{"podcast_id": r.PathValue("id")}, pInt("offset", 0, 0))
	}, v1Err{})))

	// The review queue of files and titles worth converting.
	get("/review-queue", s.v1Legacy(s.maintenanceQueue, func(r *http.Request) url.Values {
		return pick(r, "state", "issue_type", "sort", "q", "offset", "limit")
	}, v1Err{}))
	s.transcodeWriteHandlers(h)
	s.transcodeWindowReadHandlers(h)
	s.transcodeWindowWriteHandlers(h)
	s.transcodeQueueHandlers(h)
	return h
}

// podcastsOff answers 503 when podcasts are switched off on this server.
func (s *Server) podcastsOff(w http.ResponseWriter) bool {
	if s.cfg.Podcasts.Enabled {
		return false
	}
	transcodeapi.WriteError(w, 503, "unavailable", "podcasts are switched off on this server", "")
	return true
}

// v1Podcast guards a route under /podcasts/{id}: podcasts must be on and the
// profile must exist and be enabled.
func (s *Server) v1Podcast(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if s.podcastsOff(w) {
			return
		}
		if p, ok := s.cfg.Podcasts.Podcasts[id]; !ok || !p.Enabled {
			transcodeapi.WriteError(w, 404, "unknown_podcast", "no such podcast profile", "GET "+transcodeapi.BasePath+"/podcasts lists the enabled podcast profiles")
			return
		}
		fn(w, r)
	}
}

// v1Batch serves a batch sub-resource with the legacy handler fn. The batch
// is known to exist by the time fn runs, so a 404 of fn is about something
// inside it (a file, a candidate): the generic not_found, never batch_not_found.
func (s *Server) v1Batch(fn http.HandlerFunc, query func(r *http.Request) url.Values) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !s.checkBatch(w, id) {
			return
		}
		s.v1Legacy(fn, func(r *http.Request) url.Values {
			v := query(r)
			v.Set("id", id)
			return v
		}, v1Err{})(w, r)
	}
}

// checkBatch answers 404 unless id is a transcode batch.
func (s *Server) checkBatch(w http.ResponseWriter, id string) bool {
	inst, err := s.engine.Deps().Store.GetActionInstanceIfExists(id)
	if err != nil {
		log.Printf("transcode v1: reading batch %q: %v", id, err)
		transcodeapi.WriteError(w, 500, "internal", "the request could not be completed", "")
		return false
	}
	if inst == nil || inst.ActionName != "transcode_batch" {
		transcodeapi.WriteError(w, 404, "batch_not_found", "batch not found", "GET "+transcodeapi.BasePath+"/actions?status=all lists actions; a batch has action_name transcode_batch")
		return false
	}
	return true
}

// pick copies the named query parameters of r that are present.
func pick(r *http.Request, names ...string) url.Values {
	out := url.Values{}
	q := r.URL.Query()
	for _, n := range names {
		if q.Has(n) {
			out.Set(n, q.Get(n))
		}
	}
	return out
}

// v1Err names how the errors of one legacy handler or tool are presented: the
// code of its 404 (the status says "not found"; the code says what) and the
// hint that points to the endpoint to use instead.
type v1Err struct{ notFound, hint string }

// shapedWriter lets a legacy handler write its answer and turns its errors,
// {"error": ...} with any status of 400 or more, into the API's own
// {error, code, hint}. Successful answers pass through untouched.
type shapedWriter struct {
	http.ResponseWriter
	e      v1Err
	status int
	failed bool
	wrote  bool
	buf    bytes.Buffer
}

func (w *shapedWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote, w.status = true, code
	if code >= 400 {
		w.failed = true
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *shapedWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(200)
	}
	if w.failed {
		return w.buf.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *shapedWriter) finish() {
	if !w.failed {
		return
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.buf.Bytes(), &body)
	if body.Error == "" {
		body.Error = http.StatusText(w.status)
	}
	code := transcodeapi.CodeForStatus(w.status)
	if w.status == 404 && w.e.notFound != "" {
		code = w.e.notFound
	}
	if w.status >= 500 {
		// A fault of the server: the cause goes to the log and never to the
		// caller, as for a tool's Go error (writeToolFault).
		log.Printf("transcode v1: legacy handler answered %d: %s", w.status, body.Error)
		body.Error = "the request could not be completed"
		if w.status == 503 {
			body.Error = "this endpoint is not available on this server"
		}
	}
	h := w.ResponseWriter.Header()
	h.Del("Content-Length")
	h.Del("Etag")
	h.Del("Server-Timing")
	hint := ""
	if w.status == 400 || w.status == 404 {
		hint = w.e.hint
	}
	if w.status == 400 && hint == "" {
		hint = "GET " + transcodeapi.SpecPath + " describes the parameters"
	}
	transcodeapi.WriteError(w.ResponseWriter, w.status, code, body.Error, hint)
}

// v1Legacy serves a /transcode/v1 read with the maintenance handler that
// already answers it: as a GET whose query is query(r) (nil: none).
func (s *Server) v1Legacy(fn http.HandlerFunc, query func(r *http.Request) url.Values, e v1Err) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.Method = http.MethodGet
		r2.URL.RawQuery = ""
		if query != nil {
			r2.URL.RawQuery = query(r).Encode()
		}
		sw := &shapedWriter{ResponseWriter: w, e: e}
		fn(sw, r2)
		sw.finish()
	}
}

// v1Worker is one worker's row of the workers list, its store-only state
// (builds, hold, builds pause) and its pause.
func (s *Server) v1Worker(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.engine.Deps().Workers.Get(name); !ok {
		transcodeapi.WriteError(w, 404, "unknown_worker", "no such worker", hintWorkers)
		return
	}
	_, rows := s.workerRows(r.Context())
	for _, row := range rows {
		if row["worker"] == name {
			row["state"] = s.nodeStoreState(name)
			transcodeapi.WriteJSON(w, 200, row)
			return
		}
	}
	transcodeapi.WriteError(w, 404, "unknown_worker", "no such worker", hintWorkers)
}

// callTool runs an MCP tool the way the maintenance surface does and returns
// its text. errText is set when the tool refused the call (an IsError result:
// a message for the caller); err is a Go error of the handler itself, which is
// a fault of the server and never the caller's.
func (s *Server) callTool(ctx context.Context, name string, args map[string]any) (text string, errText string, found bool, err error) {
	t := s.mcp.GetTool(name)
	if t == nil {
		return "", "", false, nil
	}
	// Empty arguments are left out so the tool applies its own defaults.
	clean := map[string]any{}
	for k, v := range args {
		if str, ok := v.(string); ok && str == "" {
			continue
		}
		clean[k] = v
	}
	res, err := t.Handler(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: clean}})
	if err != nil {
		return "", "", true, err
	}
	if res == nil {
		return "", "", true, errors.New("tool returned no result")
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if res.IsError {
		return "", sb.String(), true, nil
	}
	return sb.String(), "", true, nil
}

// writeToolFault answers a Go error of a tool handler: 500, with the cause
// logged and not sent.
func writeToolFault(w http.ResponseWriter, tool string, err error) {
	log.Printf("transcode v1: tool %s failed: %v", tool, err)
	transcodeapi.WriteError(w, 500, "internal", "the request could not be completed", "")
}

// toolTooLarge is whether text is the "response_too_large" note the bounded
// tools send instead of a page that would not fit.
func toolTooLarge(text string) bool {
	var note struct {
		Error string `json:"error"`
	}
	return json.Unmarshal([]byte(text), &note) == nil && note.Error == "response_too_large"
}

func writeTooLarge(w http.ResponseWriter, hint string) {
	transcodeapi.WriteError(w, 502, "response_too_large", "the answer does not fit in one response", hint)
}

// unknownResource matches the "unknown <resource>" messages the tools behind
// this API emit for a thing that does not exist ("unknown transcode profile",
// "unknown block"). A bare "unknown" is not enough: "unknown_only" and
// "unknown mode" are about a parameter, and "unknown podcast worker status"
// is a fault.
var unknownResource = regexp.MustCompile(`\bunknown (transcode profile|block)\b`)

// toolMissing is whether a tool's refusal says the thing asked for does not
// exist: "... not found", "no such <thing>" (but not the OS's "no such file or
// directory") or an "unknown <resource>" the tools use.
func toolMissing(msg string) bool {
	low := strings.ToLower(msg)
	if strings.Contains(low, "not found") || unknownResource.MatchString(low) {
		return true
	}
	return strings.Contains(low, "no such ") && !strings.Contains(low, "no such file or directory")
}

// writeToolError presents what an MCP tool refused. The tools report errors
// as text, so a missing thing is told apart by its words (toolMissing); the
// endpoints that can know before calling check first and never reach this.
func writeToolError(w http.ResponseWriter, msg string, e v1Err) {
	if toolMissing(msg) {
		code := e.notFound
		if code == "" {
			code = "not_found"
		}
		transcodeapi.WriteError(w, 404, code, msg, e.hint)
		return
	}
	hint := "GET " + transcodeapi.SpecPath + " describes the parameters"
	if e.hint != "" {
		hint = e.hint + "; " + hint
	}
	transcodeapi.WriteError(w, 400, "invalid_request", msg, hint)
}

// writeToolText answers with what a tool returned: its JSON as it is, or its
// plain sentence ("Blocklist is empty.") as {"message": ...}.
func writeToolText(w http.ResponseWriter, text string) {
	if json.Valid([]byte(text)) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(text))
		return
	}
	transcodeapi.WriteJSON(w, 200, map[string]string{"message": text})
}

// v1Tool serves a read with the MCP tool that already answers it. args builds
// the tool's arguments from the request; a non-empty second result is a 400.
func (s *Server) v1Tool(tool string, args func(r *http.Request) (map[string]any, string), e v1Err) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, bad := args(r)
		if bad != "" {
			transcodeapi.WriteError(w, 400, "invalid_request", bad, "GET "+transcodeapi.SpecPath+" describes the parameters")
			return
		}
		text, errText, found, err := s.callTool(r.Context(), tool, a)
		switch {
		case !found:
			transcodeapi.WriteError(w, 503, "unavailable", "this endpoint is not available on this server (its capability is switched off)", "")
		case err != nil:
			writeToolFault(w, tool, err)
		case errText != "":
			writeToolError(w, errText, e)
		case toolTooLarge(text):
			writeTooLarge(w, "narrow the request: a smaller limit, a later offset, a section or a key")
		default:
			writeToolText(w, text)
		}
	}
}

// v1ActionTool is v1Tool for an endpoint under /actions/{id}: the action must
// exist and be one of the transcode workflows.
// e presents a 404 of the tool: the action is known to exist by then, so it is
// about something inside it (a key, a step, a block).
func (s *Server) v1ActionTool(tool string, args func(r *http.Request) (map[string]any, string), e v1Err) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !s.checkTranscodeAction(w, id) {
			return
		}
		s.v1Tool(tool, func(r *http.Request) (map[string]any, string) {
			a, bad := args(r)
			if a != nil {
				a["id"] = id
			}
			return a, bad
		}, e)(w, r)
	}
}

// checkTranscodeAction answers 404 unless id is a transcode action.
func (s *Server) checkTranscodeAction(w http.ResponseWriter, id string) bool {
	inst, err := s.engine.Deps().Store.GetActionInstanceIfExists(id)
	if err != nil {
		log.Printf("transcode v1: reading action %q: %v", id, err)
		transcodeapi.WriteError(w, 500, "internal", "the request could not be completed", "")
		return false
	}
	if inst == nil || !transcodeAction(inst.ActionName) {
		transcodeapi.WriteError(w, 404, "action_not_found", "action not found", hintActions)
		return false
	}
	return true
}

// v1Actions lists the transcode actions, newest first, as a page:
// {items, offset, limit, next_offset, has_more}. The engine pages over every
// action and the page is then cut to the transcode ones, so a page can hold
// fewer than limit items, even none: follow next_offset until has_more is false.
func (s *Server) v1Actions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" && !slices.Contains([]string{"running", "waiting_external", "waiting_decision", "completed", "failed", "cancelled", "all"}, status) {
		transcodeapi.WriteError(w, 400, "invalid_request", "status must be running, waiting_external, waiting_decision, completed, failed, cancelled or all", "GET "+transcodeapi.SpecPath+" describes the parameters")
		return
	}
	limit, offset := 20, 0
	for name, target := range map[string]*int{"limit": &limit, "offset": &offset} {
		if raw := q.Get(name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || (name == "limit" && (n < 1 || n > 100)) {
				transcodeapi.WriteError(w, 400, "invalid_request", "limit must be 1 to 100 and offset 0 or more", "GET "+transcodeapi.SpecPath+" describes the parameters")
				return
			}
			*target = n
		}
	}
	text, errText, found, err := s.callTool(r.Context(), "action_list", map[string]any{"status": status, "limit": limit, "offset": offset})
	switch {
	case !found:
		transcodeapi.WriteError(w, 503, "unavailable", "this endpoint is not available on this server", "")
		return
	case err != nil:
		writeToolFault(w, "action_list", err)
		return
	case errText != "":
		writeToolError(w, errText, v1Err{})
		return
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		// Not a page: the tool sent its "response too large" note (or
		// something unexpected). Never answer 200 without the page fields.
		if toolTooLarge(text) {
			writeTooLarge(w, "lower limit")
		} else {
			log.Printf("transcode v1: action_list answered something that is not a page: %v", err)
			transcodeapi.WriteError(w, 502, "internal", "the action list could not be read", "")
		}
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if name, _ := row["action_name"].(string); transcodeAction(name) {
			items = append(items, row)
		}
	}
	transcodeapi.WriteJSON(w, 200, map[string]any{"items": items, "offset": offset, "limit": limit, "next_offset": offset + len(rows), "has_more": len(rows) == limit})
}

// v1Param is one numeric or boolean query parameter of a tool-backed route.
type v1Param struct {
	name     string
	boolean  bool
	min, max int // integers; max 0 is no upper bound
}

func pInt(name string, min, max int) v1Param { return v1Param{name: name, min: min, max: max} }
func pBool(name string) v1Param              { return v1Param{name: name, boolean: true} }

func (p v1Param) problem() string {
	switch {
	case p.boolean:
		return p.name + " must be true or false (or 1 or 0)"
	case p.max > 0:
		return fmt.Sprintf("%s must be an integer from %d to %d", p.name, p.min, p.max)
	}
	return fmt.Sprintf("%s must be an integer of %d or more", p.name, p.min)
}

// v1Params reads the named query parameters as typed values (int or bool) for
// the tool. An absent or empty one is left out, so the tool applies its own
// default; a bad one is a message for a 400 and never silently falls back to
// the default, which is what the tools do with a string they cannot parse.
func v1Params(r *http.Request, ps ...v1Param) (map[string]any, string) {
	out := map[string]any{}
	q := r.URL.Query()
	for _, p := range ps {
		raw := strings.TrimSpace(q.Get(p.name))
		if raw == "" {
			continue
		}
		if p.boolean {
			switch strings.ToLower(raw) {
			case "true", "1":
				out[p.name] = true
			case "false", "0":
				out[p.name] = false
			default:
				return nil, p.problem()
			}
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < p.min || (p.max > 0 && n > p.max) {
			return nil, p.problem()
		}
		out[p.name] = n
	}
	return out, ""
}

// v1Args is v1Params plus the fixed arguments of the route.
func v1Args(r *http.Request, fixed map[string]any, ps ...v1Param) (map[string]any, string) {
	out, bad := v1Params(r, ps...)
	if bad != "" {
		return nil, bad
	}
	for k, v := range fixed {
		out[k] = v
	}
	return out, ""
}

// logEvents are the action_log entries the engine writes for a workflow
// instance (identifiers holds the instance id). Every other entry of the log
// (scan_library, safe_replace, api_call, ...) belongs to other services.
var logEvents = []string{"action_completed", "action_failed", "action_retry", "action_cancelled"}

// v1History is the audit log of the transcode workflows. action_history reads
// the whole log, so the answer is cut to the entries the engine wrote for a
// transcode action instance, and no other service's history is reachable.
// The filter `action` is a transcode workflow name (entries of its instances)
// or a log event (action_completed, ...); anything else is a 400. The log has
// no offset, so the newest 100 entries matching media, service and event are
// read (fewer when the tool answers that they do not fit) and then filtered:
// with a busy log the page can hold fewer than limit entries.
func (s *Server) v1History(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args, bad := v1Params(r, pInt("limit", 1, 100))
	if bad != "" {
		transcodeapi.WriteError(w, 400, "invalid_request", bad, "GET "+transcodeapi.SpecPath+" describes the parameters")
		return
	}
	limit := 20
	if n, ok := args["limit"].(int); ok {
		limit = n
	}
	var workflow, event string
	switch a := strings.TrimSpace(q.Get("action")); {
	case a == "":
	case transcodeAction(a):
		workflow = a
	case slices.Contains(logEvents, a):
		event = a
	default:
		transcodeapi.WriteError(w, 400, "invalid_request", "action must be a transcode workflow ("+strings.Join(transcodeActionNames(), ", ")+") or a log event ("+strings.Join(logEvents, ", ")+")", "GET "+transcodeapi.BasePath+"/catalog lists the workflows")
		return
	}
	// The tool answers a note instead of a page that would not fit its size
	// limit. Read fewer rows then: halve the underlying limit down to the one
	// asked for before giving up, so a useful subset is always reachable.
	var text string
	for rows := 100; ; {
		var errText string
		var found bool
		var err error
		text, errText, found, err = s.callTool(r.Context(), "action_history", map[string]any{"media": q.Get("media"), "service": q.Get("service"), "action": event, "limit": rows})
		switch {
		case !found:
			transcodeapi.WriteError(w, 503, "unavailable", "this endpoint is not available on this server (its capability is switched off)", "")
			return
		case err != nil:
			writeToolFault(w, "action_history", err)
			return
		case errText != "":
			writeToolError(w, errText, v1Err{})
			return
		case toolTooLarge(text):
			if rows <= limit {
				writeTooLarge(w, "narrow with media, service or action (action_completed, action_failed, action_retry or action_cancelled; a workflow name is applied after the read and does not narrow it)")
				return
			}
			rows = max(limit, rows/2)
			continue
		}
		break
	}
	// A page is an object with a "records" key (null when the log is empty);
	// {} or null is not a page, and neither is a sentence.
	var raw map[string]json.RawMessage
	var pageRecords []map[string]any
	err := json.Unmarshal([]byte(text), &raw)
	if err == nil {
		if rec, ok := raw["records"]; !ok {
			err = errors.New("no records field")
		} else {
			err = json.Unmarshal(rec, &pageRecords)
		}
	}
	if err != nil {
		log.Printf("transcode v1: action_history answered something that is not a page: %v", err)
		transcodeapi.WriteError(w, 502, "internal", "the history could not be read", "")
		return
	}
	owner := map[string]string{} // instance id -> workflow name ("" when not a transcode one)
	records := []map[string]any{}
	for _, rec := range pageRecords {
		if len(records) >= limit {
			break
		}
		ev, _ := rec["action"].(string)
		id, _ := rec["identifiers"].(string)
		if !slices.Contains(logEvents, ev) || id == "" {
			continue
		}
		name, seen := owner[id]
		if !seen {
			inst, err := s.engine.Deps().Store.GetActionInstanceIfExists(id)
			if err != nil {
				log.Printf("transcode v1: reading action %q for history: %v", id, err)
				transcodeapi.WriteError(w, 500, "internal", "the request could not be completed", "")
				return
			}
			if inst != nil && transcodeAction(inst.ActionName) {
				name = inst.ActionName
			}
			owner[id] = name
		}
		if name == "" || (workflow != "" && name != workflow) {
			continue
		}
		records = append(records, rec)
	}
	transcodeapi.WriteJSON(w, 200, map[string]any{"count": len(records), "records": records})
}

func transcodeActionNames() []string {
	names := []string{}
	for n := range allowedActions {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
