package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// BasePath is where the HTTP API lives.
const BasePath = "/nodes/v1"

// SpecPath is the OpenAPI document of the API. It is public (it describes the
// API and holds no data) so a service registry can fetch it without a key.
const SpecPath = BasePath + "/openapi.json"

// Route is one operation of the API, as registered on the mux.
type Route struct {
	Method string
	Path   string // OpenAPI form, e.g. /nodes/v1/builds/{id}
}

// Routes lists every operation Handler serves (the spec is checked against it).
func Routes() []Route {
	return []Route{
		{"GET", BasePath + "/nodes"},
		{"GET", BasePath + "/nodes/{name}"},
		{"POST", BasePath + "/nodes/{name}/builds-pause"},
		{"GET", BasePath + "/builds"},
		{"POST", BasePath + "/builds"},
		{"GET", BasePath + "/builds/{id}"},
		{"POST", BasePath + "/builds/{id}/heartbeat"},
		{"POST", BasePath + "/builds/{id}/finish"},
		{"POST", BasePath + "/builds/{id}/cancel"},
		{"GET", BasePath + "/builds/{id}/log"},
	}
}

// Handler serves /nodes/v1. It does no authentication: the caller wraps it
// (maintenanceui accepts the builds token as Bearer or X-Api-Key). The spec
// route is served by SpecHandler.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	h := map[string]http.HandlerFunc{
		"GET " + BasePath + "/nodes":                      s.apiNodes,
		"GET " + BasePath + "/nodes/{name}":               s.apiNode,
		"POST " + BasePath + "/nodes/{name}/builds-pause": s.apiBuildsPause,
		"GET " + BasePath + "/builds":                     s.apiBuilds,
		"POST " + BasePath + "/builds":                    s.apiCreate,
		"GET " + BasePath + "/builds/{id}":                s.apiBuild,
		"POST " + BasePath + "/builds/{id}/heartbeat":     s.apiHeartbeat,
		"POST " + BasePath + "/builds/{id}/finish":        s.apiFinish,
		"POST " + BasePath + "/builds/{id}/cancel":        s.apiCancel,
		"GET " + BasePath + "/builds/{id}/log":            s.apiLog,
	}
	for pattern, fn := range h {
		mux.HandleFunc(pattern, fn)
	}
	mux.HandleFunc(BasePath+"/", func(w http.ResponseWriter, r *http.Request) {
		apiError(w, 404, "not_found", "no such endpoint", "GET "+SpecPath+" lists the endpoints of this API")
	})
	return mux
}

// BuildV1 is a build as /nodes/v1 shows it.
type BuildV1 struct {
	ID              string `json:"id"`
	Target          string `json:"target"`
	Source          string `json:"source"`
	Holder          string `json:"holder"`
	Kind            string `json:"kind"`
	Title           string `json:"title"`
	PreemptMode     string `json:"preempt_mode"`
	Priority        string `json:"priority"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	StartedAt       string `json:"started_at,omitempty"`
	LastHeartbeatAt string `json:"last_heartbeat_at"`
	ExpiresAt       string `json:"expires_at"`
	EndedAt         string `json:"ended_at,omitempty"`
	Progress        string `json:"progress,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
}

func viewBuild(b Build) BuildV1 {
	return BuildV1{b.ID, b.Target, b.Source, b.Holder, b.Kind, b.Title, b.PreemptMode, b.Priority, b.Status,
		b.CreatedAt, b.StartedAt, b.LastHeartbeatAt, b.ExpiresAt, b.EndedAt, b.Progress, b.ExitCode}
}

func apiJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, code, msg, hint string) {
	body := map[string]string{"error": msg, "code": code}
	if hint != "" {
		body["hint"] = hint
	}
	apiJSON(w, status, body)
}

// apiFail answers an error of a service call and reports whether it did.
func apiFail(w http.ResponseWriter, err error) bool {
	var inv *InvalidError
	var paused *BuildsPausedError
	var taken *BuildIDTakenError
	var needLease *NodeLeaseRequiredError
	var unknown *UnknownNodeError
	var badCursor *InvalidCursorError
	switch {
	case err == nil:
		return false
	case errors.As(err, &inv):
		apiError(w, 400, "invalid_request", inv.Reason, "GET "+SpecPath+" describes the fields of each endpoint")
	case errors.As(err, &paused):
		apiError(w, 409, BuildsPausedCode, paused.Error(),
			"new builds are refused on this node while paused; resume with POST "+BasePath+"/nodes/"+paused.Node+`/builds-pause {"paused": false}`)
	case errors.As(err, &needLease):
		apiError(w, 409, NodeLeaseRequiredCode, needLease.Error(),
			`target "`+needLease.Node+`" is a transcode node: its own worker registers the lease and the record follows (send via="node_lease" only from that worker). To record a build on another machine use a target that is not a configured node; GET `+BasePath+"/nodes lists the nodes")
	case errors.As(err, &taken):
		apiError(w, 409, BuildIDTakenCode, taken.Error(), "client_id must be unique per build; reuse it only to retry the same create (same target and source)")
	case errors.As(err, &badCursor):
		apiError(w, 400, "invalid_cursor", "the cursor is not one this listing gave", "restart without cursor; a cursor is the next_cursor of the previous page")
	case errors.As(err, &unknown):
		apiError(w, 404, "unknown_node", unknown.Error(), "GET "+BasePath+"/nodes lists the configured nodes")
	case errors.Is(err, ErrBuildNotFound):
		apiError(w, 404, "build_not_found", "build not found", "GET "+BasePath+"/builds lists builds (running first, then recent history)")
	default:
		apiError(w, 500, "internal", "the request could not be completed", "")
	}
	return true
}

func apiDecode(w http.ResponseWriter, r *http.Request, v any, hint string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		msg := "the JSON body is not valid"
		if strings.Contains(err.Error(), "unknown field") {
			msg = err.Error()
		}
		apiError(w, 400, "invalid_request", msg, hint)
		return false
	}
	return true
}

func (s *Service) apiNodes(w http.ResponseWriter, r *http.Request) {
	var list []NodeView
	var err error
	withTranscode := true
	if raw := r.URL.Query().Get("transcode"); raw != "" {
		b, perr := strconv.ParseBool(raw)
		if perr != nil {
			apiError(w, 400, "invalid_request", "transcode must be true or false", "leave transcode out, or send transcode=false to skip the transcode block")
			return
		}
		withTranscode = b
	}
	if !withTranscode {
		list, err = s.NodesWithoutTranscode(r.Context())
	} else {
		list, err = s.Nodes(r.Context())
	}
	if apiFail(w, err) {
		return
	}
	apiJSON(w, 200, map[string]any{"nodes": list})
}

func (s *Service) apiNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.Node(r.Context(), r.PathValue("name"))
	if apiFail(w, err) {
		return
	}
	apiJSON(w, 200, n)
}

func (s *Service) apiBuildsPause(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused *bool `json:"paused"`
	}
	if !apiDecode(w, r, &body, `send {"paused": true} or {"paused": false}`) {
		return
	}
	if body.Paused == nil {
		apiError(w, 400, "invalid_request", "paused is required", `send {"paused": true} or {"paused": false}`)
		return
	}
	name := r.PathValue("name")
	// SetBuildsPause and Node resolve the node's canonical name (the
	// topology's) themselves, for the toggle and for the answer.
	if apiFail(w, s.SetBuildsPause(r.Context(), name, *body.Paused)) {
		return
	}
	n, err := s.Node(r.Context(), name)
	if apiFail(w, err) {
		return
	}
	apiJSON(w, 200, n)
}

// Page limits of GET /builds.
const (
	apiDefaultPage = 50
	apiMaxPage     = 500
)

func (s *Service) apiBuilds(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := apiDefaultPage
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > apiMaxPage {
			apiError(w, 400, "invalid_request", "limit must be between 1 and 500", "")
			return
		}
		limit = n
	}
	// One stable order for every page (newest created first, whatever the
	// status), so a build that changes status between pages is neither repeated
	// nor skipped. A cursor is the next_cursor of the previous page.
	page, err := s.PageBuilds(BuildFilter{Target: q.Get("node"), Status: q.Get("status"), Source: q.Get("source")}, q.Get("cursor"), limit)
	if apiFail(w, err) {
		return
	}
	out := make([]BuildV1, 0, len(page.Builds))
	for _, b := range page.Builds {
		out = append(out, viewBuild(b))
	}
	body := map[string]any{"builds": out}
	if page.Next != "" {
		body["next_cursor"] = page.Next
	}
	apiJSON(w, 200, body)
}

func (s *Service) apiCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID    string `json:"client_id"`
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
	hint := `send {"target", "source", "title", "client_id"?, "holder"?, "kind"?, "preempt_mode"?, "priority"?, "ttl_seconds"?, "progress"?}; the id is client_id here, not id`
	if !apiDecode(w, r, &body, hint) {
		return
	}
	b, err := s.CreateBuild(r.Context(), BuildInput{ID: body.ClientID, Target: body.Target, Source: body.Source, Holder: body.Holder, Kind: body.Kind,
		Title: body.Title, PreemptMode: body.PreemptMode, Priority: body.Priority, Progress: body.Progress, Via: body.Via, TTLSeconds: body.TTLSeconds})
	if apiFail(w, err) {
		return
	}
	apiJSON(w, 200, viewBuild(b))
}

func (s *Service) apiBuild(w http.ResponseWriter, r *http.Request) {
	b, err := s.GetBuild(r.PathValue("id"))
	if apiFail(w, err) {
		return
	}
	apiJSON(w, 200, viewBuild(b))
}

func buildEndedBody(w http.ResponseWriter, b Build) {
	apiJSON(w, 409, map[string]any{"error": "build already ended", "code": "build_ended",
		"hint": "the build is over (see build.status); a build cannot be resumed, register a new one", "build": viewBuild(b)})
}

func (s *Service) apiHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if b, err := s.OpenBuild(r.Context(), id); errors.Is(err, ErrBuildEnded) {
		buildEndedBody(w, b)
		return
	} else if apiFail(w, err) {
		return
	}
	var body struct {
		Progress   *string  `json:"progress"`
		TTLSeconds int      `json:"ttl_seconds"`
		Log        []string `json:"log"`
	}
	if r.ContentLength != 0 {
		if err := decodeOptional(w, r, &body); err != nil {
			apiError(w, 400, "invalid_request", "the JSON body is not valid", `send {"progress"?: string, "ttl_seconds"?: number, "log"?: [string]} or nothing`)
			return
		}
	}
	b, err := s.HeartbeatBuild(r.Context(), id, body.Progress, body.TTLSeconds, body.Log...)
	if errors.Is(err, ErrBuildEnded) {
		buildEndedBody(w, b)
		return
	}
	if apiFail(w, err) {
		return
	}
	apiJSON(w, 200, viewBuild(b))
}

// HeartbeatBodyMax is the largest heartbeat body read: BuildLogMaxBatch lines
// of BuildLogMaxLine characters, each escaped as a 12 byte surrogate pair
// (256*300*12 = 921,600 bytes), plus the progress and the other fields.
const HeartbeatBodyMax = 1 << 20

func decodeOptional(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, HeartbeatBodyMax)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *Service) apiFinish(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status   string `json:"status"`
		ExitCode *int   `json:"exit_code"`
	}
	if !apiDecode(w, r, &body, `send {"status": "succeeded"|"failed"|"cancelled", "exit_code"?}`) {
		return
	}
	b, _, err := s.FinishBuild(r.Context(), r.PathValue("id"), body.Status, body.ExitCode)
	if apiFail(w, err) {
		return
	}
	apiJSON(w, 200, viewBuild(b))
}

func (s *Service) apiCancel(w http.ResponseWriter, r *http.Request) {
	b, _, err := s.CancelBuild(r.Context(), r.PathValue("id"))
	if apiFail(w, err) {
		return
	}
	apiJSON(w, 200, viewBuild(b))
}

// LogLine is one numbered line of a build's output.
type LogLine struct {
	N    int    `json:"n"`
	Text string `json:"text"`
}

func (s *Service) apiLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after := 0
	if v := q.Get("after"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			apiError(w, 400, "invalid_request", "after must be a whole number, 0 or more", "after is the n of the last line you have seen (next_after of the previous answer)")
			return
		}
		after = n
	}
	wait := 0
	if v := q.Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > BuildLogMaxWait {
			apiError(w, 400, "invalid_request", "wait must be a whole number of seconds from 0 to "+strconv.Itoa(BuildLogMaxWait), "wait=S holds the request until a new line arrives or the build ends, at most "+strconv.Itoa(BuildLogMaxWait)+" seconds; omit it to answer at once")
			return
		}
		wait = n
	}
	id := r.PathValue("id")
	gen := q.Get("generation")
	var out BuildOutput
	var err error
	if wait > 0 {
		var tooMany bool
		out, err, tooMany = s.waitBuildOutput(r.Context(), id, after, gen, time.Duration(wait)*logWaitUnit)
		if tooMany {
			w.Header().Set("Retry-After", "1")
			apiError(w, 429, "too_many_waiters", "too many readers are waiting for build output", "read without wait (poll) or try again shortly")
			return
		}
	} else {
		out, err = s.BuildOutput(id)
	}
	if apiFail(w, err) {
		return
	}
	// Numbers restart at 1 whenever the buffer is recreated (a restart, an
	// eviction, the retention). The reader's position is stale when it names
	// another generation of the buffer or a line past the end: answer from the
	// start and say so.
	reset := after > out.Total || (gen != "" && gen != out.Generation)
	if reset {
		after = 0
	}
	lines := make([]LogLine, 0, len(out.Lines))
	for i, text := range out.Lines {
		if n := out.First + i; n > after {
			lines = append(lines, LogLine{N: n, Text: text})
		}
	}
	next := after
	if out.Total > next {
		next = out.Total
	}
	var updated, ended any
	if !out.Updated.IsZero() {
		updated = out.Updated.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if out.Build.EndedAt != "" {
		ended = out.Build.EndedAt
	}
	apiJSON(w, 200, map[string]any{
		"lines": lines, "next_after": next, "first_available": out.First, "generation": out.Generation, "reset": reset,
		"status": out.Build.Status, "updated_at": updated, "ended_at": ended, "expired": out.Expired,
	})
}

// logWaitUnit is the length of one second of the long-poll's wait (a test seam).
var logWaitUnit = time.Second

// waitBuildOutput is BuildOutput that, while the reader has nothing new, holds
// the call until a line arrives, the build ends, the buffer is dropped or
// recreated, ctx ends or max passes. It then answers like BuildOutput. tooMany
// is true when the cap of waiting readers is reached (nothing was read).
func (s *Service) waitBuildOutput(ctx context.Context, id string, after int, gen string, max time.Duration) (out BuildOutput, err error, tooMany bool) {
	startGen := ""
	ready := func(out BuildOutput) bool {
		return !out.Build.Active() || out.Expired ||
			out.Total > after || after > out.Total ||
			(gen != "" && gen != out.Generation) || // as the answer without wait: a reset
			(startGen != "" && out.Generation != startGen)
	}
	// Answer at once, without a waiter slot, when there is no reason to wait
	// (lines already past after, build ended, unknown build): the cap only
	// limits readers that actually wait.
	out, err = s.BuildOutput(id)
	if err != nil || ready(out) {
		return out, err, false
	}
	startGen = out.Generation
	release, werr := s.logs.watch(id)
	if werr != nil {
		return BuildOutput{}, nil, true
	}
	defer release()
	timer := time.NewTimer(max)
	defer timer.Stop()
	for {
		// Take the channel before reading: a change after the read closes it.
		ch := s.logs.current(id)
		out, err = s.BuildOutput(id)
		if err != nil || ready(out) {
			return out, err, false
		}
		select {
		case <-ch:
		case <-timer.C:
			return out, nil, false
		case <-ctx.Done():
			return out, nil, false
		}
	}
}
