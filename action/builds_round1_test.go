package action

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// A waiting build left by an earlier server is cancelled on reconcile, with a
// reason and a log line, and leaves the node's records.
func TestReconcileCancelsALegacyWaitingBuildWithAReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clock := &buildClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{}, Workers: twoWorkers(t, newSuspendingM1(transcode.StatusRunning), &mockTranscodeExecutor{}, 0), DrainSettle: -1, Now: clock.now})

	running := startBuild(t, e, "m1", "", 0)
	legacy := startBuild(t, e, "m1", "", 0)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if _, err := raw.Exec(`UPDATE builds SET status='waiting', started_at='' WHERE id=?`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.NodeBuildRecords("m1"); len(got) != 2 {
		t.Fatalf("the legacy waiting row is listed before the sweep: %+v", got)
	}

	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	e.reconcileBuilds(context.Background())

	got, err := e.GetBuild(legacy.ID)
	if err != nil || got.Status != store.BuildCancelled || got.EndedAt == "" || got.Progress != store.LegacyWaitingReason {
		t.Fatalf("the waiting build is cancelled with its reason: %+v %v", got, err)
	}
	if !strings.Contains(logs.String(), legacy.ID) || !strings.Contains(logs.String(), store.LegacyWaitingReason) {
		t.Fatalf("the cancel is logged with the build and the reason: %q", logs.String())
	}
	if r, _ := e.GetBuild(running.ID); r.Status != store.BuildRunning {
		t.Fatalf("a running build is untouched: %+v", r)
	}
	if recs := e.NodeBuildRecords("m1"); len(recs) != 1 || recs[0].ID != running.ID {
		t.Fatalf("the cancelled build leaves the node's records: %+v", recs)
	}
}

// What holds a node is the lease its worker reports running. A running record
// with no lease, or a waiting lease, holds nothing; an operator pause wins.
func TestHeldByBuildsComesOnlyFromRunningLeasesTheWorkerReports(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	startBuild(t, e, "m1", "", 0)
	attach := func(held string) transcode.NodePauseStatus {
		in := map[string]transcode.NodePauseStatus{"m1": {Node: "m1", HeldBy: held, Paused: held != ""}}
		return e.AttachNodeBuilds(in)["m1"]
	}

	if st := attach(""); st.Paused || st.HeldBy != "" || len(st.Builds) != 1 {
		t.Fatalf("a running record with no reported lease holds nothing, but is listed: %+v", st)
	}
	e.workerStateFor("m1").setLeases([]transcode.WorkerLease{{ID: "lease1", Status: "waiting"}})
	if st := attach(""); st.Paused || st.HeldBy != "" {
		t.Fatalf("a waiting lease does not hold the node: %+v", st)
	}
	e.workerStateFor("m1").setLeases([]transcode.WorkerLease{{ID: "lease1", Status: "running", StartedAt: "2026-11-13T10:00:00Z"}})
	if st := attach(""); !st.Paused || st.HeldBy != "builds" || st.Since != "2026-11-13T10:00:00Z" || st.Message == "" {
		t.Fatalf("a running lease the worker reports holds the node: %+v", st)
	}
	if st := attach("operator"); st.HeldBy != "operator" {
		t.Fatalf("the operator's pause wins: %+v", st)
	}
	e.workerStateFor("m1").setLeases(nil)
	if st := attach(""); st.Paused || st.HeldBy != "" || len(st.Builds) != 1 {
		t.Fatalf("the worker reports no lease: the record alone holds nothing: %+v", st)
	}
}
