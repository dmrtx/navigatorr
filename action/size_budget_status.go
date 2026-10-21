package action

import "fmt"

// SizeBudgetLadderStep names the VideoToolbox ladder rung a size budget action
// is encoding, as "VideoToolbox at quality 65 (step 1 of 4)", for status
// summaries. ok is false when the action is not running a ladder. A ladder run
// submits no benchmark, so its worker progress is the full encode of this rung.
func SizeBudgetLadderStep(state map[string]any) (step string, ok bool) {
	rec := sizeBudgetLadderOf(state)
	plan := getPlan(state["plan"])
	if rec == nil || plan == nil {
		return "", false
	}
	step = "VideoToolbox at " + sizeBudgetRungDescription(plan)
	var policy ModePolicySnapshot
	if reencode(state["mode_policy"], &policy) && policy.Size != nil && policy.Size.SizeBudget != nil && len(policy.Size.SizeBudget.VTLadder) > 0 {
		step += fmt.Sprintf(" (step %d of %d)", rec.Rung+1, len(policy.Size.SizeBudget.VTLadder)+1)
	}
	return step, true
}

// RecipeStep names the encode a policy v4 goal is running, as "x265 CRF 20
// (step 1 of 3)" or "two-pass x265 at the size budget", for status summaries.
// ok is false for an action frozen before v4, which searches. A recipe submits
// no benchmark, so its worker progress is the full encode of this step.
func RecipeStep(state map[string]any) (step string, ok bool) {
	if policyRecipe(state["mode_policy"]) == nil {
		return "", false
	}
	if rec := QualityLadderOf(state); rec != nil && rec.Rung < len(rec.Values) {
		return fmt.Sprintf("%s (step %d of %d)", recipeRungName(rec.VideoCodec, rec.Values[rec.Rung]), rec.Rung+1, len(rec.Values)), true
	}
	if step, ladder := SizeBudgetLadderStep(state); ladder {
		return step, true
	}
	if rec := sizeBudgetOf(state); rec != nil && rec.BudgetBytes > 0 {
		return "two-pass x265 at the size budget", true
	}
	return "this goal's fixed settings", true
}
