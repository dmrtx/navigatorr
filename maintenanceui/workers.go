package maintenanceui

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/action"
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
	configured := s.engine.Deps().Transcode != nil
	status, message := "unconfigured", "Configure a video worker before submitting jobs."
	ready := false
	connected := false
	if configured {
		if err := s.engine.CheckWorkerAdmission(r.Context()); err == nil {
			connected, ready, status, message = true, true, "ready", "Connected. Media storage and encoder checks passed."
		} else {
			status, message = "unavailable", "Worker offline or not ready. New jobs are blocked."
			var admission *action.WorkerAdmissionError
			if errors.As(err, &admission) && admission.Connected {
				connected, status, message = true, "blocked", "Connected, but media storage or encoder checks failed. New jobs are blocked."
				if strings.Contains(admission.Message, "media storage") {
					message = "Connected, but media storage is inaccessible. New jobs are blocked."
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"ready": ready, "checked_at": time.Now().UTC(), "nodes": []map[string]any{{"name": "Video worker", "address": address, "transport": transport, "configured": configured, "connected": connected, "ready": ready, "status": status, "message": message}}})
}
