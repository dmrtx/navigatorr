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
	"unicode/utf8"

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
			if parent := operationString(r.inputs["batch_promote_parent_id"]); byID[parent].inst.ActionName == "transcode_batch" {
				roots[r.inst.ID] = parent
				continue
			}
			source := operationString(r.inputs["transcode_action_id"])
			if source == "" {
				source = operationString(operationMap(operationValue(r, "promotion"))["transcode_action_id"])
			}
			if original, ok := byID[source]; ok && original.inst.ActionName == "transcode_media" {
				root = source
			}
		} else if (r.inst.ActionName == "transcode_media" || r.inst.ActionName == "benchmark_transcode") && (replacements[root] == "" || operationString(byID[replacements[root]].inputs["batch_promote_parent_id"]) == operationString(r.inputs["parent_action_id"])) {
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
	skippedSteps := s.skippedStoredSteps(r)
	for i, step := range tmpl.Steps {
		status := "pending"
		if i < r.inst.CurrentStep || r.inst.Status == store.ActionStatusCompleted {
			status = "completed"
		} else if i == r.inst.CurrentStep {
			status = r.inst.Status
		}
		stage := map[string]any{"name": step.Name, "status": status}
		if status == "completed" && skippedSteps[i] {
			stage["status"], stage["note"] = "skip", "Not needed; original kept."
		} else if status == "completed" && r.inst.ActionName == "transcode_media" && step.Name == "accept_result" && operationValue(r, "skip_transcode") == true {
			stage["note"] = "Original kept."
		}
		if (r.inst.Status == store.ActionStatusFailed || r.inst.Status == store.ActionStatusCancelled) && i > r.inst.CurrentStep {
			stage["status"], stage["note"] = "skip", "Not run."
		}
		if r.inst.ActionName == "transcode_batch" {
			counts := operationMap(operationValue(r, "counts"))
			if step.Name == "promote_batch" && (r.inputs["promote_candidates"] != true && r.state["post_batch_promotion"] != true) {
				stage["status"], stage["note"] = "skip", "Replacement not requested; originals kept."
			}
			if step.Name == "schedule_batch" && status == "completed" && operationNumber(counts["completed"]) == 0 && operationNumber(counts["failed"]) == 0 && operationNumber(counts["skip"]) > 0 {
				stage["status"], stage["note"] = "skip", "No conversions started; selected files were skipped."
			}
			if step.Name == "schedule_batch" && status == "completed" && operationNumber(counts["failed"]) > 0 {
				stage["status"] = "failed"
				if operationNumber(counts["completed"]) > 0 {
					stage["status"] = "partial"
				}
				stage["note"] = fmt.Sprintf("%.0f converted · %.0f failed", operationNumber(counts["completed"]), operationNumber(counts["failed"]))
			}
			if step.Name == "promote_batch" && status == "completed" {
				promotion := operationMap(operationValue(r, "batch_promotion"))
				if r.inputs["promote_candidates"] != true && r.state["post_batch_promotion"] != true {
					stage["status"], stage["note"] = "skip", "Replacement not requested; originals kept."
				} else if (promotion != nil && operationNumber(promotion["eligible"]) == 0) || (promotion == nil && operationNumber(counts["completed"]) == 0 && (operationNumber(counts["failed"]) > 0 || operationNumber(counts["skip"]) > 0)) {
					stage["status"], stage["note"] = "skip", "No eligible candidates to replace; originals kept."
				} else if promotion != nil && promotion["approved"] == false && operationNumber(promotion["promoted"]) == 0 {
					stage["status"], stage["note"] = "skip", "Replacement not approved; originals kept."
				}
			}
		}
		stages = append(stages, stage)
	}
	return stages
}

// skippedStoredSteps returns the step indexes whose stored row says the step
// was skipped. A completed action otherwise paints every stage completed, even
// the ones that never ran because the job ended early. Only a completed
// transcode that ended without a candidate can have such steps, so the step
// log is read for those alone.
func (s *Server) skippedStoredSteps(r operationRecord) map[int]bool {
	if r.inst.ActionName != "transcode_media" || r.inst.Status != store.ActionStatusCompleted || operationValue(r, "skip_transcode") != true || s.engine == nil {
		return nil
	}
	st := s.engine.Deps().Store
	if st == nil {
		return nil
	}
	steps, err := st.GetActionSteps(r.inst.ID)
	if err != nil {
		return nil
	}
	// Rows are ordered by step and then by insertion: the last row for a step
	// is its final outcome.
	final := map[int]string{}
	for _, step := range steps {
		final[step.StepIndex] = step.Status
	}
	skipped := map[int]bool{}
	for index, status := range final {
		if status == "skipped" {
			skipped[index] = true
		}
	}
	return skipped
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

func batchQueueFeedback(items []store.TranscodeBatchItem, roots []string) map[string]any {
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
	rejected, policy := 0, 0
	commonDir := ""
	for i, item := range items {
		if item.Status == "failed" && savingsFailurePattern.MatchString(item.Error) {
			policy++
		}
		if item.Status == "failed" && strings.Contains(batchReasonText(item.Error, true), "transcode candidate rejected by user decision") {
			rejected++
		}
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
		context = folderLabel(commonDir)
	}
	// Files in several folders of one show or movie (seasons, extras) are named
	// by the folder that holds them all, if that is inside a library rather than
	// the library itself. Failing that, by the files.
	name := ""
	shared := ""
	if commonDir == "" {
		if ancestor := commonAncestor(items); insideLibrary(ancestor, roots) {
			shared = ancestor
			name = folderLabel(ancestor)
		}
	}
	if name == "" {
		name = batchNameFromFiles(items)
	}
	// Where to look for the files: their shared folder, or the first one's.
	folder := commonDir
	if folder == "" {
		folder = shared
	}
	if (folder == "" || folder == ".") && len(items) > 0 {
		folder = filepath.Dir(items[0].FilePath)
	}
	if folder == "." || folder == "/" {
		folder = ""
	}
	return map[string]any{"files": preview, "file_count": len(items), "context": context, "name": name, "folder": folder, "reasons": reasonViews, "rejected_count": rejected, "policy_count": policy}
}

// folderLabel is a folder's name; a season folder is named with its show.
func folderLabel(dir string) string {
	label := filepath.Base(dir)
	if strings.HasPrefix(strings.ToLower(label), "season") {
		label = filepath.Base(filepath.Dir(dir)) + " · " + label
	}
	return label
}

// commonAncestor is the deepest folder that holds every file, or "".
func commonAncestor(items []store.TranscodeBatchItem) string {
	var shared []string
	for i, item := range items {
		if item.FilePath == "" || !filepath.IsAbs(item.FilePath) {
			return ""
		}
		parts := strings.Split(strings.Trim(filepath.Dir(item.FilePath), "/"), "/")
		if i == 0 {
			shared = parts
			continue
		}
		n := 0
		for n < len(shared) && n < len(parts) && shared[n] == parts[n] {
			n++
		}
		shared = shared[:n]
	}
	if len(shared) == 0 {
		return ""
	}
	return "/" + strings.Join(shared, "/")
}

// insideLibrary reports whether dir is below one of the library roots, not a
// root itself or above one, so it names a show or movie rather than a shelf.
func insideLibrary(dir string, roots []string) bool {
	for _, root := range roots {
		root = strings.TrimRight(root, "/")
		if root != "" && dir != "" && strings.HasPrefix(dir, root+"/") {
			return true
		}
	}
	return false
}

var nameSeparators = " -_.[(]),"

// A trailing season or episode marker names a position in a series, not the series.
var trailingEpisodeMarker = regexp.MustCompile(`(?i)[\s._\-]*(?:s\d+(?:e\d*)?|season\s*\d*|e\d+|ep\s*\d+)$`)

// batchNameFromFiles names a batch whose files do not share a folder, from the
// files themselves: the start of their names when they have one in common
// (the series), otherwise the first file and how many others come with it.
func batchNameFromFiles(items []store.TranscodeBatchItem) string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		name := filepath.Base(item.FilePath)
		if item.FilePath == "" || name == "." || name == "/" {
			continue
		}
		names = append(names, strings.TrimSuffix(name, filepath.Ext(name)))
	}
	if len(names) == 0 {
		return ""
	}
	if len(names) == 1 {
		return names[0]
	}
	prefix := names[0]
	for _, name := range names[1:] {
		n := 0
		for n < len(prefix) && n < len(name) && prefix[n] == name[n] {
			n++
		}
		prefix = prefix[:n]
	}
	// Stop at a word boundary unless every name ends exactly there, so
	// "Show - S01E0" does not leave a half-written episode number.
	whole := true
	for _, name := range names {
		if len(name) != len(prefix) && !strings.ContainsRune(nameSeparators, rune(name[len(prefix)])) {
			whole = false
		}
	}
	if !whole {
		if cut := strings.LastIndexAny(prefix, nameSeparators); cut >= 0 {
			prefix = prefix[:cut]
		} else {
			prefix = ""
		}
	}
	prefix = strings.TrimRight(prefix, nameSeparators)
	prefix = strings.TrimLeft(strings.TrimRight(trailingEpisodeMarker.ReplaceAllString(prefix, ""), nameSeparators), " -_.")
	// A release group's tag alone ("[Group]") names nobody's show.
	if utf8.RuneCountInString(prefix) >= 4 && strings.Count(prefix, "[") == strings.Count(prefix, "]") {
		return prefix
	}
	if others := len(names) - 1; others == 1 {
		return names[0] + " + 1 more"
	} else {
		return fmt.Sprintf("%s + %d more", names[0], others)
	}
}
