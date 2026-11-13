package maintenanceui

import (
	"errors"
	"net/http"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// workerPause serves GET (every node's pause, or one with ?node=) and POST
// (pause or resume one node). A paused node takes no new job so its CPU is free
// for other work: "graceful" lets what is encoding finish, "immediate" sends it
// back to the transcode queue without cancelling the conversion, "suspend"
// freezes what is encoding so it continues where it was, and "off" resumes the
// node. A separate request, {"node", "builds_paused": bool}, toggles the
// independent builds pause: the server refuses new builds on the node while it
// is on. Repeating a request changes nothing.
func (s *Server) workerPause(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Node string  `json:"node"`
			Mode *string `json:"mode"`
			// BuildsPaused is the independent builds switch: true refuses new
			// builds on the node, false lets them in again. It cannot be sent
			// together with a mode (by presence, even an empty one), so one request never half applies.
			BuildsPaused *bool `json:"builds_paused"`
		}
		mode := ""
		if decode(w, r, &body) == nil {
			switch {
			case body.BuildsPaused != nil && body.Mode != nil:
				body.Node = ""
			case body.BuildsPaused != nil:
			default:
				given := ""
				if body.Mode != nil {
					given = *body.Mode
				}
				switch given {
				case store.WorkerPauseGraceful, store.WorkerPauseImmediate, store.WorkerPauseSuspend:
					mode = given
				case "off":
				default:
					body.Node = ""
				}
			}
		} else {
			body.Node = ""
		}
		if body.Node == "" {
			fail(w, 400, `send {"node": "<name>", "mode": "graceful"|"immediate"|"suspend"|"off"} or {"node": "<name>", "builds_paused": true|false}`)
			return
		}
		var status transcode.NodePauseStatus
		var err error
		if body.BuildsPaused != nil {
			status, err = s.engine.SetNodeBuildsPause(r.Context(), body.Node, *body.BuildsPaused)
		} else {
			status, err = s.engine.SetNodePause(r.Context(), body.Node, mode)
		}
		var unknown *transcode.WorkerNotConfiguredError
		switch {
		case errors.As(err, &unknown):
			fail(w, 404, "that node is not configured")
		case errors.Is(err, transcode.ErrSuspendUnavailable):
			fail(w, 409, err.Error())
		case errors.Is(err, transcode.ErrSuspendNotUndone), errors.Is(err, transcode.ErrSuspendReleasePending):
			// The pause is stored, as suspend: not "not saved".
			fail(w, 500, err.Error())
		case err != nil:
			fail(w, 500, "node pause not saved")
		default:
			writeJSON(w, 200, status)
		}
		return
	}
	statuses := s.engine.NodePauseStatuses(r.Context())
	if node := r.URL.Query().Get("node"); node != "" {
		for _, status := range statuses {
			if status.Node == node {
				writeJSON(w, 200, status)
				return
			}
		}
		fail(w, 404, "that node is not configured")
		return
	}
	writeJSON(w, 200, map[string]any{"nodes": statuses})
}
