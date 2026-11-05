package action

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesystemPromotionNewPathKeepsOriginalExtensionSpelling(t *testing.T) {
	cases := []struct{ original, candidate, want string }{
		{"/m/T8CLRZ~D.MKV", "/m/.navigatorr-candidates/x.job-1.mkv", "/m/T8CLRZ~D.MKV"},
		{"/m/Show S01E01.Mkv", "/m/.navigatorr-candidates/x.mkv", "/m/Show S01E01.Mkv"},
		{"/m/Show.mkv", "/m/.navigatorr-candidates/x.mkv", "/m/Show.mkv"},
		{"/m/Show.mp4", "/m/.navigatorr-candidates/x.mkv", "/m/Show.mkv"},
		{"/m/Show.AVI", "/m/.navigatorr-candidates/x.mkv", "/m/Show.mkv"},
	}
	for _, c := range cases {
		if got := filesystemPromotionNewPath(c.original, c.candidate); got != c.want {
			t.Errorf("filesystemPromotionNewPath(%q, %q) = %q, want %q", c.original, c.candidate, got, c.want)
		}
	}
}

// A ".MKV" original and a ".mkv" candidate used to fail in plan_promotion as
// "destination already exists": on a case-insensitive share the lower-case
// name is the original itself. The replacement is an in-place one.
func TestFilesystemPromotionUppercaseExtensionReplacesInPlace(t *testing.T) {
	h := newPromotionHarness(t)
	old := h.original
	h.original = strings.TrimSuffix(old, filepath.Ext(old)) + ".MKV"
	if err := os.Rename(old, h.original); err != nil {
		t.Fatal(err)
	}
	h.final = h.original
	inst, err := h.st.GetActionInstance("source-transcode")
	if err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(inst, h.engine)
	ec.State["resolved_path"] = h.original
	inst.StateJSON = toJSON(ec.State)
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	r := runLocalPromotion(t, h)
	if r.Status != StatusWaitingDecision {
		t.Fatalf("plan: %+v", r)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	if p.NewPath != h.original {
		t.Fatalf("replacement is not in place: new=%q original=%q", p.NewPath, h.original)
	}
	r = h.resume(r.ID, "approve")
	if r.Status != StatusCompleted {
		t.Fatalf("uppercase-extension replacement: %+v", r)
	}
	got, err := os.ReadFile(h.original)
	if err != nil || !strings.HasPrefix(string(got), "candidate-") {
		t.Fatalf("original path does not hold the candidate: %q %v", got, err)
	}
}

func TestPromotionDestinationFreeNamesTheCause(t *testing.T) {
	dir := t.TempDir()
	if err := promotionDestinationFree(filepath.Join(dir, "free.mkv")); err != nil {
		t.Fatalf("free destination refused: %v", err)
	}
	taken := filepath.Join(dir, "taken.mkv")
	if err := os.WriteFile(taken, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := promotionDestinationFree(taken); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("occupied destination: %v", err)
	}
	// A path below a regular file cannot be inspected: not "exists".
	err := promotionDestinationFree(filepath.Join(taken, "child.mkv"))
	if err == nil || !strings.Contains(err.Error(), "cannot be inspected") || strings.Contains(err.Error(), "already exists") {
		t.Fatalf("uninspectable destination: %v", err)
	}
}
