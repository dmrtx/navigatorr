package maintenanceui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcodeapi"
	"github.com/mark3labs/mcp-go/mcp"
)

// This file serves /transcode/v1 (package transcodeapi), the read-only
// successor of the transcode reads of /api/maintenance/* and of the MCP tools.
// It holds no business logic of its own: each endpoint calls the maintenance
// handler or the MCP tool that already answers the question, so the two
// surfaces cannot drift apart, and only reshapes the request (a resource path
// instead of a query) and the error ({error, code, hint}).

// transcodeActions are the workflows the Transcode API shows. The list is the
// one the maintenance surface already restricts its actions to.
func transcodeAction(name string) bool { return allowedActions[name] }

// transcodeAuthorized is whether a /transcode/v1 call may proceed: the API key
// (web.transcode_token_file) as X-Api-Key (what call_api sends) or as a bearer,
// or a signed-in web UI session. This phase serves reads only, so a session
// needs no same-origin header, but a change attempted through one still needs
// it, as everywhere, so it answers 401 and not 405.
func (s *Server) transcodeAuthorized(r *http.Request) bool {
	if s.hasTranscodeToken {
		tok := r.Header.Get("X-Api-Key")
		if auth := r.Header.Get("Authorization"); tok == "" && strings.HasPrefix(auth, "Bearer ") {
			tok = strings.TrimPrefix(auth, "Bearer ")
		}
		if tok != "" {
			h := sha256.Sum256([]byte(tok))
			if subtle.ConstantTimeCompare(h[:], s.transcodeTokenHash[:]) == 1 {
				return true
			}
		}
	}
	if !s.authorized(r) {
		return false
	}
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Header.Get("X-Navigatorr-Request") == "1"
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
	get("/actions/{id}", s.v1ActionTool("action_status", func(r *http.Request) (map[string]any, string) {
		return map[string]any{"verbose": r.URL.Query().Get("verbose")}, ""
	}))
	get("/actions/{id}/detail/{section}", s.v1ActionTool("action_detail", func(r *http.Request) (map[string]any, string) {
		section := r.PathValue("section")
		if !slices.Contains([]string{"inputs", "outputs", "state", "steps"}, section) {
			return nil, "section must be inputs, outputs, state or steps"
		}
		q := r.URL.Query()
		return map[string]any{"section": section, "key": q.Get("key"), "chunk": q.Get("chunk"), "step_index": q.Get("step_index"), "step_offset": q.Get("step_offset"), "step_limit": q.Get("step_limit")}, ""
	}))
	get("/actions/{id}/podcast/blocks", s.v1ActionTool("podcast_blocks", func(r *http.Request) (map[string]any, string) {
		return map[string]any{"offset": r.URL.Query().Get("offset")}, ""
	}))
	get("/actions/{id}/podcast/blocks/{block_id}", s.v1ActionTool("podcast_block", func(r *http.Request) (map[string]any, string) {
		q := r.URL.Query()
		return map[string]any{"block_id": r.PathValue("block_id"), "offset": q.Get("offset"), "unknown_only": q.Get("unknown_only")}, ""
	}))
	get("/actions/{id}/podcast/artifacts/{artifact}", s.v1ActionTool("podcast_artifact", func(r *http.Request) (map[string]any, string) {
		artifact := r.PathValue("artifact")
		if !slices.Contains([]string{"transcript", "classifications", "cuts"}, artifact) {
			return nil, "artifact must be transcript, classifications or cuts"
		}
		return map[string]any{"artifact": artifact, "offset": r.URL.Query().Get("offset")}, ""
	}))
	get("/history", s.v1Tool("action_history", func(r *http.Request) (map[string]any, string) {
		q := r.URL.Query()
		return map[string]any{"media": q.Get("media"), "service": q.Get("service"), "action": q.Get("action"), "limit": q.Get("limit")}, ""
	}, v1Err{}))
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
	batch := func(fn http.HandlerFunc, query func(r *http.Request) url.Values) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			inst, err := s.engine.Deps().Store.GetActionInstanceIfExists(id)
			if err != nil {
				transcodeapi.WriteError(w, 500, "internal", "the request could not be completed", "")
				return
			}
			if inst == nil || inst.ActionName != "transcode_batch" {
				transcodeapi.WriteError(w, 404, "batch_not_found", "batch not found", "GET "+b+"/actions?status=all lists actions; a batch has action_name transcode_batch")
				return
			}
			s.v1Legacy(fn, func(r *http.Request) url.Values {
				v := query(r)
				v.Set("id", id)
				return v
			}, v1Err{notFound: "batch_not_found", hint: "GET " + b + "/actions lists actions; a batch has action_name transcode_batch"})(w, r)
		}
	}
	get("/batches/{id}/items", batch(s.batchItems, func(r *http.Request) url.Values { return pick(r, "offset", "limit", "child") }))
	get("/batches/{id}/settings", batch(s.batchSettings, func(r *http.Request) url.Values { return pick(r, "scope", "candidate_id", "item_key") }))
	get("/batches/{id}/candidates", batch(s.batchCandidates, func(r *http.Request) url.Values { return url.Values{} }))
	get("/batches/{id}/promotion-plan", batch(s.batchCandidates, func(r *http.Request) url.Values { return url.Values{"plan": {"1"}} }))
	get("/batches/{id}/preview", batch(s.batchPreview, func(r *http.Request) url.Values { return url.Values{} }))

	// Backups and leftover candidates.
	get("/backups", s.v1Tool("transcode_backups", func(r *http.Request) (map[string]any, string) {
		return map[string]any{"mode": "list", "offset": r.URL.Query().Get("offset")}, ""
	}, v1Err{}))
	get("/backups/candidates", s.v1Tool("transcode_backups", func(r *http.Request) (map[string]any, string) {
		return map[string]any{"mode": "candidates", "offset": r.URL.Query().Get("offset")}, ""
	}, v1Err{}))

	// Recipes.
	get("/recipes", s.v1Tool("recipe_list", func(r *http.Request) (map[string]any, string) {
		q := r.URL.Query()
		return map[string]any{"limit": q.Get("limit"), "offset": q.Get("offset"), "include_selection_details": q.Get("include_selection_details")}, ""
	}, v1Err{}))
	get("/recipes/{name}", s.v1Tool("recipe_get", func(r *http.Request) (map[string]any, string) {
		return map[string]any{"name": r.PathValue("name")}, ""
	}, v1Err{notFound: "recipe_not_found", hint: hintRecipes}))
	get("/recipes/{name}/history", s.v1Tool("recipe_history", func(r *http.Request) (map[string]any, string) {
		return map[string]any{"name": r.PathValue("name"), "limit": r.URL.Query().Get("limit")}, ""
	}, v1Err{notFound: "recipe_not_found", hint: hintRecipes}))
	get("/recipe-bundle", s.v1Tool("recipe_status", func(*http.Request) (map[string]any, string) { return map[string]any{}, "" }, v1Err{}))

	// Podcasts.
	get("/podcasts", s.v1Legacy(s.podcasts, nil, v1Err{}))
	podcast := func(fn func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if p, ok := s.cfg.Podcasts.Podcasts[id]; !s.cfg.Podcasts.Enabled || !ok || !p.Enabled {
				transcodeapi.WriteError(w, 404, "unknown_podcast", "no such podcast profile", "GET "+b+"/podcasts lists the enabled podcast profiles")
				return
			}
			fn(w, r)
		}
	}
	get("/podcasts/{id}/local-library", podcast(s.v1Legacy(s.podcastLocalLibrary, func(r *http.Request) url.Values {
		return url.Values{"podcast_id": {r.PathValue("id")}}
	}, v1Err{notFound: "unknown_podcast"})))
	get("/podcasts/{id}/ad-library", podcast(s.v1Tool("podcast_ad_library", func(r *http.Request) (map[string]any, string) {
		return map[string]any{"podcast_id": r.PathValue("id"), "offset": r.URL.Query().Get("offset")}, ""
	}, v1Err{})))

	// The review queue of files and titles worth converting.
	get("/review-queue", s.v1Legacy(s.maintenanceQueue, func(r *http.Request) url.Values {
		return pick(r, "state", "issue_type", "sort", "q", "offset", "limit")
	}, v1Err{}))
	return h
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
	h := w.ResponseWriter.Header()
	h.Del("Content-Length")
	h.Del("Etag")
	h.Del("Server-Timing")
	hint := ""
	if w.status == 400 || w.status == 404 {
		hint = w.e.hint
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
// its text. errText is set when the tool reported an error.
func (s *Server) callTool(ctx context.Context, name string, args map[string]any) (text string, errText string, found bool) {
	t := s.mcp.GetTool(name)
	if t == nil {
		return "", "", false
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
		return "", err.Error(), true
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if res.IsError {
		return "", sb.String(), true
	}
	return sb.String(), "", true
}

// writeToolError presents what an MCP tool refused. The tools report errors
// as text, so a missing thing is told apart by its words; the endpoints that
// can know before calling check first and never reach this.
func writeToolError(w http.ResponseWriter, msg string, e v1Err) {
	low := strings.ToLower(msg)
	if strings.Contains(low, "not found") || strings.Contains(low, "unknown") || strings.Contains(low, "no such") {
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
		text, errText, found := s.callTool(r.Context(), tool, a)
		switch {
		case !found:
			transcodeapi.WriteError(w, 503, "unavailable", "this endpoint is not available on this server (its capability is switched off)", "")
		case errText != "":
			writeToolError(w, errText, e)
		default:
			writeToolText(w, text)
		}
	}
}

// v1ActionTool is v1Tool for an endpoint under /actions/{id}: the action must
// exist and be one of the transcode workflows.
func (s *Server) v1ActionTool(tool string, args func(r *http.Request) (map[string]any, string)) http.HandlerFunc {
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
		}, v1Err{notFound: "action_not_found", hint: hintActions})(w, r)
	}
}

// checkTranscodeAction answers 404 unless id is a transcode action.
func (s *Server) checkTranscodeAction(w http.ResponseWriter, id string) bool {
	inst, err := s.engine.Deps().Store.GetActionInstanceIfExists(id)
	if err != nil {
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
	text, errText, found := s.callTool(r.Context(), "action_list", map[string]any{"status": status, "limit": limit, "offset": offset})
	switch {
	case !found:
		transcodeapi.WriteError(w, 503, "unavailable", "this endpoint is not available on this server", "")
		return
	case errText != "":
		writeToolError(w, errText, v1Err{})
		return
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		// The tool sent its "response too large" note instead of the page.
		writeToolText(w, text)
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
