package action

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// A worker daemon that is restarting refuses the capability read. Nothing has
// been submitted, so the action waits and asks again; it does not fail.
func TestSubmitDefersWhenCapabilityReadCannotReachWorker(t *testing.T) {
	var reads atomic.Int32
	mock := &mockTranscodeExecutor{
		capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
			if reads.Add(1) == 1 {
				return transcode.WorkerCapabilities{}, &httpclient.UncertainError{Op: "capabilities", Err: errors.New("dial tcp: connect: connection refused")}
			}
			return (&mockTranscodeExecutor{}).Capabilities(context.Background())
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	plan := testReconcilePlan()
	plan.AudioMode = "compact"
	ec.State["plan"] = plan

	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepWaitingExternal || res.WaitingCondition != "worker_unreachable" {
		t.Fatalf("unreachable worker failed the action instead of waiting: %+v", res)
	}
	if got := atomic.LoadInt32(&mock.submitCalls); got != 0 {
		t.Fatalf("a job was submitted while capabilities were unknown: %d", got)
	}
	if getBool(ec.State, "transcode_submitted") {
		t.Fatal("deferred step marked the job as submitted")
	}
}

func TestSubmitStillFailsOnDefinitiveCapabilityAnswer(t *testing.T) {
	mock := &mockTranscodeExecutor{
		capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
			return transcode.WorkerCapabilities{}, errors.New("worker returned unsupported protocol version 1 (fail closed)")
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	plan := testReconcilePlan()
	plan.AudioMode = "compact"
	ec.State["plan"] = plan
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil || res.Status != StepFailed {
		t.Fatalf("definitive capability error must fail: %+v %v", res, err)
	}
}
