package transcodeworker

import (
	"context"
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
	// Once it has started it is no longer waiting, and the budget is what it is.
	if _, err := f.w.Cancel(ctx, "held"); err != nil {
		t.Fatal(err)
	}
	f.schedule()
	if r := room(); r.RoomBytes != 0 {
		t.Fatalf("room = %+v, want none while the too-large job holds the node", r)
	}
}

func TestAFloorThatCanNeverBeMetFailsTheJobPromptly(t *testing.T) {
	ctx := context.Background()
	t.Run("empty node, nothing to free", func(t *testing.T) {
		f := newTooLargeFixture(t, func(c *WorkerConfig) { c.MinFreeDiskBytes = 95 })
		f.w.freeDisk = func(string) (int64, error) { return 100, nil }
		f.submitFrom("big", f.source("big.mkv", tooLargeSize), true)
		job := f.loadQueued("big")
		if job.Status != "failed" || job.ReasonCode != "retention_floor_unmet" || job.PID != 0 {
			t.Fatalf("big = %s %q pid %d; want failed retention_floor_unmet", job.Status, job.ReasonCode, job.PID)
		}
		for _, want := range []string{"needs 50 bytes", "at most 5 bytes above its 95 byte free-disk minimum"} {
			if !strings.Contains(job.Error, want) {
				t.Fatalf("error = %q, want it to say %q", job.Error, want)
			}
		}
		if st, err := f.w.Status(ctx, "big"); err != nil || st.Status != "failed" || st.Error != job.Error {
			t.Fatalf("status = %+v (%v), want the failure told as is", st, err)
		}
	})
	t.Run("another job running may still free the disk", func(t *testing.T) {
		f := newTooLargeFixture(t, func(c *WorkerConfig) { c.MinFreeDiskBytes = 95 })
		free := int64(96) // 3 bytes of source would leave 93, under the floor
		f.w.freeDisk = func(string) (int64, error) { return free, nil }
		f.submit("busy", "") // publishes on encode: not subject to the floor
		f.submitFrom("waits", f.source("w.mkv", smallSize), true)
		if job := f.loadQueued("waits"); job.Status != "queued" || job.RetentionBlocked != retentionBlockedDisk {
			t.Fatalf("waits = %s blocked %q; want queued on the floor while another job runs", job.Status, job.RetentionBlocked)
		}
		free = 1000
		f.schedule()
		if job := f.loadQueued("waits"); job.PID == 0 {
			t.Fatal("the job must start once the disk has room")
		}
	})
	t.Run("a source cache that can give way satisfies it", func(t *testing.T) {
		f := newTooLargeFixture(t, func(c *WorkerConfig) { c.MinFreeDiskBytes = 95 })
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
