package maintenanceui

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/nodes"
)

// Legacy builds API. A build is a job another tool runs on a machine (the
// target). The registry is package nodes; these routes are thin aliases onto
// it, kept byte for byte for nbuild and old workers (the successor is
// /nodes/v1, see nodes/api.go). They are not extended: new capabilities go to
// /nodes/v1 only.
//
//	POST /api/maintenance/builds                create; 200 with the build; 409
//	                                            {"error": "builds paused on <node>",
//	                                            "code": "builds_paused"} while the
//	                                            operator paused builds on that node
//	GET  /api/maintenance/builds                list (target, status, source, limit)
//	GET  /api/maintenance/builds/nodes          every transcode node (or one, with
//	                                            ?node=NAME; 404 if unknown): its
//	                                            transcode pause, builds pause and
//	                                            running builds, to pick a machine
//	                                            before registering a build. Read from
//	                                            stored state, no worker call
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
			ID          string `json:"id"`
			Target      string `json:"target"`
			Source      string `json:"source"`
			Holder      string `json:"holder"`
			Kind        string `json:"kind"`
			Title       string `json:"title"`
			PreemptMode string `json:"preempt_mode"`
			Priority    string `json:"priority"`
			TTLSeconds  int    `json:"ttl_seconds"`
			Progress    string `json:"progress"`
			Via         string `json:"via"`
		}
		if err := decode(w, r, &body); err != nil {
			fail(w, 400, `send {"target", "source", "title", "id"?, "holder"?, "kind"?, "preempt_mode"?, "priority"?, "ttl_seconds"?, "progress"?}`)
			return
		}
		b, err := s.nodes.CreateBuild(r.Context(), nodes.BuildInput{ID: body.ID, Target: body.Target, Source: body.Source, Holder: body.Holder, Kind: body.Kind, Title: body.Title, PreemptMode: body.PreemptMode, Priority: body.Priority, Progress: body.Progress, Via: body.Via, TTLSeconds: body.TTLSeconds})
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
	list, err := s.nodes.ListBuilds(nodes.BuildFilter{Target: q.Get("target"), Status: q.Get("status"), Source: q.Get("source"), Limit: limit})
	if err != nil {
		fail(w, 500, "builds could not be read")
		return
	}
	writeJSON(w, 200, map[string]any{"builds": list})
}

// buildsNodes answers the machine picker: per configured transcode node, the
// effective transcode pause and builds pause (the same state worker-pause
// reads, without its worker probes) and the builds running on it. The literal
// path wins over /builds/{id}.
func (s *Server) buildsNodes(w http.ResponseWriter, r *http.Request) {
	// Label is the build's title on purpose: nbuild status consumes this name.
	type runningBuild struct {
		ID        string `json:"id"`
		Source    string `json:"source"`
		Label     string `json:"label"`
		StartedAt string `json:"started_at"`
	}
	type nodeState struct {
		Node            string         `json:"node"`
		TranscodePaused bool           `json:"transcode_paused"`
		TranscodeMode   string         `json:"transcode_mode"`
		BuildsPaused    bool           `json:"builds_paused"`
		RunningBuilds   []runningBuild `json:"running_builds"`
	}
	want := r.URL.Query().Get("node")
	statuses := s.engine.AttachNodeBuilds(s.engine.NodePauseStatesStored(r.Context()))
	out := make([]nodeState, 0, len(statuses))
	for name, st := range statuses {
		if want != "" && name != want {
			continue
		}
		n := nodeState{Node: name, TranscodePaused: st.Paused, TranscodeMode: st.Mode, BuildsPaused: st.BuildsPaused, RunningBuilds: []runningBuild{}}
		for _, b := range st.Builds {
			if b.Status == nodes.BuildRunning {
				n.RunningBuilds = append(n.RunningBuilds, runningBuild{ID: b.ID, Source: b.Source, Label: b.Title, StartedAt: b.StartedAt})
			}
		}
		out = append(out, n)
	}
	if want != "" && len(out) == 0 {
		fail(w, 404, "unknown node "+want)
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	writeJSON(w, 200, map[string]any{"nodes": out})
}

func (s *Server) buildOne(w http.ResponseWriter, r *http.Request) {
	b, err := s.nodes.GetBuild(r.PathValue("id"))
	if s.buildError(w, err) {
		return
	}
	writeJSON(w, 200, b)
}

func (s *Server) buildHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// The build comes first, from the path alone: unknown is 404 and over (or
	// past its expiry) is 409, whatever the body is or how large. Only then is the
	// body read and judged (400).
	if b, err := s.nodes.OpenBuild(r.Context(), id); errors.Is(err, nodes.ErrBuildEnded) {
		writeBuildEnded(w, b)
		return
	} else if s.buildError(w, err) {
		return
	}
	var body struct {
		Progress   json.RawMessage `json:"progress"`
		TTLSeconds json.RawMessage `json:"ttl_seconds"`
		Log        json.RawMessage `json:"log"`
	}
	if err := decodeLimit(w, r, &body, nodes.HeartbeatBodyMax); err != nil && !errors.Is(err, io.EOF) {
		fail(w, 400, `send {"progress"?, "ttl_seconds"?, "log"?: [lines]} or nothing`)
		return
	}
	var progress *string
	var ttl int
	var lines []string
	if len(body.Progress) > 0 && string(body.Progress) != "null" {
		var p string
		if json.Unmarshal(body.Progress, &p) != nil {
			fail(w, 400, "progress must be a string")
			return
		}
		progress = &p
	}
	if len(body.TTLSeconds) > 0 && string(body.TTLSeconds) != "null" && json.Unmarshal(body.TTLSeconds, &ttl) != nil {
		fail(w, 400, "ttl_seconds must be a whole number")
		return
	}
	if len(body.Log) > 0 && string(body.Log) != "null" {
		var items []*string
		if json.Unmarshal(body.Log, &items) != nil {
			items = []*string{nil}
		}
		for _, l := range items {
			if l == nil {
				fail(w, 400, "log must be an array of strings")
				return
			}
			lines = append(lines, *l)
		}
	}
	// The engine judges the values (length, range) and, if the build ended in
	// the meantime, answers 409.
	b, err := s.nodes.HeartbeatBuild(r.Context(), id, progress, ttl, lines...)
	if errors.Is(err, nodes.ErrBuildEnded) {
		writeBuildEnded(w, b)
		return
	}
	if s.buildError(w, err) {
		return
	}
	writeJSON(w, 200, b)
}

// writeBuildEnded is the 409 of a heartbeat to a build that is over; the
// record is the one the engine marked (see Engine.GetBuild, HeartbeatBuild).
func writeBuildEnded(w http.ResponseWriter, b nodes.Build) {
	writeJSON(w, 409, map[string]any{"error": "build already ended", "build": b})
}

// buildLog answers the output tail of a build: the last lines its heartbeats
// sent, oldest first. They are kept in memory only (see action/buildlog.go).
func (s *Server) buildLog(w http.ResponseWriter, r *http.Request) {
	out, err := s.nodes.BuildOutput(r.PathValue("id"))
	if s.buildError(w, err) {
		return
	}
	var at any
	if !out.Updated.IsZero() {
		at = out.Updated.UTC().Format(time.RFC3339)
	}
	var ended any
	if out.Build.EndedAt != "" {
		ended = out.Build.EndedAt
	}
	writeJSON(w, 200, map[string]any{"lines": out.Lines, "updated_at": at, "status": out.Build.Status, "ended_at": ended, "expired": out.Expired})
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
	b, _, err := s.nodes.FinishBuild(r.Context(), r.PathValue("id"), body.Status, body.ExitCode)
	if s.buildError(w, err) {
		return
	}
	writeJSON(w, 200, b)
}

// buildError answers an error of a build call and reports whether it did.
func (s *Server) buildError(w http.ResponseWriter, err error) bool {
	var invalid *nodes.InvalidError
	var paused *nodes.BuildsPausedError
	var taken *nodes.BuildIDTakenError
	var needLease *nodes.NodeLeaseRequiredError
	switch {
	case err == nil:
		return false
	case errors.As(err, &invalid):
		fail(w, 400, invalid.Reason)
	case errors.As(err, &paused):
		// Nothing was created; the code is stable for scripts.
		writeJSON(w, 409, map[string]string{"error": paused.Error(), "code": nodes.BuildsPausedCode})
	case errors.As(err, &needLease):
		writeJSON(w, 409, map[string]string{"error": needLease.Error(), "code": nodes.NodeLeaseRequiredCode})
	case errors.As(err, &taken):
		writeJSON(w, 409, map[string]string{"error": taken.Error(), "code": nodes.BuildIDTakenCode})
	case errors.Is(err, nodes.ErrBuildNotFound):
		fail(w, 404, "build not found")
	default:
		fail(w, 500, "build not saved")
	}
	return true
}

// SetNodes gives the server the build registry (package nodes). Without it the
// builds routes answer 503.
func (s *Server) SetNodes(n *nodes.Service) { s.nodes = n }

// withNodes answers 503 while no registry is attached.
func (s *Server) withNodes(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.nodes == nil {
			fail(w, 503, "builds are not available")
			return
		}
		h(w, r)
	}
}

// nodesAPI serves /nodes/v1 (auth was checked by Handler's wrapper).
func (s *Server) nodesAPI(w http.ResponseWriter, r *http.Request) {
	if s.nodes == nil {
		writeJSON(w, 503, map[string]string{"error": "nodes is not available", "code": "unavailable"})
		return
	}
	s.nodes.Handler().ServeHTTP(w, r)
}

// nodesAuthorized is whether a /nodes/v1 call may proceed: the builds token as
// X-Api-Key (what call_api sends) or as a bearer, or a signed-in session (a
// change through a session also needs the same-origin header, as everywhere).
func (s *Server) nodesAuthorized(r *http.Request) bool {
	if s.hasBuildsToken {
		tok := r.Header.Get("X-Api-Key")
		if auth := r.Header.Get("Authorization"); tok == "" && strings.HasPrefix(auth, "Bearer ") {
			tok = strings.TrimPrefix(auth, "Bearer ")
		}
		if tok != "" {
			h := sha256.Sum256([]byte(tok))
			return subtle.ConstantTimeCompare(h[:], s.buildsTokenHash[:]) == 1
		}
	}
	if !s.authorized(r) {
		return false
	}
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Header.Get("X-Navigatorr-Request") == "1"
}
