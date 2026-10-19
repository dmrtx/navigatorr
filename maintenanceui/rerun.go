package maintenanceui

import (
	"net/http"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

// A size-mode search that ends without a usable setting is a finished result,
// not a failure. Only these outcomes, or a cancellation, offer "Run again".
var noWinnerReasons = map[string]bool{"no_suitable_candidate": true, "quality_not_met": true, "search_budget_exhausted": true}

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
	return r.inst.Status == store.ActionStatusCancelled || isNoWinnerRound(r)
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
	return map[string]any{
		"source_path":      rerunSource(r),
		"mode":             firstNonEmpty(operationString(r.inputs["mode"]), operationString(r.state["mode"])),
		"replace_original": r.inputs["replace_original"] == true,
		"no_winner_rounds": rounds[rerunSource(r)],
	}
}

// rerun starts a NEW action with the original's inputs. The old action is
// never resumed or changed, and the caller's key must differ from every
// earlier attempt. Nothing here retries on its own.
func (s *Server) rerun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID  string `json:"id"`
		Key string `json:"key"`
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
	inputs := map[string]any{}
	for k, v := range old.inputs {
		if k != "idempotency_key" {
			inputs[k] = v
		}
	}
	if err := s.engine.CheckWorkerAdmission(r.Context()); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	res, err := s.engine.Enqueue(action.WithOrigin(r.Context(), "web"), inst.ActionName, inputs, body.Key)
	if err != nil {
		fail(w, 409, err.Error())
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
