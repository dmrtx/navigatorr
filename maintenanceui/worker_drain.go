package maintenanceui

import "net/http"

// workerDrain serves GET (the drain status) and POST (turn drain on or off).
// While drain is on no new worker job is admitted; the jobs already on a
// worker finish and publish, and the status says when every worker is idle.
// Turning it off is the only way it ends, and it never resumes a paused batch.
func (s *Server) workerDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if decode(w, r, &body) != nil || body.Enabled == nil {
			fail(w, 400, `send {"enabled": true} to drain the workers or {"enabled": false} to resume`)
			return
		}
		status, err := s.engine.SetWorkerDrain(r.Context(), *body.Enabled)
		if err != nil {
			fail(w, 500, "worker drain not saved")
			return
		}
		writeJSON(w, 200, status)
		return
	}
	writeJSON(w, 200, s.engine.WorkerDrainStatus(r.Context()))
}
