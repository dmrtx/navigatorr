package action

import (
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

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

// The mode shown for a held node is the strongest mode among the leases the
// worker reports running, never a record's mode.
func TestHeldNodeModeComesFromTheReportedLeases(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	startBuild(t, e, "m1", "suspend", 0) // a record with mode suspend, no lease
	attach := func() transcode.NodePauseStatus {
		return e.AttachNodeBuilds(map[string]transcode.NodePauseStatus{"m1": {Node: "m1"}})["m1"]
	}
	lease := func(id, mode, started string) transcode.WorkerLease {
		return transcode.WorkerLease{ID: id, Status: "running", Mode: mode, StartedAt: started}
	}
	set := func(ls ...transcode.WorkerLease) { e.workerStateFor("m1").setLeases(ls) }

	if st := attach(); st.Mode != "" || st.Paused {
		t.Fatalf("a record alone sets no mode: %+v", st)
	}
	set(lease("lease1", "graceful", "2026-11-13T10:00:00Z"))
	if st := attach(); st.Mode != "graceful" {
		t.Fatalf("the reported graceful lease wins over the suspend record: %+v", st)
	}
	set(lease("lease1", "graceful", "2026-11-13T10:00:00Z"), lease("lease2", "immediate", "2026-11-13T10:01:00Z"))
	if st := attach(); st.Mode != "immediate" {
		t.Fatalf("immediate over graceful: %+v", st)
	}
	set(lease("lease1", "immediate", "2026-11-13T10:00:00Z"), lease("lease2", "suspend", "2026-11-13T10:01:00Z"))
	if st := attach(); st.Mode != "suspend" {
		t.Fatalf("suspend over immediate: %+v", st)
	}
	set(lease("lease1", "", "2026-11-13T10:00:00Z"))
	if st := attach(); !st.Paused || st.Mode != "" {
		t.Fatalf("an old worker reports no mode: held, mode empty: %+v", st)
	}
	// A lease with no record at all still sets the mode.
	e2, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	e2.workerStateFor("m1").setLeases([]transcode.WorkerLease{lease("lease9", "graceful", "2026-11-13T10:00:00Z")})
	if st := e2.AttachNodeBuilds(map[string]transcode.NodePauseStatus{"m1": {Node: "m1"}})["m1"]; st.Mode != "graceful" || len(st.Builds) != 0 {
		t.Fatalf("%+v", st)
	}
}

// Since is the earliest start among the running leases, whatever their order.
func TestRunningLeasesSinceIsTheEarliestStart(t *testing.T) {
	e, _ := buildEngine(t, newSuspendingM1(transcode.StatusRunning))
	e.workerStateFor("m1").setLeases([]transcode.WorkerLease{
		{ID: "lease2", Status: "running", StartedAt: "2026-11-13T10:05:00Z"},
		{ID: "lease1", Status: "running", StartedAt: "2026-11-13T10:00:00Z"},
		{ID: "lease3", Status: "waiting", StartedAt: "2026-11-13T09:00:00Z"},
	})
	if n, since := e.RunningLeases("m1"); n != 2 || since != "2026-11-13T10:00:00Z" {
		t.Fatalf("n=%d since=%s", n, since)
	}
}
