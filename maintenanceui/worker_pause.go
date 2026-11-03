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
// back to the transcode queue without cancelling the conversion, and "off"
// resumes the node. Repeating a request changes nothing.
func (s *Server) workerPause(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Node string `json:"node"`
			Mode string `json:"mode"`
		}
		mode := ""
		if decode(w, r, &body) == nil {
			switch body.Mode {
			case store.WorkerPauseGraceful, store.WorkerPauseImmediate:
				mode = body.Mode
			case "off":
			default:
				body.Node = ""
			}
		} else {
			body.Node = ""
		}
		if body.Node == "" {
			fail(w, 400, `send {"node": "<name>", "mode": "graceful"|"immediate"|"off"}`)
			return
		}
		status, err := s.engine.SetNodePause(r.Context(), body.Node, mode)
		var unknown *transcode.WorkerNotConfiguredError
		switch {
		case errors.As(err, &unknown):
			fail(w, 404, "that node is not configured")
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
