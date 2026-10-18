package action

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func persistedWorkProgress(t *testing.T, h *promotionHarness, id string) map[string]any {
	t.Helper()
	inst, err := h.st.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(inst.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	progress, _ := state["work_progress"].(map[string]any)
	return progress
}

// approvedPreserve returns the execution context of a promotion that is
// approved and about to save its recovery copy.
func approvedPreserve(t *testing.T, h *promotionHarness) *ExecutionContext {
	t.Helper()
	r := h.run()
	inst, err := h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(inst, h.engine)
	p, err := loadPromotion(ec)
	if err != nil {
		t.Fatal(err)
	}
	p.Approved = true
	if err := h.engine.savePromotion(context.Background(), ec, p); err != nil {
		t.Fatal(err)
	}
	return ec
}

func TestPromotionPreserveReportsEachStage(t *testing.T) {
	h := newPromotionHarness(t)
	ec := approvedPreserve(t, h)
	var phases []string
	finished := map[string]bool{}
	observe := func() {
		progress := persistedWorkProgress(t, h, ec.InstanceID)
		phase, _ := progress["phase"].(string)
		if phase == "" {
			return
		}
		if len(phases) == 0 || phases[len(phases)-1] != phase {
			phases = append(phases, phase)
		}
		if total, _ := progress["total_bytes"].(float64); total > 0 && progress["bytes_read"] == total {
			finished[phase] = true
		}
	}
	h.engine.promotionLstatHook = func(path string) (os.FileInfo, error) {
		observe()
		return os.Lstat(path)
	}
	h.engine.promotionCopyBoundaryHook = func(string) { observe() }
	step, err := h.engine.stepPromotePreserve(context.Background(), ec)
	if err != nil || step.Status != StepCompleted {
		t.Fatalf("preserve: %+v %v", step, err)
	}
	want := []string{"verifying_original", "verifying_candidate", "copying_recovery", "verifying_recovery"}
	next := 0
	for _, phase := range phases {
		if next < len(want) && phase == want[next] {
			next++
		}
	}
	if next != len(want) {
		t.Fatalf("persisted phases %v, want %v in order", phases, want)
	}
	for _, phase := range want {
		if !finished[phase] {
			t.Fatalf("%s never reported its full size: %v", phase, phases)
		}
	}
	if _, ok := ec.State["work_progress"]; ok {
		t.Fatalf("finished preserve left current work behind: %+v", ec.State["work_progress"])
	}
	if got := persistedWorkProgress(t, h, ec.InstanceID); got != nil {
		t.Fatalf("recovery checkpoint persisted finished work as current: %+v", got)
	}
}

func TestPromotionPreserveReportsWaitingForHashSlot(t *testing.T) {
	h := newPromotionHarness(t)
	ec := approvedPreserve(t, h)
	for i := 0; i < mediaHashConcurrency; i++ {
		h.engine.hashSlots <- struct{}{}
	}
	done := make(chan StepResult, 1)
	go func() {
		r, err := h.engine.stepPromotePreserve(context.Background(), ec)
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	var waiting map[string]any
	for deadline := time.Now().Add(3 * time.Second); waiting["phase"] != "waiting_for_hash_slot"; waiting = persistedWorkProgress(t, h, ec.InstanceID) {
		if time.Now().After(deadline) {
			t.Fatalf("queued hash not reported as waiting: %+v", waiting)
		}
		time.Sleep(time.Millisecond)
	}
	original, err := os.Stat(h.original)
	if err != nil {
		t.Fatal(err)
	}
	if waiting["total_bytes"] != float64(original.Size()) {
		t.Fatalf("waiting total %v, want %d", waiting["total_bytes"], original.Size())
	}
	if _, ok := waiting["bytes_read"]; ok {
		t.Fatalf("queued hash reported bytes read: %+v", waiting)
	}
	for i := 0; i < mediaHashConcurrency; i++ {
		<-h.engine.hashSlots
	}
	if r := <-done; r.Status != StepCompleted {
		t.Fatalf("preserve: %+v", r)
	}
}

func TestPromotionPreserveFailureClearsWork(t *testing.T) {
	h := newPromotionHarness(t)
	r := h.run()
	// Same size, different bytes: only the candidate's re-hash can tell.
	if err := os.WriteFile(h.candidate, []byte(strings.Repeat("CANDIDATE-", 20)), 0600); err != nil {
		t.Fatal(err)
	}
	r = h.resume(r.ID, "approve")
	if r.Status != StatusFailed || !strings.Contains(r.Error, "SHA-256 changed") {
		t.Fatalf("changed candidate was not rejected: %s %s", r.Status, r.Error)
	}
	inst, err := h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inst.ErrorJSON, "preserve_original") {
		t.Fatalf("fixture error: failed outside preserve: %s", inst.ErrorJSON)
	}
	if got := persistedWorkProgress(t, h, r.ID); got != nil {
		t.Fatalf("failed preserve left work looking current: %+v", got)
	}
}

func TestPromotionPreserveProgressThrottlesWithinAPhase(t *testing.T) {
	ec := &ExecutionContext{State: map[string]any{}}
	w := &promotionPreserveProgress{e: NewEngine(EngineDeps{}), ec: ec, p: &promotionState{OriginalPath: "/library/a.mkv"}}
	read := func() any { return ec.State["work_progress"].(map[string]any)["bytes_read"] }
	steps := []struct {
		phase       string
		read, total int64
		want        any
	}{
		{"copying_recovery", 10, 100, int64(10)},
		{"copying_recovery", 20, 100, int64(10)},   // within a second: not persisted
		{"copying_recovery", 100, 100, int64(100)}, // completion always persists
		{"verifying_recovery", 5, 100, int64(5)},   // a new phase always persists
	}
	for _, s := range steps {
		if err := w.report(context.Background(), s.phase, s.read, s.total); err != nil {
			t.Fatal(err)
		}
		if got := read(); got != s.want {
			t.Fatalf("after %s %d/%d: %+v", s.phase, s.read, s.total, ec.State["work_progress"])
		}
	}
	// Unrelated reads are never reported as one of the step's stages.
	if err := w.hashes(context.Background())("/elsewhere/other.mkv", 50, 100); err != nil || read() != int64(5) {
		t.Fatalf("unrelated read reported: %+v %v", ec.State["work_progress"], err)
	}
}
