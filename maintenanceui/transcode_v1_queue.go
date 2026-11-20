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
	switch {
	case raw[0] == '"':
		if err := json.Unmarshal(raw, &id); err != nil {
			return action.QueueRef{}, "id must be a string or a number"
		}
	case q.Kind == action.QueueKindItem:
		id = string(raw)
	default:
		return action.QueueRef{}, "the id of an action or a batch must be a string"
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return action.QueueRef{}, "id is required"
	}
	if q.Kind == action.QueueKindItem {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n <= 0 {
			return action.QueueRef{}, "the id of an item is its number"
		}
		// "01" and "+1" are the item 1.
		id = strconv.FormatInt(n, 10)
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

// planHint is the endpoint that would change a reason, when there is one.
func planHint(code, kind, id, batchID, node string) string {
	const b = transcodeapi.BasePath
	switch code {
	case action.ReasonHeld:
		if kind == action.QueueKindItem {
			return " Release it with POST " + b + "/queue/items/" + id + "/release."
		}
		return " Release it with POST " + b + "/queue/actions/" + id + "/release."
	case action.ReasonBatchHeld:
		if batchID != "" {
			return " Release the batch with POST " + b + "/queue/batches/" + batchID + "/release."
		}
	case action.ReasonNodePaused:
		return " Resume the node with DELETE " + b + "/pauses/" + node + "."
	case action.ReasonBatchGating, action.ReasonSlotTakenAhead:
		return " Move it with POST " + b + "/queue/" + kind + "s/" + id + "/move."
	case action.ReasonNodeWindow, action.ReasonMediaType, action.ReasonNoFit,
		action.ReasonNoProbe, action.ReasonNoHistory, action.ReasonWindowUnreadable:
		return " Change the node's window with PUT " + b + "/workers/" + node + "/window or DELETE the same path."
	}
	return ""
}

func planReasonOut(code, text, hint string) map[string]any {
	return map[string]any{"code": code, "reason": text + hint}
}

// v1GetQueueNext explains what would start next on each node. It only reads:
// the answer comes from the code dispatch uses (action.QueueNext) and claims
// nothing.
func (s *Server) v1GetQueueNext(w http.ResponseWriter, r *http.Request) {
	plan, err := s.engine.QueueNext(r.Context())
	if err != nil {
		v1QueueError(w, err)
		return
	}
	nodes := make([]map[string]any, 0, len(plan.Nodes))
	for _, n := range plan.Nodes {
		out := map[string]any{
			"worker":            n.Worker,
			"state":             n.State,
			"free_slots":        n.FreeSlots,
			"next":              nil,
			"skipped":           []map[string]any{},
			"skipped_truncated": n.SkippedTruncated,
		}
		if n.Next != nil {
			next := map[string]any{
				"ref":      queueRefOut(n.Next.Kind, n.Next.ID),
				"position": n.Next.Position,
				"path":     n.Next.Path,
			}
			if n.Next.MediaType != "" {
				next["media_type"] = n.Next.MediaType
			}
			if n.Next.BatchID != "" {
				next["batch_id"] = n.Next.BatchID
			}
			if n.Next.HasEstimate {
				next["estimate_sec"] = n.Next.EstimateSec
			}
			out["next"] = next
		}
		if n.WhyNone != nil {
			out["why_none"] = planReasonOut(n.WhyNone.Code, n.WhyNone.Text, planHint(n.WhyNone.Code, "", "", "", n.Worker))
		}
		skipped := make([]map[string]any, 0, len(n.Skipped))
		for _, sk := range n.Skipped {
			skipped = append(skipped, map[string]any{
				"ref":         queueRefOut(sk.Kind, sk.ID),
				"position":    sk.Position,
				"reason_code": sk.Code,
				"reason":      sk.Text + planHint(sk.Code, sk.Kind, sk.ID, sk.BatchID, n.Worker),
			})
		}
		out["skipped"] = skipped
		nodes = append(nodes, out)
	}
	transcodeapi.WriteJSON(w, 200, map[string]any{"queue_revision": plan.Revision, "nodes": nodes})
}

func (s *Server) transcodeQueueHandlers(h transcodeapi.Handlers) {
	const b = transcodeapi.BasePath
	h["GET "+b+"/queue"] = s.v1GetQueue
	h["GET "+b+"/queue/next"] = s.v1GetQueueNext
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
