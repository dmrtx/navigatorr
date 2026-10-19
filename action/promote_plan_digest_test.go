package action

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSingleFileApprovalRequiresTheReviewedPlanDigest(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan did not wait for approval: %+v", r)
	}
	inst, err := h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	good := PromotionPlanDigest(h.state(t, inst.ID)["promotion"])
	if !strings.HasPrefix(good, "sha256:") {
		t.Fatalf("digest not produced: %q", good)
	}
	for name, ctx := range map[string]context.Context{
		"missing": context.Background(),
		"stale":   WithPromotionPlanDigest(context.Background(), "sha256:0000"),
	} {
		if _, err := h.engine.Resume(ctx, r.ID, "approve", nil); err == nil {
			t.Fatalf("%s digest approved the replacement", name)
		}
	}
	after, _ := h.st.GetActionInstance(r.ID)
	if after.Status != StatusWaitingDecision || h.imports != 0 || h.deletes != 0 {
		t.Fatalf("refused approval changed the job: %s imports=%d deletes=%d", after.Status, h.imports, h.deletes)
	}
	res, err := h.engine.ResumeReviewedPromotion(context.Background(), r.ID, good)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == StatusWaitingDecision || res.Status == StatusFailed {
		t.Fatalf("reviewed approval did not proceed: %+v", res)
	}
}

func TestPlanDigestChangesWithThePlan(t *testing.T) {
	base := &promotionState{SourceActionID: "a", Service: "filesystem", OriginalPath: "/x/o.mp4", CandidatePath: "/x/c.mkv", OriginalSHA: "o", CandidateSHA: "c", OriginalBytes: 10, CandidateBytes: 5}
	same := *base
	if base.planDigest() != same.planDigest() {
		t.Fatal("digest is not deterministic")
	}
	for name, mutate := range map[string]func(*promotionState){
		"candidate sha":  func(p *promotionState) { p.CandidateSHA = "c2" },
		"original sha":   func(p *promotionState) { p.OriginalSHA = "o2" },
		"candidate path": func(p *promotionState) { p.CandidatePath = "/x/d.mkv" },
		"original path":  func(p *promotionState) { p.OriginalPath = "/x/p.mp4" },
		"episodes":       func(p *promotionState) { p.EpisodeIDs = []int{1} },
	} {
		changed := *base
		mutate(&changed)
		if changed.planDigest() == base.planDigest() {
			t.Fatalf("%s did not change the digest", name)
		}
	}
}

func (h *promotionHarness) state(t *testing.T, id string) map[string]any {
	t.Helper()
	inst, err := h.st.GetActionInstance(id)
	if err != nil || inst == nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(inst.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

// The web UI approves through a durable background command, not a direct
// Resume: the digest must survive that hop, and a stale one must still stop.
func TestBackgroundApprovalCarriesTheReviewedPlanDigest(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	good := PromotionPlanDigest(h.state(t, r.ID)["promotion"])
	for name, digest := range map[string]string{"missing": "", "stale": "sha256:0000"} {
		inputs := map[string]any{"kind": "resume", "id": r.ID, "decision": "approve"}
		if digest != "" {
			inputs["plan_digest"] = digest
		}
		cmd, err := h.engine.QueueMaintenanceCommand(context.Background(), inputs, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.engine.ReconcileOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		after, _ := h.st.GetActionInstance(r.ID)
		done, _ := h.st.GetActionInstance(cmd.ID)
		if after.Status != StatusWaitingDecision || h.imports != 0 || done.Status == StatusCompleted {
			t.Fatalf("%s digest: promotion %s, command %s, imports=%d", name, after.Status, done.Status, h.imports)
		}
	}
	cmd, err := h.engine.QueueMaintenanceCommand(context.Background(), map[string]any{"kind": "resume", "id": r.ID, "decision": "approve", "plan_digest": good}, "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := h.st.GetActionInstance(r.ID)
	done, _ := h.st.GetActionInstance(cmd.ID)
	if after.Status == StatusWaitingDecision || after.Status == StatusFailed || done.Status != StatusCompleted {
		t.Fatalf("reviewed digest did not approve: promotion %s, command %s", after.Status, done.Status)
	}
}
