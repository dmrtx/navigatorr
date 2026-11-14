package action

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// submitTemplate is a conversion whose last step is the real submit: once the
// engine has given the conversion a worker, it sends the job to that worker. A
// requeue clears what the earlier steps worked out (resolved path, plan), so
// the first step works it out again, as the real resolve and plan steps do.
func submitTemplate(e *Engine) ActionTemplate {
	prepare := func(_ context.Context, ec *ExecutionContext) (StepResult, error) {
		for k, v := range testReconcileEC("").State {
			if _, ok := ec.State[k]; !ok {
				ec.State[k] = v
			}
		}
		return StepResult{Status: StepCompleted}, nil
	}
	return ActionTemplate{Name: "transcode_media", Steps: []StepDefinition{{Name: "prepare", Run: prepare}, {Name: "submit", Run: e.stepTranscodeSubmit}}}
}

// turnWith is turn with the given template.
func turnWith(t *testing.T, e *Engine, id string, tmpl ActionTemplate) (worker, condition, reason string) {
	t.Helper()
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil || inst == nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	if _, err := e.execute(context.Background(), inst, parseExecutionContext(inst, e), tmpl); err != nil {
		t.Fatal(err)
	}
	after, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	if w := assignedWorker(actionState(t, e.deps.Store, id)); w != "" {
		return w, "", ""
	}
	return "", after.WaitingCondition, after.WaitingReason
}

// refusingNode makes a node refuse every submit for lack of room until accept
// is set, and counts the submits it got. Like a real worker, a node that
// refused a job has no such job: asking for it answers 404, and lookups counts
// those answers.
func refusingNode(node *roomReporter) (submits, lookups *atomic.Int32, accept *atomic.Bool) {
	submits, lookups, accept = &atomic.Int32{}, &atomic.Int32{}, &atomic.Bool{}
	node.submitFunc = func(_ context.Context, req transcode.Request) (transcode.Job, error) {
		submits.Add(1)
		if accept.Load() {
			return transcode.Job{ID: req.ID}, nil
		}
		return transcode.Job{}, roomRefusalError("worker busy (" + transcode.RoomRefusalTag + "): the job needs 30.0 GB and this node can offer 1.0 GB")
	}
	node.statusFunc = func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		if accept.Load() {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusQueued}, nil
		}
		lookups.Add(1)
		return transcode.JobStatus{}, &httpclient.HTTPError{StatusCode: 404, Code: transcode.WorkerErrorNotFound}
	}
	return submits, lookups, accept
}

// Two real refusals through the requeue, end to end. Both nodes report room
// (a report the refusal shows to be stale), so the coordinator places the
// conversion; each node refuses the submit. The conversion is requeued from m4,
// placed on m1, requeued again, and then reaches no node: it waits without
// holding a slot, so a conversion behind it gets one. Past the window it is
// placed again and sent to a node.
func TestTwoRealRefusalsGoThroughTheRequeueToEachNodeOnce(t *testing.T) {
	m4, m1 := diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib)
	m4Submits, m4Lookups, m4Accept := refusingNode(m4)
	m1Submits, m1Lookups, m1Accept := refusingNode(m1)
	e, clock := placementEngine(t, m4, m1)

	waiter(t, e, "big", 10*time.Minute, map[string]any{"path": "/Volumes/media/ep.mkv"}, map[string]any{SubmittedSourceBytesKey: int64(30 * gib)})
	tmpl := submitTemplate(e)

	// requeuedFrom checks the conversion is back in the queue from node, with
	// nothing of the refused job left on it, and returns its refusals.
	requeuedFrom := func(node string) map[string]time.Time {
		t.Helper()
		st := actionState(t, e.deps.Store, "big")
		if got := getString(st, requeuedFromKey); got != node {
			t.Fatalf("requeued from %q, want %s", got, node)
		}
		if st["job_id"] != nil || st["plan"] != nil || assignedWorker(st) != "" {
			t.Fatalf("the requeue kept the refused job: %v", st)
		}
		return refusalsFromState(st[roomRefusalsKey])
	}

	// m4 is first on a tie: it is picked, refuses, and the conversion is back
	// in the queue with m4's refusal recorded.
	if w, cond, _ := turnWith(t, e, "big", tmpl); w != "" || cond != "worker_busy" {
		t.Fatalf("after m4's refusal: worker %q, wait %q; want it back in the queue", w, cond)
	}
	refusals := requeuedFrom("m4")
	if m4Submits.Load() != 1 || m1Submits.Load() != 0 || refusals["m4"].IsZero() || !refusals["m1"].IsZero() {
		t.Fatalf("after the first turn: submits m4=%d m1=%d, refusals %v", m4Submits.Load(), m1Submits.Load(), refusals)
	}
	// Before moving it, the engine asked m4 for the job and was told it has none.
	if m4Lookups.Load() != 1 {
		t.Fatalf("m4 was asked for the refused job %d times, want once", m4Lookups.Load())
	}
	m4RefusedAt := refusals["m4"]

	// The next turn passes m4 over and sends it to m1, which refuses too.
	if w, cond, _ := turnWith(t, e, "big", tmpl); w != "" || cond != "worker_busy" {
		t.Fatalf("after m1's refusal: worker %q, wait %q; want it back in the queue", w, cond)
	}
	refusals = requeuedFrom("m1")
	if m4Submits.Load() != 1 || m1Submits.Load() != 1 || !refusals["m4"].Equal(m4RefusedAt) || refusals["m1"].IsZero() || m1Lookups.Load() != 1 {
		t.Fatalf("after the second turn: submits m4=%d m1=%d, m1 lookups %d, refusals %v (m4's was %v)", m4Submits.Load(), m1Submits.Load(), m1Lookups.Load(), refusals, m4RefusedAt)
	}

	// Refused by both: it reaches no node and is sent nowhere.
	w, cond, reason := turnWith(t, e, "big", tmpl)
	if w != "" || cond != "worker_busy" || !strings.Contains(reason, "refused it a moment ago") {
		t.Fatalf("refused by both: worker %q, %s %q", w, cond, reason)
	}
	if m4Submits.Load() != 1 || m1Submits.Load() != 1 {
		t.Fatalf("a node that refused a moment ago was sent the job again: m4=%d m1=%d", m4Submits.Load(), m1Submits.Load())
	}

	// It holds no slot: a smaller conversion behind it is placed.
	sizedWaiter(t, e, "small", 5*time.Minute, 10*gib, nil)
	small, _, _ := turn(t, e, "small")
	if small == "" {
		t.Fatal("the conversion behind waits although the refused one reaches no node")
	}

	// Past the window it is placed again, on the node the small one left free,
	// and sent there.
	clock.advance(roomRefusedFor + time.Second)
	m4Accept.Store(true)
	m1Accept.Store(true)
	free, busyNode := map[string]string{"m4": "m1", "m1": "m4"}[small], small
	submits := map[string]*atomic.Int32{"m4": m4Submits, "m1": m1Submits}
	freeBefore, busyBefore := submits[free].Load(), submits[busyNode].Load()
	if w, _, _ := turnWith(t, e, "big", tmpl); w != free {
		t.Fatalf("past the window: given %q, want %s (the free node)", w, free)
	}
	if submits[free].Load() != freeBefore+1 || submits[busyNode].Load() != busyBefore {
		t.Fatalf("past the window the job went to the wrong node: submits m4=%d m1=%d, want one more on %s", m4Submits.Load(), m1Submits.Load(), free)
	}
	if inst, err := e.deps.Store.GetActionInstance("big"); err != nil || inst.Status == StatusFailed {
		t.Fatalf("the conversion ended failed after its refusals: %v %v", inst, err)
	}
}
