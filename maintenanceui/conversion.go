package maintenanceui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// A conversion is one file's transcode and everything that follows it: its
// replacement and the retries of either. A person sees it as one job; the
// actions underneath are how the server runs it. These describe a conversion
// in data a client draws, not in sentences.

// conversionFacts is what the operations page already knows about every
// record, read once per page.
type conversionFacts struct {
	byID          map[string]operationRecord
	roots         map[string]string
	replacements  map[string]string
	replaced      map[string]bool
	replacedByAct map[string]bool
	archives      map[string]string
	active        map[string]bool
	numbers       map[string]int
	// byPath lists each file's transcode jobs, oldest first.
	byPath map[string][]operationRecord
}

func newConversionFacts(records []operationRecord, byID map[string]operationRecord, roots, replacements map[string]string, replaced, replacedByAct map[string]bool, archives map[string]string, active map[string]bool, numbers map[string]int) conversionFacts {
	f := conversionFacts{byID: byID, roots: roots, replacements: replacements, replaced: replaced, replacedByAct: replacedByAct, archives: archives, active: active, numbers: numbers, byPath: map[string][]operationRecord{}}
	for _, r := range records {
		if r.inst.ActionName == "transcode_media" {
			if path := conversionPath(r); path != "" {
				f.byPath[path] = append(f.byPath[path], r)
			}
		}
	}
	for _, list := range f.byPath {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].inst.CreatedAt == list[j].inst.CreatedAt {
				return list[i].inst.ID < list[j].inst.ID
			}
			return list[i].inst.CreatedAt < list[j].inst.CreatedAt
		})
	}
	return f
}

func conversionPath(r operationRecord) string {
	return firstNonEmpty(operationString(r.state["resolved_path"]), operationString(r.inputs["path"]))
}

// conversionID is the transcode a single-file workflow started from, "" for a
// job that is no single file's conversion (a batch, a podcast, a file of a
// batch: those open as themselves).
//
// The replacement of a file of a batch belongs to the batch's workflow, but
// it is that file's conversion: its conversion is the file's transcode.
func (f conversionFacts) conversionID(r operationRecord) string {
	root := f.roots[r.inst.ID]
	if r.inst.ActionName == "promote_transcode_candidate" && f.byID[root].inst.ActionName == "transcode_batch" {
		source := firstNonEmpty(operationString(r.inputs["transcode_action_id"]), operationString(operationMap(operationValue(r, "promotion"))["transcode_action_id"]))
		if f.byID[source].inst.ActionName == "transcode_media" {
			return source
		}
		return ""
	}
	if f.byID[root].inst.ActionName != "transcode_media" {
		return ""
	}
	return root
}

// attempts is every run of the job's file, oldest first: a file of a batch
// counts its runs in that batch and the file's own runs outside any batch.
// supersededBy is the latest run when it is not this one.
func (f conversionFacts) attempts(r operationRecord) (list []map[string]any, supersededBy map[string]any) {
	path := conversionPath(r)
	if r.inst.ActionName != "transcode_media" || path == "" {
		return nil, nil
	}
	parent := operationString(r.inputs["parent_action_id"])
	var runs []operationRecord
	for _, run := range f.byPath[path] {
		if p := operationString(run.inputs["parent_action_id"]); parent == "" || p == "" || p == parent {
			runs = append(runs, run)
		}
	}
	list = []map[string]any{}
	for _, run := range runs {
		a := map[string]any{"id": run.inst.ID, "status": run.inst.Status, "conversion_status": f.conversionStatus(run), "created_at": run.inst.CreatedAt, "current": run.inst.ID == r.inst.ID}
		if n := f.numbers[f.roots[run.inst.ID]]; n > 0 {
			a["number"] = n
		}
		if setting := attemptSetting(run); setting != "" {
			a["setting"] = setting
		}
		if run.savings.CandidateBytes != nil {
			a["candidate_bytes"] = *run.savings.CandidateBytes
		}
		if c := f.cause(run, nil); c != nil {
			a["cause"] = c
		}
		list = append(list, a)
	}
	if n := len(runs); n > 0 && runs[n-1].inst.ID != r.inst.ID {
		latest := runs[n-1]
		supersededBy = map[string]any{"id": latest.inst.ID}
		if n := f.numbers[f.roots[latest.inst.ID]]; n > 0 {
			supersededBy["number"] = n
		}
	}
	return list, supersededBy
}

// attemptSetting is the setting a run encoded with last, "" when unknown.
func attemptSetting(r operationRecord) string {
	if r.state["size_audio_only"] == true {
		return "audio only"
	}
	if ladder := jobLadder(r); ladder != nil {
		rung := min(max(ladder.Rung, 0), len(ladder.Values)-1)
		if ladder.Final == nil && len(ladder.Tried) > 0 {
			return ladderSetting(ladder, ladder.Tried[len(ladder.Tried)-1].Quality)
		}
		return ladderSetting(ladder, ladder.Values[rung])
	}
	if kbps := operationNumber(operationMap(operationValue(r, "size_budget"))["video_bitrate_kbps"]); kbps > 0 {
		return fmt.Sprintf("%.0f kbps", kbps)
	}
	return ""
}

// waitingItsTurn says the job is held back, for a worker slot or in the worker's
// own queue, not working.
func waitingItsTurn(r operationRecord) bool {
	return r.inst.WaitingCondition == "worker_busy" || r.inst.WaitingCondition == "worker_drain" || operationQueuedOnWorker(r)
}

// conversionStatus is the one state a transcode job's conversion is in.
func (f conversionFacts) conversionStatus(r operationRecord) string {
	switch r.inst.Status {
	case store.ActionStatusPending:
		return "queued"
	case store.ActionStatusRunning, store.ActionStatusWaitingExternal:
		if waitingItsTurn(r) {
			return "queued"
		}
		return "encoding"
	case store.ActionStatusWaitingDecision:
		return "needs_decision"
	case store.ActionStatusFailed:
		return "failed"
	case store.ActionStatusCancelled:
		return "cancelled"
	}
	if rep, ok := f.byID[f.replacements[r.inst.ID]]; ok {
		switch rep.inst.Status {
		case store.ActionStatusCompleted:
			if rep.savings.RealizedSavedBytes != nil {
				return "replaced"
			}
		case store.ActionStatusFailed:
			return "replace_failed"
		case store.ActionStatusCancelled:
		default:
			return "replacing"
		}
	}
	// A job that made no new file replaced nothing, even when another job of
	// the same file did.
	if operationValue(r, "skip_transcode") == true || r.savings.CandidateBytes == nil {
		return "no_result"
	}
	if f.replacedByAct[r.inst.ID] || f.replaced[operationSourceKey(r)] {
		return "replaced"
	}
	if operationValue(r, "original_intact") != true {
		return "no_result"
	}
	if operationValue(r, "candidate_removed") == true || f.archives[f.roots[r.inst.ID]] != "" && !f.active[f.roots[r.inst.ID]] {
		return "kept_original"
	}
	return "ready_to_replace"
}

// conversionReplacement is the replacement a transcode started, nil when none.
func (f conversionFacts) conversionReplacement(r operationRecord) map[string]any {
	rep, ok := f.byID[f.replacements[r.inst.ID]]
	if !ok {
		return nil
	}
	out := map[string]any{"id": rep.inst.ID, "status": rep.inst.Status}
	if rep.inst.Status == store.ActionStatusFailed {
		out["error"] = failureText(rep)
	}
	if rep.inst.Status == store.ActionStatusCompleted {
		out["completed_at"] = rep.inst.UpdatedAt
	}
	return out
}

// resolvedBy is a later conversion of the same file that got a result, for a
// failed one: the failure no longer needs anyone. Nil otherwise.
func (f conversionFacts) resolvedBy(r operationRecord) map[string]any {
	if r.inst.ActionName != "transcode_media" || r.inst.Status != store.ActionStatusFailed {
		return nil
	}
	seen := false
	for _, later := range f.byPath[conversionPath(r)] {
		if later.inst.ID == r.inst.ID {
			seen = true
			continue
		}
		if !seen || f.conversionID(later) != "" && f.conversionID(later) == f.conversionID(r) {
			continue
		}
		if status := f.conversionStatus(later); status == "replaced" || status == "ready_to_replace" || status == "replacing" {
			out := map[string]any{"id": later.inst.ID, "conversion_status": status}
			if conversion := f.conversionID(later); conversion != "" {
				out["id"] = conversion
			}
			if n := f.numbers[f.roots[later.inst.ID]]; n > 0 {
				out["number"] = n
			}
			return out
		}
	}
	return nil
}

// conversionOutcome is how a finished encode ended: the encoder and the
// setting that passed, which of the frozen settings it was, and its measured
// quality against the goal's limits. Nil for a job without a new file.
func conversionOutcome(r operationRecord) map[string]any {
	if r.inst.ActionName != "transcode_media" || r.inst.Status != store.ActionStatusCompleted || r.savings.CandidateBytes == nil {
		return nil
	}
	out := map[string]any{"candidate_bytes": *r.savings.CandidateBytes}
	if r.savings.SourceBytes != nil {
		out["source_bytes"] = *r.savings.SourceBytes
	}
	main10 := r.state["size_main10"] == true
	if plan := operationMap(r.state["plan"]); operationNumber(plan["expected_bit_depth"]) == 10 {
		main10 = true
	}
	encoder := func(codec string) string {
		name := map[string]string{transcode.VideoCodecLibX265: "x265", transcode.VideoCodecHEVCVideoToolbox: "VideoToolbox", "libsvtav1": "SVT-AV1"}[codec]
		if name == "" {
			name = codec
		}
		if main10 && codec == transcode.VideoCodecLibX265 {
			name += " 10-bit"
		}
		return name
	}
	var final *action.FinalQualitySummary
	if ladder := action.QualityLadderOf(map[string]any{"quality_ladder": operationValue(r, "quality_ladder")}); ladder != nil {
		rung := min(max(ladder.Rung, 0), len(ladder.Values)-1)
		name := "CRF"
		if ladder.VideoCodec == transcode.VideoCodecHEVCVideoToolbox {
			name = "Quality"
		}
		setting := fmt.Sprintf("%s %d", name, ladder.Values[rung])
		if ladder.Preset != "" {
			setting += " · " + ladder.Preset
		}
		out["encoder"], out["setting"] = encoder(ladder.VideoCodec), setting
		out["attempt"], out["attempts"] = rung+1, len(ladder.Values)
		final = ladder.Final
	} else if budget := operationMap(operationValue(r, "size_budget")); operationNumber(budget["video_bitrate_kbps"]) > 0 {
		codec := transcode.VideoCodecLibX265
		if c := operationString(operationMap(budget["encoder_choice"])["video_codec"]); c != "" {
			codec = c
		}
		out["encoder"], out["setting"] = encoder(codec), fmt.Sprintf("%.0f kbps", operationNumber(budget["video_bitrate_kbps"]))
	}
	if r.state["size_audio_only"] == true {
		out["encoder"], out["setting"] = "copied", "audio only"
		delete(out, "attempt")
		delete(out, "attempts")
	}
	if final != nil {
		if v := final.Measured.VMAFMean; v != nil {
			out["vmaf_mean"] = *v
		}
		if v := final.Measured.VMAFWorstWindow; v != nil {
			out["vmaf_worst"] = *v
		}
		if l := final.Limits; l != nil {
			if l.VMAFMean != nil {
				out["vmaf_target"] = *l.VMAFMean
			}
			if l.VMAFWorstWindow != nil {
				out["vmaf_floor"] = *l.VMAFWorstWindow
			}
		}
	} else if vmaf := operationMap(operationMap(operationValue(r, "quality_evidence"))["vmaf"]); vmaf["mean"] != nil {
		out["vmaf_mean"] = operationNumber(vmaf["mean"])
		if worst, ok := vmaf["worst_window_mean"].(float64); ok {
			out["vmaf_worst"] = worst
		}
	}
	return out
}

// conversionStreams is the before → after of each stream, nil without the
// probe and plan it is read from. Before the new file exists it is the plan:
// each track's fate, with the video's size after left unknown.
func conversionStreams(r operationRecord) *action.ConversionStreams {
	if r.inst.ActionName != "transcode_media" {
		return nil
	}
	var candidate int64
	if r.savings.CandidateBytes != nil {
		candidate = int64(*r.savings.CandidateBytes)
	}
	return action.ConversionStreamsOf(r.state, candidate)
}

// shortTrackMismatch says in a line why chosen tracks no longer fit a file.
func shortTrackMismatch(detail string) string {
	if strings.Contains(detail, "which is not") || strings.Contains(detail, "does not have") {
		return "The tracks picked earlier are not in this file any more: it changed after they were picked."
	}
	return "The tracks picked for this job do not match the file."
}

// batchReadyItems are the child transcodes of every file of a batch that is
// ready to replace its original, in the batch's order and across all its
// pages: a finished encode whose original is intact, whose new file is still
// there, and which no replacement, retry or prepared plan has taken.
func batchReadyItems(batch operationRecord, items []store.TranscodeBatchItem, byID map[string]operationRecord, replacements map[string]string, replacedActs, replaced map[string]bool) []string {
	out := []string{}
	if batch.inputs["dry_run"] == true {
		return out
	}
	pending := operationMap(batch.state["batch_retry_pending"])
	seen := map[string]bool{}
	for _, item := range items {
		child, ok := byID[item.ChildActionID]
		if !ok || seen[child.inst.ID] || child.inst.ActionName != "transcode_media" || operationString(child.inputs["parent_action_id"]) != batch.inst.ID {
			continue
		}
		if item.Status != "completed" || item.Decision != "transcode" || pending[item.ItemKey] == true {
			continue
		}
		if child.inst.Status != store.ActionStatusCompleted || operationValue(child, "skip_transcode") == true || operationValue(child, "original_intact") != true || operationValue(child, "candidate_removed") == true || child.savings.CandidateBytes == nil {
			continue
		}
		if replacements[child.inst.ID] != "" || replacedActs[child.inst.ID] || replaced[operationSourceKey(child)] || replacementPlanState(batch.state, child.inst.ID) != "" {
			continue
		}
		seen[child.inst.ID] = true
		out = append(out, child.inst.ID)
	}
	return out
}
