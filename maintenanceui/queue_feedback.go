package maintenanceui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// Queue grouping follows durable action links, never filenames: independent
// requests for the same media must retain their own outcome and identity.
func queueReplacementLinks(records []operationRecord) map[string]string {
	links := map[string]string{}
	for _, r := range records {
		if r.inst.ActionName != "promote_transcode_candidate" {
			continue
		}
		source := operationString(r.inputs["transcode_action_id"])
		if source == "" {
			source = operationString(operationMap(operationValue(r, "promotion"))["transcode_action_id"])
		}
		if source != "" && links[source] == "" {
			links[source] = r.inst.ID
		}
	}
	return links
}

func queueWorkflows(records []operationRecord, byID map[string]operationRecord, replacements map[string]string) (map[string]string, map[string]int, map[string][]operationRecord) {
	roots := map[string]string{}
	members := map[string][]operationRecord{}
	for _, r := range records {
		root := r.inst.ID
		if r.inst.ActionName == "transcode_batch" && strings.HasPrefix(r.inst.IdempotencyKey, previewExecutionPrefix) {
			preview := strings.TrimPrefix(r.inst.IdempotencyKey, previewExecutionPrefix)
			if original, ok := byID[preview]; ok && original.inst.ActionName == "transcode_batch" && original.inputs["dry_run"] == true {
				root = preview
			}
		} else if r.inst.ActionName == "promote_transcode_candidate" {
			source := operationString(r.inputs["transcode_action_id"])
			if source == "" {
				source = operationString(operationMap(operationValue(r, "promotion"))["transcode_action_id"])
			}
			if original, ok := byID[source]; ok && original.inst.ActionName == "transcode_media" {
				root = source
			}
		} else if (r.inst.ActionName == "transcode_media" || r.inst.ActionName == "benchmark_transcode") && replacements[root] == "" {
			parent := operationString(r.inputs["parent_action_id"])
			if batch, ok := byID[parent]; ok && batch.inst.ActionName == "transcode_batch" {
				root = parent
			}
		}
		roots[r.inst.ID] = root
	}
	for _, r := range records {
		root := roots[r.inst.ID]
		seen := map[string]bool{r.inst.ID: true}
		for roots[root] != "" && roots[root] != root && !seen[root] {
			seen[root] = true
			root = roots[root]
		}
		roots[r.inst.ID] = root
		members[root] = append(members[root], r)
	}
	ordered := []operationRecord{}
	// Reserve each original action's number even while its row is nested in a
	// batch. Promoting a child later must not renumber the rest of the queue.
	for _, record := range records {
		if record.inst.ActionName != "promote_transcode_candidate" || roots[record.inst.ID] == record.inst.ID {
			ordered = append(ordered, record)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		left, leftErr := time.Parse(time.RFC3339Nano, ordered[i].inst.CreatedAt)
		right, rightErr := time.Parse(time.RFC3339Nano, ordered[j].inst.CreatedAt)
		if leftErr == nil && rightErr == nil && !left.Equal(right) {
			return left.Before(right)
		}
		if ordered[i].inst.CreatedAt == ordered[j].inst.CreatedAt {
			return ordered[i].inst.ID < ordered[j].inst.ID
		}
		return ordered[i].inst.CreatedAt < ordered[j].inst.CreatedAt
	})
	numbers := map[string]int{}
	for i, r := range ordered {
		numbers[r.inst.ID] = i + 1
	}
	return roots, numbers, members
}

func (s *Server) queueStages(r operationRecord) []map[string]any {
	tmpl, ok := s.engine.GetTemplate(r.inst.ActionName)
	if !ok {
		return nil
	}
	stages := make([]map[string]any, 0, len(tmpl.Steps))
	for i, step := range tmpl.Steps {
		status := "pending"
		if i < r.inst.CurrentStep || r.inst.Status == store.ActionStatusCompleted {
			status = "completed"
		} else if i == r.inst.CurrentStep {
			status = r.inst.Status
		}
		stage := map[string]any{"name": step.Name, "status": status}
		if r.inst.ActionName == "transcode_batch" {
			counts := operationMap(operationValue(r, "counts"))
			if step.Name == "schedule_batch" && status == "completed" && operationNumber(counts["failed"]) > 0 {
				stage["status"] = "failed"
				if operationNumber(counts["completed"]) > 0 {
					stage["status"] = "partial"
				}
				stage["note"] = fmt.Sprintf("%.0f converted · %.0f failed", operationNumber(counts["completed"]), operationNumber(counts["failed"]))
			}
			if step.Name == "promote_batch" && status == "completed" {
				promotion := operationMap(operationValue(r, "batch_promotion"))
				if r.inputs["promote_candidates"] != true {
					stage["status"], stage["note"] = "skip", "Replacement not requested; originals kept."
				} else if (promotion != nil && operationNumber(promotion["eligible"]) == 0) || (promotion == nil && operationNumber(counts["completed"]) == 0 && operationNumber(counts["failed"]) > 0) {
					stage["status"], stage["note"] = "skip", "No eligible candidates to replace; originals kept."
				} else if promotion != nil && promotion["approved"] == false && operationNumber(promotion["promoted"]) == 0 {
					stage["status"], stage["note"] = "skip", "Replacement not approved; originals kept."
				}
			}
			if (r.inst.Status == store.ActionStatusFailed || r.inst.Status == store.ActionStatusCancelled) && i > r.inst.CurrentStep {
				stage["status"], stage["note"] = "skip", "Not run."
			}
		}
		stages = append(stages, stage)
	}
	return stages
}

var savingsFailurePattern = regexp.MustCompile(`benchmark winner predicts only (-?\d+(?:\.\d+)?)% savings, below required minimum (\d+(?:\.\d+)?)%`)

// Percentages differ by file, but they describe the same failed savings gate.
// Keep per-file estimates in details while grouping that common cause in the queue.
func batchReasonText(raw string, details bool) string {
	text := strings.TrimSpace(raw)
	var stored struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(text), &stored) == nil && stored.Error != "" {
		text = stored.Error
	}
	if match := savingsFailurePattern.FindStringSubmatch(text); match != nil {
		if !details {
			return "Sample estimates do not meet the minimum savings; originals kept."
		}
		estimate, _ := strconv.ParseFloat(match[1], 64)
		if estimate < 0 {
			return fmt.Sprintf("Estimated output is %s%% larger; at least %s%% savings required. Full conversion not started; original kept.", strconv.FormatFloat(-estimate, 'f', -1, 64), match[2])
		}
		return fmt.Sprintf("Estimated savings %s%%; at least %s%% required. Full conversion not started; original kept.", match[1], match[2])
	}
	return text
}

func batchQueueFeedback(items []store.TranscodeBatchItem) map[string]any {
	preview := []batchItemView{}
	ordered := append([]store.TranscodeBatchItem(nil), items...)
	sort.SliceStable(ordered, func(i, j int) bool {
		active := func(item store.TranscodeBatchItem) bool {
			return item.Status == "running" || item.Status == "waiting_decision" || item.Status == "waiting_for_slot"
		}
		return active(ordered[i]) && !active(ordered[j])
	})
	for _, item := range ordered[:min(2, len(ordered))] {
		preview = append(preview, batchItemView{FilePath: item.FilePath, DisplayLabel: item.DisplayLabel, Status: item.Status, Reasons: item.Reasons, Error: item.Error})
	}
	reasons := map[string]int{}
	commonDir := ""
	for i, item := range items {
		dir := filepath.Dir(item.FilePath)
		if i == 0 {
			commonDir = dir
		} else if dir != commonDir {
			commonDir = ""
		}
		if item.Status == "skip" || item.Status == "failed" || item.Status == "review" {
			seen := map[string]bool{}
			itemReasons := []string{}
			for _, reason := range item.Reasons {
				// Positive selection criteria describe eligibility, not why work
				// was skipped or needs a decision.
				if reason != "" && reason != "anime" && reason != "h264_1080p" && reason != "oversized" && reason != "explicit profile requested" {
					itemReasons = append(itemReasons, batchReasonText(reason, false))
				}
			}
			if item.Error != "" {
				itemReasons = []string{batchReasonText(item.Error, false)}
			} else if item.Status == "failed" && len(itemReasons) == 0 {
				itemReasons = []string{"Conversion failed; open file details"}
			}
			for _, reason := range itemReasons {
				if reason != "" && !seen[reason] {
					reasons[reason]++
					seen[reason] = true
				}
			}
		}
	}
	reasonViews := []map[string]any{}
	for reason, count := range reasons {
		reasonViews = append(reasonViews, map[string]any{"reason": reason, "count": count})
	}
	sort.Slice(reasonViews, func(i, j int) bool {
		if reasonViews[i]["count"] == reasonViews[j]["count"] {
			return reasonViews[i]["reason"].(string) < reasonViews[j]["reason"].(string)
		}
		return reasonViews[i]["count"].(int) > reasonViews[j]["count"].(int)
	})
	if len(reasonViews) > 2 {
		reasonViews = reasonViews[:2]
	}
	context := ""
	if commonDir != "" && commonDir != "." && commonDir != "/" {
		context = filepath.Base(commonDir)
		if strings.HasPrefix(strings.ToLower(context), "season") {
			context = filepath.Base(filepath.Dir(commonDir)) + " · " + context
		}
	}
	return map[string]any{"files": preview, "file_count": len(items), "context": context, "reasons": reasonViews}
}
