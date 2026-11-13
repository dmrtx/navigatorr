package maintenanceui

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

// Builds API. A build is a job another tool runs on a machine (the target);
// when the target is a transcode node it holds that node while it runs (see
// action/builds.go).
//
//	POST /api/maintenance/builds                create; 200 with the build
//	GET  /api/maintenance/builds                list (target, status, source, limit)
//	GET  /api/maintenance/builds/{id}           one build
//	POST /api/maintenance/builds/{id}/heartbeat extend; 409 if the build ended
//	GET  /api/maintenance/builds/{id}/log       output tail: {lines, updated_at, status}
//	POST /api/maintenance/builds/{id}/finish    end it; finishing an ended build
//	                                            answers 200 with it unchanged
//
// A second bearer token (web.builds_token_file) may call only these routes.

const buildsPath = "/api/maintenance/builds"

// isBuildsPath is whether a request path is one of the build routes, and only
// those: the builds token is accepted nowhere else.
func isBuildsPath(p string) bool {
	if p == buildsPath {
		return true
	}
	rest, ok := strings.CutPrefix(p, buildsPath+"/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || len(parts) > 2 || !validBuildID(parts[0]) {
		return false
	}
	return len(parts) == 1 || parts[1] == "heartbeat" || parts[1] == "finish" || parts[1] == "log"
}

func validBuildID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// buildsTokenCall is whether the request carries the builds token on a build
// route. The token is a bearer, not an ambient cookie, so such a call needs no
// same-origin header.
func (s *Server) buildsTokenCall(r *http.Request) bool {
	if !s.hasBuildsToken || !isBuildsPath(r.URL.Path) {
		return false
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	h := sha256.Sum256([]byte(strings.TrimPrefix(auth, "Bearer ")))
	return subtle.ConstantTimeCompare(h[:], s.buildsTokenHash[:]) == 1
}

func (s *Server) buildsRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Target      string `json:"target"`
			Source      string `json:"source"`
			Holder      string `json:"holder"`
			Kind        string `json:"kind"`
			Title       string `json:"title"`
			PreemptMode string `json:"preempt_mode"`
			Priority    string `json:"priority"`
			TTLSeconds  int    `json:"ttl_seconds"`
			Progress    string `json:"progress"`
		}
		if err := decode(w, r, &body); err != nil {
			fail(w, 400, `send {"target", "source", "title", "holder"?, "kind"?, "preempt_mode"?, "priority"?, "ttl_seconds"?, "progress"?}`)
			return
		}
		b, err := s.engine.CreateBuild(r.Context(), action.BuildInput{Target: body.Target, Source: body.Source, Holder: body.Holder, Kind: body.Kind, Title: body.Title, PreemptMode: body.PreemptMode, Priority: body.Priority, Progress: body.Progress, TTLSeconds: body.TTLSeconds})
		if s.buildError(w, err) {
			return
		}
		writeJSON(w, 200, b)
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			fail(w, 400, "limit must be between 1 and 1000")
			return
		}
		limit = n
	}
	q := r.URL.Query()
	list, err := s.engine.ListBuilds(store.BuildFilter{Target: q.Get("target"), Status: q.Get("status"), Source: q.Get("source"), Limit: limit})
	if err != nil {
		fail(w, 500, "builds could not be read")
		return
	}
	writeJSON(w, 200, map[string]any{"builds": list})
}

func (s *Server) buildOne(w http.ResponseWriter, r *http.Request) {
	b, err := s.engine.GetBuild(r.PathValue("id"))
	if s.buildError(w, err) {
		return
	}
	writeJSON(w, 200, b)
}

func (s *Server) buildHeartbeat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Progress   *string   `json:"progress"`
		TTLSeconds int       `json:"ttl_seconds"`
		Log        []*string `json:"log"`
	}
	if err := decode(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		fail(w, 400, `send {"progress"?, "ttl_seconds"?, "log"?: [lines]} or nothing`)
		return
	}
	// A null element is no line. A build that is gone or over still answers
	// 404 or 409 first, as for any other bad line.
	lines := make([]string, 0, len(body.Log))
	for _, l := range body.Log {
		if l == nil {
			cur, err := s.engine.GetBuild(r.PathValue("id"))
			if s.buildError(w, err) {
				return
			}
			if cur.Status != store.BuildRunning && cur.Status != store.BuildWaiting {
				writeJSON(w, 409, map[string]any{"error": "build already ended", "build": cur})
				return
			}
			fail(w, 400, "log lines must be strings")
			return
		}
		lines = append(lines, *l)
	}
	b, err := s.engine.HeartbeatBuild(r.Context(), r.PathValue("id"), body.Progress, body.TTLSeconds, lines...)
	if errors.Is(err, store.ErrBuildEnded) {
		writeJSON(w, 409, map[string]any{"error": "build already ended", "build": b})
		return
	}
	if s.buildError(w, err) {
		return
	}
	writeJSON(w, 200, b)
}

// buildLog answers the output tail of a build: the last lines its heartbeats
// sent, oldest first. They are kept in memory only (see action/buildlog.go).
func (s *Server) buildLog(w http.ResponseWriter, r *http.Request) {
	b, lines, updated, err := s.engine.BuildLog(r.PathValue("id"))
	if s.buildError(w, err) {
		return
	}
	var at any
	if !updated.IsZero() {
		at = updated.UTC().Format(time.RFC3339)
	}
	var ended any
	if b.EndedAt != "" {
		ended = b.EndedAt
	}
	writeJSON(w, 200, map[string]any{"lines": lines, "updated_at": at, "status": b.Status, "ended_at": ended})
}

func (s *Server) buildFinish(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status   string `json:"status"`
		ExitCode *int   `json:"exit_code"`
	}
	if err := decode(w, r, &body); err != nil {
		fail(w, 400, `send {"status": "succeeded"|"failed"|"cancelled", "exit_code"?}`)
		return
	}
	b, _, err := s.engine.FinishBuild(r.Context(), r.PathValue("id"), body.Status, body.ExitCode)
	if s.buildError(w, err) {
		return
	}
	writeJSON(w, 200, b)
}

// buildError answers an error of a build call and reports whether it did.
func (s *Server) buildError(w http.ResponseWriter, err error) bool {
	var invalid *action.BuildInvalidError
	switch {
	case err == nil:
		return false
	case errors.As(err, &invalid):
		fail(w, 400, invalid.Reason)
	case errors.Is(err, store.ErrBuildNotFound):
		fail(w, 404, "build not found")
	default:
		fail(w, 500, "build not saved")
	}
	return true
}
