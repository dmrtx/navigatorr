package transcodeworker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// A file larger than the whole retention budget runs alone: it waits for the
// node to hold no candidate, and nothing that would keep a candidate starts
// ahead of it meanwhile.

const (
	tooLargeBudget = int64(40)
	tooLargeSize   = 50 // bytes of the too-large source
	smallSize      = 3  // bytes of a source that fits the budget
)

// source writes a file of n bytes beside the fixture's source and returns it.
func (f *retainedFixture) source(name string, n int) string {
	f.t.Helper()
	path := filepath.Join(filepath.Dir(f.src), name)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", n)), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// submitFrom submits an on_approval job reading from src, optionally waiting
// for retention room.
func (f *retainedFixture) submitFrom(id, src string, wait bool) {
	f.t.Helper()
	req := f.request(id, "on_approval")
	req.SourcePath = src
	req.RetentionWait = wait
	if _, err := f.w.Submit(context.Background(), req, "test-exe", ""); err != nil {
		f.t.Fatalf("Submit(%s): %v", id, err)
	}
}

func newTooLargeFixture(t *testing.T, mutate func(*WorkerConfig)) *retainedFixture {
	return newRetainedFixture(t, func(c *WorkerConfig) {
		c.MaxParallelJobs = 4
		c.RetainedCandidatesMaxBytes = tooLargeBudget
		if mutate != nil {
			mutate(c)
		}
	})
}

func TestTooLargeJobRunsAloneWhenTheNodeHoldsNothing(t *testing.T) {
	f := newTooLargeFixture(t, nil)
	f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
	job := f.loadQueued("big")
	if job.PID == 0 || job.RetentionBlocked != "" || job.PublishFallback != "" || !job.deferredPublication() {
		t.Fatalf("big = pid %d blocked %q fallback %q deferred %v; want started, still keeping its candidate", job.PID, job.RetentionBlocked, job.PublishFallback, job.deferredPublication())
	}
	if err := f.w.InternalRun(context.Background(), "big"); err != nil {
		t.Fatal(err)
	}
	f.assertRetained("big")
}

func TestTooLargeJobWaitsForTheNodeToEmptyAndStartsWhenItDoes(t *testing.T) {
	ctx := context.Background()
	f := newTooLargeFixture(t, nil)
	f.submitFrom("held", f.source("small.mkv", smallSize), true)
	if err := f.w.InternalRun(ctx, "held"); err != nil {
		t.Fatal(err)
	}
	f.assertRetained("held")

	f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
	if job := f.loadQueued("big"); job.PID != 0 || job.RetentionBlocked != retentionBlockedAlone {
		t.Fatalf("big = pid %d blocked %q; want queued, %q", job.PID, job.RetentionBlocked, retentionBlockedAlone)
	}
	if st, err := f.w.Status(ctx, "big"); err != nil || st.RetentionBlocked != retentionBlockedAlone {
		t.Fatalf("status = %+v (%v), want retention_blocked %q", st, err, retentionBlockedAlone)
	}
	f.schedule()
	if job := f.loadQueued("big"); job.PID != 0 {
		t.Fatal("big must keep waiting while the node holds a candidate")
	}

	// Releasing the retained candidate empties the node: big starts.
	if _, err := f.w.Cancel(ctx, "held"); err != nil {
		t.Fatal(err)
	}
	f.schedule()
	job := f.loadQueued("big")
	if job.PID == 0 || job.RetentionBlocked != "" || job.PublishFallback != "" || !job.deferredPublication() {
		t.Fatalf("big = pid %d blocked %q fallback %q; want started and keeping its candidate", job.PID, job.RetentionBlocked, job.PublishFallback)
	}
	if err := f.w.InternalRun(ctx, "big"); err != nil {
		t.Fatal(err)
	}
	f.assertRetained("big")
}

func TestNothingKeepsACandidateAheadOfAWaitingTooLargeJob(t *testing.T) {
	ctx := context.Background()
	f := newTooLargeFixture(t, nil)
	f.submitFrom("held", f.source("small.mkv", smallSize), true)
	if err := f.w.InternalRun(ctx, "held"); err != nil {
		t.Fatal(err)
	}
	f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)

	// A later standalone job that would fit the budget stays queued behind it.
	f.submitFrom("later", f.source("later.mkv", smallSize), true)
	if job := f.loadQueued("later"); job.PID != 0 || job.RetentionBlocked == "" {
		t.Fatalf("later = pid %d blocked %q; want queued behind the too-large job", job.PID, job.RetentionBlocked)
	}
	f.schedule()
	if job := f.loadQueued("later"); job.PID != 0 {
		t.Fatal("a small standalone job must not start ahead of the too-large one")
	}

	// A job that publishes on encode (a batch child: it does not wait) still runs.
	f.submitFrom("child", f.source("child.mkv", smallSize), false)
	child := f.loadQueued("child")
	if child.PID == 0 || child.PublishFallback != transcode.PublishFallbackRetentionFull || child.deferredPublication() {
		t.Fatalf("child = pid %d fallback %q deferred %v; want started to publish on encode", child.PID, child.PublishFallback, child.deferredPublication())
	}

	// Once the node is empty the too-large job goes first; the small one is
	// refused by the budget it fills.
	if _, err := f.w.Cancel(ctx, "held"); err != nil {
		t.Fatal(err)
	}
	if err := f.w.InternalRun(ctx, "child"); err != nil {
		t.Fatal(err)
	}
	f.schedule()
	if job := f.loadQueued("big"); job.PID == 0 {
		t.Fatal("the too-large job must start once the node holds nothing")
	}
	if job := f.loadQueued("later"); job.PID != 0 {
		t.Fatal("the small job must keep waiting while the too-large one holds the whole node")
	}
}

func TestRetentionRoomIsZeroWhileATooLargeJobWaits(t *testing.T) {
	ctx := context.Background()
	f := newTooLargeFixture(t, nil)
	f.submitFrom("held", f.source("small.mkv", smallSize), true)
	if err := f.w.InternalRun(ctx, "held"); err != nil {
		t.Fatal(err)
	}
	held, _, err := f.w.retainedBytesLocked()
	if err != nil || held <= 0 || held >= tooLargeBudget {
		t.Fatalf("held = %d (%v), want a candidate under the budget", held, err)
	}
	room := func() *transcode.RetentionRoom {
		f.w.refreshRetentionRoom(time.Now().UTC())
		f.w.schedulerMu.Lock()
		defer f.w.schedulerMu.Unlock()
		if f.w.retentionRoom == nil {
			t.Fatal("no retention room reported")
		}
		r := *f.w.retentionRoom
		return &r
	}
	if r := room(); r.RoomBytes != tooLargeBudget-held || r.CapacityBytes != tooLargeBudget {
		t.Fatalf("room = %+v, want %d left of %d", r, tooLargeBudget-held, tooLargeBudget)
	}
	f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
	if r := room(); r.RoomBytes != 0 {
		t.Fatalf("room = %+v, want none while a too-large job waits", r)
	}
	// Once it no longer waits the room is what the budget leaves.
	if _, err := f.w.Cancel(ctx, "big"); err != nil {
		t.Fatal(err)
	}
	if r := room(); r.RoomBytes != tooLargeBudget-held {
		t.Fatalf("room = %+v, want %d again once nothing too large waits", r, tooLargeBudget-held)
	}
}

func TestTwoTooLargeJobsRunInQueueOrder(t *testing.T) {
	ctx := context.Background()
	f := newTooLargeFixture(t, nil)
	f.submitFrom("held", f.source("small.mkv", smallSize), true)
	if err := f.w.InternalRun(ctx, "held"); err != nil {
		t.Fatal(err)
	}
	f.submitFrom("big-a", f.source("a.mkv", tooLargeSize), true)
	f.submitFrom("big-b", f.source("b.mkv", tooLargeSize+1), true)
	for _, id := range []string{"big-a", "big-b"} {
		if job := f.loadQueued(id); job.PID != 0 || job.RetentionBlocked != retentionBlockedAlone {
			t.Fatalf("%s = pid %d blocked %q; want waiting alone", id, job.PID, job.RetentionBlocked)
		}
	}
	if _, err := f.w.Cancel(ctx, "held"); err != nil {
		t.Fatal(err)
	}
	f.schedule()
	if job := f.loadQueued("big-a"); job.PID == 0 {
		t.Fatal("the first too-large job must start when the node is empty")
	}
	if job := f.loadQueued("big-b"); job.PID != 0 {
		t.Fatal("the second too-large job must not start with the first")
	}
}

func TestAStaleTooLargeMarkIsIgnoredAfterARestartWithABiggerBudget(t *testing.T) {
	ctx := context.Background()
	f := newTooLargeFixture(t, nil)
	f.submitFrom("held", f.source("small.mkv", smallSize), true)
	if err := f.w.InternalRun(ctx, "held"); err != nil {
		t.Fatal(err)
	}
	f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
	if job := f.loadQueued("big"); job.RetentionBlocked != retentionBlockedAlone {
		t.Fatalf("big blocked %q, want the persisted mark", job.RetentionBlocked)
	}
	// The worker restarts with a budget the job fits.
	f.cfg.RetainedCandidatesMaxBytes = 200
	f.w = f.newWorker()
	f.submitFrom("later", f.source("later.mkv", smallSize), true)
	if job := f.loadQueued("later"); job.PID == 0 {
		t.Fatal("a small job must not be held back by a stale too-large mark")
	}
	f.schedule()
	if job := f.loadQueued("big"); job.PID == 0 || job.RetentionBlocked != "" {
		t.Fatalf("big = pid %d blocked %q; want admitted normally under the bigger budget", job.PID, job.RetentionBlocked)
	}
}

// A record that cannot be read is no reason to gate the node or to make a batch
// child publish without approval. (Admission itself still fails closed on it,
// as it always did: the retained bytes cannot be measured, so the job stays
// queued; it starts once the record is gone.) A submit refuses to run past such
// a record, so the jobs are queued first (the queue held) and it is damaged after.
func TestACorruptJobRecordDoesNotFreezeTheNode(t *testing.T) {
	ctx := context.Background()
	corruptAndRelease := func(f *retainedFixture) {
		dir := filepath.Join(f.cfg.StateDir, "corrupt")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "job.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := f.w.SetHold(false); err != nil {
			t.Fatal(err)
		}
		// The pass reports the unreadable record but still works the queue.
		if _, err := f.w.ScheduleQueued(ctx, "test-exe", ""); err == nil {
			t.Fatal("the unreadable record must be reported")
		}
	}
	t.Run("nothing too large waits", func(t *testing.T) {
		f := newTooLargeFixture(t, nil)
		if err := f.w.SetHold(true); err != nil {
			t.Fatal(err)
		}
		f.submitFrom("small", f.source("small.mkv", smallSize), true)
		f.submitFrom("child", f.source("child.mkv", smallSize), false)
		corruptAndRelease(f)
		for _, id := range []string{"small", "child"} {
			if job := f.loadQueued(id); job.PID != 0 || job.PublishFallback != "" || job.RetentionBlocked != "" {
				t.Fatalf("%s = pid %d fallback %q blocked %q; want queued, untouched by the unreadable record", id, job.PID, job.PublishFallback, job.RetentionBlocked)
			}
		}
		if err := os.RemoveAll(filepath.Join(f.cfg.StateDir, "corrupt")); err != nil {
			t.Fatal(err)
		}
		f.schedule()
		if job := f.loadQueued("small"); job.PID == 0 || !job.deferredPublication() {
			t.Fatalf("small = pid %d deferred %v; want started normally", job.PID, job.deferredPublication())
		}
		if job := f.loadQueued("child"); job.PID == 0 || job.PublishFallback != "" || !job.deferredPublication() {
			t.Fatalf("child = pid %d fallback %q deferred %v; want started keeping its candidate", job.PID, job.PublishFallback, job.deferredPublication())
		}
	})
	t.Run("a readable too-large job still gates", func(t *testing.T) {
		f := newTooLargeFixture(t, nil)
		f.submitFrom("held", f.source("held.mkv", smallSize), true)
		if err := f.w.InternalRun(ctx, "held"); err != nil {
			t.Fatal(err)
		}
		if err := f.w.SetHold(true); err != nil {
			t.Fatal(err)
		}
		f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
		f.submitFrom("later", f.source("later.mkv", smallSize), true)
		f.submitFrom("child", f.source("child.mkv", smallSize), false)
		corruptAndRelease(f)
		if job := f.loadQueued("big"); job.PID != 0 {
			t.Fatalf("big = pid %d; want waiting", job.PID)
		}
		if job := f.loadQueued("later"); job.PID != 0 || job.RetentionBlocked != retentionBlockedBudget {
			t.Fatalf("later = pid %d blocked %q; want waiting behind the readable too-large job", job.PID, job.RetentionBlocked)
		}
		if job := f.loadQueued("child"); job.PID == 0 || job.PublishFallback != transcode.PublishFallbackRetentionFull {
			t.Fatalf("child = pid %d fallback %q; want it to publish on encode behind the gate", job.PID, job.PublishFallback)
		}
	})
}

func TestAFloorThatCanNeverBeMetFailsTheJobOnlyAfterItHeldForAWhile(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	setup := func(t *testing.T, free *int64) (*retainedFixture, *time.Time) {
		f := newTooLargeFixture(t, func(c *WorkerConfig) { c.MinFreeDiskBytes = 95 })
		now := start
		f.w.retentionNow = func() time.Time { return now }
		f.w.freeDisk = func(string) (int64, error) { return *free, nil }
		return f, &now
	}
	t.Run("persistent: fails after the grace with the specific message", func(t *testing.T) {
		free := int64(100)
		f, now := setup(t, &free)
		f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
		job := f.loadQueued("big")
		if job.Status != "queued" || job.RetentionBlocked != retentionBlockedDisk || !job.FloorUnmetSince.Equal(start) {
			t.Fatalf("big = %s blocked %q since %v; want queued on the floor, mark set", job.Status, job.RetentionBlocked, job.FloorUnmetSince)
		}
		for d := 4 * time.Minute; d < floorUnmetGrace; d += 4 * time.Minute {
			*now = start.Add(d)
			f.schedule()
			if job := f.loadQueued("big"); job.Status != "queued" {
				t.Fatalf("big = %s after %s, before the grace ran out", job.Status, d)
			}
		}
		*now = start.Add(floorUnmetGrace)
		f.schedule()
		job = f.loadQueued("big")
		if job.Status != "failed" || job.ReasonCode != "retention_floor_unmet" || job.PID != 0 {
			t.Fatalf("big = %s %q pid %d; want failed retention_floor_unmet", job.Status, job.ReasonCode, job.PID)
		}
		for _, want := range []string{"needs 50 bytes", "offers 5 bytes above its 95 byte free-disk minimum now"} {
			if !strings.Contains(job.Error, want) {
				t.Fatalf("error = %q, want it to say %q", job.Error, want)
			}
		}
		if st, err := f.w.Status(ctx, "big"); err != nil || st.Status != "failed" || st.Error != job.Error {
			t.Fatalf("status = %+v (%v), want the failure told as is", st, err)
		}
	})
	t.Run("transient: the disk frees up before the grace", func(t *testing.T) {
		free := int64(100)
		f, now := setup(t, &free)
		f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
		*now = start.Add(floorUnmetGrace - time.Minute)
		free = 1000
		f.schedule()
		if job := f.loadQueued("big"); job.PID == 0 || job.Status == "failed" || !job.FloorUnmetSince.IsZero() {
			t.Fatalf("big = %s pid %d since %v; want started with the mark cleared", job.Status, job.PID, job.FloorUnmetSince)
		}
	})
	t.Run("the grace restarts when the condition clears", func(t *testing.T) {
		free := int64(100)
		f, now := setup(t, &free)
		f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
		f.submit("busy", "") // another job running: the disk may still free up
		*now = start.Add(floorUnmetGrace - time.Minute)
		f.schedule()
		if job := f.loadQueued("big"); !job.FloorUnmetSince.IsZero() {
			t.Fatal("the mark must be cleared while another job runs")
		}
		if err := f.w.InternalRun(ctx, "busy"); err != nil {
			t.Fatal(err)
		}
		*now = start.Add(floorUnmetGrace + time.Minute)
		f.schedule()
		job := f.loadQueued("big")
		if job.Status != "queued" || !job.FloorUnmetSince.Equal(*now) {
			t.Fatalf("big = %s since %v; want waiting, the grace restarted", job.Status, job.FloorUnmetSince)
		}
	})
	t.Run("a young cache entry keeps it waiting for good", func(t *testing.T) {
		free := int64(100)
		f, now := setup(t, &free)
		cacheDir := f.w.sourceCacheDir()
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cacheDir, "young-entry"), []byte("c"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
		*now = start.Add(10 * floorUnmetGrace)
		f.schedule()
		if job := f.loadQueued("big"); job.Status != "queued" || !job.FloorUnmetSince.IsZero() {
			t.Fatalf("big = %s since %v; want waiting, never failing", job.Status, job.FloorUnmetSince)
		}
	})
	t.Run("another job running may still free the disk", func(t *testing.T) {
		free := int64(96) // 3 bytes of source would leave 93, under the floor
		f, now := setup(t, &free)
		f.submit("busy", "") // publishes on encode: not subject to the floor
		f.submitFrom("waits", f.source("w.mkv", smallSize), true)
		*now = start.Add(10 * floorUnmetGrace)
		f.schedule()
		if job := f.loadQueued("waits"); job.Status != "queued" || job.RetentionBlocked != retentionBlockedDisk {
			t.Fatalf("waits = %s blocked %q; want queued on the floor while another job runs", job.Status, job.RetentionBlocked)
		}
		free = 1000
		f.schedule()
		if job := f.loadQueued("waits"); job.PID == 0 {
			t.Fatal("the job must start once the disk has room")
		}
	})
	notFailedAfter := func(t *testing.T, f *retainedFixture, now *time.Time, at time.Duration) {
		t.Helper()
		*now = start.Add(at)
		f.schedule()
		job := f.loadQueued("big")
		if job.Status != "queued" || !job.FloorUnmetSince.Equal(*now) {
			t.Fatalf("big = %s since %v at %s; want waiting with the grace started over", job.Status, job.FloorUnmetSince, at)
		}
	}
	t.Run("a gate in between starts the grace over", func(t *testing.T) {
		free := int64(100)
		f, now := setup(t, &free)
		f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
		*now = start.Add(20 * time.Minute)
		job := f.loadQueued("big")
		jobFile := filepath.Join(f.cfg.StateDir, "big", "job.json")
		if ok, err := f.w.admitAtStartLocked(jobFile, job, "someone-ahead"); ok || err != nil {
			t.Fatalf("admitted = %v (%v) behind a gate", ok, err)
		}
		if job := f.loadQueued("big"); !job.FloorUnmetSince.IsZero() {
			t.Fatal("a pass held behind a gate must clear the mark")
		}
		notFailedAfter(t, f, now, floorUnmetGrace+time.Minute)
	})
	t.Run("a measurement error in between starts the grace over", func(t *testing.T) {
		free := int64(100)
		f, now := setup(t, &free)
		f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
		f.w.freeDisk = func(string) (int64, error) { return 0, errors.New("statfs failed") }
		*now = start.Add(20 * time.Minute)
		if _, err := f.w.ScheduleQueued(ctx, "test-exe", ""); err == nil {
			t.Fatal("an unknown free space must be reported")
		}
		if job := f.loadQueued("big"); !job.FloorUnmetSince.IsZero() {
			t.Fatal("an unmeasured pass must clear the mark")
		}
		f.w.freeDisk = func(string) (int64, error) { return free, nil }
		notFailedAfter(t, f, now, floorUnmetGrace+time.Minute)
	})
	t.Run("a restart with a persisted mark starts the grace over", func(t *testing.T) {
		free := int64(100)
		f, now := setup(t, &free)
		f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
		f.w = f.newWorker()
		f.w.retentionNow = func() time.Time { return *now }
		f.w.freeDisk = func(string) (int64, error) { return free, nil }
		notFailedAfter(t, f, now, floorUnmetGrace+time.Minute)
	})
	t.Run("it applies to any on_approval job, not only a too-large one", func(t *testing.T) {
		free := int64(100) // 11 bytes of source would leave 89, under the floor
		f, now := setup(t, &free)
		f.submit("small", "on_approval")
		for d := 4 * time.Minute; d < floorUnmetGrace; d += 4 * time.Minute {
			*now = start.Add(d)
			f.schedule()
		}
		if job := f.loadQueued("small"); job.Status != "queued" {
			t.Fatalf("small = %s before the grace ran out", job.Status)
		}
		*now = start.Add(floorUnmetGrace)
		f.schedule()
		if job := f.loadQueued("small"); job.Status != "failed" || job.ReasonCode != "retention_floor_unmet" {
			t.Fatalf("small = %s %q; want failed retention_floor_unmet", job.Status, job.ReasonCode)
		}
	})
	t.Run("a source cache that can give way satisfies it", func(t *testing.T) {
		free := int64(100)
		f, _ := setup(t, &free)
		cacheDir := f.w.sourceCacheDir()
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			t.Fatal(err)
		}
		entry := filepath.Join(cacheDir, "old-entry")
		if err := os.WriteFile(entry, []byte(strings.Repeat("c", 200)), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(entry, old, old); err != nil {
			t.Fatal(err)
		}
		f.w.freeDisk = func(string) (int64, error) {
			if _, err := os.Stat(entry); err == nil {
				return 100, nil
			}
			return 1000, nil
		}
		f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
		job := f.loadQueued("big")
		if job.PID == 0 || job.Status == "failed" {
			t.Fatalf("big = %s pid %d (%s); want started after the cache gave way", job.Status, job.PID, job.Error)
		}
		if _, err := os.Stat(entry); !os.IsNotExist(err) {
			t.Fatalf("the cache entry must have been dropped: %v", err)
		}
	})
}
