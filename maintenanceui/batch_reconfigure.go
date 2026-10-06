package maintenanceui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

type batchReconfigurePlan struct {
	ID       string         `json:"id"`
	Title    string         `json:"title"`
	Selected int            `json:"selected"`
	Kept     int            `json:"kept"`
	Files    []string       `json:"files"`
	Settings map[string]any `json:"settings"`
}

// Restore the frozen failed/cancelled subset, never expand it from a live catalog.
// Original configuration remains server-side, including custom profile fields.
func (s *Server) reconfigurePlan(id string) (batchReconfigurePlan, map[string]any, error) {
	p := batchReconfigurePlan{ID: id, Files: []string{}, Settings: map[string]any{}}
	st := s.engine.Deps().Store
	if st == nil {
		return p, nil, fmt.Errorf("maintenance store is required")
	}
	inst, err := st.GetActionInstanceIfExists(id)
	if err != nil {
		return p, nil, err
	}
	if inst == nil || inst.ActionName != "transcode_batch" || (inst.Status != store.ActionStatusCompleted && inst.Status != store.ActionStatusFailed && inst.Status != store.ActionStatusCancelled) {
		return p, nil, fmt.Errorf("only a finished batch can be reconfigured")
	}
	inputs := decodeOperationJSON(inst.InputsJSON)
	if inputs["dry_run"] == true {
		return p, nil, fmt.Errorf("start a preview from its review instead")
	}
	tmpl, _ := s.engine.GetTemplate("transcode_batch")
	allowed := map[string]bool{}
	for _, key := range tmpl.OptionalInputs {
		allowed[key] = true
	}
	for key := range inputs {
		if !allowed[key] && key != "idempotency_key" {
			return p, nil, fmt.Errorf("this batch has unsupported settings; configure a new batch")
		}
	}
	delete(inputs, "idempotency_key")
	inputs["dry_run"], inputs["paused"] = false, false
	items, err := st.ListTranscodeBatchItems(id)
	if err != nil {
		return p, nil, err
	}
	items, _, err = s.previewPendingItems(*inst, items)
	if err != nil {
		return p, nil, err
	}
	paths, ids := []string{}, []int{}
	for _, item := range items {
		if item.Status != "failed" && item.Status != "cancelled" {
			p.Kept++
			continue
		}
		if item.ChildActionID != "" {
			child, err := st.GetActionInstanceIfExists(item.ChildActionID)
			if err != nil {
				return p, nil, err
			}
			if child != nil && child.Status != store.ActionStatusCompleted && child.Status != store.ActionStatusFailed && child.Status != store.ActionStatusCancelled {
				return p, nil, fmt.Errorf("a selected file still has active work; wait for it to stop")
			}
		}
		paths = append(paths, item.FilePath)
		if inputs["service"] == "sonarr" {
			fileID, err := strconv.Atoi(strings.TrimPrefix(item.ItemKey, "epfile-"))
			if err != nil || fileID <= 0 || !strings.HasPrefix(item.ItemKey, "epfile-") {
				return p, nil, fmt.Errorf("saved file selection cannot be restored")
			}
			ids = append(ids, fileID)
		}
		if len(p.Files) < 25 {
			label := item.DisplayLabel
			if label == "" {
				label = item.FilePath[strings.LastIndex(item.FilePath, "/")+1:]
			}
			p.Files = append(p.Files, label)
		}
	}
	p.Selected = len(paths)
	if p.Selected == 0 {
		return p, nil, fmt.Errorf("this batch has no failed or cancelled files to reconfigure")
	}
	if inputs["service"] == "sonarr" {
		inputs["episode_file_ids"] = ids
	} else {
		inputs["paths"] = paths
	}
	p.Title = operationString(decodeOperationJSON(inst.OutputsJSON)["series_title"])
	if p.Title == "" {
		p.Title = "Selected files"
	}
	p.Settings["profile_label"] = "Original profile: " + operationString(inputs["profile"])
	if inputs["profile_config"] != nil {
		p.Settings["profile_label"] = "Original custom profile"
	} else if operationString(inputs["profile"]) == "" || inputs["profile"] == "auto" {
		p.Settings["profile_label"] = "Original automatic selection"
	}
	for _, key := range []string{"priority", "min_savings_percent", "max_size_increase_percent", "preserve_source_bit_depth", "promote_candidates"} {
		if value, ok := inputs[key]; ok {
			p.Settings[key] = value
		}
	}
	return p, inputs, nil
}

var reconfigureKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

func (s *Server) batchReconfigure(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID         string   `json:"id"`
		Profile    string   `json:"profile"`
		Priority   string   `json:"priority"`
		MinSavings *float64 `json:"min_savings_percent"`
		MaxGrowth  *float64 `json:"max_size_increase_percent"`
		Key        string   `json:"key"`
	}
	id := r.URL.Query().Get("id")
	if r.Method == http.MethodPost {
		if decode(w, r, &body) != nil {
			fail(w, 400, "invalid reconfiguration request")
			return
		}
		id = body.ID
	}
	p, inputs, err := s.reconfigurePlan(id)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, p)
		return
	}
	if !reconfigureKey.MatchString(body.Key) {
		fail(w, 400, "a valid submission key is required")
		return
	}
	if body.Profile != "same" {
		if strings.TrimSpace(body.Profile) == "" {
			fail(w, 400, "choose a profile")
			return
		}
		delete(inputs, "profile_config")
		delete(inputs, "priority")
		inputs["profile"] = body.Profile
		if body.Profile == "auto" {
			if body.Priority != "balanced" && body.Priority != "quality" && body.Priority != "savings" && body.Priority != "preserve_quality" {
				fail(w, 400, "choose a quality intent")
				return
			}
			inputs["priority"] = body.Priority
		} else if _, err := s.cfg.Transcode.ResolveProfile(body.Profile); err != nil {
			fail(w, 400, "profile not found")
			return
		}
	}
	for _, field := range []struct {
		key   string
		value *float64
		max   float64
	}{{"min_savings_percent", body.MinSavings, 100}, {"max_size_increase_percent", body.MaxGrowth, 1000}} {
		if field.value == nil {
			delete(inputs, field.key)
			continue
		}
		if math.IsNaN(*field.value) || math.IsInf(*field.value, 0) || *field.value < 0 || *field.value > field.max {
			fail(w, 400, "invalid savings or growth limit")
			return
		}
		// The engine uses its default when minimum savings is zero.
		if field.key == "min_savings_percent" && *field.value == 0 {
			fail(w, 400, "minimum savings must be greater than zero, or blank for the default")
			return
		}
		inputs[field.key] = *field.value
	}
	key := "web-reconfigure:" + id + ":" + body.Key
	if len(key) > 200 {
		fail(w, 400, "invalid batch identifier")
		return
	}
	st := s.engine.Deps().Store
	if old, err := st.FindActionByIdempotencyKey("transcode_batch", key); err != nil {
		fail(w, 500, "read submission")
		return
	} else if old != nil {
		before, _ := json.Marshal(decodeOperationJSON(old.InputsJSON))
		after, _ := json.Marshal(inputs)
		if !bytes.Equal(before, after) {
			fail(w, 409, "submission key belongs to different settings")
			return
		}
		writeJSON(w, 200, map[string]string{"id": old.ID})
		return
	}
	if err := s.engine.CheckWorkerAdmission(r.Context()); err != nil {
		fail(w, 409, err.Error())
		return
	}
	result, err := s.engine.Enqueue(action.WithOrigin(r.Context(), "web"), "transcode_batch", inputs, key)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 202, map[string]string{"id": result.ID})
}
