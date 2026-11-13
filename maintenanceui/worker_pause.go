package maintenanceui

import (
	"encoding/json"
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
		// The two switches are read key-wise, by presence in any form (an empty
		// string or null counts), the same rule as the MCP tool: one request
		// flips one switch, so it never half applies.
		var body struct {
			Node         string          `json:"node"`
			Mode         json.RawMessage `json:"mode"`
			BuildsPaused json.RawMessage `json:"builds_paused"`
		}
		mode := ""
		var builds *bool
		if decode(w, r, &body) == nil {
			hasMode, hasBuilds := len(body.Mode) > 0, len(body.BuildsPaused) > 0
			switch {
			case hasMode && hasBuilds:
				body.Node = ""
			case hasBuilds:
				// Only a JSON true or false: null or any other type is refused.
				switch string(body.BuildsPaused) {
				case "true":
					v := true
					builds = &v
				case "false":
					v := false
					builds = &v
				default:
					body.Node = ""
				}
			default:
				var given string
				if hasMode && json.Unmarshal(body.Mode, &given) != nil {
					given = "?"
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
		if builds != nil {
			status, err = s.engine.SetNodeBuildsPause(r.Context(), body.Node, *builds)
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
