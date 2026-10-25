package maintenanceui

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// slotSyncInterval spaces the pushes that bring a worker back to the stored
// split after a restart or a failed save, so a poll never hammers it.
const slotSyncInterval = 10 * time.Second

// workerSlotController is what an HTTP video worker offers for dividing its
// slots between video and podcast work.
type workerSlotController interface {
	Slots(ctx context.Context) (transcode.WorkerSlots, error)
	SetSlots(ctx context.Context, split transcode.SlotSplit) (transcode.WorkerSlots, []string, error)
}

// slotNode is the worker a slot request is about: the node query parameter,
// or the first worker. The per-node settings table is keyed by worker name.
func (s *Server) slotNode(r *http.Request) (transcode.NamedWorker, bool) {
	workers := s.engine.Deps().Workers
	if name := r.URL.Query().Get("node"); name != "" {
		return workers.Get(name)
	}
	all := workers.All()
	if len(all) == 0 {
		return transcode.NamedWorker{}, false
	}
	return all[0], true
}

// Configured is about the split; Ceiling 0 means the node follows its own
// max_parallel_jobs and X265Slots 0 means no limit on x265 encodes.
type workerSlotSettingsView struct {
	Configured   bool   `json:"configured"`
	VideoSlots   int    `json:"video_slots"`
	PodcastSlots int    `json:"podcast_slots"`
	Ceiling      int    `json:"ceiling"`
	X265Slots    int    `json:"x265_slots"`
	Revision     int64  `json:"revision"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

func slotSettingsView(row *store.WorkerSlotSettings) workerSlotSettingsView {
	if row == nil {
		return workerSlotSettingsView{}
	}
	return workerSlotSettingsView{Configured: !row.Shared(), VideoSlots: row.VideoSlots, PodcastSlots: row.PodcastSlots, Ceiling: row.Ceiling, X265Slots: row.X265Slots, Revision: row.Revision, UpdatedAt: row.UpdatedAt}
}

// slotsMatch reports whether the worker runs the stored ceiling, split and
// x265 limit. A worker that cannot take a ceiling or an x265 limit is only
// compared on the rest.
func slotsMatch(row *store.WorkerSlotSettings, worker transcode.WorkerSlots) bool {
	if row != nil && row.Ceiling > 0 && worker.CeilingAdjustable() && worker.Ceiling != row.Ceiling {
		return false
	}
	if worker.X265Capable() {
		stored := 0
		if row != nil {
			stored = row.X265Slots
		}
		if worker.X265.Total != stored {
			return false
		}
	}
	if row == nil || row.Shared() {
		return !worker.Split
	}
	return worker.Split && worker.Video.Total == row.VideoSlots && worker.Podcast.Total == row.PodcastSlots
}

// slotPush is what the server sends for a stored row: the ceiling and the x265
// limit only to a worker that accepts them, since an older one refuses the
// unknown fields.
func slotPush(row store.WorkerSlotSettings, worker transcode.WorkerSlots) transcode.SlotSplit {
	split := transcode.SlotSplit{VideoSlots: row.VideoSlots, PodcastSlots: row.PodcastSlots}
	if worker.CeilingAdjustable() {
		split.Ceiling = row.Ceiling
	}
	if worker.X265Capable() {
		split.X265Slots = row.X265Slots
	}
	return split
}

// workerSlots serves GET (the stored ceiling and split, what the worker runs
// and whether they agree) and POST (validate against the worker's limits and
// CPUs, store, push). The worker applies a change at its next admission and
// never interrupts a running job.
func (s *Server) workerSlots(w http.ResponseWriter, r *http.Request) {
	worker, ok := s.slotNode(r)
	if !ok {
		fail(w, 404, "no such worker")
		return
	}
	node := worker.Name
	ctrl, ok := worker.Executor.(workerSlotController)
	if !ok {
		writeJSON(w, 200, map[string]any{"supported": false, "available": false, "node": node, "message": "Slot assignment requires the HTTP video worker."})
		return
	}
	st := s.engine.Deps().Store
	row, err := st.GetWorkerSlotSettings(node)
	if err != nil {
		fail(w, 500, "worker slot settings unavailable")
		return
	}
	if r.Method == http.MethodPost {
		s.setWorkerSlots(w, r, node, ctrl)
		return
	}
	view := map[string]any{"supported": true, "node": node, "settings": slotSettingsView(row)}
	slots, err := ctrl.Slots(r.Context())
	switch {
	case errors.Is(err, httpclient.ErrSlotsUnsupported):
		view["available"], view["message"] = false, "The video worker predates slot assignment. Update it to divide its slots."
	case err != nil:
		view["available"], view["message"] = false, "Worker unreachable. The stored settings are applied when it returns."
	default:
		view["available"], view["worker"], view["applied"] = true, slots, slotsMatch(row, slots)
		view["ceiling_adjustable"] = slots.CeilingAdjustable()
		view["x265_adjustable"] = slots.X265Capable()
		if !slots.CeilingAdjustable() {
			view["ceiling_message"] = "The video worker predates setting parallel jobs here. Update it to change them."
		}
		if msg := s.slotSyncError(node); msg != "" && !slotsMatch(row, slots) {
			view["sync_error"] = msg
		}
	}
	writeJSON(w, 200, view)
}

// setWorkerSlots stores and pushes a split and, with a positive ceiling, the
// node's parallel jobs. Without a ceiling the stored one is kept. A split that
// does not fit a ceiling set in the same request returns to shared slots
// (transcode.FitSlotSplit); a split over an unchanged ceiling is refused.
func (s *Server) setWorkerSlots(w http.ResponseWriter, r *http.Request, node string, ctrl workerSlotController) {
	var body struct {
		VideoSlots   int `json:"video_slots"`
		PodcastSlots int `json:"podcast_slots"`
		Ceiling      int `json:"ceiling"`
		// X265Slots omitted keeps the stored limit.
		X265Slots *int `json:"x265_slots"`
	}
	if decode(w, r, &body) != nil {
		fail(w, 400, "invalid slot assignment")
		return
	}
	split := transcode.SlotSplit{VideoSlots: body.VideoSlots, PodcastSlots: body.PodcastSlots}
	if (split.VideoSlots == 0) != (split.PodcastSlots == 0) {
		fail(w, 400, "set both video and podcast slots, or clear both to share the worker's slots")
		return
	}
	if body.Ceiling < 0 {
		fail(w, 400, "parallel jobs must be at least 1")
		return
	}
	if body.X265Slots != nil && *body.X265Slots < 0 {
		fail(w, 400, "x265 at a time must be at least 1, or 0 for no limit")
		return
	}
	current, err := ctrl.Slots(r.Context())
	if errors.Is(err, httpclient.ErrSlotsUnsupported) {
		fail(w, 409, "The video worker predates slot assignment. Update it first.")
		return
	}
	if err != nil {
		fail(w, 503, "Worker unreachable; its slot ceiling is needed to validate the split.")
		return
	}
	st := s.engine.Deps().Store
	previous, _ := st.GetWorkerSlotSettings(node)
	ceiling := 0
	if previous != nil {
		ceiling = previous.Ceiling
	}
	switch {
	case body.X265Slots != nil:
		split.X265Slots = *body.X265Slots
	case previous != nil:
		split.X265Slots = previous.X265Slots
	}
	// Only a limit asked for now needs the worker to take it; one inherited from
	// the stored row is kept (slotPush never sends it to a worker that cannot).
	if body.X265Slots != nil && *body.X265Slots > 0 && !current.X265Capable() {
		fail(w, 409, "The video worker predates limiting x265 here. Update it first.")
		return
	}
	var notes []string
	if body.Ceiling > 0 {
		switch {
		case current.CeilingAdjustable():
			if err := transcode.ValidateSlotCeiling(body.Ceiling, current.CeilingLimit); err != nil {
				fail(w, 400, err.Error())
				return
			}
			ceiling = body.Ceiling
			split, notes = transcode.FitSlotSplit(split, ceiling)
		case body.Ceiling != current.Ceiling:
			fail(w, 409, "The video worker predates setting parallel jobs here. Update it first.")
			return
		}
	}
	target := current.Ceiling
	if ceiling > 0 && current.CeilingAdjustable() {
		target = ceiling
	}
	warnings, err := transcode.ValidateSlotSplit(split, target, current.HostCPUs)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	warnings = append(notes, warnings...)
	row, err := st.SetWorkerSlotSettings(node, split.VideoSlots, split.PodcastSlots, ceiling, split.X265Slots)
	if err != nil {
		fail(w, 500, "worker slot settings not saved")
		return
	}
	out := map[string]any{"supported": true, "node": node, "settings": slotSettingsView(&row), "warnings": warnings}
	applied, pushed, err := ctrl.SetSlots(r.Context(), slotPush(row, current))
	var rejected *httpclient.SlotSplitRejectedError
	switch {
	case errors.As(err, &rejected):
		// The worker's refusal is final: do not keep a split that the resync
		// would otherwise push again every few seconds.
		// A node that was never configured goes back to that state: a stored
		// "shared" row would make the resync clear a split the worker runs.
		var revertErr error
		if previous != nil {
			_, revertErr = st.SetWorkerSlotSettings(node, previous.VideoSlots, previous.PodcastSlots, previous.Ceiling, previous.X265Slots)
		} else {
			revertErr = st.DeleteWorkerSlotSettings(node)
		}
		if revertErr != nil {
			log.Printf("worker slot settings not reverted after the worker refused the split: %v", revertErr)
		}
		fail(w, 400, rejected.Message)
		return
	case err != nil:
		s.noteSlotSync(node, err)
		out["available"], out["applied"], out["message"] = false, false, "Saved. The worker did not confirm yet; it is retried while this page is open."
	default:
		s.noteSlotSync(node, nil)
		out["available"], out["worker"], out["applied"] = true, applied, slotsMatch(&row, applied)
		if len(pushed) > 0 {
			out["warnings"] = append(notes, pushed...)
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) slotSyncError(node string) string {
	s.slotSync.mu.Lock()
	defer s.slotSync.mu.Unlock()
	if st := s.slotSync.nodes[node]; st != nil {
		return st.err
	}
	return ""
}

// slotSyncState is the node's push record; the caller holds slotSync.mu.
func (s *Server) slotSyncStateLocked(node string) *slotSyncState {
	if s.slotSync.nodes == nil {
		s.slotSync.nodes = make(map[string]*slotSyncState)
	}
	st := s.slotSync.nodes[node]
	if st == nil {
		st = &slotSyncState{}
		s.slotSync.nodes[node] = st
	}
	return st
}

func (s *Server) noteSlotSync(node string, err error) {
	s.slotSync.mu.Lock()
	defer s.slotSync.mu.Unlock()
	st := s.slotSyncStateLocked(node)
	st.last = time.Now()
	st.err = ""
	if err != nil {
		st.err = "The worker did not accept the stored slot settings."
	}
}

// syncWorkerSlots pushes a worker's stored ceiling and split when it reports
// something else (restarted with a wiped state directory, a save that could
// not reach it, or a redeploy that changed max_parallel_jobs). The stored row
// wins; a node never configured is left alone. Rate limited per worker, and
// called with the activity the worker just reported.
func (s *Server) syncWorkerSlots(ctx context.Context, worker transcode.NamedWorker, observed *transcode.WorkerSlots) {
	ctrl, ok := worker.Executor.(workerSlotController)
	if !ok || observed == nil {
		return
	}
	row, err := s.engine.Deps().Store.GetWorkerSlotSettings(worker.Name)
	if err != nil || row == nil || slotsMatch(row, *observed) {
		return
	}
	s.slotSync.mu.Lock()
	st := s.slotSyncStateLocked(worker.Name)
	if time.Since(st.last) < slotSyncInterval {
		s.slotSync.mu.Unlock()
		return
	}
	st.last = time.Now()
	s.slotSync.mu.Unlock()
	_, _, err = ctrl.SetSlots(ctx, slotPush(*row, *observed))
	s.noteSlotSync(worker.Name, err)
}
