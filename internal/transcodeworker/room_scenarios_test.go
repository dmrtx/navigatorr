package transcodeworker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// Scenarios of room admission (docs/transcode/DEFERRED_CANDIDATE_PUBLICATION.md,
// "Room"): the real code admits or refuses a deferred job against a fake free
// disk and fake source cache entries. Sizes are in bytes scaled down: the
// source is 700 bytes, so its output reserve is 350 (half) and the floor 100.
const (
	roomSource  = int64(700)
	roomReserve = roomSource / 2
	roomFloor   = int64(100)
)

type roomFixture struct {
	*retainedFixture
	free atomic.Int64
}

// newRoomFixture is a node with a free-disk floor and a source of roomSource
// bytes. free is the disk it measures.
func newRoomFixture(t *testing.T, mutate func(*WorkerConfig)) *roomFixture {
	t.Helper()
	f := &roomFixture{retainedFixture: newRetainedFixture(t, func(c *WorkerConfig) {
		c.MinFreeDiskBytes = roomFloor
		c.MaxParallelJobs = 2
		if mutate != nil {
			mutate(c)
		}
	})}
	if err := os.WriteFile(f.src, make([]byte, roomSource), 0o644); err != nil {
		t.Fatal(err)
	}
	f.free.Store(10_000)
	f.install()
	return f
}

func (f *roomFixture) install() {
	f.w.freeDisk = func(string) (int64, error) { return f.free.Load(), nil }
	f.w.diskTotal = func(string) (int64, error) { return 100_000, nil }
}

// restart is the worker process coming back on the same state directory.
func (f *roomFixture) restart() {
	f.w = f.newWorker()
	f.install()
}

// try submits an on_approval job and returns the node's answer.
func (f *roomFixture) try(id string) error { return f.tryPolicy(id, "on_approval") }

// tryPolicy submits a job with the given publish policy ("" publishes on
// encode, as every batch child may).
func (f *roomFixture) tryPolicy(id, policy string) error {
	_, err := f.w.Submit(context.Background(), f.request(id, policy), "test-exe", "")
	return err
}

func (f *roomFixture) mustAdmit(id string) {
	f.t.Helper()
	if err := f.try(id); err != nil {
		f.t.Fatalf("%s refused: %v", id, err)
	}
	if job := f.loadQueued(id); job.PID == 0 || job.RetentionBlocked != "" {
		f.t.Fatalf("%s = pid %d blocked %q, want started", id, job.PID, job.RetentionBlocked)
	}
}

func (f *roomFixture) mustRefuse(id string) error {
	f.t.Helper()
	return f.mustRefusePolicy(id, "on_approval")
}

func (f *roomFixture) mustRefusePolicy(id, policy string) error {
	f.t.Helper()
	err := f.tryPolicy(id, policy)
	if err == nil || !transcode.IsRoomRefusal(err) {
		f.t.Fatalf("%s: err = %v, want a room refusal", id, err)
	}
	if status, code := classifySubmitError(err); status != http.StatusTooManyRequests || code != transcode.WorkerErrorBusy {
		f.t.Fatalf("%s: classify = %d %q, want 429 worker_busy", id, status, code)
	}
	// Nothing is left behind on the node: the job is in the server's queue.
	if _, statErr := os.Stat(filepath.Join(f.cfg.StateDir, id, "job.json")); !os.IsNotExist(statErr) {
		f.t.Fatalf("%s: a refused job must not stay on the node (%v)", id, statErr)
	}
	return err
}

// cacheEntry makes a cached source of size bytes for another file, last used
// age ago.
func (f *roomFixture) cacheEntry(key string, size int64, age time.Duration) string {
	f.t.Helper()
	return f.writeCache(key, "/library/"+key+".mkv", size, 1, age)
}

// ownCacheEntry makes the cached copy of this job's own source.
func (f *roomFixture) ownCacheEntry(key string) string {
	f.t.Helper()
	fi, err := os.Stat(f.src)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.writeCache(key, filepath.Clean(f.src), fi.Size(), fi.ModTime().UTC().UnixNano(), time.Hour)
}

func (f *roomFixture) writeCache(key, source string, size, mtime int64, age time.Duration) string {
	f.t.Helper()
	dir := f.w.sourceCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	data := filepath.Join(dir, key+".mkv")
	if err := os.WriteFile(data, make([]byte, size), 0o644); err != nil {
		f.t.Fatal(err)
	}
	meta, _ := json.Marshal(sourceIdentity{Source: source, Size: size, ModTime: mtime})
	if err := os.WriteFile(filepath.Join(dir, key+".meta.json"), meta, 0o644); err != nil {
		f.t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(data, at, at); err != nil {
		f.t.Fatal(err)
	}
	return data
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// The real failure: a big source was copied to the node, the job was cancelled
// by an immediate pause and came back. Its own copy was counted as used and its
// output reserve was the whole source, so the node waited while idle.
func TestRoom_ARequeuedJobIsNotCountedAgainstItsOwnCachedSource(t *testing.T) {
	f := newRoomFixture(t, nil)
	own := f.ownCacheEntry("own")
	// The copy already took its 700 bytes: 450 free is the 350 reserve plus the
	// 100 floor. Counting the source again would need 1150.
	f.free.Store(roomReserve + roomFloor)
	f.mustAdmit("job-requeued")
	if !exists(own) {
		t.Fatal("a job's own cached source must never be deleted for it")
	}
}

// Other jobs' cached sources go oldest first, only as many as the job needs;
// an entry a running job holds and a finished candidate waiting for approval
// are never touched.
func TestRoom_OtherCachedSourcesGoOldestFirstAndOnlyAsManyAsNeeded(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.retain("job-waiting-approval")
	candidate := f.loadHidden("job-waiting-approval").LocalCandidatePath
	oldest := f.cacheEntry("oldest", 400, 5*time.Hour)
	middle := f.cacheEntry("middle", 500, 3*time.Hour)
	newest := f.cacheEntry("newest", 600, time.Hour)
	inUse := f.cacheEntry("inuse", 900, 9*time.Hour) // the oldest, but a running job links it
	if err := os.Link(inUse, filepath.Join(t.TempDir(), "job-link")); err != nil {
		t.Fatal(err)
	}
	// need = 700 + 350 + 100 = 1150; free 300: 850 more is wanted. The oldest
	// freeable (400) and the middle one (500) are enough.
	f.free.Store(300)
	f.mustAdmit("job-needs-room")
	if exists(oldest) || exists(middle) {
		t.Fatal("the oldest freeable entries must be deleted")
	}
	if !exists(newest) {
		t.Fatal("an entry the job did not need to free must stay")
	}
	if !exists(inUse) {
		t.Fatal("a source a running job holds is never freeable")
	}
	if !exists(candidate) {
		t.Fatal("a candidate waiting for approval is never freeable")
	}
	if exists(strings.TrimSuffix(oldest, ".mkv") + ".meta.json") {
		t.Fatal("a deleted entry takes its sidecar with it")
	}
}

// What the node cannot offer even after deleting everything it may delete is
// refused at once, with the numbers, and nothing is deleted for a job that
// still would not fit.
func TestRoom_ARefusalIsImmediateNamesTheNumbersAndDeletesNothing(t *testing.T) {
	f := newRoomFixture(t, nil)
	entry := f.cacheEntry("spare", 200, time.Hour)
	f.free.Store(100) // can offer 100 + 200 = 300 of the 1150 needed
	err := f.mustRefuse("job-too-big-now")
	if !strings.Contains(err.Error(), "can offer") || !strings.Contains(err.Error(), "needs") {
		t.Fatalf("refusal %q must say what is needed and what the node can offer", err)
	}
	if !exists(entry) {
		t.Fatal("nothing may be deleted for a job that still would not fit")
	}
	// Room appears (a spare file went elsewhere): the same job is admitted.
	f.free.Store(10_000)
	f.mustAdmit("job-too-big-now")
}

// Two jobs for one node: the oldest is admitted, the other is refused at once
// (the server sends it elsewhere), since the first one's unwritten output
// reserve is not free for it.
func TestRoom_TwoJobsForOneNodeTheOldestIsAdmittedAndTheOtherRefused(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.free.Store(1_300) // one job needs 1150; two need 2300
	f.mustAdmit("job-first")
	f.mustRefuse("job-second")
	// The first one finishes its encode: its candidate is written and counted
	// by its size, so the second may be sent again once the node has room.
	f.free.Store(2_500)
	f.mustAdmit("job-second")
}

// A paused node (graceful or immediate: a hold; suspend) and a node held by a
// build take no new job: they refuse at once, and take it after the resume.
func TestRoom_ANodeThatIsPausedOrHeldByABuildRefusesAndResumes(t *testing.T) {
	f := newRoomFixture(t, func(c *WorkerConfig) { c.BuildNode = "build" })

	if err := f.w.SetHold(true); err != nil {
		t.Fatal(err)
	}
	if err := f.mustRefuse("job-held"); !strings.Contains(err.Error(), "paused") {
		t.Fatalf("a held node says why: %v", err)
	}
	if err := f.w.SetHold(false); err != nil {
		t.Fatal(err)
	}

	if err := f.w.SetSuspend(true); err != nil {
		t.Fatal(err)
	}
	if err := f.mustRefuse("job-suspended"); !strings.Contains(err.Error(), "paused") {
		t.Fatalf("a suspended node says why: %v", err)
	}
	if err := f.w.SetSuspend(false); err != nil {
		t.Fatal(err)
	}

	lease, err := f.w.LeaseCreate(LeaseRequest{Source: "tester", Title: "compile", TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.mustRefuse("job-build"); !strings.Contains(err.Error(), "build") {
		t.Fatalf("a node a build holds says why: %v", err)
	}
	if _, err := f.w.LeaseFinish(lease.ID, leaseStatusSucceeded, new(int)); err != nil {
		t.Fatal(err)
	}
	f.mustAdmit("job-after-resume")
}

// The report the node gives the server says what it can take: the disk it can
// offer (free, plus deletable cache, less the floor and the unwritten reserves)
// and whether it is open.
func TestRoom_TheNodeReportsWhatItCanTake(t *testing.T) {
	f := newRoomFixture(t, func(c *WorkerConfig) { c.RetainedCandidatesMaxBytes = 5_000 })
	f.cacheEntry("spare", 300, time.Hour)
	f.free.Store(2_000)
	f.mustAdmit("job-running") // promises its 700 source and 350 reserve, none written yet
	room := roomOf(t, f)
	// 2000 free - 1050 promised + 300 deletable - 100 floor
	if room.DiskRoomBytes != 1_150 || room.DiskCapacityBytes != 100_000 || room.FloorBytes != roomFloor || room.CapacityBytes != 5_000 || room.RoomBytes != 4_650 || room.HeldBytes != 350 || room.Hold != "" {
		t.Fatalf("report = %+v", room)
	}
	if err := f.w.SetHold(true); err != nil {
		t.Fatal(err)
	}
	if got := roomOf(t, f).Hold; got != "paused" {
		t.Fatalf("hold = %q, want paused", got)
	}
}

func roomOf(t *testing.T, f *roomFixture) *transcode.RetentionRoom {
	t.Helper()
	capLock, err := acquireCapacityLock(f.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer capLock.Unlock()
	room, err := f.w.roomReport(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return room
}

// With a retention budget full of candidates waiting for approval, a job that
// asked to wait is refused (not queued on the node) and says it waits for
// approvals; one that did not ask publishes on encode.
func TestRoom_ARetentionBudgetFullOfApprovalsRefusesAJobThatWaits(t *testing.T) {
	f := newRoomFixture(t, func(c *WorkerConfig) { c.RetainedCandidatesMaxBytes = int64(len("dummy-media")) + 5 })
	if err := os.WriteFile(f.src, []byte("dummy-media"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.retain("job-held-1") // its candidate waits for approval
	req := f.request("job-wait", "on_approval")
	req.RetentionWait = true
	_, err := f.w.Submit(context.Background(), req, "test-exe", "")
	if err == nil || !transcode.IsRoomRefusal(err) || !strings.Contains(err.Error(), "approvals") {
		t.Fatalf("err = %v, want a refusal that waits for approvals", err)
	}
	f.submit("job-spill", "on_approval") // asked not to wait: publishes on encode
	if job := f.loadQueued("job-spill"); job.PID == 0 || job.PublishFallback != transcode.PublishFallbackRetentionFull {
		t.Fatalf("job-spill = pid %d fallback %q, want started to publish on encode", job.PID, job.PublishFallback)
	}
}

// An immediate pause clears what the node keeps for itself: cached sources no
// job holds. Candidates waiting for approval stay. After the resume a job
// that needs room admits as usual.
func TestRoom_AnImmediatePauseClearsTheFreeableCacheAndKeepsRetainedOutputs(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.retain("job-waiting-approval")
	candidate := f.loadHidden("job-waiting-approval").LocalCandidatePath
	spare := f.cacheEntry("spare", 400, time.Hour)
	inUse := f.cacheEntry("inuse", 300, time.Hour)
	if err := os.Link(inUse, filepath.Join(t.TempDir(), "job-link")); err != nil {
		t.Fatal(err)
	}
	freed, err := f.w.ClearFreeableSourceCache()
	if err != nil || freed != 400 {
		t.Fatalf("freed %d (%v), want the 400 bytes of the spare entry", freed, err)
	}
	if exists(spare) || !exists(inUse) || !exists(candidate) {
		t.Fatalf("spare %v, in use %v, candidate %v: only the spare entry may go", exists(spare), exists(inUse), exists(candidate))
	}
	f.mustAdmit("job-after-resume")
}

// The same through the worker's HTTP hold: PUT /v1/hold with clear_cache.
func TestRoom_TheHoldRequestCanClearTheCache(t *testing.T) {
	f := newRoomFixture(t, nil)
	spare := f.cacheEntry("spare", 400, time.Hour)
	srv := &Server{worker: f.w}
	req, _ := http.NewRequest(http.MethodPut, "/v1/hold", strings.NewReader(`{"hold":true,"clear_cache":true}`))
	rec := httptest.NewRecorder()
	srv.handleHold(rec, req)
	if rec.Code != http.StatusOK || exists(spare) || !f.w.Held() {
		t.Fatalf("code %d, spare kept %v, held %v: %s", rec.Code, exists(spare), f.w.Held(), rec.Body.String())
	}
}

// After a restart a complete cached source nobody holds is freeable and a half
// copy is not counted as a source.
func TestRoom_AfterARestartCompleteCachedSourcesAreFreeableAndHalfCopiesAreNot(t *testing.T) {
	f := newRoomFixture(t, nil)
	complete := f.cacheEntry("complete", 800, time.Hour)
	dir := f.w.sourceCacheDir()
	half := filepath.Join(dir, ".cache-stage-123")
	if err := os.WriteFile(half, make([]byte, 5_000), 0o644); err != nil {
		t.Fatal(err)
	}
	f.restart()
	if err := f.w.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 300 free + 800 deletable covers 1150 only if the 5000-byte half copy is
	// ignored as a source and the complete entry is deletable.
	f.free.Store(400)
	f.mustAdmit("job-after-restart")
	if exists(complete) {
		t.Fatal("a complete cached source nobody holds is freeable after a restart")
	}
}

// The hard floor: free disk falling under it while a job encodes stops the
// encode as a disk_floor stop, which the server requeues, never a failure of
// the job itself.
func TestRoom_FreeDiskUnderTheHardFloorStopsTheEncodeForARequeue(t *testing.T) {
	f := newRoomFixture(t, nil)
	job := &JobRecord{ID: "job-encoding", PublishPolicy: transcode.PublishPolicyOnApproval, RetainReserveBytes: roomReserve}
	f.free.Store(roomFloor / 2) // exactly the hard floor
	ctx := f.w.withDiskFloor(context.Background(), job, filepath.Join(t.TempDir(), "out.mkv"), "")
	watch := encodeSizeWatchFrom(ctx)
	if watch == nil {
		t.Fatal("a deferred job under a floor is watched")
	}
	if err := watch.check(); err != nil {
		t.Fatalf("at the hard floor: %v, want the encode to go on", err)
	}
	f.free.Store(roomFloor/2 - 1)
	err := watch.check()
	stop, ok := err.(*diskFloorStop)
	if !ok || !strings.Contains(err.Error(), "disk_floor") || stop.hard != roomFloor/2 {
		t.Fatalf("check = %v, want a disk_floor stop", err)
	}
}

// A job whose output grows past what it reserved is not compressing: it fails
// at once, saying how big the output is against the source, from what is
// written or projected.
func TestRoom_AnOutputPastItsReserveIsNotCompressing(t *testing.T) {
	rule := qualityModeProjection
	// Written alone: 400 of the 350 reserved, 10% into the file.
	r := decideReserveRefusal(roomSource, roomReserve, 100, rule, nil, 400, 10)
	if r == nil || r.Rejection.ReasonCode != "insufficient_savings" || !strings.Contains(r.Rejection.Message, "not compressing") || !strings.Contains(r.Rejection.Message, "57% of the") {
		t.Fatalf("refusal = %+v, want not compressing at 57%% of the source", r)
	}
	// Within the reserve and not projected: it goes on.
	if r := decideReserveRefusal(roomSource, roomReserve, 100, rule, nil, 300, 10); r != nil {
		t.Fatalf("300 of 350: %+v", r)
	}
	// Projected from half way: 200 at 60% of the file with a steady rate gives
	// ~333, still within; 300 at 60% gives 500, over the reserve by more than
	// the margin: it stops before the limit.
	base := &sizeWatchPoint{bytes: 0, encodedSec: 1}
	if r := decideReserveRefusal(roomSource, roomReserve, 100, rule, base, 200, 60); r != nil {
		t.Fatalf("a projection within the reserve stopped: %+v", r)
	}
	if r := decideReserveRefusal(roomSource, roomReserve, 100, rule, base, 340, 60); r == nil {
		t.Fatal("a projection far past the reserve must stop before the limit")
	}
}

// The reserve depends on the goal: a size goal reserves its byte budget, a
// quality or compact goal half of the source and an audio-only goal the most
// the size policy accepts.
func TestRoom_TheReserveFollowsTheGoal(t *testing.T) {
	size := &transcode.Plan{SizePolicy: &transcode.SizePolicy{MinSavingsPercent: 10, MaxCandidateBytes: 120}}
	if got := transcode.OutputReserveBytes(size, 1000); got != 120 {
		t.Fatalf("size goal reserve = %d, want its budget 120", got)
	}
	if got := transcode.OutputReserveBytes(&transcode.Plan{SizePolicy: &transcode.SizePolicy{MinSavingsPercent: 10, MaxCandidateBytes: 5000}}, 1000); got != 1000 {
		t.Fatalf("a budget over the source reserves the source, got %d", got)
	}
	if got := transcode.OutputReserveBytes(&transcode.Plan{SizePolicy: &transcode.SizePolicy{MinSavingsPercent: 15}}, 1000); got != 500 {
		t.Fatalf("quality goal reserve = %d, want half", got)
	}
	if got := transcode.OutputReserveBytes(nil, 1000); got != 500 {
		t.Fatalf("no plan reserve = %d, want half", got)
	}
	audio := &transcode.Plan{VideoCodec: transcode.VideoCodecCopy, SizePolicy: &transcode.SizePolicy{MinSavingsPercent: 10}}
	if got := transcode.OutputReserveBytes(audio, 1000); transcode.IsSizeAudioOnlyPlan(audio) && got != 900 {
		t.Fatalf("audio-only reserve = %d, want the source less its minimum saving (900)", got)
	}
}

// The job's reserve is stored from its plan when it is submitted.
func TestRoom_ASubmittedJobKeepsItsReserveAndSource(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.mustAdmit("job-keeps-reserve")
	job := f.loadQueued("job-keeps-reserve")
	if job.RetainReserveBytes != roomReserve || job.RoomSourceBytes != roomSource || job.RoomSourceMTime == 0 {
		t.Fatalf("job = reserve %d source %d mtime %d", job.RetainReserveBytes, job.RoomSourceBytes, job.RoomSourceMTime)
	}
}

// Job records written before this change carry floor_unmet_* fields: they are
// ignored, never a crash.
func TestRoom_OldRecordsWithTheFloorUnmetFieldsStillLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.json")
	old := `{"id":"job-old","status":"queued","retention_blocked":"retention_alone","floor_unmet_since":"2026-01-01T00:00:00Z","floor_unmet_seen_at":"2026-01-01T00:00:00Z","retain_reserve_bytes":9}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	job, err := LoadJob(path)
	if err != nil || job.ID != "job-old" || job.RetainReserveBytes != 9 {
		t.Fatalf("job = %+v (%v)", job, err)
	}
}

// Every job uses this node's disk, whether its output waits for approval or is
// published on encode: a publish-on-encode job (a batch child, say) takes the
// same room admission, with the same oldest-first eviction.
func TestRoom_APublishOnEncodeJobEvictsOtherCachedSourcesOldestFirst(t *testing.T) {
	f := newRoomFixture(t, nil)
	oldest := f.cacheEntry("oldest", 400, 5*time.Hour)
	middle := f.cacheEntry("middle", 500, 3*time.Hour)
	newest := f.cacheEntry("newest", 600, time.Hour)
	f.free.Store(300) // needs 1150: the oldest two are enough
	if err := f.tryPolicy("job-plain", ""); err != nil {
		t.Fatal(err)
	}
	if job := f.loadQueued("job-plain"); job.PID == 0 || job.RetainReserveBytes != roomReserve {
		t.Fatalf("job = pid %d reserve %d, want started with its reserve", job.PID, job.RetainReserveBytes)
	}
	if exists(oldest) || exists(middle) || !exists(newest) {
		t.Fatalf("oldest %v middle %v newest %v: only the two oldest may go", exists(oldest), exists(middle), exists(newest))
	}
}

func TestRoom_APublishOnEncodeJobThatCannotFitIsRefusedAtOnce(t *testing.T) {
	f := newRoomFixture(t, nil)
	spare := f.cacheEntry("spare", 200, time.Hour)
	f.free.Store(100)
	err := f.mustRefusePolicy("job-plain-big", "")
	if !strings.Contains(err.Error(), "needs") {
		t.Fatalf("refusal %q must say what is needed", err)
	}
	if !exists(spare) {
		t.Fatal("nothing may be deleted for a job that would not fit")
	}
	f.free.Store(10_000)
	if err := f.tryPolicy("job-plain-big", ""); err != nil {
		t.Fatalf("with room it is admitted: %v", err)
	}
}

// A running publish-on-encode job's output is promised room too: the next job
// sees it as unwritten and is refused if the rest does not fit.
func TestRoom_APublishOnEncodeJobsUnwrittenOutputIsPromisedRoom(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.free.Store(1_300)
	if err := f.tryPolicy("job-first", ""); err != nil {
		t.Fatal(err)
	}
	f.mustRefusePolicy("job-second", "on_approval")
}

func TestRoom_APausedNodeRefusesAPublishOnEncodeJobToo(t *testing.T) {
	f := newRoomFixture(t, nil)
	if err := f.w.SetHold(true); err != nil {
		t.Fatal(err)
	}
	f.mustRefusePolicy("job-plain-held", "")
	if err := f.w.SetHold(false); err != nil {
		t.Fatal(err)
	}
	if err := f.tryPolicy("job-plain-held", ""); err != nil {
		t.Fatal(err)
	}
}

// The hard floor and the reserve watch apply to a publish-on-encode job.
func TestRoom_APublishOnEncodeJobIsWatchedForTheHardFloorAndItsReserve(t *testing.T) {
	f := newRoomFixture(t, nil)
	job := &JobRecord{ID: "job-plain-encoding", RetainReserveBytes: roomReserve, SourceSizeBytes: roomSource, DurationSec: 100, Plan: &transcode.Plan{}}
	ctx := f.w.withDiskFloor(withEncodeSizeWatch(context.Background(), job, filepath.Join(t.TempDir(), "out.mkv"), ""), job, "", "")
	watch := encodeSizeWatchFrom(ctx)
	if watch == nil || watch.reserveBytes != roomReserve {
		t.Fatalf("watch = %+v, want one holding the reserve", watch)
	}
	f.free.Store(roomFloor / 2)
	if err := watch.check(); err != nil {
		t.Fatalf("at the hard floor: %v", err)
	}
	f.free.Store(roomFloor/2 - 1)
	if _, ok := watch.check().(*diskFloorStop); !ok {
		t.Fatal("under the hard floor the encode must stop for a requeue")
	}
}

// A half copy in the source cache that no live copy holds is free disk again:
// it goes before admission measures, and at start.
func TestRoom_AStaleHalfCopyIsDroppedAndItsBytesCountAsFree(t *testing.T) {
	f := newRoomFixture(t, nil)
	dir := f.w.sourceCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	half := filepath.Join(dir, ".cache-stage-dead")
	if err := os.WriteFile(half, make([]byte, 5_000), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(half, old, old); err != nil {
		t.Fatal(err)
	}
	// The disk shows the half copy's bytes only while the file is there.
	f.w.freeDisk = func(string) (int64, error) {
		if exists(half) {
			return 300, nil
		}
		return 5_300, nil
	}
	f.mustAdmit("job-after-half-copy")
	if exists(half) {
		t.Fatal("a half copy nobody holds must be deleted before admission measures")
	}
}

func TestRoom_AHalfCopyALiveCopyHoldsOrJustWrittenIsKept(t *testing.T) {
	f := newRoomFixture(t, nil)
	dir := f.w.sourceCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	held := filepath.Join(dir, ".cache-stage-live")
	young := filepath.Join(dir, ".cache-stage-young")
	for _, p := range []string{held, young} {
		if err := os.WriteFile(p, make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(held, old, old); err != nil {
		t.Fatal(err)
	}
	live, err := os.Open(held) // the copy in progress holds its lock
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := syscall.Flock(int(live.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if freed := f.w.dropHalfCopies(); freed != 0 || !exists(held) || !exists(young) {
		t.Fatalf("freed %d, held kept %v, young kept %v: a live or fresh copy must stay", freed, exists(held), exists(young))
	}
	_ = syscall.Flock(int(live.Fd()), syscall.LOCK_UN) // the copy ended
	if freed := f.w.dropHalfCopies(); freed != 100 || exists(held) || !exists(young) {
		t.Fatalf("freed %d, held kept %v: an ended copy goes, a fresh one stays", freed, exists(held))
	}
}

func TestRoom_AStartupDropsHalfCopiesACrashLeft(t *testing.T) {
	f := newRoomFixture(t, nil)
	dir := f.w.sourceCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	half := filepath.Join(dir, ".cache-stage-crash")
	if err := os.WriteFile(half, make([]byte, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(half, old, old); err != nil {
		t.Fatal(err)
	}
	f.restart()
	if err := f.w.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exists(half) {
		t.Fatal("the half copy a crash left must go at start")
	}
}
