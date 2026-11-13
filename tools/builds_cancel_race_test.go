package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/mark3labs/mcp-go/server"
)

// build_cancel ends a build through Engine.FinishBuild, the call that holds the
// engine's build lock (the deterministic proof of that lock is
// action.TestBuildFinishWaitsForAHeartbeatInTheGap). This checks the tool end to
// end: heartbeats racing a cancel never add lines once the tool has returned.
func TestHeartbeatRacingBuildCancelAddsNothingAfterItReturns(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "builds.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "m1", Executor: &observedHealthWorker{}, Podcast: true}})
	if err != nil {
		t.Fatal(err)
	}
	engine := action.NewEngine(action.EngineDeps{Store: st, Workers: set, DrainSettle: -1})
	s := server.NewMCPServer("test", "0")
	registerBuilds(s, engine)
	b, err := engine.CreateBuild(context.Background(), action.BuildInput{Target: "laptop", Source: "ci", Title: "unit run"})
	if err != nil {
		t.Fatal(err)
	}

	const workers, perWorker = 4, 40
	var (
		wg       sync.WaitGroup
		accepted atomic.Int32
		start    = make(chan struct{})
		midway   = make(chan struct{})
		once     sync.Once
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < perWorker; i++ {
				_, err := engine.HeartbeatBuild(context.Background(), b.ID, nil, 0, fmt.Sprintf("w%d-%02d", w, i))
				switch {
				case err == nil:
					if accepted.Add(1) == 20 {
						once.Do(func() { close(midway) })
					}
				case errors.Is(err, store.ErrBuildEnded):
				default:
					t.Errorf("heartbeat: %v", err)
				}
			}
		}(w)
	}
	close(start)
	<-midway
	callTool(t, s, "build_cancel", map[string]any{"id": b.ID})
	_, atCancel, _, _ := engine.BuildLog(b.ID)
	wg.Wait()
	_, lines, _, err := engine.BuildLog(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(atCancel) < 20 {
		t.Fatalf("the cancel came before the heartbeats: %d lines", len(atCancel))
	}
	if !reflect.DeepEqual(lines, atCancel) {
		t.Fatalf("lines landed after build_cancel returned: %d then %d", len(atCancel), len(lines))
	}
}
