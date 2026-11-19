package nodes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Many finishes and cancels of one build at once: exactly one of them changes
// it, the others get the stored record back unchanged, and the record ends in
// the status of the one that won.
func TestConcurrentFinishAndCancelChangeTheBuildOnce(t *testing.T) {
	for round := 0; round < 30; round++ {
		s, _ := buildSvc(t)
		b := startBuild(t, s, "nodeA", "graceful", 0)
		const callers = 12
		var wg sync.WaitGroup
		var changed atomic.Int32
		statuses := make([]string, callers)
		errs := make([]error, callers)
		start := make(chan struct{})
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				var got Build
				var did bool
				var err error
				switch i % 3 {
				case 0:
					got, did, err = s.FinishBuild(context.Background(), b.ID, BuildSucceeded, nil)
				case 1:
					got, did, err = s.FinishBuild(context.Background(), b.ID, BuildFailed, ptr(1))
				default:
					got, did, err = s.CancelBuild(context.Background(), b.ID)
				}
				if did {
					changed.Add(1)
				}
				statuses[i], errs[i] = got.Status, err
			}(i)
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if changed.Load() != 1 {
			t.Fatalf("round %d: %d calls changed the build, want exactly 1", round, changed.Load())
		}
		final, err := s.GetBuild(b.ID)
		if err != nil || final.Active() || final.EndedAt == "" {
			t.Fatal(final, err)
		}
		// Every caller saw the one final record (the winner's status).
		for i, st := range statuses {
			if st != final.Status {
				t.Fatalf("round %d: caller %d saw %s, the record is %s", round, i, st, final.Status)
			}
		}
		if (final.Status == BuildFailed) != (final.ExitCode != nil) {
			t.Fatalf("the exit code belongs to the failed finish only: %+v", final)
		}
	}
}

func ptr(n int) *int { return &n }

// An operator cancel racing the reconcile pass that expires the build: it ends
// once, as lost or as cancelled, never both and never flipping afterwards; the
// cancel reports a change only when it is the one that ended it.
func TestConcurrentCancelAndReconcileExpiryEndTheBuildOnce(t *testing.T) {
	sawLost, sawCancelled := 0, 0
	for round := 0; round < 60; round++ {
		s, clock := buildSvc(t)
		b := startBuild(t, s, "nodeA", "graceful", 30)
		clock.advance(time.Minute) // past its expiry
		var wg sync.WaitGroup
		var cancelChanged bool
		var cancelErr error
		start := make(chan struct{})
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			_, cancelChanged, cancelErr = s.CancelBuild(context.Background(), b.ID)
		}()
		for i := 0; i < 2; i++ {
			go func() {
				defer wg.Done()
				<-start
				tick(s)
			}()
		}
		close(start)
		wg.Wait()
		if cancelErr != nil {
			t.Fatal(cancelErr)
		}
		tick(s) // in case the cancel won and the ticks ran first
		final, err := s.GetBuild(b.ID)
		if err != nil || final.Active() {
			t.Fatal(final, err)
		}
		switch final.Status {
		case BuildCancelled:
			sawCancelled++
			if !cancelChanged {
				t.Fatal("the build is cancelled but the cancel says it changed nothing")
			}
		case BuildLost:
			sawLost++
			if cancelChanged {
				t.Fatal("the build is lost but the cancel says it ended it")
			}
		default:
			t.Fatalf("unexpected final status %s", final.Status)
		}
		again, _ := s.GetBuild(b.ID)
		if again.Status != final.Status || again.EndedAt != final.EndedAt {
			t.Fatalf("an ended build never changes again: %+v then %+v", final, again)
		}
	}
	t.Logf("cancel won %d times, expiry %d times", sawCancelled, sawLost)
}

// The same client id created at once: one record, every caller gets that build.
func TestConcurrentCreatesOfOneClientIDMakeOneBuild(t *testing.T) {
	for _, target := range []string{"remote-box", "nodeA"} {
		for round := 0; round < 15; round++ {
			s, _ := buildSvc(t)
			const callers = 16
			var wg sync.WaitGroup
			ids := make([]string, callers)
			errs := make([]error, callers)
			start := make(chan struct{})
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					b, err := s.CreateBuild(context.Background(), BuildInput{ID: "bld_same0001", Target: target, Source: "ci", Title: "t", Via: BuildViaNodeLease})
					ids[i], errs[i] = b.ID, err
				}(i)
			}
			close(start)
			wg.Wait()
			for i := range ids {
				if errs[i] != nil || ids[i] != "bld_same0001" {
					t.Fatalf("%s round %d caller %d: %q %v", target, round, i, ids[i], errs[i])
				}
			}
			if list, _ := s.ListBuilds(BuildFilter{Limit: -1}); len(list) != 1 {
				t.Fatalf("%s round %d: %d records for one client id", target, round, len(list))
			}
		}
	}
}

// The same client id with different sources at once: exactly one create wins,
// the others are BuildIDTakenError (or the winner's own retry), nothing else.
func TestConcurrentCreatesOfOneClientIDWithOtherSourcesAreRefused(t *testing.T) {
	for round := 0; round < 15; round++ {
		s, _ := buildSvc(t)
		const callers = 10
		var wg sync.WaitGroup
		var ok, taken atomic.Int32
		start := make(chan struct{})
		var mu sync.Mutex
		var other []error
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, err := s.CreateBuild(context.Background(), BuildInput{ID: "bld_race0001", Target: "remote-box", Source: fmt.Sprintf("tool-%d", i), Title: "t"})
				var tk *BuildIDTakenError
				switch {
				case err == nil:
					ok.Add(1)
				case errors.As(err, &tk):
					taken.Add(1)
				default:
					mu.Lock()
					other = append(other, err)
					mu.Unlock()
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if len(other) > 0 {
			t.Fatal(other)
		}
		if ok.Load() != 1 || taken.Load() != callers-1 {
			t.Fatalf("round %d: %d created, %d refused", round, ok.Load(), taken.Load())
		}
		if list, _ := s.ListBuilds(BuildFilter{Limit: -1}); len(list) != 1 {
			t.Fatalf("one record, got %d", len(list))
		}
	}
}
