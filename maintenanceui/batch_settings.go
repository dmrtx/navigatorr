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
		ID              string   `json:"id"`
		Scope           string   `json:"scope"`
		CandidateID     string   `json:"candidate_id"`
		Version         string   `json:"selection_version"`
		DecisionVersion string   `json:"decision_version"`
		Profile         string   `json:"profile"`
		MinSavings      *float64 `json:"min_savings_percent"`
		MaxGrowth       *float64 `json:"max_size_increase_percent"`
		PreserveLimits  bool     `json:"preserve_limits"`
		PreserveSavings bool     `json:"preserve_savings"`
		PreserveGrowth  bool     `json:"preserve_growth"`
		Key             string   `json:"key"`
	}
	if r.Method == http.MethodGet {
		body.ID, body.Scope, body.CandidateID = r.URL.Query().Get("id"), r.URL.Query().Get("scope"), r.URL.Query().Get("candidate_id")
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
	if body.Scope != "all" && body.Scope != "unfinished" && body.Scope != "candidate" {
		fail(w, 400, "invalid scope")
		return
	}
	items, err := st.ListTranscodeBatchItems(body.ID)
	if err != nil || len(items) == 0 {
		fail(w, 409, "batch file selection is not ready yet")
		return
	}
	plan := map[string]any{"id": body.ID, "scope": body.Scope, "selection_version": action.BatchSelectionVersion(inst, items), "title": operationString(decodeOperationJSON(inst.OutputsJSON)["series_title"]), "settings": map[string]any{"profile_label": "Keep each file's current profile", "min_savings_percent": inputs["min_savings_percent"], "max_size_increase_percent": inputs["max_size_increase_percent"]}}
	files := []string{}
	selected, active := 0, 0
	requiresProfile := false
	limits, mixed := map[string]any{}, map[string]bool{}
	itemSettings := operationMap(state["batch_item_settings"])
	profiles := map[string]int{}
	for _, item := range items {
		if !action.BatchRevisionSelects(item, body.Scope, body.CandidateID) {
			continue
		}
		selected++
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
	plan["requires_explicit_profile"] = requiresProfile
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
	if r.Method == http.MethodGet {
		writeJSON(w, 200, plan)
		return
	}
	if selected == 0 {
		fail(w, 409, "no files selected")
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
	s.admitCommand(w, r, map[string]any{"kind": "reconfigure", "id": body.ID, "scope": body.Scope, "candidate_id": body.CandidateID, "selection_version": body.Version, "decision_version": body.DecisionVersion, "settings": settings}, body.Key)
}
