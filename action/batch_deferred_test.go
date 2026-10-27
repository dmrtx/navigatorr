package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// Deferred publication for the children of a batch (see
// docs/transcode/DEFERRED_CANDIDATE_PUBLICATION.md, "Batches").

// --- admission --------------------------------------------------------------

func TestABatchChildDefersLikeAnyOtherAttempt(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	child := &ExecutionContext{InstanceID: "x", Inputs: map[string]any{"parent_action_id": "batch"}, State: map[string]any{}, Outputs: map[string]any{}}
	policy, held := e.publishPolicyFor(context.Background(), child)
	if policy != transcode.PublishPolicyOnApproval || held != nil || getString(child.State, publishPolicyKey) != transcode.PublishPolicyOnApproval {
		t.Fatalf("a batch child must defer: %q %v %v", policy, held, child.State)
	}
	if _, ok := child.State[publishPolicyFallbackKey]; ok {
		t.Fatalf("a deferred child recorded a fallback: %v", child.State)
	}
}

func TestABatchChildOnAWorkerThatPublishesOnEncodeIsUnchanged(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, "")
	child := &ExecutionContext{InstanceID: "x", Inputs: map[string]any{"parent_action_id": "batch"}, State: map[string]any{}, Outputs: map[string]any{}}
	if policy, held := e.publishPolicyFor(context.Background(), child); policy != "" || held != nil || len(child.State) != 0 {
		t.Fatalf("%q %v %v", policy, held, child.State)
	}
}

// batchChildSubmit runs the submit step of a batch child against a worker that
// answers the first submits with the given errors.
func batchChildSubmit(t *testing.T, refusals ...error) (*ExecutionContext, StepResult, *retainedExecutor, *Engine, *store.Store) {
	t.Helper()
	st := setupTestStore(t)
	seedBatchParent(t, st, "batch-parent", StatusWaitingExternal, map[string]any{})
	state := submitStepState()
	seedActionInstance(t, st, "batch-child", "transcode_media", StatusRunning, 3, "", map[string]any{"path": "/Volumes/media/ep.mkv", "parent_action_id": "batch-parent"}, state)
	w := newRetainedExecutor()
	var sent []transcode.Request
	w.submitFunc = func(_ context.Context, req transcode.Request) (transcode.Job, error) {
		sent = append(sent, req)
		if len(sent) <= len(refusals) {
			return transcode.Job{}, refusals[len(sent)-1]
		}
		return transcode.Job{ID: req.ID}, nil
	}
	e := NewEngine(EngineDeps{Store: st, Transcode: w})
	workers, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: transcode.DefaultWorkerName, Executor: w, Podcast: true, PublishPolicy: transcode.PublishPolicyOnApproval}})
	if err != nil {
		t.Fatal(err)
	}
	e.deps.Workers = workers
	ec := childECFor("batch-child", "batch-parent", state)
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	return ec, res, w, e, st
}

func retentionBusy(msg string) error {
	return &httpclient.HTTPError{Method: "POST", URL: "/v1/jobs", StatusCode: 429, Code: transcode.WorkerErrorBusy, Message: "worker busy: " + msg, Retryable: true}
}

func TestABatchChildAcceptedByTheWorkerIsRetained(t *testing.T) {
	ec, res, w, _, _ := batchChildSubmit(t)
	if res.Status != StepCompleted {
		t.Fatalf("submit: %+v", res)
	}
	if reqs := w.requests; len(reqs) != 1 || reqs[0].PublishPolicy != transcode.PublishPolicyOnApproval {
		t.Fatalf("the job was not asked to be retained: %+v", reqs)
	}
	if getString(ec.State, publishPolicyKey) != transcode.PublishPolicyOnApproval {
		t.Fatalf("state: %v", ec.State)
	}
}

// A batch cannot wait for room: nothing in it is approved before every child
// has encoded, so the candidates that fill the worker would never leave.
func TestABatchChildTheWorkerHasNoRoomToRetainPublishesOnEncode(t *testing.T) {
	ec, res, w, e, _ := batchChildSubmit(t, retentionBusy(fmt.Sprintf("%s 90 of 80 allowed bytes", transcode.RetentionRefusalBudget)))
	if res.Status != StepCompleted {
		t.Fatalf("the child waited instead of publishing on encode: %+v", res)
	}
	if len(w.requests) != 2 || w.requests[0].PublishPolicy != transcode.PublishPolicyOnApproval || w.requests[1].PublishPolicy != "" {
		t.Fatalf("requests: %+v", w.requests)
	}
	if w.requests[0].ID != w.requests[1].ID || w.requests[0].IdempotencyKey != w.requests[1].IdempotencyKey {
		t.Fatalf("the second submit must name the same job: %+v", w.requests)
	}
	if _, ok := ec.State[publishPolicyKey]; ok || getString(ec.State, publishPolicyFallbackKey) != fallbackRetentionFull {
		t.Fatalf("state: %v", ec.State)
	}
	// The decision holds for later submits of the same attempt (a reconcile
	// that finds no job), and the engine does not defer it again.
	if policy, held := e.publishPolicyFor(context.Background(), ec); policy != "" || held != nil {
		t.Fatalf("a spilled child deferred again: %q %v", policy, held)
	}
}

// The disk floor is not a lack of room to retain: a job that publishes on
// encode needs that disk too, so the child waits like any busy submit.
func TestABatchChildWaitsWhenTheDiskFloorRefusesIt(t *testing.T) {
	ec, res, w, _, _ := batchChildSubmit(t, retentionBusy(fmt.Sprintf("10 %s the 50 byte minimum", transcode.RetentionRefusalDisk)))
	if res.Status != StepWaitingExternal || len(w.requests) != 1 {
		t.Fatalf("%+v requests=%d", res, len(w.requests))
	}
	if _, ok := ec.State[publishPolicyFallbackKey]; ok || getString(ec.State, publishPolicyKey) != transcode.PublishPolicyOnApproval {
		t.Fatalf("a disk refusal changed the policy: %v", ec.State)
	}
}

func TestOnlyABatchChildSpillsAndOnlyForARetentionRefusal(t *testing.T) {
	deferred := func() *transcode.Request { return &transcode.Request{PublishPolicy: transcode.PublishPolicyOnApproval} }
	child := func() *ExecutionContext {
		return &ExecutionContext{InstanceID: "c", Inputs: map[string]any{"parent_action_id": "batch"}, State: map[string]any{publishPolicyKey: transcode.PublishPolicyOnApproval}, Outputs: map[string]any{}}
	}
	room := retentionBusy(transcode.RetentionRefusalBudget + " 9 of 8 allowed bytes")
	slots := retentionBusy("max parallel jobs reached")

	if req := deferred(); !spillRetentionRefusal(child(), req, room) || req.PublishPolicy != "" {
		t.Fatalf("a batch child must spill: %+v", req)
	}
	standalone := child()
	standalone.Inputs = map[string]any{}
	if req := deferred(); spillRetentionRefusal(standalone, req, room) || req.PublishPolicy == "" {
		t.Fatal("a standalone attempt waits for room, it does not spill")
	}
	if req := deferred(); spillRetentionRefusal(child(), req, slots) || req.PublishPolicy == "" {
		t.Fatal("a refusal for slots is not a refusal for room")
	}
	if req := deferred(); spillRetentionRefusal(child(), req, errors.New("connection refused")) {
		t.Fatal("a transport error is not a refusal")
	}
	if req := (&transcode.Request{}); spillRetentionRefusal(child(), req, room) {
		t.Fatal("a job that was not deferred has nothing to spill")
	}
}

// A refusal on both submits is an ordinary busy answer: the child waits.
func TestABatchChildStillWaitsWhenEvenPublishOnEncodeIsRefused(t *testing.T) {
	ec, res, w, e, _ := batchChildSubmit(t,
		retentionBusy(transcode.RetentionRefusalBudget+" 9 of 8 allowed bytes"),
		retentionBusy("max parallel jobs reached"))
	if res.Status != StepWaitingExternal || res.WaitingCondition != "worker_busy" || len(w.requests) != 2 {
		t.Fatalf("%+v requests=%d", res, len(w.requests))
	}
	// The next submit goes straight out without a policy: the refusal that
	// spilled it is remembered.
	if getString(ec.State, publishPolicyFallbackKey) != fallbackRetentionFull {
		t.Fatalf("state: %v", ec.State)
	}
	if policy, held := e.publishPolicyFor(context.Background(), ec); policy != "" || held != nil {
		t.Fatalf("the next submit deferred again: %q %v", policy, held)
	}
}

// --- the batch's promotion --------------------------------------------------

// retainedBatch is a batch of one finished child whose candidate is still on its
// worker, with the replacement plan ready for review.
func retainedBatch(t *testing.T) (*retainedPromotion, string) {
	t.Helper()
	rp := newRetainedPromotion(t)
	source, err := rp.st.GetActionInstance("source-transcode")
	if err != nil {
		t.Fatal(err)
	}
	source.OutputsJSON = toJSON(map[string]any{"candidate_path": rp.candidate, "original_intact": true})
	batchID := "retained-batch"
	rp.makeBatchChild(source, batchID)
	if err := rp.st.UpdateActionInstance(*source); err != nil {
		t.Fatal(err)
	}
	if err := rp.st.CreateActionInstance(store.ActionInstance{ID: batchID, ActionName: "transcode_batch", Status: StatusWaitingExternal, CurrentStep: 2, InputsJSON: `{"service":"sonarr","series_id":1,"promote_candidates":true}`, StateJSON: "{}", OutputsJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := rp.st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: batchID, ItemKey: "epfile-101", FilePath: rp.original, EpisodeInfo: "S01E01-E02", Decision: "transcode", Status: "completed", ChildActionID: "source-transcode", CandidatePath: rp.candidate}); err != nil {
		t.Fatal(err)
	}
	return rp, batchID
}

// makeBatchChild makes the source attempt a child of the batch, as the
// attempts of a real batch are: that is what sends its release and its
// leftover listing through the batch's plan.
func (rp *retainedPromotion) makeBatchChild(source *store.ActionInstance, batchID string) {
	rp.t.Helper()
	var inputs map[string]any
	if err := json.Unmarshal([]byte(source.InputsJSON), &inputs); err != nil {
		rp.t.Fatal(err)
	}
	inputs["parent_action_id"] = batchID
	source.InputsJSON = toJSON(inputs)
}

func (rp *retainedPromotion) resumeBatch(batchID, decision string) *ActionResult {
	rp.t.Helper()
	r, err := rp.engine.Resume(context.Background(), batchID, decision, nil)
	if err != nil {
		rp.t.Fatal(err)
	}
	return r
}

// settleBatch resumes the batch until it stops waiting on its promotions.
func (rp *retainedPromotion) settleBatch(batchID string, r *ActionResult) *ActionResult {
	rp.t.Helper()
	for i := 0; i < 20 && r.Status == StatusWaitingExternal; i++ {
		r = rp.resumeBatch(batchID, "")
	}
	return r
}

func TestBatchPlanOfARetainedCandidateReadsNoShareFile(t *testing.T) {
	rp, batchID := retainedBatch(t)
	r := rp.resumeBatch(batchID, "")
	if r.Status != StatusWaitingDecision {
		t.Fatalf("the batch should wait for one approval: %+v", r)
	}
	plan, err := rp.engine.BatchPromotionPlan(context.Background(), batchID)
	if err != nil || plan == nil || len(plan.Members) != 1 || plan.Members[0].CandidateSHA != rp.sha {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if len(rp.w.published) != 0 {
		t.Fatal("planning asked the worker to upload")
	}
	if _, err := os.Lstat(rp.candidate); !os.IsNotExist(err) {
		t.Fatal("planning put a candidate on the share")
	}
	if rp.imports != 0 {
		t.Fatalf("planning imported: %d", rp.imports)
	}
}

func TestOneBatchApprovalUploadsAndPromotesRetainedCandidates(t *testing.T) {
	rp, batchID := retainedBatch(t)
	originalSeenAtUpload := false
	rp.onPublish = func() {
		_, err := os.Stat(rp.original)
		originalSeenAtUpload = err == nil
	}
	if r := rp.resumeBatch(batchID, ""); r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	if len(rp.w.published) != 0 {
		t.Fatal("the worker was asked to upload before approval")
	}
	r := rp.settleBatch(batchID, rp.resumeBatch(batchID, "approve"))
	if r.Status != StatusCompleted || rp.imports != 1 || rp.deletes != 1 {
		t.Fatalf("batch: status=%s error=%s imports=%d deletes=%d", r.Status, r.Error, rp.imports, rp.deletes)
	}
	if len(rp.w.published) == 0 || rp.w.published[0] != "job-source" || !originalSeenAtUpload {
		t.Fatalf("upload requests %v; original untouched at upload: %v", rp.w.published, originalSeenAtUpload)
	}
	if got := getString(rp.sourceState(), candidateLocationKey); got != candidateOnShare {
		t.Fatalf("the source still says the candidate is %q", got)
	}
	if got := rp.w.releases(); len(got) != 0 {
		t.Fatalf("a promoted candidate was released: %v", got)
	}
}

// The worker restarts while the batch is uploading: its answer is lost, the
// job comes back retained, and the same batch approval finishes the upload.
func TestBatchPromotionResumesAfterTheWorkerRestarts(t *testing.T) {
	rp, batchID := retainedBatch(t)
	down := true
	publish := rp.w.publish
	rp.w.publish = func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
		if down {
			return transcode.JobStatus{}, &httpclient.UncertainError{Op: "publish", JobID: jobID, Err: errors.New("connection reset")}
		}
		return publish(ctx, jobID)
	}
	if r := rp.resumeBatch(batchID, ""); r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	r := rp.resumeBatch(batchID, "approve")
	for i := 0; i < 3; i++ {
		r = rp.resumeBatch(batchID, "")
	}
	if r.Status != StatusWaitingExternal || rp.imports != 0 {
		t.Fatalf("an unreachable worker must leave the batch waiting: %s %s imports=%d", r.Status, r.Error, rp.imports)
	}
	if _, err := os.Stat(rp.original); err != nil {
		t.Fatalf("the original was touched while the upload was unknown: %v", err)
	}
	if got := rp.w.releases(); len(got) != 0 {
		t.Fatalf("an unreachable worker made the engine release: %v", got)
	}

	// The worker is back with the job still retained, and a new coordinator
	// takes over from the stored state.
	down = false
	rp.restart()
	r = rp.settleBatch(batchID, rp.resumeBatch(batchID, ""))
	if r.Status != StatusCompleted || rp.imports != 1 {
		t.Fatalf("batch after the restart: status=%s error=%s imports=%d", r.Status, r.Error, rp.imports)
	}
}

// A retained candidate the worker dropped fails only its own member, says to
// encode the file again, and leaves the original alone.
func TestBatchPromotionOfAnExpiredCandidateFailsItsMemberOnly(t *testing.T) {
	rp, batchID := retainedBatch(t)
	if r := rp.resumeBatch(batchID, ""); r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	rp.w.statusFunc = func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: jobID, Status: transcode.StatusExpired}, nil
	}
	rp.w.publish = func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: jobID, Status: transcode.StatusExpired}, nil
	}
	r := rp.settleBatch(batchID, rp.resumeBatch(batchID, "approve"))
	if r.Status != StatusFailed || !strings.Contains(r.Error, "encode the file again") {
		t.Fatalf("batch: %s %s", r.Status, r.Error)
	}
	if rp.imports != 0 || rp.deletes != 0 {
		t.Fatalf("an expired candidate replaced the original: imports=%d deletes=%d", rp.imports, rp.deletes)
	}
	if _, err := os.Stat(rp.original); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(rp.candidate); !os.IsNotExist(err) {
		t.Fatal("an expired candidate appeared on the share")
	}
}

// Rejecting the batch's plan keeps the candidates, as for candidates on the
// share: a later review can still replace them.
func TestRejectingTheBatchPlanKeepsRetainedCandidatesForALaterReview(t *testing.T) {
	rp, batchID := retainedBatch(t)
	if r := rp.resumeBatch(batchID, ""); r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	if r := rp.resumeBatch(batchID, "reject"); r.Status != StatusCompleted || rp.imports != 0 {
		t.Fatalf("reject: %s %s", r.Status, r.Error)
	}
	if got := rp.w.releases(); len(got) != 0 {
		t.Fatalf("a rejected plan released a candidate that can still be reviewed: %v", got)
	}
	if candidateIsOnWorker(rp.sourceState()) == false {
		t.Fatal("the candidate left the worker")
	}
}

// --- every path that ends the candidate -------------------------------------

func TestCancellingTheBatchFromItsScheduleStepReleasesFinishedChildren(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	seedRetainedAttempt(t, st, "child-done", StatusCompleted, original, map[string]any{"parent_action_id": "batch-cancel"}, nil)
	seedCandidateAttempt(t, st, "child-on-share", StatusCompleted, original, "job-on-share", map[string]any{"parent_action_id": "batch-cancel"}, nil)
	seedActionInstance(t, st, "batch-cancel", "transcode_batch", StatusWaitingExternal, 1, "", map[string]any{"service": "sonarr", "series_id": 1}, map[string]any{})
	for key, child := range map[string]string{"a": "child-done", "b": "child-on-share"} {
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-cancel", ItemKey: key, FilePath: original, Decision: "transcode", Status: "completed", ChildActionID: child}); err != nil {
			t.Fatal(err)
		}
	}
	e.registerTranscodeBatchTemplate()
	r, err := e.Resume(context.Background(), "batch-cancel", "cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusCancelled {
		t.Fatalf("batch: %s %s", r.Status, r.Error)
	}
	if got := w.releases(); len(got) != 1 || got[0] != "job-child-done" {
		t.Fatalf("released %v, want only the retained child's", got)
	}
	state := parseExecutionContext(mustInstance(t, st, "child-done"), e).State
	if !getBool(state, candidateRemovedKey) {
		t.Fatalf("the release was not recorded: %v", state)
	}
}

// Rejecting the promotion of one retained child ends its candidate and no
// other, and a batch child is released like any other attempt.
func TestRejectingOneRetainedChildOfABatchReleasesOnlyItsCandidate(t *testing.T) {
	rp := newRetainedPromotion(t)
	source := mustInstance(t, rp.st, "source-transcode")
	rp.makeBatchChild(source, "retained-batch")
	if err := rp.st.UpdateActionInstance(*source); err != nil {
		t.Fatal(err)
	}
	seedBatchParent(t, rp.st, "retained-batch", StatusWaitingExternal, map[string]any{})
	seedRetainedAttempt(t, rp.st, "other-child", StatusCompleted, filepath.Join(rp.root, "other.mkv"), map[string]any{"parent_action_id": "retained-batch"}, nil)
	r := runLocalPromotion(t, rp.promotionHarness)
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	r = rp.resume(r.ID, "reject")
	if r.Status != StatusFailed || !strings.Contains(r.Error, "rejected") {
		t.Fatalf("reject: %s %s", r.Status, r.Error)
	}
	if got := rp.w.releases(); len(got) != 1 || got[0] != "job-source" {
		t.Fatalf("released %v, want only the rejected child's", got)
	}
}

// Cancelling a batch that is waiting for approval of its plan still releases
// the candidates the plan names: a cancelled batch is never reviewed again.
func TestCancellingABatchWaitingOnItsPlanReleasesTheCandidatesItNames(t *testing.T) {
	rp, batchID := retainedBatch(t)
	if r := rp.resumeBatch(batchID, ""); r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	if plan, err := rp.engine.BatchPromotionPlan(context.Background(), batchID); err != nil || plan == nil || len(plan.Members) != 1 {
		t.Fatalf("the batch holds no plan: %+v %v", plan, err)
	}
	if _, err := rp.engine.Cancel(context.Background(), batchID, "no longer wanted"); err != nil {
		t.Fatal(err)
	}
	if got := rp.w.releases(); len(got) != 1 || got[0] != "job-source" {
		t.Fatalf("released %v, want the planned candidate's", got)
	}
	if !getBool(rp.sourceState(), candidateRemovedKey) {
		t.Fatalf("the release was not recorded: %v", rp.sourceState())
	}
}

// A release the worker cannot take (it is unreachable) leaves the candidate
// there. The cancelled batch reports it and lists it to be discarded later,
// where a candidate of a live batch is not listed.
func TestACandidateTheCancelledBatchCouldNotReleaseIsReportedAndListed(t *testing.T) {
	rp, batchID := retainedBatch(t)
	rp.w.release = func(_ context.Context, jobID string) (transcode.ReleaseOutcome, error) {
		return "", &httpclient.UncertainError{Op: "release", JobID: jobID, Err: errors.New("connection refused")}
	}
	if page, err := rp.engine.LeftoverCandidatesOf(context.Background(), []string{"source-transcode"}); err != nil || len(page.Items) != 0 {
		t.Fatalf("a candidate of a live batch is listed: %+v %v", page, err)
	}
	if err := rp.engine.releaseRetainedChild(context.Background(), "source-transcode", candidateRetiredCancelled); err == nil || !strings.Contains(err.Error(), "retained candidate kept") {
		t.Fatalf("an unreleased candidate was not reported: %v", err)
	}
	if page, err := rp.engine.LeftoverCandidatesOf(context.Background(), []string{"source-transcode"}); err != nil || len(page.Items) != 0 {
		t.Fatalf("a candidate of a live batch is listed: %+v %v", page, err)
	}
	if _, err := rp.engine.Cancel(context.Background(), batchID, "no longer wanted"); err != nil {
		t.Fatal(err)
	}
	page, err := rp.engine.LeftoverCandidatesOf(context.Background(), []string{"source-transcode"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Location != candidateOnWorker {
		t.Fatalf("the kept candidate is not listed: %+v %v", page, err)
	}
	// Discarding it once the worker answers frees it.
	rp.w.release = nil
	if _, err := rp.engine.DiscardLeftoverCandidate(context.Background(), "source-transcode"); err != nil {
		t.Fatal(err)
	}
	if !getBool(rp.sourceState(), candidateRemovedKey) {
		t.Fatalf("the discard was not recorded: %v", rp.sourceState())
	}
}
