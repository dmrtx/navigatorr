package nodes

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A build is either registered before the pause call returns or refused; it is
// never created after. The seam holds a create between its check and its
// insert, which is exactly where a toggle without the node's lock would slip in.
func TestBuildsPauseNeverLetsABuildBeCreatedAfterItReturns(t *testing.T) {
	e, _ := buildSvc(t)
	checked := make(chan struct{})
	release := make(chan struct{})
	e.afterBuildsPausedCheck = func(string) {
		close(checked)
		<-release
	}
	created := make(chan error, 1)
	go func() {
		_, err := createBuild(e, "nodeA", "")
		created <- err
	}()
	// The create has passed the check and is held before its insert.
	<-checked
	paused := make(chan struct{})
	pauseErr := make(chan error, 1)
	go func() {
		// Not t.Fatal: this is not the test goroutine. The error goes back.
		pauseErr <- e.SetBuildsPause(context.Background(), "nodeA", true)
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
		if err := <-pauseErr; err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pause must return once the create is done")
	}
	if list, _ := e.ListBuilds(BuildFilter{}); len(list) != 1 {
		t.Fatalf("the build registered before the pause: %+v", list)
	}
	if _, err := createBuild(e, "nodeA", ""); err == nil {
		t.Fatal("and now builds are refused")
	}
}

// The same invariant under load, for the race detector: many creates race one
// toggle, and once the toggle has returned nothing more is registered.
func TestBuildsPauseRacingCreatesRegisterBeforeItOrAreRefused(t *testing.T) {
	for round := 0; round < 25; round++ {
		e, _ := buildSvc(t)
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
					_, err := createBuild(e, "nodeA", "")
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
		pauseBuilds(t, e, "nodeA", true)
		atReturn, _ := e.ListBuilds(BuildFilter{})
		wg.Wait()
		after, _ := e.ListBuilds(BuildFilter{})
		if len(unexpected) > 0 {
			t.Fatal(unexpected)
		}
		if len(after) != len(atReturn) {
			t.Fatalf("round %d: a build was created after the pause returned: %d then %d", round, len(atReturn), len(after))
		}
	}
}

// The create path and the pause toggle take the same per-node lock: holding it
// blocks both until it is released, and a target that is not a node never
// takes it.
func TestBuildsPauseAndCreateShareTheNodeLock(t *testing.T) {
	e, _ := buildSvc(t)
	lock := e.pauseLock("nodeA")
	lock.Lock()
	finished := make(chan string, 3)
	go func() {
		_ = e.SetBuildsPause(context.Background(), "nodeA", true)
		finished <- "pause"
	}()
	go func() {
		_, _ = createBuild(e, "laptop-is-not-a-node-so-no-lock", "")
		finished <- "other"
	}()
	if got := <-finished; got != "other" {
		t.Fatalf("a non-node create must not wait for the lock, %s finished first", got)
	}
	go func() {
		_, _ = createBuild(e, "nodeA", "")
		finished <- "create"
	}()
	select {
	case got := <-finished:
		t.Fatalf("%s finished while the node's lock was held", got)
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
