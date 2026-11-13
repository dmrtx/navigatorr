package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
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

// The output reaches nothing that outlives the process or leaves it: not the
// database files (rows, write-ahead log), not the build records the lists and
// the node status serve. There is no database export or backup of builds in the
// repo to check beyond the files themselves.
func TestBuildLogIsNotPersisted(t *testing.T) {
	const secret = "token=synthetic-secret-8f3a"
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	clock := &buildClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}, Workers: twoWorkers(t, &mockTranscodeExecutor{}, &mockTranscodeExecutor{}, 0), DrainSettle: -1, Now: clock.now})
	b := startBuild(t, e, "m1", "graceful", 0)
	for i := 0; i < 3; i++ {
		if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, secret, "second "+secret); err != nil {
			t.Fatal(err)
		}
	}
	if _, lines, _, _ := e.BuildLog(b.ID); len(lines) != 6 {
		t.Fatal("the output is meant to be in memory", lines)
	}
	finishBuild(t, e, b.ID, store.BuildSucceeded)

	// What the callers can read.
	payloads := map[string]any{"build": mustBuild(t, e, b.ID), "list": mustList(t, e), "pause": e.NodePauseStatuses(context.Background())}
	for name, v := range payloads {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "synthetic-secret") {
			t.Fatalf("the output reached the %s payload: %s", name, raw)
		}
	}
	// What the disk holds, the write-ahead log included, before and after a close.
	scan := func(when string) {
		files, _ := filepath.Glob(filepath.Join(dir, "state.db*"))
		if len(files) == 0 {
			t.Fatal("no database file")
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte("synthetic-secret")) {
				t.Fatalf("the output reached %s %s", filepath.Base(f), when)
			}
		}
	}
	scan("while open")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	scan("after close")
	// The scan can see what the database does hold: the build's title is there.
	if raw, err := os.ReadFile(filepath.Join(dir, "state.db")); err != nil || !bytes.Contains(raw, []byte("integration run")) {
		t.Fatal("the scan is blind: the title is not in the database file", err)
	}
	// A restart over the same file has no output.
	st2, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	e2 := NewEngine(EngineDeps{Store: st2, Config: &config.Config{}, Now: clock.now})
	if _, lines, _, err := e2.BuildLog(b.ID); err != nil || len(lines) != 0 {
		t.Fatalf("a restart must forget the output: %v %v", lines, err)
	}
}

func mustBuild(t *testing.T, e *Engine, id string) store.Build {
	t.Helper()
	b, err := e.GetBuild(id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustList(t *testing.T, e *Engine) []store.Build {
	t.Helper()
	list, err := e.ListBuilds(store.BuildFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// A store that cannot answer says nothing about a build: it is not ended, so
// its buffer is not the first to go.
func TestBuildEndedIgnoresStoreErrors(t *testing.T) {
	e, _ := buildEngine(t, &mockTranscodeExecutor{})
	b := startBuild(t, e, "laptop", "", 0)
	if e.buildEnded(b.ID) {
		t.Fatal("a running build is not ended")
	}
	if !e.buildEnded("bld_gone") {
		t.Fatal("a build that is gone is")
	}
	finishBuild(t, e, b.ID, store.BuildFailed)
	if !e.buildEnded(b.ID) {
		t.Fatal("a finished build is ended")
	}
	e.deps.Store.Close()
	if e.buildEnded(b.ID) {
		t.Fatal("a store error must not read as ended")
	}
}

// A store that fails for a moment says nothing about the build: the buffer is
// kept and the next read has the lines. Only "unknown build" drops it.
func TestBuildLogSurvivesATransientStoreError(t *testing.T) {
	e, _ := buildEngine(t, &mockTranscodeExecutor{})
	b := startBuild(t, e, "laptop", "", 0)
	if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, "one", "two"); err != nil {
		t.Fatal(err)
	}
	working := e.deps.Store
	broken, err := store.Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	broken.Close()
	e.deps.Store = broken
	if _, _, _, err := e.BuildLog(b.ID); err == nil || errors.Is(err, store.ErrBuildNotFound) {
		t.Fatalf("a store error is returned as it is: %v", err)
	}
	if lines, _ := e.buildLogs.get(b.ID); len(lines) != 2 {
		t.Fatal("a transient error dropped the buffer")
	}
	e.deps.Store = working
	if _, lines, _, err := e.BuildLog(b.ID); err != nil || len(lines) != 2 {
		t.Fatalf("the next read has the lines: %v %v", lines, err)
	}
	// A build the store does not know loses its buffer.
	e.buildLogs.append("bld_gone", []string{"x"}, e.now(), nil)
	if _, _, _, err := e.BuildLog("bld_gone"); !errors.Is(err, store.ErrBuildNotFound) {
		t.Fatal(err)
	}
	if lines, _ := e.buildLogs.get("bld_gone"); lines != nil {
		t.Fatal("an unknown build keeps a buffer")
	}
}

// expired says the output was dropped on purpose (retention, the cap), not
// that an ended build never had any.
func TestBuildOutputExpiredOnlyWhenDropped(t *testing.T) {
	e, clock := buildEngine(t, &mockTranscodeExecutor{})
	ctx := context.Background()
	silent := startBuild(t, e, "laptop", "", 0)
	finishBuild(t, e, silent.ID, store.BuildSucceeded)
	out, err := e.BuildOutput(silent.ID)
	if err != nil || out.Expired || len(out.Lines) != 0 {
		t.Fatalf("an ended build that sent nothing is not expired: %+v %v", out, err)
	}
	running := startBuild(t, e, "laptop", "", 0)
	if out, _ := e.BuildOutput(running.ID); out.Expired {
		t.Fatal("a running build is not expired")
	}
	e.HeartbeatBuild(ctx, running.ID, nil, 0, "x")
	if out, _ := e.BuildOutput(running.ID); out.Expired || len(out.Lines) != 1 {
		t.Fatalf("%+v", out)
	}
	// Retention.
	kept := startBuild(t, e, "laptop", "", 0)
	e.HeartbeatBuild(ctx, kept.ID, nil, 0, "line")
	finishBuild(t, e, kept.ID, store.BuildSucceeded)
	clock.advance(BuildLogRetention)
	if out, _ := e.BuildOutput(kept.ID); !out.Expired || len(out.Lines) != 0 {
		t.Fatalf("after the retention: %+v", out)
	}
	// A build that never sent output stays "No output" after the retention too,
	// on a read and after the reconcile tick has pruned.
	if out, _ := e.BuildOutput(silent.ID); out.Expired || len(out.Lines) != 0 {
		t.Fatalf("a quiet build read after the retention is not expired: %+v", out)
	}
	e.pruneBuildLogs()
	if out, _ := e.BuildOutput(silent.ID); out.Expired {
		t.Fatalf("pruning does not make a quiet build expired: %+v", out)
	}
	if out, _ := e.BuildOutput(kept.ID); !out.Expired {
		t.Fatal("the dropped build stays expired after the prune")
	}
	// The cap.
	clock.advance(time.Second)
	first := startBuild(t, e, "laptop", "", 0)
	e.HeartbeatBuild(ctx, first.ID, nil, 0, "x")
	finishBuild(t, e, first.ID, store.BuildFailed)
	for i := 1; i < buildLogMaxBuilds; i++ {
		clock.advance(time.Second)
		b := startBuild(t, e, "laptop", "", 0)
		e.HeartbeatBuild(ctx, b.ID, nil, 0, "x")
	}
	clock.advance(time.Second)
	over := startBuild(t, e, "laptop", "", 0)
	e.HeartbeatBuild(ctx, over.ID, nil, 0, "x")
	if out, _ := e.BuildOutput(first.ID); !out.Expired || len(out.Lines) != 0 {
		t.Fatalf("an evicted buffer is expired: %+v", out)
	}
}

// A buffer the reconcile tick drops (nobody read it) is remembered as dropped too.
func TestPruneRemembersTheBuffersItDrops(t *testing.T) {
	e, clock := buildEngine(t, &mockTranscodeExecutor{})
	b := startBuild(t, e, "laptop", "", 0)
	e.HeartbeatBuild(context.Background(), b.ID, nil, 0, "line")
	finishBuild(t, e, b.ID, store.BuildSucceeded)
	clock.advance(BuildLogRetention)
	e.pruneBuildLogs()
	if lines, _ := e.buildLogs.get(b.ID); lines != nil {
		t.Fatal("the prune drops the buffer")
	}
	if out, _ := e.BuildOutput(b.ID); !out.Expired {
		t.Fatalf("%+v", out)
	}
}

func TestDroppedSetIsBoundedAndOldestGoesFirst(t *testing.T) {
	var l buildLogs
	now := time.Now()
	total := buildLogMaxDropped + 44
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("b%d", i)
		l.append(id, []string{"x"}, now, nil)
		l.expire(id)
	}
	if n := len(l.dropped); n != buildLogMaxDropped || len(l.droppedOrder) != buildLogMaxDropped {
		t.Fatalf("%d ids, %d in order", n, len(l.droppedOrder))
	}
	if l.wasDropped("b0") || l.wasDropped("b43") {
		t.Fatal("the oldest ids go first")
	}
	if !l.wasDropped("b44") || !l.wasDropped(fmt.Sprintf("b%d", total-1)) {
		t.Fatal("the newest ids stay")
	}
	// Expiring a build that has no buffer records nothing.
	l.expire("never-had-output")
	if l.wasDropped("never-had-output") || len(l.dropped) != buildLogMaxDropped {
		t.Fatal("nothing was dropped")
	}
	// Output sent again, or the build gone, takes the id back out.
	l.append("b100", []string{"x"}, now, nil)
	if l.wasDropped("b100") || len(l.droppedOrder) != buildLogMaxDropped-1 {
		t.Fatal("a fresh buffer is not dropped")
	}
	l.drop("b101")
	if l.wasDropped("b101") || len(l.dropped) != len(l.droppedOrder) {
		t.Fatal("a gone build is forgotten")
	}
}
