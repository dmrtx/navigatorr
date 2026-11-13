package maintenanceui

import (
	"os"
	"strings"
	"testing"
)

// nodePauseControlsSource is the workers dialog's pause control, from its
// declaration to the next function.
func nodePauseControlsSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	start := strings.Index(src, "function nodePauseControls(")
	end := strings.Index(src, "function setNodePause(")
	if start < 0 || end < start {
		t.Fatal("nodePauseControls not found")
	}
	return src[start:end]
}

// The pause control offers Suspend now beside the two older buttons, with
// every button in the one actions row (no loose button) and no explanatory
// paragraph under the controls: the only text the control shows is the node's
// own status message, above them.
func TestNodePauseControlsOfferSuspendInTheOneActionsRow(t *testing.T) {
	src := nodePauseControlsSource(t)
	for _, want := range []string{`add("Pause transcode after current files", "graceful"`, `add("Pause transcode now", "immediate"`, `add("Suspend transcode now", "suspend"`, `add("Resume transcode", "off"`} {
		if !strings.Contains(src, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if strings.Count(src, "buttons.append(button)") != 1 || strings.Contains(src, "box.append(button)") {
		t.Fatal("every pause button belongs to the one actions row")
	}
	// The status message comes before the buttons; nothing explains them after.
	afterButtons := src[strings.Index(src, "box.append(buttons)"):]
	if strings.Contains(afterButtons, `node("p"`) {
		t.Fatal("no explainer text under the controls")
	}
	// Suspending is reversible, so it never asks to be tapped twice.
	if !strings.Contains(src, `mode !== "immediate" || button.dataset.armed`) {
		t.Fatal("only sending files back needs the second tap")
	}
}

// Confirming the request says only that it was requested: a suspend promises
// neither a time nor that the files are frozen, which a failed signal can
// still prevent. The workers dialog shows the freeze once the worker reports it.
func TestSuspendNoticePromisesNeitherATimeNorAFreeze(t *testing.T) {
	data, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	start := strings.Index(src, "function setNodePause(")
	if start < 0 {
		t.Fatal("setNodePause not found")
	}
	end := strings.Index(src[start:], "\n}\n")
	body := src[start : start+end]
	if !strings.Contains(body, "transcode suspend requested.`") {
		t.Fatal("the suspend notice must say the suspend was requested")
	}
	for _, banned := range []string{"within a second", "are frozen", "frozen and"} {
		if strings.Contains(body, banned) {
			t.Fatalf("the suspend notice must not promise %q", banned)
		}
	}
}
