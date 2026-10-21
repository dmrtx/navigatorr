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
