package action

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// retryingExecutor is a worker that can be asked to publish again.
type retryingExecutor struct {
	*mockTranscodeExecutor
	retryErr error
	retried  []string
}

func (r *retryingExecutor) RetryPublication(_ context.Context, jobID string) error {
	r.retried = append(r.retried, jobID)
	return r.retryErr
}

func publicationStuck(recovering bool) func(context.Context, string) (transcode.JobStatus, error) {
	return func(context.Context, string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: "job-existing", Status: transcode.StatusRunning, FailureClassification: "smb_transport_error", Error: "cannot publish candidate", JobTelemetry: transcode.JobTelemetry{Phase: "publication_pending", RecoveryRequired: !recovering}}, nil
	}
}

func recheckWait(t *testing.T, w *retryingExecutor, decision string) (StepResult, *ExecutionContext) {
	t.Helper()
	e := NewEngine(EngineDeps{Transcode: w})
	ec := testReconcileEC("")
	ec.State["job_id"], ec.Decision = "job-existing", decision
	r, err := e.stepTranscodeWait(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	return r, ec
}

func TestRecheckAsksTheWorkerToPublishAgainWithoutEncoding(t *testing.T) {
	w := &retryingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}}
	// The worker takes the request: its status no longer asks for a person.
	w.statusFunc = publicationStuck(true)
	r, ec := recheckWait(t, w, "recheck")
	if len(w.retried) != 1 || w.retried[0] != "job-existing" || w.submitCalls != 0 {
		t.Fatalf("retries=%v submits=%d", w.retried, w.submitCalls)
	}
	if r.Status != StepWaitingExternal || ec.Decision != "" {
		t.Fatalf("the job should be recovering again: %+v decision=%q", r, ec.Decision)
	}
}

func TestPublicationIsOnlyRetriedWhenAPersonAsks(t *testing.T) {
	w := &retryingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}}
	w.statusFunc = publicationStuck(false)
	r, _ := recheckWait(t, w, "")
	if len(w.retried) != 0 || r.Status != StepWaitingDecision {
		t.Fatalf("a poll must not retry a publication: retries=%v %+v", w.retried, r)
	}
}

func TestRecheckSaysWhyTheWorkerCouldNotRetry(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"older worker": {transcode.ErrPublicationRetryUnsupported, "has not been updated"},
		"unreachable":  {errors.New("connection refused"), "could not be asked to try again: connection refused"},
		"nothing left": {transcode.ErrNothingToRetry, ""},
	} {
		t.Run(name, func(t *testing.T) {
			w := &retryingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}, retryErr: tc.err}
			w.statusFunc = publicationStuck(false)
			r, _ := recheckWait(t, w, "recheck")
			if r.Status != StepWaitingDecision {
				t.Fatalf("the job must keep waiting for a person: %+v", r)
			}
			if tc.want == "" {
				if strings.Contains(r.WaitingReason, "worker") && strings.Contains(r.WaitingReason, "asked") {
					t.Fatalf("unexpected note: %q", r.WaitingReason)
				}
				return
			}
			if !strings.Contains(r.WaitingReason, tc.want) {
				t.Fatalf("reason %q does not say %q", r.WaitingReason, tc.want)
			}
		})
	}
}

func TestRecheckOnAWorkerWithoutRetrySupportJustReadsItsStatus(t *testing.T) {
	mock := &mockTranscodeExecutor{statusFunc: publicationStuck(false)}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("")
	ec.State["job_id"], ec.Decision = "job-existing", "recheck"
	r, err := e.stepTranscodeWait(context.Background(), ec)
	if err != nil || r.Status != StepWaitingDecision || mock.submitCalls != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

// Changing a goal retires the file's attempt and encodes it again. A file whose
// encode finished and only failed to save must say so, and point at Try again.
func TestChangingTheGoalOfAnUnsavedEncodeIsRefusedWithTheWayOut(t *testing.T) {
	child := &store.ActionInstance{ID: "child", WaitingOptionsJSON: `[{"decision":"recheck"},{"decision":"cancel"}]`}
	err := (&Engine{}).retireBatchDecision(context.Background(), child)
	if err == nil || !strings.Contains(err.Error(), "use Try again") || !strings.Contains(err.Error(), "encode it again") {
		t.Fatalf("the refusal should name Try again and why: %v", err)
	}
	// Any other decision still has no review to reject and nothing to cancel.
	child.WaitingOptionsJSON = `[{"decision":"approve"}]`
	if err := (&Engine{}).retireBatchDecision(context.Background(), child); err == nil || strings.Contains(err.Error(), "Try again") {
		t.Fatalf("an unrelated decision must keep its own message: %v", err)
	}
}
