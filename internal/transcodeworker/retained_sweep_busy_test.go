package transcodeworker

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// A publication holds its job's lock for as long as it takes to hash the
// candidate. The retained sweep rides on the scheduler's post-encode sweep,
// which runs before the queue's: it must pass over a job someone is working on
// instead of waiting for the lock, or every queued job waits for the hash too.
func TestRetainedSweepDoesNotWaitBehindAPublicationVerifyingItsCandidate(t *testing.T) {
	ctx := context.Background()
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedTTLHours = 1 })
	f.retain("job-verifying-1")

	entered := make(chan struct{})
	release := make(chan struct{})
	var once, unblock sync.Once
	prevHook := onLocalHashRead
	onLocalHashRead = func(string) {
		once.Do(func() { close(entered) })
		<-release
	}
	t.Cleanup(func() {
		unblock.Do(func() { close(release) })
		onLocalHashRead = prevHook
	})

	published := make(chan error, 1)
	go func() {
		_, err := f.w.PublishRetained(ctx, "job-verifying-1")
		published <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the publication never started hashing the candidate")
	}

	// The publication is now inside the hash with the job locked.
	swept := make(chan error, 1)
	go func() { swept <- f.w.sweepRetained(ctx) }()
	select {
	case err := <-swept:
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep waited behind a publication that is verifying its candidate")
	}

	unblock.Do(func() { close(release) })
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("PublishRetained: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the publication never finished")
	}
	if job := pr6b1LoadJob(t, f.cfg.StateDir, "job-verifying-1"); job.FinalizationState != string(FinalizationStateFinalizing) {
		t.Fatalf("job = %+v, want the publication recorded", job)
	}
}

// A job the sweep passed over because it was busy is looked at again once it
// is free: here an expired candidate, which the sweep must still collect.
func TestRetainedSweepRevisitsAJobItSkippedWhileBusy(t *testing.T) {
	ctx := context.Background()
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedTTLHours = 1 })
	f.retain("job-skipped-1")
	f.rewriteHidden("job-skipped-1", func(j *JobRecord) { j.RetainedAt = time.Now().Add(-48 * time.Hour) })
	local := f.loadHidden("job-skipped-1").LocalCandidatePath

	lock, err := acquireExistingJobLock(f.hiddenDir("job-skipped-1"))
	if err != nil {
		t.Fatal(err)
	}
	swept := make(chan error, 1)
	go func() { swept <- f.w.sweepRetained(ctx) }()
	select {
	case err := <-swept:
		if err != nil {
			lock.Unlock()
			t.Fatalf("a busy job is not a sweep failure: %v", err)
		}
	case <-time.After(5 * time.Second):
		lock.Unlock()
		<-swept
		t.Fatal("the sweep waited for a job that someone else holds")
	}
	if _, err := os.Stat(local); err != nil {
		lock.Unlock()
		t.Fatalf("a job the sweep could not lock must be left alone: %v", err)
	}
	lock.Unlock()

	if err := f.w.sweepRetained(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("the expired candidate must be collected once the job is free: %v", err)
	}
}
