package maintenanceui

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/mark3labs/mcp-go/mcp"
)

// Savings describe content bytes, rather than filesystem allocation or free
// disk measurements. A smaller candidate is only potential savings: its
// original and recovery copy may still exist. Realized savings require the
// promotion's verified final cleanup to have completed.
type operationSavings struct {
	SourceBytes         *int64 `json:"source_bytes,omitempty"`
	CandidateBytes      *int64 `json:"candidate_bytes,omitempty"`
	EstimatedBytes      *int64 `json:"estimated_bytes,omitempty"`
	EstimatedSavedBytes *int64 `json:"estimated_saved_bytes,omitempty"`
	CandidateSavedBytes *int64 `json:"candidate_saved_bytes,omitempty"`
	RealizedSavedBytes  *int64 `json:"realized_saved_bytes,omitempty"`
	EstimateKind        string `json:"estimate_kind,omitempty"`
	ETASeconds          *int64 `json:"eta_seconds,omitempty"`
	MeasuredFiles       int    `json:"measured_files,omitempty"`
	Partial             bool   `json:"partial,omitempty"`
}

type savingsReplacement struct {
	ActionID        string `json:"action_id"`
	SourcePath      string `json:"source_path"`
	CompletedAt     string `json:"completed_at"`
	SourceBytes     int64  `json:"source_bytes"`
	CandidateBytes  int64  `json:"candidate_bytes"`
	SavedBytes      int64  `json:"saved_bytes"`
	CumulativeBytes int64  `json:"cumulative_bytes"`
}

type operationsPage struct {
	Jobs        []map[string]any `json:"jobs"`
	Offset      int              `json:"offset"`
	HasMore     bool             `json:"has_more"`
	Total       int              `json:"total"`
	ActiveCount int              `json:"active_count"`
	Savings     struct {
		RealizedBytes         int64  `json:"realized_bytes"`
		CompletedReplacements int    `json:"completed_replacements"`
		CandidateBytes        int64  `json:"candidate_bytes"`
		CandidateSavedBytes   int64  `json:"candidate_saved_bytes"`
		EstimatedBytes        int64  `json:"estimated_bytes"`
		EstimatedSavedBytes   int64  `json:"estimated_saved_bytes"`
		Measurement           string `json:"measurement"`
	} `json:"savings"`
	History    []savingsReplacement `json:"history"`
	ObservedAt string               `json:"observed_at"`
}

type operationRecord struct {
	inst                   store.ActionInstance
	inputs, outputs, state map[string]any
	savings                operationSavings
}

func decodeOperationJSON(raw string) map[string]any {
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}
func operationMap(v any) map[string]any { out, _ := v.(map[string]any); return out }
func operationNumber(v any) float64     { f, _ := v.(float64); return f }
func operationString(v any) string      { s, _ := v.(string); return s }
func operationValue(r operationRecord, key string) any {
	if v, ok := r.outputs[key]; ok {
		return v
	}
	return r.state[key]
}
func operationActive(status string) bool {
	return status != store.ActionStatusCompleted && status != store.ActionStatusFailed && status != store.ActionStatusCancelled
}
func operationSourceKey(r operationRecord) string {
	path := operationString(r.state["resolved_path"])
	if path == "" {
		path = operationString(r.inputs["path"])
	}
	sha := operationString(operationValue(r, "original_sha256"))
	if path == "" {
		return "action:" + r.inst.ID
	}
	return path + "\x00" + sha
}

// Import context is a durable routing hint, never evidence that replacement is
// safe. The existing promotion workflow must independently resolve the exact
// original file and validate the candidate again before any mutation.
func operationLibraryID(value any) int64 {
	if text, ok := value.(string); ok {
		n, err := strconv.ParseInt(text, 10, 32)
		if err == nil && n > 0 {
			return n
		}
		return 0
	}
	n, ok := value.(float64)
	if ok && n > 0 && n <= math.MaxInt32 && math.Trunc(n) == n {
		return int64(n)
	}
	return 0
}

func operationReplacementContext(r operationRecord, recordsByID map[string]operationRecord) map[string]any {
	context := func(service string, id int64, source string) map[string]any {
		if id <= 0 || (service != "sonarr" && service != "radarr") {
			return nil
		}
		key := "series_id"
		if service == "radarr" {
			key = "movie_id"
		}
		return map[string]any{"service": service, key: id, "source": source}
	}
	if library := operationMap(r.inputs["library_context"]); library != nil {
		if hint := context(operationString(library["service"]), operationLibraryID(library["id"]), "job_inputs"); hint != nil {
			return hint
		}
	}
	service := operationString(r.inputs["service"])
	key := "series_id"
	if service == "radarr" {
		key = "movie_id"
	}
	id := operationLibraryID(r.inputs[key])
	if id == 0 {
		id = operationLibraryID(r.inputs["media_id"])
	}
	if hint := context(service, id, "job_inputs"); hint != nil {
		return hint
	}
	if parent, found := recordsByID[operationString(r.inputs["parent_action_id"])]; found && parent.inst.ActionName == "transcode_batch" {
		if hint := context(operationString(parent.inputs["service"]), operationLibraryID(parent.inputs["series_id"]), "parent_batch"); hint != nil {
			return hint
		}
		if _, filesystemBatch := parent.inputs["paths"]; filesystemBatch {
			return map[string]any{"service": "filesystem", "source": "parent_batch"}
		}
	}
	return nil
}

func projectOperationSavings(r operationRecord, now time.Time) operationSavings {
	var s operationSavings
	original := operationMap(operationValue(r, "original"))
	source := int64(operationNumber(original["size_bytes"]))
	if source > 0 {
		s.SourceBytes = &source
	}
	candidate := int64(operationNumber(operationMap(operationValue(r, "result"))["size_bytes"]))
	if candidate <= 0 {
		candidate = int64(operationNumber(operationValue(r, "candidate_size_bytes")))
	}
	if r.inst.ActionName == "promote_transcode_candidate" {
		p := operationMap(operationValue(r, "promotion"))
		source = int64(operationNumber(p["original_bytes"]))
		candidate = int64(operationNumber(p["candidate_bytes"]))
		if source > 0 {
			s.SourceBytes = &source
		}
		cleaned := p["recovery_cleanup_completed"] == true || p["recovery_cleanup_started"] == true && r.outputs["recovery_retained"] == false
		verified := p["recovery_verified_before_replacement"] == true || r.outputs["original_integrity"] == "verified_before_replacement"
		if r.inst.Status == store.ActionStatusCompleted && r.outputs["promoted"] == true && r.outputs["recovery_retained"] == false && cleaned && verified && source > 0 && candidate > 0 {
			saved := source - candidate
			s.RealizedSavedBytes = &saved
		}
	}
	if source > 0 && candidate > 0 {
		s.CandidateBytes = &candidate
		saved := source - candidate
		s.CandidateSavedBytes = &saved
	}
	winner := operationMap(operationMap(operationValue(r, "benchmark_decision"))["winner"])
	estimated := int64(operationNumber(winner["estimated_bytes"]))
	if estimated > 0 && source > 0 {
		s.EstimateKind = "sampled_benchmark"
	} else if percent, ok := operationValue(r, "expected_savings_percent").(float64); ok && source > 0 && percent <= 100 {
		estimated = int64(float64(source) * (1 - percent/100))
		s.EstimateKind = "profile_heuristic"
	}
	if estimated > 0 && source > 0 {
		s.EstimatedBytes = &estimated
		saved := source - estimated
		s.EstimatedSavedBytes = &saved
	}
	phase := operationString(operationValue(r, "transcode_phase"))
	if phase == "encoding" && operationActive(r.inst.Status) && operationValue(r, "progress_is_stale") != true {
		progress := operationNumber(operationValue(r, "progress"))
		speed := operationNumber(operationValue(r, "speed"))
		duration := operationNumber(original["duration_sec"])
		last, err := time.Parse(time.RFC3339Nano, operationString(operationValue(r, "last_progress_at")))
		if err == nil && now.Sub(last) >= 0 && now.Sub(last) <= 30*time.Second && progress >= 0 && progress < 100 && speed > 0 && duration > 0 {
			eta := int64(math.Ceil(duration * (1 - progress/100) / speed))
			s.ETASeconds = &eta
		}
	}
	return s
}

func operationPaging(r *http.Request) (offset, limit int, err error) {
	limit = 25
	for key, target := range map[string]*int{"offset": &offset, "limit": &limit} {
		if raw := r.URL.Query().Get(key); raw != "" {
			*target, err = strconv.Atoi(raw)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid %s", key)
			}
		}
	}
	if offset < 0 || limit < 1 || limit > 100 {
		return 0, 0, fmt.Errorf("offset must be nonnegative and limit between 1 and 100")
	}
	return
}

// Batch figures project each real encoding child once. They never enter the
// global totals a second time. Unknown queued files are explicitly partial,
// rather than extrapolating a sampled episode's result across a whole season.
func aggregateBatchSavings(records []operationRecord) {
	children := map[string][]operationRecord{}
	parentsByChild := map[string]string{}
	for _, r := range records {
		if r.inst.ActionName == "transcode_media" {
			if parent := operationString(r.inputs["parent_action_id"]); parent != "" {
				children[parent] = append(children[parent], r)
				parentsByChild[r.inst.ID] = parent
			}
		}
	}
	promotions := map[string][]operationRecord{}
	for _, r := range records {
		if r.savings.RealizedSavedBytes != nil {
			p := operationMap(operationValue(r, "promotion"))
			if parent := parentsByChild[operationString(p["transcode_action_id"])]; parent != "" {
				promotions[parent] = append(promotions[parent], r)
			}
		}
	}
	for i := range records {
		if records[i].inst.ActionName != "transcode_batch" {
			continue
		}
		s := operationSavings{}
		add := func(dst **int64, src *int64) {
			if src != nil {
				if *dst == nil {
					n := int64(0)
					*dst = &n
				}
				**dst += *src
			}
		}
		seen := map[string]bool{}
		for _, child := range children[records[i].inst.ID] {
			key := operationSourceKey(child)
			if seen[key] {
				continue
			}
			seen[key] = true
			add(&s.SourceBytes, child.savings.SourceBytes)
			add(&s.CandidateBytes, child.savings.CandidateBytes)
			add(&s.CandidateSavedBytes, child.savings.CandidateSavedBytes)
			add(&s.EstimatedBytes, child.savings.EstimatedBytes)
			add(&s.EstimatedSavedBytes, child.savings.EstimatedSavedBytes)
			if child.savings.CandidateBytes != nil {
				s.MeasuredFiles++
			}
		}
		seen = map[string]bool{}
		for _, promoted := range promotions[records[i].inst.ID] {
			p := operationMap(operationValue(promoted, "promotion"))
			key := operationString(p["original_path"]) + "\x00" + operationString(p["original_sha256"])
			if seen[key] {
				continue
			}
			seen[key] = true
			add(&s.RealizedSavedBytes, promoted.savings.RealizedSavedBytes)
		}
		counts := operationMap(operationValue(records[i], "counts"))
		planned := int(operationNumber(counts["transcode"]))
		if planned == 0 {
			planned = int(operationNumber(counts["total"])) - int(operationNumber(counts["skip"])) - int(operationNumber(counts["review"]))
		}
		s.Partial = s.MeasuredFiles < planned
		if s.EstimatedBytes != nil {
			s.EstimateKind = "encoding_children"
		}
		records[i].savings = s
	}
}

func (s *Server) operations(w http.ResponseWriter, req *http.Request) {
	offset, limit, err := operationPaging(req)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	status := strings.TrimSpace(req.URL.Query().Get("status"))
	selectedID := strings.TrimSpace(req.URL.Query().Get("id"))
	grouped := req.URL.Query().Get("group") == "workflow" && selectedID == ""
	if status != "" && status != "all" && status != "archived" && status != "active" && status != "pending" && status != "running" && status != "waiting_external" && status != "waiting_decision" && status != "completed" && status != "failed" && status != "cancelled" {
		fail(w, 400, "invalid workflow status")
		return
	}
	if s.engine.Deps().Store == nil {
		fail(w, 503, "maintenance store is required")
		return
	}
	instances, err := s.engine.Deps().Store.ListMaintenanceActionSnapshot()
	if err != nil {
		fail(w, 500, "read maintenance history")
		return
	}
	archives, err := s.engine.Deps().Store.MaintenanceArchives()
	if err != nil {
		fail(w, 500, "read archive state")
		return
	}
	now := time.Now().UTC()
	page := operationsPage{Jobs: []map[string]any{}, Offset: offset, History: []savingsReplacement{}, ObservedAt: now.Format(time.RFC3339Nano)}
	page.Savings.Measurement = "verified_content_bytes_after_replacement_cleanup; not filesystem free space"
	records := make([]operationRecord, 0, len(instances))
	for _, inst := range instances {
		r := operationRecord{inst: inst, inputs: decodeOperationJSON(inst.InputsJSON), outputs: decodeOperationJSON(inst.OutputsJSON), state: decodeOperationJSON(inst.StateJSON)}
		r.savings = projectOperationSavings(r, now)
		records = append(records, r)
		if operationActive(inst.Status) {
			page.ActiveCount++
		}
	}
	aggregateBatchSavings(records)
	recordsByID := make(map[string]operationRecord, len(records))
	existingReplacements := queueReplacementLinks(records)
	previewExecutions := map[string]string{}
	for _, r := range records {
		recordsByID[r.inst.ID] = r
	}
	roots, numbers, members := queueWorkflows(records, recordsByID, existingReplacements)
	for _, r := range records {
		root := roots[r.inst.ID]
		if r.inst.ActionName == "transcode_batch" && r.inst.ID != root && recordsByID[root].inputs["dry_run"] == true {
			previewExecutions[root] = r.inst.ID
		}
	}
	activeWorkflows := map[string]bool{}
	for _, r := range records {
		if operationActive(r.inst.Status) {
			activeWorkflows[roots[r.inst.ID]] = true
		}
	}
	representatives := map[string]string{}
	for root := range members {
		representatives[root] = root
		if replacement := existingReplacements[root]; replacement != "" {
			representatives[root] = replacement
		}
		if execution := previewExecutions[root]; execution != "" {
			representatives[root] = execution
		}
	}
	if grouped {
		page.ActiveCount = 0
		for _, id := range representatives {
			if operationActive(recordsByID[id].inst.Status) {
				page.ActiveCount++
			}
		}
	}
	// Oldest verified replacement wins an identical original-content claim.
	// Later transcodes of the new content have a different original digest and
	// remain distinct. Failed cleanups and candidate-only jobs are excluded.
	chronological := append([]operationRecord(nil), records...)
	sort.Slice(chronological, func(i, j int) bool {
		if chronological[i].inst.UpdatedAt == chronological[j].inst.UpdatedAt {
			return chronological[i].inst.ID < chronological[j].inst.ID
		}
		return chronological[i].inst.UpdatedAt < chronological[j].inst.UpdatedAt
	})
	replaced := map[string]bool{}
	replacedActions := map[string]bool{}
	for _, r := range chronological {
		if r.savings.RealizedSavedBytes == nil {
			continue
		}
		p := operationMap(operationValue(r, "promotion"))
		path, sha := operationString(p["original_path"]), operationString(p["original_sha256"])
		if path == "" || sha == "" {
			continue
		}
		key := path + "\x00" + sha
		replacedActions[operationString(p["transcode_action_id"])] = true
		if replaced[key] {
			continue
		}
		replaced[key] = true
		page.Savings.RealizedBytes += *r.savings.RealizedSavedBytes
		page.Savings.CompletedReplacements++
		page.History = append(page.History, savingsReplacement{ActionID: r.inst.ID, SourcePath: path, CompletedAt: r.inst.UpdatedAt, SourceBytes: *r.savings.SourceBytes, CandidateBytes: *r.savings.CandidateBytes, SavedBytes: *r.savings.RealizedSavedBytes, CumulativeBytes: page.Savings.RealizedBytes})
	}
	seenCandidates, seenEstimates := map[string]bool{}, map[string]bool{}
	filtered := []operationRecord{}
	for _, r := range records {
		key := operationSourceKey(r)
		if r.inst.ActionName == "transcode_media" && !replaced[key] && !replacedActions[r.inst.ID] {
			if r.inst.Status == store.ActionStatusCompleted && r.outputs["original_intact"] == true && r.savings.CandidateSavedBytes != nil && !seenCandidates[key] {
				page.Savings.CandidateBytes += *r.savings.CandidateSavedBytes
				seenCandidates[key] = true
			}
			if operationActive(r.inst.Status) && r.savings.EstimatedSavedBytes != nil && !seenEstimates[key] {
				page.Savings.EstimatedBytes += *r.savings.EstimatedSavedBytes
				seenEstimates[key] = true
			}
		}
		if grouped && representatives[roots[r.inst.ID]] != r.inst.ID {
			continue
		}
		archived := archives[roots[r.inst.ID]] != "" && !activeWorkflows[roots[r.inst.ID]]
		// An archived workflow that resumes through another client must remain
		// visible while active. Direct detail links can always read its history.
		if selectedID == "" && archived != (status == "archived") {
			continue
		}
		if (selectedID == "" || selectedID == r.inst.ID) && (status == "" || status == "all" || status == "archived" || r.inst.Status == status || status == "active" && operationActive(r.inst.Status)) {
			filtered = append(filtered, r)
		}
	}
	if grouped {
		sort.SliceStable(filtered, func(i, j int) bool {
			// Workflow creation order survives polling, state changes and a
			// conversion becoming a replacement. New submissions appear first.
			return numbers[roots[filtered[i].inst.ID]] > numbers[roots[filtered[j].inst.ID]]
		})
	}
	page.Savings.EstimatedSavedBytes = page.Savings.EstimatedBytes
	page.Savings.CandidateSavedBytes = page.Savings.CandidateBytes
	page.Total = len(filtered)
	end := min(offset, len(filtered)) + min(limit, max(0, len(filtered)-offset))
	page.HasMore = end < len(filtered)
	if offset < len(filtered) {
		for _, r := range filtered[offset:end] {
			if err := req.Context().Err(); err != nil {
				return
			}
			t := s.mcp.GetTool("action_status")
			if t == nil {
				fail(w, 503, "action_status is not configured")
				return
			}
			res, err := t.Handler(req.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "action_status", Arguments: map[string]any{"id": r.inst.ID}}})
			if err != nil || res == nil || res.IsError {
				fail(w, 500, "read action status")
				return
			}
			var summary map[string]any
			for _, content := range res.Content {
				if text, ok := content.(mcp.TextContent); ok {
					_ = json.Unmarshal([]byte(text.Text), &summary)
					break
				}
			}
			job := operationMap(summary["action"])
			if job == nil {
				fail(w, 500, "invalid action status projection")
				return
			}
			job["savings"] = r.savings
			root := roots[r.inst.ID]
			job["workflow_id"] = root
			job["archive_id"] = root
			job["can_archive"] = !activeWorkflows[root]
			job["archived"] = archives[root] != "" && !activeWorkflows[root]
			if r.inst.ID != root && recordsByID[root].inputs["dry_run"] == true {
				job["preview_action_id"] = root
			}
			job["number"] = numbers[root]
			job["stages"] = s.queueStages(r)
			// Planning can hash large files before a promotion checkpoint exists.
			// Its durable source link already identifies the file during that stage.
			if r.inst.ActionName == "promote_transcode_candidate" {
				if original, ok := recordsByID[root]; ok && original.inst.ActionName == "transcode_media" {
					path := operationString(original.inputs["path"])
					if resolved := operationString(original.state["resolved_path"]); resolved != "" {
						path = resolved
					}
					job["source_path"] = path
					if r.savings.SourceBytes == nil {
						job["savings"] = original.savings
					}
				}
			}
			if recordsByID[root].inst.ActionName != "transcode_batch" && len(members[root]) > 1 {
				related := []map[string]any{}
				for _, id := range []string{root, existingReplacements[root]} {
					if member, ok := recordsByID[id]; ok {
						related = append(related, map[string]any{"id": id, "action_name": member.inst.ActionName, "status": member.inst.Status, "created_at": member.inst.CreatedAt, "stages": s.queueStages(member)})
					}
				}
				job["workflow_actions"] = related
			}
			job["candidate_ready"] = r.inst.ActionName == "transcode_media" && r.inst.Status == store.ActionStatusCompleted && operationValue(r, "skip_transcode") != true && operationValue(r, "original_intact") == true && r.savings.CandidateBytes != nil && !replacedActions[r.inst.ID] && !replaced[operationSourceKey(r)]
			if r.inst.ActionName == "transcode_media" {
				job["replaced"] = replacedActions[r.inst.ID] || replaced[operationSourceKey(r)]
			}
			if r.inst.ActionName == "transcode_media" {
				if context := operationReplacementContext(r, recordsByID); context != nil {
					job["replacement_context"] = context
				}
				if existing := existingReplacements[r.inst.ID]; existing != "" {
					job["replacement_action_id"] = existing
				}
			}
			_, httpWorker := s.engine.Deps().Transcode.(*transcode.HTTPExecutor)
			comparison := r
			if r.inst.ActionName == "promote_transcode_candidate" {
				comparison = recordsByID[root]
			}
			if httpWorker && operationValue(comparison, "benchmark_comparison_available") == true {
				job["comparison_action_id"] = comparison.inst.ID
			}
			job["logs_available"] = httpWorker && (operationString(r.state["job_id"]) != "" || operationString(r.state["transcode_job_id"]) != "")
			if r.inst.ActionName == "transcode_batch" {
				if r.inputs["dry_run"] == true && r.inst.Status == store.ActionStatusCompleted {
					job["preview_execution_action_id"] = previewExecutions[r.inst.ID]
				}
				job["replacement_requested"] = r.inputs["promote_candidates"] == true
				job["paused"] = operationValue(r, "paused") == true
				items, err := s.engine.Deps().Store.ListTranscodeBatchItems(r.inst.ID)
				inferred := false
				if err == nil {
					items, inferred, err = s.previewPendingItems(r.inst, items)
				}
				if err != nil {
					fail(w, 500, "read batch contents")
					return
				}
				job["batch_files"] = batchQueueFeedback(items)
				if inferred {
					batch := operationMap(job["batch"])
					if batch == nil {
						batch = map[string]any{}
					}
					batch["total"] = len(items)
					batch[items[0].Status] = len(items)
					job["batch"] = batch
				}
				if operationActive(r.inst.Status) {
					activities := []map[string]any{}
					for _, child := range members[root] {
						if child.inst.ID == r.inst.ID || (child.inst.Status != store.ActionStatusRunning && child.inst.Status != store.ActionStatusWaitingExternal) {
							continue
						}
						res, err := t.Handler(req.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "action_status", Arguments: map[string]any{"id": child.inst.ID}}})
						if err == nil && res != nil && !res.IsError {
							var data map[string]any
							for _, content := range res.Content {
								if text, ok := content.(mcp.TextContent); ok {
									_ = json.Unmarshal([]byte(text.Text), &data)
									break
								}
							}
							childJob := operationMap(data["action"])
							file := operationString(child.inputs["path"])
							if file == "" {
								file = operationString(operationMap(operationValue(child, "promotion"))["original_path"])
							}
							activities = append(activities, map[string]any{"id": child.inst.ID, "parent_action_id": child.inputs["parent_action_id"], "action_name": child.inst.ActionName, "current_step": child.inst.CurrentStep, "stages": s.queueStages(child), "file": file, "worker": childJob["worker"], "work": childJob["work"], "waiting_condition": childJob["waiting_condition"]})
							job["activity_waiting_condition"] = childJob["waiting_condition"]
							if worker := operationMap(childJob["worker"]); worker != nil {
								job["worker"] = worker
								job["activity_file"] = operationString(child.inputs["path"])
							}
						}
					}
					job["activities"] = activities
				}
			}
			page.Jobs = append(page.Jobs, job)
		}
	}
	if len(page.History) > 25 {
		page.History = page.History[len(page.History)-25:]
	}
	writeJSON(w, http.StatusOK, page)
}
