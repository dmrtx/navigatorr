package maintenanceui

import (
	"fmt"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/action"
)

// Uses the stored item relation, never the current Sonarr/folder catalog.
func (s *Server) batchSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode             string   `json:"mode"`
		AllowHardware    *bool    `json:"allow_hardware"`
		HardwareMargin   *float64 `json:"hardware_margin_percent"`
		BudgetPercent    *float64 `json:"size_budget_percent"`
		BudgetBytes      *float64 `json:"size_budget_bytes"`
		BudgetSource     *string  `json:"size_budget_source"`
		BudgetMultiplier *float64 `json:"size_budget_sibling_multiplier"`
		AudioOnly        *bool    `json:"audio_only"`
		MaxHeight        *float64 `json:"max_height"`
		ID               string   `json:"id"`
		Scope            string   `json:"scope"`
		CandidateID      string   `json:"candidate_id"`
		ItemKey          string   `json:"item_key"`
		Version          string   `json:"selection_version"`
		DecisionVersion  string   `json:"decision_version"`
		Profile          string   `json:"profile"`
		MinSavings       *float64 `json:"min_savings_percent"`
		MaxGrowth        *float64 `json:"max_size_increase_percent"`
		PreserveLimits   bool     `json:"preserve_limits"`
		PreserveSavings  bool     `json:"preserve_savings"`
		PreserveGrowth   bool     `json:"preserve_growth"`
		Key              string   `json:"key"`
	}
	if r.Method == http.MethodGet {
		body.ID, body.Scope, body.CandidateID = r.URL.Query().Get("id"), r.URL.Query().Get("scope"), r.URL.Query().Get("candidate_id")
		body.ItemKey = r.URL.Query().Get("item_key")
	} else if decode(w, r, &body) != nil {
		fail(w, 400, "invalid settings request")
		return
	}
	if body.Scope == "" {
		body.Scope = "unfinished"
	}
	st := s.engine.Deps().Store
	inst, err := st.GetActionInstanceIfExists(body.ID)
	if err != nil || inst == nil || inst.ActionName != "transcode_batch" {
		fail(w, 404, "batch not found")
		return
	}
	inputs, state := decodeOperationJSON(inst.InputsJSON), decodeOperationJSON(inst.StateJSON)
	if inputs["dry_run"] == true || state["batch_promotion_plan"] != nil {
		fail(w, 409, "resolve the preview or replacement review before changing settings")
		return
	}
	if body.Scope != "all" && body.Scope != "unfinished" && body.Scope != "candidate" && body.Scope != "file" {
		fail(w, 400, "invalid scope")
		return
	}
	selectionID := body.CandidateID
	if body.Scope == "file" {
		selectionID = body.ItemKey
		if selectionID == "" {
			fail(w, 400, "file selection is required")
			return
		}
	}
	items, err := st.ListTranscodeBatchItems(body.ID)
	if err != nil || len(items) == 0 {
		fail(w, 409, "batch file selection is not ready yet")
		return
	}
	plan := map[string]any{"id": body.ID, "scope": body.Scope, "selection_version": action.BatchSelectionVersion(inst, items), "title": operationString(decodeOperationJSON(inst.OutputsJSON)["series_title"]), "settings": map[string]any{"profile_label": "Keep each file's current profile", "min_savings_percent": inputs["min_savings_percent"], "max_size_increase_percent": inputs["max_size_increase_percent"], "allow_hardware": inputs["allow_hardware"], "hardware_margin_percent": inputs["hardware_margin_percent"]}}
	files := []string{}
	selected, active := 0, 0
	requiresProfile := false
	limits, mixed := map[string]any{}, map[string]bool{}
	itemSettings := operationMap(state["batch_item_settings"])
	var hardware action.HardwareChoice
	var budgets []frozenBudget
	audioOnly := 0
	heights := map[int]bool{}
	profiles := map[string]int{}
	damaged := 0
	for _, item := range items {
		if (body.Scope == "unfinished" || body.Scope == "all") && action.BatchItemSourceDamaged(item) {
			damaged++
		}
		if !action.BatchRevisionSelects(item, body.Scope, selectionID) {
			continue
		}
		reserved, err := s.engine.BatchItemReplacementReserved(item)
		if err != nil {
			fail(w, 500, "read replacement history")
			return
		}
		if reserved {
			continue
		}
		selected++
		if allow, margin, ok := action.BatchItemFrozenHardware(state, item.ItemKey); ok {
			hardware.Add(allow, margin)
		}
		budgets = append(budgets, itemFrozenSizeBudget(state, item.ItemKey))
		if itemFrozenAudioOnly(state, item.ItemKey) {
			audioOnly++
		}
		heights[itemFrozenMaxHeight(state, item.ItemKey)] = true
		if body.Scope == "file" {
			// The file itself, so the goal dialog can preview its streams.
			plan["item_key"], plan["source_path"] = item.ItemKey, item.FilePath
		}
		profile := item.Profile
		if profile == "" {
			profile = operationString(inputs["profile"])
		}
		if override := operationString(operationMap(itemSettings[item.ItemKey])["profile"]); override != "" {
			profile = override
		}
		if profile == "" || profile == "auto" || profile == "batch-generated" {
			profile = "Automatic"
			if inputs["profile_config"] != nil {
				profile = "Custom profile"
			}
		}
		profiles[profile]++
		for _, key := range []string{"min_savings_percent", "max_size_increase_percent"} {
			value := inputs[key]
			if overrides := operationMap(itemSettings[item.ItemKey]); overrides != nil {
				if override, exists := overrides[key]; exists {
					value = override
				}
			}
			if selected == 1 {
				limits[key] = value
			} else if !reflect.DeepEqual(limits[key], value) {
				mixed[key] = true
			}
		}
		for _, reason := range item.Reasons {
			if item.Status == "skip" && strings.Contains(reason, "shared VMAF/CAMBI calibration supports 8-bit SDR video below 45 fps") {
				requiresProfile = true
			}
		}
		if item.ChildActionID != "" {
			child, err := st.GetActionInstance(item.ChildActionID)
			if err != nil {
				fail(w, 500, "read file attempt")
				return
			}
			if child.Status == "running" || child.Status == "pending" || child.Status == "waiting_external" {
				active++
			}
			if body.Scope == "file" && child.Status == "waiting_decision" {
				plan["decision_version"] = action.CandidateDecisionVersion(child)
			}
			if body.Scope == "candidate" {
				plan["candidate_id"], plan["decision_version"] = child.ID, action.CandidateDecisionVersion(child)
			}
		}
		if len(files) < 25 {
			label := item.DisplayLabel
			if label == "" {
				label = item.FilePath[strings.LastIndex(item.FilePath, "/")+1:]
			}
			files = append(files, label)
		}
	}
	plan["selected"], plan["kept"], plan["active"], plan["files"] = selected, len(items)-selected, active, files
	plan["damaged"] = damaged
	plan["requires_explicit_profile"] = requiresProfile
	plan["mode"] = inputs["mode"]
	if goal := operationString(state["batch_goal"]); goal != "" {
		plan["mode"] = goal
	}
	if body.Scope == "file" {
		if selected != 1 {
			fail(w, 409, "this file is no longer available for new settings")
			return
		}
		if goal := operationString(operationMap(itemSettings[body.ItemKey])["mode"]); goal != "" {
			plan["mode"] = goal
		}
	}
	settingsView := plan["settings"].(map[string]any)
	profileNames := make([]string, 0, len(profiles))
	for name := range profiles {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	profileViews := []map[string]any{}
	for _, name := range profileNames {
		profileViews = append(profileViews, map[string]any{"name": name, "files": profiles[name]})
	}
	settingsView["current_profiles"] = profileViews
	if len(profileNames) == 1 {
		settingsView["profile_label"] = "Keep saved: " + profileNames[0]
	}
	for key, value := range limits {
		if mixed[key] {
			value = nil
		}
		settingsView[key] = value
	}
	settingsView["mixed_limits"] = mixed["min_savings_percent"] || mixed["max_size_increase_percent"]
	settingsView["preserve_source_bit_depth"] = inputs["preserve_source_bit_depth"]
	if hardware.Known {
		// Show the media engine choice the files were frozen with, not today's default.
		settingsView["allow_hardware"], settingsView["hardware_margin_percent"] = hardware.Allow, hardware.Margin
		settingsView["hardware_mixed"] = hardware.Mixed
	}
	// Whether the files were frozen with the audio-only goal: all, some or none.
	if selected > 0 {
		settingsView["audio_only"], settingsView["audio_only_mixed"] = audioOnly == selected, audioOnly > 0 && audioOnly < selected
	}
	// The Shrink height the files were frozen with, when they agree on one.
	if len(heights) == 1 {
		for h := range heights {
			if h > 0 {
				settingsView["max_height"] = h
			}
		}
	}
	// The size budget the files were frozen with, when they agree on one.
	if len(budgets) > 0 {
		settingsView["size_budget_mixed"] = false
		for _, b := range budgets[1:] {
			if b != budgets[0] {
				settingsView["size_budget_mixed"] = true
			}
		}
		if settingsView["size_budget_mixed"] == false {
			if budgets[0].percent > 0 {
				settingsView["size_budget_percent"] = budgets[0].percent
			} else if budgets[0].bytes > 0 {
				settingsView["size_budget_bytes"] = budgets[0].bytes
			} else if budgets[0].source != "" {
				settingsView["size_budget_source"], settingsView["size_budget_sibling_multiplier"] = budgets[0].source, budgets[0].multiplier
			}
		}
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, plan)
		return
	}
	if selected == 0 {
		fail(w, 409, "no files selected")
		return
	}
	if body.Mode != "" {
		if operationString(inputs["mode"]) == "" || !containsMode(body.Mode) {
			fail(w, 400, "invalid processing goal")
			return
		}
		settings := map[string]any{"mode": body.Mode}
		if body.AllowHardware != nil {
			settings["allow_hardware"] = *body.AllowHardware
		}
		if body.HardwareMargin != nil {
			settings["hardware_margin_percent"] = *body.HardwareMargin
		}
		if body.BudgetPercent != nil {
			settings["size_budget_percent"] = *body.BudgetPercent
		}
		if body.BudgetBytes != nil {
			settings["size_budget_bytes"] = *body.BudgetBytes
		}
		if body.BudgetSource != nil {
			settings["size_budget_source"] = *body.BudgetSource
		}
		if body.BudgetMultiplier != nil {
			settings["size_budget_sibling_multiplier"] = *body.BudgetMultiplier
		}
		if body.AudioOnly != nil && *body.AudioOnly {
			settings["audio_only"] = true
		}
		if body.MaxHeight != nil {
			settings["max_height"] = *body.MaxHeight
		}
		if err := action.ValidateHardwareInputs(body.Mode, settings); err != nil {
			fail(w, 400, err.Error())
			return
		}
		s.admitCommand(w, r, map[string]any{"kind": "reconfigure", "id": body.ID, "scope": body.Scope, "candidate_id": body.CandidateID, "item_key": body.ItemKey, "selection_version": body.Version, "decision_version": body.DecisionVersion, "settings": settings}, body.Key)
		return
	}
	settings := map[string]any{}
	if requiresProfile && body.Profile == "same" {
		fail(w, 400, "automatic testing skipped this video; choose an explicit profile to try again")
		return
	}
	if body.Profile != "same" {
		if body.Profile == "" || body.Profile == "auto" {
			fail(w, 400, "choose an explicit profile for this attempt")
			return
		}
		if _, err := s.cfg.Transcode.ResolveProfile(body.Profile); err != nil {
			fail(w, 400, "profile not found")
			return
		}
		settings["profile"] = body.Profile
	}
	for _, field := range []struct {
		key   string
		value *float64
		max   float64
	}{{"min_savings_percent", body.MinSavings, 100}, {"max_size_increase_percent", body.MaxGrowth, 1000}} {
		if body.PreserveLimits || field.key == "min_savings_percent" && body.PreserveSavings || field.key == "max_size_increase_percent" && body.PreserveGrowth {
			continue
		}
		settings[field.key] = nil
		if field.value == nil {
			continue
		}
		if math.IsNaN(*field.value) || math.IsInf(*field.value, 0) || *field.value < 0 || *field.value > field.max || field.key == "min_savings_percent" && *field.value == 0 {
			fail(w, 400, fmt.Sprintf("invalid %s", field.key))
			return
		}
		settings[field.key] = *field.value
	}
	s.admitCommand(w, r, map[string]any{"kind": "reconfigure", "id": body.ID, "scope": body.Scope, "candidate_id": body.CandidateID, "item_key": body.ItemKey, "selection_version": body.Version, "decision_version": body.DecisionVersion, "settings": settings}, body.Key)
}

// frozenBudget is the size budget one file's frozen policy carries: zero values
// mean none.
type frozenBudget struct {
	percent, bytes, multiplier float64
	source                     string
}

// itemFrozenSizeBudget reads the budget from the item's own goal-change policy
// when it has one, else the batch's.
func itemFrozenSizeBudget(state map[string]any, itemKey string) frozenBudget {
	holder := state
	if row := operationMap(operationMap(state["batch_item_settings"])[itemKey]); operationMap(row["mode_policy"]) != nil {
		holder = row
	}
	budget := operationMap(operationMap(operationMap(holder["mode_policy"])["size"])["size_budget"])
	b := frozenBudget{percent: operationNumber(budget["percent"]), bytes: operationNumber(budget["bytes"])}
	if siblings := operationMap(budget["siblings"]); siblings != nil {
		b.source, b.multiplier = "siblings", operationNumber(siblings["multiplier"])
	}
	return b
}

// itemFrozenAudioOnly reports whether the item's goal, its own goal-change
// policy when it has one, else the batch's, is the audio-only size goal.
func itemFrozenAudioOnly(state map[string]any, itemKey string) bool {
	holder := state
	if row := operationMap(operationMap(state["batch_item_settings"])[itemKey]); operationMap(row["mode_policy"]) != nil {
		holder = row
	}
	return operationMap(operationMap(holder["mode_policy"])["size"])["audio_only"] == true
}

// itemFrozenMaxHeight is the output height of the item's Shrink goal, its own
// goal-change policy when it has one, else the batch's; 0 under another goal.
func itemFrozenMaxHeight(state map[string]any, itemKey string) int {
	holder := state
	if row := operationMap(operationMap(state["batch_item_settings"])[itemKey]); operationMap(row["mode_policy"]) != nil {
		holder = row
	}
	policy := operationMap(holder["mode_policy"])
	if operationString(policy["mode"]) != "compact" {
		return 0
	}
	return compactMaxHeight(policy)
}

func containsMode(mode string) bool {
	return mode == "size" || mode == "quality" || mode == "x265_preserve" || mode == "compact"
}
