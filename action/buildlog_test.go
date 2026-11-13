package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

func numbered(from, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line %d", from+i)
	}
	return out
}

func TestBuildLogRejectsBadLines(t *testing.T) {
	e, _ := buildEngine(t, &mockTranscodeExecutor{})
	b := startBuild(t, e, "laptop", "", 0)
	bad := map[string][]string{
		"too many lines": numbered(0, BuildLogMaxBatch+1),
		"too long":       {strings.Repeat("x", BuildLogMaxLine+1)},
		"newline":        {"a\nb"},
		"carriage":       {"a\rb"},
		"nul":            {"a\x00b"},
	}
	for name, lines := range bad {
		_, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, lines...)
		var invalid *BuildInvalidError
		if !errors.As(err, &invalid) {
			t.Errorf("%s: want a BuildInvalidError, got %v", name, err)
		}
	}
	if _, lines, _, _ := e.BuildLog(b.ID); len(lines) != 0 {
		t.Fatalf("a rejected heartbeat must keep nothing: %v", lines)
	}
	// The limits themselves are fine, multi-byte characters count once, and
	// an empty array changes nothing.
	ok := append(numbered(0, BuildLogMaxBatch-1), strings.Repeat("é", BuildLogMaxLine))
	if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, ok...); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, lines, _, _ := e.BuildLog(b.ID); len(lines) != BuildLogMaxBatch {
		t.Fatalf("got %d lines", len(lines))
	}
}

func TestBuildLogKeepsTheLastLinesInOrder(t *testing.T) {
	e, clock := buildEngine(t, &mockTranscodeExecutor{})
	b := startBuild(t, e, "laptop", "", 0)
	if _, lines, at, _ := e.BuildLog(b.ID); lines == nil || len(lines) != 0 || !at.IsZero() {
		t.Fatalf("no output yet must be an empty slice: %v %v", lines, at)
	}
	for i := 0; i < 5; i++ { // 5 x 60 = 300 lines
		clock.advance(time.Second)
		if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, numbered(i*60, 60)...); err != nil {
			t.Fatal(err)
		}
	}
	_, lines, at, err := e.BuildLog(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != BuildLogKeep || lines[0] != "line 100" || lines[len(lines)-1] != "line 299" {
		t.Fatalf("want the last %d lines in order, got %d: %q .. %q", BuildLogKeep, len(lines), lines[0], lines[len(lines)-1])
	}
	if !at.Equal(clock.now()) {
		t.Fatalf("updated_at %v", at)
	}
	// A heartbeat without lines leaves them and the update time alone.
	clock.advance(time.Minute)
	if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, l2, at2, _ := e.BuildLog(b.ID); len(l2) != BuildLogKeep || !at2.Equal(at) {
		t.Fatal("an empty heartbeat changed the log")
	}
}

func TestBuildLogIsForgottenAfterTheBuildEnds(t *testing.T) {
	for _, end := range []string{"finish", "lost"} {
		t.Run(end, func(t *testing.T) {
			e, clock := buildEngine(t, &mockTranscodeExecutor{})
			b := startBuild(t, e, "laptop", "", 60)
			if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, "synthetic output"); err != nil {
				t.Fatal(err)
			}
			if end == "finish" {
				if _, _, err := e.FinishBuild(context.Background(), b.ID, store.BuildSucceeded, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				clock.advance(2 * time.Minute)
				e.reconcileBuilds(context.Background())
			}
			// Readable for a while after the end, with the final status.
			clock.advance(BuildLogRetention - time.Minute*3)
			got, lines, _, err := e.BuildLog(b.ID)
			if err != nil || len(lines) != 1 {
				t.Fatalf("within the retention: %v %v", lines, err)
			}
			if end == "finish" && got.Status != store.BuildSucceeded || end == "lost" && got.Status != store.BuildLost {
				t.Fatalf("status %s", got.Status)
			}
			clock.advance(5 * time.Minute)
			e.reconcileBuilds(context.Background())
			if ids := e.buildLogs.ids(); len(ids) != 0 {
				t.Fatalf("the buffer must be dropped: %v", ids)
			}
			got, lines, at, err := e.BuildLog(b.ID)
			if err != nil || len(lines) != 0 || !at.IsZero() || got.ID != b.ID {
				t.Fatalf("after the retention: %v %v %v", lines, at, err)
			}
		})
	}
}

func TestBuildLogReadDropsWithoutAReconcile(t *testing.T) {
	e, clock := buildEngine(t, &mockTranscodeExecutor{})
	b := startBuild(t, e, "laptop", "", 0)
	e.HeartbeatBuild(context.Background(), b.ID, nil, 0, "x")
	e.FinishBuild(context.Background(), b.ID, store.BuildFailed, nil)
	clock.advance(BuildLogRetention)
	if _, lines, _, _ := e.BuildLog(b.ID); len(lines) != 0 {
		t.Fatal("expired output was served")
	}
	if len(e.buildLogs.ids()) != 0 {
		t.Fatal("a read after the retention must drop the buffer")
	}
}

func TestBuildLogUnknownBuildAndEndedHeartbeat(t *testing.T) {
	e, _ := buildEngine(t, &mockTranscodeExecutor{})
	if _, _, _, err := e.BuildLog("nope"); !errors.Is(err, store.ErrBuildNotFound) {
		t.Fatal(err)
	}
	b := startBuild(t, e, "laptop", "", 0)
	e.FinishBuild(context.Background(), b.ID, store.BuildSucceeded, nil)
	if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, "late"); !errors.Is(err, store.ErrBuildEnded) {
		t.Fatal(err)
	}
	if _, lines, _, _ := e.BuildLog(b.ID); len(lines) != 0 {
		t.Fatal("an ended build takes no more output")
	}
}

func TestBuildLogBuffersAreBounded(t *testing.T) {
	var l buildLogs
	now := time.Now()
	for i := 0; i < buildLogMaxBuilds+20; i++ {
		l.append(fmt.Sprintf("b%d", i), []string{"x"}, now.Add(time.Duration(i)*time.Second), nil)
	}
	if n := len(l.ids()); n != buildLogMaxBuilds {
		t.Fatalf("%d buffers", n)
	}
	if lines, _ := l.get("b0"); lines != nil {
		t.Fatal("the oldest buffer must go first")
	}
}

func TestBuildLogIsNotPersisted(t *testing.T) {
	e, _ := buildEngine(t, &mockTranscodeExecutor{})
	b := startBuild(t, e, "laptop", "", 0)
	if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, "token=synthetic-secret"); err != nil {
		t.Fatal(err)
	}
	// Nothing about the output is in the stored record.
	rec, err := e.deps.Store.GetBuild(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s := fmt.Sprintf("%+v", rec); strings.Contains(s, "synthetic-secret") {
		t.Fatalf("the output reached the build record: %s", s)
	}
	// A new engine over the same store (a restart) has no output.
	e2 := NewEngine(EngineDeps{Store: e.deps.Store, Config: e.deps.Config, Now: e.deps.Now})
	if _, lines, _, err := e2.BuildLog(b.ID); err != nil || len(lines) != 0 {
		t.Fatalf("a restart must forget the output: %v %v", lines, err)
	}
}
