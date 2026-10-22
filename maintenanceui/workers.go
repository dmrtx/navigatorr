package maintenanceui

import (
	"context"
	"net/http"
	"sync"

	"github.com/jakenesler/navigatorr/transcode"
)

type workerActivityReader interface {
	Activity(ctx context.Context) (transcode.WorkerActivity, error)
}

// workerActivity reports every worker's slots and jobs, read at once so an
// offline worker never delays the others. activity is the first available
// worker's, for a page that shows one node.
func (s *Server) workerActivity(w http.ResponseWriter, r *http.Request) {
	workers := s.engine.Deps().Workers.All()
	if len(workers) == 0 {
		writeJSON(w, 200, map[string]any{"available": false, "message": "Live worker activity requires the HTTP video worker."})
		return
	}
	nodes := make([]map[string]any, len(workers))
	var wg sync.WaitGroup
	for i, worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nodes[i] = s.nodeActivity(r.Context(), worker)
		}()
	}
	wg.Wait()
	out := map[string]any{"available": false, "message": "Worker activity unavailable. Slot occupancy is unknown.", "workers": nodes}
	for _, node := range nodes {
		if node["available"] == true {
			out["available"], out["activity"] = true, node["activity"]
			delete(out, "message")
			break
		}
	}
	if len(nodes) == 1 && nodes[0]["message"] != nil {
		out["message"] = nodes[0]["message"]
	}
	writeJSON(w, 200, out)
}

func (s *Server) nodeActivity(ctx context.Context, worker transcode.NamedWorker) map[string]any {
	node := map[string]any{"name": worker.Name, "address": worker.Address, "podcast": worker.Podcast, "available": false}
	reader, ok := worker.Executor.(workerActivityReader)
	if !ok {
		node["message"] = "Live worker activity requires the HTTP video worker."
		return node
	}
	activity, err := reader.Activity(ctx)
	if err != nil {
		node["message"] = "Worker activity unavailable. Slot occupancy is unknown."
		return node
	}
	s.syncWorkerSlots(ctx, worker, activity.Slots)
	node["available"], node["activity"] = true, activity
	return node
}

// workers lists every configured worker as a node, with the summary of them
// all at the top.
func (s *Server) workers(w http.ResponseWriter, r *http.Request) {
	transport := s.cfg.Transcode.Executor
	observation := s.engine.WorkerObservation(r.Context())
	nodes := []map[string]any{}
	for _, o := range observation.Workers {
		address := ""
		if worker, ok := s.engine.Deps().Workers.Get(o.Name); ok {
			address = worker.Address
		}
		nodes = append(nodes, workerNode(o, address, transport))
	}
	if len(nodes) == 0 {
		nodes = append(nodes, workerNode(observation, "", transport))
	}
	writeJSON(w, 200, map[string]any{"ready": observation.Ready, "checked_at": observation.CheckedAt, "message": observation.Message, "observation": observation, "nodes": nodes})
}

func workerNode(o transcode.WorkerObservation, address, transport string) map[string]any {
	status := "unconfigured"
	if o.Configured {
		status = "unavailable"
		if o.Reachable {
			status = "blocked"
		}
		if o.Ready {
			status = "ready"
		}
	}
	name := o.Name
	if name == "" || name == transcode.DefaultWorkerName {
		name = "Video worker"
	}
	return map[string]any{"name": name, "worker": o.Name, "podcast": o.Podcast, "address": address, "transport": transport, "configured": o.Configured, "connected": o.Reachable, "reachable": o.Reachable, "ready": o.Ready, "status": status, "message": o.Message, "admission_note": o.AdmissionNote, "scheduler_health": o.Health, "observed_at": o.ObservedAt, "fresh_until": o.FreshUntil, "last_success_at": o.LastSuccessAt, "observation_age_seconds": o.ObservationAgeSeconds, "stale": o.Stale, "last_error": o.LastError, "sweeps": o.Sweeps}
}
