package action

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// Node availability window.
//
// A node can be given a window: an end (until), a margin before it, and the
// media types it takes. Until the node closes (until less the margin) it takes
// a new conversion only when
//
//   - the conversion's media type is one the window allows (a conversion with
//     no media type only where the window lists none), and
//   - when the window has an end, the conversion can be finished before it:
//     now plus the encode time estimated for this node from its own history
//     (encode_samples.go) is not past the closing time. The slowest estimate
//     among the encoder families the node has history for counts, since the
//     family is not known before the plan. With no estimate (no probe of the
//     source yet, or too little history on the node) it does not start there;
//     it can still go to another node.
//
// At the closing time nothing new starts on the node, and it stays closed
// until the operator changes or deletes the window. What is encoding is never
// touched. A window that cannot be read closes every node (fail closed).
//
// The decision lives in placer.placeable (I1 in transcode_queue_claims.go), so
// the queue's claims, the pick and the explanation agree. A conversion that
// only the windows keep off every node it could use waits with the condition
// node_window: it is no contender for a slot (not worker_busy), and the rest
// of its batch goes on.
const windowWaitCondition = "node_window"

// queueProbeKey is where the early check keeps what it probed of the source
// (duration_sec, width, height), so the queue can estimate the encode before
// the conversion has a node.
const queueProbeKey = "queue_probe"

// windowPollDelay is how often a conversion held back by a window looks again.
const windowPollDelay = 15 * time.Second

// workerWindows is the window of each node by name. A store that cannot be
// read gives an error, which callers treat as every node closed.
func (e *Engine) workerWindows() (map[string]store.WorkerWindow, error) {
	if e.deps.Store == nil {
		return nil, nil
	}
	return e.deps.Store.ListWorkerWindows()
}

// workerWindowClosed reports whether a node's window has closed (or cannot be
// read): it then takes nothing new, as if paused.
func (e *Engine) workerWindowClosed(name string) bool {
	wins, err := e.workerWindows()
	if err != nil {
		return true
	}
	w, ok := wins[e.resolveWorker(name)]
	if !ok {
		return false
	}
	closes, has := w.ClosesAt()
	return has && !e.now().Before(closes)
}

// windowShape is what the windows need to know of a conversion. The zero value
// is not checked: a batch item that has no conversion yet is placed as
// fitting any open window.
type windowShape struct {
	check       bool
	mediaType   string
	durationSec float64
	class       string
}

func normalizedMediaType(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// windowShapeOfState is the shape of the conversion in ec: its media type and
// what was probed of its source (the preflight's, else the early check's).
func windowShapeOfState(inputs, state map[string]any) windowShape {
	dur, class := probeOfState(state)
	return windowShape{check: true, mediaType: normalizedMediaType(getString(inputs, "media_type")), durationSec: dur, class: class}
}

// probeOfState reads the source's length and resolution class from the
// preflight's original, else from the early check's queue_probe.
func probeOfState(state map[string]any) (durationSec float64, class string) {
	if o, ok := state["original"].(map[string]any); ok {
		durationSec = getFloat(o, "duration_sec")
		if w, h, ok := strings.Cut(getString(o, "resolution"), "x"); ok {
			wi, _ := strconv.Atoi(w)
			hi, _ := strconv.Atoi(h)
			class = ResolutionClass(wi, hi)
		}
	}
	if durationSec > 0 && class != "" {
		return durationSec, class
	}
	if q, ok := state[queueProbeKey].(map[string]any); ok {
		return getFloat(q, "duration_sec"), ResolutionClass(getInt(q, "width"), getInt(q, "height"))
	}
	return 0, ""
}

// maxEncodeEstimate is the slowest estimate, over the encoder families the
// node has history for, of encoding durationSec of a class file there.
func (e *Engine) maxEncodeEstimate(node, class string, durationSec float64) (float64, bool) {
	samples := e.encodeSamples()
	best, found := 0.0, false
	for _, family := range []string{EncodeFamilyCopy, EncodeFamilyX265, EncodeFamilyHardware} {
		if est, ok := EstimateEncodeSeconds(samples, node, class, family, durationSec); ok && (!found || est > best) {
			best, found = est, true
		}
	}
	return best, found
}

// windowVerdict says whether node's window lets the conversion start there
// now, with the reason when it does not. A node without a window, and a
// conversion that is not checked, always pass.
func (e *Engine) windowVerdict(wins map[string]store.WorkerWindow, werr error, s windowShape, node string) (bool, string) {
	if !s.check {
		return true, ""
	}
	if werr != nil {
		return false, "the availability windows could not be read, so no node takes anything new"
	}
	w, ok := wins[node]
	if !ok {
		return true, ""
	}
	now := e.now()
	closes, hasEnd := w.ClosesAt()
	if hasEnd && !now.Before(closes) {
		return false, "its window closed since " + windowClock(closes, now)
	}
	if len(w.MediaTypes) > 0 && !slices.Contains(w.MediaTypes, s.mediaType) {
		takes := strings.Join(w.MediaTypes, ", ")
		if s.mediaType == "" {
			return false, "its media type is unknown and the window takes only " + takes
		}
		return false, fmt.Sprintf("type %s is not allowed (the window takes only %s)", s.mediaType, takes)
	}
	if !hasEnd {
		return true, ""
	}
	if s.durationSec <= 0 || s.class == "" {
		return false, "no duration estimate yet: its source has not been probed"
	}
	est, ok := e.maxEncodeEstimate(node, s.class, s.durationSec)
	if !ok {
		return false, fmt.Sprintf("no duration estimate: no encode history on this node for %s files", s.class)
	}
	if now.Add(time.Duration(est * float64(time.Second))).After(closes) {
		return false, fmt.Sprintf("the window closes at %s and the estimated %d min encode does not fit", windowClock(closes, now), int(est/60+0.5))
	}
	return true, ""
}

// windowClock words a time as HH:MM UTC, with the date when it is not today.
func windowClock(at, now time.Time) string {
	at, now = at.UTC(), now.UTC()
	if at.YearDay() == now.YearDay() && at.Year() == now.Year() {
		return at.Format("15:04") + " UTC"
	}
	return at.Format("2006-01-02 15:04") + " UTC"
}

// windowAdmits re-checks, as the conversion is about to be sent to its node,
// whether the node's window still takes it.
func (e *Engine) windowAdmits(ec *ExecutionContext) (ok bool, why string, unreadable bool) {
	if ec.ActionName != "transcode_media" {
		return true, "", false
	}
	node := e.instanceWorker(ec.State)
	if node == "" {
		return true, "", false
	}
	wins, err := e.workerWindows()
	if err != nil {
		return false, "the availability windows could not be read, so no node takes anything new", true
	}
	ok, why = e.windowVerdict(wins, nil, windowShapeOfState(ec.Inputs, ec.State), node)
	return ok, why, false
}

// windowBlockedResult is what a submit returns when the node's window no
// longer takes the conversion: it goes back to the transcode queue (not
// failed) to be placed again, as when the node is paused. A window that cannot
// be read only defers it.
func (e *Engine) windowBlockedResult(ec *ExecutionContext, node, why string, unreadable bool) StepResult {
	reason := fmt.Sprintf("Node %s: %s", node, why)
	if unreadable {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: windowWaitCondition, WaitingReason: reason}
	}
	ec.State[requeueWhyKey] = reason
	return StepResult{Status: StepWaitingExternal, WaitingCondition: windowWaitCondition, WaitingReason: reason, Requeue: true}
}

// SetNodeWindow writes a node's availability window (expected, when set, is the
// revision it must be at) and wakes the queue so conversions it held back look
// again at once. It returns the stored window.
func (e *Engine) SetNodeWindow(w store.WorkerWindow, expected *int64) (store.WorkerWindow, error) {
	if e.deps.Store == nil {
		return store.WorkerWindow{}, errors.New("store is required for worker windows")
	}
	node, ok := e.deps.Workers.Get(w.Worker)
	if !ok {
		return store.WorkerWindow{}, &transcode.WorkerNotConfiguredError{Name: w.Worker}
	}
	w.Worker = node.Name
	out, err := e.deps.Store.SetWorkerWindow(w, expected)
	if err != nil {
		return store.WorkerWindow{}, err
	}
	e.noteWindowChanged()
	return out, nil
}

// DeleteNodeWindow removes a node's window and wakes the queue. It reports
// whether the node had one.
func (e *Engine) DeleteNodeWindow(name string, expected *int64) (bool, error) {
	if e.deps.Store == nil {
		return false, errors.New("store is required for worker windows")
	}
	node, ok := e.deps.Workers.Get(name)
	if !ok {
		return false, &transcode.WorkerNotConfiguredError{Name: name}
	}
	had, err := e.deps.Store.DeleteWorkerWindow(node.Name, expected)
	if err != nil {
		return false, err
	}
	e.noteWindowChanged()
	return had, nil
}

func (e *Engine) noteWindowChanged() {
	e.windowChangedAt.Store(e.now().UnixNano())
	e.WakeReconciler()
}

// NodeWindowStatus is a window as the API reports it.
type NodeWindowStatus struct {
	store.WorkerWindow
	ClosesAt *time.Time
	Closed   bool
}

// NodeWindows lists every node's window with its closing time and state.
func (e *Engine) NodeWindows() ([]NodeWindowStatus, error) {
	wins, err := e.workerWindows()
	if err != nil {
		return nil, err
	}
	var out []NodeWindowStatus
	for _, node := range e.deps.Workers.All() {
		if w, ok := wins[node.Name]; ok {
			out = append(out, e.windowStatus(w))
		}
	}
	return out, nil
}

// NodeWindow is one node's window; ok is false when it has none.
func (e *Engine) NodeWindow(name string) (NodeWindowStatus, bool, error) {
	node, ok := e.deps.Workers.Get(name)
	if !ok {
		return NodeWindowStatus{}, false, &transcode.WorkerNotConfiguredError{Name: name}
	}
	if e.deps.Store == nil {
		return NodeWindowStatus{}, false, nil
	}
	w, found, err := e.deps.Store.GetWorkerWindow(node.Name)
	if err != nil || !found {
		return NodeWindowStatus{}, false, err
	}
	return e.windowStatus(w), true, nil
}

func (e *Engine) windowStatus(w store.WorkerWindow) NodeWindowStatus {
	st := NodeWindowStatus{WorkerWindow: w}
	if closes, ok := w.ClosesAt(); ok {
		st.ClosesAt = &closes
		st.Closed = !e.now().Before(closes)
	}
	return st
}
