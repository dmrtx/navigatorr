package maintenanceui

import (
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

func seedWaitingCandidate(t *testing.T, s *Server) batchReconfigurePlan {
	seedFailedBatch(t, s, true)
	st := s.engine.Deps().Store
	parent, _ := st.GetActionInstance("failed-batch")
	parent.Status, parent.CurrentStep = store.ActionStatusWaitingDecision, 1
	parent.StateJSON = "{}"
	parent.WaitingOptionsJSON = `[{"decision":"reject"},{"decision":"accept_loss"}]`
	inputs := decodeOperationJSON(parent.InputsJSON)
	inputs["promote_candidates"] = false
	parent.InputsJSON = mustJSON(t, inputs)
	st.UpdateActionInstance(*parent)
	seedOperation(t, st, "review-child", "transcode_media", store.ActionStatusWaitingDecision, map[string]any{"path": "/media/one.mkv", "parent_action_id": "failed-batch"}, nil, map[string]any{})
	child, _ := st.GetActionInstance("review-child")
	child.CurrentStep = 5
	child.WaitingOptionsJSON = parent.WaitingOptionsJSON
	child.WaitingReason = "Candidate too large"
	st.UpdateActionInstance(*child)
	items, _ := st.ListTranscodeBatchItems(parent.ID)
	items[0].Status, items[0].ChildActionID = "waiting_decision", child.ID
	st.UpdateTranscodeBatchItem(items[0])
	plan, _, err := s.candidateReconfigurePlan(parent.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestCandidateReconfigurationRejectsOnlyReviewedFileAndKeepsReceipt(t *testing.T) {
	s, h := testUI(t)
	plan := seedWaitingCandidate(t, s)
	w := request(h, "GET", "/api/maintenance/batch-reconfigure?id=failed-batch&candidate=1", "", true)
	if w.Code != 200 || plan.Selected != 1 || plan.Kept != 2 || plan.CandidateID != "review-child" {
		t.Fatal(w.Code, w.Body.String(), plan)
	}
	body := map[string]any{"id": plan.ID, "candidate_id": plan.CandidateID, "decision_version": plan.DecisionVersion, "profile": "auto", "priority": "savings", "min_savings_percent": 20, "key": "retry-1"}
	w = request(h, "POST", "/api/maintenance/batch-reconfigure", mustJSON(t, body), true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var receipt map[string]string
	json.Unmarshal(w.Body.Bytes(), &receipt)
	st := s.engine.Deps().Store
	child, _ := st.GetActionInstance(plan.CandidateID)
	if !rejectedCandidate(child) {
		t.Fatal("candidate not rejected", child)
	}
	next, _ := st.GetActionInstance(receipt["id"])
	inputs := decodeOperationJSON(next.InputsJSON)
	if mustJSON(t, map[string]any{"ids": inputs["episode_file_ids"]}) != `{"ids":[101]}` || inputs["promote_candidates"] != false || inputs["priority"] != "savings" {
		t.Fatal(inputs)
	}
	w = request(h, "POST", "/api/maintenance/batch-reconfigure", mustJSON(t, body), true)
	if w.Code != 200 {
		t.Fatal("lost reply queued twice", w.Code, w.Body.String())
	}
	body["key"], body["min_savings_percent"] = "retry-2", 30
	w = request(h, "POST", "/api/maintenance/batch-reconfigure", mustJSON(t, body), true)
	if w.Code != 409 {
		t.Fatal("candidate submitted with another configuration twice", w.Code)
	}
	items, _ := st.ListTranscodeBatchItems(plan.ID)
	if items[1].Status != "completed" || items[2].Status != "cancelled" {
		t.Fatal("other files changed", items)
	}
}

func TestCandidateDecisionStaleReviewAndOfflineRetryDoNotReject(t *testing.T) {
	for _, scenario := range []string{"changed", "offline", "bad-profile", "auth"} {
		t.Run(scenario, func(t *testing.T) {
			s, h := testUI(t)
			plan := seedWaitingCandidate(t, s)
			body := map[string]any{"id": plan.ID, "candidate_id": plan.CandidateID, "decision_version": plan.DecisionVersion, "profile": "auto", "priority": "savings", "key": "retry-1"}
			st := s.engine.Deps().Store
			switch scenario {
			case "changed":
				child, _ := st.GetActionInstance(plan.CandidateID)
				child.WaitingReason = "new validation"
				st.UpdateActionInstance(*child)
			case "offline":
				deps := s.engine.Deps()
				deps.Transcode = unavailableWorker{}
				s.engine = action.NewEngine(deps)
			case "bad-profile":
				body["profile"] = "missing"
			}
			w := request(h, "POST", "/api/maintenance/batch-reconfigure", mustJSON(t, body), scenario != "auth")
			if w.Code < 400 {
				t.Fatal(w.Code, w.Body.String())
			}
			child, _ := st.GetActionInstance(plan.CandidateID)
			if child.Status != store.ActionStatusWaitingDecision {
				t.Fatal("rejected despite failed admission", child)
			}
		})
	}
}
