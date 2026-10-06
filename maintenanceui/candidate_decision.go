package maintenanceui

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

func rejectedCandidate(child *store.ActionInstance) bool {
	return child != nil && child.Status == store.ActionStatusFailed && strings.Contains(child.ErrorJSON, "transcode candidate rejected by user decision")
}

// Freeze only the reviewed file. Other queued/active/completed batch files are
// excluded, and the previous attempt retains its immutable history.
func (s *Server) candidateReconfigurePlan(id, candidateID string) (batchReconfigurePlan, map[string]any, error) {
	p := batchReconfigurePlan{ID: id, Files: []string{}, Settings: map[string]any{}}
	st := s.engine.Deps().Store
	parent, err := st.GetActionInstance(id)
	if err != nil {
		return p, nil, err
	}
	if parent == nil || parent.ActionName != "transcode_batch" {
		return p, nil, fmt.Errorf("a batch candidate is required")
	}
	inputs := decodeOperationJSON(parent.InputsJSON)
	if inputs["dry_run"] == true {
		return p, nil, fmt.Errorf("preview has no candidate")
	}
	items, err := st.ListTranscodeBatchItems(id)
	if err != nil {
		return p, nil, err
	}
	for _, item := range items {
		if candidateID == "" && item.Status != "waiting_decision" || candidateID != "" && item.ChildActionID != candidateID {
			continue
		}
		child, err := st.GetActionInstance(item.ChildActionID)
		if err != nil {
			return p, nil, err
		}
		if child == nil || child.ActionName != "transcode_media" {
			return p, nil, fmt.Errorf("candidate not found")
		}
		if !rejectedCandidate(child) && (child.Status != store.ActionStatusWaitingDecision || !strings.Contains(child.WaitingOptionsJSON, "accept_loss") || !strings.Contains(child.WaitingOptionsJSON, "reject")) {
			return p, nil, fmt.Errorf("candidate is no longer awaiting validation review")
		}
		childInputs := decodeOperationJSON(child.InputsJSON)
		if childInputs["parent_action_id"] != id || childInputs["path"] != item.FilePath {
			return p, nil, fmt.Errorf("saved candidate does not match its batch file")
		}
		p.CandidateID, p.DecisionVersion = child.ID, action.CandidateDecisionVersion(child)
		p.Selected, p.Kept = 1, len(items)-1
		p.Files = []string{item.DisplayLabel}
		if p.Files[0] == "" {
			p.Files[0] = item.FilePath[strings.LastIndex(item.FilePath, "/")+1:]
		}
		p.Title = p.Files[0]
		delete(inputs, "idempotency_key")
		inputs["dry_run"], inputs["paused"] = false, false
		// The review does not grant replacement approval to the new attempt.
		inputs["promote_candidates"] = false
		delete(inputs, "file_limit")
		if inputs["service"] == "sonarr" {
			fileID, err := strconv.Atoi(strings.TrimPrefix(item.ItemKey, "epfile-"))
			if err != nil || fileID <= 0 || !strings.HasPrefix(item.ItemKey, "epfile-") {
				return p, nil, fmt.Errorf("saved file selection cannot be restored")
			}
			inputs["episode_file_ids"] = []int{fileID}
		} else {
			inputs["paths"] = []string{item.FilePath}
		}
		p.Settings["profile_label"] = "Original profile: " + operationString(inputs["profile"])
		if inputs["profile_config"] != nil {
			p.Settings["profile_label"] = "Original custom profile"
		}
		for _, key := range []string{"priority", "min_savings_percent", "max_size_increase_percent", "preserve_source_bit_depth"} {
			p.Settings[key] = inputs[key]
		}
		return p, inputs, nil
	}
	return p, nil, fmt.Errorf("no candidate is waiting for review")
}

func (s *Server) candidateDecision(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          string `json:"id"`
		CandidateID string `json:"candidate_id"`
		Version     string `json:"decision_version"`
		Decision    string `json:"decision"`
	}
	if decode(w, r, &body) != nil {
		fail(w, 400, "invalid candidate decision")
		return
	}
	if body.Decision != "reject" && body.Decision != "accept_loss" {
		fail(w, 400, "unsupported candidate decision")
		return
	}
	if body.Decision == "accept_loss" {
		if err := s.engine.CheckWorkerAdmission(r.Context()); err != nil {
			fail(w, 409, err.Error())
			return
		}
	}
	if _, err := s.engine.ResumeReviewedCandidate(r.Context(), body.ID, body.CandidateID, body.Version, body.Decision); err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"id": body.ID})
}
