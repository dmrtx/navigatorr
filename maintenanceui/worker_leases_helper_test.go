package maintenanceui

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// reportedLeases is what the synthetic worker answers on /v1/health: the build
// leases the worker says it holds. Tests in this package do not run in
// parallel, so one pointer is enough.
var reportedLeases atomic.Pointer[[]transcode.WorkerLease]

// serveHealth answers /v1/health like a worker that reports its leases. It
// reports false for any other path so the caller keeps its own switch.
func serveHealth(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/v1/health" {
		return false
	}
	var leases []transcode.WorkerLease
	if p := reportedLeases.Load(); p != nil {
		leases = *p
	}
	writeJSON(w, 200, map[string]any{"ok": true, "scheduler": transcode.SchedulerObservation{Health: "ok", ObservedAt: time.Now().UTC(), Leases: leases}})
	return true
}

// reportLeases makes the synthetic worker report the leases and lets the
// server observe them (the observation is cached for a few seconds, so call
// it before anything else has observed the node in the test).
func reportLeases(t *testing.T, s *Server, leases ...transcode.WorkerLease) {
	t.Helper()
	reportedLeases.Store(&leases)
	t.Cleanup(func() { reportedLeases.Store(nil) })
	s.engine.WorkerObservation(t.Context())
}

func runningLease(id string) transcode.WorkerLease {
	return transcode.WorkerLease{ID: id, Title: "compile", Source: "ci", Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339)}
}
