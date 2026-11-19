package maintenanceui

import (
	"context"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// activityProbeTimeout bounds each worker's live read for the activity view. A
// Mac that is busy with a build can take long to answer; the page must not wait
// for it (the executor's own deadline is 15 s), because the builds and holds it
// shows come from the server's store and are known at once.
var activityProbeTimeout = 5 * time.Second

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

// nodeStoreState is what the server itself knows about a node without asking
// its worker: the builds running or waiting on it, what keeps new files off it
// and whether new builds are refused. It reads stored state only, so it is
// immediate and does not depend on the node answering.
func (s *Server) nodeStoreState(name string) map[string]any {
	state := map[string]any{}
	// The builds running or waiting on this node, so the activity view can say why it is busy.
	// This is the registry's view (the build records); the node's worker is
	// what actually holds the node for them.
	builds := s.engine.NodeBuildRecords(name)
	if len(builds) > 0 {
		state["builds"] = builds
	}
	// What keeps new files off the node, so the slot count can leave it out:
	// the operator's pause, else a running build on it.
	if held := s.engine.NodeHeldBy(name); held != "" {
		state["held_by"] = held
	} else {
		for _, b := range builds {
			if b.Status == store.BuildRunning {
				state["held_by"] = "builds"
				break
			}
		}
	}
	// Whether new builds on the node are refused, shown beside its slots.
	if s.engine.NodeBuildsPaused(name) {
		state["builds_paused"] = true
	}
	return state
}

// workerNodeState answers, for every worker, only what the server knows from
// its store (builds, hold, builds pause). The page polls it every second or two
// so a build starting, ending or being cancelled, or a pause, shows at once,
// while the slower worker-activity read keeps the slots.
func (s *Server) workerNodeState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workers := s.engine.Deps().Workers.All()
	nodes := make([]map[string]any, len(workers))
	for i, worker := range workers {
		node := s.nodeStoreState(worker.Name)
		node["name"] = worker.Name
		nodes[i] = node
	}
	writeJSON(w, 200, map[string]any{"nodes": nodes, "at": time.Now().UTC()})
}

func (s *Server) nodeActivity(ctx context.Context, worker transcode.NamedWorker) map[string]any {
	node := map[string]any{"name": worker.Name, "address": worker.Address, "podcast": worker.Podcast, "available": false}
	maps.Copy(node, s.nodeStoreState(worker.Name))
	reader, ok := worker.Executor.(workerActivityReader)
	if !ok {
		node["message"] = "Live worker activity requires the HTTP video worker."
		return node
	}
	probe, cancel := context.WithTimeout(ctx, activityProbeTimeout)
	defer cancel()
	activity, err := reader.Activity(probe)
	if err != nil {
		node["message"] = "Worker activity unavailable. Slot occupancy is unknown."
		return node
	}
	// Pushing the saved slot split back is bounded the same way: it must not hold the answer either.
	syncCtx, syncCancel := context.WithTimeout(ctx, activityProbeTimeout)
	defer syncCancel()
	s.syncWorkerSlots(syncCtx, worker, activity.Slots)
	s.describeActivityJobs(&activity)
	node["available"], node["activity"] = true, activity
	return node
}

// workers lists every configured worker as a node, with the summary of them
// all at the top.
func (s *Server) workers(w http.ResponseWriter, r *http.Request) {
	transport := s.cfg.Transcode.Executor
	observation := s.engine.WorkerObservation(r.Context())
	nodes := []map[string]any{}
	pauses := s.engine.AttachNodeBuilds(s.engine.NodePauseStatuses(r.Context()))
	for _, o := range observation.Workers {
		address := ""
		if worker, ok := s.engine.Deps().Workers.Get(o.Name); ok {
			address = worker.Address
		}
		node := workerNode(o, address, transport)
		if pause, ok := pauses[o.Name]; ok {
			node["pause"] = pause
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		nodes = append(nodes, workerNode(observation, "", transport))
	}
	writeJSON(w, 200, map[string]any{"ready": observation.Ready, "checked_at": observation.CheckedAt, "message": observation.Message, "observation": observation, "drain": observation.Drain, "nodes": nodes})
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
