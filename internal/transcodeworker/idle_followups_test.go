package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Follow-ups to the idle-cost work: the log cap while the worker is silent,
// the capacity lock's span in a prune pass, and the bounded record index.

func TestCappedLogCheckRotatesWhileTheWorkerIsSilent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := NewCappedLog(f, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Another process writes to the same file; the worker logs nothing.
	other, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.WriteString(strings.Repeat("y", 500)); err != nil {
		t.Fatal(err)
	}
	c.Check() // first call: statAt is zero, so it looks
	if fi, _ := os.Stat(path); fi.Size() != 0 {
		t.Fatalf("an oversized log must be cut by Check alone, size %d", fi.Size())
	}
	if old, err := os.ReadFile(path + ".1"); err != nil || len(old) != 100 {
		t.Fatalf("the newest 100 bytes must be kept: len=%d err=%v", len(old), err)
	}
	// Within the recheck interval Check does not even stat.
	if _, err := other.WriteString(strings.Repeat("z", 500)); err != nil {
		t.Fatal(err)
	}
	c.Check()
	if fi, _ := os.Stat(path); fi.Size() != 500 {
		t.Fatalf("Check must be throttled, size %d", fi.Size())
	}
	c.mu.Lock()
	c.statAt = time.Now().Add(-2 * logRecheckEvery)
	c.mu.Unlock()
	c.Check()
	if fi, _ := os.Stat(path); fi.Size() != 0 {
		t.Fatalf("size %d after the interval", fi.Size())
	}
}

func TestSchedulerTickChecksTheStderrCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := NewCappedLog(f, 100)
	if err != nil {
		t.Fatal(err)
	}
	stderrCap.Store(c)
	defer stderrCap.Store(nil)
	if _, err := f.WriteString(strings.Repeat("y", 500)); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(t.TempDir(), "jobs")})
	w.schedulerSweepHook = func(string) error { return nil }
	w.runSchedulerSweeps(context.Background(), "exe", "")
	if fi, _ := os.Stat(path); fi.Size() != 0 {
		t.Fatalf("a scheduler pass must enforce the cap, size %d", fi.Size())
	}
}

func TestPruneReleasesTheCapacityLockBetweenCandidates(t *testing.T) {
	resetJobIndexes()
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(t.TempDir(), "jobs")})
	w.isAlive = func(job *JobRecord) bool { return strings.HasPrefix(job.ID, "a-live") }
	old := 60 * 24 * time.Hour
	const live = 300
	for i := 0; i < live; i++ {
		seedFinished(t, w, &JobRecord{ID: fmt.Sprintf("a-live-%04d", i), Status: "completed", PID: 9}, old)
	}
	for i := 0; i < 5; i++ {
		seedFinished(t, w, &JobRecord{ID: fmt.Sprintf("z-done-%d", i), Status: "completed"}, old)
	}
	// With the lock held across the pass this probe would fail from the second
	// candidate on: flock locks belong to the open file, so a second descriptor
	// in this process conflicts like another process would.
	seen, blocked := 0, 0
	pruneCandidateHook = func(string) {
		seen++
		f, err := os.OpenFile(filepath.Join(w.cfg.StateDir, ".capacity.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			blocked++
			return
		}
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	defer func() { pruneCandidateHook = nil }()
	n, err := w.PruneJobs(context.Background(), time.Now())
	if err != nil || n != 5 {
		t.Fatalf("emptied %d (err %v), want the 5 behind %d live ones", n, err, live)
	}
	if seen != live+5 || blocked != 0 {
		t.Fatalf("examined %d, capacity lock held at %d candidate boundaries", seen, blocked)
	}
}

func TestJobIndexStaysBoundedWithoutThrashing(t *testing.T) {
	resetJobIndexes()
	prev := jobCacheMaxEntries
	jobCacheMaxEntries = 64
	defer func() { jobCacheMaxEntries = prev }()
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(t.TempDir(), "jobs")})
	const total = 90 // more records than the index may hold
	paths := make([]string, total)
	for i := range paths {
		paths[i] = seedAged(t, w, &JobRecord{ID: fmt.Sprintf("job-%03d", i), Status: "completed"}, 72*time.Hour)
	}
	scan := func() []*JobRecord {
		out := make([]*JobRecord, total)
		for i, p := range paths {
			j, err := peekJob(p)
			if err != nil {
				t.Fatal(err)
			}
			out[i] = j
		}
		return out
	}
	scan()
	for round := 0; round < 3; round++ {
		before, after := scan(), scan()
		reused := 0
		for i := range before {
			if before[i] == after[i] { // the same pointer: served from the index, not decoded
				reused++
			}
		}
		jobIndex.Lock()
		size := len(jobIndex.m)
		jobIndex.Unlock()
		if size > jobCacheMaxEntries {
			t.Fatalf("index holds %d, bound %d", size, jobCacheMaxEntries)
		}
		// A full reset on overflow would leave about total-cap (26) hits.
		if reused < jobCacheMaxEntries/2+10 {
			t.Fatalf("round %d: only %d of %d records served from the index", round, reused, total)
		}
	}
}

func TestFullIndexStillAdmitsNewerRecords(t *testing.T) {
	resetJobIndexes()
	prev := jobCacheMaxEntries
	jobCacheMaxEntries = 16
	defer func() { jobCacheMaxEntries = prev }()
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(t.TempDir(), "jobs")})
	for i := 0; i < 16; i++ {
		p := seedAged(t, w, &JobRecord{ID: fmt.Sprintf("old-%02d", i), Status: "completed"}, 90*24*time.Hour)
		if _, err := peekJob(p); err != nil {
			t.Fatal(err)
		}
	}
	fresh := seedAged(t, w, &JobRecord{ID: "fresh", Status: "completed"}, time.Hour)
	if _, err := peekJob(fresh); err != nil {
		t.Fatal(err)
	}
	if cachedEntry(fresh) == nil {
		t.Fatal("a recent record must displace an old one in a full index")
	}
	jobIndex.Lock()
	size := len(jobIndex.m)
	jobIndex.Unlock()
	if size != 16 {
		t.Fatalf("index holds %d, want 16", size)
	}
}

func TestRacyListingStillForgetsDepartedRecords(t *testing.T) {
	resetJobIndexes()
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(t.TempDir(), "jobs")})
	keep := seedAged(t, w, &JobRecord{ID: "keep", Status: "completed"}, 72*time.Hour)
	gone := seedAged(t, w, &JobRecord{ID: "gone", Status: "completed"}, 72*time.Hour)
	ageFile(t, w.cfg.StateDir, 72*time.Hour)
	if _, err := w.readJobDirs(); err != nil { // settled listing, now indexed
		t.Fatal(err)
	}
	for _, p := range []string{keep, gone} {
		if _, err := peekJob(p); err != nil {
			t.Fatal(err)
		}
	}
	if cachedEntry(gone) == nil || cachedEntry(keep) == nil {
		t.Fatal("both records must be cached")
	}
	// The job directory goes away; the state dir's mtime is now (racy).
	if err := os.RemoveAll(filepath.Dir(gone)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.readJobDirs(); err != nil {
		t.Fatal(err)
	}
	if cachedEntry(gone) != nil {
		t.Fatal("a departed job must leave the record index even when the listing is too fresh to keep")
	}
	if cachedEntry(keep) == nil {
		t.Fatal("a job still listed must stay cached")
	}
}
