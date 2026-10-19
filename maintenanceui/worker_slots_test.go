package maintenanceui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// fakeSlotWorker stands in for the HTTP worker: it keeps one split and can be
// made old, unreachable or strict.
type fakeSlotWorker struct {
	transcode.Executor
	ceiling, cpus  int
	split          transcode.SlotSplit
	getErr, putErr error
	puts           int
}

func (f *fakeSlotWorker) slots() transcode.WorkerSlots {
	out := transcode.WorkerSlots{Ceiling: f.ceiling, HostCPUs: f.cpus, Split: !f.split.Shared()}
	if out.Split {
		out.Video = transcode.SlotPool{Total: f.split.VideoSlots}
		out.Podcast = transcode.SlotPool{Total: f.split.PodcastSlots}
	} else {
		out.Video = transcode.SlotPool{Total: f.ceiling}
		out.Podcast = transcode.SlotPool{Total: f.ceiling}
	}
	return out
}

func (f *fakeSlotWorker) Slots(context.Context) (transcode.WorkerSlots, error) {
	return f.slots(), f.getErr
}

func (f *fakeSlotWorker) SetSlots(_ context.Context, split transcode.SlotSplit) (transcode.WorkerSlots, []string, error) {
	f.puts++
	if f.putErr != nil {
		return transcode.WorkerSlots{}, nil, f.putErr
	}
	f.split = split
	return f.slots(), nil, nil
}

func slotUI(t *testing.T, worker *fakeSlotWorker) (*Server, http.Handler) {
	t.Helper()
	s, h := testUI(t)
	deps := s.engine.Deps()
	deps.Transcode = worker
	s.engine = action.NewEngine(deps)
	return s, h
}

func slotsGet(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	w := request(h, "GET", "/api/maintenance/worker-slots", "", true)
	var out map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return out
}

func TestWorkerSlotsRequireAuth(t *testing.T) {
	_, h := slotUI(t, &fakeSlotWorker{ceiling: 3, cpus: 10})
	if request(h, "GET", "/api/maintenance/worker-slots", "", false).Code != 401 {
		t.Fatal("slot settings readable without a session")
	}
	if request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":1,"podcast_slots":1}`, false).Code != 401 {
		t.Fatal("slot settings writable without a session")
	}
}

func TestWorkerSlotsSaveStoresAndPushes(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 3, cpus: 10}
	s, h := slotUI(t, worker)
	got := slotsGet(t, h)
	if got["supported"] != true || got["available"] != true || got["applied"] != true || got["settings"].(map[string]any)["configured"] != false {
		t.Fatalf("untouched worker should read as shared: %v", got)
	}
	w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":2,"podcast_slots":1}`, true)
	var saved map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &saved) != nil || saved["applied"] != true {
		t.Fatal(w.Code, w.Body.String())
	}
	if worker.split.VideoSlots != 2 || worker.split.PodcastSlots != 1 {
		t.Fatalf("worker not pushed: %+v", worker.split)
	}
	row, err := s.engine.Deps().Store.GetWorkerSlotSettings(videoWorkerNode)
	if err != nil || row == nil || row.VideoSlots != 2 || row.Revision != 1 {
		t.Fatalf("not stored: %+v %v", row, err)
	}
	settings := slotsGet(t, h)["settings"].(map[string]any)
	if settings["configured"] != true || settings["video_slots"] != float64(2) {
		t.Fatalf("settings view: %v", settings)
	}
	// Clearing both returns the worker to one shared pool.
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":0,"podcast_slots":0}`, true); w.Code != 200 || !worker.split.Shared() {
		t.Fatal("clear failed", w.Code, w.Body.String(), worker.split)
	}
}

func TestWorkerSlotsRejectInvalidSplitsWithoutStoringThem(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 2, cpus: 10}
	s, h := slotUI(t, worker)
	for name, body := range map[string]string{
		"over the ceiling": `{"video_slots":2,"podcast_slots":1}`,
		"half cleared":     `{"video_slots":0,"podcast_slots":1}`,
		"negative":         `{"video_slots":-1,"podcast_slots":2}`,
		"malformed":        `{"video_slots":"two"}`,
		"unknown field":    `{"video_slots":1,"podcast_slots":1,"extra":1}`,
	} {
		if w := request(h, "POST", "/api/maintenance/worker-slots", body, true); w.Code != 400 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if worker.puts != 0 {
		t.Fatal("an invalid split reached the worker")
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(videoWorkerNode); row != nil {
		t.Fatal("an invalid split was stored", row)
	}
	// The worker's own refusal (a stricter or newer worker) is a 400 too.
	worker.putErr = &httpclient.SlotSplitRejectedError{Message: "refused by worker"}
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":1,"podcast_slots":1}`, true); w.Code != 400 {
		t.Fatal("worker refusal", w.Code, w.Body.String())
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(videoWorkerNode); row != nil && !row.Shared() {
		t.Fatal("a split the worker refused stays stored and would be re-pushed", row)
	}
	// A refusal restores the split that was in force, not just clears.
	worker.putErr = nil
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":1,"podcast_slots":1}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	worker.putErr = &httpclient.SlotSplitRejectedError{Message: "refused by worker"}
	request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":2,"podcast_slots":1}`, true)
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(videoWorkerNode); row == nil || row.VideoSlots != 1 || row.PodcastSlots != 1 {
		t.Fatal("the previous split was not restored", row)
	}
}

func TestWorkerSlotsOldOrUnreachableWorker(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 3, cpus: 10, getErr: httpclient.ErrSlotsUnsupported}
	_, h := slotUI(t, worker)
	if got := slotsGet(t, h); got["available"] != false || got["supported"] != true {
		t.Fatalf("old worker view: %v", got)
	}
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":1,"podcast_slots":1}`, true); w.Code != 409 {
		t.Fatal("old worker", w.Code)
	}
	worker.getErr = errors.New("connection refused")
	if got := slotsGet(t, h); got["available"] != false {
		t.Fatalf("unreachable view: %v", got)
	}
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":1,"podcast_slots":1}`, true); w.Code != 503 {
		t.Fatal("unreachable worker", w.Code)
	}
}

func TestWorkerSlotsSaveWhenPushFailsIsRetried(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 3, cpus: 10}
	s, h := slotUI(t, worker)
	worker.putErr = errors.New("connection reset")
	w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":2,"podcast_slots":1}`, true)
	var out map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out["applied"] != false {
		t.Fatal("a saved-but-unpushed split must say so", w.Code, w.Body.String())
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(videoWorkerNode); row == nil || row.VideoSlots != 2 {
		t.Fatal("the split must be stored even when the worker is away", row)
	}
	if got := slotsGet(t, h); got["applied"] != false || got["sync_error"] == nil {
		t.Fatalf("view after a failed push: %v", got)
	}
	// The worker comes back (possibly restarted with its state wiped): the next
	// activity poll re-applies the stored split.
	worker.putErr = nil
	s.slotSync.mu.Lock()
	s.slotSync.last = time.Now().Add(-time.Minute)
	s.slotSync.mu.Unlock()
	observed := worker.slots()
	s.syncWorkerSlots(context.Background(), &observed)
	if worker.split.VideoSlots != 2 || worker.split.PodcastSlots != 1 {
		t.Fatalf("stored split not re-applied: %+v", worker.split)
	}
	if got := slotsGet(t, h); got["applied"] != true || got["sync_error"] != nil {
		t.Fatalf("view after resync: %v", got)
	}
}

func TestWorkerSlotsSyncIsRateLimitedAndLeavesUnconfiguredNodesAlone(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 3, cpus: 10}
	s, _ := slotUI(t, worker)
	observed := worker.slots()
	s.syncWorkerSlots(context.Background(), &observed)
	if worker.puts != 0 {
		t.Fatal("a node nobody configured must not be pushed to")
	}
	if _, err := s.engine.Deps().Store.SetWorkerSlotSettings(videoWorkerNode, 2, 1); err != nil {
		t.Fatal(err)
	}
	worker.putErr = errors.New("down")
	s.syncWorkerSlots(context.Background(), &observed)
	s.syncWorkerSlots(context.Background(), &observed)
	s.syncWorkerSlots(context.Background(), &observed)
	if worker.puts != 1 {
		t.Fatalf("polling must not hammer a failing worker: %d pushes", worker.puts)
	}
	// Matching state is never re-pushed.
	worker.split = transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 1}
	worker.putErr = nil
	s.slotSync.mu.Lock()
	s.slotSync.last = time.Time{}
	s.slotSync.mu.Unlock()
	matching := worker.slots()
	s.syncWorkerSlots(context.Background(), &matching)
	if worker.puts != 1 {
		t.Fatal("an in-sync worker was pushed again")
	}
}

func TestWorkerSlotsUnsupportedWithoutHTTPWorker(t *testing.T) {
	s, h := testUI(t)
	deps := s.engine.Deps()
	deps.Transcode = unavailableWorker{}
	s.engine = action.NewEngine(deps)
	if got := slotsGet(t, h); got["supported"] != false {
		t.Fatalf("%v", got)
	}
}

// A split the worker refuses, on a node nobody had configured, must leave the
// node unconfigured: a stored "shared" row would make the resync clear a split
// the worker runs by itself.
func TestWorkerSlotsRefusalOnUnconfiguredNodeLeavesItUnconfigured(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 4, cpus: 10, split: transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 2}}
	s, h := slotUI(t, worker)
	worker.putErr = &httpclient.SlotSplitRejectedError{Message: "refused by worker"}
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":3,"podcast_slots":1}`, true); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(videoWorkerNode); row != nil {
		t.Fatalf("the refused save left a row behind: %+v", row)
	}
	worker.putErr = nil
	s.slotSync.mu.Lock()
	s.slotSync.last = time.Time{}
	s.slotSync.mu.Unlock()
	observed := worker.slots()
	pushes := worker.puts
	s.syncWorkerSlots(context.Background(), &observed)
	if worker.puts != pushes || worker.split.VideoSlots != 2 || worker.split.PodcastSlots != 2 {
		t.Fatalf("the resync changed a split nobody stored: pushes %d, worker %+v", worker.puts-pushes, worker.split)
	}
}
