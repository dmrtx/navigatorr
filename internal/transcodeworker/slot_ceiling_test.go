package transcodeworker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func finishSlotJob(t *testing.T, w *Worker, stub *stubSpawn, id string) {
	t.Helper()
	stub.kill(id)
	path := filepath.Join(w.cfg.StateDir, id, "job.json")
	job, err := LoadJob(path)
	if err != nil {
		t.Fatal(err)
	}
	job.Status = "completed"
	if err := SaveJobAtomic(path, job); err != nil {
		t.Fatal(err)
	}
}

func TestSlotCeilingOverridePersistsAcrossRestart(t *testing.T) {
	withHostCPUs(t, 10)
	w, _, _ := newPR3Worker(t, 1, nil)
	if got := w.slotsStatus(); got.Ceiling != 1 || got.CeilingLimit != 10 || got.CeilingSource != transcode.SlotCeilingFromConfig {
		t.Fatalf("boot default: %+v", got)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{Ceiling: 3}); err != nil {
		t.Fatal(err)
	}
	if got := w.slotsStatus(); got.Ceiling != 3 || got.CeilingSource != transcode.SlotCeilingFromOverride || got.Split || got.Video.Total != 3 {
		t.Fatalf("override not applied: %+v", got)
	}
	// A restarted daemon (or a spawned job process) reads it from the state
	// directory; the config value is only the default.
	restarted := NewWorker(&WorkerConfig{StateDir: w.cfg.StateDir, MaxParallelJobs: 1})
	if l := restarted.slotLimits(); l.Ceiling != 3 || l.Split {
		t.Fatalf("override lost across restart: %+v", l)
	}
	// Setting the config value again returns to following the config.
	if _, err := restarted.SetSlotSplit(transcode.SlotSplit{Ceiling: 1}); err != nil {
		t.Fatal(err)
	}
	if got := restarted.slotsStatus(); got.Ceiling != 1 || got.CeilingSource != transcode.SlotCeilingFromConfig {
		t.Fatalf("config value must clear the override: %+v", got)
	}
	if _, err := os.Stat(restarted.slotSplitPath()); !os.IsNotExist(err) {
		t.Fatal("nothing left to store, so the file goes", err)
	}
}

func TestSlotCeilingBounds(t *testing.T) {
	withHostCPUs(t, 10)
	w, _, _ := newPR3Worker(t, 2, nil)
	for _, ceiling := range []int{-1, 11} {
		if _, err := w.SetSlotSplit(transcode.SlotSplit{Ceiling: ceiling}); err == nil {
			t.Errorf("ceiling %d accepted on 10 CPUs", ceiling)
		}
	}
	if _, err := os.Stat(w.slotSplitPath()); !os.IsNotExist(err) {
		t.Fatal("a rejected ceiling was stored")
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{Ceiling: 10}); err != nil {
		t.Fatal("the host CPU count is the default limit:", err)
	}
	// max_parallel_jobs_limit replaces the CPU count, and an override it no
	// longer allows falls back to the config value.
	w.cfg.MaxParallelJobsLimit = 4
	if got := w.slotsStatus(); got.CeilingLimit != 4 || got.Ceiling != 2 || got.CeilingSource != transcode.SlotCeilingFromConfig {
		t.Fatalf("override over a lowered limit: %+v", got)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{Ceiling: 5}); err == nil {
		t.Fatal("ceiling over max_parallel_jobs_limit accepted")
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{Ceiling: 4}); err != nil {
		t.Fatal(err)
	}
	// The limit never falls below the configured default.
	w.cfg.MaxParallelJobs = 6
	if got := w.slotCeilingLimit(); got != 6 {
		t.Fatalf("limit below the config default: %d", got)
	}
}

func TestLoweredCeilingAdmitsNothingUntilDrainedAndRaisedAdmits(t *testing.T) {
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 1, stub)
	ctx := context.Background()
	if _, err := w.SetSlotSplit(transcode.SlotSplit{Ceiling: 3}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		seedSlotJob(t, w, stub, id, "running", slotKindVideo, true, time.Hour)
	}
	seedSlotJob(t, w, stub, "queued", "queued", slotKindVideo, false, time.Minute)
	// Lowering never touches the three running jobs.
	if _, err := w.SetSlotSplit(transcode.SlotSplit{Ceiling: 2}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if !stub.isAlive(&JobRecord{ID: id}) {
			t.Fatalf("lowering the ceiling stopped %s", id)
		}
	}
	if started, _ := w.ScheduleQueued(ctx, "exe", ""); started != 0 {
		t.Fatalf("started %d over a lowered ceiling", started)
	}
	finishSlotJob(t, w, stub, "a")
	if started, _ := w.ScheduleQueued(ctx, "exe", ""); started != 0 {
		t.Fatalf("started %d with 2 running and a ceiling of 2", started)
	}
	finishSlotJob(t, w, stub, "b")
	if started, err := w.ScheduleQueued(ctx, "exe", ""); err != nil || started != 1 {
		t.Fatalf("drained below the ceiling: started %d, err %v", started, err)
	}
	// Raising takes effect at the next admission.
	seedSlotJob(t, w, stub, "queued-2", "queued", slotKindVideo, false, time.Minute)
	if started, _ := w.ScheduleQueued(ctx, "exe", ""); started != 0 {
		t.Fatalf("started %d at the ceiling", started)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{Ceiling: 3}); err != nil {
		t.Fatal(err)
	}
	if started, err := w.ScheduleQueued(ctx, "exe", ""); err != nil || started != 1 {
		t.Fatalf("raised ceiling: started %d, err %v", started, err)
	}
}

// x265 thread pools are sized from the ceiling in force when a job starts,
// not from max_parallel_jobs at boot.
func TestX265ThreadsFollowTheEffectiveCeiling(t *testing.T) {
	withHostCPUs(t, 10)
	w := &Worker{cfg: &WorkerConfig{MaxParallelJobs: 1, StateDir: t.TempDir()}, videoInUseFn: func() (int, bool) { return 0, true }}
	if got := w.x265ThreadParams(); got != "" {
		t.Fatalf("one job keeps x265's default pool: %q", got)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{Ceiling: 2}); err != nil {
		t.Fatal(err)
	}
	if got := w.x265ThreadParams(); got != "pools=5:frame-threads=3" {
		t.Fatalf("two parallel jobs on 10 CPUs: %q", got)
	}
	if got := strings.Join(w.x265PartitionCandidates(), ","); got != "pools=5:frame-threads=3" {
		t.Fatalf("the self-check must prove the override's pools: %q", got)
	}
}

func TestLoweringTheCeilingResetsASplitThatNoLongerFits(t *testing.T) {
	withHostCPUs(t, 10)
	w, _, _ := newPR3Worker(t, 4, nil)
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 2}); err != nil {
		t.Fatal(err)
	}
	notes, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 2, Ceiling: 3})
	if err != nil || len(notes) == 0 || !strings.Contains(notes[0], "shared again") {
		t.Fatalf("notes %v err %v", notes, err)
	}
	if l := w.slotLimits(); l.Split || l.Ceiling != 3 {
		t.Fatalf("a split over the new ceiling must return to shared: %+v", l)
	}
	// A split that fits the new ceiling is kept as it is.
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 1, Ceiling: 3}); err != nil {
		t.Fatal(err)
	}
	notes, err = w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 1, Ceiling: 3})
	if l := w.slotLimits(); err != nil || len(notes) != 0 || !l.Split || l.Video != 2 || l.Podcast != 1 {
		t.Fatalf("fitting split: %+v %v %v", l, notes, err)
	}
	// Without a ceiling in the request a split over the ceiling is an error,
	// as before.
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 3, PodcastSlots: 1}); err == nil {
		t.Fatal("split over an unchanged ceiling accepted")
	}
	// One parallel job cannot be split at all.
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 1, Ceiling: 1}); err != nil {
		t.Fatal(err)
	}
	if l := w.slotLimits(); l.Split || l.Ceiling != 1 {
		t.Fatalf("ceiling 1: %+v", l)
	}
}

// Servers that predate the ceiling send only the split, and files written
// before it have no ceiling key: both keep working.
func TestSlotsEndpointCeilingCompatibility(t *testing.T) {
	withHostCPUs(t, 10)
	worker, srv, _, _ := newHTTPTestSetup(t, "secret")
	worker.cfg.MaxParallelJobs = 2
	var out struct {
		Slots    transcode.WorkerSlots `json:"slots"`
		Warnings []string              `json:"warnings"`
	}
	put := func(body string) int {
		resp := doRequest(t, srv, "PUT", "/v1/slots", body, "secret")
		out.Slots, out.Warnings = transcode.WorkerSlots{}, nil
		_ = json.Unmarshal(resp.Body.Bytes(), &out)
		return resp.Code
	}
	if code := put(`{"video_slots":0,"podcast_slots":0,"ceiling":4}`); code != 200 || out.Slots.Ceiling != 4 || out.Slots.CeilingSource != "override" || out.Slots.CeilingLimit != 10 {
		t.Fatalf("ceiling %d %+v", code, out)
	}
	// An old server's split-only request keeps the override.
	if code := put(`{"video_slots":3,"podcast_slots":1}`); code != 200 || out.Slots.Ceiling != 4 || !out.Slots.Split || out.Slots.Video.Total != 3 {
		t.Fatalf("split-only %d %+v", code, out)
	}
	if code := put(`{"video_slots":0,"podcast_slots":0,"ceiling":11}`); code != 400 {
		t.Fatalf("over the limit: %d", code)
	}
	if code := put(`{"video_slots":2,"podcast_slots":2,"ceiling":3}`); code != 200 || out.Slots.Split || out.Slots.Ceiling != 3 || len(out.Warnings) == 0 {
		t.Fatalf("reset split %d %+v", code, out)
	}
	ready := doRequest(t, srv, "GET", "/v1/ready", "", "secret")
	var body struct {
		MaxParallel int `json:"max_parallel_jobs"`
		Total       int `json:"worker_slots_total"`
	}
	if err := json.Unmarshal(ready.Body.Bytes(), &body); err != nil || body.MaxParallel != 3 || body.Total != 3 {
		t.Fatalf("ready reports the effective ceiling: %s", ready.Body.String())
	}
	activity := doRequest(t, srv, "GET", "/v1/activity", "", "secret")
	var act transcode.WorkerActivity
	if err := json.Unmarshal(activity.Body.Bytes(), &act); err != nil || act.SlotsTotal != 3 || act.Slots == nil || act.Slots.Ceiling != 3 {
		t.Fatalf("activity reports the effective ceiling: %s", activity.Body.String())
	}
	// A split file from before overrides existed.
	if err := os.WriteFile(worker.slotSplitPath(), []byte(`{"video_slots":1,"podcast_slots":1,"updated_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	worker.noteSlotSplitChanged()
	if l := worker.slotLimits(); l.Ceiling != 2 || !l.Split || l.Video != 1 {
		t.Fatalf("old split file: %+v", l)
	}
}
