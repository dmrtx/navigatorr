package action

import (
	"context"
	"reflect"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// NodePauseStatesStored is nodePauseStatuses with an empty observation. The
// observation only feeds Reachable (and the messages that depend on it), so the
// pause mode, the paused flags and the build list must not change with it. The
// read also has no side effects: it writes nothing to the store.
func TestNodePauseStatesStoredEqualsObservedState(t *testing.T) {
	ctx := context.Background()
	observed := transcode.WorkerObservation{Configured: true, Reachable: true, Ready: true, Workers: []transcode.WorkerObservation{
		{Name: "m1", Configured: true, Reachable: true, Ready: true},
		{Name: "m4", Configured: true, Reachable: true, Ready: true},
	}}
	cases := []struct {
		name  string
		apply func(t *testing.T, e *Engine)
	}{
		{"off", func(t *testing.T, e *Engine) {}},
		{"graceful", func(t *testing.T, e *Engine) { pauseNode(t, e, "m1", store.WorkerPauseGraceful) }},
		{"immediate", func(t *testing.T, e *Engine) { pauseNode(t, e, "m1", store.WorkerPauseImmediate) }},
		{"suspend", func(t *testing.T, e *Engine) { pauseNode(t, e, "m1", store.WorkerPauseSuspend) }},
		{"builds only", func(t *testing.T, e *Engine) {
			if _, err := e.SetNodeBuildsPause(ctx, "m1", true); err != nil {
				t.Fatal(err)
			}
		}},
		{"running build and an encode", func(t *testing.T, e *Engine) {
			seedEncoding(t, e, "enc", "m1", "job-enc")
			startBuild(t, e, "m4", "", 0)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m1 := newSuspendingM1(transcode.StatusRunning)
			m1.active = 1
			e := pauseEngine(t, m1, &mockTranscodeExecutor{})
			c.apply(t, e)

			snapshot := func() [3]any {
				pauses, _ := e.deps.Store.ListWorkerPauses()
				builds, _ := e.deps.Store.ListBuildPauses()
				list, _ := e.deps.Store.ListBuilds(store.BuildFilter{})
				return [3]any{pauses, builds, list}
			}
			before := snapshot()
			stored := e.NodePauseStatesStored(ctx)
			withObs := e.nodePauseStatuses(ctx, observed)
			if !reflect.DeepEqual(before, snapshot()) {
				t.Fatal("reading the state wrote to the store")
			}
			if len(stored) != 2 || len(withObs) != 2 {
				t.Fatal("both nodes are reported", stored, withObs)
			}
			for name, want := range withObs {
				got := stored[name]
				if got.Mode != want.Mode || got.Paused != want.Paused || got.BuildsPaused != want.BuildsPaused || !reflect.DeepEqual(got.Builds, want.Builds) {
					t.Fatalf("%s differs: stored %+v, observed %+v", name, got, want)
				}
			}
		})
	}
}
