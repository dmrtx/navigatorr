package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

// A transcode attempt's candidate lives at
// <source dir>/.navigatorr-candidates/<stem>.<job id><ext> until a promotion
// moves it. These keys record when a durable decision means nothing can
// promote it any more and its own files were cleaned up.
const (
	candidateRejectedKey = "candidate_rejected"
	candidateRemovedKey  = "candidate_removed"
	candidateCleanupKey  = "candidate_cleanup"

	candidateRejectedError = "transcode candidate rejected by user decision; original file remains untouched"
)

// Why a candidate can no longer be promoted.
const (
	candidateRetiredRejected   = "rejected"
	candidateRetiredCancelled  = "cancelled"
	candidateRetiredSuperseded = "superseded"
	candidateRetiredDiscarded  = "discarded"
	candidateRetiredReplaced   = "replaced"
)

func rejectedCandidateResult() StepResult {
	return StepResult{Status: StepFailed, Error: candidateRejectedError, Outputs: map[string]any{candidateRejectedKey: true}}
}

// ownCandidateFiles returns the exact files one attempt owns: the candidate
// path it recorded and the worker's partial of it. Anything that is not
// shaped like this attempt's own temporary file is refused, never guessed.
func ownCandidateFiles(state map[string]any) (candidate, partial string, err error) {
	candidate = getString(state, "candidate_path")
	jobID := getString(state, "job_id")
	original := getString(state, "resolved_path")
	if !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
		return "", "", fmt.Errorf("the recorded candidate path is not a clean absolute path")
	}
	if jobID == "" || original == "" {
		return "", "", fmt.Errorf("the attempt has no recorded job or original to match the candidate against")
	}
	dir := filepath.Dir(candidate)
	if filepath.Base(dir) != ".navigatorr-candidates" || filepath.Dir(dir) != filepath.Dir(filepath.Clean(original)) || !withinPromotionPath(filepath.Dir(original), candidate) || temporaryPromotionPath(original) {
		return "", "", fmt.Errorf("the candidate is not in the .navigatorr-candidates directory beside its original")
	}
	if candidate == filepath.Clean(original) || strings.Contains(filepath.ToSlash(candidate), "/.promotion-recovery/") {
		return "", "", fmt.Errorf("the recorded candidate path names an original or a recovery file")
	}
	base := filepath.Base(candidate)
	if !strings.HasSuffix(strings.TrimSuffix(base, filepath.Ext(base)), "."+jobID) {
		return "", "", fmt.Errorf("the candidate name does not carry this attempt's job id")
	}
	return candidate, candidate + ".partial." + jobID, nil
}

// candidatePromotionStarted refuses a candidate that a promotion, or a batch
// replacement review, references. Unreadable promotion state counts as
// started: the file is kept rather than guessed about.
func (e *Engine) candidatePromotionStarted(inst *store.ActionInstance, ec *ExecutionContext, candidate string) error {
	promotion, err := e.deps.Store.FindActionByIdempotencyKeySuffix("promote_transcode_candidate", ":"+inst.ID)
	if err != nil {
		return fmt.Errorf("promotion state could not be read: %v", err)
	}
	if promotion != nil {
		return fmt.Errorf("replacement %s references this candidate", promotion.ID)
	}
	parentID := getString(ec.Inputs, "parent_action_id")
	if parentID == "" {
		return nil
	}
	parent, err := e.deps.Store.GetActionInstanceIfExists(parentID)
	if err != nil {
		return fmt.Errorf("the batch replacement review could not be read: %v", err)
	}
	if parent == nil {
		return nil
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(parent.StateJSON), &state); err != nil {
		return fmt.Errorf("the batch replacement review could not be read: %v", err)
	}
	if state["batch_promotion_plan"] == nil {
		return nil
	}
	plan := getBatchPromotionPlan(state["batch_promotion_plan"])
	if plan == nil {
		return fmt.Errorf("the batch replacement review could not be read")
	}
	for _, member := range plan.Members {
		if member.SourceAction == inst.ID || filepath.Clean(member.CandidatePath) == candidate {
			return fmt.Errorf("the batch replacement review includes this candidate")
		}
	}
	return nil
}

// removeOwnCandidateFile removes one owned file, through the same root and
// symlink checks promotion uses, and returns what it removed. A missing file
// is already removed.
func (e *Engine) removeOwnCandidateFile(path string) (os.FileInfo, error) {
	if _, err := e.promotionPath(path, true); err != nil {
		return nil, err
	}
	info, err := e.promotionLstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if e.candidateRemoveHook != nil {
		e.candidateRemoveHook(path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return info, nil
}

// sameCandidateBytes reports whether the partial is another name for the
// candidate's bytes: a link-based publish whose cleanup of the partial name
// did not finish. The listing counts those bytes once.
func sameCandidateBytes(candidate, partial os.FileInfo) bool {
	return candidate != nil && partial != nil && os.SameFile(candidate, partial)
}

// removeOwnCandidate removes the attempt's candidate and partial and returns
// the bytes that removal freed. A name whose bytes another link still holds
// (the partial of a link-based publish, a hard link elsewhere) frees nothing
// until its last name goes. pruneDir also removes the candidate directory when
// it is left empty.
func (e *Engine) removeOwnCandidate(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext, pruneDir bool) (int64, error) {
	candidate, partial, err := ownCandidateFiles(ec.State)
	if err != nil {
		return 0, err
	}
	if err := e.candidatePromotionStarted(inst, ec, candidate); err != nil {
		return 0, err
	}
	var freed int64
	for _, path := range []string{candidate, partial} {
		if err := ctx.Err(); err != nil {
			return freed, err
		}
		info, err := e.removeOwnCandidateFile(path)
		if info != nil && candidateLastLink(info) {
			freed += info.Size()
		}
		if err != nil {
			return freed, err
		}
	}
	if pruneDir {
		_ = os.Remove(filepath.Dir(candidate)) // Empty-only; other candidates survive.
	}
	return freed, nil
}

// retireCandidate removes the candidate an attempt recorded, and the worker's
// partial of it, once a durable decision means nothing can promote it: the
// owner rejected it, the action was cancelled, or a batch settings revision
// replaced the attempt. The caller holds the action's execution lease and has
// already persisted that decision. Anything ambiguous is kept and the reason
// recorded; no outcome fails, retries or blocks the decision itself.
func (e *Engine) retireCandidate(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext, why string) {
	if inst.ActionName != "transcode_media" || getString(ec.State, "candidate_path") == "" || getBool(ec.State, candidateRemovedKey) {
		return
	}
	freed, err := e.removeOwnCandidate(ctx, inst, ec, true)
	e.recordCandidateCleanup(ctx, inst, ec, why, freed, err)
}

// DiscardLeftoverCandidate is the owner's approval to remove one listed
// leftover: the candidate and partial a finished attempt owns, through the
// same guards and removal as automatic cleanup. It holds the attempt's
// execution lease and re-checks at call time that nothing can promote the
// candidate and that no worker job can still write it. Every call checks the
// disk, not the recorded outcome, so a file a late publish left after an
// earlier removal is removed too; a file that is already gone counts as
// removed, and a call that finds nothing changes nothing. The candidate
// directory is left in place even when empty: a sibling job may be about to
// upload into it.
func (e *Engine) DiscardLeftoverCandidate(ctx context.Context, id string) (*ActionResult, error) {
	if !e.AllowDestructive() {
		return nil, fmt.Errorf("allow_destructive must be enabled for candidate removal")
	}
	notAttempt := fmt.Errorf("only a transcode attempt's own candidate can be removed")
	// An unknown id cannot take a lease, so it is refused by name first.
	if inst, err := e.deps.Store.GetActionInstanceIfExists(id); err != nil {
		return nil, err
	} else if inst == nil || inst.ActionName != "transcode_media" {
		return nil, notAttempt
	}
	ctx, release, err := e.claimExecution(ctx, id, false)
	if err != nil {
		return nil, err
	}
	defer release()
	inst, err := e.deps.Store.GetActionInstanceIfExists(id)
	if err != nil {
		return nil, err
	}
	if inst == nil || inst.ActionName != "transcode_media" {
		return nil, notAttempt
	}
	ec := parseExecutionContext(inst, e)
	reason, err := e.leftoverCandidateReason(inst, ec, map[string]map[string]bool{})
	if err != nil {
		return nil, err
	}
	if reason == "" {
		return nil, fmt.Errorf("this attempt's candidate can still be promoted or the action is not finished; nothing was removed")
	}
	if err := e.candidateWorkerStopped(ctx, ec); err != nil {
		return nil, fmt.Errorf("candidate file kept: %v", err)
	}
	recorded := getBool(ec.State, candidateRemovedKey)
	freed, err := e.removeOwnCandidate(ctx, inst, ec, false)
	if err == nil && freed == 0 && recorded {
		return e.Status(ctx, id)
	}
	e.recordCandidateCleanup(ctx, inst, ec, candidateRetiredDiscarded, freed, err)
	if err != nil {
		return nil, fmt.Errorf("candidate file kept: %v", err)
	}
	return e.Status(ctx, id)
}

// candidateNeedsWorkerCheck reports whether a worker job might still write
// the attempt's files: the job never reported done, or an earlier cleanup
// kept them (an unconfirmed cancellation among those reasons).
func candidateNeedsWorkerCheck(state map[string]any) bool {
	if getString(state, "job_id") == "" {
		return false
	}
	cleanup, _ := state[candidateCleanupKey].(map[string]any)
	return !getBool(state, "transcode_done") || getString(cleanup, "outcome") == "kept"
}

// candidateWorkerStopped asks the worker, when one might still write, whether
// the attempt's job has stopped. The execution lease stops only this server's
// executor; a worker job keeps uploading the partial and can publish the
// candidate after it is removed. Only a terminal status, or the worker saying
// it does not know the job, lets the files go. Any other status, an
// unreachable worker or no worker at all keeps them.
func (e *Engine) candidateWorkerStopped(ctx context.Context, ec *ExecutionContext) error {
	if !candidateNeedsWorkerCheck(ec.State) {
		return nil
	}
	const wait = "the worker job may still be writing; cancel it or wait"
	jobID := getString(ec.State, "job_id")
	if !e.workersConfigured() {
		return fmt.Errorf("%s (no transcode worker is configured to confirm job %s stopped)", wait, jobID)
	}
	// Only the job's own worker can say it stopped: another worker answering
	// not_found says nothing about it.
	st, err := e.actionWorker(ec).Status(ctx, jobID)
	if err != nil {
		if workerJobUnknown(err) {
			return nil
		}
		return fmt.Errorf("%s (status of job %s: %v)", wait, jobID, err)
	}
	switch st.Status {
	case transcode.StatusFailed, transcode.StatusCancelled, transcode.StatusCompleted:
		return nil
	}
	return fmt.Errorf("%s (the worker reports job %s as %q)", wait, jobID, st.Status)
}

// workerJobUnknown reports a definitive "no such job" from the worker: a 404
// whose envelope code is not_found. A bare 404 is not enough to delete on:
// older workers answered 404 for read errors on a live job, and a proxy or a
// misrouted base URL answers 404 too.
func workerJobUnknown(err error) bool {
	if httpclient.IsTransportUncertain(err) {
		return false
	}
	he, ok := transcodeHTTPError(err)
	return ok && he.StatusCode == 404 && he.Code == transcode.WorkerErrorNotFound
}

// recordCandidateCleanup persists the cleanup outcome and says it in the
// action's own message: the error of a rejected attempt, the reason of a
// cancelled one. A superseded completed attempt keeps its message.
func (e *Engine) recordCandidateCleanup(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext, why string, freed int64, cause error) {
	outcome := map[string]any{"reason": why, "at": time.Now().UTC().Format(time.RFC3339Nano)}
	note := "candidate file removed"
	if cause != nil {
		outcome["outcome"], outcome["detail"] = "kept", cause.Error()
		note = "candidate file kept: " + cause.Error()
	} else {
		outcome["outcome"], outcome["bytes_freed"] = "removed", freed
		ec.State[candidateRemovedKey], ec.State["candidate_removed_bytes"] = true, freed
		ec.Outputs[candidateRemovedKey], ec.Outputs["candidate_removed_bytes"] = true, freed
		if freed == 0 {
			note = "no candidate file was left"
		}
	}
	ec.State[candidateCleanupKey], ec.Outputs[candidateCleanupKey] = outcome, outcome
	switch inst.Status {
	case StatusFailed:
		var failure map[string]any
		if json.Unmarshal([]byte(inst.ErrorJSON), &failure) == nil && failure != nil {
			failure["error"] = withCandidateNote(getString(failure, "error"), note)
			inst.ErrorJSON = toJSON(failure)
		}
	case StatusCancelled:
		inst.WaitingReason = withCandidateNote(inst.WaitingReason, note)
	}
	inst.StateJSON, inst.OutputsJSON = toJSON(ec.State), toJSON(ec.Outputs)
	if err := e.updateInstance(ctx, inst); err != nil {
		log.Printf("action %s: recording candidate cleanup (%s): %v", inst.ID, note, err)
	}
}

// withCandidateNote replaces an earlier cleanup note, so a later attempt to
// remove a kept file restates the outcome instead of adding to it.
func withCandidateNote(message, note string) string {
	for _, earlier := range []string{"; candidate file ", "; no candidate file was left"} {
		if i := strings.Index(message, earlier); i >= 0 {
			message = message[:i]
		}
	}
	if message == "" {
		return note
	}
	return message + "; " + note
}

// retireSupersededCandidates removes the candidates of attempts a committed
// batch settings revision replaced. An attempt another executor holds right
// now is left for the leftover listing; nothing waits for it.
func (e *Engine) retireSupersededCandidates(ctx context.Context, ids []string) {
	for _, id := range ids {
		childCtx, release, err := e.claimExecution(ctx, id, false)
		if err != nil {
			log.Printf("action %s: superseded candidate kept: %v", id, err)
			continue
		}
		child, err := e.deps.Store.GetActionInstanceIfExists(id)
		if err == nil && child != nil && (child.Status == StatusCompleted || child.Status == StatusFailed || child.Status == StatusCancelled) {
			e.retireCandidate(childCtx, child, parseExecutionContext(child, e), candidateRetiredSuperseded)
		}
		release()
	}
}

// retireReplacedSiblings removes the candidates of the other finished attempts
// at the file a completed promotion just replaced. Batch admission lets the
// same file into two batches, so two attempts can each hold a candidate for one
// original; once either is promoted the original is gone and the other can
// never be promoted. An attempt still running is left alone: its candidate is
// removed when the owner rejects it. Nothing here fails the promotion.
func (e *Engine) retireReplacedSiblings(ctx context.Context, p *promotionState) {
	if e.deps.Store == nil || p == nil || p.OriginalPath == "" {
		return
	}
	original := filepath.Clean(p.OriginalPath)
	// The state is stored with toJSON, so the file name appears in it exactly
	// as this encodes it; only matching rows are parsed in full.
	needle := strings.Trim(toJSON(filepath.Base(original)), `"`)
	var siblings []string
	const pageSize = 100
	for offset := 0; ; offset += pageSize {
		instances, err := e.deps.Store.ListActionInstancesByName("transcode_media", pageSize, offset)
		if err != nil {
			log.Printf("action %s: listing replaced attempts: %v", p.SourceActionID, err)
			return
		}
		for i := range instances {
			inst := &instances[i]
			if inst.ID == p.SourceActionID || !strings.Contains(inst.StateJSON, needle) {
				continue
			}
			switch inst.Status {
			case StatusCompleted, StatusFailed, StatusCancelled:
				siblings = append(siblings, inst.ID)
			}
		}
		if len(instances) < pageSize {
			break
		}
	}
	for _, id := range siblings {
		childCtx, release, err := e.claimExecution(ctx, id, false)
		if err != nil {
			log.Printf("action %s: replaced candidate kept: %v", id, err)
			continue
		}
		child, err := e.deps.Store.GetActionInstanceIfExists(id)
		// The status is read again under the lease: a retry may have started
		// the attempt since it was listed.
		if err == nil && child != nil && (child.Status == StatusCompleted || child.Status == StatusFailed || child.Status == StatusCancelled) {
			ec := parseExecutionContext(child, e)
			// The path alone is not the file: a later attempt can be at the
			// same path after this promotion replaced it, so the original's
			// hash must match too.
			sameOriginal := filepath.Clean(getString(ec.State, "resolved_path")) == original && p.OriginalSHA != "" && getString(ec.State, "original_sha256") == p.OriginalSHA
			if sameOriginal && getString(ec.State, "candidate_path") != "" && !getBool(ec.State, candidateRemovedKey) {
				if err := e.candidateWorkerStopped(childCtx, ec); err != nil {
					e.recordCandidateCleanup(childCtx, child, ec, candidateRetiredReplaced, 0, err)
				} else {
					e.retireCandidate(childCtx, child, ec, candidateRetiredReplaced)
				}
			}
		}
		release()
	}
}

// LeftoverCandidate is a candidate file whose owning action is finished and
// was never promoted. Listing it deletes nothing. WorkerCheck marks an
// attempt whose worker job might still write: the listing does not ask the
// worker, and removal goes ahead only once the worker reports the job
// finished or unknown.
type LeftoverCandidate struct {
	ActionID     string `json:"action_id"`
	Status       string `json:"status"`
	Path         string `json:"path"`
	OriginalPath string `json:"original_path"`
	Bytes        int64  `json:"bytes"`
	PartialBytes int64  `json:"partial_bytes"`
	Reason       string `json:"reason"`
	WorkerCheck  bool   `json:"worker_check,omitempty"`
}

type LeftoverCandidatePage struct {
	Items      []LeftoverCandidate `json:"items"`
	Bytes      int64               `json:"page_bytes"`
	NextOffset *int                `json:"next_offset,omitempty"`
}

// ListLeftoverCandidates reports, read-only, candidate files that finished
// transcodes left in .navigatorr-candidates: rejected, cancelled, failed or
// superseded attempts nothing promotes. Ready candidates, promotions and
// recovery files are never listed. Inventory follows action ownership, never
// a directory sweep, so a page of actions may list nothing and still have a
// next page. DiscardLeftoverCandidate removes one listed item.
func (e *Engine) ListLeftoverCandidates(ctx context.Context, offset int) (*LeftoverCandidatePage, error) {
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required")
	}
	if offset < 0 {
		return nil, fmt.Errorf("offset must not be negative")
	}
	const pageSize = 100
	instances, err := e.deps.Store.ListActionInstancesByName("transcode_media", pageSize, offset)
	if err != nil {
		return nil, err
	}
	page := &LeftoverCandidatePage{Items: []LeftoverCandidate{}}
	if len(instances) == pageSize {
		next := offset + pageSize
		page.NextOffset = &next
	}
	replaced := map[string]map[string]bool{}
	for i := range instances {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if item, ok := e.leftoverOf(&instances[i], replaced); ok {
			page.Bytes += item.Bytes + item.PartialBytes
			page.Items = append(page.Items, item)
		}
	}
	return page, nil
}

// leftoverOf reports the candidate files one attempt left behind that nothing
// can promote, and whether it left any. replaced caches each batch's replaced
// attempts across calls.
func (e *Engine) leftoverOf(inst *store.ActionInstance, replaced map[string]map[string]bool) (LeftoverCandidate, bool) {
	ec := parseExecutionContext(inst, e)
	reason, err := e.leftoverCandidateReason(inst, ec, replaced)
	if reason == "" || err != nil {
		return LeftoverCandidate{}, false
	}
	candidate, partial, err := ownCandidateFiles(ec.State)
	if err != nil || e.candidatePromotionStarted(inst, ec, candidate) != nil {
		return LeftoverCandidate{}, false
	}
	item := LeftoverCandidate{ActionID: inst.ID, Status: inst.Status, Path: candidate, OriginalPath: getString(ec.State, "resolved_path"), Reason: reason, WorkerCheck: candidateNeedsWorkerCheck(ec.State)}
	var candidateInfo os.FileInfo
	for _, path := range []string{candidate, partial} {
		if _, err := e.promotionPath(path, false); err != nil {
			break
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if path == candidate {
			candidateInfo, item.Bytes = info, info.Size()
		} else if !sameCandidateBytes(candidateInfo, info) {
			item.PartialBytes = info.Size()
		}
	}
	return item, item.Bytes+item.PartialBytes > 0
}

// LeftoverCandidatesOf lists, read-only, the leftovers among the named actions:
// the attempts of one job. Ids that are not transcode attempts are ignored.
func (e *Engine) LeftoverCandidatesOf(ctx context.Context, ids []string) (*LeftoverCandidatePage, error) {
	if e.deps.Store == nil {
		return nil, fmt.Errorf("maintenance store is required")
	}
	page := &LeftoverCandidatePage{Items: []LeftoverCandidate{}}
	replaced := map[string]map[string]bool{}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		inst, err := e.deps.Store.GetActionInstanceIfExists(id)
		if err != nil {
			return nil, err
		}
		if inst == nil || inst.ActionName != "transcode_media" {
			continue
		}
		if item, ok := e.leftoverOf(inst, replaced); ok {
			page.Bytes += item.Bytes + item.PartialBytes
			page.Items = append(page.Items, item)
		}
	}
	return page, nil
}

// leftoverCandidateReason says why a finished attempt's candidate can no
// longer be promoted, or "" when it still can (or the action is active).
// replaced caches each batch's replaced attempts for one listing.
func (e *Engine) leftoverCandidateReason(inst *store.ActionInstance, ec *ExecutionContext, replaced map[string]map[string]bool) (string, error) {
	if getString(ec.State, "candidate_path") == "" {
		return "", nil
	}
	switch inst.Status {
	case StatusCancelled:
		return "the action was cancelled; a cancelled transcode is never promoted", nil
	case StatusFailed:
		if strings.Contains(inst.ErrorJSON, "transcode candidate rejected by user decision") {
			return "the candidate was rejected by user decision", nil
		}
		return "the action failed; nothing promotes a failed transcode unless it is retried", nil
	case StatusCompleted:
		parentID := getString(ec.Inputs, "parent_action_id")
		if parentID == "" {
			return e.unpromotableOriginalReason(ec), nil
		}
		if replaced[parentID] == nil {
			attempts, err := e.replacedBatchAttempts(parentID)
			if err != nil {
				return "", err
			}
			replaced[parentID] = attempts
		}
		if replaced[parentID][inst.ID] {
			return "a batch settings revision replaced this attempt", nil
		}
		return e.unpromotableOriginalReason(ec), nil
	}
	return "", nil
}

// replacedOriginalReason names a finished attempt whose original another
// attempt's promotion replaced and whose candidate cleanup kept the file, so
// it stays listed instead of being offered for a promotion that cannot pass.
func replacedOriginalReason(ec *ExecutionContext) string {
	cleanup, _ := ec.State[candidateCleanupKey].(map[string]any)
	if getString(cleanup, "reason") == candidateRetiredReplaced && getString(cleanup, "outcome") == "kept" {
		return "another attempt's promotion replaced this attempt's original"
	}
	return ""
}

// unpromotableOriginalReason names a finished attempt whose original is no
// longer the file it encoded, so a promotion would refuse it. It covers
// attempts that finished before sibling retirement existed, which no cleanup
// record mentions. The original is only compared by size: a size that differs
// from the one recorded when the attempt started is never the same bytes,
// whereas a modification time can move without the content changing. An
// original that is simply gone counts too, since the candidate file this is
// asked about was itself found on the same share. Anything that cannot be read
// is not evidence and keeps the candidate listed as promotable.
func (e *Engine) unpromotableOriginalReason(ec *ExecutionContext) string {
	if reason := replacedOriginalReason(ec); reason != "" {
		return reason
	}
	original := getString(ec.State, "resolved_path")
	size := getInt64(ec.State, "original_size")
	if original == "" || size <= 0 {
		return ""
	}
	if _, err := e.promotionPath(original, false); err != nil {
		return ""
	}
	info, err := os.Lstat(original)
	switch {
	case err == nil && info.Mode().IsRegular() && info.Size() != size:
		return "the original is no longer the file this attempt encoded (its size changed)"
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return "the original this attempt encoded no longer exists"
	}
	return ""
}

// replacedBatchAttempts returns the children a batch's attempt history
// records: each was replaced by a later attempt of the same file.
func (e *Engine) replacedBatchAttempts(batchID string) (map[string]bool, error) {
	out := map[string]bool{}
	parent, err := e.deps.Store.GetActionInstanceIfExists(batchID)
	if err != nil || parent == nil {
		return out, err
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(parent.StateJSON), &state); err != nil {
		return nil, err
	}
	history, _ := state["batch_attempt_history"].(map[string]any)
	for _, rows := range history {
		var attempts []store.TranscodeBatchItem
		_ = json.Unmarshal([]byte(toJSON(rows)), &attempts)
		for _, attempt := range attempts {
			if attempt.ChildActionID != "" {
				out[attempt.ChildActionID] = true
			}
		}
	}
	return out, nil
}
