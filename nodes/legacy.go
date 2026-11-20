package nodes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// LegacyBasePath is where the legacy builds API lives. It is kept byte for byte
// for nbuild and old workers (the successor is /nodes/v1, see api.go); it is not
// extended: new capabilities go to /nodes/v1 only.
//
//	POST /api/maintenance/builds                create; 200 with the build; 409
//	                                            {"error": "builds paused on <node>",
//	                                            "code": "builds_paused"} while the
//	                                            operator paused builds on that node
//	GET  /api/maintenance/builds                list (target, status, source, limit)
//	GET  /api/maintenance/builds/nodes          every configured node (or one, with
//	                                            ?node=NAME; 404 if unknown): its
//	                                            transcode pause, builds pause and
//	                                            running builds, to pick a machine
//	                                            before registering a build. Read from
//	                                            stored state, no worker call
//	GET  /api/maintenance/builds/{id}           one build
//	POST /api/maintenance/builds/{id}/heartbeat extend; 409 if the build ended
//	GET  /api/maintenance/builds/{id}/log       output tail: {lines, updated_at, status, ...}
//	POST /api/maintenance/builds/{id}/finish    end it; finishing an ended build
//	                                            answers 200 with it unchanged
const LegacyBasePath = "/api/maintenance/builds"

// legacyBodyMax is the body limit of the legacy calls that take a body, the
// heartbeat aside (HeartbeatBodyMax).
const legacyBodyMax = 256 << 10

// LegacyHandler serves the legacy builds routes under LegacyBasePath. It does no
// authentication: the caller wraps it (maintenanceui accepts the builds token as
// a bearer, or a signed-in session). It owns the full paths, so the same
// handler can sit behind a reverse proxy that forwards them unchanged.
//
// GET .../builds/nodes is computed from the registry (the builds pause and the
// running builds) and from the TranscodeView (the transcode pause and mode).
// A registry that cannot be read answers 503, never "builds paused": a worker
// fails open on anything but 200 (N3). A missing view, or one that does not
// know a node, leaves transcode_paused false and transcode_mode empty.
func (s *Service) LegacyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+LegacyBasePath, s.legacyRoot)
	mux.HandleFunc("POST "+LegacyBasePath, s.legacyRoot)
	mux.HandleFunc("GET "+LegacyBasePath+"/nodes", s.legacyNodes)
	mux.HandleFunc("GET "+LegacyBasePath+"/{id}", s.legacyOne)
	mux.HandleFunc("POST "+LegacyBasePath+"/{id}/heartbeat", s.legacyHeartbeat)
	mux.HandleFunc("GET "+LegacyBasePath+"/{id}/log", s.legacyLog)
	mux.HandleFunc("POST "+LegacyBasePath+"/{id}/finish", s.legacyFinish)
	return mux
}

func legacyFail(w http.ResponseWriter, code int, msg string) {
	apiJSON(w, code, map[string]string{"error": msg})
}

func legacyDecode(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}

func (s *Service) legacyRoot(w http.ResponseWriter, r *http.Request) {
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
		if err := legacyDecode(w, r, &body, legacyBodyMax); err != nil {
			legacyFail(w, 400, `send {"target", "source", "title", "id"?, "holder"?, "kind"?, "preempt_mode"?, "priority"?, "ttl_seconds"?, "progress"?}`)
			return
		}
		b, err := s.CreateBuild(r.Context(), BuildInput{ID: body.ID, Target: body.Target, Source: body.Source, Holder: body.Holder, Kind: body.Kind, Title: body.Title, PreemptMode: body.PreemptMode, Priority: body.Priority, Progress: body.Progress, Via: body.Via, TTLSeconds: body.TTLSeconds})
		if legacyError(w, err) {
			return
		}
		apiJSON(w, 200, b)
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			legacyFail(w, 400, "limit must be between 1 and 1000")
			return
		}
		limit = n
	}
	q := r.URL.Query()
	list, err := s.ListBuilds(BuildFilter{Target: q.Get("target"), Status: q.Get("status"), Source: q.Get("source"), Limit: limit})
	if err != nil {
		legacyFail(w, 500, "builds could not be read")
		return
	}
	apiJSON(w, 200, map[string]any{"builds": list})
}

// legacyNodes answers the machine picker: per configured node, the effective
// transcode pause and builds pause and the builds running on it. The literal
// path wins over /builds/{id}.
func (s *Service) legacyNodes(w http.ResponseWriter, r *http.Request) {
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
	// The registry is read first and failing it is a 503: an unreadable builds
	// pause is not "paused" here (N3), the worker simply fails open.
	pauses, err := s.reg.listPauses()
	if err != nil {
		legacyFail(w, 503, "builds are not available")
		return
	}
	active, err := s.reg.active()
	if err != nil {
		legacyFail(w, 503, "builds are not available")
		return
	}
	var states map[string]TranscodeState
	if s.tv != nil {
		states = s.tv.TranscodeStates(r.Context())
	}
	want := r.URL.Query().Get("node")
	var names []string
	if s.topo != nil {
		names = s.topo.Nodes()
	}
	out := make([]nodeState, 0, len(names))
	for _, name := range names {
		if want != "" && name != want {
			continue
		}
		st := states[name]
		_, paused := pauses[name]
		n := nodeState{Node: name, TranscodePaused: st.Paused, TranscodeMode: st.Mode, BuildsPaused: paused, RunningBuilds: []runningBuild{}}
		for _, b := range active {
			if b.Status != BuildRunning {
				continue
			}
			if node, ok := s.node(b.Target); ok && node == name {
				n.RunningBuilds = append(n.RunningBuilds, runningBuild{ID: b.ID, Source: b.Source, Label: b.Title, StartedAt: b.StartedAt})
			}
		}
		out = append(out, n)
	}
	if want != "" && len(out) == 0 {
		legacyFail(w, 404, "unknown node "+want)
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	apiJSON(w, 200, map[string]any{"nodes": out})
}

func (s *Service) legacyOne(w http.ResponseWriter, r *http.Request) {
	b, err := s.GetBuild(r.PathValue("id"))
	if legacyError(w, err) {
		return
	}
	apiJSON(w, 200, b)
}

func (s *Service) legacyHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// The build comes first, from the path alone: unknown is 404 and over (or
	// past its expiry) is 409, whatever the body is or how large. Only then is the
	// body read and judged (400).
	if b, err := s.OpenBuild(r.Context(), id); errors.Is(err, ErrBuildEnded) {
		legacyEnded(w, b)
		return
	} else if legacyError(w, err) {
		return
	}
	var body struct {
		Progress   json.RawMessage `json:"progress"`
		TTLSeconds json.RawMessage `json:"ttl_seconds"`
		Log        json.RawMessage `json:"log"`
	}
	if err := legacyDecode(w, r, &body, HeartbeatBodyMax); err != nil && !errors.Is(err, io.EOF) {
		legacyFail(w, 400, `send {"progress"?, "ttl_seconds"?, "log"?: [lines]} or nothing`)
		return
	}
	var progress *string
	var ttl int
	var lines []string
	if len(body.Progress) > 0 && string(body.Progress) != "null" {
		var p string
		if json.Unmarshal(body.Progress, &p) != nil {
			legacyFail(w, 400, "progress must be a string")
			return
		}
		progress = &p
	}
	if len(body.TTLSeconds) > 0 && string(body.TTLSeconds) != "null" && json.Unmarshal(body.TTLSeconds, &ttl) != nil {
		legacyFail(w, 400, "ttl_seconds must be a whole number")
		return
	}
	if len(body.Log) > 0 && string(body.Log) != "null" {
		var items []*string
		if json.Unmarshal(body.Log, &items) != nil {
			items = []*string{nil}
		}
		for _, l := range items {
			if l == nil {
				legacyFail(w, 400, "log must be an array of strings")
				return
			}
			lines = append(lines, *l)
		}
	}
	// The service judges the values (length, range) and, if the build ended in
	// the meantime, answers 409.
	b, err := s.HeartbeatBuild(r.Context(), id, progress, ttl, lines...)
	if errors.Is(err, ErrBuildEnded) {
		legacyEnded(w, b)
		return
	}
	if legacyError(w, err) {
		return
	}
	apiJSON(w, 200, b)
}

// legacyEnded is the 409 of a heartbeat to a build that is over, with the
// record the service marked.
func legacyEnded(w http.ResponseWriter, b Build) {
	apiJSON(w, 409, map[string]any{"error": "build already ended", "build": b})
}

// legacyLog answers the output tail of a build: the last lines its heartbeats
// sent, oldest first. They are kept in memory only.
func (s *Service) legacyLog(w http.ResponseWriter, r *http.Request) {
	out, err := s.BuildOutput(r.PathValue("id"))
	if legacyError(w, err) {
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
	apiJSON(w, 200, map[string]any{"lines": out.Lines, "updated_at": at, "status": out.Build.Status, "ended_at": ended, "expired": out.Expired})
}

func (s *Service) legacyFinish(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status   string `json:"status"`
		ExitCode *int   `json:"exit_code"`
	}
	if err := legacyDecode(w, r, &body, legacyBodyMax); err != nil {
		legacyFail(w, 400, `send {"status": "succeeded"|"failed"|"cancelled", "exit_code"?}`)
		return
	}
	b, _, err := s.FinishBuild(r.Context(), r.PathValue("id"), body.Status, body.ExitCode)
	if legacyError(w, err) {
		return
	}
	apiJSON(w, 200, b)
}

// legacyError answers an error of a build call and reports whether it did.
func legacyError(w http.ResponseWriter, err error) bool {
	var invalid *InvalidError
	var paused *BuildsPausedError
	var taken *BuildIDTakenError
	var needLease *NodeLeaseRequiredError
	switch {
	case err == nil:
		return false
	case errors.As(err, &invalid):
		legacyFail(w, 400, invalid.Reason)
	case errors.As(err, &paused):
		// Nothing was created; the code is stable for scripts.
		apiJSON(w, 409, map[string]string{"error": paused.Error(), "code": BuildsPausedCode})
	case errors.As(err, &needLease):
		apiJSON(w, 409, map[string]string{"error": needLease.Error(), "code": NodeLeaseRequiredCode})
	case errors.As(err, &taken):
		apiJSON(w, 409, map[string]string{"error": taken.Error(), "code": BuildIDTakenCode})
	case errors.Is(err, ErrBuildNotFound):
		legacyFail(w, 404, "build not found")
	default:
		legacyFail(w, 500, "build not saved")
	}
	return true
}
