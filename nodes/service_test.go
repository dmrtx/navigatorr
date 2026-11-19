package nodes

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestBuildLifecycleCreateHeartbeatFinish(t *testing.T) {
	e, clock := buildSvc(t)
	b := startBuild(t, e, "nodeA", "", 0)
	if b.ID == "" || b.Status != BuildRunning || b.PreemptMode != PreemptSuspend || b.HoldsTranscodeNode {
		t.Fatalf("create: %+v", b)
	}
	if want := clock.now().Add(BuildDefaultTTL).Format(time.RFC3339); b.ExpiresAt != want {
		t.Fatalf("default ttl: expires %s want %s", b.ExpiresAt, want)
	}

	clock.advance(5 * time.Minute)
	progress := "step 2 of 5"
	hb, err := e.HeartbeatBuild(context.Background(), b.ID, &progress, 0)
	if err != nil || hb.Progress != progress || hb.ExpiresAt != clock.now().Add(BuildDefaultTTL).Format(time.RFC3339) {
		t.Fatalf("heartbeat: %+v %v", hb, err)
	}
	// A heartbeat with a ttl of its own, capped at two hours.
	hb, err = e.HeartbeatBuild(context.Background(), b.ID, nil, 99999)
	if err != nil || hb.ExpiresAt != clock.now().Add(BuildMaxTTL).Format(time.RFC3339) || hb.Progress != progress {
		t.Fatalf("ttl cap / progress kept: %+v %v", hb, err)
	}
	// Without a ttl the window the build already has is kept.
	clock.advance(time.Minute)
	hb, _ = e.HeartbeatBuild(context.Background(), b.ID, nil, 0)
	if hb.ExpiresAt != clock.now().Add(BuildMaxTTL).Format(time.RFC3339) {
		t.Fatalf("a heartbeat without ttl keeps the window: %+v", hb)
	}

	exit := 3
	done, changed, err := e.FinishBuild(context.Background(), b.ID, BuildFailed, &exit)
	if err != nil || !changed || done.Status != BuildFailed || done.EndedAt == "" || done.ExitCode == nil || *done.ExitCode != 3 {
		t.Fatalf("finish: %+v %v %v", done, changed, err)
	}
	again, changed, err := e.FinishBuild(context.Background(), b.ID, BuildSucceeded, nil)
	if err != nil || changed || again.Status != BuildFailed {
		t.Fatalf("finishing an ended build changes nothing: %+v %v %v", again, changed, err)
	}
	if got, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0); !errors.Is(err, ErrBuildEnded) || got.Status != BuildFailed {
		t.Fatalf("a heartbeat after the end is refused: %+v %v", got, err)
	}
	if _, _, err := e.FinishBuild(context.Background(), "bld_nope", BuildSucceeded, nil); !errors.Is(err, ErrBuildNotFound) {
		t.Fatal(err)
	}
	if _, _, err := e.FinishBuild(context.Background(), b.ID, "running", nil); err == nil {
		t.Fatal("finish needs a final status")
	}
}

func TestBuildRequestsAreValidated(t *testing.T) {
	e, _ := buildSvc(t)
	cases := map[string]BuildInput{
		"no target":     {Source: "s", Title: "t"},
		"bad target":    {Target: "bad target!", Source: "s", Title: "t"},
		"long target":   {Target: strings.Repeat("a", 65), Source: "s", Title: "t"},
		"long source":   {Target: "nodeA", Source: strings.Repeat("a", 65), Title: "t"},
		"long holder":   {Target: "nodeA", Source: "s", Title: "t", Holder: strings.Repeat("a", 65)},
		"long kind":     {Target: "nodeA", Source: "s", Title: "t", Kind: strings.Repeat("a", 65)},
		"long progress": {Target: "nodeA", Source: "s", Title: "t", Progress: strings.Repeat("a", 201)},
		"no source":     {Target: "nodeA", Title: "t"},
		"no title":      {Target: "nodeA", Source: "s"},
		"bad mode":      {Target: "nodeA", Source: "s", Title: "t", PreemptMode: "sideways"},
		"negative ttl":  {Target: "nodeA", Source: "s", Title: "t", TTLSeconds: -1},
		"multiline":     {Target: "nodeA", Source: "s", Title: "a\nb"},
		"overlong text": {Target: "nodeA", Source: "s", Title: strings.Repeat("é", 201)},
	}
	for name, in := range cases {
		var invalid *InvalidError
		if _, err := e.CreateBuild(context.Background(), in); !errors.As(err, &invalid) {
			t.Errorf("%s: want an invalid-request error, got %v", name, err)
		}
	}
	if list, _ := e.ListBuilds(BuildFilter{}); len(list) != 0 {
		t.Fatalf("a refused request records nothing: %+v", list)
	}
	// Exactly at the limits is accepted (the title and progress count characters).
	ok, err := e.CreateBuild(context.Background(), BuildInput{Target: strings.Repeat("a", 64), Source: strings.Repeat("s", 64), Holder: strings.Repeat("h", 64), Kind: strings.Repeat("k", 64), Title: strings.Repeat("é", 200), Progress: strings.Repeat("é", 200)})
	if err != nil || utf8Len(ok.Title) != 200 {
		t.Fatalf("the boundary values are valid: %+v %v", ok, err)
	}
	b := startBuild(t, e, "nodeA", "", 99999)
	if b.ExpiresAt != e.now().Add(BuildMaxTTL).Format(time.RFC3339) {
		t.Fatalf("ttl is capped at two hours: %+v", b)
	}
}

func utf8Len(s string) int { return len([]rune(s)) }

func TestBuildTTLCannotOverflowTheCap(t *testing.T) {
	for _, secs := range []int{9223372037, math.MaxInt, math.MaxInt32, 7201} {
		if d, err := buildTTL(secs); err != nil || d != BuildMaxTTL {
			t.Fatalf("%d: %v %v", secs, d, err)
		}
	}
	if d, _ := buildTTL(7200); d != BuildMaxTTL {
		t.Fatal(d)
	}
	e, _ := buildSvc(t)
	b := startBuild(t, e, "host-a", "", math.MaxInt)
	if b.ExpiresAt != e.now().Add(BuildMaxTTL).Format(time.RFC3339) {
		t.Fatalf("create: %+v", b)
	}
	hb, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 9223372037)
	if err != nil || hb.ExpiresAt != e.now().Add(BuildMaxTTL).Format(time.RFC3339) {
		t.Fatalf("heartbeat: %+v %v", hb, err)
	}
}

func TestBuildWithoutHeartbeatExpiresToLost(t *testing.T) {
	e, clock := buildSvc(t)
	b := startBuild(t, e, "nodeA", PreemptSuspend, 60)

	clock.advance(59 * time.Second)
	tick(e)
	if got, _ := e.GetBuild(b.ID); got.Status != BuildRunning {
		t.Fatalf("before its expiry the build still runs: %+v", got)
	}
	clock.advance(2 * time.Second)
	tick(e)
	got, _ := e.GetBuild(b.ID)
	if got.Status != BuildLost || got.EndedAt == "" {
		t.Fatalf("past its expiry the build is lost: %+v", got)
	}
}

func TestBuildHistoryIsOrderedFilteredAndPruned(t *testing.T) {
	e, clock := buildSvc(t)
	old := startBuild(t, e, "host-a", "", 0)
	finishBuild(t, e, old.ID, BuildSucceeded)
	clock.advance(time.Hour)
	mid := startBuild(t, e, "host-b", "", 0)
	finishBuild(t, e, mid.ID, BuildFailed)
	clock.advance(time.Hour)
	running := startBuild(t, e, "host-a", "", 7200)

	list, err := e.ListBuilds(BuildFilter{})
	if err != nil || len(list) != 3 || list[0].ID != running.ID || list[1].ID != mid.ID || list[2].ID != old.ID {
		t.Fatalf("running first, then history newest first: %+v %v", list, err)
	}
	if l, _ := e.ListBuilds(BuildFilter{Target: "host-a"}); len(l) != 2 {
		t.Fatalf("target filter: %+v", l)
	}
	if l, _ := e.ListBuilds(BuildFilter{Status: BuildFailed}); len(l) != 1 || l[0].ID != mid.ID {
		t.Fatalf("status filter: %+v", l)
	}
	if l, _ := e.ListBuilds(BuildFilter{Source: "nobody"}); len(l) != 0 {
		t.Fatalf("source filter: %+v", l)
	}
	if l, _ := e.ListBuilds(BuildFilter{Limit: 2}); len(l) != 3 {
		t.Fatalf("limit cuts only the history: the running build plus two ended: %+v", l)
	}
	waiting := lowBuild(t, e, "nodeA", 7200)
	if waiting.Status != BuildRunning {
		t.Fatalf("%+v", waiting)
	}

	// Retention is 30 days. Both active builds are heartbeated inside their
	// 7200 s window (every 90 minutes), so they stay active for 32 days; they
	// are never pruned while the ended builds past the retention go.
	for i := 0; i < 32*16; i++ {
		clock.advance(90 * time.Minute)
		for _, id := range []string{running.ID, waiting.ID} {
			if _, err := e.HeartbeatBuild(context.Background(), id, nil, 7200); err != nil {
				t.Fatal(err)
			}
		}
		e.Reconcile(context.Background())
	}
	l, _ := e.ListBuilds(BuildFilter{Limit: -1})
	if len(l) != 2 {
		t.Fatalf("only the two active builds are left: %+v", l)
	}
	got := map[string]string{}
	for _, b := range l {
		got[b.ID] = b.Status
	}
	if got[running.ID] != BuildRunning || got[waiting.ID] != BuildRunning {
		t.Fatalf("running builds are never pruned: %+v", got)
	}
}

func TestBuildRetentionIsConfigurable(t *testing.T) {
	e, clock := buildSvc(t)
	e.retention = func() time.Duration { return 48 * time.Hour }
	b := startBuild(t, e, "host-a", "", 0)
	finishBuild(t, e, b.ID, BuildSucceeded)
	clock.advance(36 * time.Hour)
	e.Reconcile(context.Background())
	if l, _ := e.ListBuilds(BuildFilter{}); len(l) != 1 {
		t.Fatal("still inside the retention")
	}
	clock.advance(36 * time.Hour)
	e.Reconcile(context.Background())
	if l, _ := e.ListBuilds(BuildFilter{}); len(l) != 0 {
		t.Fatalf("pruned after two days: %+v", l)
	}
}

func TestHeartbeatWithoutTTLKeepsAShortWindow(t *testing.T) {
	e, clock := buildSvc(t)
	b := startBuild(t, e, "host-a", "", 30)
	clock.advance(10 * time.Second)
	hb, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0)
	if err != nil || hb.ExpiresAt != clock.now().Add(30*time.Second).Format(time.RFC3339) {
		t.Fatalf("a build created with ttl 30 keeps 30 s, not 60: %+v %v", hb, err)
	}
}

func TestLateHeartbeatMarksTheBuildLost(t *testing.T) {
	e, clock := buildSvc(t)
	b := startBuild(t, e, "nodeA", "", 60)
	created := b.ExpiresAt
	clock.advance(61 * time.Second) // past expires_at, before any tick
	got, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 600)
	if !errors.Is(err, ErrBuildEnded) || got.Status != BuildLost || got.EndedAt != created {
		t.Fatalf("a late heartbeat does not revive the build: %+v %v", got, err)
	}
	if again, _ := e.GetBuild(b.ID); again.Status != BuildLost || again.ExpiresAt != created {
		t.Fatalf("stored lost, expiry untouched: %+v", again)
	}
}
