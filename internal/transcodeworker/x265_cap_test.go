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

// seedEncoderJob writes a video job whose plan encodes with codec.
func seedEncoderJob(t *testing.T, w *Worker, stub *stubSpawn, id, status, codec string, running bool, age time.Duration) {
	t.Helper()
	job := &JobRecord{ID: id, Status: status, Source: "/media/" + id + ".mkv", Candidate: "/media/" + id + ".out.mkv", CreatedAt: time.Now().UTC().Add(-age), Plan: &transcode.Plan{VideoCodec: codec}}
	if running {
		job.PID = stub.occupy(id)
	}
	if err := SaveJobAtomic(filepath.Join(w.cfg.StateDir, id, "job.json"), job); err != nil {
		t.Fatal(err)
	}
}

func jobStarted(t *testing.T, w *Worker, id string) bool {
	t.Helper()
	job, err := LoadJob(filepath.Join(w.cfg.StateDir, id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	return job.PID > 0
}

func TestJobIsX265ClassifiesByPlanEncoder(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  *JobRecord
		want bool
	}{
		{"nil job", nil, false},
		{"no plan", &JobRecord{}, false},
		{"libx265", &JobRecord{Plan: &transcode.Plan{VideoCodec: "libx265"}}, true},
		{"normalized", &JobRecord{Plan: &transcode.Plan{VideoCodec: " LibX265 "}}, true},
		{"videotoolbox", &JobRecord{Plan: &transcode.Plan{VideoCodec: "hevc_videotoolbox"}}, false},
		{"video copy", &JobRecord{Plan: &transcode.Plan{VideoCodec: transcode.VideoCodecCopy}}, false},
	} {
		if got := jobIsX265(tc.job); got != tc.want {
			t.Errorf("%s: jobIsX265 = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSlotLimitsHasRoomCapsX265InsideTheVideoPool(t *testing.T) {
	l := slotLimits{Ceiling: 2, Video: 2, Podcast: 2, X265: 1}
	x265, vt := jobSlot{kind: slotKindVideo, x265: true}, jobSlot{kind: slotKindVideo}
	if !l.hasRoom(x265, slotCounts{}) || !l.hasRoom(vt, slotCounts{}) {
		t.Fatal("an idle worker admits both encoders")
	}
	if l.hasRoom(x265, slotCounts{Video: 1, X265: 1}) {
		t.Fatal("a second x265 must wait")
	}
	if !l.hasRoom(vt, slotCounts{Video: 1, X265: 1}) {
		t.Fatal("VideoToolbox runs beside the x265 encode")
	}
	if !l.hasRoom(x265, slotCounts{Video: 1}) {
		t.Fatal("x265 runs beside a VideoToolbox encode")
	}
	if l.hasRoom(vt, slotCounts{Video: 2, X265: 1}) {
		t.Fatal("the ceiling still caps everything")
	}
	if uncapped := (slotLimits{Ceiling: 2, Video: 2, Podcast: 2}); !uncapped.hasRoom(x265, slotCounts{Video: 1, X265: 1}) {
		t.Fatal("without a cap a second x265 starts, as before")
	}
}

// With one x265 running, the next x265 in the queue is skipped and the
// VideoToolbox job behind it takes the other slot.
func TestSchedulerSkipsQueuedX265AtCapAndStartsVideoToolbox(t *testing.T) {
	withHostCPUs(t, 8)
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 2, stub)
	if _, err := w.SetSlotSplit(transcode.SlotSplit{X265Slots: 1}); err != nil {
		t.Fatal(err)
	}
	seedEncoderJob(t, w, stub, "running-x265", "running", "libx265", true, time.Hour)
	seedEncoderJob(t, w, stub, "queued-x265", "queued", "libx265", false, 3*time.Minute)
	seedEncoderJob(t, w, stub, "queued-vt", "queued", "hevc_videotoolbox", false, 2*time.Minute)
	started, err := w.ScheduleQueued(context.Background(), "exe", "")
	if err != nil || started != 1 {
		t.Fatalf("started %d, err %v; want only the VideoToolbox job", started, err)
	}
	if jobStarted(t, w, "queued-x265") || !jobStarted(t, w, "queued-vt") {
		t.Fatal("the x265 job must wait and the VideoToolbox job behind it must start")
	}
	// Full ceiling: nothing else starts.
	if started, _ := w.ScheduleQueued(context.Background(), "exe", ""); started != 0 {
		t.Fatalf("started %d past the ceiling", started)
	}
	// The x265 encode ends; the queued x265 takes its slot once VideoToolbox does too.
	stub.kill("running-x265")
	done, _ := LoadJob(filepath.Join(w.cfg.StateDir, "running-x265", "job.json"))
	done.Status = "completed"
	if err := SaveJobAtomic(filepath.Join(w.cfg.StateDir, "running-x265", "job.json"), done); err != nil {
		t.Fatal(err)
	}
	if started, err := w.ScheduleQueued(context.Background(), "exe", ""); err != nil || started != 1 || !jobStarted(t, w, "queued-x265") {
		t.Fatalf("freed x265 slot: started %d, err %v", started, err)
	}
}

// Without the cap, the same queue starts both x265 jobs, as it always did.
func TestSchedulerWithoutX265CapStartsTwoX265(t *testing.T) {
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 2, stub)
	seedEncoderJob(t, w, stub, "a", "queued", "libx265", false, 3*time.Minute)
	seedEncoderJob(t, w, stub, "b", "queued", "libx265", false, 2*time.Minute)
	if started, err := w.ScheduleQueued(context.Background(), "exe", ""); err != nil || started != 2 {
		t.Fatalf("started %d, err %v; no cap must mean no limit", started, err)
	}
}

func TestCountActiveJobsByKindCountsX265(t *testing.T) {
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 4, stub)
	seedEncoderJob(t, w, stub, "x", "running", "libx265", true, time.Hour)
	seedEncoderJob(t, w, stub, "vt", "running", "hevc_videotoolbox", true, time.Hour)
	seedSlotJob(t, w, stub, "pod", "running", slotKindPodcast, true, time.Hour)
	use, err := w.countActiveJobsByKind("")
	if err != nil || use != (slotCounts{Video: 2, Podcast: 1, X265: 1}) {
		t.Fatalf("counts %+v, err %v", use, err)
	}
	observed, err := w.observeSlotUse(true)
	if err != nil || observed.slotCounts != use {
		t.Fatalf("observed %+v, err %v", observed.slotCounts, err)
	}
	status := w.slotsStatus()
	if status.X265 == nil || status.X265.Used != 1 || status.X265.Total != 0 {
		t.Fatalf("status %+v", status.X265)
	}
}

// One VideoToolbox plus one x265 encode on a ceiling of two: x265 is not
// partitioned (it is alone among x265 encodes) and keeps every CPU.
func TestX265ThreadParamsIgnoreVideoToolboxJobsUnderTheCap(t *testing.T) {
	withHostCPUs(t, 10)
	videoInUse, x265InUse := 2, 1
	w := &Worker{cfg: &WorkerConfig{MaxParallelJobs: 2, StateDir: t.TempDir()}, videoInUseFn: func() (int, bool) { return videoInUse, true }, x265InUseFn: func() (int, bool) { return x265InUse, true }}
	// No cap: the VideoToolbox job counts, as it did before the cap existed.
	if got := w.x265ThreadParams(); got != "pools=5:frame-threads=3" {
		t.Fatalf("uncapped: %q", got)
	}
	if _, err := w.SetSlotSplit(transcode.SlotSplit{X265Slots: 1}); err != nil {
		t.Fatal(err)
	}
	if got := w.x265ThreadParams(); got != "" {
		t.Fatalf("1 VideoToolbox + 1 x265 must not partition x265: %q", got)
	}
	x265InUse = 0 // nothing counted: assume the x265 pool, which is one
	if got := w.x265ThreadParams(); got != "" {
		t.Fatalf("capped at one, no partition is ever needed: %q", got)
	}
	if got := w.x265PartitionCandidates(); len(got) != 0 {
		t.Fatalf("no partition to prove under a cap of one: %v", got)
	}
	// A cap of two sizes by the x265 encodes, not the VideoToolbox ones.
	w.cfg.MaxParallelJobs = 3
	if _, err := w.SetSlotSplit(transcode.SlotSplit{X265Slots: 2}); err != nil {
		t.Fatal(err)
	}
	videoInUse, x265InUse = 3, 1
	if got := w.x265ThreadParams(); got != "" {
		t.Fatalf("one x265 among three jobs: %q", got)
	}
	x265InUse = 2
	if got := w.x265ThreadParams(); got != "pools=5:frame-threads=3" {
		t.Fatalf("two x265 share the host: %q", got)
	}
	if got := strings.Join(w.x265PartitionCandidates(), ","); got != "pools=5:frame-threads=3" {
		t.Fatalf("candidates %q", got)
	}
}

func TestX265ThreadParamsUseTheJobCountOnDiskUnderTheCap(t *testing.T) {
	withHostCPUs(t, 10)
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 3, stub)
	if _, err := w.SetSlotSplit(transcode.SlotSplit{X265Slots: 2}); err != nil {
		t.Fatal(err)
	}
	seedEncoderJob(t, w, stub, "x", "running", "libx265", true, time.Hour)
	seedEncoderJob(t, w, stub, "vt", "running", "hevc_videotoolbox", true, time.Hour)
	if got := w.x265ThreadParams(); got != "" {
		t.Fatalf("a single x265 among two video jobs: %q", got)
	}
}

// A worker-slots.json written before the cap existed has no x265_slots key,
// which means no cap.
func TestStoredSlotsWithoutX265KeyMeanNoCap(t *testing.T) {
	w, _, _ := newPR3Worker(t, 2, nil)
	if err := os.MkdirAll(w.cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.cfg.StateDir, slotSplitFile), []byte(`{"video_slots":1,"podcast_slots":1,"updated_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if l := w.slotLimits(); l.X265 != 0 || !l.Split || l.Video != 1 || l.x265Pool() != 1 {
		t.Fatalf("limits %+v", l)
	}
	status := w.slotsStatus()
	if status.X265 == nil || status.X265.Total != 0 {
		t.Fatalf("an old file reports no cap: %+v", status.X265)
	}
}

func TestSetSlotSplitStoresTheX265CapAndClearsIt(t *testing.T) {
	withHostCPUs(t, 10)
	w, _, _ := newPR3Worker(t, 2, nil)
	path := filepath.Join(w.cfg.StateDir, slotSplitFile)
	if _, err := w.SetSlotSplit(transcode.SlotSplit{X265Slots: 1}); err != nil {
		t.Fatal(err)
	}
	if l := w.slotLimits(); l.X265 != 1 || l.Split {
		t.Fatalf("a cap with no split is stored: %+v", l)
	}
	var rec slotSplitRecord
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &rec) != nil || rec.X265Slots != 1 {
		t.Fatalf("stored %s, err %v", data, err)
	}
	// A cap alongside a split.
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 1, PodcastSlots: 1, X265Slots: 1}); err != nil {
		t.Fatal(err)
	}
	if l := w.slotLimits(); l.X265 != 1 || !l.Split {
		t.Fatalf("%+v", l)
	}
	// Clearing everything removes the file.
	if _, err := w.SetSlotSplit(transcode.SlotSplit{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an empty assignment must remove the file: %v", err)
	}
	if l := w.slotLimits(); l.X265 != 0 {
		t.Fatalf("%+v", l)
	}
}

func TestSetSlotSplitRejectsX265CapLargerThanTheVideoPool(t *testing.T) {
	withHostCPUs(t, 10)
	w, _, _ := newPR3Worker(t, 3, nil)
	for _, split := range []transcode.SlotSplit{
		{X265Slots: 4},  // over the shared ceiling
		{X265Slots: -1}, // negative
		{VideoSlots: 1, PodcastSlots: 2, X265Slots: 2}, // over the video slots
	} {
		if _, err := w.SetSlotSplit(split); err == nil {
			t.Errorf("%+v must be refused", split)
		}
	}
}

// A cap that stops fitting after the ceiling or the split shrinks is ignored
// rather than clamped, and a ceiling lowered in the same request clears it.
func TestX265CapThatNoLongerFitsIsIgnored(t *testing.T) {
	withHostCPUs(t, 10)
	w, _, _ := newPR3Worker(t, 3, nil)
	if _, err := w.SetSlotSplit(transcode.SlotSplit{X265Slots: 3}); err != nil {
		t.Fatal(err)
	}
	w.cfg.MaxParallelJobs = 2
	if l := w.slotLimits(); l.X265 != 0 {
		t.Fatalf("a cap above the lowered ceiling: %+v", l)
	}
	w.cfg.MaxParallelJobs = 3
	w.cfg.MaxParallelJobsLimit = 4
	split, err := w.SetSlotSplit(transcode.SlotSplit{X265Slots: 3, Ceiling: 2})
	if err != nil {
		t.Fatal(err)
	}
	if l := w.slotLimits(); l.Ceiling != 2 || l.X265 != 0 || len(split) == 0 {
		t.Fatalf("%+v notes %v", l, split)
	}
}

func TestSlotsEndpointX265Cap(t *testing.T) {
	withHostCPUs(t, 10)
	worker, srv, _, _ := newHTTPTestSetup(t, "secret")
	worker.cfg.MaxParallelJobs = 2
	var out struct {
		Slots transcode.WorkerSlots `json:"slots"`
	}
	put := func(body string) int {
		resp := doRequest(t, srv, "PUT", "/v1/slots", body, "secret")
		out.Slots = transcode.WorkerSlots{}
		_ = json.Unmarshal(resp.Body.Bytes(), &out)
		return resp.Code
	}
	if code := put(`{"video_slots":0,"podcast_slots":0,"x265_slots":1}`); code != 200 || !out.Slots.X265Capable() || out.Slots.X265.Total != 1 {
		t.Fatalf("cap %d %+v", code, out.Slots)
	}
	if code := put(`{"video_slots":0,"podcast_slots":0,"x265_slots":3}`); code != 400 {
		t.Fatalf("a cap over the video pool must be a 400: %d", code)
	}
	if code := put(`{"video_slots":1,"podcast_slots":1,"x265_slots":2}`); code != 400 {
		t.Fatalf("a cap over the video slots must be a 400: %d", code)
	}
	if code := put(`{"video_slots":0,"podcast_slots":0}`); code != 200 || out.Slots.X265.Total != 0 {
		t.Fatalf("omitting the cap clears it: %d %+v", code, out.Slots.X265)
	}
	// The worker always reports the pool, so a server knows it can send the field.
	resp := doRequest(t, srv, "GET", "/v1/slots", "", "secret")
	if !strings.Contains(resp.Body.String(), `"x265":{"total":0,"used":0}`) {
		t.Fatalf("GET /v1/slots: %s", resp.Body.String())
	}
}
