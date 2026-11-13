package store

import (
	"errors"
	"testing"
	"time"
)

func TestBuildStoreLifecycle(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	b, err := s.CreateBuild(Build{Target: "m1", Source: "ci", Title: "run", PreemptMode: WorkerPauseSuspend}, now, 10*time.Minute, false)
	if err != nil || b.Status != BuildRunning || b.Priority != BuildPriorityNormal || b.StartedAt != b.CreatedAt || b.ExpiresAt != "2026-11-13T10:10:00Z" {
		t.Fatalf("%+v %v", b, err)
	}
	w, err := s.CreateBuild(Build{Target: "m1", Source: "ci", Title: "warm", PreemptMode: WorkerPauseSuspend, Priority: BuildPriorityLow}, now.Add(time.Second), time.Minute, true)
	if err != nil || w.Status != BuildWaiting || w.StartedAt != "" {
		t.Fatalf("%+v %v", w, err)
	}
	// A waiting build holds no running slot and is promoted exactly once.
	if running, _ := s.RunningBuilds(); len(running) != 1 || running[0].ID != b.ID {
		t.Fatalf("%+v", running)
	}
	if ok, err := s.PromoteBuild(w.ID, now.Add(2*time.Second)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := s.PromoteBuild(w.ID, now.Add(3*time.Second)); ok {
		t.Fatal("a running build is not promoted again")
	}
	if got, _ := s.GetBuild(w.ID); got.Status != BuildRunning || got.StartedAt != "2026-11-13T10:00:02Z" {
		t.Fatalf("%+v", got)
	}
	if _, err := s.GetBuild("nope"); !errors.Is(err, ErrBuildNotFound) {
		t.Fatal(err)
	}

	// The shorter ttl expires first; the other keeps running.
	lost, err := s.ExpireBuilds(now.Add(2 * time.Minute))
	if err != nil || len(lost) != 1 || lost[0].ID != w.ID {
		t.Fatalf("%+v %v", lost, err)
	}
	if got, _ := s.GetBuild(w.ID); got.Status != BuildLost || got.EndedAt == "" {
		t.Fatalf("%+v", got)
	}
	if _, err := s.HeartbeatBuild(w.ID, now, time.Minute, nil); !errors.Is(err, ErrBuildEnded) {
		t.Fatal(err)
	}
	code := 7
	done, changed, err := s.FinishBuild(b.ID, BuildFailed, &code, now.Add(3*time.Minute))
	if err != nil || !changed || done.ExitCode == nil || *done.ExitCode != 7 {
		t.Fatalf("%+v %v %v", done, changed, err)
	}

	// Only ended builds older than the cutoff are pruned.
	if n, err := s.PruneBuilds(now.Add(150 * time.Second)); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if n, err := s.PruneBuilds(now.Add(time.Hour)); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if list, _ := s.ListBuilds(BuildFilter{}); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
}
