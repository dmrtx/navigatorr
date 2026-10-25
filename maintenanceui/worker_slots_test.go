package maintenanceui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// fakeSlotWorker stands in for the HTTP worker: it keeps one split and can be
// made old, unreachable or strict. With limit 0 it predates the ceiling
// override and refuses a push that carries one, as an old worker refuses the
// unknown field. With x265Capable it also reports and takes the x265 limit; an
// older one refuses a push that carries it.
type fakeSlotWorker struct {
	transcode.Executor
	ceiling, cpus  int
	limit          int
	override       int
	split          transcode.SlotSplit
	getErr, putErr error
	puts           int
	lastPush       transcode.SlotSplit
	x265Capable    bool
}

func (f *fakeSlotWorker) slots() transcode.WorkerSlots {
	ceiling := f.ceiling
	out := transcode.WorkerSlots{HostCPUs: f.cpus, Split: !f.split.Shared()}
	if f.limit > 0 {
		out.CeilingLimit, out.CeilingSource = f.limit, transcode.SlotCeilingFromConfig
		if f.override > 0 {
			ceiling, out.CeilingSource = f.override, transcode.SlotCeilingFromOverride
		}
	}
	out.Ceiling = ceiling
	if out.Split {
		out.Video = transcode.SlotPool{Total: f.split.VideoSlots}
		out.Podcast = transcode.SlotPool{Total: f.split.PodcastSlots}
	} else {
		out.Video = transcode.SlotPool{Total: ceiling}
		out.Podcast = transcode.SlotPool{Total: ceiling}
	}
	if f.x265Capable {
		out.X265 = &transcode.SlotPool{Total: f.split.X265Slots}
	}
	return out
}

func (f *fakeSlotWorker) Slots(context.Context) (transcode.WorkerSlots, error) {
	return f.slots(), f.getErr
}

func (f *fakeSlotWorker) SetSlots(_ context.Context, split transcode.SlotSplit) (transcode.WorkerSlots, []string, error) {
	f.puts++
	f.lastPush = split
	if f.putErr != nil {
		return transcode.WorkerSlots{}, nil, f.putErr
	}
	if split.Ceiling > 0 {
		if f.limit == 0 {
			return transcode.WorkerSlots{}, nil, &httpclient.SlotSplitRejectedError{Message: `invalid slot split: json: unknown field "ceiling"`}
		}
		f.override = split.Ceiling
		if f.override == f.ceiling {
			f.override = 0
		}
	}
	if split.X265Slots > 0 && !f.x265Capable {
		return transcode.WorkerSlots{}, nil, &httpclient.SlotSplitRejectedError{Message: `invalid slot split: json: unknown field "x265_slots"`}
	}
	f.split = transcode.SlotSplit{VideoSlots: split.VideoSlots, PodcastSlots: split.PodcastSlots, X265Slots: split.X265Slots}
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
	row, err := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName)
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
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName); row != nil {
		t.Fatal("an invalid split was stored", row)
	}
	// The worker's own refusal (a stricter or newer worker) is a 400 too.
	worker.putErr = &httpclient.SlotSplitRejectedError{Message: "refused by worker"}
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":1,"podcast_slots":1}`, true); w.Code != 400 {
		t.Fatal("worker refusal", w.Code, w.Body.String())
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName); row != nil && !row.Shared() {
		t.Fatal("a split the worker refused stays stored and would be re-pushed", row)
	}
	// A refusal restores the split that was in force, not just clears.
	worker.putErr = nil
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":1,"podcast_slots":1}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	worker.putErr = &httpclient.SlotSplitRejectedError{Message: "refused by worker"}
	request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":2,"podcast_slots":1}`, true)
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName); row == nil || row.VideoSlots != 1 || row.PodcastSlots != 1 {
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
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName); row == nil || row.VideoSlots != 2 {
		t.Fatal("the split must be stored even when the worker is away", row)
	}
	if got := slotsGet(t, h); got["applied"] != false || got["sync_error"] == nil {
		t.Fatalf("view after a failed push: %v", got)
	}
	// The worker comes back (possibly restarted with its state wiped): the next
	// activity poll re-applies the stored split.
	worker.putErr = nil
	s.slotSync.mu.Lock()
	s.slotSyncStateLocked(transcode.DefaultWorkerName).last = time.Now().Add(-time.Minute)
	s.slotSync.mu.Unlock()
	observed := worker.slots()
	s.syncWorkerSlots(context.Background(), defaultNode(s), &observed)
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
	s.syncWorkerSlots(context.Background(), defaultNode(s), &observed)
	if worker.puts != 0 {
		t.Fatal("a node nobody configured must not be pushed to")
	}
	if _, err := s.engine.Deps().Store.SetWorkerSlotSettings(transcode.DefaultWorkerName, 2, 1, 0, 0); err != nil {
		t.Fatal(err)
	}
	worker.putErr = errors.New("down")
	s.syncWorkerSlots(context.Background(), defaultNode(s), &observed)
	s.syncWorkerSlots(context.Background(), defaultNode(s), &observed)
	s.syncWorkerSlots(context.Background(), defaultNode(s), &observed)
	if worker.puts != 1 {
		t.Fatalf("polling must not hammer a failing worker: %d pushes", worker.puts)
	}
	// Matching state is never re-pushed.
	worker.split = transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 1}
	worker.putErr = nil
	s.slotSync.mu.Lock()
	s.slotSyncStateLocked(transcode.DefaultWorkerName).last = time.Time{}
	s.slotSync.mu.Unlock()
	matching := worker.slots()
	s.syncWorkerSlots(context.Background(), defaultNode(s), &matching)
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
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName); row != nil {
		t.Fatalf("the refused save left a row behind: %+v", row)
	}
	worker.putErr = nil
	s.slotSync.mu.Lock()
	s.slotSyncStateLocked(transcode.DefaultWorkerName).last = time.Time{}
	s.slotSync.mu.Unlock()
	observed := worker.slots()
	pushes := worker.puts
	s.syncWorkerSlots(context.Background(), defaultNode(s), &observed)
	if worker.puts != pushes || worker.split.VideoSlots != 2 || worker.split.PodcastSlots != 2 {
		t.Fatalf("the resync changed a split nobody stored: pushes %d, worker %+v", worker.puts-pushes, worker.split)
	}
}

func TestWorkerSlotsCeilingIsStoredAndPushed(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 1, cpus: 10, limit: 10}
	s, h := slotUI(t, worker)
	if got := slotsGet(t, h); got["ceiling_adjustable"] != true || got["worker"].(map[string]any)["ceiling_limit"] != float64(10) {
		t.Fatalf("a current worker reports its limit: %v", got)
	}
	w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":0,"podcast_slots":0,"ceiling":3}`, true)
	var saved map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &saved) != nil || saved["applied"] != true {
		t.Fatal(w.Code, w.Body.String())
	}
	if worker.slots().Ceiling != 3 || worker.lastPush.Ceiling != 3 {
		t.Fatalf("worker not pushed: %+v", worker.lastPush)
	}
	row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName)
	if row == nil || row.Ceiling != 3 || !row.Shared() {
		t.Fatalf("not stored: %+v", row)
	}
	if settings := slotsGet(t, h)["settings"].(map[string]any); settings["ceiling"] != float64(3) {
		t.Fatalf("settings view: %v", settings)
	}
	// Saving only the split keeps the stored ceiling and validates against it.
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":2,"podcast_slots":1}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName); row.Ceiling != 3 || row.VideoSlots != 2 || worker.lastPush.Ceiling != 3 {
		t.Fatalf("split save dropped the ceiling: %+v push %+v", row, worker.lastPush)
	}
	// Out of range is refused and nothing changes.
	pushes := worker.puts
	for _, body := range []string{`{"video_slots":2,"podcast_slots":1,"ceiling":11}`, `{"video_slots":2,"podcast_slots":1,"ceiling":-1}`} {
		if w := request(h, "POST", "/api/maintenance/worker-slots", body, true); w.Code != 400 {
			t.Errorf("%s: %d", body, w.Code)
		}
	}
	if worker.puts != pushes {
		t.Fatal("a refused ceiling reached the worker")
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName); row.Ceiling != 3 {
		t.Fatalf("a refused ceiling was stored: %+v", row)
	}
}

func TestWorkerSlotsLowerCeilingResetsASplitThatNoLongerFits(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 1, cpus: 10, limit: 10}
	s, h := slotUI(t, worker)
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":2,"podcast_slots":1,"ceiling":3}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":2,"podcast_slots":1,"ceiling":2}`, true)
	var out map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if warnings, _ := out["warnings"].([]any); len(warnings) == 0 || !strings.Contains(warnings[0].(string), "shared again") {
		t.Fatalf("the reset must be explained: %v", out)
	}
	row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName)
	if row == nil || !row.Shared() || row.Ceiling != 2 || worker.split.VideoSlots != 0 || worker.slots().Ceiling != 2 {
		t.Fatalf("row %+v worker %+v", row, worker.slots())
	}
}

func TestWorkerSlotsOldWorkerShowsTheCeilingReadOnly(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 2, cpus: 10}
	s, h := slotUI(t, worker)
	if got := slotsGet(t, h); got["ceiling_adjustable"] != false || got["ceiling_message"] == nil {
		t.Fatalf("old worker view: %v", got)
	}
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":0,"podcast_slots":0,"ceiling":3}`, true); w.Code != 409 {
		t.Fatal("an old worker cannot take a new ceiling", w.Code, w.Body.String())
	}
	// The value it already runs, or no ceiling at all, is a plain split save
	// that never carries the field the old worker would refuse.
	for _, body := range []string{`{"video_slots":1,"podcast_slots":1,"ceiling":2}`, `{"video_slots":1,"podcast_slots":1}`} {
		if w := request(h, "POST", "/api/maintenance/worker-slots", body, true); w.Code != 200 || worker.lastPush.Ceiling != 0 {
			t.Fatalf("%s: %d %s push %+v", body, w.Code, w.Body.String(), worker.lastPush)
		}
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName); row == nil || row.VideoSlots != 1 {
		t.Fatalf("row %+v", row)
	}
}

func TestWorkerSlotsSyncRestoresTheCeilingOnlyOnWorkersThatTakeIt(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 1, cpus: 10, limit: 10}
	s, _ := slotUI(t, worker)
	if _, err := s.engine.Deps().Store.SetWorkerSlotSettings(transcode.DefaultWorkerName, 0, 0, 3, 0); err != nil {
		t.Fatal(err)
	}
	// The worker restarted with its state directory wiped: back to its config.
	observed := worker.slots()
	s.syncWorkerSlots(context.Background(), defaultNode(s), &observed)
	if worker.puts != 1 || worker.lastPush.Ceiling != 3 || worker.slots().Ceiling != 3 {
		t.Fatalf("stored ceiling not restored: %+v", worker.lastPush)
	}
	// An older worker is compared on the split only and never pushed a ceiling.
	old := &fakeSlotWorker{ceiling: 1, cpus: 10}
	s2, _ := slotUI(t, old)
	if _, err := s2.engine.Deps().Store.SetWorkerSlotSettings(transcode.DefaultWorkerName, 0, 0, 3, 0); err != nil {
		t.Fatal(err)
	}
	observed = old.slots()
	s2.syncWorkerSlots(context.Background(), defaultNode(s2), &observed)
	if old.puts != 0 {
		t.Fatalf("an old worker running the stored split was pushed: %+v", old.lastPush)
	}
}

// defaultNode is the one worker of the single base_url config.
func defaultNode(s *Server) transcode.NamedWorker {
	w, _ := s.engine.Deps().Workers.Get(transcode.DefaultWorkerName)
	return w
}

func TestWorkerSlotsX265LimitIsStoredAndPushed(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 2, cpus: 10, limit: 10, x265Capable: true}
	s, h := slotUI(t, worker)
	if got := slotsGet(t, h); got["x265_adjustable"] != true || got["worker"].(map[string]any)["x265"] == nil {
		t.Fatalf("a current worker reports its x265 pool: %v", got)
	}
	w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":0,"podcast_slots":0,"x265_slots":1}`, true)
	var saved map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &saved) != nil || saved["applied"] != true {
		t.Fatal(w.Code, w.Body.String())
	}
	if worker.lastPush.X265Slots != 1 || worker.slots().X265.Total != 1 {
		t.Fatalf("worker not pushed: %+v", worker.lastPush)
	}
	row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName)
	if row == nil || row.X265Slots != 1 || !row.Shared() {
		t.Fatalf("not stored: %+v", row)
	}
	if settings := slotsGet(t, h)["settings"].(map[string]any); settings["x265_slots"] != float64(1) {
		t.Fatalf("settings view: %v", settings)
	}
	// A request that does not mention the limit (an older page) keeps it.
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":1,"podcast_slots":1}`, true); w.Code != 200 || worker.lastPush.X265Slots != 1 {
		t.Fatal(w.Code, w.Body.String(), worker.lastPush)
	}
	// An explicit zero removes it.
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":1,"podcast_slots":1,"x265_slots":0}`, true); w.Code != 200 || worker.lastPush.X265Slots != 0 {
		t.Fatal(w.Code, w.Body.String(), worker.lastPush)
	}
	// Over the video slots, or negative, is refused and nothing changes.
	pushes := worker.puts
	for _, body := range []string{`{"video_slots":1,"podcast_slots":1,"x265_slots":2}`, `{"video_slots":0,"podcast_slots":0,"x265_slots":3}`, `{"video_slots":0,"podcast_slots":0,"x265_slots":-1}`} {
		if w := request(h, "POST", "/api/maintenance/worker-slots", body, true); w.Code != 400 {
			t.Errorf("%s: %d", body, w.Code)
		}
	}
	if worker.puts != pushes {
		t.Fatal("a refused limit reached the worker")
	}
}

func TestWorkerSlotsLowerCeilingClearsAnX265LimitThatNoLongerFits(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 3, cpus: 10, limit: 10, x265Capable: true}
	s, h := slotUI(t, worker)
	w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":0,"podcast_slots":0,"ceiling":2,"x265_slots":3}`, true)
	var saved map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &saved) != nil || worker.lastPush.X265Slots != 0 || len(saved["warnings"].([]any)) == 0 {
		t.Fatal(w.Code, w.Body.String(), worker.lastPush)
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName); row.X265Slots != 0 || row.Ceiling != 2 {
		t.Fatalf("%+v", row)
	}
}

func TestWorkerSlotsOldWorkerNeverGetsTheX265Limit(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 2, cpus: 10, limit: 10}
	s, h := slotUI(t, worker)
	if got := slotsGet(t, h); got["x265_adjustable"] != false {
		t.Fatalf("a worker without the x265 pool is not adjustable: %v", got)
	}
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":0,"podcast_slots":0,"x265_slots":1}`, true); w.Code != 409 {
		t.Fatalf("asking an old worker for a limit is a 409: %d %s", w.Code, w.Body.String())
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName); row != nil {
		t.Fatalf("a refused limit was stored: %+v", row)
	}
	// A stored limit is not pushed to a worker that cannot take it, and does
	// not count as a mismatch there.
	if _, err := s.engine.Deps().Store.SetWorkerSlotSettings(transcode.DefaultWorkerName, 0, 0, 0, 1); err != nil {
		t.Fatal(err)
	}
	observed := worker.slots()
	s.syncWorkerSlots(context.Background(), defaultNode(s), &observed)
	if worker.puts != 0 {
		t.Fatalf("an old worker was pushed %+v", worker.lastPush)
	}
	// Changing only the split keeps the stored limit without asking the old
	// worker for it.
	if w := request(h, "POST", "/api/maintenance/worker-slots", `{"video_slots":1,"podcast_slots":1}`, true); w.Code != 200 {
		t.Fatalf("a split that does not mention the limit is accepted: %d %s", w.Code, w.Body.String())
	}
	if row, _ := s.engine.Deps().Store.GetWorkerSlotSettings(transcode.DefaultWorkerName); row == nil || row.X265Slots != 1 || row.VideoSlots != 1 {
		t.Fatalf("the split must keep the stored limit: %+v", row)
	}
	if worker.lastPush.X265Slots != 0 {
		t.Fatalf("an old worker was sent the limit: %+v", worker.lastPush)
	}
}

func TestWorkerSlotsSyncRestoresTheX265LimitAfterAWorkerWipe(t *testing.T) {
	worker := &fakeSlotWorker{ceiling: 2, cpus: 10, limit: 10, x265Capable: true}
	s, _ := slotUI(t, worker)
	if _, err := s.engine.Deps().Store.SetWorkerSlotSettings(transcode.DefaultWorkerName, 0, 0, 0, 1); err != nil {
		t.Fatal(err)
	}
	observed := worker.slots()
	s.syncWorkerSlots(context.Background(), defaultNode(s), &observed)
	if worker.puts != 1 || worker.lastPush.X265Slots != 1 || worker.slots().X265.Total != 1 {
		t.Fatalf("stored x265 limit not restored: %+v", worker.lastPush)
	}
	// Agreeing is not pushed again.
	observed = worker.slots()
	s.syncWorkerSlots(context.Background(), defaultNode(s), &observed)
	if worker.puts != 1 {
		t.Fatal("a matching worker was pushed")
	}
}
