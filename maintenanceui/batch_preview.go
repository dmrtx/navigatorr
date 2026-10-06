package maintenanceui

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

const previewExecutionPrefix = "web-preview:"

type previewSetting struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type batchPreviewPlan struct {
	ID                string           `json:"id"`
	Title             string           `json:"title"`
	Eligible          int              `json:"eligible"`
	Other             int              `json:"other"`
	Files             []string         `json:"files"`
	Settings          []previewSetting `json:"settings"`
	ExecutionActionID string           `json:"execution_action_id,omitempty"`
}

// A preview is a completed inspection. Starting it admits a separate encode
// with a frozen eligible selection and the original immutable settings.
func (s *Server) previewPlan(id string) (batchPreviewPlan, map[string]any, error) {
	plan := batchPreviewPlan{ID: id, Files: []string{}, Settings: []previewSetting{}}
	st := s.engine.Deps().Store
	if st == nil {
		return plan, nil, fmt.Errorf("maintenance store is required")
	}
	inst, err := st.GetActionInstance(id)
	if err != nil || inst == nil || inst.ActionName != "transcode_batch" || inst.Status != store.ActionStatusCompleted {
		return plan, nil, fmt.Errorf("this job is not a completed batch preview")
	}
	inputs := decodeOperationJSON(inst.InputsJSON)
	if inputs["dry_run"] != true {
		return plan, nil, fmt.Errorf("this job is not a completed batch preview")
	}
	if existing, err := st.FindActionByIdempotencyKey("transcode_batch", previewExecutionPrefix+id); err != nil {
		return plan, nil, err
	} else if existing != nil {
		plan.ExecutionActionID = existing.ID
	}
	// Keep execution inputs on the server; only display settings enter the UI.
	allowed := map[string]bool{}
	template, _ := s.engine.GetTemplate("transcode_batch")
	for _, key := range template.OptionalInputs {
		allowed[key] = true
	}
	for key := range inputs {
		if !allowed[key] && key != "idempotency_key" {
			return plan, nil, fmt.Errorf("preview has unsupported settings; configure a new batch")
		}
	}
	delete(inputs, "idempotency_key")
	inputs["dry_run"] = false
	if _, exists := inputs["paused"]; exists {
		inputs["paused"] = false
	}
	items, err := st.ListTranscodeBatchItems(id)
	if err != nil {
		return plan, nil, err
	}
	plan.Title = operationString(decodeOperationJSON(inst.OutputsJSON)["series_title"])
	if plan.Title == "" {
		plan.Title = "Selected files"
	}
	paths, fileIDs := []string{}, []int{}
	sonarr := inputs["service"] == "sonarr"
	for _, item := range items {
		if item.Status != "queued" || item.Decision != "transcode" {
			plan.Other++
			continue
		}
		paths = append(paths, item.FilePath)
		if sonarr {
			fileID, err := strconv.Atoi(strings.TrimPrefix(item.ItemKey, "epfile-"))
			if err != nil || fileID <= 0 || !strings.HasPrefix(item.ItemKey, "epfile-") {
				return plan, nil, fmt.Errorf("preview selection cannot be restored; configure a new batch")
			}
			fileIDs = append(fileIDs, fileID)
		}
		if len(plan.Files) < 25 {
			label := item.DisplayLabel
			if label == "" {
				label = item.FilePath[strings.LastIndex(item.FilePath, "/")+1:]
			}
			plan.Files = append(plan.Files, label)
		}
	}
	plan.Eligible = len(paths)
	if sonarr {
		inputs["episode_file_ids"] = fileIDs
	} else {
		inputs["paths"] = paths
	}
	add := func(label, value string) { plan.Settings = append(plan.Settings, previewSetting{label, value}) }
	if sonarr {
		source := "Sonarr"
		if season, ok := inputs["season"]; ok {
			source += fmt.Sprintf(" · Season %v", season)
		}
		add("Source", source)
	} else {
		add("Source", "Folder · Frozen file selection")
	}
	profile := operationString(inputs["profile"])
	if inputs["profile_config"] != nil {
		profile = "Custom profile from this preview"
	} else if profile == "" {
		profile = "auto"
	}
	add("Profile", profile)
	for _, field := range []struct{ key, label, suffix string }{
		{"priority", "Quality intent", ""}, {"metric", "Metric", ""},
		{"min_savings_percent", "Minimum savings", "%"}, {"max_size_increase_percent", "Maximum size increase", "%"},
	} {
		if value, ok := inputs[field.key]; ok {
			add(field.label, fmt.Sprint(value)+field.suffix)
		}
	}
	if inputs["preserve_source_bit_depth"] == true {
		add("Bit depth", "Preserve source")
	}
	if inputs["promote_candidates"] == true {
		add("Replacement", "Ask for approval after conversion")
	} else {
		add("Replacement", "Keep originals; create candidates only")
	}
	return plan, inputs, nil
}

func (s *Server) batchPreview(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if r.Method == http.MethodPost {
		var body struct {
			ID string `json:"id"`
		}
		if err := decode(w, r, &body); err != nil {
			fail(w, 400, "invalid preview request")
			return
		}
		id = body.ID
	}
	plan, inputs, err := s.previewPlan(id)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, plan)
		return
	}
	if plan.ExecutionActionID != "" {
		writeJSON(w, 200, map[string]string{"id": plan.ExecutionActionID})
		return
	}
	if plan.Eligible == 0 {
		fail(w, 409, "this preview has no eligible files; nothing was submitted")
		return
	}
	if err := s.engine.CheckWorkerAdmission(r.Context()); err != nil {
		fail(w, 409, err.Error())
		return
	}
	result, err := s.engine.Enqueue(action.WithOrigin(r.Context(), "web"), "transcode_batch", inputs, previewExecutionPrefix+id)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 202, map[string]string{"id": result.ID})
}

// Before the reconciler's inspection creates new items, show the frozen
// selection with its admission status. This is a read-only projection, never
// execution state; even cancelling before inspection must retain the names.
func (s *Server) previewPendingItems(inst store.ActionInstance, items []store.TranscodeBatchItem) ([]store.TranscodeBatchItem, bool, error) {
	return s.pendingBatchItems(inst, items, map[string]bool{})
}

func (s *Server) pendingBatchItems(inst store.ActionInstance, items []store.TranscodeBatchItem, seen map[string]bool) ([]store.TranscodeBatchItem, bool, error) {
	if seen[inst.ID] || len(seen) >= 32 {
		return items, false, nil
	}
	seen[inst.ID] = true
	if len(items) != 0 || inst.CurrentStep != 0 || inst.Status == store.ActionStatusCompleted {
		return items, false, nil
	}
	sourceID := ""
	if strings.HasPrefix(inst.IdempotencyKey, previewExecutionPrefix) {
		sourceID = strings.TrimPrefix(inst.IdempotencyKey, previewExecutionPrefix)
	} else if strings.HasPrefix(inst.IdempotencyKey, "web-reconfigure:") {
		value := strings.TrimPrefix(inst.IdempotencyKey, "web-reconfigure:")
		if index := strings.LastIndex(value, ":"); index > 0 {
			sourceID = value[:index]
		}
	}
	if sourceID == "" {
		return items, false, nil
	}
	source, err := s.engine.Deps().Store.ListTranscodeBatchItems(sourceID)
	if err != nil {
		return nil, false, err
	}
	if len(source) == 0 {
		sourceInst, readErr := s.engine.Deps().Store.GetActionInstanceIfExists(sourceID)
		if readErr != nil {
			return nil, false, readErr
		}
		if sourceInst != nil {
			source, _, err = s.pendingBatchItems(*sourceInst, source, seen)
			if err != nil {
				return nil, false, err
			}
		}
	}
	inputs := decodeOperationJSON(inst.InputsJSON)
	selected := map[string]bool{}
	if ids, ok := inputs["episode_file_ids"].([]any); ok {
		for _, id := range ids {
			selected[fmt.Sprintf("epfile-%v", id)] = true
		}
	} else if paths, ok := inputs["paths"].([]any); ok {
		for _, path := range paths {
			selected[operationString(path)] = true
		}
	}
	for _, item := range source {
		if !selected[item.ItemKey] && !selected[item.FilePath] {
			continue
		}
		item.BatchID, item.Status, item.ChildActionID, item.Error = inst.ID, "queued", "", ""
		if inst.Status == store.ActionStatusCancelled || inst.Status == store.ActionStatusFailed {
			item.Status = inst.Status
		}
		item.Reasons = nil
		items = append(items, item)
	}
	return items, len(items) > 0, nil
}
