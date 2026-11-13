package action

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// A build is either registered before the pause call returns or refused; it is
// never created after. The seam holds a create between its check and its
// insert, which is exactly where a toggle without the node's lock would slip in.
func TestBuildsPauseNeverLetsABuildBeCreatedAfterItReturns(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	checked := make(chan struct{})
	release := make(chan struct{})
	e.afterBuildsPausedCheck = func(string) {
		close(checked)
		<-release
	}
	created := make(chan error, 1)
	go func() {
		_, err := createBuild(e, "m1", "")
		created <- err
	}()
	// The create has passed the check and is held before its insert.
	<-checked
	paused := make(chan struct{})
	go func() {
		pauseBuilds(t, e, "m1", true)
		close(paused)
	}()
	select {
	case <-paused:
		t.Fatal("the pause returned while a create was between its check and its insert")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("the pause must return once the create is done")
	}
	if list, _ := e.ListBuilds(store.BuildFilter{}); len(list) != 1 {
		t.Fatalf("the build registered before the pause: %+v", list)
	}
	if _, err := createBuild(e, "m1", ""); err == nil {
		t.Fatal("and now builds are refused")
	}
}

// The same invariant under load, for the race detector: many creates race one
// toggle, and once the toggle has returned nothing more is registered.
func TestBuildsPauseRacingCreatesRegisterBeforeItOrAreRefused(t *testing.T) {
	for round := 0; round < 25; round++ {
		e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
		var wg sync.WaitGroup
		var mu sync.Mutex
		var unexpected []error
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for j := 0; j < 5; j++ {
					_, err := createBuild(e, "m1", "")
					var refused *BuildsPausedError
					if err != nil && !errors.As(err, &refused) {
						mu.Lock()
						unexpected = append(unexpected, err)
						mu.Unlock()
					}
				}
			}()
		}
		close(start)
		pauseBuilds(t, e, "m1", true)
		atReturn, _ := e.ListBuilds(store.BuildFilter{})
		wg.Wait()
		after, _ := e.ListBuilds(store.BuildFilter{})
		if len(unexpected) > 0 {
			t.Fatal(unexpected)
		}
		if len(after) != len(atReturn) {
			t.Fatalf("round %d: a build was created after the pause returned: %d then %d", round, len(atReturn), len(after))
		}
	}
}

// withNodeLock (the create path) and SetNodeBuildsPause take the same mutex:
// holding the node's request lock blocks both until it is released.
func TestBuildsPauseAndCreateShareTheNodeRequestLock(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	lock := &e.workerStateFor("m1").pauseMu
	lock.Lock()
	finished := make(chan string, 2)
	go func() {
		_, _ = e.SetNodeBuildsPause(context.Background(), "m1", true)
		finished <- "pause"
	}()
	go func() {
		_, _ = createBuild(e, "laptop-is-not-a-node-so-no-lock", "")
		finished <- "other"
	}()
	// A target that is not a node never takes the lock.
	if got := <-finished; got != "other" {
		t.Fatalf("a non-node create must not wait for the lock, %s finished first", got)
	}
	go func() {
		_, _ = createBuild(e, "m1", "")
		finished <- "create"
	}()
	select {
	case got := <-finished:
		t.Fatalf("%s finished while the node's request lock was held", got)
	case <-time.After(150 * time.Millisecond):
	}
	lock.Unlock()
	for i := 0; i < 2; i++ {
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("both must finish once the lock is released")
		}
	}
}

// With the build-pause state unreadable, status and observation say paused, as
// the create path refuses.
func TestBuildsPauseStatusFailsClosedWhenTheStateCannotBeRead(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	if e.NodeBuildsPaused("m1") {
		t.Fatal("readable and unpaused")
	}
	if err := e.deps.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if !e.NodeBuildsPaused("m1") {
		t.Fatal("an unreadable state counts as paused")
	}
	st := e.NodePauseStatuses(context.Background())
	if !st["m1"].BuildsPaused || !st["m4"].BuildsPaused {
		t.Fatalf("status must fail closed: %+v", st)
	}
}

// An unpaused node's status JSON has no builds pause keys, so existing
// responses are unchanged while the switch is off.
func TestUnpausedNodeStatusJSONHasNoBuildsPauseKeys(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	data, err := json.Marshal(e.NodePauseStatuses(context.Background())["m1"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "builds_paused") {
		t.Fatalf("an unpaused node must not mention the builds pause: %s", data)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"node", "mode", "paused", "reachable", "running", "publication_pending", "idle", "message", "checked_at"} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("existing key %q missing: %s", key, data)
		}
	}
	pauseBuilds(t, e, "m1", true)
	data, _ = json.Marshal(e.NodePauseStatuses(context.Background())["m1"])
	if !strings.Contains(string(data), `"builds_paused":true`) || !strings.Contains(string(data), `"builds_paused_since"`) {
		t.Fatalf("a paused node carries both: %s", data)
	}
}
