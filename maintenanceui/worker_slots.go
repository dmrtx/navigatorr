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

// videoWorkerNode names the one video worker this server drives in the
// per-node settings table.
const videoWorkerNode = "video-worker"

// slotSyncInterval spaces the pushes that bring a worker back to the stored
// split after a restart or a failed save, so a poll never hammers it.
const slotSyncInterval = 10 * time.Second

// workerSlotController is what an HTTP video worker offers for dividing its
// slots between video and podcast work.
type workerSlotController interface {
	Slots(ctx context.Context) (transcode.WorkerSlots, error)
	SetSlots(ctx context.Context, split transcode.SlotSplit) (transcode.WorkerSlots, []string, error)
}

func (s *Server) slotController() (workerSlotController, bool) {
	c, ok := s.engine.Deps().Transcode.(workerSlotController)
	return c, ok
}

type workerSlotSettingsView struct {
	Configured   bool   `json:"configured"`
	VideoSlots   int    `json:"video_slots"`
	PodcastSlots int    `json:"podcast_slots"`
	Revision     int64  `json:"revision"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

func slotSettingsView(row *store.WorkerSlotSettings) workerSlotSettingsView {
	if row == nil {
		return workerSlotSettingsView{}
	}
	return workerSlotSettingsView{Configured: !row.Shared(), VideoSlots: row.VideoSlots, PodcastSlots: row.PodcastSlots, Revision: row.Revision, UpdatedAt: row.UpdatedAt}
}

// slotsMatch reports whether the worker runs the stored split.
func slotsMatch(row *store.WorkerSlotSettings, worker transcode.WorkerSlots) bool {
	if row == nil || row.Shared() {
		return !worker.Split
	}
	return worker.Split && worker.Video.Total == row.VideoSlots && worker.Podcast.Total == row.PodcastSlots
}

// workerSlots serves GET (the stored split, what the worker runs and whether
// they agree) and POST (validate against the worker's ceiling and CPUs, store,
// push). The worker applies a split at its next admission and never
// interrupts a running job.
func (s *Server) workerSlots(w http.ResponseWriter, r *http.Request) {
	ctrl, ok := s.slotController()
	if !ok {
		writeJSON(w, 200, map[string]any{"supported": false, "available": false, "message": "Slot assignment requires the HTTP video worker."})
		return
	}
	st := s.engine.Deps().Store
	row, err := st.GetWorkerSlotSettings(videoWorkerNode)
	if err != nil {
		fail(w, 500, "worker slot settings unavailable")
		return
	}
	if r.Method == http.MethodPost {
		s.setWorkerSlots(w, r, ctrl)
		return
	}
	view := map[string]any{"supported": true, "node": videoWorkerNode, "settings": slotSettingsView(row)}
	worker, err := ctrl.Slots(r.Context())
	switch {
	case errors.Is(err, httpclient.ErrSlotsUnsupported):
		view["available"], view["message"] = false, "The video worker predates slot assignment. Update it to divide its slots."
	case err != nil:
		view["available"], view["message"] = false, "Worker unreachable. The stored split is applied when it returns."
	default:
		view["available"], view["worker"], view["applied"] = true, worker, slotsMatch(row, worker)
		if msg := s.slotSyncError(); msg != "" && !slotsMatch(row, worker) {
			view["sync_error"] = msg
		}
	}
	writeJSON(w, 200, view)
}

func (s *Server) setWorkerSlots(w http.ResponseWriter, r *http.Request, ctrl workerSlotController) {
	var body struct {
		VideoSlots   int `json:"video_slots"`
		PodcastSlots int `json:"podcast_slots"`
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
	current, err := ctrl.Slots(r.Context())
	if errors.Is(err, httpclient.ErrSlotsUnsupported) {
		fail(w, 409, "The video worker predates slot assignment. Update it first.")
		return
	}
	if err != nil {
		fail(w, 503, "Worker unreachable; its slot ceiling is needed to validate the split.")
		return
	}
	warnings, err := transcode.ValidateSlotSplit(split, current.Ceiling, current.HostCPUs)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	st := s.engine.Deps().Store
	previous, _ := st.GetWorkerSlotSettings(videoWorkerNode)
	row, err := st.SetWorkerSlotSettings(videoWorkerNode, split.VideoSlots, split.PodcastSlots)
	if err != nil {
		fail(w, 500, "worker slot settings not saved")
		return
	}
	out := map[string]any{"supported": true, "node": videoWorkerNode, "settings": slotSettingsView(&row), "warnings": warnings}
	applied, pushed, err := ctrl.SetSlots(r.Context(), split)
	var rejected *httpclient.SlotSplitRejectedError
	switch {
	case errors.As(err, &rejected):
		// The worker's refusal is final: do not keep a split that the resync
		// would otherwise push again every few seconds.
		// A node that was never configured goes back to that state: a stored
		// "shared" row would make the resync clear a split the worker runs.
		var revertErr error
		if previous != nil {
			_, revertErr = st.SetWorkerSlotSettings(videoWorkerNode, previous.VideoSlots, previous.PodcastSlots)
		} else {
			revertErr = st.DeleteWorkerSlotSettings(videoWorkerNode)
		}
		if revertErr != nil {
			log.Printf("worker slot settings not reverted after the worker refused the split: %v", revertErr)
		}
		fail(w, 400, rejected.Message)
		return
	case err != nil:
		s.noteSlotSync(err)
		out["available"], out["applied"], out["message"] = false, false, "Saved. The worker did not confirm yet; it is retried while this page is open."
	default:
		s.noteSlotSync(nil)
		out["available"], out["worker"], out["applied"] = true, applied, slotsMatch(&row, applied)
		if len(pushed) > 0 {
			out["warnings"] = pushed
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) slotSyncError() string {
	s.slotSync.mu.Lock()
	defer s.slotSync.mu.Unlock()
	return s.slotSync.err
}

func (s *Server) noteSlotSync(err error) {
	s.slotSync.mu.Lock()
	defer s.slotSync.mu.Unlock()
	s.slotSync.last = time.Now()
	s.slotSync.err = ""
	if err != nil {
		s.slotSync.err = "The worker did not accept the stored split."
	}
}

// syncWorkerSlots pushes the stored split when the worker reports something
// else (restarted with a wiped state directory, or a save that could not reach
// it). The stored row wins; a node never configured is left alone. Rate
// limited, and called with the activity the worker just reported.
func (s *Server) syncWorkerSlots(ctx context.Context, observed *transcode.WorkerSlots) {
	ctrl, ok := s.slotController()
	if !ok || observed == nil {
		return
	}
	row, err := s.engine.Deps().Store.GetWorkerSlotSettings(videoWorkerNode)
	if err != nil || row == nil || slotsMatch(row, *observed) {
		return
	}
	s.slotSync.mu.Lock()
	if time.Since(s.slotSync.last) < slotSyncInterval {
		s.slotSync.mu.Unlock()
		return
	}
	s.slotSync.last = time.Now()
	s.slotSync.mu.Unlock()
	_, _, err = ctrl.SetSlots(ctx, transcode.SlotSplit{VideoSlots: row.VideoSlots, PodcastSlots: row.PodcastSlots})
	s.noteSlotSync(err)
}
