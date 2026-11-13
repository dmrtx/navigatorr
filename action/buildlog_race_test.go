package action

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

func TestBuildLogEvictsEndedBuffersBeforeRunningOnes(t *testing.T) {
	e, clock := buildEngine(t, &mockTranscodeExecutor{})
	running := startBuild(t, e, "laptop", "", 0)
	e.HeartbeatBuild(context.Background(), running.ID, nil, 0, "keep me") // the oldest buffer
	var ended []string
	for i := 1; i < buildLogMaxBuilds; i++ {
		clock.advance(time.Second)
		b := startBuild(t, e, "laptop", "", 0)
		e.HeartbeatBuild(context.Background(), b.ID, nil, 0, "x")
		if i%2 == 0 {
			e.FinishBuild(context.Background(), b.ID, store.BuildSucceeded, nil)
			ended = append(ended, b.ID)
		}
	}
	if n := len(e.buildLogs.ids()); n != buildLogMaxBuilds {
		t.Fatalf("%d buffers", n)
	}
	// Two more builds: the two oldest ended buffers go, the old running one stays.
	for i := 0; i < 2; i++ {
		clock.advance(time.Second)
		b := startBuild(t, e, "laptop", "", 0)
		e.HeartbeatBuild(context.Background(), b.ID, nil, 0, "new")
	}
	if lines, _ := e.buildLogs.get(running.ID); len(lines) != 1 {
		t.Fatal("a running build lost its output while ended ones were kept")
	}
	for _, id := range ended[:2] {
		if lines, _ := e.buildLogs.get(id); lines != nil {
			t.Fatalf("the oldest ended build %s kept its buffer", id)
		}
	}
	if lines, _ := e.buildLogs.get(ended[2]); lines == nil {
		t.Fatal("only as many ended buffers as needed go")
	}
}

func TestBuildLogEvictsTheLeastRecentRunningBufferWhenNoneEnded(t *testing.T) {
	var l buildLogs
	now := time.Now()
	for i := 0; i < buildLogMaxBuilds; i++ {
		l.append(fmt.Sprintf("b%d", i), []string{"x"}, now.Add(time.Duration(i)*time.Second), func(string) bool { return false })
	}
	l.append("new", []string{"x"}, now.Add(time.Hour), func(string) bool { return false })
	if lines, _ := l.get("b0"); lines != nil {
		t.Fatal("the least recently updated running buffer goes when none ended")
	}
	if lines, _ := l.get("b1"); lines == nil {
		t.Fatal("only one goes")
	}
}

func TestBuildLogConcurrentHeartbeatsKeepOrderAndNothingLandsAfterTheEnd(t *testing.T) {
	e, _ := buildEngine(t, &mockTranscodeExecutor{})
	b := startBuild(t, e, "laptop", "", 0)
	const workers, perWorker = 8, 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := map[string]bool{}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				line := fmt.Sprintf("w%d-%03d", w, i)
				_, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, line)
				if err == nil {
					mu.Lock()
					accepted[line] = true
					mu.Unlock()
				} else if !errors.Is(err, store.ErrBuildEnded) {
					t.Errorf("heartbeat: %v", err)
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(2 * time.Millisecond)
		e.FinishBuild(context.Background(), b.ID, store.BuildCancelled, nil)
	}()
	wg.Wait()
	_, lines, _, err := e.BuildLog(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Only accepted lines are there, each once, and each worker's lines in order.
	last := map[string]string{}
	seen := map[string]bool{}
	for _, l := range lines {
		if !accepted[l] || seen[l] {
			t.Fatalf("line %q was not accepted or is duplicated", l)
		}
		seen[l] = true
		w := l[:2]
		if l <= last[w] {
			t.Fatalf("order broken: %q after %q", l, last[w])
		}
		last[w] = l
	}
	if len(lines) != len(accepted) {
		t.Fatalf("%d lines kept, %d accepted", len(lines), len(accepted))
	}
	// After the end, nothing more lands.
	n := len(lines)
	if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, "after"); !errors.Is(err, store.ErrBuildEnded) {
		t.Fatal(err)
	}
	if _, after, _, _ := e.BuildLog(b.ID); len(after) != n {
		t.Fatal("a line landed after the end")
	}
}

func TestBuildLogInvalidLinesOnAnEndedOrUnknownBuild(t *testing.T) {
	e, _ := buildEngine(t, &mockTranscodeExecutor{})
	bad := []string{"a\nb"}
	if _, err := e.HeartbeatBuild(context.Background(), "nope", nil, 0, bad...); !errors.Is(err, store.ErrBuildNotFound) {
		t.Fatal(err)
	}
	b := startBuild(t, e, "laptop", "", 0)
	e.FinishBuild(context.Background(), b.ID, store.BuildSucceeded, nil)
	if _, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, bad...); !errors.Is(err, store.ErrBuildEnded) {
		t.Fatal("an ended build answers 409 whatever the lines:", err)
	}
}
