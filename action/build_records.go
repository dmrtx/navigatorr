package action

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// Build records.
//
// A build is a job another tool runs on a machine (see package nodes, which
// owns the registry: its own database, its API and the builds-pause switch).
// This package never imports nodes and never decides on a build record. What it
// needs of the registry it reads through BuildRecords, wired in main:
//
//   - a node's running records, for the registry's view in the UI and the
//     worker observation;
//   - the per-node "builds paused" switch, shown next to the transcode pause
//     and set by the operator through worker-pause.
//
// The node's worker is the only authority on who occupies the node: it takes
// the lease (see internal/transcodeworker and docs/transcode/BUILD_LEASES.md),
// holds its queue, freezes its encoders, refuses room and expires the lease by
// itself.
//
// Invariants (the full list is in the doc comment of package nodes).
//
//	N1  With a running lease on a node, no new conversion starts on that node.
//	    The worker alone guarantees it (queueHeld and the room it reports); the
//	    server does nothing for it.
//	N2  The transcode engine's dispatch, placement, pause view (workerPauses),
//	    drain and freeze logic never read build records, and this package never
//	    imports nodes. The only readers of BuildRecords are the display methods
//	    in this file and worker_observation.go.
//
// Why an encode is frozen comes from the leases its worker reports (see
// suspendedBy), not from the records.

// BuildRecords is the registry's side that the engine displays: implemented by
// package nodes through an adapter in main. A nil BuildRecords reads as no
// records and no builds pause.
type BuildRecords interface {
	// NodeBuildRecords is the node's running and waiting records, oldest first.
	NodeBuildRecords(node string) []transcode.NodeBuild
	// BuildsPauses is the nodes whose builds are paused, by node name, with the
	// time each pause began.
	BuildsPauses() (map[string]string, error)
	// SetBuildsPause pauses or resumes new builds on a node.
	SetBuildsPause(ctx context.Context, node string, paused bool) error
	// BuildsPaused reports whether new builds on the node are refused.
	BuildsPaused(node string) (bool, error)
}

// ErrBuildRecordsUnavailable is what a BuildRecords returns (wrapped, so test it
// with errors.Is) when the registry cannot be read at all, as a registry that
// runs in another process can be. The engine shows it as "builds registry
// unavailable", never as paused and never as not paused: nothing is known.
// Any other error keeps failing closed (reads as paused).
var ErrBuildRecordsUnavailable = errors.New("builds registry not available")

// recordsBox lets an interface value sit in an atomic pointer.
type recordsBox struct{ r BuildRecords }

// SetBuildRecords wires the registry. main calls it once, before the reconciler
// starts; the field is atomic anyway, so a goroutine the engine started earlier
// reads either none or the registry, never a torn value.
func (e *Engine) SetBuildRecords(r BuildRecords) {
	if r == nil {
		e.records.Store(nil)
		return
	}
	e.records.Store(&recordsBox{r})
}

// buildRecords is the wired registry, nil when there is none.
func (e *Engine) buildRecords() BuildRecords {
	if b := e.records.Load(); b != nil {
		return b.r
	}
	return nil
}

// SetNodeBuildsPause pauses or resumes new builds on a node and returns the
// node's status afterwards. It never touches the transcode pause or any build.
// Repeating a request changes nothing.
func (e *Engine) SetNodeBuildsPause(ctx context.Context, node string, paused bool) (transcode.NodePauseStatus, error) {
	records := e.buildRecords()
	if records == nil {
		return transcode.NodePauseStatus{}, errors.New("the builds registry is not configured")
	}
	w, ok := e.deps.Workers.Get(node)
	if !ok {
		return transcode.NodePauseStatus{}, &transcode.WorkerNotConfiguredError{Name: node}
	}
	if err := records.SetBuildsPause(ctx, w.Name, paused); err != nil {
		return transcode.NodePauseStatus{}, err
	}
	return e.NodePauseStatuses(ctx)[w.Name], nil
}

// NodeBuildsPaused reports whether new builds on the named node are refused. An
// unreadable state reads as paused, as the create path fails closed too, except
// when the registry is unavailable (see NodeBuildsState).
func (e *Engine) NodeBuildsPaused(name string) bool {
	paused, _ := e.NodeBuildsState(name)
	return paused
}

// NodeBuildsState is NodeBuildsPaused with the unavailable flag: unavailable is
// true when the registry could not be read at all (ErrBuildRecordsUnavailable);
// paused is then false, because nothing is known and the screens say so
// instead. Any other read error reads as paused.
func (e *Engine) NodeBuildsState(name string) (paused, unavailable bool) {
	records := e.buildRecords()
	if records == nil {
		return false, false
	}
	paused, err := records.BuildsPaused(e.resolveWorker(name))
	if errors.Is(err, ErrBuildRecordsUnavailable) {
		return false, true
	}
	return paused || err != nil, false
}

// buildsPauses is the registry's paused nodes with the two read failures: the
// state could not be read (reads as paused) and the registry is unavailable
// (reads as neither).
func (e *Engine) buildsPauses() (m map[string]string, unreadable, unavailable bool) {
	records := e.buildRecords()
	if records == nil {
		return nil, false, false
	}
	m, err := records.BuildsPauses()
	if errors.Is(err, ErrBuildRecordsUnavailable) {
		return nil, false, true
	}
	return m, err != nil, false
}

// NodeBuildRecords is a node's running and waiting build records, oldest first,
// for the registry's view of it (the UI). It is a record read only: nothing in
// the engine decides on it.
func (e *Engine) NodeBuildRecords(node string) []transcode.NodeBuild {
	records := e.buildRecords()
	if records == nil {
		return nil
	}
	return records.NodeBuildRecords(node)
}

// RunningLeases is how many build leases the node's worker reports running,
// and when the earliest began (compared as times, not by list order). It reads
// the last worker observation (memory only, up to a few seconds old), never
// the build records: the worker is the only authority on what holds the node,
// the records are history.
func (e *Engine) RunningLeases(node string) (int, string) {
	n, since := 0, ""
	var earliest time.Time
	for _, l := range e.workerLeases(node) {
		if l.Status != "running" {
			continue
		}
		n++
		if l.StartedAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, l.StartedAt)
		if err != nil {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest, since = t, l.StartedAt
		}
	}
	return n, since
}

// runningLeaseMode is the strongest mode among the node's running leases:
// suspend (and its alias immediate) over graceful. Empty when no running
// lease reports a mode (an old worker).
func (e *Engine) runningLeaseMode(node string) string {
	rank := map[string]int{store.WorkerPauseGraceful: 1, store.WorkerPauseImmediate: 2, store.WorkerPauseSuspend: 3}
	mode := ""
	for _, l := range e.workerLeases(node) {
		if l.Status == "running" && rank[l.Mode] > rank[mode] {
			mode = l.Mode
		}
	}
	return mode
}

// AttachNodeBuilds adds each node's build records to its pause status, for the
// callers that show the registry's view next to the pause. The records are a
// list only. "Held by builds" comes solely from the leases the node's worker
// reports running; a running record with no reported lease holds nothing.
func (e *Engine) AttachNodeBuilds(statuses map[string]transcode.NodePauseStatus) map[string]transcode.NodePauseStatus {
	for name, st := range statuses {
		st = e.applyLeaseHold(name, st)
		st.Builds = e.NodeBuildRecords(name)
		statuses[name] = st
	}
	return statuses
}

// applyLeaseHold is the state half of AttachNodeBuilds: it shows a node as
// held by builds when its worker reports running leases, and reads no build
// record. An operator pause wins over the display of a lease.
func (e *Engine) applyLeaseHold(name string, st transcode.NodePauseStatus) transcode.NodePauseStatus {
	if st.HeldBy != "" {
		return st
	}
	if running, since := e.RunningLeases(name); running > 0 {
		st.Paused, st.HeldBy = true, "builds"
		st.Since = since
		st.Mode = e.runningLeaseMode(name)
		st.Message = fmt.Sprintf("Busy with %d running build(s). New conversions wait on this node; podcast jobs still run.", running)
	}
	return st
}

// NodeTranscodeState is what Nodes shows of a node's transcode side.
type NodeTranscodeState struct {
	Node    string
	Paused  bool
	Mode    string
	Since   string
	HeldBy  string
	Message string
}

// NodeTranscodeStates is every configured node's transcode state (paused,
// mode, since, held_by, message) for a Nodes process that runs apart from the
// engine. It reads the stored pauses and the last worker observation only: no
// worker is probed, no network call is made, and neither a build record nor
// the builds pause (both live in the registry) is read, so
// the registry can ask the engine without the engine asking it back. The
// result is a copy, sorted by node. It is the same state AttachNodeBuilds
// shows: an operator pause wins over a running lease.
func (e *Engine) NodeTranscodeStates(ctx context.Context) []NodeTranscodeState {
	stored := e.nodePauseStatuses(ctx, transcode.WorkerObservation{}, false)
	out := make([]NodeTranscodeState, 0, len(stored))
	for name, st := range stored {
		st = e.applyLeaseHold(name, st)
		out = append(out, NodeTranscodeState{Node: name, Paused: st.Paused, Mode: st.Mode, Since: st.Since, HeldBy: st.HeldBy, Message: st.Message})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}
