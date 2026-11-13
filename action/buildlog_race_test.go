package action

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
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
	// Widen the gap between the store update and the append, so a heartbeat
	// is regularly caught in it when the finish lands: without the engine's
	// lock the output then changes after the finish returned.
	gap := func() { time.Sleep(time.Millisecond) }
	buildHeartbeatGap.Store(&gap)
	t.Cleanup(func() { buildHeartbeatGap.Store(nil) })
	// 8 x 20 lines stay below BuildLogKeep, so every accepted line is still kept.
	const workers, perWorker, finishAfter = 8, 20, 40
	if workers*perWorker >= BuildLogKeep {
		t.Fatal("the test must not overflow the buffer")
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted = map[string]bool{}
		refused  int
		count    atomic.Int32
		start    = make(chan struct{})
		reached  = make(chan struct{})
		finished = make(chan struct{})
		once     sync.Once
		atEnd    []string
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < perWorker; i++ {
				line := fmt.Sprintf("w%d-%03d", w, i)
				_, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, line)
				mu.Lock()
				switch {
				case err == nil:
					accepted[line] = true
				case errors.Is(err, store.ErrBuildEnded):
					refused++
				default:
					t.Errorf("heartbeat: %v", err)
				}
				mu.Unlock()
				if err == nil && count.Add(1) == finishAfter {
					once.Do(func() { close(reached) })
				}
			}
			// One more after the end, so the refusal is always exercised.
			<-finished
			_, err := e.HeartbeatBuild(context.Background(), b.ID, nil, 0, fmt.Sprintf("w%d-late", w))
			mu.Lock()
			if errors.Is(err, store.ErrBuildEnded) {
				refused++
			} else {
				t.Errorf("a heartbeat after the end: %v", err)
			}
			mu.Unlock()
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		<-reached
		if _, _, err := e.FinishBuild(context.Background(), b.ID, store.BuildCancelled, nil); err != nil {
			t.Errorf("finish: %v", err)
		}
		// The output as the finish leaves it: nothing may change it afterwards.
		_, atEnd, _, _ = e.BuildLog(b.ID)
		close(finished)
	}()
	close(start)
	wg.Wait()
	_, lines, _, err := e.BuildLog(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) < finishAfter || refused == 0 {
		t.Fatalf("the test proves nothing: %d accepted, %d refused", len(accepted), refused)
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
	if !reflect.DeepEqual(lines, atEnd) {
		t.Fatalf("lines landed after the finish returned: %d at the end, %d now", len(atEnd), len(lines))
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
