package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestActionStepSummariesCollapsePollsWithoutReadingPayloads(t *testing.T) {
	s := openTest(t)
	if err := s.CreateActionInstance(ActionInstance{ID: "batch", ActionName: "transcode_batch"}); err != nil {
		t.Fatal(err)
	}
	payload := `{"large":"` + strings.Repeat("x", 8192) + `"}`
	for i := 0; i < 100; i++ {
		status := "waiting_external"
		if i == 99 {
			status = "completed"
		}
		if err := s.LogActionStep(ActionStepLog{InstanceID: "batch", StepIndex: 0, StepName: "encode", Status: status, InputsJSON: payload, OutputsJSON: payload}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.LogActionStep(ActionStepLog{InstanceID: "batch", StepIndex: 1, StepName: "promote", Status: "waiting_decision"}); err != nil {
		t.Fatal(err)
	}
	steps, total, err := s.GetActionStepSummaries("batch", 25)
	if err != nil {
		t.Fatal(err)
	}
	if total != 101 || len(steps) != 2 || steps[0].Status != "completed" || steps[1].Status != "waiting_decision" {
		t.Fatalf("unexpected summaries: %+v total=%d", steps, total)
	}
	for _, step := range steps {
		if step.InputsJSON != "" || step.OutputsJSON != "" {
			t.Fatal("summary read raw audit payloads")
		}
	}
	full, err := s.GetActionSteps("batch")
	if err != nil || len(full) != 101 || full[0].OutputsJSON != payload {
		t.Fatal("compact read changed audit history")
	}
}

func TestContextIncludesDecisionsWithoutRawWorkflowHistory(t *testing.T) {
	s := openTest(t)
	payload := `{"recipe":"` + strings.Repeat("heavy-payload", 8192) + `"}`
	for _, item := range []ActionInstance{
		{ID: "running", ActionName: "transcode_media", Status: ActionStatusWaitingExternal},
		{ID: "decision", ActionName: "transcode_batch", Status: ActionStatusWaitingDecision, WaitingReason: "Approve candidates"},
		{ID: "completed", ActionName: "transcode_media", Status: ActionStatusCompleted},
	} {
		if err := s.CreateActionInstance(item); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.LogActionEnriched("action_completed", "sonarr", "Series", payload, payload, "", "completed", 0); err != nil {
		t.Fatal(err)
	}
	before, err := s.RecentActions(1)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.GetContext("global", "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ActiveActions) != 1 || c.ActiveActions[0].ID != "decision" {
		t.Fatalf("decision omitted: %+v", c.ActiveActions)
	}
	b, err := json.Marshal(c)
	if err != nil || len(b) > 2048 || strings.Contains(string(b), "heavy-payload") {
		t.Fatalf("briefing leaked workflow payload: %d bytes, %v", len(b), err)
	}
	if len(c.RecentLog) != 1 || c.RecentLog[0]["identifiers"] != "completed" {
		t.Fatal("audit detail reference missing")
	}
	full, err := s.RecentActions(1)
	if err != nil || full[0]["result"] != before[0]["result"] {
		t.Fatal("audit payload was not preserved")
	}
}
