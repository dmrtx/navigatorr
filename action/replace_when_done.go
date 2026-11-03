package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/store"
)

// Promotion approvals a promote_transcode_candidate can carry in its inputs.
// Either one still stops at a regrab warning, and a batch-linked promotion
// ignores both.
const (
	// ApprovalReviewed: the person approved this file after seeing its result
	// (POST /api/maintenance/replace); the plan the server makes for it is
	// approved once it passes its checks.
	ApprovalReviewed = "reviewed"
	// ApprovalAutomatic: the file's job or batch replaces each file when it is
	// done. Checked again at approval: switched off by then, the promotion
	// waits for a decision like any other.
	ApprovalAutomatic = "automatic"
)

// ErrReplaceWhenDoneFinished refuses a change to a job that has ended.
var ErrReplaceWhenDoneFinished = errors.New("this job has finished; replace each ready file from its review")

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
// file is done: the value it was last switched to, else its batch's, else the
// one it was submitted with. A preview never replaces anything.
func (e *Engine) ReplaceWhenDone(inst *store.ActionInstance) bool {
	value, _ := e.replaceWhenDone(inst, 0)
	return value
}

func (e *Engine) replaceWhenDone(inst *store.ActionInstance, depth int) (bool, error) {
	if inst == nil || e.deps.Store == nil || depth > 2 {
		return false, nil
	}
	var inputs map[string]any
	_ = json.Unmarshal([]byte(inst.InputsJSON), &inputs)
	if getBool(inputs, "dry_run") {
		return false, nil
	}
	if value, ok, err := e.deps.Store.GetReplaceWhenDone(inst.ID); err != nil || ok {
		return value, err
	}
	if inst.ActionName == "transcode_media" {
		if parentID := getString(inputs, "parent_action_id"); parentID != "" {
			if parent, err := e.deps.Store.GetActionInstanceIfExists(parentID); err == nil && parent != nil && parent.ActionName == "transcode_batch" {
				return e.replaceWhenDone(parent, depth+1)
			}
		}
	}
	return getBool(inputs, "replace_when_done"), nil
}

// SetReplaceWhenDone switches a job or batch that has not ended. Switching it
// on replaces only files that finish from now on; switching it off stops the
// replacements not yet approved, and one already under way finishes.
func (e *Engine) SetReplaceWhenDone(ctx context.Context, id string, value bool) (*store.ActionInstance, error) {
	inst, err := e.deps.Store.GetActionInstanceIfExists(id)
	if err != nil {
		return nil, err
	}
	if inst == nil || inst.ActionName != "transcode_media" && inst.ActionName != "transcode_batch" {
		return nil, fmt.Errorf("job is unavailable")
	}
	switch inst.Status {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return nil, ErrReplaceWhenDoneFinished
	}
	if err := e.deps.Store.SetReplaceWhenDone(id, value); err != nil {
		return nil, err
	}
	return inst, nil
}

// autoReplacement is the promotion a transcode that just finished starts by
// itself, nil when it does not: replace_when_done is off, the file has no new
// file to replace with, or its library cannot be told. reason says why not
// when the option is on and the file has a new file nobody else replaces.
func (e *Engine) autoReplacement(inst *store.ActionInstance, ec *ExecutionContext) (inputs map[string]any, reason string) {
	if inst.ActionName != "transcode_media" || !e.ReplaceWhenDone(inst) {
		return nil, ""
	}
	if !e.AllowDestructive() {
		return nil, "replacement is disabled in the server settings"
	}
	// The job is saved as completed right after this; judge it as it will be.
	done := *inst
	done.Status = StatusCompleted
	inputs, prior, code := e.replacement(&done, ec.Inputs, ec.State, ec.Outputs, nil)
	switch {
	case code == ReplaceNeedsLibrary:
		return nil, "it needs a library choice: use its Replace"
	case code == ReplaceBatchUnavailable:
		return nil, "its batch is unavailable"
	case code != "" || prior != nil:
		return nil, ""
	}
	inputs["approval"] = ApprovalAutomatic
	return inputs, ""
}

// Why a file cannot be replaced now, as POST /api/maintenance/replace lists it.
const (
	ReplaceUnavailable      = "unavailable"          // no such file job
	ReplaceNotFinished      = "not_finished"         // still queued, encoding or waiting
	ReplaceNoResult         = "no_result"            // no new file to replace with
	ReplaceKeptOriginal     = "kept_original"        // the owner kept the original
	ReplacePreview          = "preview"              // a preview never replaces
	ReplaceStarted          = "replacement_started"  // a replacement exists and is past approval
	ReplaceInBatchReview    = "in_batch_review"      // its batch's prepared plan holds it
	ReplaceRetryPending     = "retry_pending"        // its batch encodes it again
	ReplaceBatchUnavailable = "batch_unavailable"    // its batch was cancelled or is gone
	ReplaceNeedsLibrary     = "needs_library_choice" // its library is not known: replace it from its own review
)

// replacement is the promotion that replaces a finished transcode's original
// with its new file, or why it cannot start: code is "" when it can. prior is
// the replacement that already exists, waiting for its approval; inputs is
// nil then. archives, when given, is the kept-original record.
func (e *Engine) replacement(inst *store.ActionInstance, inputs, state, outputs map[string]any, archives map[string]string) (out map[string]any, prior *store.ActionInstance, code string) {
	if inst == nil || inst.ActionName != "transcode_media" {
		return nil, nil, ReplaceUnavailable
	}
	if getBool(inputs, "dry_run") {
		return nil, nil, ReplacePreview
	}
	if inst.Status != StatusCompleted {
		if inst.Status == StatusFailed || inst.Status == StatusCancelled {
			return nil, nil, ReplaceNoResult
		}
		return nil, nil, ReplaceNotFinished
	}
	if getBool(state, "skip_transcode") || !getBool(outputs, "original_intact") || getString(outputs, "candidate_path") == "" {
		return nil, nil, ReplaceNoResult
	}
	if getBool(outputs, candidateRemovedKey) || archives[inst.ID] != "" {
		return nil, nil, ReplaceKeptOriginal
	}
	var parentInputs map[string]any
	if parentID := getString(inputs, "parent_action_id"); parentID != "" {
		parent, _ := e.deps.Store.GetActionInstanceIfExists(parentID)
		if parent == nil || parent.ActionName != "transcode_batch" || parent.Status == StatusCancelled {
			return nil, nil, ReplaceBatchUnavailable
		}
		if archives[parent.ID] != "" && (parent.Status == StatusCompleted || parent.Status == StatusFailed) {
			return nil, nil, ReplaceKeptOriginal
		}
		var parentState map[string]any
		_ = json.Unmarshal([]byte(parent.InputsJSON), &parentInputs)
		_ = json.Unmarshal([]byte(parent.StateJSON), &parentState)
		if getBool(parentInputs, "dry_run") {
			return nil, nil, ReplacePreview
		}
		if key := getString(inputs, "batch_item_key"); key != "" && stateObject(parentState, "batch_retry_pending")[key] == true {
			return nil, nil, ReplaceRetryPending
		}
		if plan := getBatchPromotionPlan(parentState["batch_promotion_plan"]); plan != nil {
			for _, member := range plan.Members {
				if member.SourceAction == inst.ID {
					return nil, nil, ReplaceInBatchReview
				}
			}
		}
	}
	for _, service := range []string{"sonarr", "radarr", "filesystem"} {
		old, err := e.deps.Store.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:"+service+":"+inst.ID)
		if err != nil {
			return nil, nil, ReplaceUnavailable
		}
		if old == nil {
			continue
		}
		if old.Status == StatusWaitingDecision && old.CurrentStep == 1 && getString(decodeInputs(old.InputsJSON), "batch_promote_parent_id") == "" {
			return nil, old, ""
		}
		return nil, nil, ReplaceStarted
	}
	out = map[string]any{"transcode_action_id": inst.ID}
	service, key, id := e.replacementLibrary(inputs, parentInputs)
	if service == "" {
		return nil, nil, ReplaceNeedsLibrary
	}
	out["service"] = service
	if key != "" {
		out[key] = id
	}
	return out, nil, ""
}

func decodeInputs(raw string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	return m
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

// startAutoReplacement admits the promotion a finished transcode starts by
// itself. The reconciler runs it; its approval is checked again then.
func (e *Engine) startAutoReplacement(ctx context.Context, inst *store.ActionInstance, inputs map[string]any) {
	if inputs == nil {
		return
	}
	if _, err := e.Enqueue(WithOrigin(context.WithoutCancel(ctx), "automatic"), "promote_transcode_candidate", inputs, "auto-replace:"+inst.ID); err != nil {
		log.Printf("replace when done: %s: %v", inst.ID, err)
		return
	}
	e.WakeReconciler()
}

// promotionApprovedAhead says whether a promotion was approved before its
// plan was made, and why not when it says no: a person approved the file
// after its review, or its job replaces each file when done and still does.
func (e *Engine) promotionApprovedAhead(ec *ExecutionContext, p *promotionState) (bool, string) {
	switch getString(ec.Inputs, "approval") {
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
	return true, ""
}

// ReplaceRefused is one file POST /api/maintenance/replace could not replace.
type ReplaceRefused struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// ReplaceStartedFile is one file whose replacement was started, or approved when
// it was already planned and waiting.
type ReplaceStartedFile struct {
	ID                  string `json:"id"`
	ReplacementActionID string `json:"replacement_action_id"`
	Approved            string `json:"approved"` // "new" or "waiting_plan"
}

// ErrReplaceRefused carries the files that cannot be replaced now; nothing
// was started.
type ErrReplaceRefused struct {
	Ready    []string
	NotReady []ReplaceRefused
}

func (e *ErrReplaceRefused) Error() string {
	return fmt.Sprintf("%d of the files cannot be replaced now; nothing was replaced", len(e.NotReady))
}

// ReplaceReviewed replaces exactly the files a person reviewed, all of them or
// none: each must be a finished file job whose new file is ready now. A file
// of a batch that is still encoding others is replaced now; the batch's other
// files are untouched. Each replacement runs the usual promotion, with every
// check and the stop on a regrab warning; one already planned and waiting is
// approved with the plan it waits on.
func (e *Engine) ReplaceReviewed(ctx context.Context, ids []string, key string) ([]ReplaceStartedFile, error) {
	if !e.AllowDestructive() {
		return nil, fmt.Errorf("replacement is disabled in the server settings")
	}
	archives, err := e.deps.Store.MaintenanceArchives()
	if err != nil {
		return nil, err
	}
	type planned struct {
		id     string
		inputs map[string]any
		prior  *store.ActionInstance
	}
	var work []planned
	refused := &ErrReplaceRefused{Ready: []string{}, NotReady: []ReplaceRefused{}}
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return nil, inputErr("ids", "list each file once")
		}
		seen[id] = true
		inst, err := e.deps.Store.GetActionInstanceIfExists(id)
		if err != nil {
			return nil, err
		}
		code := ReplaceUnavailable
		var inputs map[string]any
		var prior *store.ActionInstance
		if inst != nil {
			inputs, prior, code = e.replacement(inst, decodeInputs(inst.InputsJSON), decodeInputs(inst.StateJSON), decodeInputs(inst.OutputsJSON), archives)
		}
		if code != "" {
			refused.NotReady = append(refused.NotReady, ReplaceRefused{ID: id, Reason: code})
			continue
		}
		refused.Ready = append(refused.Ready, id)
		work = append(work, planned{id, inputs, prior})
	}
	if len(refused.NotReady) > 0 {
		return nil, refused
	}
	started := make([]ReplaceStartedFile, 0, len(work))
	for _, item := range work {
		if item.prior != nil {
			digest := PromotionPlanDigest(decodeInputs(item.prior.StateJSON)["promotion"])
			if digest == "" {
				return started, fmt.Errorf("the replacement of %s has no plan to approve", item.id)
			}
			if _, err := e.QueueMaintenanceCommand(ctx, map[string]any{"id": item.prior.ID, "kind": "resume", "decision": "approve", "plan_digest": digest}, "replace-"+key+"-"+item.prior.ID); err != nil {
				return started, err
			}
			started = append(started, ReplaceStartedFile{ID: item.id, ReplacementActionID: item.prior.ID, Approved: "waiting_plan"})
			continue
		}
		item.inputs["approval"] = ApprovalReviewed
		r, err := e.Enqueue(WithOrigin(ctx, "web"), "promote_transcode_candidate", item.inputs, "replace:"+item.id)
		if err != nil {
			return started, err
		}
		started = append(started, ReplaceStartedFile{ID: item.id, ReplacementActionID: r.ID, Approved: "new"})
	}
	e.WakeReconciler()
	return started, nil
}
