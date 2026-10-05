package maintenanceui

import (
	"github.com/jakenesler/navigatorr/transcode"
	"net/http"
	"strconv"
)

// Resolve a known action first. The authenticated browser cannot supply a
// worker URL, token, job identity, or artifact path.
func (s *Server) benchmarkComparison(w http.ResponseWriter, r *http.Request) {
	inst, err := s.engine.Deps().Store.GetActionInstance(r.URL.Query().Get("id"))
	if err != nil || (inst.ActionName != "benchmark_transcode" && inst.ActionName != "transcode_media") {
		fail(w, 404, "benchmark action not found")
		return
	}
	state := decodeOperationJSON(inst.StateJSON)
	id, _ := state["benchmark_job_id"].(string)
	if transcode.ValidateBenchmarkJobID(id) != nil {
		fail(w, 404, "this benchmark has no saved comparison")
		return
	}
	executor, ok := s.engine.Deps().Transcode.(*transcode.HTTPExecutor)
	if !ok {
		fail(w, 404, "visual comparisons require the HTTP video worker")
		return
	}
	if raw := r.URL.Query().Get("image"); raw != "" {
		index, err := strconv.Atoi(raw)
		side := r.URL.Query().Get("side")
		if err != nil || index < 0 || index > 100 || (side != "original" && side != "candidate") {
			fail(w, 400, "invalid comparison image")
			return
		}
		data, err := executor.BenchmarkComparisonImage(r.Context(), id, index, side)
		if err != nil {
			fail(w, 502, "comparison image unavailable; check the video worker or reopen the comparison")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
		return
	}
	manifest, err := executor.BenchmarkComparison(r.Context(), id)
	if err != nil {
		fail(w, 404, "comparison unavailable; this benchmark's images were not saved or have expired")
		return
	}
	writeJSON(w, 200, manifest)
}
