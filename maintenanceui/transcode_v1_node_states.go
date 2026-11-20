package maintenanceui

import (
	"net/http"

	"github.com/jakenesler/navigatorr/transcodeapi"
)

// nodeStateView is one node of GET /transcode/v1/node-states.
type nodeStateView struct {
	Node    string `json:"node"`
	Paused  bool   `json:"paused"`
	Mode    string `json:"mode,omitempty"`
	Since   string `json:"since,omitempty"`
	HeldBy  string `json:"held_by,omitempty"`
	Message string `json:"message,omitempty"`
}

// v1NodeStates serves the transcode state of each node for a Nodes process
// that runs apart from this one. The engine copies the state and returns it
// before anything is written, so no engine lock is held while the response is
// written, and nothing here probes a worker or reads a build record.
func (s *Server) v1NodeStates(w http.ResponseWriter, r *http.Request) {
	states := s.engine.NodeTranscodeStates(r.Context())
	out := make([]nodeStateView, 0, len(states))
	for _, st := range states {
		out = append(out, nodeStateView{Node: st.Node, Paused: st.Paused, Mode: st.Mode, Since: st.Since, HeldBy: st.HeldBy, Message: st.Message})
	}
	transcodeapi.WriteJSON(w, 200, map[string]any{"nodes": out})
}
