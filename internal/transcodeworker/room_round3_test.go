package transcodeworker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// progress records what an admitted job has already put on this disk: staged
// bytes of its source and written bytes of its output.
func (f *roomFixture) progress(id string, staged, written int64) {
	f.t.Helper()
	path := filepath.Join(f.cfg.StateDir, id, "job.json")
	job, err := LoadJob(path)
	if err != nil {
		f.t.Fatal(err)
	}
	job.StagedInputPath = filepath.Join(f.cfg.StateDir, id, "input.mkv")
	job.LocalCandidatePath = filepath.Join(f.cfg.StateDir, id, "candidate.mkv")
	for p, n := range map[string]int64{job.StagedInputPath: staged, job.LocalCandidatePath: written} {
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := SaveJobAtomic(path, job); err != nil {
		f.t.Fatal(err)
	}
}

// A. What an admitted job promises is what it will still write: its source
// less what is staged, its output reserve less what is written. Free disk shows
// the bytes already on disk, so counting them again would refuse a job that
// fits. Two admissions before either copies, with free disk measured again as
// the copies progress.
func TestRoom_APromisedRoomShrinksAsAnAdmittedJobWritesIt(t *testing.T) {
	for _, policy := range []string{"on_approval", ""} {
		t.Run("policy="+policy, func(t *testing.T) {
			f := newRoomFixture(t, nil)
			f.free.Store(2_000)
			if err := f.tryPolicy("job-first", policy); err != nil {
				t.Fatal(err)
			}
			// Nothing copied yet: the first promises 700 + 350 of the 2000, so the
			// second (1150) does not fit next to it.
			f.mustRefusePolicy("job-second", policy)

			// The copy is half done: 300 staged, so 1700 free; 400 + 350 still promised.
			f.progress("job-first", 300, 0)
			f.free.Store(1_700)
			f.mustRefusePolicy("job-second", policy)

			// The copy is done and the disk is larger than the sum of both: 700 staged,
			// 1600 free, only the 350 reserve still promised. Counting the source
			// twice (as promised and as staged) would leave 550 and refuse.
			f.progress("job-first", 700, 0)
			f.free.Store(1_600)
			if err := f.tryPolicy("job-second", policy); err != nil {
				t.Fatalf("with the source on disk and 1250 left to offer: %v", err)
			}
		})
	}
}

// The output already written is not promised again either.
func TestRoom_AnOutputAlreadyWrittenIsNotPromisedAgain(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.free.Store(2_000)
	f.mustAdmit("job-first")
	f.progress("job-first", 700, roomReserve)
	// Everything is on disk: nothing is promised. 1150 free is exactly enough.
	f.free.Store(1_150)
	f.mustAdmit("job-second")
}

// A job that reuses its own cached copy promises no source: it is on disk.
func TestRoom_AJobThatReusesItsOwnCopyPromisesNoSource(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.ownCacheEntry("own")
	f.free.Store(roomReserve + roomFloor)
	f.mustAdmit("job-requeued")
	if got := f.loadQueued("job-requeued").RoomOwnKey; got != "own" {
		t.Fatalf("own key = %q, want own", got)
	}
	capLock, err := acquireCapacityLock(f.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer capLock.Unlock()
	if _, unwritten, err := f.w.retainedBytesLocked(); err != nil || unwritten != roomReserve {
		t.Fatalf("unwritten = %d (%v), want only its %d reserve", unwritten, err, roomReserve)
	}
}

// B. When a delete fails the node cannot have the room it counted on: it
// refuses with the numbers and never admits.
func TestRoom_AFailedDeleteRefusesInsteadOfAdmitting(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.cacheEntry("spare", 900, time.Hour)
	f.free.Store(300) // needs 1150: only with the 900 freed
	dir := f.w.sourceCacheDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if probe, err := os.Create(filepath.Join(dir, "probe")); err == nil {
		probe.Close()
		t.Skip("a read-only directory does not stop deletes here (running as root)")
	}
	err := f.mustRefuse("job-delete-fails")
	if !strings.Contains(err.Error(), "could free only") {
		t.Fatalf("refusal %q must say what the node could free", err)
	}
}

// C. A queued record written before the size was stored has no
// RoomSourceBytes: the node falls back to the size it knows, so it never
// admits it blindly.
func TestRoom_AnOldRecordWithoutItsSourceSizeIsRefusedWhenTheSourceDoesNotFit(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.submit("job-old", "on_approval")
	path := filepath.Join(f.cfg.StateDir, "job-old", "job.json")
	job := f.loadQueued("job-old")
	job.Status, job.PID, job.RoomSourceBytes, job.RoomSourceMTime = "queued", 0, 0, 0
	job.SourceSizeBytes, job.ExpectedSourceSizeBytes = 0, 0
	job.RetentionBlocked, job.RoomNote, job.RoomOwnKey = "", "", ""
	if err := SaveJobAtomic(path, job); err != nil {
		t.Fatal(err)
	}
	f.free.Store(600) // the source (700) alone does not fit
	f.schedule()
	if got := f.loadQueued("job-old"); got.PID != 0 || got.RetentionBlocked == "" {
		t.Fatalf("job = pid %d blocked %q, want refused for room", got.PID, got.RetentionBlocked)
	}
	f.free.Store(10_000)
	f.schedule()
	if got := f.loadQueued("job-old"); got.PID == 0 {
		t.Fatalf("job = pid %d, want started once there is room", got.PID)
	}
}

// D. A work directory that does not exist reports no capacity: the coordinator
// then cannot say the job never fits.
func TestRoom_AMissingWorkDirReportsNoCapacity(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.w.diskTotal = nil
	f.w.cfg.LocalWorkDir = filepath.Join(t.TempDir(), "missing", "work")
	if _, err := f.w.diskTotalBytes(); err == nil {
		t.Fatal("the capacity of a work directory that does not exist is unknown")
	}
	room := roomOf(t, f)
	if room.DiskCapacityBytes != 0 {
		t.Fatalf("capacity = %d, want none reported", room.DiskCapacityBytes)
	}
	need := transcode.RoomNeed{SourceBytes: 1 << 40, ReserveBytes: 1 << 40}
	if verdict, _ := room.Judge(need, time.Now()); verdict != transcode.RoomFits {
		t.Fatalf("verdict = %v, want fits (the node decides)", verdict)
	}
}

// E. A copy that cannot lock its stage file is not made, and no unlocked live
// stage file is left; a copy that holds its lock is never swept, however old
// its file looks.
func TestRoom_ACopyThatCannotLockItsStageFileIsNotMade(t *testing.T) {
	f := newRoomFixture(t, nil)
	dir := f.w.sourceCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	prev := lockStageFile
	lockStageFile = func(*os.File) error { return errors.New("no lock for you") }
	t.Cleanup(func() { lockStageFile = prev })
	err := copySourceCacheEntry(context.Background(), f.src, filepath.Join(dir, "k.mkv"), roomSource)
	if err == nil || !errors.Is(err, ErrStorageIO) {
		t.Fatalf("err = %v, want a storage error", err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".cache-stage-*"))
	if len(left) != 0 || exists(filepath.Join(dir, "k.mkv")) {
		t.Fatalf("left %v: a copy that could not lock leaves nothing", left)
	}
}

func TestRoom_ALiveCopyIsNeverSweptEvenWhenItsFileLooksOld(t *testing.T) {
	f := newRoomFixture(t, nil)
	dir := f.w.sourceCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	swept := int64(-1)
	var stage string
	stageCopyHook = func(path string) {
		stage = path
		old := time.Now().Add(-time.Hour)
		_ = os.Chtimes(path, old, old)
		swept = f.w.dropHalfCopies()
	}
	t.Cleanup(func() { stageCopyHook = nil })
	data := filepath.Join(dir, "k.mkv")
	if err := copySourceCacheEntry(context.Background(), f.src, data, roomSource); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(stage), ".cache-stage-") {
		t.Fatalf("stage file %q is not named as a half copy", stage)
	}
	if swept != 0 {
		t.Fatalf("the sweep removed %d bytes from a copy that held its lock", swept)
	}
	if fi, err := os.Stat(data); err != nil || fi.Size() != roomSource {
		t.Fatalf("the copy did not complete: %v", err)
	}
}

// F. A cached source an admitted job chose as its own is its until it links
// it: another job's admission never deletes it.
func TestRoom_AnAdmittedJobsOwnCachedCopyIsNotFreeableForAnotherJob(t *testing.T) {
	f := newRoomFixture(t, nil)
	own := f.ownCacheEntry("own")
	f.free.Store(1_000)
	f.mustAdmit("job-owner") // 450 needed: it keeps its own copy
	// A second source needs 1150; 1000 - 350 promised = 650, and only the
	// first job's copy could make up the rest.
	other := filepath.Join(filepath.Dir(f.src), "other.mkv")
	if err := os.WriteFile(other, make([]byte, roomSource), 0o644); err != nil {
		t.Fatal(err)
	}
	req := f.request("job-other", "on_approval")
	req.SourcePath = other
	_, err := f.w.Submit(context.Background(), req, "test-exe", "")
	if err == nil || !transcode.IsRoomRefusal(err) {
		t.Fatalf("err = %v, want a room refusal", err)
	}
	if !exists(own) {
		t.Fatal("the owner's cached copy was deleted for another job")
	}
}

// J. The real cache writer and the job's recorded source time use the same
// unit, so a job that comes back finds its own copy (the original bug).
func TestRoom_TheCacheWriterAndTheJobRecordAgreeOnTheSourceTime(t *testing.T) {
	f := newRoomFixture(t, nil)
	fi, err := os.Stat(f.src)
	if err != nil {
		t.Fatal(err)
	}
	dir := f.w.sourceCacheDir()
	local := filepath.Join(t.TempDir(), "local.mkv")
	if err := os.WriteFile(local, make([]byte, roomSource), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := populateSourceCacheFromLocal(context.Background(), dir, filepath.Clean(f.src), fi, local, "")
	if err != nil || !exists(data) {
		t.Fatalf("populate: %v", err)
	}
	f.free.Store(roomReserve + roomFloor) // fits only if the copy counts as its own
	f.mustAdmit("job-comes-back")
	job := f.loadQueued("job-comes-back")
	if job.RoomOwnKey == "" || job.RoomSourceMTime != fi.ModTime().UTC().UnixNano() {
		t.Fatalf("own key %q, mtime %d, want the cached copy found by the recorded time", job.RoomOwnKey, job.RoomSourceMTime)
	}
	if !exists(data) {
		t.Fatal("the job's own copy was deleted")
	}
}

// J. A hold refusal comes after the replay of a job the node already accepted.
func TestRoom_AnAcceptedJobIsReplayedEvenWhileTheNodeIsPaused(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.mustAdmit("job-accepted")
	if err := f.w.SetHold(true); err != nil {
		t.Fatal(err)
	}
	resp, err := f.w.Submit(context.Background(), f.request("job-accepted", "on_approval"), "test-exe", "")
	if err != nil || !resp.Reused {
		t.Fatalf("replay = %+v (%v), want the accepted job answered as reused", resp, err)
	}
}

// J. A refused job's directory is removed only while it has not started.
func TestRoom_ARefusedSubmitLeavesNoJobAndNoStartedProcess(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.free.Store(100)
	f.mustRefuse("job-nothing-left")
	if exists(filepath.Join(f.cfg.StateDir, "job-nothing-left")) {
		t.Fatal("a refused job's directory must be gone")
	}
}

// K. The hard floor stops the most recently started encode, not all of them.
func TestRoom_TheHardFloorStopsOnlyTheNewestEncode(t *testing.T) {
	f := newRoomFixture(t, nil)
	now := time.Now().UTC()
	for id, started := range map[string]time.Time{"job-old": now.Add(-time.Hour), "job-new": now} {
		dir := filepath.Join(f.cfg.StateDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		job := &JobRecord{ID: id, Status: "running", PID: 1, StartedAt: started, CreatedAt: started, RetainReserveBytes: roomReserve, PublishPolicy: transcode.PublishPolicyOnApproval}
		if err := SaveJobAtomic(filepath.Join(dir, "job.json"), job); err != nil {
			t.Fatal(err)
		}
	}
	f.free.Store(roomFloor/2 - 1)
	for id, stops := range map[string]bool{"job-old": false, "job-new": true} {
		job := &JobRecord{ID: id, PublishPolicy: transcode.PublishPolicyOnApproval, RetainReserveBytes: roomReserve}
		ctx := f.w.withDiskFloor(context.Background(), job, filepath.Join(t.TempDir(), "out.mkv"), "")
		err := encodeSizeWatchFrom(ctx).check()
		if _, stopped := err.(*diskFloorStop); stopped != stops {
			t.Fatalf("%s: check = %v, want stop = %v", id, err, stops)
		}
	}
}

// L. The disk_floor stop end to end on the worker: the encode's watch sees the
// disk under the hard floor, the job fails with the reason disk_floor, and the
// status the coordinator polls carries it (the coordinator requeues on it, see
// action's TestADiskFloorStopPutsTheConversionBackInTheQueue).
func TestRoom_ADiskFloorStopReachesTheCoordinatorAsReasonDiskFloor(t *testing.T) {
	f := newRoomFixture(t, nil)
	f.submit("job-floor", "on_approval")
	f.w.SetRunFFmpeg(func(ctx context.Context, _ *ExecutionPlan, _ *JobRecord, _, out, _, _ string) error {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, []byte("partial"), 0o644); err != nil {
			return err
		}
		f.free.Store(roomFloor/2 - 1) // the disk fills under the encode
		watch := encodeSizeWatchFrom(ctx)
		if watch == nil {
			return errors.New("the encode has no disk watch")
		}
		return watch.check()
	})
	_ = f.w.InternalRun(context.Background(), "job-floor")
	st, err := f.w.Status(context.Background(), "job-floor")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var seen transcode.JobStatus
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatal(err)
	}
	if seen.Status != transcode.StatusFailed || seen.ReasonCode != "disk_floor" || seen.FailureClassification != "disk_floor" {
		t.Fatalf("status = %s reason %q class %q (%s), want failed with disk_floor", seen.Status, seen.ReasonCode, seen.FailureClassification, seen.Error)
	}
}
