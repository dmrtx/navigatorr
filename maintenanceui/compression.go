package maintenanceui

import (
	"net/http"

	"github.com/jakenesler/navigatorr/action"
)

// compression serves the "How we compress" page: each goal as a new job would
// freeze it now, and the dated changes.
func (s *Server) compression(w http.ResponseWriter, _ *http.Request) {
	today, err := action.CompressionPolicyToday()
	if err != nil {
		fail(w, 500, "describe the goals: "+err.Error())
		return
	}
	writeJSON(w, 200, today)
}
