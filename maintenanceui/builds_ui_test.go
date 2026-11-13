package maintenanceui

import (
	"os"
	"strings"
	"testing"
)

// The workers dialog shows the Builds view and the node card's busy line, a
// build hold leaves the operator's pause buttons available, and the new
// controls follow the owner's UI rules (no explainer under controls, no
// loose button, no Apply row).
func TestBuildsViewIsWiredIntoTheWorkersDialog(t *testing.T) {
	read := func(name string) string {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	js, html, css := read("assets/app.js"), read("assets/index.html"), read("assets/app.css")
	for _, want := range []string{
		`api("builds?limit=100")`,
		`renderBuilds($("builds-list")`,
		`nodeBuildLines(worker.name, worker.pause?.builds)`,
		`worker.pause.held_by === "builds"`,
		`const operatorPaused = pause.paused && pause.held_by !== "builds"`,
		`"Target", "Source", "Holder", "Kind", "Title", "Priority", "Status", "Duration", "Last heartbeat", "Progress"`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js is missing %s", want)
		}
	}
	if !strings.Contains(js, "if (!operatorPaused) {") || strings.Contains(js, "if (!pause.paused) {\n    add(") {
		t.Fatal("a build hold must not turn the operator's buttons into Resume")
	}
	if !strings.Contains(html, `id="builds-list"`) || strings.Contains(html, "builds-apply") {
		t.Fatal("the dialog needs the builds list and no Apply row")
	}
	for _, status := range []string{"running", "waiting", "failed", "lost", "succeeded", "cancelled"} {
		if !strings.Contains(css, `.build-status[data-status="`+status+`"]`) {
			t.Fatalf("no state color for %s", status)
		}
	}
	// The builds block makes no buttons and no explanation paragraphs.
	block := js[strings.Index(js, "// builds:start"):strings.Index(js, "// builds:end")]
	if strings.Contains(block, `node("button"`) || strings.Contains(block, "button(") {
		t.Fatal("the Builds view has no loose buttons")
	}
}
