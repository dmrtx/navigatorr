package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// retainedExecutor is a worker that can keep a candidate and be asked to
// publish or drop it.
type retainedExecutor struct {
	*mockTranscodeExecutor
	mu        sync.Mutex
	deferred  bool // advertises deferred_publication
	published []string
	released  []string
	publish   func(context.Context, string) (transcode.JobStatus, error)
	release   func(context.Context, string) (transcode.ReleaseOutcome, error)
	requests  []transcode.Request
}

func newRetainedExecutor() *retainedExecutor {
	return &retainedExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}, deferred: true}
}

func (r *retainedExecutor) Capabilities(ctx context.Context) (transcode.WorkerCapabilities, error) {
	caps, err := r.mockTranscodeExecutor.Capabilities(ctx)
	caps.DeferredPublication = r.deferred
	caps.CapabilityFingerprint, _ = transcode.ComputeCapabilityFingerprint(caps)
	return caps, err
}

func (r *retainedExecutor) Submit(ctx context.Context, req transcode.Request) (transcode.Job, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	return r.mockTranscodeExecutor.Submit(ctx, req)
}

func (r *retainedExecutor) PublishRetained(ctx context.Context, jobID string) (transcode.JobStatus, error) {
	r.mu.Lock()
	r.published = append(r.published, jobID)
	r.mu.Unlock()
	if r.publish != nil {
		return r.publish(ctx, jobID)
	}
	return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted}, nil
}

func (r *retainedExecutor) ReleaseRetained(ctx context.Context, jobID string) (transcode.ReleaseOutcome, error) {
	r.mu.Lock()
	r.released = append(r.released, jobID)
	r.mu.Unlock()
	if r.release != nil {
		return r.release(ctx, jobID)
	}
	return transcode.ReleaseDropped, nil
}

func (r *retainedExecutor) releases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.released...)
}

func (r *retainedExecutor) lastRequest(t *testing.T) transcode.Request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		t.Fatal("the worker was sent no job")
	}
	return r.requests[len(r.requests)-1]
}

// retainedEngine is an engine whose only worker is ex, configured with policy.
func retainedEngine(t *testing.T, st *store.Store, root string, ex transcode.Executor, policy string) *Engine {
	t.Helper()
	resolver, err := fsop.NewResolver([]string{root}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{AllowDestructive: true}, Fs: resolver})
	workers, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: transcode.DefaultWorkerName, Executor: ex, Podcast: true, PublishPolicy: policy}})
	if err != nil {
		t.Fatal(err)
	}
	e.deps.Workers = workers
	return e
}

const retainedSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// seedRetainedAttempt records a finished attempt whose candidate is still on
// its worker: nothing exists at its candidate path.
func seedRetainedAttempt(t *testing.T, st *store.Store, id, status, original string, inputs, extra map[string]any) string {
	t.Helper()
	state := map[string]any{candidateLocationKey: candidateOnWorker, publishPolicyKey: transcode.PublishPolicyOnApproval, "candidate_size_bytes": 300, "candidate_sha256": retainedSHA, "transcode_done": true}
	for k, v := range extra {
		state[k] = v
	}
	return seedCandidateAttempt(t, st, id, status, original, "job-"+id, inputs, state)
}

func noShareRemoval(t *testing.T, e *Engine) {
	t.Helper()
	e.candidateRemoveHook = func(path string) { t.Fatalf("touched the share: %s", path) }
}

// --- the release hook -------------------------------------------------------

func TestRetiringARetainedCandidateAsksItsWorkerToDropIt(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	noShareRemoval(t, e)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	seedRetainedAttempt(t, st, "attempt", StatusFailed, original, nil, nil)
	markRejected(t, st, "attempt")

	inst := retireUnderLease(t, e, "attempt", candidateRetiredRejected)
	if got := w.releases(); len(got) != 1 || got[0] != "job-attempt" {
		t.Fatalf("worker asked to release %v", got)
	}
	state := parseExecutionContext(inst, e).State
	cleanup := candidateCleanupOf(t, e, inst)
	if !getBool(state, candidateRemovedKey) || cleanup["outcome"] != "removed" || cleanup["released_from_worker"] != true || getString(cleanup, "reason") != candidateRetiredRejected {
		t.Fatalf("release not recorded: %v", cleanup)
	}
	if !strings.Contains(inst.ErrorJSON, "retained candidate released on its worker") {
		t.Fatalf("the failure does not say the worker dropped it: %s", inst.ErrorJSON)
	}
	// A second pass is a no-op: the worker is not asked again.
	retireUnderLease(t, e, "attempt", candidateRetiredRejected)
	if got := w.releases(); len(got) != 1 {
		t.Fatalf("released twice: %v", got)
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		outcome   transcode.ReleaseOutcome
		err       error
		noHandler bool
		removed   bool
		detail    string
		onShare   bool // the worker had already copied the candidate
	}{
		{name: "worker drops it", outcome: transcode.ReleaseDropped, removed: true},
		{name: "worker already published it", outcome: transcode.ReleasePublished, removed: true, onShare: true},
		{name: "worker is publishing it", outcome: transcode.ReleaseRefused, detail: "copying the candidate to the share"},
		{name: "worker unreachable", err: &httpclient.UncertainError{Op: "release", JobID: "job-attempt", Err: errors.New("connection refused")}, detail: "could not be released on its worker"},
		{name: "worker answers an error", err: &httpclient.HTTPError{StatusCode: 500, Code: transcode.WorkerErrorInternal}, detail: "could not be released on its worker"},
		{name: "executor cannot release", noHandler: true, detail: "cannot release a retained candidate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := candidateRoot(t)
			st := setupTestStore(t)
			w := newRetainedExecutor()
			w.release = func(context.Context, string) (transcode.ReleaseOutcome, error) { return tc.outcome, tc.err }
			var ex transcode.Executor = w
			if tc.noHandler {
				ex = &mockTranscodeExecutor{}
			}
			e := retainedEngine(t, st, root, ex, transcode.PublishPolicyOnApproval)
			original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
			writeCandidateFile(t, original, "original")
			candidate := seedRetainedAttempt(t, st, "attempt", StatusFailed, original, nil, nil)
			if tc.onShare {
				writeCandidateFile(t, candidate, strings.Repeat("c", 40))
			} else {
				noShareRemoval(t, e)
			}
			inst := retireUnderLease(t, e, "attempt", candidateRetiredRejected)
			state := parseExecutionContext(inst, e).State
			cleanup := candidateCleanupOf(t, e, inst)
			if got := getBool(state, candidateRemovedKey); got != tc.removed {
				t.Fatalf("removed=%v want %v: %v", got, tc.removed, cleanup)
			}
			if tc.detail != "" {
				if cleanup["outcome"] != "kept" || !strings.Contains(getString(cleanup, "detail"), tc.detail) {
					t.Fatalf("a candidate that could not be released must be recorded as kept: %v", cleanup)
				}
				return
			}
			if tc.onShare {
				if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
					t.Fatalf("the published file was not removed: %v", err)
				}
				if getString(state, candidateLocationKey) != candidateOnShare {
					t.Fatalf("location not updated: %v", state[candidateLocationKey])
				}
			}
		})
	}
}

func TestReleaseKeepsACandidateAPromotionStarted(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	seedRetainedAttempt(t, st, "attempt", StatusCompleted, original, nil, nil)
	seedActionInstance(t, st, "promotion", "promote_transcode_candidate", StatusRunning, 2, "promote:filesystem:attempt", map[string]any{"transcode_action_id": "attempt"}, nil)
	inst := retireUnderLease(t, e, "attempt", candidateRetiredReplaced)
	if got := w.releases(); len(got) != 0 {
		t.Fatalf("released a candidate a promotion is using: %v", got)
	}
	if cleanup := candidateCleanupOf(t, e, inst); cleanup["outcome"] != "kept" {
		t.Fatalf("not recorded as kept: %v", cleanup)
	}
}

// --- every path that stops a candidate being promotable ---------------------

func TestSiblingReplacementReleasesARetainedCandidate(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	seedRetainedAttempt(t, st, "promoted", StatusCompleted, original, nil, map[string]any{"original_sha256": "baseline"})
	seedRetainedAttempt(t, st, "sibling", StatusCompleted, original, nil, map[string]any{"original_sha256": "baseline"})
	seedRetainedAttempt(t, st, "other-file", StatusCompleted, filepath.Join(root, "Sample Show", "sample-episode-02.mkv"), nil, map[string]any{"original_sha256": "baseline"})
	e.retireReplacedSiblings(context.Background(), &promotionState{SourceActionID: "promoted", OriginalPath: original, OriginalSHA: "baseline"})
	if got := w.releases(); len(got) != 1 || got[0] != "job-sibling" {
		t.Fatalf("released %v, want only the sibling's", got)
	}
}

func TestBatchRevisionReleasesSupersededRetainedCandidates(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	seedRetainedAttempt(t, st, "old", StatusCompleted, original, map[string]any{"parent_action_id": "batch"}, nil)
	seedRetainedAttempt(t, st, "running", StatusRunning, original, map[string]any{"parent_action_id": "batch"}, nil)
	e.retireSupersededCandidates(context.Background(), []string{"old", "running"})
	if got := w.releases(); len(got) != 1 || got[0] != "job-old" {
		t.Fatalf("released %v, want only the finished attempt's", got)
	}
}

func TestCancellingABatchReleasesTheRetainedCandidateOfAFinishedChild(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	seedRetainedAttempt(t, st, "child", StatusCompleted, original, map[string]any{"parent_action_id": "batch"}, nil)
	e.releaseRetainedChild(context.Background(), "child", candidateRetiredCancelled)
	if got := w.releases(); len(got) != 1 || got[0] != "job-child" {
		t.Fatalf("released %v", got)
	}
	// A child with nothing on a worker is left alone.
	seedCandidateAttempt(t, st, "on-share", StatusCompleted, original, "job-on-share", nil, nil)
	e.releaseRetainedChild(context.Background(), "on-share", candidateRetiredCancelled)
	if got := w.releases(); len(got) != 1 {
		t.Fatalf("a share candidate was released through the worker: %v", got)
	}
}

func TestDiscardingAWorkerLeftoverReleasesItAndAnUnreachableWorkerKeepsIt(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	noShareRemoval(t, e)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	seedRetainedAttempt(t, st, "attempt", StatusFailed, original, nil, nil)
	markRejected(t, st, "attempt")

	// Listed from the worker's attestation, without a stat and without asking.
	page, err := e.ListLeftoverCandidates(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ActionID != "attempt" || page.Items[0].Location != candidateOnWorker || page.Items[0].Bytes != 300 || page.Items[0].WorkerCheck || page.Bytes != 300 {
		t.Fatalf("listing: %+v", page)
	}
	if got := w.releases(); len(got) != 0 {
		t.Fatalf("listing released something: %v", got)
	}

	w.release = func(context.Context, string) (transcode.ReleaseOutcome, error) {
		return "", &httpclient.UncertainError{Op: "release", JobID: "job-attempt", Err: errors.New("connection refused")}
	}
	before := mustInstance(t, st, "attempt")
	if _, err := e.DiscardLeftoverCandidate(context.Background(), "attempt"); err == nil || !strings.Contains(err.Error(), "candidate file kept") {
		t.Fatalf("an unreachable worker must keep it: %v", err)
	}
	if after := mustInstance(t, st, "attempt"); getBool(parseExecutionContext(after, e).State, candidateRemovedKey) || after.Status != before.Status {
		t.Fatal("the candidate was recorded as removed")
	}
	if page, _ := e.ListLeftoverCandidates(context.Background(), 0); len(page.Items) != 1 {
		t.Fatalf("a kept candidate must stay listed: %+v", page)
	}

	w.release = nil
	if _, err := e.DiscardLeftoverCandidate(context.Background(), "attempt"); err != nil {
		t.Fatal(err)
	}
	if got := w.releases(); len(got) != 2 {
		t.Fatalf("releases: %v", got)
	}
	if page, _ := e.ListLeftoverCandidates(context.Background(), 0); len(page.Items) != 0 {
		t.Fatalf("a released candidate must not be listed: %+v", page)
	}
}

// A copy to the share that was asked for and failed can leave the attempt's own
// files there; releasing the candidate removes them with it.
func TestReleasingARetainedCandidateRemovesWhatAFailedCopyLeftOnTheShare(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	candidate := seedRetainedAttempt(t, st, "attempt", StatusFailed, original, nil, nil)
	markRejected(t, st, "attempt")
	_, partial, err := ownCandidateFiles(parseExecutionContext(mustInstance(t, st, "attempt"), e).State)
	if err != nil {
		t.Fatal(err)
	}
	writeCandidateFile(t, partial, strings.Repeat("p", 120))

	if _, err := e.DiscardLeftoverCandidate(context.Background(), "attempt"); err != nil {
		t.Fatal(err)
	}
	if got := w.releases(); len(got) != 1 {
		t.Fatalf("releases: %v", got)
	}
	if _, err := os.Lstat(partial); !os.IsNotExist(err) {
		t.Fatalf("the failed copy's partial stayed on the share: %v", err)
	}
	if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
		t.Fatalf("candidate: %v", err)
	}
	cleanup := candidateCleanupOf(t, e, mustInstance(t, st, "attempt"))
	if cleanup["outcome"] != "removed" || cleanup["released_from_worker"] != true {
		t.Fatalf("cleanup: %v", cleanup)
	}
}

// --- validation without a file ---------------------------------------------

func TestValidationTrustsTheAttestedSizeOfARetainedCandidate(t *testing.T) {
	e := NewEngine(EngineDeps{})
	ec := func(state map[string]any) *ExecutionContext {
		return &ExecutionContext{InstanceID: "x", Inputs: map[string]any{}, State: state, Outputs: map[string]any{}}
	}
	path := filepath.Join(t.TempDir(), "missing.mkv") // nothing is there
	for _, tc := range []struct {
		name   string
		state  map[string]any
		wantOK bool
		errHas string
	}{
		{name: "retained with an attested size", state: map[string]any{candidateLocationKey: candidateOnWorker, "candidate_size_bytes": 300}, wantOK: true},
		{name: "retained without a size", state: map[string]any{candidateLocationKey: candidateOnWorker}, errHas: "no worker-attested size"},
		{name: "released on its worker", state: map[string]any{candidateLocationKey: candidateOnWorker, "candidate_size_bytes": 300, candidateRemovedKey: true}, errHas: "released on its worker"},
		{name: "on the share and missing", state: map[string]any{"candidate_size_bytes": 300}, errHas: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.state["candidate_path"] = path
			n, err := e.verifyPublishedCandidateLight(context.Background(), ec(tc.state), path)
			switch {
			case tc.wantOK && (err != nil || n != 0):
				t.Fatalf("got %d %v", n, err)
			case !tc.wantOK && err == nil:
				t.Fatal("expected a refusal")
			case tc.errHas != "" && !strings.Contains(err.Error(), tc.errHas):
				t.Fatalf("error %q does not say %q", err, tc.errHas)
			}
		})
	}
}

// --- the whole action -------------------------------------------------------

// retainedStatus is the worker's answer for a job whose encode finished and
// whose candidate it keeps.
func retainedStatus(jobID, candidate string, size int64) transcode.JobStatus {
	return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning, CandidatePath: candidate, Retained: true, FinalizationState: "retained", CandidateSizeBytes: size, CandidateSHA256: retainedSHA}
}

// retainedFlow runs transcode_media against a worker that keeps its candidate.
// The candidate is never written to the share.
func retainedFlow(t *testing.T, policy string, w *retainedExecutor, candidateSize int64) (*Engine, *store.Store, string, string, *ActionResult) {
	t.Helper()
	root := candidateRoot(t)
	original := filepath.Join(root, "RetainedSource.mkv")
	writeCandidateFile(t, original, strings.Repeat("o", 1000))
	probe := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\ncat << 'JSON'\n" + defaultValidFFprobeJSON + "\nJSON\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var candidate string
	w.statusFunc = func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
		candidate = filepath.Join(root, ".navigatorr-candidates", "RetainedSource."+jobID+".mkv")
		return retainedStatus(jobID, candidate, candidateSize), nil
	}
	e, st := setupTranscodeEngine(t, nil, probe, []string{root}, []string{root}, true)
	workers, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: transcode.DefaultWorkerName, Executor: w, Podcast: true, PublishPolicy: policy}})
	if err != nil {
		t.Fatal(err)
	}
	e.deps.Workers = workers
	res, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": original})
	if err != nil {
		t.Fatal(err)
	}
	return e, st, original, candidate, res
}

func TestARetainedCandidateFreesTheWorkerAndReachesReviewWithoutAFile(t *testing.T) {
	w := newRetainedExecutor()
	e, st, original, candidate, res := retainedFlow(t, transcode.PublishPolicyOnApproval, w, 300)
	if res.Status != StatusWaitingDecision && res.Status != StatusCompleted {
		t.Fatalf("expected the action to get past wait and validate: %s %s", res.Status, res.Error)
	}
	if req := w.lastRequest(t); req.PublishPolicy != transcode.PublishPolicyOnApproval {
		t.Fatalf("the job was not asked to be retained: %+v", req.PublishPolicy)
	}
	inst := mustInstance(t, st, res.ID)
	ec := parseExecutionContext(inst, e)
	if !candidateIsOnWorker(ec.State) || getString(ec.State, publishPolicyKey) != transcode.PublishPolicyOnApproval || getInt64(ec.State, "candidate_size_bytes") != 300 || !getBool(ec.State, "transcode_done") {
		t.Fatalf("state: %v", ec.State)
	}
	if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
		t.Fatalf("nothing should be on the share: %v", err)
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatal(err)
	}
	if got := w.releases(); len(got) != 0 {
		t.Fatalf("a candidate awaiting review was released: %v", got)
	}
}

func TestRejectingAReviewedRetainedCandidateReleasesIt(t *testing.T) {
	w := newRetainedExecutor()
	// Bigger than the 1000-byte original: validation waits for a decision.
	e, st, _, candidate, res := retainedFlow(t, transcode.PublishPolicyOnApproval, w, 5000)
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected a size decision: %s %s", res.Status, res.Error)
	}
	noShareRemoval(t, e)
	if got := w.releases(); len(got) != 0 {
		t.Fatalf("released before any decision: %v", got)
	}
	rejected, err := e.Resume(context.Background(), res.ID, "reject", nil)
	if err != nil || rejected.Status != StatusFailed {
		t.Fatalf("reject: %+v %v", rejected, err)
	}
	if got := w.releases(); len(got) != 1 || got[0] != getString(parseExecutionContext(mustInstance(t, st, res.ID), e).State, "job_id") {
		t.Fatalf("released %v", got)
	}
	inst := mustInstance(t, st, res.ID)
	if cleanup := candidateCleanupOf(t, e, inst); cleanup["outcome"] != "removed" || cleanup["released_from_worker"] != true || getString(cleanup, "reason") != candidateRetiredRejected {
		t.Fatalf("cleanup: %v", cleanup)
	}
	if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
		t.Fatalf("the share was touched: %v", err)
	}
	if _, err := e.Retry(context.Background(), res.ID); err == nil || !strings.Contains(err.Error(), "candidate was removed") {
		t.Fatalf("retry of a released candidate allowed: %v", err)
	}
}

func TestCancellingAReviewedRetainedCandidateReleasesIt(t *testing.T) {
	w := newRetainedExecutor()
	e, st, _, _, res := retainedFlow(t, transcode.PublishPolicyOnApproval, w, 5000)
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected a size decision: %s %s", res.Status, res.Error)
	}
	if _, err := e.Cancel(context.Background(), res.ID, "not wanted"); err != nil {
		t.Fatal(err)
	}
	inst := mustInstance(t, st, res.ID)
	if cleanup := candidateCleanupOf(t, e, inst); cleanup["outcome"] != "removed" || cleanup["released_from_worker"] != true {
		t.Fatalf("cleanup: %v", cleanup)
	}
}

// A failure is not a decision: the error may be a passing one, and Retry
// resumes the attempt on its candidate, so the worker keeps it. Until someone
// discards it the attempt lists it as a leftover.
func TestAFailedAttemptKeepsItsRetainedCandidateUntilItIsDiscarded(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	// Stopped at validate_result with no plan: validation fails closed.
	tmpl, _ := e.GetTemplate("transcode_media")
	seedRetainedAttempt(t, st, "attempt", StatusFailed, original, nil, nil)
	inst := mustInstance(t, st, "attempt")
	inst.CurrentStep = actionStepIndex(tmpl, "validate_result")
	inst.ErrorJSON = toJSON(map[string]any{"step": "validate_result", "error": "earlier failure"})
	if err := st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		res, err := e.Retry(context.Background(), "attempt")
		if err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		if res.Status != StatusFailed || !strings.Contains(res.Error, "resolved plan missing") {
			t.Fatalf("expected validation to fail: %s %s", res.Status, res.Error)
		}
		if got := w.releases(); len(got) != 0 {
			t.Fatalf("a failed attempt released its retained candidate: %v", got)
		}
	}
	state := parseExecutionContext(mustInstance(t, st, "attempt"), e).State
	if getBool(state, candidateRemovedKey) || getString(state, candidateLocationKey) != candidateOnWorker {
		t.Fatalf("the failed attempt lost its candidate: %v", state)
	}
	page, err := e.LeftoverCandidatesOf(context.Background(), []string{"attempt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Location != candidateOnWorker || page.Items[0].Bytes != 300 {
		t.Fatalf("the kept candidate must be listed from its attestation: %+v", page.Items)
	}
	noShareRemoval(t, e)
	if _, err := e.DiscardLeftoverCandidate(context.Background(), "attempt"); err != nil {
		t.Fatal(err)
	}
	if got := w.releases(); len(got) != 1 || got[0] != "job-attempt" {
		t.Fatalf("discard must release the candidate on its worker: %v", got)
	}
	// Once released, Retry cannot resume it: the candidate is gone.
	if _, err := e.Retry(context.Background(), "attempt"); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("retry of a released attempt: %v", err)
	}
}

func TestAnAttemptThatEndsWithoutACandidateToPromoteReleasesIt(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	tmpl, _ := e.GetTemplate("transcode_media")
	// A size or saving guard skipped the file after the worker retained it.
	seedRetainedAttempt(t, st, "attempt", StatusFailed, original, nil, map[string]any{"skip_transcode": true, "reason_code": "insufficient_savings", "original_sha256": "0682c5f2076f099c34cfdd15a9e063849ed437a49677e6fcc5b4198c76575be5"})
	inst := mustInstance(t, st, "attempt")
	inst.CurrentStep = actionStepIndex(tmpl, "validate_result")
	if err := st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	res, err := e.Retry(context.Background(), "attempt")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s %s", res.Status, res.Error)
	if got := w.releases(); len(got) != 1 || got[0] != "job-attempt" {
		t.Fatalf("a skipped attempt kept its retained candidate: %v", got)
	}
	if cleanup := candidateCleanupOf(t, e, mustInstance(t, st, "attempt")); getString(cleanup, "reason") != candidateRetiredSkipped {
		t.Fatalf("cleanup: %v", cleanup)
	}
}

// --- inert by default -------------------------------------------------------

func TestPublishOnEncodeIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   string
		deferred bool
		fallback string
	}{
		{name: "default policy", policy: "", deferred: true},
		{name: "default policy on an old worker", policy: "", deferred: false},
		{name: "on_approval without the capability", policy: transcode.PublishPolicyOnApproval, deferred: false, fallback: "worker_lacks_deferred_publication"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := candidateRoot(t)
			original := filepath.Join(root, "PlainSource.mkv")
			writeCandidateFile(t, original, strings.Repeat("o", 1000))
			probe := createFakeFFprobeScript(t, defaultValidFFprobeJSON)
			w := newRetainedExecutor()
			w.deferred = tc.deferred
			w.statusFunc = func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
				candidate := filepath.Join(root, ".navigatorr-candidates", "PlainSource."+jobID+".mkv")
				if _, err := os.Lstat(candidate); os.IsNotExist(err) {
					writeCandidateFile(t, candidate, strings.Repeat("c", 300))
				}
				return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted, CandidatePath: candidate, CandidateSizeBytes: 300, CandidateSHA256: retainedSHA}, nil
			}
			e, st := setupTranscodeEngine(t, nil, probe, []string{root}, []string{root}, true)
			workers, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: transcode.DefaultWorkerName, Executor: w, Podcast: true, PublishPolicy: tc.policy}})
			if err != nil {
				t.Fatal(err)
			}
			e.deps.Workers = workers
			res, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": original})
			if err != nil {
				t.Fatal(err)
			}
			if req := w.lastRequest(t); req.PublishPolicy != "" {
				t.Fatalf("the job carries publish_policy %q", req.PublishPolicy)
			}
			state := parseExecutionContext(mustInstance(t, st, res.ID), e).State
			for _, key := range []string{candidateLocationKey, publishPolicyKey} {
				if _, ok := state[key]; ok {
					t.Fatalf("state gained %s: %v", key, state[key])
				}
			}
			if getString(state, publishPolicyFallbackKey) != tc.fallback {
				t.Fatalf("fallback %q, want %q", getString(state, publishPolicyFallbackKey), tc.fallback)
			}
			if candidateIsOnWorker(state) || len(w.releases()) != 0 {
				t.Fatal("a share candidate was treated as retained")
			}
		})
	}
}

func TestThePublishPolicyIsDecidedOncePerAttempt(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	ec := &ExecutionContext{InstanceID: "x", Inputs: map[string]any{}, State: map[string]any{}, Outputs: map[string]any{}}
	if policy, _ := e.publishPolicyFor(context.Background(), ec); policy != transcode.PublishPolicyOnApproval {
		t.Fatal(policy)
	}
	// The worker is reconfigured or loses the capability: a resubmit of the same
	// job still sends what the first submit sent.
	w.deferred = false
	if policy, held := e.publishPolicyFor(context.Background(), ec); policy != transcode.PublishPolicyOnApproval || held != nil {
		t.Fatalf("the decision changed: %q %v", policy, held)
	}
}

func TestACapabilityLookupFailureHoldsTheSubmit(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	w.capabilitiesFunc = func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{}, &httpclient.UncertainError{Op: "capabilities", Err: errors.New("connection refused")}
	}
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	ec := &ExecutionContext{InstanceID: "x", Inputs: map[string]any{}, State: map[string]any{}, Outputs: map[string]any{}}
	policy, held := e.publishPolicyFor(context.Background(), ec)
	if policy != "" || held == nil || held.Status == StepCompleted {
		t.Fatalf("a worker that cannot be asked must not silently publish on encode: %q %+v", policy, held)
	}
	if _, ok := ec.State[publishPolicyFallbackKey]; ok {
		t.Fatal("recorded a fallback for a worker that was never asked")
	}
}

// --- the worker dropped it --------------------------------------------------

func TestAnExpiredRetainedCandidateEncodesAgainUpToACap(t *testing.T) {
	e := NewEngine(EngineDeps{})
	ec := &ExecutionContext{InstanceID: "x", Inputs: map[string]any{}, Outputs: map[string]any{}, State: map[string]any{
		"job_id": "job-1", candidateLocationKey: candidateOnWorker, "candidate_size_bytes": 300, "candidate_sha256": retainedSHA, publishPolicyKey: transcode.PublishPolicyOnApproval, "transcode_submitted": true,
	}}
	for i := 1; i <= maxCandidateExpiries; i++ {
		res := e.retainedExpiredResult(ec, "job-1")
		if res.Status != StepCompleted || res.RewindTo != "submit_transcode" {
			t.Fatalf("expiry %d: %+v", i, res)
		}
		if _, ok := ec.State[candidateLocationKey]; ok {
			t.Fatalf("the attempt still says its candidate is on the worker: %v", ec.State)
		}
		if getString(ec.State, "job_id") != "job-1" || getInt(ec.State, candidateExpiredCountKey) != i {
			t.Fatalf("job id / count lost: %v", ec.State)
		}
		ec.State[candidateLocationKey], ec.State["candidate_size_bytes"] = candidateOnWorker, 300
	}
	if res := e.retainedExpiredResult(ec, "job-1"); res.Status != StepFailed || !strings.Contains(res.Error, "dropped the retained candidate") {
		t.Fatalf("the cap was not enforced: %+v", res)
	}
}

func TestWaitSeesAnExpiredJobAndARetainedJob(t *testing.T) {
	w := newRetainedExecutor()
	e := NewEngine(EngineDeps{Transcode: w})
	newEC := func() *ExecutionContext {
		ec := testReconcileEC("")
		ec.State["job_id"] = "job-existing"
		ec.State["candidate_path"] = "/Volumes/media/.navigatorr-candidates/ep.job-existing.mkv"
		return ec
	}
	w.statusFunc = func(context.Context, string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: "job-existing", Status: transcode.StatusExpired}, nil
	}
	ec := newEC()
	res, err := e.stepTranscodeWait(context.Background(), ec)
	if err != nil || res.Status != StepCompleted || res.RewindTo != "submit_transcode" {
		t.Fatalf("expired: %+v %v", res, err)
	}

	w.statusFunc = func(context.Context, string) (transcode.JobStatus, error) {
		return retainedStatus("job-existing", "/Volumes/media/.navigatorr-candidates/ep.job-existing.mkv", 300), nil
	}
	ec = newEC()
	res, err = e.stepTranscodeWait(context.Background(), ec)
	if err != nil || res.Status != StepCompleted || res.Outputs["transcode_done"] != true || res.Outputs[candidateLocationKey] != candidateOnWorker || !candidateIsOnWorker(ec.State) {
		t.Fatalf("retained: %+v %v", res, err)
	}

	// A retained job that attests nothing cannot be promoted: fail closed.
	w.statusFunc = func(context.Context, string) (transcode.JobStatus, error) {
		st := retainedStatus("job-existing", "/Volumes/media/ep.mkv", 0)
		st.CandidateSHA256 = ""
		return st, nil
	}
	ec = newEC()
	if res, _ = e.stepTranscodeWait(context.Background(), ec); res.Status != StepFailed || candidateIsOnWorker(ec.State) {
		t.Fatalf("unattested: %+v", res)
	}
}

// --- drain readiness --------------------------------------------------------

func TestRetainedCandidatesDoNotBlockADeployButAnUploadDoes(t *testing.T) {
	retained := store.DrainActionRow{ActionName: "transcode_media", Status: StatusCompleted, Done: true}
	if got := classifyDrainRow(retained); got != "" {
		t.Fatalf("a retained candidate blocks a deploy: %q", got)
	}
	idle := store.DrainActionRow{ActionName: "promote_transcode_candidate", Status: StatusWaitingDecision}
	if got := classifyDrainRow(idle); got != "" {
		t.Fatalf("a promotion awaiting a person blocks a deploy: %q", got)
	}
	uploading := store.DrainActionRow{ActionName: "promote_transcode_candidate", Status: StatusRunning, Uploading: true}
	if got := classifyDrainRow(uploading); got != transcode.DrainJobRunning {
		t.Fatalf("an upload must block a deploy: %q", got)
	}
}

func TestDrainListsAnUploadingPromotionAndItsWorker(t *testing.T) {
	st := setupTestStore(t)
	seedActionInstance(t, st, "retained", "transcode_media", StatusCompleted, 6, "", map[string]any{}, map[string]any{"worker": "node-a", "job_id": "job-retained", "transcode_done": true, candidateLocationKey: candidateOnWorker})
	seedActionInstance(t, st, "uploading", "promote_transcode_candidate", StatusRunning, 2, "promote:filesystem:retained", map[string]any{"transcode_action_id": "retained"},
		map[string]any{"upload_worker": "node-a", "work_progress": map[string]any{"phase": "uploading"}})
	seedActionInstance(t, st, "waiting", "promote_transcode_candidate", StatusWaitingDecision, 1, "promote:filesystem:other", map[string]any{"transcode_action_id": "other"}, map[string]any{})
	rows, err := st.ListDrainActions()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.DrainActionRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if r, ok := byID["uploading"]; !ok || !r.Uploading || r.Worker != "node-a" {
		t.Fatalf("uploading promotion: %+v", byID)
	}
	if r, ok := byID["waiting"]; ok && r.Uploading {
		t.Fatalf("a waiting promotion is not uploading: %+v", r)
	}
	if r := byID["retained"]; classifyDrainRow(r) != "" {
		t.Fatalf("the retained candidate blocks: %+v", r)
	}
}

// --- promotion of a retained candidate -------------------------------------

// retainedPromotion is the promotion harness with its source attempt's
// candidate moved off the share onto a worker: the worker "publishes" it by
// writing the same bytes back when asked.
type retainedPromotion struct {
	*promotionHarness
	w       *retainedExecutor
	content []byte
	sha     string
	// onPublish runs when the worker is asked to publish, before it writes.
	onPublish func()
	// workerStatus is what the worker reports for the job before it publishes.
	workerStatus func() transcode.JobStatus
	published    bool
}

func newRetainedPromotion(t *testing.T) *retainedPromotion {
	t.Helper()
	h := newPromotionHarness(t)
	content, err := os.ReadFile(h.candidate)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	rp := &retainedPromotion{promotionHarness: h, w: newRetainedExecutor(), content: content, sha: hex.EncodeToString(sum[:])}
	// The candidate's name carries its job id, as the real ones do.
	if err := os.Remove(h.candidate); err != nil {
		t.Fatal(err)
	}
	h.candidate = filepath.Join(filepath.Dir(h.candidate), "candidate.job-source.mkv")
	rp.workerStatus = func() transcode.JobStatus {
		return retainedStatus("job-source", h.candidate, int64(len(content)))
	}
	rp.w.statusFunc = func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		if rp.published {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted, CandidatePath: h.candidate}, nil
		}
		return rp.workerStatus(), nil
	}
	rp.w.publish = func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		if rp.onPublish != nil {
			rp.onPublish()
		}
		writeCandidateFile(t, h.candidate, string(rp.content))
		rp.published = true
		return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted}, nil
	}
	workers, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: transcode.DefaultWorkerName, Executor: rp.w, Podcast: true, PublishPolicy: transcode.PublishPolicyOnApproval}})
	if err != nil {
		t.Fatal(err)
	}
	h.engine.deps.Workers = workers
	inst := mustInstance(t, h.st, "source-transcode")
	ec := parseExecutionContext(inst, h.engine)
	ec.State["job_id"], ec.State[candidateLocationKey], ec.State[publishPolicyKey] = "job-source", candidateOnWorker, transcode.PublishPolicyOnApproval
	ec.State["candidate_path"], ec.State["output_path"] = h.candidate, h.candidate
	ec.State["candidate_sha256"], ec.State["candidate_size_bytes"], ec.State["transcode_done"] = rp.sha, len(content), true
	inst.StateJSON = toJSON(ec.State)
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	return rp
}

func (rp *retainedPromotion) sourceState() map[string]any {
	return parseExecutionContext(mustInstance(rp.t, rp.st, "source-transcode"), rp.engine).State
}

func TestPromotionUploadsARetainedCandidateOnlyOnceApproved(t *testing.T) {
	rp := newRetainedPromotion(t)
	originalSeenAtUpload, locationAtUpload := false, ""
	rp.onPublish = func() {
		_, err := os.Stat(rp.original)
		originalSeenAtUpload = err == nil
		locationAtUpload = getString(rp.sourceState(), candidateLocationKey)
	}
	r := runLocalPromotion(t, rp.promotionHarness)
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	if len(rp.w.published) != 0 {
		t.Fatal("the worker was asked to upload before approval")
	}
	if _, err := os.Lstat(rp.candidate); !os.IsNotExist(err) {
		t.Fatal("the plan put a candidate on the share")
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil || !p.CandidateRetained || p.CandidateUploaded || p.CandidateSHA != rp.sha || p.CandidateBytes != int64(len(rp.content)) {
		t.Fatalf("the plan should carry the attestation: %+v %v", p, err)
	}

	r = rp.resume(r.ID, "approve")
	if r.Status != StatusCompleted || r.Outputs["promoted"] != true {
		t.Fatalf("promotion: %+v", r)
	}
	if len(rp.w.published) != 1 || rp.w.published[0] != "job-source" {
		t.Fatalf("upload requests: %v", rp.w.published)
	}
	if !originalSeenAtUpload {
		t.Fatal("the original was touched before the upload")
	}
	if locationAtUpload != candidateOnWorker {
		t.Fatalf("the location was already %q when the upload started", locationAtUpload)
	}
	if got := getString(rp.sourceState(), candidateLocationKey); got != candidateOnShare {
		t.Fatalf("the source still says the candidate is %q", got)
	}
	if got := rp.w.releases(); len(got) != 0 {
		t.Fatalf("a promoted candidate was released: %v", got)
	}
	if err := rp.engine.verifyPromotionHash(context.Background(), rp.final, rp.sha); err != nil {
		t.Fatal(err)
	}
}

// A copy that is still going is a wait the scheduler comes back to, not a loop
// inside the step: the promotion's progress keeps saying "uploading" through it
// (a deploy must keep waiting), the publication is asked for again each time
// and the original is left alone.
func TestPromotionWaitsForAnUploadTheWorkerIsStillDoing(t *testing.T) {
	rp := newRetainedPromotion(t)
	asks := 0
	rp.w.publish = func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		asks++
		switch asks {
		case 1:
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning, FinalizationState: "finalizing"}, nil
		case 2:
			return transcode.JobStatus{}, &httpclient.UncertainError{Op: "publish", JobID: jobID, Err: errors.New("connection refused")}
		}
		writeCandidateFile(t, rp.candidate, string(rp.content))
		rp.published = true
		return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted}, nil
	}
	r := runLocalPromotion(t, rp.promotionHarness)
	r = rp.resume(r.ID, "approve")
	for wait := 1; wait <= 2; wait++ {
		if r.Status != StatusWaitingExternal || asks != wait {
			t.Fatalf("wait %d: %s asks=%d %s", wait, r.Status, asks, r.Error)
		}
		inst := mustInstance(t, rp.st, r.ID)
		state := parseExecutionContext(inst, rp.engine).State
		if progress, _ := state["work_progress"].(map[string]any); getString(progress, "phase") != "uploading" || getString(state, "upload_worker") == "" {
			t.Fatalf("a waiting upload must still read as an upload: %v", state)
		}
		rows, err := rp.st.ListDrainActions()
		if err != nil {
			t.Fatal(err)
		}
		uploading := false
		for _, row := range rows {
			uploading = uploading || (row.ID == r.ID && row.Uploading)
		}
		if !uploading {
			t.Fatalf("wait %d: the drain does not see the upload: %+v", wait, rows)
		}
		if _, err := os.Stat(rp.original); err != nil {
			t.Fatalf("the original was touched while the copy ran: %v", err)
		}
		var rerr error
		r, rerr = rp.engine.Resume(context.Background(), r.ID, "", nil)
		if rerr != nil {
			t.Fatal(rerr)
		}
	}
	if r.Status != StatusCompleted || asks != 3 {
		t.Fatalf("promotion: %+v asks=%d", r, asks)
	}
	if len(rp.w.published) != 3 {
		t.Fatalf("publish requested %d times", len(rp.w.published))
	}
}

func TestPromotionUploadFailuresKeepTheOriginalAndTheRightCandidate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		publish  func(context.Context, string) (transcode.JobStatus, error)
		status   transcode.JobStatus // what the worker reports after the request
		waits    bool
		released bool // the candidate can never be uploaded: the worker drops it
	}{
		{name: "worker is draining", publish: func(context.Context, string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, transcode.ErrPublishHeld
		}, waits: true},
		{name: "worker is unreachable", publish: func(context.Context, string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &httpclient.UncertainError{Op: "publish", JobID: "job-source", Err: errors.New("connection refused")}
		}, waits: true},
		{name: "worker was not updated", publish: func(context.Context, string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, transcode.ErrDeferredPublicationUnsupported
		}},
		{name: "worker no longer holds it", publish: func(context.Context, string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, transcode.ErrNotRetained
		}, released: true},
		{name: "worker says it is unknown", publish: func(context.Context, string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, &httpclient.HTTPError{StatusCode: 404, Code: transcode.WorkerErrorNotFound}
		}, released: true},
		{name: "candidate changed on the worker", publish: func(context.Context, string) (transcode.JobStatus, error) {
			return transcode.JobStatus{}, transcode.ErrRetainedCandidateChanged
		}, released: true},
		{name: "retention ended", publish: func(_ context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusExpired}, nil
		}, released: true},
		{name: "worker could not copy it", publish: func(_ context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusFailed, Error: "share is full"}, nil
		}},
		{name: "worker needs intervention", publish: func(_ context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning, JobTelemetry: transcode.JobTelemetry{RecoveryRequired: true}, Error: "share is full"}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rp := newRetainedPromotion(t)
			rp.w.publish = tc.publish
			r := runLocalPromotion(t, rp.promotionHarness)
			r = rp.resume(r.ID, "approve")
			if tc.waits && r.Status == StatusFailed {
				t.Fatalf("a worker that is only unavailable must not fail the promotion: %+v", r)
			}
			if !tc.waits && r.Status != StatusFailed {
				t.Fatalf("expected a failed promotion: %+v", r)
			}
			if _, err := os.Stat(rp.original); err != nil {
				t.Fatalf("the original was touched: %v", err)
			}
			released := rp.w.releases()
			if tc.released && (len(released) != 1 || released[0] != "job-source") {
				t.Fatalf("a candidate that can never be uploaded must be released: %v", released)
			}
			if !tc.released && len(released) != 0 {
				t.Fatalf("a transient failure released the candidate: %v", released)
			}
			if tc.released {
				if !getBool(rp.sourceState(), candidateRemovedKey) {
					t.Fatal("the release was not recorded on the attempt")
				}
			} else if getBool(rp.sourceState(), candidateRemovedKey) || !candidateIsOnWorker(rp.sourceState()) {
				t.Fatalf("the attempt should still hold its candidate on the worker: %v", rp.sourceState())
			}
		})
	}
}

func TestPromotionRefusesAnUploadedFileThatIsNotWhatTheWorkerAttested(t *testing.T) {
	rp := newRetainedPromotion(t)
	rp.content = []byte(strings.Repeat("X", len(rp.content))) // same size, other bytes
	// The plan holds the attested hash; the worker then publishes other bytes.
	r := runLocalPromotion(t, rp.promotionHarness)
	r = rp.resume(r.ID, "approve")
	if r.Status != StatusFailed || !strings.Contains(r.Error, "does not match what its worker attested") {
		t.Fatalf("a mismatched upload was accepted: %+v", r)
	}
	if _, err := os.Stat(rp.original); err != nil {
		t.Fatalf("the original was touched: %v", err)
	}
	if _, err := os.Stat(rp.final); !os.IsNotExist(err) {
		t.Fatal("a file that failed inspection was published")
	}
}

func TestPromotionPlanReleasesACandidateThatCanNeverBeUploaded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status func() transcode.JobStatus
		err    error
		kept   bool // transient: the candidate stays
	}{
		{name: "retention ended", status: func() transcode.JobStatus { return transcode.JobStatus{Status: transcode.StatusExpired} }},
		{name: "worker forgot the job", err: &httpclient.HTTPError{StatusCode: 404, Code: transcode.WorkerErrorNotFound}},
		{name: "job cancelled", status: func() transcode.JobStatus { return transcode.JobStatus{Status: transcode.StatusCancelled} }},
		// The worker refuses to publish a job that has not finished encoding, so
		// a plan that took it for uploadable would promise what it cannot do.
		{name: "not retained yet", status: func() transcode.JobStatus {
			return transcode.JobStatus{Status: transcode.StatusRunning, FinalizationState: "pending"}
		}},
		{name: "worker unreachable", err: &httpclient.UncertainError{Op: "status", JobID: "job-source", Err: errors.New("connection refused")}, kept: true},
		// A copy that was asked for and ended badly is not a verdict on the
		// candidate: it may have left bytes on the share, so the owner decides.
		{name: "an earlier copy failed", status: func() transcode.JobStatus {
			return transcode.JobStatus{Status: transcode.StatusFailed, Error: "copy retries exhausted"}
		}, kept: true},
		{name: "an earlier copy needs intervention", status: func() transcode.JobStatus {
			st := transcode.JobStatus{Status: transcode.StatusRunning, Error: "share unreachable"}
			st.RecoveryRequired = true
			return st
		}, kept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rp := newRetainedPromotion(t)
			rp.w.statusFunc = func(context.Context, string) (transcode.JobStatus, error) {
				if tc.err != nil {
					return transcode.JobStatus{}, tc.err
				}
				return tc.status(), nil
			}
			r := runLocalPromotion(t, rp.promotionHarness)
			if r.Status != StatusFailed {
				t.Fatalf("plan: %+v", r)
			}
			released := rp.w.releases()
			if tc.kept && (len(released) != 0 || !candidateIsOnWorker(rp.sourceState())) {
				t.Fatalf("an unreachable worker released the candidate: %v", released)
			}
			if !tc.kept && (len(released) != 1 || !getBool(rp.sourceState(), candidateRemovedKey)) {
				t.Fatalf("a candidate that can never be uploaded stayed on the worker: %v", released)
			}
		})
	}
}

func TestPromotionPlanOfARetainedCandidateWithoutAttestationIsRefused(t *testing.T) {
	rp := newRetainedPromotion(t)
	inst := mustInstance(t, rp.st, "source-transcode")
	ec := parseExecutionContext(inst, rp.engine)
	delete(ec.State, "candidate_sha256")
	inst.StateJSON = toJSON(ec.State)
	if err := rp.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	r := runLocalPromotion(t, rp.promotionHarness)
	if r.Status != StatusFailed || !strings.Contains(r.Error, "attestation") {
		t.Fatalf("plan: %+v", r)
	}
	if len(rp.w.published) != 0 {
		t.Fatal("asked the worker to upload an unattested candidate")
	}
}

func TestRejectingARetainedPromotionReleasesTheCandidate(t *testing.T) {
	rp := newRetainedPromotion(t)
	r := runLocalPromotion(t, rp.promotionHarness)
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	r = rp.resume(r.ID, "reject")
	if r.Status != StatusFailed {
		t.Fatalf("reject: %+v", r)
	}
	if got := rp.w.releases(); len(got) != 1 || got[0] != "job-source" {
		t.Fatalf("a rejected plan kept its retained candidate: %v", got)
	}
	if len(rp.w.published) != 0 {
		t.Fatal("a rejected plan uploaded the candidate")
	}
	state := rp.sourceState()
	if !getBool(state, candidateRemovedKey) || getString(state[candidateCleanupKey].(map[string]any), "reason") != candidateRetiredPromotionRefused {
		t.Fatalf("source: %v", state[candidateCleanupKey])
	}
}

func TestAChangedOriginalReleasesTheRetainedCandidateAtPlanTime(t *testing.T) {
	rp := newRetainedPromotion(t)
	if err := os.WriteFile(rp.original, []byte("edited after the encode"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runLocalPromotion(t, rp.promotionHarness)
	if r.Status != StatusFailed {
		t.Fatalf("plan: %+v", r)
	}
	if got := rp.w.releases(); len(got) != 1 {
		t.Fatalf("the candidate of a changed original stayed on the worker: %v", got)
	}
}

func TestAWorkerSetOnlyTakesANormalizedPublishPolicy(t *testing.T) {
	for policy, ok := range map[string]bool{"": true, transcode.PublishPolicyOnApproval: true, transcode.PublishPolicyOnEncode: false, "later": false, " on_approval": false} {
		_, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "m1", Executor: &mockTranscodeExecutor{}, Podcast: true, PublishPolicy: policy}})
		if (err == nil) != ok {
			t.Errorf("policy %q: %v", policy, err)
		}
	}
}

// --- review round: releases that do not go through ---------------------------

func holdLease(t *testing.T, e *Engine, id string) func() {
	t.Helper()
	_, release, err := e.claimExecution(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	return release
}

// A source that is busy for a moment must not keep its candidate on the worker.
func TestAReleaseWaitsBrieflyForABusyAttempt(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	w := newRetainedExecutor()
	e := retainedEngine(t, st, root, w, transcode.PublishPolicyOnApproval)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	seedRetainedAttempt(t, st, "attempt", StatusCompleted, original, nil, nil)
	release := holdLease(t, e, "attempt")
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	e.releaseRetainedChild(context.Background(), "attempt", candidateRetiredCancelled)
	if got := w.releases(); len(got) != 1 {
		t.Fatalf("a briefly busy attempt kept its candidate: %v", got)
	}
	if !getBool(parseExecutionContext(mustInstance(t, st, "attempt"), e).State, candidateRemovedKey) {
		t.Fatal("the release was not recorded")
	}
}

// Cancelling a promotion ends it for good: its attempt's candidate goes too.
func TestCancellingAPromotionReleasesTheRetainedCandidate(t *testing.T) {
	rp := newRetainedPromotion(t)
	r := runLocalPromotion(t, rp.promotionHarness)
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	if _, err := rp.engine.Cancel(context.Background(), r.ID, "not wanted"); err != nil {
		t.Fatal(err)
	}
	if got := rp.w.releases(); len(got) != 1 || got[0] != "job-source" {
		t.Fatalf("a cancelled promotion left the candidate on the worker: %v", got)
	}
	if !getBool(rp.sourceState(), candidateRemovedKey) {
		t.Fatal("the release was not recorded")
	}
	if len(rp.w.published) != 0 {
		t.Fatal("a cancelled promotion uploaded")
	}
}

// A promotion cancelled while the worker copies cannot drop the candidate. The
// copy lands on the share anyway: the attempt lists it, and discarding it
// removes the file.
func TestACandidateTheWorkerWasCopyingWhenItsPromotionWasCancelledCanBeDiscarded(t *testing.T) {
	rp := newRetainedPromotion(t)
	rp.w.publish = func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning, FinalizationState: "finalizing"}, nil
	}
	rp.w.release = func(context.Context, string) (transcode.ReleaseOutcome, error) { return transcode.ReleaseRefused, nil }
	r := runLocalPromotion(t, rp.promotionHarness)
	r = rp.resume(r.ID, "approve")
	if r.Status != StatusWaitingExternal {
		t.Fatalf("upload: %+v", r)
	}
	if _, err := rp.engine.Cancel(context.Background(), r.ID, "not wanted"); err != nil {
		t.Fatal(err)
	}
	if candidateCleanupOf(t, rp.engine, mustInstance(t, rp.st, "source-transcode"))["outcome"] != "kept" || getBool(rp.sourceState(), candidateRemovedKey) {
		t.Fatalf("a copy under way cannot be dropped: %v", rp.sourceState())
	}
	page, err := rp.engine.LeftoverCandidatesOf(context.Background(), []string{"source-transcode"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Location != candidateOnWorker {
		t.Fatalf("the candidate must stay listed: %+v", page.Items)
	}

	// The copy finishes: the file is on the share and the worker says so.
	writeCandidateFile(t, rp.candidate, string(rp.content))
	rp.w.release = func(context.Context, string) (transcode.ReleaseOutcome, error) {
		return transcode.ReleasePublished, nil
	}
	if _, err := rp.engine.DiscardLeftoverCandidate(context.Background(), "source-transcode"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(rp.candidate); !os.IsNotExist(err) {
		t.Fatalf("the copy of a cancelled promotion stayed on the share: %v", err)
	}
	if got := getString(rp.sourceState(), candidateLocationKey); got != candidateOnShare {
		t.Fatalf("location: %q", got)
	}
	if _, err := os.Stat(rp.original); err != nil {
		t.Fatalf("the original was touched: %v", err)
	}
}

// A promotion that ended for good while its worker could not be reached leaves
// a candidate nothing would offer: it is listed, and discarding it is allowed
// although a promotion references it.
func TestACandidateLeftByAnEndedPromotionIsListedAndDiscardable(t *testing.T) {
	rp := newRetainedPromotion(t)
	rp.w.statusFunc = func(context.Context, string) (transcode.JobStatus, error) {
		return transcode.JobStatus{Status: transcode.StatusExpired}, nil
	}
	reachable := false
	rp.w.release = func(context.Context, string) (transcode.ReleaseOutcome, error) {
		if !reachable {
			return "", &httpclient.UncertainError{Op: "release", JobID: "job-source", Err: errors.New("connection refused")}
		}
		return transcode.ReleaseDropped, nil
	}
	if r := runLocalPromotion(t, rp.promotionHarness); r.Status != StatusFailed {
		t.Fatalf("plan: %+v", r)
	}
	if getBool(rp.sourceState(), candidateRemovedKey) {
		t.Fatal("an unreachable worker released the candidate")
	}
	page, err := rp.engine.ListLeftoverCandidates(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ActionID != "source-transcode" || page.Items[0].Location != candidateOnWorker {
		t.Fatalf("the candidate is not offered: %+v", page.Items)
	}
	reachable = true
	if _, err := rp.engine.DiscardLeftoverCandidate(context.Background(), "source-transcode"); err != nil {
		t.Fatal(err)
	}
	if !getBool(rp.sourceState(), candidateRemovedKey) {
		t.Fatal("the discard was not recorded")
	}
	if page, _ := rp.engine.ListLeftoverCandidates(context.Background(), 0); len(page.Items) != 0 {
		t.Fatalf("a released candidate must not be listed: %+v", page.Items)
	}
}

// A promotion that is still alive keeps its candidate out of reach of Discard.
func TestAPendingPromotionStillProtectsItsRetainedCandidate(t *testing.T) {
	rp := newRetainedPromotion(t)
	if r := runLocalPromotion(t, rp.promotionHarness); r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	if page, _ := rp.engine.ListLeftoverCandidates(context.Background(), 0); len(page.Items) != 0 {
		t.Fatalf("a candidate under review is not a leftover: %+v", page.Items)
	}
	if _, err := rp.engine.DiscardLeftoverCandidate(context.Background(), "source-transcode"); err == nil {
		t.Fatal("discarded a candidate a pending promotion references")
	}
	if len(rp.w.releases()) != 0 {
		t.Fatal("released")
	}
}

// --- review round: the upload ------------------------------------------------

// A server that stopped after the file arrived but before the promotion
// recorded it must not ask a worker that may have forgotten the job.
func TestAnUploadThatAlreadyArrivedIsNotRequestedAgain(t *testing.T) {
	rp := newRetainedPromotion(t)
	r := runLocalPromotion(t, rp.promotionHarness)
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	writeCandidateFile(t, rp.candidate, string(rp.content))
	inst := mustInstance(t, rp.st, "source-transcode")
	ec := parseExecutionContext(inst, rp.engine)
	ec.State[candidateLocationKey] = candidateOnShare
	inst.StateJSON = toJSON(ec.State)
	if err := rp.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	rp.w.publish = func(context.Context, string) (transcode.JobStatus, error) {
		return transcode.JobStatus{}, &httpclient.HTTPError{StatusCode: 404, Code: transcode.WorkerErrorNotFound}
	}
	r = rp.resume(r.ID, "approve")
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %+v", r)
	}
	if len(rp.w.published) != 0 {
		t.Fatalf("a file that is on the share was asked for again: %v", rp.w.published)
	}
}

// A source that stays busy is a wait for the upload, not a failure of it.
func TestAnUploadWaitsForABusyAttemptInsteadOfFailing(t *testing.T) {
	old := retainedLeaseWait
	retainedLeaseWait = 50 * time.Millisecond
	t.Cleanup(func() { retainedLeaseWait = old })
	rp := newRetainedPromotion(t)
	r := runLocalPromotion(t, rp.promotionHarness)
	release := holdLease(t, rp.engine, "source-transcode")
	r = rp.resume(r.ID, "approve")
	if r.Status != StatusWaitingExternal {
		release()
		t.Fatalf("a busy attempt failed the promotion: %+v", r)
	}
	if _, err := os.Stat(rp.original); err != nil {
		t.Fatalf("the original was touched: %v", err)
	}
	release()
	r, err := rp.engine.Resume(context.Background(), r.ID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %+v", r)
	}
}
