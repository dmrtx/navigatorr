package transcodeworker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

// The podcast lane: a build lease holds the conversion (video) lane only. A
// podcast job waits for an operator pause and for nothing else.

func laneJobPID(t *testing.T, w *Worker, id string) int {
	t.Helper()
	job, err := LoadJob(filepath.Join(w.cfg.StateDir, id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	return job.PID
}

// C1 and C3: under a running lease, in each mode, a queued podcast starts and a
// queued video job does not (and says why).
func TestPodcastStartsUnderEveryBuildLeaseModeVideoDoesNot(t *testing.T) {
	for _, mode := range []string{"graceful", "suspend", "immediate"} {
		t.Run(mode, func(t *testing.T) {
			withHostCPUs(t, 10)
			stub := newStubSpawn()
			w, _, _ := newLeaseWorker(t, 4, stub)
			if _, err := w.LeaseCreate(LeaseRequest{Source: "tester", Title: "compile", PreemptMode: mode, TTLSeconds: 60}); err != nil {
				t.Fatal(err)
			}
			if !w.leaseHolds() {
				t.Fatal("the lease must run")
			}
			seedSlotJob(t, w, stub, "queued-video", "queued", slotKindVideo, false, 3*time.Minute)
			seedSlotJob(t, w, stub, "queued-podcast", "queued", slotKindPodcast, false, 2*time.Minute)
			started, err := w.ScheduleQueued(context.Background(), "exe", "")
			if err != nil || started != 1 {
				t.Fatalf("started %d, err %v; want only the podcast job", started, err)
			}
			if laneJobPID(t, w, "queued-podcast") == 0 {
				t.Fatal("the podcast job must start under a build")
			}
			if laneJobPID(t, w, "queued-video") != 0 {
				t.Fatal("the video job must wait for the build")
			}
			video, _ := LoadJob(filepath.Join(w.cfg.StateDir, "queued-video", "job.json"))
			if video.RetentionBlocked == "" {
				t.Fatalf("the held video job must say why: %+v", video)
			}
			podcast, _ := LoadJob(filepath.Join(w.cfg.StateDir, "queued-podcast", "job.json"))
			if podcast.RetentionBlocked != "" || podcast.Status == "failed" {
				t.Fatalf("the podcast job must not be refused for a build: %+v", podcast)
			}
		})
	}
}

// C2: a freezing lease leaves a running podcast alone: it is not an encoder the
// lease waits for, it keeps running, and the video encoder it does wait for is
// the one that gets frozen.
func TestFreezingLeaseLeavesARunningPodcastRunning(t *testing.T) {
	old := leaseFreezeWait
	leaseFreezeWait = 150 * time.Millisecond
	defer func() { leaseFreezeWait = old }()
	for _, mode := range []string{"suspend", "immediate"} {
		t.Run(mode, func(t *testing.T) {
			withHostCPUs(t, 10)
			stub := newStubSpawn()
			w, _, _ := newLeaseWorker(t, 4, stub)
			seedSlotJob(t, w, stub, "run-podcast", "running", slotKindPodcast, true, time.Minute)
			podPID := laneJobPID(t, w, "run-podcast")
			// A podcast alone is no encoder to freeze: the lease runs at once.
			l, err := w.LeaseCreate(LeaseRequest{Source: "tester", Title: "compile", PreemptMode: mode, TTLSeconds: 60})
			if err != nil || l.Status != leaseStatusRunning {
				t.Fatalf("lease: %+v %v", l, err)
			}
			if on, _ := w.freezeState(); !on {
				t.Fatal("a normal lease freezes the encoders")
			}
			active, frozen, err := w.suspendCounts()
			if err != nil || active != 0 || frozen != 0 {
				t.Fatalf("the podcast job must not count as an encoder: active %d frozen %d (%v)", active, frozen, err)
			}
			job, _ := LoadJob(filepath.Join(w.cfg.StateDir, "run-podcast", "job.json"))
			if job.Status != "running" || job.PID != podPID || job.Suspended {
				t.Fatalf("the podcast job must be left running: %+v", job)
			}
			// A running video encoder, in contrast, is what the lease counts.
			seedSlotJob(t, w, stub, "run-video", "running", slotKindVideo, true, time.Minute)
			active, frozen, _ = w.suspendCounts()
			if active != 1 || frozen != 0 {
				t.Fatalf("the video encoder is the one to freeze: active %d frozen %d", active, frozen)
			}
			if _, err := w.LeaseFinish(l.ID, leaseStatusSucceeded, ptr(0)); err != nil {
				t.Fatal(err)
			}
			// While the video encoder never reports frozen, a second lease is
			// refused; the running podcast is not what it waited on.
			if _, err := w.LeaseCreate(LeaseRequest{Source: "tester", Title: "again", PreemptMode: mode, TTLSeconds: 60}); err == nil {
				t.Fatal("the unfrozen video encoder must refuse the lease")
			}
			if laneJobPID(t, w, "run-podcast") != podPID {
				t.Fatal("the podcast job must still be running")
			}
		})
	}
}

// The hold refusal and the queue hold are per lane.
func TestHoldRefusalIsPerLane(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	if _, err := w.LeaseCreate(leaseReq("compile")); err != nil {
		t.Fatal(err)
	}
	if reason, _ := w.holdRefusalFor(false); reason == "" {
		t.Fatal("a build refuses conversions")
	}
	if reason, msg := w.holdRefusalFor(true); reason != "" || msg != "" {
		t.Fatalf("a build must not refuse a podcast: %q %q", reason, msg)
	}
	if !w.queueHeldFor(false) || w.queueHeldFor(true) {
		t.Fatal("the build holds the conversion lane only")
	}
}

// An operator hold or suspend still keeps a podcast from starting, with or
// without a build.
func TestOperatorPauseStillStopsPodcasts(t *testing.T) {
	for name, pause := range map[string]func(*Worker) error{
		"hold":    func(w *Worker) error { return w.SetHold(true) },
		"suspend": func(w *Worker) error { return w.SetSuspend(true) },
	} {
		for _, withLease := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				withHostCPUs(t, 10)
				stub := newStubSpawn()
				w, _, _ := newLeaseWorker(t, 4, stub)
				if withLease {
					if _, err := w.LeaseCreate(leaseReq("compile")); err != nil {
						t.Fatal(err)
					}
				}
				seedSlotJob(t, w, stub, "queued-podcast", "queued", slotKindPodcast, false, time.Minute)
				seedSlotJob(t, w, stub, "queued-video", "queued", slotKindVideo, false, time.Minute)
				if err := pause(w); err != nil {
					t.Fatal(err)
				}
				if started, err := w.ScheduleQueued(context.Background(), "exe", ""); err != nil || started != 0 {
					t.Fatalf("started %d (%v) under an operator pause", started, err)
				}
				if laneJobPID(t, w, "queued-podcast") != 0 || laneJobPID(t, w, "queued-video") != 0 {
					t.Fatal("nothing may start")
				}
				if reason, _ := w.holdRefusalFor(true); reason == "" {
					t.Fatal("a paused node refuses podcasts too")
				}
			})
		}
	}
}

// Post-encode recovery is per lane too: under a running lease a podcast's
// finished encode goes on to be saved, a conversion's waits for the build.
func TestResumePostEncodeUnderALeaseFinishesPodcastsOnly(t *testing.T) {
	for _, podcastJob := range []bool{true, false} {
		w, stub, cfg := postEncodeWorker(t)
		if podcastJob {
			jobFile := filepath.Join(cfg.StateDir, "job1", "job.json")
			job, err := LoadJob(jobFile)
			if err != nil {
				t.Fatal(err)
			}
			job.Plan = &transcode.Plan{Podcast: &podcast.Task{}}
			if err := SaveJobAtomic(jobFile, job); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := w.LeaseCreate(LeaseRequest{Source: "tester", Title: "compile", PreemptMode: "graceful", TTLSeconds: 60}); err != nil {
			t.Fatal(err)
		}
		n, err := w.ResumePostEncode(context.Background(), "self", "")
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if podcastJob {
			want = 1
		}
		if n != want || stub.n() != want {
			t.Fatalf("podcast=%v: started %d (spawns %d), want %d", podcastJob, n, stub.n(), want)
		}
	}
}

// A podcast that stays queued under a build (its own slots are full) is not
// marked as held by the build; the queued conversion is.
func TestQueuedPodcastUnderALeaseIsNotMarkedHeld(t *testing.T) {
	withHostCPUs(t, 10)
	stub := newStubSpawn()
	w, _, _ := newLeaseWorker(t, 4, stub)
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 3, PodcastSlots: 1}); err != nil {
		t.Fatal(err)
	}
	seedSlotJob(t, w, stub, "run-podcast", "running", slotKindPodcast, true, 3*time.Minute)
	if _, err := w.LeaseCreate(leaseReq("compile")); err != nil {
		t.Fatal(err)
	}
	seedSlotJob(t, w, stub, "queued-podcast", "queued", slotKindPodcast, false, 2*time.Minute)
	seedSlotJob(t, w, stub, "queued-video", "queued", slotKindVideo, false, time.Minute)
	if started, err := w.ScheduleQueued(context.Background(), "exe", ""); err != nil || started != 0 {
		t.Fatalf("started %d (%v); the podcast slot is full and the build holds video", started, err)
	}
	podcastJob, _ := LoadJob(filepath.Join(w.cfg.StateDir, "queued-podcast", "job.json"))
	if podcastJob.RetentionBlocked != "" {
		t.Fatalf("a queued podcast must not be marked as held by a build: %+v", podcastJob)
	}
	video, _ := LoadJob(filepath.Join(w.cfg.StateDir, "queued-video", "job.json"))
	if video.RetentionBlocked == "" {
		t.Fatalf("the queued conversion must say the build holds it: %+v", video)
	}
}

// A low-priority lease, once running, holds conversions and never podcasts.
func TestLowLeaseRunningHoldsConversionsOnly(t *testing.T) {
	withHostCPUs(t, 10)
	stub := newStubSpawn()
	w, _, _ := newLeaseWorker(t, 4, stub)
	if _, err := w.LeaseCreate(LeaseRequest{Source: "tester", Title: "compile", Priority: "low", TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	// The worker's tick promotes the waiting low lease on an idle node.
	w.leaseTick()
	if !w.leaseHolds() {
		t.Fatal("the low lease must be running")
	}
	if on, _ := w.freezeState(); on {
		t.Fatal("a low lease never freezes")
	}
	seedSlotJob(t, w, stub, "queued-video", "queued", slotKindVideo, false, 2*time.Minute)
	seedSlotJob(t, w, stub, "queued-podcast", "queued", slotKindPodcast, false, time.Minute)
	if started, err := w.ScheduleQueued(context.Background(), "exe", ""); err != nil || started != 1 {
		t.Fatalf("started %d (%v); want only the podcast", started, err)
	}
	if laneJobPID(t, w, "queued-podcast") == 0 || laneJobPID(t, w, "queued-video") != 0 {
		t.Fatal("the low lease must hold the conversion and let the podcast run")
	}
}
