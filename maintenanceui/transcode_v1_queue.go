package maintenanceui

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/transcodeapi"
)

// The queue of /transcode/v1: GET /queue reads the waiting conversions in
// order, and hold, release and move act on one conversion (an action or a
// queued batch item) or on a whole batch. The work is the engine's
// (action.TranscodeQueueView, HoldQueued, ReleaseQueued, MoveQueued); this
// file only maps the HTTP request and the engine's refusals.

const hintQueue = "GET " + transcodeapi.BasePath + "/queue lists what is waiting, in order, with the ref of each entry"

// queueRefJSON is a reference as it is sent and shown: a kind and an id. An
// action or a batch has a string id, an item a number (a numeric string is
// accepted too).
type queueRefJSON struct {
	Kind string          `json:"kind"`
	ID   json.RawMessage `json:"id"`
}

func (q queueRefJSON) toRef() (action.QueueRef, string) {
	switch q.Kind {
	case action.QueueKindAction, action.QueueKindItem, action.QueueKindBatch:
	default:
		return action.QueueRef{}, `kind must be "action", "item" or "batch"`
	}
	raw := bytes.TrimSpace(q.ID)
	if len(raw) == 0 || string(raw) == "null" {
		return action.QueueRef{}, "id is required"
	}
	var id string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &id); err != nil {
			return action.QueueRef{}, "id must be a string or a number"
		}
	} else {
		id = string(raw)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return action.QueueRef{}, "id is required"
	}
	if q.Kind == action.QueueKindItem {
		if n, err := strconv.ParseInt(id, 10, 64); err != nil || n <= 0 {
			return action.QueueRef{}, "the id of an item is its number"
		}
	}
	return action.QueueRef{Kind: q.Kind, ID: id}, ""
}

func queueRefOut(kind, id string) map[string]any {
	if kind == action.QueueKindItem {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			return map[string]any{"kind": kind, "id": n}
		}
	}
	return map[string]any{"kind": kind, "id": id}
}

func queueEntryOut(en action.QueueEntry) map[string]any {
	out := map[string]any{
		"ref":               queueRefOut(en.Kind, en.ID),
		"position":          en.Position,
		"held":              en.Held,
		"path":              en.Path,
		"waiting_condition": en.WaitingCondition,
		"waiting_reason":    en.WaitingReason,
	}
	if en.HeldBy != "" {
		out["held_by"] = en.HeldBy
	}
	if en.MediaType != "" {
		out["media_type"] = en.MediaType
	}
	if en.BatchID != "" {
		out["batch_id"] = en.BatchID
	}
	return out
}

func queueResultOut(res action.QueueResult) map[string]any {
	out := map[string]any{"queue_revision": res.Revision}
	switch {
	case res.Entry != nil:
		out["entry"] = queueEntryOut(*res.Entry)
	case res.Batch != nil:
		out["batch"] = map[string]any{
			"id":             res.Batch.ID,
			"held":           res.Batch.Held,
			"waiting":        res.Batch.Waiting,
			"first_position": res.Batch.FirstPosition,
		}
	}
	return out
}

// v1QueueError answers a refusal of the engine: 404, 409 or 400 with its own
// words, and 500 (logged, not shown) for anything else.
func v1QueueError(w http.ResponseWriter, err error) {
	var qe *action.QueueError
	if !errors.As(err, &qe) {
		log.Printf("transcode v1: queue: %v", err)
		transcodeapi.WriteError(w, 500, "internal", "the request could not be completed", "")
		return
	}
	switch qe.Kind {
	case action.QueueNotFound:
		transcodeapi.WriteError(w, 404, "queue_entry_not_found", qe.Msg, hintQueue)
	case action.QueueConflict:
		code := "queue_conflict"
		if strings.Contains(qe.Msg, "queue_revision") {
			code = "stale_queue_revision"
		}
		transcodeapi.WriteError(w, 409, code, qe.Msg, hintQueue)
	default:
		transcodeapi.WriteError(w, 400, "invalid_request", qe.Msg, hintQueue)
	}
}

func (s *Server) v1GetQueue(w http.ResponseWriter, r *http.Request) {
	v, err := s.engine.TranscodeQueueView()
	if err != nil {
		v1QueueError(w, err)
		return
	}
	entries := make([]map[string]any, 0, len(v.Entries))
	for _, en := range v.Entries {
		entries = append(entries, queueEntryOut(en))
	}
	transcodeapi.WriteJSON(w, 200, map[string]any{"queue_revision": v.Revision, "entries": entries})
}

// queueSubjectRef is the reference a route names: kind from the route, id from
// the path.
func queueSubjectRef(w http.ResponseWriter, kind string, r *http.Request) (action.QueueRef, bool) {
	ref, bad := queueRefJSON{Kind: kind, ID: json.RawMessage(strconv.Quote(r.PathValue("id")))}.toRef()
	if bad != "" {
		v1Bad(w, bad, hintQueue)
		return action.QueueRef{}, false
	}
	return ref, true
}

type queueRevisionBody struct {
	QueueRevision *int64 `json:"queue_revision"`
}

type queueMoveBody struct {
	First         bool          `json:"first"`
	Before        *queueRefJSON `json:"before"`
	After         *queueRefJSON `json:"after"`
	QueueRevision *int64        `json:"queue_revision"`
}

func (s *Server) queueHold(kind string, hold bool) http.HandlerFunc {
	return withBody(false, func(w http.ResponseWriter, r *http.Request, body queueRevisionBody) {
		ref, ok := queueSubjectRef(w, kind, r)
		if !ok {
			return
		}
		var res action.QueueResult
		var err error
		if hold {
			res, err = s.engine.HoldQueued(r.Context(), ref, body.QueueRevision)
		} else {
			res, err = s.engine.ReleaseQueued(r.Context(), ref, body.QueueRevision)
		}
		if err != nil {
			v1QueueError(w, err)
			return
		}
		transcodeapi.WriteJSON(w, 200, queueResultOut(res))
	})
}

func (s *Server) queueMove(kind string) http.HandlerFunc {
	return withBody(true, func(w http.ResponseWriter, r *http.Request, body queueMoveBody) {
		ref, ok := queueSubjectRef(w, kind, r)
		if !ok {
			return
		}
		n := 0
		for _, set := range []bool{body.First, body.Before != nil, body.After != nil} {
			if set {
				n++
			}
		}
		if n != 1 {
			v1Bad(w, `send exactly one of {"first": true}, {"before": {"kind": ..., "id": ...}} and {"after": {"kind": ..., "id": ...}}`, hintQueue)
			return
		}
		to := action.QueueMove{First: body.First}
		for _, p := range []struct {
			in  *queueRefJSON
			out **action.QueueRef
			nm  string
		}{{body.Before, &to.Before, "before"}, {body.After, &to.After, "after"}} {
			if p.in == nil {
				continue
			}
			ref, bad := p.in.toRef()
			if bad != "" {
				v1Bad(w, p.nm+": "+bad, hintQueue)
				return
			}
			*p.out = &ref
		}
		res, err := s.engine.MoveQueued(r.Context(), ref, to, body.QueueRevision)
		if err != nil {
			v1QueueError(w, err)
			return
		}
		transcodeapi.WriteJSON(w, 200, queueResultOut(res))
	})
}

func (s *Server) transcodeQueueHandlers(h transcodeapi.Handlers) {
	const b = transcodeapi.BasePath
	h["GET "+b+"/queue"] = s.v1GetQueue
	for _, kind := range []struct{ path, kind string }{
		{"/queue/actions/{id}", action.QueueKindAction},
		{"/queue/items/{id}", action.QueueKindItem},
		{"/queue/batches/{id}", action.QueueKindBatch},
	} {
		h["POST "+b+kind.path+"/hold"] = s.queueHold(kind.kind, true)
		h["POST "+b+kind.path+"/release"] = s.queueHold(kind.kind, false)
		h["POST "+b+kind.path+"/move"] = s.queueMove(kind.kind)
	}
}
