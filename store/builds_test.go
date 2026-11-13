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

func mustBuild(t *testing.T, s *Store, b Build, now time.Time, ttl time.Duration, wait bool) Build {
	t.Helper()
	got, err := s.CreateBuild(b, now, ttl, wait)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPromoteBuildNeverStartsAnExpiredBuild(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	w := mustBuild(t, s, Build{Target: "m1", Source: "ci", Title: "warm", PreemptMode: WorkerPauseGraceful, Priority: BuildPriorityLow}, now, 30*time.Second, true)
	if ok, err := s.PromoteBuild(w.ID, now.Add(30*time.Second)); ok || err != nil {
		t.Fatalf("a build whose expiry has passed is not promoted: %v %v", ok, err)
	}
	if got, _ := s.GetBuild(w.ID); got.Status != BuildWaiting {
		t.Fatalf("%+v", got)
	}
	if ok, _ := s.PromoteBuild(w.ID, now.Add(29*time.Second)); !ok {
		t.Fatal("inside its window it is promoted")
	}
}

func TestListBuildsKeepsActiveBuildsPastTheLimit(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	running := mustBuild(t, s, Build{Target: "m1", Source: "ci", Title: "old", PreemptMode: WorkerPauseSuspend}, now, time.Hour, false)
	waiting := mustBuild(t, s, Build{Target: "m1", Source: "ci", Title: "old wait", PreemptMode: WorkerPauseGraceful, Priority: BuildPriorityLow}, now, time.Hour, true)
	for i := 1; i <= 120; i++ {
		b := mustBuild(t, s, Build{Target: "h", Source: "ci", Title: "done", PreemptMode: WorkerPauseSuspend}, now.Add(time.Duration(i)*time.Minute), time.Hour, false)
		if _, _, err := s.FinishBuild(b.ID, BuildSucceeded, nil, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// The limit applies to the history: both active builds always come first.
	for _, limit := range []int{0, 100, 5, 1} {
		list, err := s.ListBuilds(BuildFilter{Limit: limit})
		want := limit
		if want == 0 {
			want = 100
		}
		if err != nil || len(list) != 2+want {
			t.Fatalf("limit %d: %d builds %v", limit, len(list), err)
		}
		// Newest first inside the active group: the waiting one was created second.
		if list[0].ID != waiting.ID || list[1].ID != running.ID {
			t.Fatalf("limit %d: active builds come first: %+v %+v", limit, list[0], list[1])
		}
		if list[2].Title != "done" || list[2].CreatedAt != buildTime(now.Add(120*time.Minute)) {
			t.Fatalf("limit %d: then the history newest first: %+v", limit, list[2])
		}
	}
	// More active builds than the limit: none is dropped.
	for i := 0; i < 3; i++ {
		mustBuild(t, s, Build{Target: "x", Source: "ci", Title: "more", PreemptMode: WorkerPauseSuspend}, now.Add(time.Duration(200+i)*time.Minute), 24*time.Hour, false)
	}
	list, err := s.ListBuilds(BuildFilter{Limit: 2})
	if err != nil || len(list) != 5+2 {
		t.Fatalf("5 active builds past a limit of 2 plus 2 ended: %d %v", len(list), err)
	}
	if l, _ := s.ListBuilds(BuildFilter{Limit: -1}); len(l) != 5+120 {
		t.Fatalf("no limit: %d", len(l))
	}
}

func TestLateHeartbeatMarksTheBuildLostInTheStore(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	for _, wait := range []bool{false, true} {
		b := mustBuild(t, s, Build{Target: "m1", Source: "ci", Title: "x", PreemptMode: WorkerPauseGraceful, Priority: BuildPriorityLow}, now, time.Minute, wait)
		got, err := s.HeartbeatBuild(b.ID, now.Add(2*time.Minute), time.Hour, nil)
		if !errors.Is(err, ErrBuildExpired) || !errors.Is(err, ErrBuildEnded) || got.Status != BuildLost || got.EndedAt != b.ExpiresAt {
			t.Fatalf("wait=%v: %+v %v", wait, got, err)
		}
		stored, _ := s.GetBuild(b.ID)
		if stored.Status != BuildLost || stored.ExpiresAt != b.ExpiresAt || stored.EndedAt != b.ExpiresAt {
			t.Fatalf("wait=%v: not extended: %+v", wait, stored)
		}
		if _, err := s.HeartbeatBuild(b.ID, now.Add(2*time.Minute), time.Hour, nil); !errors.Is(err, ErrBuildEnded) || errors.Is(err, ErrBuildExpired) {
			t.Fatalf("a second heartbeat is the plain ended error: %v", err)
		}
	}
}

func TestBuildsCreatedInTheSameSecondKeepCreationOrder(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 12; i++ {
		b := mustBuild(t, s, Build{Target: "m1", Source: "ci", Title: "t", PreemptMode: WorkerPauseGraceful, Priority: BuildPriorityLow}, now, time.Hour, true)
		ids = append(ids, b.ID)
	}
	for name, read := range map[string]func() ([]Build, error){"waiting": s.WaitingBuilds, "active": s.ActiveBuilds} {
		list, err := read()
		if err != nil || len(list) != len(ids) {
			t.Fatalf("%s: %v %v", name, len(list), err)
		}
		for i := range ids {
			if list[i].ID != ids[i] {
				t.Fatalf("%s: position %d is not creation order", name, i)
			}
		}
	}
	for i := range ids {
		if ok, _ := s.PromoteBuild(ids[i], now); !ok {
			t.Fatal("promote")
		}
	}
	running, _ := s.RunningBuilds()
	for i := range ids {
		if running[i].ID != ids[i] {
			t.Fatalf("running: position %d is not creation order", i)
		}
	}
	// ListBuilds is the reverse inside the active group.
	newest, _ := s.ListBuilds(BuildFilter{})
	if newest[0].ID != ids[len(ids)-1] {
		t.Fatal("newest first")
	}
}

func TestHeartbeatWithoutTTLKeepsTheOwnWindowWithoutInflatingIt(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	b := mustBuild(t, s, Build{Target: "m1", Source: "ci", Title: "t", PreemptMode: WorkerPauseSuspend}, now, 30*time.Second, false)
	hb, err := s.HeartbeatBuild(b.ID, now.Add(10*time.Second), 0, nil)
	if err != nil || hb.ExpiresAt != buildTime(now.Add(40*time.Second)) {
		t.Fatalf("a 30 s build keeps 30 s: %+v %v", hb, err)
	}
	hb, _ = s.HeartbeatBuild(b.ID, now.Add(20*time.Second), 0, nil)
	if hb.ExpiresAt != buildTime(now.Add(50*time.Second)) {
		t.Fatalf("and it does not drift: %+v", hb)
	}
}
