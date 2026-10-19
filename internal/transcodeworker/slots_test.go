package transcodeworker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

func seedSlotJob(t *testing.T, w *Worker, stub *stubSpawn, id, status string, kind slotKind, running bool, age time.Duration) {
	t.Helper()
	job := &JobRecord{ID: id, Status: status, Source: "/media/" + id + ".mkv", Candidate: "/media/" + id + ".out.mkv", CreatedAt: time.Now().UTC().Add(-age)}
	if kind == slotKindPodcast {
		job.Plan = &transcode.Plan{Podcast: &podcast.Task{}}
	}
	if running {
		job.PID = stub.occupy(id)
	}
	if err := SaveJobAtomic(filepath.Join(w.cfg.StateDir, id, "job.json"), job); err != nil {
		t.Fatal(err)
	}
}

func TestSlotLimitsFollowTheStoredSplit(t *testing.T) {
	withHostCPUs(t, 10)
	w, _, _ := newPR3Worker(t, 4, nil)
	shared := w.slotLimits()
	if shared.Split || shared.Ceiling != 4 || shared.Video != 4 || shared.Podcast != 4 || shared.videoPool() != 4 {
		t.Fatalf("no file must mean one shared pool of the ceiling: %+v", shared)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 3, PodcastSlots: 1}); err != nil {
		t.Fatal(err)
	}
	if l := w.slotLimits(); !l.Split || l.Video != 3 || l.Podcast != 1 || l.videoPool() != 3 {
		t.Fatalf("stored split not read: %+v", l)
	}
	// A second process (a spawned job) sees it through the state directory.
	other := NewWorker(&WorkerConfig{StateDir: w.cfg.StateDir, MaxParallelJobs: 4})
	if l := other.slotLimits(); !l.Split || l.Video != 3 {
		t.Fatalf("another worker process missed the split: %+v", l)
	}
	// Lowering the ceiling under a stored split falls back to sharing rather
	// than admitting more than the ceiling.
	other.cfg.MaxParallelJobs = 2
	if l := other.slotLimits(); l.Split || l.Ceiling != 2 {
		t.Fatalf("a split larger than the ceiling must be ignored: %+v", l)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{}); err != nil {
		t.Fatal(err)
	}
	if l := w.slotLimits(); l.Split {
		t.Fatalf("clearing left the split: %+v", l)
	}
	if _, err := os.Stat(w.slotSplitPath()); !os.IsNotExist(err) {
		t.Fatal("clearing must remove the stored split", err)
	}
	// A corrupt file never blocks the worker.
	if err := os.WriteFile(w.slotSplitPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l := w.slotLimits(); l.Split {
		t.Fatalf("corrupt split file applied: %+v", l)
	}
}

func TestSetSlotSplitValidates(t *testing.T) {
	withHostCPUs(t, 10)
	w, _, _ := newPR3Worker(t, 2, nil)
	for name, split := range map[string]transcode.SlotSplit{
		"over the ceiling": {VideoSlots: 2, PodcastSlots: 1},
		"no video":         {VideoSlots: 0, PodcastSlots: 1},
		"no podcast":       {VideoSlots: 1, PodcastSlots: 0},
		"negative":         {VideoSlots: -1, PodcastSlots: 2},
	} {
		if _, err := w.SetSlotSplit(split); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := os.Stat(w.slotSplitPath()); !os.IsNotExist(err) {
		t.Fatal("a rejected split was stored")
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 1, PodcastSlots: 1}); err != nil {
		t.Fatal(err)
	}
	single, _, _ := newPR3Worker(t, 1, nil)
	if _, err := single.SetSlotSplit(transcode.SlotSplit{VideoSlots: 1, PodcastSlots: 1}); err == nil {
		t.Fatal("a one-slot worker cannot be split")
	}
}

func TestValidateSlotSplitCPUBudget(t *testing.T) {
	if _, err := transcode.ValidateSlotSplit(transcode.SlotSplit{VideoSlots: 12, PodcastSlots: 1}, 16, 10); err == nil {
		t.Error("more video slots than CPUs accepted")
	}
	warnings, err := transcode.ValidateSlotSplit(transcode.SlotSplit{VideoSlots: 3, PodcastSlots: 1}, 4, 10)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "cannot partition") {
		t.Errorf("3 video slots on 10 CPUs: %v %v", warnings, err)
	}
	warnings, err = transcode.ValidateSlotSplit(transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 1}, 4, 10)
	if err != nil || len(warnings) != 0 {
		t.Errorf("2 video slots on 10 CPUs: %v %v", warnings, err)
	}
	if warnings, err := transcode.ValidateSlotSplit(transcode.SlotSplit{VideoSlots: 1, PodcastSlots: 1}, 2, 0); err != nil || len(warnings) != 0 {
		t.Errorf("unknown CPU count: %v %v", warnings, err)
	}
}

// With the ceiling split, a full pool of one kind never holds back the other:
// the scheduler skips the queued job whose pool is full and starts the next
// one. Shared, the same queue is plain FIFO against the ceiling.
func TestSchedulerAdmitsEachKindAgainstItsOwnPool(t *testing.T) {
	withHostCPUs(t, 10)
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 2, stub)
	ctx := context.Background()
	seedSlotJob(t, w, stub, "running-video", "running", slotKindVideo, true, time.Hour)
	seedSlotJob(t, w, stub, "queued-video", "queued", slotKindVideo, false, 3*time.Minute)
	seedSlotJob(t, w, stub, "queued-podcast", "queued", slotKindPodcast, false, 2*time.Minute)
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 1, PodcastSlots: 1}); err != nil {
		t.Fatal(err)
	}
	started, err := w.ScheduleQueued(ctx, "exe", "")
	if err != nil || started != 1 {
		t.Fatalf("started %d, err %v; want only the podcast job", started, err)
	}
	for id, want := range map[string]int{"queued-video": 0, "queued-podcast": 1} {
		job, err := LoadJob(filepath.Join(w.cfg.StateDir, id, "job.json"))
		if err != nil || (job.PID > 0) != (want == 1) {
			t.Fatalf("%s: pid %d err %v", id, job.PID, err)
		}
	}
	// A second pass changes nothing: video is full, podcast is full.
	if started, _ := w.ScheduleQueued(ctx, "exe", ""); started != 0 {
		t.Fatalf("started %d more jobs into full pools", started)
	}
	// The video job ends; the queued video job takes the freed video slot.
	stub.kill("running-video")
	running, _ := LoadJob(filepath.Join(w.cfg.StateDir, "running-video", "job.json"))
	running.Status = "completed"
	if err := SaveJobAtomic(filepath.Join(w.cfg.StateDir, "running-video", "job.json"), running); err != nil {
		t.Fatal(err)
	}
	if started, err := w.ScheduleQueued(ctx, "exe", ""); err != nil || started != 1 {
		t.Fatalf("freed video slot: started %d, err %v", started, err)
	}
}

func TestSchedulerSplitNeverAdmitsPastEitherPool(t *testing.T) {
	withHostCPUs(t, 10)
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 3, stub)
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 1, PodcastSlots: 2}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"v1", "v2", "v3"} {
		seedSlotJob(t, w, stub, id, "queued", slotKindVideo, false, time.Duration(10-i)*time.Minute)
	}
	for i, id := range []string{"p1", "p2", "p3"} {
		seedSlotJob(t, w, stub, id, "queued", slotKindPodcast, false, time.Duration(5-i)*time.Minute)
	}
	started, err := w.ScheduleQueued(context.Background(), "exe", "")
	if err != nil || started != 3 {
		t.Fatalf("started %d, err %v; want 1 video + 2 podcast", started, err)
	}
	video, podcast, err := w.countActiveJobsByKind("")
	if err != nil || video != 1 || podcast != 2 {
		t.Fatalf("running video %d podcast %d, err %v", video, podcast, err)
	}
}

func TestSharedPoolStillCountsBothKindsAgainstTheCeiling(t *testing.T) {
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 2, stub)
	seedSlotJob(t, w, stub, "running-podcast", "running", slotKindPodcast, true, time.Hour)
	seedSlotJob(t, w, stub, "queued-video-a", "queued", slotKindVideo, false, 2*time.Minute)
	seedSlotJob(t, w, stub, "queued-video-b", "queued", slotKindVideo, false, time.Minute)
	if started, err := w.ScheduleQueued(context.Background(), "exe", ""); err != nil || started != 1 {
		t.Fatalf("shared ceiling of 2 with one podcast job running: started %d, err %v", started, err)
	}
}

func TestBenchmarkAdmissionUsesTheVideoPool(t *testing.T) {
	withHostCPUs(t, 10)
	stub := newStubSpawn()
	w, tempDir, _ := newPR3Worker(t, 2, stub)
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 1, PodcastSlots: 1}); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(tempDir, "media")
	_ = os.MkdirAll(media, 0o755)
	source, _ := createTestMediaSource(t, media, "bench.mkv", "bench-data")
	w.cfg.AllowedRoots = []string{media}

	// A running video job fills the video pool: the benchmark is refused
	// although a podcast slot is free.
	seedSlotJob(t, w, stub, "running-video", "running", slotKindVideo, true, time.Hour)
	req := validWorkerBenchmarkRequest(source)
	req.ID = "bench-refused"
	_, err := w.BenchmarkSubmit(context.Background(), req, "exe", "")
	if err == nil || !strings.Contains(err.Error(), "worker busy") {
		t.Fatalf("video pool full: %v", err)
	}
	// A running podcast job leaves the video pool free for the benchmark.
	stub.kill("running-video")
	if err := os.RemoveAll(filepath.Join(w.cfg.StateDir, "running-video")); err != nil {
		t.Fatal(err)
	}
	seedSlotJob(t, w, stub, "running-podcast", "running", slotKindPodcast, true, time.Hour)
	mockExe := createMockWorkerScript(t, tempDir)
	req = validWorkerBenchmarkRequest(source)
	req.ID = "bench-admitted"
	if resp, err := w.BenchmarkSubmit(context.Background(), req, mockExe, ""); err != nil || resp.Status != "queued" {
		t.Fatalf("a busy podcast slot must not block a benchmark: %+v %v", resp, err)
	}
}

func TestSlotSnapshotReportsTheJobsOwnPool(t *testing.T) {
	withHostCPUs(t, 10)
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 3, stub)
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 1}); err != nil {
		t.Fatal(err)
	}
	seedSlotJob(t, w, stub, "running-podcast", "running", slotKindPodcast, true, time.Hour)
	seedSlotJob(t, w, stub, "queued-video", "queued", slotKindVideo, false, 2*time.Minute)
	seedSlotJob(t, w, stub, "queued-podcast", "queued", slotKindPodcast, false, time.Minute)
	total, used, position := w.slotSnapshot("queued-video")
	if total != 2 || used != 0 || position != 1 {
		t.Errorf("video job sees %d/%d at %d", used, total, position)
	}
	total, used, position = w.slotSnapshot("queued-podcast")
	if total != 1 || used != 1 || position != 1 {
		t.Errorf("podcast job sees %d/%d at %d (the video job queued before it is not ahead)", used, total, position)
	}
	if total, used, _ = w.slotSnapshot(""); total != 3 || used != 1 {
		t.Errorf("overall %d/%d", used, total)
	}
}

func TestSlotsEndpointAppliesTheSplit(t *testing.T) {
	withHostCPUs(t, 10)
	worker, srv, _, _ := newHTTPTestSetup(t, "secret")
	worker.cfg.MaxParallelJobs = 2
	if got := doRequest(t, srv, "GET", "/v1/slots", "", ""); got.Code != 401 {
		t.Fatal("slots must need the token", got.Code)
	}
	if got := doRequest(t, srv, "POST", "/v1/slots", "{}", "secret"); got.Code != 405 {
		t.Fatal("slots accepts only GET and PUT", got.Code)
	}
	var out struct {
		Slots    transcode.WorkerSlots `json:"slots"`
		Warnings []string              `json:"warnings"`
	}
	get := func(method, body string) int {
		resp := doRequest(t, srv, method, "/v1/slots", body, "secret")
		out.Slots, out.Warnings = transcode.WorkerSlots{}, nil
		_ = json.Unmarshal(resp.Body.Bytes(), &out)
		return resp.Code
	}
	if code := get("GET", ""); code != 200 || out.Slots.Split || out.Slots.Ceiling != 2 || out.Slots.HostCPUs != 10 {
		t.Fatalf("initial %d %+v", code, out)
	}
	if code := get("PUT", `{"video_slots":2,"podcast_slots":1}`); code != 400 {
		t.Fatalf("over the ceiling accepted: %d", code)
	}
	if code := get("PUT", `{"video_slots":1,"podcast_slots":1,"extra":true}`); code != 400 {
		t.Fatalf("unknown field accepted: %d", code)
	}
	if code := get("PUT", `{"video_slots":1,"podcast_slots":1}`); code != 200 || !out.Slots.Split || out.Slots.Video.Total != 1 || out.Slots.Podcast.Total != 1 {
		t.Fatalf("applied %d %+v", code, out)
	}
	ready := doRequest(t, srv, "GET", "/v1/ready", "", "secret")
	var body struct {
		Total int                   `json:"worker_slots_total"`
		Slots transcode.WorkerSlots `json:"slots"`
	}
	if err := json.Unmarshal(ready.Body.Bytes(), &body); err != nil || body.Total != 2 || !body.Slots.Split {
		t.Fatalf("ready %s", ready.Body.String())
	}
	if code := get("PUT", `{"video_slots":0,"podcast_slots":0}`); code != 200 || out.Slots.Split {
		t.Fatalf("cleared %d %+v", code, out)
	}
}

func TestActivityReportsUseByKind(t *testing.T) {
	withHostCPUs(t, 10)
	worker, srv, _, _ := newHTTPTestSetup(t, "secret")
	worker.cfg.MaxParallelJobs = 2
	if _, err := worker.SetSlotSplit(transcode.SlotSplit{VideoSlots: 1, PodcastSlots: 1}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for id, plan := range map[string]*transcode.Plan{"video-job": nil, "podcast-job": {Podcast: &podcast.Task{}}} {
		job := &JobRecord{ID: id, Status: "running", PID: 4242, Source: "/media/" + id + ".mkv", StartedAt: now, Plan: plan}
		if err := SaveJobAtomic(filepath.Join(worker.cfg.StateDir, id, "job.json"), job); err != nil {
			t.Fatal(err)
		}
	}
	resp := doRequest(t, srv, "GET", "/v1/activity", "", "secret")
	var activity transcode.WorkerActivity
	if resp.Code != 200 || json.Unmarshal(resp.Body.Bytes(), &activity) != nil || activity.Slots == nil {
		t.Fatal(resp.Code, resp.Body.String())
	}
	if !activity.Slots.Split || activity.Slots.Video.Used != 1 || activity.Slots.Podcast.Used != 1 || activity.SlotsUsed != 2 || activity.SlotsTotal != 2 {
		t.Fatalf("%+v %+v", activity, activity.Slots)
	}
	kinds := map[string]string{}
	for _, job := range activity.Jobs {
		kinds[job.ID] = job.Kind
	}
	if kinds["video-job"] != "transcode" || kinds["podcast-job"] != "podcast" {
		t.Fatalf("kinds %v", kinds)
	}
}

// One video job alone keeps x265's default pool and so uses every CPU; video
// jobs that run together split the host. The pool follows the video jobs
// running, not the ceiling.
func TestX265PoolFollowsVideoJobsInUse(t *testing.T) {
	withHostCPUs(t, 10)
	inUse := 0
	w := &Worker{cfg: &WorkerConfig{MaxParallelJobs: 3, StateDir: t.TempDir()}, videoInUseFn: func() (int, bool) { return inUse, true }}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 1}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		inUse int
		want  string
	}{
		{1, ""},                        // alone: x265 sizes its own pool from the host
		{2, "pools=5:frame-threads=3"}, // two together: half the host each
		{0, "pools=5:frame-threads=3"}, // nothing counted: assume the video pool is full
	} {
		inUse = tc.inUse
		if got := w.x265ThreadParams(); got != tc.want {
			t.Errorf("%d video jobs: %q, want %q", tc.inUse, got, tc.want)
		}
	}
	// The same ceiling shared (no split) sizes for three.
	if _, err := w.SetSlotSplit(transcode.SlotSplit{}); err != nil {
		t.Fatal(err)
	}
	inUse = 0
	if got := w.x265ThreadParams(); got != "" {
		t.Errorf("three-way share on 10 CPUs is below the identical-pool floor: %q", got)
	}
	// An explicit x265_pools stays exactly what the operator asked for.
	w.cfg.X265Pools = 6
	inUse = 1
	if got := w.x265ThreadParams(); got != "pools=6:frame-threads=3" {
		t.Errorf("explicit pools: %q", got)
	}
}

func TestMetricThreadsFollowVideoJobsInUse(t *testing.T) {
	withHostCPUs(t, 10)
	inUse := 1
	w := &Worker{cfg: &WorkerConfig{MaxParallelJobs: 2}, videoInUseFn: func() (int, bool) { return inUse, true }}
	if got := w.metricThreads(2); got != 5 {
		t.Errorf("a lone video job splits all 10 CPUs over 2 metrics: %d threads", got)
	}
	if got := w.finalValidationConcurrency(6); got != 3 {
		t.Errorf("a lone video job validates 3 windows at once: %d", got)
	}
	inUse = 2
	if got := w.metricThreads(2); got != 2 {
		t.Errorf("two video jobs share the host: %d threads", got)
	}
	if got := w.finalValidationConcurrency(6); got != 2 {
		t.Errorf("two video jobs validate 2 windows at once: %d", got)
	}
}

func TestVideoJobsSharingCPUCountsOnlyLiveVideo(t *testing.T) {
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 4, stub)
	seedSlotJob(t, w, stub, "video-a", "running", slotKindVideo, true, time.Hour)
	seedSlotJob(t, w, stub, "podcast-a", "running", slotKindPodcast, true, time.Hour)
	seedSlotJob(t, w, stub, "dead-video", "running", slotKindVideo, true, time.Hour)
	stub.kill("dead-video")
	seedSlotJob(t, w, stub, "queued-video", "queued", slotKindVideo, false, time.Minute)
	if got := w.videoJobsSharingCPU(); got != 1 {
		t.Fatalf("one live video job, got %d", got)
	}
	// Observing leaves the stopped runner's record for the scheduler to reconcile.
	job, err := LoadJob(filepath.Join(w.cfg.StateDir, "dead-video", "job.json"))
	if err != nil || job.Status != "running" {
		t.Fatalf("observation changed a record: %+v %v", job, err)
	}
}

func TestPartitionCandidatesCoverEveryPoolSize(t *testing.T) {
	withHostCPUs(t, 12)
	w := &Worker{cfg: &WorkerConfig{MaxParallelJobs: 4, StateDir: t.TempDir()}}
	if got := w.x265PartitionCandidates(); strings.Join(got, ",") != "pools=6:frame-threads=3,pools=4:frame-threads=3" {
		t.Fatalf("shared ceiling of 4 on 12 CPUs: %v", got)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 2, PodcastSlots: 2}); err != nil {
		t.Fatal(err)
	}
	if got := w.x265PartitionCandidates(); strings.Join(got, ",") != "pools=6:frame-threads=3" {
		t.Fatalf("2 video slots: %v", got)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 1, PodcastSlots: 3}); err != nil {
		t.Fatal(err)
	}
	if got := w.x265PartitionCandidates(); len(got) != 0 {
		t.Fatalf("one video slot never partitions: %v", got)
	}
	w.cfg.X265Pools = -1
	if got := w.x265PartitionCandidates(); len(got) != 0 {
		t.Fatalf("disabled: %v", got)
	}
}

// The self-check proves every partition the split can produce, not only the
// full-load one.
func TestSelfCheckProvesEveryCandidate(t *testing.T) {
	f := &fakeX265Check{diverge: func(dir string) bool { return dir == "crf-partitioned2" }}
	w := newSelfCheckWorker(t, WorkerConfig{MaxParallelJobs: 4}, f)
	withHostCPUs(t, 12)
	rec := w.RunX265PartitionCheck(context.Background())
	if rec.Status != x265CheckFailed || !strings.Contains(rec.Detail, "pools=4:frame-threads=3") || strings.Contains(rec.Detail, "pools=6") {
		t.Fatalf("the second candidate diverges alone: %+v", rec)
	}
	if rec.Params != "pools=6:frame-threads=3,pools=4:frame-threads=3" {
		t.Fatalf("verdict not keyed on the candidate set: %q", rec.Params)
	}
	if w.x265ThreadParams() != "" {
		t.Fatal("a diverging candidate must switch partitioning off")
	}
}
