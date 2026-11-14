package transcode

import (
	"testing"
	"time"
)

// The reserve follows the goal, and the server and the worker use this one
// function, so the figure the queue waits with is the figure the node admits.
func TestOutputReserveBytesFollowsTheGoal(t *testing.T) {
	policy := func(min float64, max int64) *Plan {
		return &Plan{SizePolicy: &SizePolicy{MinSavingsPercent: min, MaxCandidateBytes: max}}
	}
	audio := policy(10, 0)
	audio.VideoCodec, audio.Mode, audio.ModePolicyVersion = VideoCodecCopy, "size", ModePolicyV9
	for _, tc := range []struct {
		name   string
		plan   *Plan
		source int64
		want   int64
	}{
		{"size goal: its byte budget", policy(10, 120), 1000, 120},
		{"size goal: a budget over the source reserves the source", policy(10, 5000), 1000, 1000},
		{"quality or compact goal: half the source", policy(15, 0), 1000, 500},
		{"x265_preserve: the whole source", &Plan{Mode: "x265_preserve", SizePolicy: &SizePolicy{}}, 1000, 1000},
		{"no plan: half the source", nil, 1000, 500},
		{"no size policy: half the source", &Plan{}, 1000, 500},
		{"audio-only goal: the most the policy accepts", audio, 1000, 900},
		{"unknown source: nothing", policy(10, 120), 0, 0},
	} {
		if got := OutputReserveBytes(tc.plan, tc.source); got != tc.want {
			t.Errorf("%s: reserve = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A conversion that publishes on encode needs disk room only; one that keeps
// its candidate also needs room in the retention budget.
func TestJudgeAppliesTheRetentionBudgetOnlyToARetainedConversion(t *testing.T) {
	now := time.Now()
	r := &RetentionRoom{ObservedAt: now, CapacityBytes: 100, RoomBytes: 10, DiskCapacityBytes: 10_000, DiskRoomBytes: 5_000}
	retained := RoomNeed{SourceBytes: 1_000, ReserveBytes: 50, Retained: true}
	plain := RoomNeed{SourceBytes: 1_000, ReserveBytes: 50}
	if v, _ := r.Judge(retained, now); v != RoomApprovals {
		t.Fatalf("retained verdict = %v, want approvals", v)
	}
	if v, _ := r.Judge(plain, now); v != RoomFits {
		t.Fatalf("publish-on-encode verdict = %v, want fits", v)
	}
	if v, offered := r.Judge(RoomNeed{SourceBytes: 6_000, ReserveBytes: 50}, now); v != RoomNoSpace || offered != 5_000 {
		t.Fatalf("a disk that cannot give it room: %v %d", v, offered)
	}
	if r.EverFits(RoomNeed{SourceBytes: 100, ReserveBytes: 200, Retained: true}) || !r.EverFits(RoomNeed{SourceBytes: 100, ReserveBytes: 200}) {
		t.Fatal("an output over the whole budget never fits a node that must retain it, but fits one that publishes on encode")
	}
	if v, _ := (&RetentionRoom{ObservedAt: now, Hold: "paused"}).Judge(plain, now); v != RoomPaused {
		t.Fatalf("a paused node takes no job, whatever the policy: %v", v)
	}
	if v, _ := r.Judge(retained, now.Add(time.Hour)); v != RoomFits {
		t.Fatalf("an old report says nothing: %v", v)
	}
}

// An old report may describe a disk that has since changed: it never says a job
// can never fit. A fresh one does.
func TestJudgeSaysNeverOnlyFromAFreshReport(t *testing.T) {
	now := time.Now()
	need := RoomNeed{SourceBytes: 900, ReserveBytes: 450}
	report := func(at time.Time) *RetentionRoom {
		return &RetentionRoom{ObservedAt: at, DiskCapacityBytes: 1000, FloorBytes: 100, DiskRoomBytes: 900}
	}
	if v, _ := report(now).Judge(need, now); v != RoomNever {
		t.Fatalf("fresh report: %v, want never", v)
	}
	if v, _ := report(now.Add(-2*RetentionRoomMaxAge)).Judge(need, now); v != RoomFits {
		t.Fatalf("old report: %v, want fits (the node decides)", v)
	}
}
