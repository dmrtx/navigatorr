package maintenanceui

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/store"
)

// Queue grouping follows durable action links, never filenames: independent
// requests for the same media must retain their own outcome and identity.
func queueWorkflows(records []operationRecord, byID map[string]operationRecord, replacements map[string]string) (map[string]string, map[string]int, map[string][]operationRecord) {
	roots := map[string]string{}
	members := map[string][]operationRecord{}
	for _, r := range records {
		root := r.inst.ID
		if r.inst.ActionName == "promote_transcode_candidate" {
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
		members[root] = append(members[root], r)
	}
	ordered := []operationRecord{}
	for root := range members {
		ordered = append(ordered, byID[root])
	}
	sort.Slice(ordered, func(i, j int) bool {
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
		stages = append(stages, map[string]any{"name": step.Name, "status": status})
	}
	return stages
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
			for _, reason := range item.Reasons {
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
