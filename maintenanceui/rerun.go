package maintenanceui

import (
	"net/http"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

// A size-mode search that ends without a usable setting is a finished result,
// not a failure. Only these outcomes, or a cancellation, offer "Run again".
var noWinnerReasons = map[string]bool{"no_suitable_candidate": true, "quality_not_met": true, "search_budget_exhausted": true, "size_budget_exceeded": true}

// A standalone file the goal could not handle ends with a reason that names the
// goal that can. The server decides the remedy; the client only asks for it.
// hdr_requires_compact: only Much smaller converts HDR and Dolby Vision.
// compact_not_needed: an SDR source already at the compact target needs the
// size goal to shrink further. Other skip reasons (unsupported_source,
// hfr_quality_unavailable, already_target_codec, insufficient_savings) are not
// fixed by one unambiguous goal, so they offer no remedy.
var remedyModes = map[string]string{"hdr_requires_compact": "compact", "compact_not_needed": "size"}

// remedyInputKeys are the inputs that mean the same under every goal. Everything
// else (media engine, size budget, height cap, profile, preservation and size
// limits, metric, batch calibration) belongs to one goal's policy and would be
// refused or change meaning under another, so a goal switch starts from these.
var remedyInputKeys = []string{"path", "replace_original", "media_type", "is_anime", "library_context", "surface_worker_busy"}

func rerunSource(r operationRecord) string {
	if path := operationString(r.state["resolved_path"]); path != "" {
		return path
	}
	return operationString(r.inputs["path"])
}

func isNoWinnerRound(r operationRecord) bool {
	return (r.inst.ActionName == "transcode_media" || r.inst.ActionName == "benchmark_transcode") &&
		r.inst.Status == store.ActionStatusCompleted && noWinnerReasons[operationString(operationValue(r, "reason_code"))]
}

// A standalone single-file job may be run again once it was cancelled or ended
// without a winner. Batch children keep "Change goal", and jobs with a
// candidate or a pending decision are reviewed instead.
func rerunEligible(r operationRecord) bool {
	if r.inst.ActionName != "transcode_media" && r.inst.ActionName != "benchmark_transcode" {
		return false
	}
	if operationString(r.inputs["parent_action_id"]) != "" || operationString(r.inputs["path"]) == "" {
		return false
	}
	return r.inst.Status == store.ActionStatusCancelled || isNoWinnerRound(r) || remedyMode(r) != ""
}

// remedyMode is the goal that would handle this job's source, or "" when the
// job finished for another reason or already ran under that goal.
func remedyMode(r operationRecord) string {
	if r.inst.ActionName != "transcode_media" || r.inst.Status != store.ActionStatusCompleted {
		return ""
	}
	remedy := remedyModes[operationString(operationValue(r, "reason_code"))]
	if remedy == "" || remedy == firstNonEmpty(operationString(r.inputs["mode"]), operationString(r.state["mode"])) {
		return ""
	}
	return remedy
}

// remedyTaken reports whether a newer job for the same file exists, so a
// remedy is offered once: after the right goal ran, the old skip is history. A
// timestamp that does not parse counts as taken, never as a second offer.
func remedyTaken(r operationRecord, records []operationRecord) bool {
	if remedyMode(r) == "" {
		return false
	}
	created, err := time.Parse(time.RFC3339Nano, r.inst.CreatedAt)
	if err != nil {
		return true
	}
	source := rerunSource(r)
	for _, other := range records {
		if other.inst.ID == r.inst.ID || other.inst.ActionName != r.inst.ActionName || rerunSource(other) != source {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, other.inst.CreatedAt)
		// Stored times may be whole seconds, so the same second counts as newer.
		if err != nil || !at.Truncate(time.Second).Before(created.Truncate(time.Second)) {
			return true
		}
	}
	return false
}

// rerunHasResult mirrors the queue's candidate_ready and replacement checks so
// the server refuses what the UI already hides.
func rerunHasResult(r operationRecord, replacements map[string]string) bool {
	if replacements[r.inst.ID] != "" {
		return true
	}
	return r.inst.Status == store.ActionStatusCompleted && operationValue(r, "skip_transcode") != true && operationValue(r, "original_intact") == true && r.savings.CandidateBytes != nil
}

// noWinnerRounds counts finished searches per source file, batch children
// included, so the confirmation can say how many rounds a file already had.
func noWinnerRounds(records []operationRecord) map[string]int {
	rounds := map[string]int{}
	for _, r := range records {
		if isNoWinnerRound(r) {
			rounds[rerunSource(r)]++
		}
	}
	return rounds
}

func rerunSummary(r operationRecord, rounds map[string]int) map[string]any {
	summary := map[string]any{
		"source_path":      rerunSource(r),
		"mode":             firstNonEmpty(operationString(r.inputs["mode"]), operationString(r.state["mode"])),
		"replace_original": r.inputs["replace_original"] == true,
		"no_winner_rounds": rounds[rerunSource(r)],
	}
	if remedy := remedyMode(r); remedy != "" {
		summary["remedy_mode"] = remedy
	}
	return summary
}

// rerun starts a NEW action with the original's inputs. The old action is
// never resumed or changed, and the caller's key must differ from every
// earlier attempt. Nothing here retries on its own.
func (s *Server) rerun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID  string `json:"id"`
		Key string `json:"key"`
		// WithoutHardware runs the file again with the media engine off. Only a
		// size budget the media engine encoded below the quality floor offers it.
		WithoutHardware bool `json:"without_hardware"`
		// Mode starts the file under the goal that handles its source. It must be
		// the server's remedy for this job; the client cannot pick another goal.
		Mode string `json:"mode"`
	}
	if decode(w, r, &body) != nil || !reconfigureKey.MatchString(body.Key) {
		fail(w, 400, "a job and a valid submission key are required")
		return
	}
	st := s.engine.Deps().Store
	inst, err := st.GetActionInstanceIfExists(body.ID)
	if err != nil {
		fail(w, 500, "read job")
		return
	}
	if inst == nil || !allowedActions[inst.ActionName] {
		fail(w, 403, "job is unavailable")
		return
	}
	old := operationRecord{inst: *inst, inputs: decodeOperationJSON(inst.InputsJSON), outputs: decodeOperationJSON(inst.OutputsJSON), state: decodeOperationJSON(inst.StateJSON)}
	if !rerunEligible(old) {
		fail(w, 409, "this job cannot be run again")
		return
	}
	if body.Key == inst.IdempotencyKey || body.Key == operationString(old.inputs["idempotency_key"]) {
		fail(w, 400, "a new attempt needs a new submission key")
		return
	}
	remedy := remedyMode(old)
	if body.Mode != "" && (body.Mode != remedy || body.WithoutHardware) {
		fail(w, 409, "this job has no remedy for that goal")
		return
	}
	if body.Mode == "" && remedy != "" {
		fail(w, 409, "this file was skipped because of its goal; choose the goal that handles it instead of running it again")
		return
	}
	inputs := map[string]any{}
	if body.Mode != "" {
		for _, k := range remedyInputKeys {
			if v, ok := old.inputs[k]; ok {
				inputs[k] = v
			}
		}
		inputs["mode"] = body.Mode
	} else {
		for k, v := range old.inputs {
			if k != "idempotency_key" {
				inputs[k] = v
			}
		}
	}
	if body.WithoutHardware {
		if operationMap(operationValue(old, "size_budget"))["retry_without_hardware"] != true {
			fail(w, 409, "this job did not offer a retry without the media engine")
			return
		}
		inputs["allow_hardware"] = false
		delete(inputs, "hardware_margin_percent")
	}
	// A goal switch starts from the neutral inputs alone: nothing the earlier goal
	// froze carries over.
	same := body.Mode == ""
	// Run again means the same settings. A size job that never named the media
	// engine would otherwise take today's default instead of the policy it ran
	// under, so carry the choice its frozen policy made.
	if _, said := inputs["allow_hardware"]; same && !said && operationString(inputs["mode"]) == "size" {
		if allow, margin, ok := action.FrozenHardware(old.state); ok {
			inputs["allow_hardware"] = allow
			if allow {
				inputs["hardware_margin_percent"] = margin
			}
		}
	}
	// The same goes for a size budget a goal change froze without naming it in
	// the job's own inputs.
	_, hasPercent := inputs["size_budget_percent"]
	_, hasBytes := inputs["size_budget_bytes"]
	_, hasSource := inputs["size_budget_source"]
	if same && !hasPercent && !hasBytes && !hasSource && operationString(inputs["mode"]) == "size" {
		budget := operationMap(operationMap(operationMap(old.state["mode_policy"])["size"])["size_budget"])
		if percent, ok := budget["percent"].(float64); ok && percent > 0 {
			inputs["size_budget_percent"] = percent
		} else if bytes, ok := budget["bytes"].(float64); ok && bytes > 0 {
			inputs["size_budget_bytes"] = bytes
		} else if siblings := operationMap(budget["siblings"]); siblings != nil {
			inputs["size_budget_source"] = "siblings"
			if multiplier, ok := siblings["multiplier"].(float64); ok && multiplier > 0 {
				inputs["size_budget_sibling_multiplier"] = multiplier
			}
		}
	}
	ctx := action.WithOrigin(r.Context(), "web")
	// A lost response replays the same key: return that action, never a
	// second job and never "worker busy" for work that is already admitted.
	if prior, err := st.FindActionByIdempotencyKey(inst.ActionName, body.Key); err != nil {
		fail(w, 500, "read earlier attempt")
		return
	} else if prior != nil {
		res, err := s.engine.Enqueue(ctx, inst.ActionName, inputs, body.Key)
		if err != nil {
			failAdmission(w, 409, err)
			return
		}
		writeJSON(w, 202, map[string]any{"id": res.ID, "status": res.Status, "action_name": res.ActionName})
		return
	}
	snapshot, err := st.ListMaintenanceActionSnapshot()
	if err != nil {
		fail(w, 500, "read maintenance history")
		return
	}
	records := make([]operationRecord, 0, len(snapshot))
	for _, other := range snapshot {
		rec := operationRecord{inst: other, inputs: decodeOperationJSON(other.InputsJSON), outputs: decodeOperationJSON(other.OutputsJSON), state: decodeOperationJSON(other.StateJSON)}
		rec.savings = projectOperationSavings(rec, time.Now().UTC())
		records = append(records, rec)
	}
	old.savings = projectOperationSavings(old, time.Now().UTC())
	if rerunHasResult(old, queueReplacementLinks(records)) {
		fail(w, 409, "this job produced a candidate or replacement; review it instead of running it again")
		return
	}
	if body.Mode != "" && remedyTaken(old, records) {
		fail(w, 409, "this file already has a newer job; open that one instead")
		return
	}
	source := rerunSource(old)
	for _, other := range records {
		if other.inst.ID != inst.ID && other.inst.ActionName == inst.ActionName && operationActive(other.inst.Status) && rerunSource(other) == source {
			fail(w, 409, "this file already has an active job; wait for it to finish or cancel it before running it again")
			return
		}
	}
	res, err := s.engine.EnqueueWorker(ctx, inst.ActionName, inputs, body.Key)
	if err != nil {
		failAdmission(w, 409, err)
		return
	}
	writeJSON(w, 202, map[string]any{"id": res.ID, "status": res.Status, "action_name": res.ActionName})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
