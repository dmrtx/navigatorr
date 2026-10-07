package maintenanceui

import (
	"net/http"
	"net/url"

	"github.com/jakenesler/navigatorr/transcode"
)

func (s *Server) workerActivity(w http.ResponseWriter, r *http.Request) {
	executor, ok := s.engine.Deps().Transcode.(*transcode.HTTPExecutor)
	if !ok {
		writeJSON(w, 200, map[string]any{"available": false, "message": "Live worker activity requires the HTTP video worker."})
		return
	}
	activity, err := executor.Activity(r.Context())
	if err != nil {
		writeJSON(w, 200, map[string]any{"available": false, "message": "Worker activity unavailable. Slot occupancy is unknown."})
		return
	}
	writeJSON(w, 200, map[string]any{"available": true, "activity": activity})
}

func (s *Server) workers(w http.ResponseWriter, r *http.Request) {
	transport := s.cfg.Transcode.Executor
	address := s.cfg.Transcode.SSH.Host
	if transport == "http" {
		if endpoint, err := url.Parse(s.cfg.Transcode.EffectiveHTTPConfig().BaseURL); err == nil {
			address = endpoint.Host
		}
	}
	observation := s.engine.WorkerObservation(r.Context())
	status := "unconfigured"
	if observation.Configured {
		status = "unavailable"
		if observation.Reachable {
			status = "blocked"
		}
		if observation.Ready {
			status = "ready"
		}
	}
	node := map[string]any{"name": "Video worker", "address": address, "transport": transport, "configured": observation.Configured, "connected": observation.Reachable, "reachable": observation.Reachable, "ready": observation.Ready, "status": status, "message": observation.Message, "scheduler_health": observation.Health, "observed_at": observation.ObservedAt, "fresh_until": observation.FreshUntil, "last_success_at": observation.LastSuccessAt, "observation_age_seconds": observation.ObservationAgeSeconds, "stale": observation.Stale, "last_error": observation.LastError, "sweeps": observation.Sweeps}
	writeJSON(w, 200, map[string]any{"ready": observation.Ready, "checked_at": observation.CheckedAt, "observation": observation, "nodes": []map[string]any{node}})
}
