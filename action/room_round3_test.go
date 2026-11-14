package action

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// D. A conversion is failed as too big only from fresh reports and only once
// its plan's exact reserve is recorded.
func TestTooBigForEveryNodeNeedsFreshReportsAndTheExactReserve(t *testing.T) {
	// Fresh reports, exact reserve: failed.
	e := retentionWorkers(t, diskNode(400*gib, 20*gib, 300*gib), diskNode(600*gib, 20*gib, 500*gib))
	if _, res, waiting := convert(t, e, "conv-fresh", 500*gib, ""); !waiting || res.Status != StepFailed {
		t.Fatalf("fresh reports and an exact reserve: %+v (waiting %v), want it failed", res, waiting)
	}

	// An old report may describe a disk that has changed: the conversion waits.
	old := diskNode(400*gib, 20*gib, 300*gib)
	old.age = 2 * transcode.RetentionRoomMaxAge
	e = retentionWorkers(t, old, diskNode(600*gib, 20*gib, 500*gib))
	if _, res, waiting := convert(t, e, "conv-old", 500*gib, ""); res.Status == StepFailed {
		t.Fatalf("an old report failed a conversion: %+v (waiting %v)", res, waiting)
	}

	// A node that reports nothing could take it.
	silent := diskNode(400*gib, 20*gib, 300*gib)
	silent.silent = true
	e = retentionWorkers(t, silent, diskNode(600*gib, 20*gib, 500*gib))
	if _, res, _ := convert(t, e, "conv-silent", 500*gib, ""); res.Status == StepFailed {
		t.Fatalf("a node with no report failed a conversion: %+v", res)
	}

	// Before the plan's reserve is recorded the estimate (half the source) never
	// fails a conversion: a size goal's budget may be far smaller.
	e = retentionWorkers(t, diskNode(400*gib, 20*gib, 300*gib), diskNode(600*gib, 20*gib, 500*gib))
	seedActionInstance(t, e.deps.Store, "conv-estimate", "transcode_media", StatusRunning, 0, "", map[string]any{},
		map[string]any{SubmittedSourceBytesKey: 500 * gib})
	if _, res, waiting := assignAgain(t, e, "conv-estimate"); res.Status == StepFailed {
		t.Fatalf("an estimated reserve failed a conversion: %+v (waiting %v)", res, waiting)
	}
}

// H. One failure counts once, however many polls see it; a second failure of a
// later job counts again.
func TestARunnerKilledFailureIsCountedOnceHoweverOftenItIsPolled(t *testing.T) {
	killed := transcode.JobStatus{FailureClassification: runnerKilledClass, ReasonCode: "runner_killed", Error: "process terminated unexpectedly"}
	first, ec := failedPoll(t, nil, killed)
	if !first.Requeue || getInt(ec.State, runnerKilledKey) != 1 {
		t.Fatalf("first poll: %+v count %d", first, getInt(ec.State, runnerKilledKey))
	}
	// The requeue did not move it (the node could not be told): the next poll
	// sees the same failure and must not count it again.
	for range 4 {
		again, ec2 := failedPoll(t, map[string]any{runnerKilledKey: 1, runnerKilledJobKey: "job-existing"}, killed)
		if !again.Requeue || getInt(ec2.State, runnerKilledKey) != 1 {
			t.Fatalf("a repeated poll: %+v count %d, want a requeue counted once", again, getInt(ec2.State, runnerKilledKey))
		}
	}
	// A failure of another job is a new one.
	next, ec3 := failedPoll(t, map[string]any{runnerKilledKey: 1, runnerKilledJobKey: "job-before"}, killed)
	if !next.Requeue || getInt(ec3.State, runnerKilledKey) != 2 {
		t.Fatalf("a second job's failure: %+v count %d, want 2", next, getInt(ec3.State, runnerKilledKey))
	}
}

// I. The node that refused a conversion is passed over only within the window;
// afterwards the preference for the node it came from applies again.
func TestTheNodeThatRefusedAConversionIsPreferredAgainAfterTheWindow(t *testing.T) {
	e := retentionWorkers(t, diskNode(1000*gib, 10*gib, 500*gib), diskNode(1000*gib, 10*gib, 500*gib))
	stale := e.now().Add(-roomRefusedFor - time.Second).Format(time.RFC3339Nano)
	seedActionInstance(t, e.deps.Store, "conv-a", "transcode_media", StatusRunning, 0, "", map[string]any{},
		map[string]any{SubmittedSourceBytesKey: 50 * gib, requeuedFromKey: "m1", requeueWhyKey: "no room", roomRefusalsKey: map[string]any{"m1": stale}})
	if name, _, waiting := assignAgain(t, e, "conv-a"); waiting || name != "m1" {
		t.Fatalf("went to %q (waiting %v), want m1 again once its refusal is old", name, waiting)
	}
}

// J. The only worker that just refused a conversion is not asked again at
// once: the conversion waits out the window instead of looping.
func TestTheOnlyWorkerThatJustRefusedAConversionIsNotAskedAgainAtOnce(t *testing.T) {
	set, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "solo", Executor: newRoomReporter(0, 0), MaxParallelJobs: 1, Podcast: true}})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineDeps{Store: setupTestStore(t), Config: &config.Config{}, Workers: set})
	seedActionInstance(t, e.deps.Store, "conv-a", "transcode_media", StatusRunning, 0, "", map[string]any{},
		map[string]any{SubmittedSourceBytesKey: gib, roomRefusalsKey: map[string]any{"solo": e.now().Format(time.RFC3339Nano)}})
	name, res, waiting := assignAgain(t, e, "conv-a")
	if !waiting || name != "" || res.WaitingCondition != "worker_busy" {
		t.Fatalf("assigned %q (waiting %v) %+v, want it to wait", name, waiting, res)
	}
	seedActionInstance(t, e.deps.Store, "conv-b", "transcode_media", StatusRunning, 0, "", map[string]any{},
		map[string]any{SubmittedSourceBytesKey: gib, roomRefusalsKey: map[string]any{"solo": e.now().Add(-roomRefusedFor - time.Second).Format(time.RFC3339Nano)}})
	if name, _, waiting := assignAgain(t, e, "conv-b"); waiting || name != "solo" {
		t.Fatalf("assigned %q (waiting %v), want solo once the window has passed", name, waiting)
	}
}

func roomRefusalError(msg string) error {
	return &httpclient.HTTPError{Method: "POST", URL: "/v1/jobs", StatusCode: 429, Message: msg, Code: transcode.WorkerErrorBusy, Retryable: true}
}

// L. A submit the node refuses for lack of room puts the conversion back in the
// queue, whatever words the worker's version used; the same answer to any other
// action is an ordinary busy wait.
func TestARoomRefusalAtSubmitPutsTheConversionBackInTheQueue(t *testing.T) {
	for name, msg := range map[string]string{
		"current worker": "worker busy (" + transcode.RoomRefusalTag + "): the job needs 2.0 GB and this node can offer 1.0 GB",
		"old budget":     "worker busy: retained candidates would use 9 of 8 bytes",
		"old floor":      "worker busy: free bytes would fall below the floor",
	} {
		t.Run(name, func(t *testing.T) {
			mock := &mockTranscodeExecutor{submitFunc: func(context.Context, transcode.Request) (transcode.Job, error) {
				return transcode.Job{}, roomRefusalError(msg)
			}}
			e := NewEngine(EngineDeps{Transcode: mock})
			ec := testReconcileEC("job-test-inst")
			ec.ActionName = "transcode_media"
			res, err := e.stepTranscodeSubmit(context.Background(), ec)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Requeue || res.Status != StepWaitingExternal || getString(ec.State, requeueWhyKey) == "" || len(refusalsFromState(ec.State[roomRefusalsKey])) == 0 {
				t.Fatalf("result %+v state %v, want a requeue that says why", res, ec.State)
			}
		})
	}
	mock := &mockTranscodeExecutor{submitFunc: func(context.Context, transcode.Request) (transcode.Job, error) {
		return transcode.Job{}, roomRefusalError("worker busy (" + transcode.RoomRefusalTag + "): no")
	}}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("job-test-inst")
	ec.ActionName = "something_else"
	res, _ := e.stepTranscodeSubmit(context.Background(), ec)
	if res.Requeue {
		t.Fatalf("an action that is not a conversion was requeued: %+v", res)
	}
}

func TestIsRoomRefusalRecognisesOldAndNewWorkers(t *testing.T) {
	for msg, want := range map[string]bool{
		"worker busy (" + transcode.RoomRefusalTag + "): x":      true,
		"worker busy: retained candidates would use 9 of 8":      true,
		"worker busy: free bytes would fall below the floor":     true,
		"worker busy: maximum parallel jobs (2) reached":         false,
		"retained candidates would use 9 of 8 (not a busy node)": false,
	} {
		if got := transcode.IsRoomRefusal(errors.New(msg)); got != want {
			t.Errorf("IsRoomRefusal(%q) = %v, want %v", msg, got, want)
		}
	}
}
