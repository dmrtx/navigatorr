package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// Approvals a file's replacement can carry before its plan exists. They are
// never inputs: only the server grants them (store.GrantReplacementApproval),
// from POST /api/maintenance/replace or a job that replaces each file when
// done, and the first promotion of the file to reach its approval uses one.
// Either still stops at a regrab warning; a batch-linked promotion ignores
// both.
const (
	// ApprovalReviewed: a person approved this file after seeing its result.
	ApprovalReviewed = "reviewed"
	// ApprovalAutomatic: the file's job or batch replaces each file when it is
	// done. Checked again at approval: switched off by then, the promotion
	// waits for a decision like any other.
	ApprovalAutomatic = "automatic"
)

// ErrReplaceWhenDoneFinished refuses a change to a job that has ended.
var ErrReplaceWhenDoneFinished = errors.New("this job has finished; replace each ready file from its review")

// autoReplaceAttempts bounds how often a failed automatic submission is
// tried again before the file is left for its review.
const autoReplaceAttempts = 5

// normalizeReplaceWhenDone checks replace_when_done and, on a job or batch
// submitted without it, freezes the server's default into its inputs. A
// batch's file takes the batch's value, so it is left as it came.
func (e *Engine) normalizeReplaceWhenDone(name string, inputs map[string]any) error {
	if name != "transcode_media" && name != "transcode_batch" {
		return nil
	}
	if raw, present := inputs["replace_when_done"]; present {
		if _, ok := raw.(bool); !ok {
			return inputErr("replace_when_done", "replace_when_done must be a boolean")
		}
		return nil
	}
	if name == "transcode_media" && getString(inputs, "parent_action_id") != "" || getBool(inputs, "dry_run") || e.deps.Store == nil {
		return nil
	}
	value, err := e.deps.Store.ReplaceWhenDoneDefault()
	if err != nil {
		return err
	}
	inputs["replace_when_done"] = value
	return nil
}

// ReplaceWhenDone says whether a job replaces its file by itself once the
// file is done: the value it was last switched to, else its batch's switch,
// else the batch's submitted value, else its own. A preview never does.
func (e *Engine) ReplaceWhenDone(inst *store.ActionInstance) bool {
	if inst == nil || e.deps.Store == nil {
		return false
	}
	switched, err := e.deps.Store.ListReplaceWhenDone()
	if err != nil {
		return false
	}
	return e.replaceWhenDoneWith(inst, switched, 0)
}

func (e *Engine) replaceWhenDoneWith(inst *store.ActionInstance, switched map[string]bool, depth int) bool {
	inputs := decodeInputs(inst.InputsJSON)
	return ResolveReplaceWhenDone(inst.ID, inst.ActionName, inputs, switched, func(id string) (string, map[string]any, bool) {
		if depth > 2 {
			return "", nil, false
		}
		parent, err := e.deps.Store.GetActionInstanceIfExists(id)
		if err != nil || parent == nil {
			return "", nil, false
		}
		return parent.ActionName, decodeInputs(parent.InputsJSON), true
	})
}

// ResolveReplaceWhenDone is the one rule both the engine and the lists read
// replace_when_done by. parent reads a batch's name and inputs; a batch that
// cannot be read still lends its switch, which is kept apart from it.
func ResolveReplaceWhenDone(id, name string, inputs map[string]any, switched map[string]bool, parent func(string) (string, map[string]any, bool)) bool {
	if getBool(inputs, "dry_run") {
		return false
	}
	if value, ok := switched[id]; ok {
		return value
	}
	if parentID := getString(inputs, "parent_action_id"); name == "transcode_media" && parentID != "" {
		if value, ok := switched[parentID]; ok {
			return value
		}
		if parentName, parentInputs, ok := parent(parentID); ok && parentName == "transcode_batch" {
			return !getBool(parentInputs, "dry_run") && getBool(parentInputs, "replace_when_done")
		}
	}
	return getBool(inputs, "replace_when_done")
}

// SetReplaceWhenDone switches a job or batch. Switching on applies to files
// that finish from now on and needs a job that has not ended. Switching off
// stops every automatic replacement not yet approved; one already under way
// finishes. It is allowed on an ended job while one of its automatic
// replacements has not been approved yet, which then waits for a decision.
func (e *Engine) SetReplaceWhenDone(ctx context.Context, id string, value bool) (*store.ActionInstance, error) {
	e.replaceMu.Lock()
	defer e.replaceMu.Unlock()
	inst, err := e.deps.Store.GetActionInstanceIfExists(id)
	if err != nil {
		return nil, err
	}
	if inst == nil || inst.ActionName != "transcode_media" && inst.ActionName != "transcode_batch" {
		return nil, fmt.Errorf("job is unavailable")
	}
	files := []string{inst.ID}
	if inst.ActionName == "transcode_batch" {
		items, err := e.deps.Store.ListTranscodeBatchItems(inst.ID)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if item.ChildActionID != "" {
				files = append(files, item.ChildActionID)
			}
		}
	}
	switch inst.Status {
	case StatusCompleted, StatusFailed, StatusCancelled:
		outstanding := false
		for _, file := range files {
			if e.autoReplacementOutstanding(file) {
				outstanding = true
			}
		}
		if value || !outstanding {
			return nil, ErrReplaceWhenDoneFinished
		}
	}
	if err := e.deps.Store.SetReplaceWhenDone(id, value); err != nil {
		return nil, err
	}
	if !value {
		// Not submitted yet: it never will be. Submitted and not approved: it
		// waits for a decision when it reaches its approval.
		for _, file := range files {
			_ = e.deps.Store.DoneAutoReplacePending(file)
		}
	}
	return inst, nil
}

// autoReplacementOutstanding says a file's automatic replacement is still to
// be submitted or has not been approved yet.
func (e *Engine) autoReplacementOutstanding(transcodeID string) bool {
	if pending, _, err := e.deps.Store.AutoReplacePending(transcodeID); err == nil && pending {
		return true
	}
	approval, err := e.deps.Store.GetReplacementApproval(transcodeID)
	if err != nil || approval == nil || approval.Approval != ApprovalAutomatic || approval.Used {
		// Used: its promotion went ahead on it, even if not saved yet.
		return false
	}
	prior, err := e.priorPromotion(transcodeID)
	if err != nil || prior == nil {
		return false
	}
	switch prior.Status {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return false
	}
	return !promotionApproved(prior)
}

// promotionApproved says a promotion is past its approval.
func promotionApproved(inst *store.ActionInstance) bool {
	if inst.CurrentStep > 1 {
		return true
	}
	p, err := loadPromotion(&ExecutionContext{State: decodeInputs(inst.StateJSON)})
	return err == nil && p.Approved
}

// priorPromotion is the file's own replacement under any library, nil when
// none.
func (e *Engine) priorPromotion(transcodeID string) (*store.ActionInstance, error) {
	for _, service := range []string{"sonarr", "radarr", "filesystem"} {
		old, err := e.deps.Store.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:"+service+":"+transcodeID)
		if err != nil || old != nil {
			return old, err
		}
	}
	return nil, nil
}

// autoReplacement says whether a transcode that just finished replaces its
// file by itself, and why not when the option is on and it cannot. The full
// check runs again, under the replacement lock, when it is submitted.
func (e *Engine) autoReplacement(inst *store.ActionInstance, ec *ExecutionContext) (replace bool, reason string) {
	if inst.ActionName != "transcode_media" || !e.ReplaceWhenDone(inst) {
		return false, ""
	}
	if !e.AllowDestructive() {
		return false, "replacement is disabled in the server settings"
	}
	// The job is saved as completed right after this; judge it as it will be.
	done := *inst
	done.Status = StatusCompleted
	check := e.replacement(&done, ec.Inputs, ec.State, ec.Outputs, nil)
	switch check.Code {
	case "":
		return check.Prior == nil, ""
	case ReplaceNeedsLibrary:
		return false, "it needs a library choice: use its Replace"
	case ReplaceBatchUnavailable:
		return false, "its batch is unavailable"
	}
	return false, ""
}

// drainAutoReplacements submits the automatic replacements a crash or an
// error left behind.
func (e *Engine) drainAutoReplacements(ctx context.Context) {
	if e.deps.Store == nil {
		return
	}
	ids, err := e.deps.Store.ListAutoReplacePending()
	if err != nil {
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		e.submitAutoReplacement(ctx, id, false)
	}
}

// submitAutoReplacement submits one pending automatic replacement, under the
// replacement lock and after every check again. leased is true while the
// caller holds the transcode's execution lease (its own completion). A note
// on the job, when there is one, is written after the lock is released.
func (e *Engine) submitAutoReplacement(ctx context.Context, id string, leased bool) {
	loop := ctx // the reconciler's: a note still waiting stops with it
	ctx = context.WithoutCancel(ctx)
	note := e.submitAutoReplacementLocked(ctx, id)
	switch {
	case note == "":
	case leased:
		e.noteAutoReplaceSkipped(ctx, id, note, true)
	default:
		// From the reconciler: waiting for the job's lease must not hold up
		// the tick, so the note is written on its own.
		e.notes.Add(1)
		go func() {
			defer e.notes.Done()
			e.noteAutoReplaceSkipped(loop, id, note, false)
		}()
	}
}

func (e *Engine) submitAutoReplacementLocked(ctx context.Context, id string) (note string) {
	e.replaceMu.Lock()
	defer e.replaceMu.Unlock()
	pending, attempts, err := e.deps.Store.AutoReplacePending(id)
	if err != nil || !pending {
		return ""
	}
	inst, err := e.deps.Store.GetActionInstanceIfExists(id)
	if err != nil {
		return ""
	}
	if inst == nil || inst.Status == StatusFailed || inst.Status == StatusCancelled {
		_ = e.deps.Store.DoneAutoReplacePending(id)
		return ""
	}
	if inst.Status != StatusCompleted {
		return "" // it completes again and is judged then
	}
	if !e.ReplaceWhenDone(inst) {
		_ = e.deps.Store.DoneAutoReplacePending(id)
		return ""
	}
	if !e.AllowDestructive() {
		_ = e.deps.Store.DoneAutoReplacePending(id)
		return "replacement is disabled in the server settings"
	}
	check := e.replacement(inst, decodeInputs(inst.InputsJSON), decodeInputs(inst.StateJSON), decodeInputs(inst.OutputsJSON), nil)
	if check.Code != "" || check.Prior != nil {
		// Another job replaces it, or it is no longer ready: nothing to do.
		_ = e.deps.Store.DoneAutoReplacePending(id)
		if check.Code == ReplaceNeedsLibrary {
			return "it needs a library choice: use its Replace"
		}
		return ""
	}
	if e.beforeAutoEnqueue != nil {
		err = e.beforeAutoEnqueue(id)
	}
	var r *ActionResult
	if err == nil {
		r, err = e.Enqueue(WithOrigin(ctx, "automatic"), "promote_transcode_candidate", check.Inputs, "auto-replace:"+id)
	}
	if err != nil {
		log.Printf("replace when done: %s: %v", id, err)
		_ = e.deps.Store.FailAutoReplacePending(id, err.Error())
		if attempts+1 >= autoReplaceAttempts {
			_ = e.deps.Store.DoneAutoReplacePending(id)
		}
		return "the automatic replacement could not be started: " + err.Error()
	}
	// Still under the lock, so the promotion cannot reach its approval first.
	_ = e.deps.Store.DoneAutoReplacePending(id)
	e.WakeReconciler()
	if err := e.deps.Store.GrantReplacementApproval(id, ApprovalAutomatic, r.ID); err != nil {
		return "the automatic approval could not be recorded, so its replacement waits for a decision: " + err.Error()
	}
	return ""
}

// noteAutoReplaceSkipped says on a finished job why it was not replaced by
// itself. Never under the replacement lock: it can wait for the job's lease.
func (e *Engine) noteAutoReplaceSkipped(ctx context.Context, id, why string, leased bool) {
	if !leased {
		lease, release, err := e.claimExecutionWithin(ctx, id, 30*time.Second)
		if err != nil {
			log.Printf("replace when done: %s: note not saved: %s", id, why)
			return
		}
		defer release()
		ctx = lease
	}
	inst, err := e.deps.Store.GetActionInstanceIfExists(id)
	if err != nil || inst == nil {
		return
	}
	outputs := decodeInputs(inst.OutputsJSON)
	if outputs == nil {
		outputs = map[string]any{}
	}
	outputs["replace_when_done_skipped"] = why
	inst.OutputsJSON = toJSON(outputs)
	_ = e.updateInstance(ctx, inst)
}

// Why a file cannot be replaced now, as POST /api/maintenance/replace lists it.
const (
	ReplaceUnavailable      = "unavailable"          // no such file job
	ReplaceNotFinished      = "not_finished"         // still queued, encoding or waiting
	ReplaceNoResult         = "no_result"            // no new file to replace with
	ReplaceKeptOriginal     = "kept_original"        // the owner kept the original
	ReplacePreview          = "preview"              // a preview never replaces
	ReplaceStarted          = "replacement_started"  // its replacement is past approval or done
	ReplaceInBatchReview    = "in_batch_review"      // its batch's replacement plan holds it
	ReplaceRetryPending     = "retry_pending"        // its batch encodes it again
	ReplaceBatchUnavailable = "batch_unavailable"    // its batch was cancelled or is gone
	ReplaceNeedsLibrary     = "needs_library_choice" // its library is not known: replace it from its own review
	ReplacePlanChanged      = "plan_changed"         // its planned replacement is not the plan the client sent
	ReplaceNeedsDecision    = "needs_decision"       // its plan carries a regrab warning: decide on it in its review
	// ReplaceNeedsReview: a replacement that was rejected or refused before
	// its approval is planned again and waits; show its new plan, then send
	// its plan_digest.
	ReplaceNeedsReview = "needs_review"
)

// How a replacement of a file goes ahead.
const (
	replaceNew     = "new"          // a new replacement
	replaceQueued  = "queued"       // one submitted, not approved yet
	replaceWaiting = "waiting_plan" // one waiting on the plan the client sent
	replaceRetry   = "retry"        // one rejected or refused before approval, planned again
)

// replacementCheck is a file's replacement as it stands: Code is "" when it
// can go ahead. Prior is the file's replacement that already exists and can
// still be approved (How says how); Inputs is a new one's.
type replacementCheck struct {
	Code   string
	How    string
	Inputs map[string]any
	Prior  *store.ActionInstance
	Digest string // the plan Prior waits on
	Regrab bool   // Prior's plan carries a regrab warning
}

// replacement is the promotion that replaces a finished transcode's original
// with its new file, or why it cannot. archives, when given, is the
// kept-original record.
func (e *Engine) replacement(inst *store.ActionInstance, inputs, state, outputs map[string]any, archives map[string]string) replacementCheck {
	refuse := func(code string) replacementCheck { return replacementCheck{Code: code} }
	if inst == nil || inst.ActionName != "transcode_media" {
		return refuse(ReplaceUnavailable)
	}
	if getBool(inputs, "dry_run") {
		return refuse(ReplacePreview)
	}
	if inst.Status != StatusCompleted {
		if inst.Status == StatusFailed || inst.Status == StatusCancelled {
			return refuse(ReplaceNoResult)
		}
		return refuse(ReplaceNotFinished)
	}
	if getBool(state, "skip_transcode") || !getBool(outputs, "original_intact") || getString(outputs, "candidate_path") == "" {
		return refuse(ReplaceNoResult)
	}
	if getBool(outputs, candidateRemovedKey) || archives[inst.ID] != "" {
		return refuse(ReplaceKeptOriginal)
	}
	var parentInputs map[string]any
	if parentID := getString(inputs, "parent_action_id"); parentID != "" {
		parent, _ := e.deps.Store.GetActionInstanceIfExists(parentID)
		if parent == nil || parent.ActionName != "transcode_batch" || parent.Status == StatusCancelled {
			return refuse(ReplaceBatchUnavailable)
		}
		if archives[parent.ID] != "" && (parent.Status == StatusCompleted || parent.Status == StatusFailed) {
			return refuse(ReplaceKeptOriginal)
		}
		parentInputs = decodeInputs(parent.InputsJSON)
		parentState := decodeInputs(parent.StateJSON)
		if getBool(parentInputs, "dry_run") {
			return refuse(ReplacePreview)
		}
		if key := getString(inputs, "batch_item_key"); key != "" && stateObject(parentState, "batch_retry_pending")[key] == true {
			return refuse(ReplaceRetryPending)
		}
		if plan := getBatchPromotionPlan(parentState["batch_promotion_plan"]); plan != nil {
			for _, member := range plan.Members {
				if member.SourceAction == inst.ID {
					return refuse(ReplaceInBatchReview)
				}
			}
		}
	}
	prior, err := e.priorPromotion(inst.ID)
	if err != nil {
		return refuse(ReplaceUnavailable)
	}
	if prior != nil {
		if getString(decodeInputs(prior.InputsJSON), "batch_promote_parent_id") != "" {
			return refuse(ReplaceInBatchReview)
		}
		state := decodeInputs(prior.StateJSON)
		switch {
		case prior.Status == StatusWaitingDecision && prior.CurrentStep == 1:
			p, _ := loadPromotion(&ExecutionContext{State: state})
			return replacementCheck{How: replaceWaiting, Prior: prior, Digest: PromotionPlanDigest(state["promotion"]), Regrab: p != nil && p.RegrabWarning != ""}
		case prior.Status == StatusFailed && prior.CurrentStep <= 1:
			// Rejected, or refused before its approval: nothing was changed.
			return replacementCheck{How: replaceRetry, Prior: prior}
		case (prior.Status == StatusPending || prior.Status == StatusRunning) && !promotionApproved(prior):
			return replacementCheck{How: replaceQueued, Prior: prior}
		}
		return refuse(ReplaceStarted)
	}
	out := map[string]any{"transcode_action_id": inst.ID}
	service, key, id := e.replacementLibrary(inputs, parentInputs)
	if service == "" {
		return refuse(ReplaceNeedsLibrary)
	}
	out["service"] = service
	if key != "" {
		out[key] = id
	}
	return replacementCheck{How: replaceNew, Inputs: out}
}

// replacementLibrary is the library a file is replaced through, as its
// Replace button reads it: the job's library, its batch's series or files,
// else the files alone when no library service is configured.
func (e *Engine) replacementLibrary(inputs, parentInputs map[string]any) (service, key string, id int) {
	library := func(service string, raw any) (string, string, int) {
		n := libraryID(raw)
		switch {
		case n <= 0:
			return "", "", 0
		case service == "sonarr":
			return service, "series_id", n
		case service == "radarr":
			return service, "movie_id", n
		}
		return "", "", 0
	}
	if lib, ok := inputs["library_context"].(map[string]any); ok {
		if s, k, n := library(getString(lib, "service"), lib["id"]); s != "" {
			return s, k, n
		}
	}
	service = getString(inputs, "service")
	raw := inputs["series_id"]
	if service == "radarr" {
		raw = inputs["movie_id"]
	}
	if raw == nil {
		raw = inputs["media_id"]
	}
	if s, k, n := library(service, raw); s != "" {
		return s, k, n
	}
	if parentInputs != nil {
		if s, k, n := library(getString(parentInputs, "service"), parentInputs["series_id"]); s != "" {
			return s, k, n
		}
		if _, files := parentInputs["paths"]; files {
			return "filesystem", "", 0
		}
	}
	if e.deps.Registry == nil {
		return "filesystem", "", 0
	}
	for _, name := range e.deps.Registry.List() {
		if name == "sonarr" || name == "radarr" {
			return "", "", 0
		}
	}
	return "filesystem", "", 0
}

func libraryID(raw any) int {
	switch v := raw.(type) {
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil && n > 0 && n <= math.MaxInt32 {
			return n
		}
	case float64:
		if v > 0 && v <= math.MaxInt32 && math.Trunc(v) == v {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case json.Number:
		n, err := strconv.Atoi(string(v))
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func decodeInputs(raw string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	return m
}

// ReplaceRefused is one file POST /api/maintenance/replace could not replace.
type ReplaceRefused struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
	// PlanDigest is the plan the file's planned replacement waits on, when
	// the request sent none or another one: show it, then send its digest.
	PlanDigest string `json:"plan_digest,omitempty"`
}

// ReplaceFile is one file's outcome. Started is false only in a partial
// answer, for a file whose submission failed.
type ReplaceFile struct {
	ID                  string `json:"id"`
	Started             bool   `json:"started"`
	ReplacementActionID string `json:"replacement_action_id,omitempty"`
	// Approved: new (a new replacement), queued (one already submitted, now
	// approved), waiting_plan (one waiting on the plan sent, approved),
	// "" with reason needs_review (one rejected or refused before approval,
	// planned again and waiting for a decision).
	Approved string `json:"approved,omitempty"`
	// Reason is needs_review for a file that started without an approval:
	// its replacement waits on a plan the client has not seen.
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ReplaceRequest is what a person reviewed: the files, and for a file whose
// replacement already waits on a plan, the digest of the plan shown.
type ReplaceRequest struct {
	IDs     []string          `json:"ids"`
	Digests map[string]string `json:"digests,omitempty"`
	Key     string            `json:"key"`
}

// ReplaceAnswer is the HTTP status and body POST /api/maintenance/replace
// answers with.
type ReplaceAnswer struct {
	Status int
	Body   map[string]any
}

// ReplaceReviewed replaces exactly the files a person reviewed, all of them
// or none. Every file is checked and submitted under the replacement lock,
// so an automatic replacement or a batch plan cannot claim one in between.
// When a submission still fails partway, the answer is 207 and says which
// files started. The answer to a key is stored: an exact replay gets it
// again, the same key with other files gets 409 key_reused.
func (e *Engine) ReplaceReviewed(ctx context.Context, req ReplaceRequest) (ReplaceAnswer, error) {
	ctx = context.WithoutCancel(ctx)
	if !e.AllowDestructive() {
		return ReplaceAnswer{}, fmt.Errorf("replacement is disabled in the server settings")
	}
	canonical := canonicalReplaceRequest(req)
	e.replaceMu.Lock()
	defer e.replaceMu.Unlock()
	if receipt, err := e.deps.Store.GetReplaceReceipt(req.Key); err != nil {
		return ReplaceAnswer{}, err
	} else if receipt != nil {
		if receipt.Request != canonical {
			return ReplaceAnswer{Status: 409, Body: map[string]any{"error": "this submission key was used for other files; use a new key", "code": "key_reused"}}, nil
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(receipt.Response), &body)
		return ReplaceAnswer{Status: receipt.Status, Body: body}, nil
	}
	archives, err := e.deps.Store.MaintenanceArchives()
	if err != nil {
		return ReplaceAnswer{}, err
	}
	type planned struct {
		id    string
		check replacementCheck
	}
	var work []planned
	ready, refused := []string{}, []ReplaceRefused{}
	for _, id := range req.IDs {
		inst, err := e.deps.Store.GetActionInstanceIfExists(id)
		if err != nil {
			return ReplaceAnswer{}, err
		}
		check := replacementCheck{Code: ReplaceUnavailable}
		if inst != nil {
			check = e.replacement(inst, decodeInputs(inst.InputsJSON), decodeInputs(inst.StateJSON), decodeInputs(inst.OutputsJSON), archives)
		}
		if check.Code == "" && check.How == replaceWaiting {
			switch {
			case check.Digest == "" || req.Digests[id] != check.Digest:
				refused = append(refused, ReplaceRefused{ID: id, Reason: ReplacePlanChanged, PlanDigest: check.Digest})
				continue
			case check.Regrab:
				refused = append(refused, ReplaceRefused{ID: id, Reason: ReplaceNeedsDecision, PlanDigest: check.Digest})
				continue
			}
		}
		if check.Code != "" {
			refused = append(refused, ReplaceRefused{ID: id, Reason: check.Code})
			continue
		}
		ready = append(ready, id)
		work = append(work, planned{id, check})
	}
	if len(refused) > 0 {
		return ReplaceAnswer{Status: 409, Body: map[string]any{"error": fmt.Sprintf("%d of the files cannot be replaced now; nothing was replaced", len(refused)), "code": "not_ready", "ready": ready, "not_ready": refused}}, nil
	}
	if e.afterReplaceCheck != nil {
		e.afterReplaceCheck()
	}
	files := make([]ReplaceFile, 0, len(work))
	started := 0
	for _, item := range work {
		result, err := e.submitReviewed(ctx, req, item.id, item.check)
		if err != nil {
			files = append(files, ReplaceFile{ID: item.id, Error: err.Error()})
			continue
		}
		started++
		files = append(files, result)
	}
	e.WakeReconciler()
	answer := ReplaceAnswer{Status: 202, Body: map[string]any{"replacements": files}}
	switch {
	case started == 0:
		return ReplaceAnswer{Status: 409, Body: map[string]any{"error": "no replacement could be started", "code": "not_started", "replacements": files}}, nil
	case started < len(work):
		answer.Status = 207
		answer.Body["error"] = fmt.Sprintf("%d of %d replacements started; the others did not", started, len(work))
		answer.Body["code"] = "partial"
	}
	// The answer is the one a replay gets, byte for byte.
	response, _ := json.Marshal(answer.Body)
	answer.Body = nil
	_ = json.Unmarshal(response, &answer.Body)
	if err := e.deps.Store.PutReplaceReceipt(req.Key, store.ReplaceReceipt{Request: canonical, Status: answer.Status, Response: string(response)}); err != nil {
		log.Printf("replace: receipt %s: %v", req.Key, err)
	}
	return answer, nil
}

// submitReviewed starts one reviewed file's replacement, under the lock.
func (e *Engine) submitReviewed(ctx context.Context, req ReplaceRequest, id string, check replacementCheck) (ReplaceFile, error) {
	switch check.How {
	case replaceWaiting:
		if _, err := e.QueueMaintenanceCommand(ctx, map[string]any{"id": check.Prior.ID, "kind": "resume", "decision": "approve", "plan_digest": check.Digest}, "replace-"+req.Key+"-"+check.Prior.ID); err != nil {
			return ReplaceFile{}, err
		}
		return ReplaceFile{ID: id, Started: true, ReplacementActionID: check.Prior.ID, Approved: replaceWaiting}, nil
	case replaceQueued:
		if err := e.deps.Store.GrantReplacementApproval(id, ApprovalReviewed, check.Prior.ID); err != nil {
			return ReplaceFile{}, err
		}
		return ReplaceFile{ID: id, Started: true, ReplacementActionID: check.Prior.ID, Approved: replaceQueued}, nil
	case replaceRetry:
		// Its new plan is one nobody saw: it is planned again and waits for
		// a decision, which the client takes after showing it.
		_ = e.deps.Store.RevokeReplacementApproval(id, check.Prior.ID)
		if err := e.replanPromotion(ctx, check.Prior.ID); err != nil {
			return ReplaceFile{}, err
		}
		if _, err := e.QueueMaintenanceCommand(ctx, map[string]any{"id": check.Prior.ID, "kind": "retry"}, "replace-"+req.Key+"-"+check.Prior.ID); err != nil {
			return ReplaceFile{}, err
		}
		return ReplaceFile{ID: id, Started: true, ReplacementActionID: check.Prior.ID, Reason: ReplaceNeedsReview}, nil
	}
	r, err := e.Enqueue(WithOrigin(ctx, "web"), "promote_transcode_candidate", check.Inputs, "replace:"+id)
	if err != nil {
		return ReplaceFile{}, err
	}
	// Still under the lock, so the promotion cannot reach its approval first.
	if err := e.deps.Store.GrantReplacementApproval(id, ApprovalReviewed, r.ID); err != nil {
		return ReplaceFile{ID: id, Started: true, ReplacementActionID: r.ID, Reason: ReplaceNeedsReview, Error: "the approval could not be recorded; the replacement waits for a decision: " + err.Error()}, nil
	}
	return ReplaceFile{ID: id, Started: true, ReplacementActionID: r.ID, Approved: replaceNew}, nil
}

// replanPromotion sends a promotion that failed at or before its approval
// back to its plan, so its retry measures the files again before the
// approval it is given.
func (e *Engine) replanPromotion(ctx context.Context, id string) error {
	lease, release, err := e.claimExecutionWithin(ctx, id, 2*time.Second)
	if err != nil {
		return err
	}
	defer release()
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		return err
	}
	if inst.Status != StatusFailed || inst.CurrentStep > 1 {
		return fmt.Errorf("the replacement changed; review it again")
	}
	inst.CurrentStep = 0
	return e.updateInstance(lease, inst)
}

func canonicalReplaceRequest(req ReplaceRequest) string {
	ids := append([]string(nil), req.IDs...)
	sort.Strings(ids)
	digests := map[string]string{}
	for _, id := range ids {
		if d := req.Digests[id]; d != "" {
			digests[id] = d
		}
	}
	b, _ := json.Marshal(map[string]any{"ids": ids, "digests": digests})
	return string(b)
}

// promotionApprovedAhead says whether a promotion was approved before its
// plan was made, and why not when it says no: a person approved the file
// after its review, or its job replaces each file when done and still does.
// The approval is bound to this promotion, under the replacement lock, so a
// switch made after this answer cannot undo it and one made before holds it.
func (e *Engine) promotionApprovedAhead(ec *ExecutionContext, p *promotionState) (bool, string) {
	e.replaceMu.Lock()
	defer e.replaceMu.Unlock()
	approval, err := e.deps.Store.GetReplacementApproval(p.SourceActionID)
	if err != nil || approval == nil || approval.PromoteActionID != ec.InstanceID {
		return false, ""
	}
	switch approval.Approval {
	case ApprovalReviewed:
	case ApprovalAutomatic:
		source, err := e.deps.Store.GetActionInstanceIfExists(p.SourceActionID)
		if err != nil || source == nil || !e.ReplaceWhenDone(source) {
			return false, "Replace automatically was switched off before this file was replaced"
		}
	default:
		return false, ""
	}
	if p.RegrabWarning != "" {
		return false, ""
	}
	// Marked used under the lock: a switch made after this sees it approved.
	used, err := e.deps.Store.UseReplacementApproval(p.SourceActionID, approval.Approval, ec.InstanceID)
	return err == nil && used, ""
}

// revokeUnusedApproval drops the approval a promotion held when its plan was
// rejected or it failed before its approval: a later replacement of the file
// needs one of its own.
func (e *Engine) revokeUnusedApproval(inst *store.ActionInstance) {
	if inst.ActionName != "promote_transcode_candidate" || inst.CurrentStep > 1 || e.deps.Store == nil {
		return
	}
	source := getString(decodeInputs(inst.InputsJSON), "transcode_action_id")
	if source == "" {
		return
	}
	e.replaceMu.Lock()
	defer e.replaceMu.Unlock()
	if a, err := e.deps.Store.GetReplacementApproval(source); err == nil && a != nil && a.PromoteActionID == inst.ID && !a.Used {
		_ = e.deps.Store.RevokeReplacementApproval(source, inst.ID)
	}
}

// ReplacedAutomatically says a promotion went ahead on its job's automatic
// approval.
func ReplacedAutomatically(promoteID, transcodeID string, approvals map[string]store.ReplacementApproval) bool {
	a, ok := approvals[transcodeID]
	return ok && a.Approval == ApprovalAutomatic && a.PromoteActionID == promoteID && a.Used
}
