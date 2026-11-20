package maintenanceui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcodeapi"
)

// This file serves the writes of /transcode/v1 (phase 1b). Like the reads, each
// one calls the MCP tool or the maintenance handler that already does the
// work, so the two surfaces cannot drift apart, and every safeguard of the
// backend (an expected revision, digest or version, a confirm flag, an
// idempotency key) is passed through unchanged: the HTTP layer adds checks of
// its own (scope, body shape) and never removes one.

// v1MaxBody is the most bytes a request body may hold.
const v1MaxBody = 1 << 20

func v1Bad(w http.ResponseWriter, msg, hint string) {
	if hint == "" {
		hint = "GET " + transcodeapi.SpecPath + " describes the request body"
	}
	transcodeapi.WriteError(w, 400, "invalid_request", msg, hint)
}

// v1Body decodes the JSON body of a write into dst. A body is JSON: any other
// Content-Type is 415, a body over v1MaxBody is 413, an unknown field, a
// second value or a wrong type is 400. An empty body is a 400 when required
// and otherwise leaves dst as it is.
func v1Body(w http.ResponseWriter, r *http.Request, dst any, required bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, v1MaxBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			transcodeapi.WriteError(w, 413, "payload_too_large", "the request body is over 1 MiB", "send a smaller body")
			return false
		}
		v1Bad(w, "the request body could not be read", "")
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if required {
			v1Bad(w, "a JSON body is required", "")
			return false
		}
		return true
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		transcodeapi.WriteError(w, 415, "unsupported_media_type", "the body must be JSON", "send Content-Type: application/json")
		return false
	}
	if t := bytes.TrimSpace(raw); t[0] != '{' {
		v1Bad(w, "invalid body: the body must be a JSON object", "")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		v1Bad(w, "invalid body: "+err.Error(), "")
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		v1Bad(w, "invalid body: expected one JSON object", "")
		return false
	}
	return true
}

// v1Key is the idempotency key of a request: the Idempotency-Key header, the
// body's idempotency_key, or either when only one is sent. Two different keys
// are a 400, never a silent pick.
func v1Key(w http.ResponseWriter, r *http.Request, body string) (string, bool) {
	h, b := strings.TrimSpace(r.Header.Get("Idempotency-Key")), strings.TrimSpace(body)
	if h != "" && b != "" && h != b {
		v1Bad(w, "the Idempotency-Key header and the idempotency_key field differ", "send the key once")
		return "", false
	}
	if h != "" {
		return h, true
	}
	return b, true
}

// conflictWords are the phrases the tools and the engine use when the thing
// exists but its CURRENT STATE, revision, digest or version is not what the
// request expects. Two families:
//   - revision and digest guards: "managed profile changed: expected
//     generation", "batch selection changed", "cuts approval is missing or
//     stale", "recipe version conflict", "approval requires current waiting
//     review", "action_resume blocked: ...", "action already completed",
//     "... already exists", "does not match";
//   - state refusals: "only failed actions can be retried (current
//     status: ...)", "only a finished promotion's recovery copy can be
//     removed", "classification is frozen", "cuts are not planned yet",
//     "no previous valid cached recipe bundle", "inputs are immutable after
//     action creation", "candidate file kept", "recovery retained", "not a
//     regular file", "was removed after", "belongs to batch ...".
//
// Anything else the tool refuses is a bad parameter (400): a missing flag or
// digest ("needs confirm=true", "are required", "must carry", "invalid ...")
// is never a conflict.
var conflictWords = regexp.MustCompile(`\b(?:stale|changed|mismatch|differs|conflict|no longer|already|blocked|not waiting|current waiting|does not match|` +
	`only (?:a |an )?(?:failed|finished|automatic|original)|current status|is frozen|not planned yet|read every|no previous valid|immutable|` +
	`was removed after|belongs to batch|can still be promoted|candidate file kept|not a regular file|does not hold a regular file|` +
	`never moved aside|is not empty|retained|not confirmed|uncertain|unresolved|cannot create it|not finished|not ready yet|` +
	`requires (?:a|an|the|current) (?:waiting|unapproved|exact current))\b`)

// unavailableWords are refusals because the worker or service cannot take
// the request now (503), not because of the request or the resource.
var unavailableWords = regexp.MustCompile(`\b(?:unreachable|offline or not ready|not configured on this server|template unavailable)\b`)

// unavailableMessage is what a 503 says: the kind of failure, never the
// refusal itself (it can carry a worker name, a path or a URL; the cause is
// logged).
func unavailableMessage(low string) string {
	switch {
	case strings.Contains(low, "offline or not ready"):
		return "the video worker is offline or not ready"
	case strings.Contains(low, "not configured on this server"):
		return "the worker is not configured on this server"
	case strings.Contains(low, "template unavailable"):
		return "the podcast template is unavailable"
	}
	return "the worker or service is unreachable"
}

// internalWords are refusals that are a fault of the server, not of the
// request or the resource: a file or database that could not be written
// ("could not remove candidate file: ..." wraps the I/O error of
// action/candidate_cleanup.go; the rest are the words of the operating system
// and of SQLite). They answer 500 and the cause stays in the log.
var internalWords = regexp.MustCompile(`\b(?:could not remove candidate file|permission denied|input/output error|i/o error|disk i/o|no space left|read-only file system|disk is full|database is locked|database disk image|sql logic error|sqlite|no such table|no such column|cross-device link|too many open files|operation not permitted|cached recipe version \d+ is corrupt)\b`)

// fileMissingWords is the operating system's missing file: a fault of the
// server's own files when a write tool reports it (toolMissing never takes it
// for a missing resource).
var fileMissingWords = regexp.MustCompile(`\bno such file or directory\b`)

// destructiveWords are the refusals for the switch that keeps destructive
// changes off ("allow_destructive must be enabled for backup removal",
// "needs allow_destructive on this server", "Set allow_destructive: true"). It
// is matched after the quoted values are left out, and only in the phrasings
// the tools use, so a value the caller sent is not it.
var destructiveWords = regexp.MustCompile(`\b(?:(?:needs|requires|enable|set) allow_destructive|allow_destructive:? (?:must|is|on|true))\b`)

// quotedSwitches are the switch name alone in matching quotes, which the tools
// may use; such a quoted segment is kept as the bare name while every other
// quoted value is left out.
var quotedSwitches = map[string]bool{`"allow_destructive"`: true, "`allow_destructive`": true, `'allow_destructive'`: true}

// destructiveRefusal is whether the refusal is the destructive switch's: its
// phrasing, with the values the caller sent left out but a quoted bare switch
// name kept.
func destructiveRefusal(msg string) bool {
	low := quotedValues.ReplaceAllStringFunc(strings.ToLower(msg), func(seg string) string {
		if q := strings.TrimLeft(seg, " \t([:=,"); quotedSwitches[q] {
			return seg[:len(seg)-len(q)] + "allow_destructive"
		}
		return " "
	})
	return destructiveWords.MatchString(low)
}

// decisionName is the shape of a waiting option of an action (resume, pause,
// cancel, approve, reject, accept_loss, ...): the options differ by workflow,
// so only the shape is checked here and the engine judges the rest.
var decisionName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// quotedValues are the segments of a refusal that echo what the caller sent
// ("got \"x\"", 'x', `x`): never classified, or a client value such as
// "already" or "unreachable" would pick the status. A double-quoted segment
// may hold escaped quotes (the tools quote with %q). A single quote counts
// only when it opens a word ("attempt's" is not one).
var quotedValues = regexp.MustCompile("\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`|(?:^|[\\s(\\[:=,])'[^']*'")

// v1WriteErr names how a write presents what its tool refused: the 404 and
// hint as for a read; a refusal that is neither a missing thing, an
// unreachable service nor a conflict is a bad parameter (400).
type v1WriteErr struct {
	v1Err
}

// writeToolWriteError presents the refusal msg of a write tool.
func writeToolWriteError(w http.ResponseWriter, msg string, e v1WriteErr) {
	// A refusal of the inputs of action_run comes as {"error","code","field"}.
	var invalid struct {
		Error string `json:"error"`
		Code  string `json:"code"`
		Field string `json:"field"`
	}
	if json.Unmarshal([]byte(msg), &invalid) == nil && invalid.Code == "invalid_params" {
		hint := "GET " + transcodeapi.BasePath + "/catalog lists the inputs of each workflow"
		if invalid.Field != "" {
			hint = "the input " + strconv.Quote(invalid.Field) + " is not valid; " + hint
		}
		transcodeapi.WriteError(w, 400, "invalid_request", invalid.Error, hint)
		return
	}
	low := strings.ToLower(quotedValues.ReplaceAllString(msg, " "))
	switch {
	case internalWords.MatchString(low) || fileMissingWords.MatchString(low):
		log.Printf("transcode v1: a write tool failed inside the server: %s", msg)
		transcodeapi.WriteError(w, 500, "internal", "the change could not be completed", "the cause is in the server log; the state is unchanged or can be read again")
	case destructiveRefusal(msg):
		transcodeapi.WriteError(w, 403, "forbidden", msg, "destructive changes are switched off on this server")
	case toolMissing(low):
		code := e.notFound
		if code == "" {
			code = "not_found"
		}
		transcodeapi.WriteError(w, 404, code, msg, e.hint)
	case unavailableWords.MatchString(low):
		log.Printf("transcode v1: a write tool could not reach a worker or service: %s", msg)
		transcodeapi.WriteError(w, 503, "unavailable", unavailableMessage(low), "the worker or service did not answer; try again when it is back")
	case conflictWords.MatchString(low):
		hint := "read the current state, then retry with its current revision, digest or version"
		if e.hint != "" {
			hint = e.hint + "; " + hint
		}
		transcodeapi.WriteError(w, 409, "conflict", msg, hint)
	default:
		// Not a missing thing (checked above on the text without the echoed
		// values): a bad or missing parameter.
		hint := "GET " + transcodeapi.SpecPath + " describes the parameters"
		if e.hint != "" {
			hint = e.hint + "; " + hint
		}
		transcodeapi.WriteError(w, 400, "invalid_request", msg, hint)
	}
}

// writeToolTextStatus answers with what a tool returned, as writeToolText but
// with status.
func writeToolTextStatus(w http.ResponseWriter, status int, text string) {
	if json.Valid([]byte(text)) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(text))
		return
	}
	transcodeapi.WriteJSON(w, status, map[string]string{"message": text})
}

// v1Call runs a write tool and presents its answer with status. ok is the
// text on success; done is false when the answer was already written.
func (s *Server) v1Call(w http.ResponseWriter, r *http.Request, tool string, args map[string]any, e v1WriteErr) (text string, done bool) {
	text, errText, found, err := s.callTool(r.Context(), tool, args)
	switch {
	case !found:
		transcodeapi.WriteError(w, 503, "unavailable", "this endpoint is not available on this server (its capability is switched off)", "")
	case err != nil:
		writeToolFault(w, tool, err)
	case errText != "":
		writeToolWriteError(w, errText, e)
	case toolTooLarge(text):
		writeTooLarge(w, "the change was applied: read the result with the GET endpoint of the resource")
	default:
		return text, true
	}
	return "", false
}

// v1WriteTool serves a write with the tool that already does it.
func (s *Server) v1WriteTool(w http.ResponseWriter, r *http.Request, tool string, args map[string]any, status int, e v1WriteErr) {
	if text, ok := s.v1Call(w, r, tool, args, e); ok {
		writeToolTextStatus(w, status, text)
	}
}

// v1LegacyWrite runs a maintenance handler that serves a POST, as that POST
// with the given query and JSON body, and presents its answer and errors as
// the API does.
func (s *Server) v1LegacyWrite(w http.ResponseWriter, r *http.Request, fn http.HandlerFunc, query url.Values, body any, e v1Err) {
	raw, err := json.Marshal(body)
	if err != nil {
		writeToolFault(w, "legacy write", err)
		return
	}
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.URL.RawQuery = query.Encode()
	r2.Body = io.NopCloser(bytes.NewReader(raw))
	r2.ContentLength = int64(len(raw))
	r2.Header.Set("Content-Type", "application/json")
	sw := &shapedWriter{ResponseWriter: w, e: e}
	fn(sw, r2)
	sw.finish()
}

// withBody wraps a write that has a JSON body of type T.
func withBody[T any](required bool, fn func(w http.ResponseWriter, r *http.Request, body T)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body T
		if v1Body(w, r, &body, required) {
			fn(w, r, body)
		}
	}
}

type noBody struct{}

// v1ActionWrite guards a write under /actions/{id}: the action must be a
// transcode one (never a back door into other work).
func (s *Server) v1ActionWrite(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.checkTranscodeAction(w, r.PathValue("id")) {
			fn(w, r)
		}
	}
}

// v1Worker404 answers 404 unless name is a configured worker.
func (s *Server) knownWorker(w http.ResponseWriter, name string) bool {
	if _, ok := s.engine.Deps().Workers.Get(name); !ok {
		transcodeapi.WriteError(w, 404, "unknown_worker", "no such worker", hintWorkers)
		return false
	}
	return true
}

// rawObject is a JSON object kept as it was sent.
type rawObject = json.RawMessage

// objectString checks that raw is a JSON object and returns it as the string
// the tools take.
func objectString(raw rawObject) (string, bool) {
	t := bytes.TrimSpace(raw)
	return string(t), len(t) > 0 && t[0] == '{' && json.Valid(t)
}

func (s *Server) transcodeWriteHandlers(h transcodeapi.Handlers) {
	const b = transcodeapi.BasePath
	put := func(path string, fn http.HandlerFunc) { h["PUT "+b+path] = fn }
	post := func(path string, fn http.HandlerFunc) { h["POST "+b+path] = fn }
	del := func(path string, fn http.HandlerFunc) { h["DELETE "+b+path] = fn }
	state := v1WriteErr{}

	// Workers: slots, drain, pause.
	put("/workers/{name}/slots", s.v1PutSlots)
	put("/drain", withBody(true, func(w http.ResponseWriter, r *http.Request, body struct {
		Enabled *bool `json:"enabled"`
	}) {
		if body.Enabled == nil {
			v1Bad(w, `send {"enabled": true} to drain the workers or {"enabled": false} to resume`, "")
			return
		}
		s.v1LegacyWrite(w, r, s.workerDrain, nil, map[string]any{"enabled": *body.Enabled}, v1Err{})
	}))
	put("/pauses/{name}", s.v1PutPause)
	del("/pauses/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !s.knownWorker(w, name) {
			return
		}
		if !v1Body(w, r, &noBody{}, false) {
			return
		}
		s.v1LegacyWrite(w, r, s.workerPause, nil, map[string]any{"node": name, "mode": "off"}, v1Err{notFound: "unknown_worker", hint: hintWorkers})
	})

	// Actions.
	post("/actions", s.v1Run)
	post("/actions/{id}/cancel", s.v1ActionWrite(withBody(false, func(w http.ResponseWriter, r *http.Request, body struct {
		Reason string `json:"reason"`
	}) {
		s.v1WriteTool(w, r, "action_cancel", map[string]any{"id": r.PathValue("id"), "reason": body.Reason}, 200, state)
	})))
	post("/actions/{id}/resume", s.v1ActionWrite(withBody(false, func(w http.ResponseWriter, r *http.Request, body struct {
		Decision   string    `json:"decision"`
		Inputs     rawObject `json:"inputs"`
		PlanDigest string    `json:"plan_digest"`
	}) {
		if len(body.Decision) > 40 || (body.Decision != "" && !decisionName.MatchString(body.Decision)) {
			v1Bad(w, "decision must be one of the action's waiting options (a lowercase name such as resume, approve, reject)", "GET "+b+"/actions/{id} lists them")
			return
		}
		args := map[string]any{"id": r.PathValue("id"), "decision": body.Decision, "plan_digest": body.PlanDigest}
		if len(bytes.TrimSpace(body.Inputs)) > 0 {
			inputs, ok := objectString(body.Inputs)
			if !ok {
				v1Bad(w, "inputs must be a JSON object", "")
				return
			}
			args["inputs"] = inputs
		}
		s.v1WriteTool(w, r, "action_resume", args, 200, state)
	})))
	post("/actions/{id}/retry", s.v1ActionWrite(withBody(false, func(w http.ResponseWriter, r *http.Request, _ noBody) {
		s.v1WriteTool(w, r, "action_retry", map[string]any{"id": r.PathValue("id")}, 200, state)
	})))

	// Podcast cleaning.
	post("/actions/{id}/podcast/classify", s.v1ActionWrite(withBody(true, func(w http.ResponseWriter, r *http.Request, body rawObject) {
		classification, ok := objectString(body)
		if !ok {
			v1Bad(w, "the body must be the classification: a JSON object", "GET "+b+"/actions/{id}/podcast/blocks gives the block ids and digests to copy")
			return
		}
		s.v1WriteTool(w, r, "podcast_classify", map[string]any{"id": r.PathValue("id"), "classification": classification}, 200, v1WriteErr{v1Err: v1Err{notFound: "not_found", hint: "podcast writes need a clean_podcast_ads action: GET " + b + "/actions lists the actions"}})
	})))
	post("/actions/{id}/podcast/review", s.v1ActionWrite(withBody(false, func(w http.ResponseWriter, r *http.Request, body struct {
		Digest     string `json:"digest"`
		Approve    *bool  `json:"approve"`
		Reclassify *bool  `json:"reclassify"`
		Offset     *int   `json:"offset"`
	}) {
		args := map[string]any{"id": r.PathValue("id"), "digest": body.Digest}
		if body.Approve != nil {
			args["approve"] = *body.Approve
		}
		if body.Reclassify != nil {
			args["reclassify"] = *body.Reclassify
		}
		if body.Offset != nil {
			if *body.Offset < 0 {
				v1Bad(w, "offset must be 0 or more", "")
				return
			}
			args["offset"] = float64(*body.Offset)
		}
		s.v1WriteTool(w, r, "podcast_review", args, 200, v1WriteErr{v1Err: v1Err{notFound: "not_found", hint: "podcast writes need a clean_podcast_ads action: GET " + b + "/actions lists the actions"}})
	})))
	del("/podcasts/{id}/ad-library/{entry}", s.v1Podcast(func(w http.ResponseWriter, r *http.Request) {
		if !v1Body(w, r, &noBody{}, false) {
			return
		}
		s.v1WriteTool(w, r, "podcast_ad_library", map[string]any{"podcast_id": r.PathValue("id"), "revoke": r.PathValue("entry")}, 200, v1WriteErr{})
	}))

	// Batch goal.
	h["GET "+b+"/batches/{id}/goal"] = func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !s.checkBatch(w, id) {
			return
		}
		if scope := r.URL.Query().Get("scope"); scope != "" && scope != "all" && scope != "unfinished" {
			v1Bad(w, "scope must be all or unfinished", "GET "+transcodeapi.SpecPath+" describes the query parameters")
			return
		}
		s.v1Tool("transcode_goal_change", func(r *http.Request) (map[string]any, string) {
			args := map[string]any{"id": id}
			if scope := r.URL.Query().Get("scope"); scope != "" {
				args["scope"] = scope
			}
			return args, ""
		}, v1Err{notFound: "batch_not_found"})(w, r)
	}
	post("/batches/{id}/goal", s.v1PostGoal)

	// Backups and leftover candidates.
	backup := func(path, mode string) {
		post(path, s.v1ActionWrite(withBody(false, func(w http.ResponseWriter, r *http.Request, _ noBody) {
			s.v1WriteTool(w, r, "transcode_backups", map[string]any{"mode": mode, "action_id": r.PathValue("id")}, 200, state)
		})))
	}
	backup("/backups/{id}/clean", "clean")
	backup("/backups/{id}/restore", "restore")
	backup("/backups/{id}/discard-duplicate", "discard_duplicate")
	// discard removes a recovery copy WITHOUT verifying its replacement; the
	// tool leaves it to the caller's intent, so the API asks for it in words.
	post("/backups/{id}/discard", s.v1ActionWrite(withBody(true, func(w http.ResponseWriter, r *http.Request, body struct {
		Confirm *bool `json:"confirm"`
	}) {
		if body.Confirm == nil || !*body.Confirm {
			v1Bad(w, `this removes files for good: send {"confirm": true}`, "")
			return
		}
		s.v1WriteTool(w, r, "transcode_backups", map[string]any{"mode": "discard", "action_id": r.PathValue("id")}, 200, state)
	})))
	post("/backups/candidates/{id}/discard", s.v1ActionWrite(withBody(true, func(w http.ResponseWriter, r *http.Request, body struct {
		Confirm *bool `json:"confirm"`
	}) {
		// The tool refuses without confirm=true; it is passed as sent.
		args := map[string]any{"mode": "discard_candidate", "action_id": r.PathValue("id")}
		if body.Confirm != nil {
			args["confirm"] = *body.Confirm
		}
		s.v1WriteTool(w, r, "transcode_backups", args, 200, state)
	})))

	// Recipes.
	recipeErr := v1WriteErr{v1Err: v1Err{notFound: "recipe_not_found", hint: hintRecipes}}
	put("/recipes/{name}", withBody(true, func(w http.ResponseWriter, r *http.Request, body struct {
		Profile            rawObject `json:"profile"`
		Description        string    `json:"description"`
		SourceActionID     string    `json:"source_action_id"`
		ExpectedGeneration *int64    `json:"expected_generation"`
		ExpectedDigest     string    `json:"expected_digest"`
	}) {
		profile, ok := objectString(body.Profile)
		if !ok {
			v1Bad(w, "profile must be a JSON object (the complete recipe profile)", "")
			return
		}
		args := map[string]any{"name": r.PathValue("name"), "profile": profile, "description": body.Description,
			"source_action_id": body.SourceActionID, "expected_digest": body.ExpectedDigest}
		if body.ExpectedGeneration != nil {
			args["expected_generation"] = float64(*body.ExpectedGeneration)
		}
		s.v1WriteTool(w, r, "recipe_save", args, 200, recipeErr)
	}))
	del("/recipes/{name}", withBody(true, func(w http.ResponseWriter, r *http.Request, body struct {
		ExpectedGeneration *int64 `json:"expected_generation"`
		ExpectedDigest     string `json:"expected_digest"`
	}) {
		if body.ExpectedGeneration == nil || body.ExpectedDigest == "" {
			v1Bad(w, "expected_generation and expected_digest are both required (read them with GET "+b+"/recipes/{name})", "")
			return
		}
		s.v1WriteTool(w, r, "recipe_delete", map[string]any{"name": r.PathValue("name"), "expected_generation": float64(*body.ExpectedGeneration), "expected_digest": body.ExpectedDigest}, 200, recipeErr)
	}))
	post("/recipe-bundle/reload", s.v1BundleCall("recipe_reload", "activated", func(http.ResponseWriter, *http.Request) (map[string]any, bool) {
		return map[string]any{}, true
	}))
	post("/recipe-bundle/update", s.v1BundleCall("recipe_update", "activated", func(http.ResponseWriter, *http.Request) (map[string]any, bool) {
		return map[string]any{}, true
	}))
	post("/recipe-bundle/rollback", withBody(true, func(w http.ResponseWriter, r *http.Request, body struct {
		ExpectedActiveDigest   string `json:"expected_active_digest"`
		ExpectedPreviousDigest string `json:"expected_previous_digest"`
	}) {
		// The manager refuses a rollback without both digests; say so before the call.
		if body.ExpectedActiveDigest == "" || body.ExpectedPreviousDigest == "" {
			v1Bad(w, "expected_active_digest and expected_previous_digest are both required (read them with GET "+b+"/recipe-bundle)", "")
			return
		}
		s.v1BundleCall("recipe_rollback", "rolled_back", func(http.ResponseWriter, *http.Request) (map[string]any, bool) {
			return map[string]any{"expected_active_digest": body.ExpectedActiveDigest, "expected_previous_digest": body.ExpectedPreviousDigest}, true
		})(w, r)
	}))

	// Review queue.
	queue := v1Err{hint: "GET " + b + "/review-queue lists the entries"}
	post("/review-queue/{id}/close", withBody(true, func(w http.ResponseWriter, r *http.Request, body struct {
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
	}) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			v1Bad(w, "id must be the number of a review-queue entry", queue.hint)
			return
		}
		action := map[string]string{"processed": "processed", "dismissed": "dismiss"}[body.Outcome]
		if action == "" {
			v1Bad(w, "outcome must be processed or dismissed", "")
			return
		}
		s.v1LegacyWrite(w, r, s.maintenanceQueue, nil, map[string]any{"id": id, "action": action, "note": body.Note}, queue)
	}))
	post("/review-queue/{id}/decisions", withBody(true, func(w http.ResponseWriter, r *http.Request, body struct {
		Title        string   `json:"title"`
		Decision     string   `json:"decision"`
		ReleaseGUID  string   `json:"release_guid"`
		ReleaseGroup string   `json:"release_group"`
		Size         *int64   `json:"size"`
		Seeders      *int     `json:"seeders"`
		Score        *float64 `json:"score"`
		Reasons      []string `json:"reasons"`
	}) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			v1Bad(w, "id must be the number of a review-queue entry", queue.hint)
			return
		}
		if body.Decision != "selected" && body.Decision != "rejected" && body.Decision != "alternate" {
			v1Bad(w, "decision must be selected, rejected or alternate", "")
			return
		}
		args := map[string]any{"maintenance_item_id": strconv.FormatInt(id, 10), "title": body.Title, "decision": body.Decision,
			"release_guid": body.ReleaseGUID, "release_group": body.ReleaseGroup}
		if body.Size != nil {
			args["size"] = float64(*body.Size)
		}
		if body.Seeders != nil {
			args["seeders"] = float64(*body.Seeders)
		}
		if body.Score != nil {
			args["score"] = *body.Score
		}
		if len(body.Reasons) > 0 {
			reasons := make([]any, len(body.Reasons))
			for i, v := range body.Reasons {
				reasons[i] = v
			}
			args["reasons"] = reasons
		}
		s.v1WriteTool(w, r, "decision_record", args, 201, v1WriteErr{v1Err: queue})
	}))
}

// v1BundleCall serves a recipe-bundle write. These tools answer 200 with the
// bundle's status and a flag (activated, rolled_back) that is false when the
// change was refused; a refusal must not look like a success, so it is
// presented as one (409 for a stale review, 422 for a source that did not
// validate).
func (s *Server) v1BundleCall(tool, flag string, args func(w http.ResponseWriter, r *http.Request) (map[string]any, bool)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tool != "recipe_rollback" && !v1Body(w, r, &noBody{}, false) {
			return
		}
		a, ok := args(w, r)
		if !ok {
			return
		}
		text, done := s.v1Call(w, r, tool, a, v1WriteErr{v1Err: v1Err{hint: "GET " + transcodeapi.BasePath + "/recipe-bundle shows the active and previous digests and the status"}})
		if !done {
			return
		}
		var out struct {
			Error string `json:"error"`
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(text), &raw) != nil {
			log.Printf("transcode v1: %s answered something that is not an object", tool)
			transcodeapi.WriteError(w, 502, "internal", "the answer could not be read", "")
			return
		}
		var applied bool
		_ = json.Unmarshal(raw[flag], &applied)
		_ = json.Unmarshal(raw["error"], &out.Error)
		if applied {
			writeToolTextStatus(w, 200, text)
			return
		}
		msg := out.Error
		if msg == "" {
			msg = "the change was not applied"
		}
		if tool == "recipe_rollback" {
			writeToolWriteError(w, msg, v1WriteErr{v1Err: v1Err{hint: "GET " + transcodeapi.BasePath + "/recipe-bundle shows the active and previous digests"}})
			return
		}
		bundleRefusal(w, msg)
	}
}

// bundleSourceWords are the failures of reaching the recipe source (recipe/
// provider.go: reading the file, fetching the manifest or the bundle).
var bundleSourceWords = regexp.MustCompile(`\b(?:reading recipe file|fetching recipe|http \d{3}|connection refused|no such host|timeout|timed out|response exceeds|recipe file path is required)\b`)

// bundleCollisionWords are the refusals of the cache for a version that is
// already there with other content (recipe/manager.go cacheSnapshot).
var bundleCollisionWords = regexp.MustCompile(`\b(?:immutable version collision|already exists)\b`)

// bundleValidationWords is the allow-list of what a bundle (or its manifest)
// can get wrong, from the start of the refusal: transcode/recipe/validate.go
// and provider.go. Only these are the 422 rejection and show their text.
var bundleValidationWords = regexp.MustCompile(`^(?:parsing recipe (?:bundle|manifest)|recipe (?:bundle|manifest) (?:must|is nil|has no|sha256 mismatch|download must)|` +
	`unsupported (?:recipe|manifest)|invalid (?:bundle_version|manifest|recipe (?:manifest|download) url|profile name)|` +
	`container \S+: |profile (?:name )?"|x265 abr requires|recipe requires navigatorr|github recipe bundle download|` +
	`manifest (?:bundle_version|version) |only absolute https urls|redirected recipe request)`)

// bundleRefusal presents why recipe_reload or recipe_update did not activate
// a bundle. Only what the bundle itself got wrong (an allow-list of validation
// refusals) is the 422 rejection with its text; an unreachable source is a 503
// and a version that exists with other content a 409, each with a message of
// its own kind; anything else (I/O, a corrupt cache, an unrecognised error) is
// a 500. A 5xx never carries the refusal: it is logged.
func bundleRefusal(w http.ResponseWriter, msg string) {
	low := strings.ToLower(quotedValues.ReplaceAllString(msg, " "))
	hint := "the active bundle is unchanged: GET " + transcodeapi.BasePath + "/recipe-bundle shows its status"
	switch {
	case internalWords.MatchString(low) || fileMissingWords.MatchString(low) && !bundleSourceWords.MatchString(low):
		log.Printf("transcode v1: a recipe bundle change failed inside the server: %s", msg)
		transcodeapi.WriteError(w, 500, "internal", "the change could not be completed", "the cause is in the server log; "+hint)
	case bundleSourceWords.MatchString(low):
		log.Printf("transcode v1: the recipe source could not be reached: %s", msg)
		transcodeapi.WriteError(w, 503, "unavailable", "the recipe source could not be reached", "try again when it is back; "+hint)
	case bundleCollisionWords.MatchString(low):
		transcodeapi.WriteError(w, 409, "conflict", msg, hint)
	case bundleValidationWords.MatchString(strings.ToLower(msg)):
		transcodeapi.WriteError(w, 422, "rejected", msg, hint)
	default:
		log.Printf("transcode v1: a recipe bundle change failed in an unrecognised way: %s", msg)
		transcodeapi.WriteError(w, 500, "internal", "the change could not be completed", "the cause is in the server log; "+hint)
	}
}

// v1Run starts a workflow: only the transcode ones (allowedActions), the same
// check the maintenance console makes, and through action_run, whose
// validation and idempotency rules apply unchanged.
func (s *Server) v1Run(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action         string    `json:"action"`
		Inputs         rawObject `json:"inputs"`
		IdempotencyKey string    `json:"idempotency_key"`
	}
	if !v1Body(w, r, &body, true) {
		return
	}
	if !transcodeAction(body.Action) {
		v1Bad(w, "action must be a transcode workflow ("+strings.Join(transcodeActionNames(), ", ")+")", "GET "+transcodeapi.BasePath+"/catalog lists the workflows and their inputs")
		return
	}
	args := map[string]any{"action": body.Action}
	if len(bytes.TrimSpace(body.Inputs)) > 0 {
		inputs, ok := objectString(body.Inputs)
		if !ok {
			v1Bad(w, "inputs must be a JSON object", "")
			return
		}
		args["inputs"] = inputs
	}
	key, ok := v1Key(w, r, body.IdempotencyKey)
	if !ok {
		return
	}
	args["idempotency_key"] = key
	s.v1WriteTool(w, r, "action_run", args, 202, v1WriteErr{v1Err: v1Err{hint: "GET " + transcodeapi.BasePath + "/catalog lists the workflows and their inputs"}})
}

// v1PostGoal changes the goal of an automatic batch. The first half of the
// confirmation is GET /batches/{id}/goal (the exact selection and its
// version); the change needs that selection_version and an idempotency key,
// as the tool does.
func (s *Server) v1PostGoal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.checkBatch(w, id) {
		return
	}
	var body struct {
		Mode                string   `json:"mode"`
		Scope               string   `json:"scope"`
		SelectionVersion    string   `json:"selection_version"`
		IdempotencyKey      string   `json:"idempotency_key"`
		AllowHardware       *bool    `json:"allow_hardware"`
		AllowSlowPreset     *bool    `json:"allow_slow_preset"`
		KeepHDR             *bool    `json:"keep_hdr"`
		AudioOnly           *bool    `json:"audio_only"`
		SizeBudgetPercent   *float64 `json:"size_budget_percent"`
		SizeBudgetBytes     *float64 `json:"size_budget_bytes"`
		SizeBudgetSource    *string  `json:"size_budget_source"`
		SizeBudgetMultipler *float64 `json:"size_budget_sibling_multiplier"`
		HardwareMargin      *float64 `json:"hardware_margin_percent"`
	}
	if !v1Body(w, r, &body, true) {
		return
	}
	if body.Mode == "" {
		v1Bad(w, "mode is required: size, quality, x265_preserve or compact", "GET "+transcodeapi.BasePath+"/batches/{id}/goal previews the selection and gives its selection_version")
		return
	}
	switch body.Mode {
	case "size", "quality", "x265_preserve", "compact":
	default:
		v1Bad(w, "mode must be size, quality, x265_preserve or compact", "GET "+transcodeapi.BasePath+"/batches/{id}/goal previews the selection and gives its selection_version")
		return
	}
	if body.Scope != "" && body.Scope != "all" && body.Scope != "unfinished" {
		v1Bad(w, "scope must be all or unfinished", "")
		return
	}
	if body.SizeBudgetSource != nil && *body.SizeBudgetSource != "siblings" {
		v1Bad(w, `size_budget_source must be "siblings"`, "")
		return
	}
	key, ok := v1Key(w, r, body.IdempotencyKey)
	if !ok {
		return
	}
	args := map[string]any{"id": id, "mode": body.Mode, "scope": body.Scope, "selection_version": body.SelectionVersion, "idempotency_key": key}
	for name, v := range map[string]*bool{"allow_hardware": body.AllowHardware, "allow_slow_preset": body.AllowSlowPreset, "keep_hdr": body.KeepHDR, "audio_only": body.AudioOnly} {
		if v != nil {
			args[name] = *v
		}
	}
	for name, v := range map[string]*float64{"size_budget_percent": body.SizeBudgetPercent, "size_budget_bytes": body.SizeBudgetBytes,
		"size_budget_sibling_multiplier": body.SizeBudgetMultipler, "hardware_margin_percent": body.HardwareMargin} {
		if v != nil {
			args[name] = *v
		}
	}
	if body.SizeBudgetSource != nil {
		args["size_budget_source"] = *body.SizeBudgetSource
	}
	s.v1WriteTool(w, r, "transcode_goal_change", args, 202, v1WriteErr{v1Err: v1Err{notFound: "batch_not_found", hint: "GET " + transcodeapi.BasePath + "/batches/{id}/goal previews the selection and gives its selection_version"}})
}

// v1PutPause turns one switch of a worker's pause: mode (graceful, immediate,
// suspend or off) or builds_paused, never both (they are independent
// switches), through the maintenance console's handler.
func (s *Server) v1PutPause(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.knownWorker(w, name) {
		return
	}
	var body struct {
		Mode         *string `json:"mode"`
		BuildsPaused *bool   `json:"builds_paused"`
	}
	if !v1Body(w, r, &body, true) {
		return
	}
	e := v1Err{notFound: "unknown_worker", hint: hintWorkers}
	switch {
	case (body.Mode == nil) == (body.BuildsPaused == nil):
		v1Bad(w, "send exactly one of mode and builds_paused: they are independent switches", "")
	case body.BuildsPaused != nil:
		s.v1LegacyWrite(w, r, s.workerPause, nil, map[string]any{"node": name, "builds_paused": *body.BuildsPaused}, e)
	case !slices.Contains([]string{"graceful", "immediate", "suspend", "off"}, *body.Mode):
		v1Bad(w, `mode must be "graceful", "immediate", "suspend" or "off"`, "")
	default:
		s.v1LegacyWrite(w, r, s.workerPause, nil, map[string]any{"node": name, "mode": *body.Mode}, e)
	}
}

// v1PutSlots stores and pushes a worker's slot split, parallel jobs, x265 and
// core limits through the console's handler. revision is the one read with GET
// /workers/{name}/slots (settings.revision): a stale one is a 409 and nothing
// is changed.
func (s *Server) v1PutSlots(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.knownWorker(w, name) {
		return
	}
	var body struct {
		VideoSlots   *int   `json:"video_slots"`
		PodcastSlots *int   `json:"podcast_slots"`
		Ceiling      int    `json:"ceiling"`
		X265Slots    *int   `json:"x265_slots"`
		CoreBudget   *int   `json:"core_budget"`
		Revision     *int64 `json:"revision"`
	}
	if !v1Body(w, r, &body, true) {
		return
	}
	// The console reads a missing slot count as 0 (back to shared slots) and a
	// missing revision as "no check": a request that forgets them must not
	// clear a split or skip the revision test, so all three are required.
	if body.VideoSlots == nil || body.PodcastSlots == nil || body.Revision == nil {
		v1Bad(w, "video_slots, podcast_slots and revision are required (0 and 0 return the worker to shared slots; read revision with GET "+transcodeapi.BasePath+"/workers/{name}/slots)", "")
		return
	}
	legacy := map[string]any{"video_slots": *body.VideoSlots, "podcast_slots": *body.PodcastSlots, "ceiling": body.Ceiling, "revision": *body.Revision}
	if body.X265Slots != nil {
		legacy["x265_slots"] = *body.X265Slots
	}
	if body.CoreBudget != nil {
		legacy["core_budget"] = *body.CoreBudget
	}
	worker, _ := s.engine.Deps().Workers.Get(name)
	if _, ok := worker.Executor.(workerSlotController); !ok {
		transcodeapi.WriteError(w, 409, "conflict", "slot assignment requires the HTTP video worker", "GET "+transcodeapi.BasePath+"/workers/"+url.PathEscape(name)+"/slots says whether this worker supports it")
		return
	}
	s.v1LegacyWrite(w, r, s.workerSlots, url.Values{"node": {name}}, legacy, v1Err{notFound: "unknown_worker", hint: hintWorkers})
}
