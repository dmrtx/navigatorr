package optimization

import (
	"fmt"
	"strings"
	"testing"
)

func hardwareMargin(v float64) *float64 { return &v }

// selectSizeForTest runs a size selection against a 100-byte source under the
// size v3 policy, which needs a result of at most 85 bytes.
func selectSizeForTest(candidates []EvaluatedCandidate, margin *float64) SelectionResult {
	return selectMeasuredCandidates(candidates, "size", 2, 100, ModePolicyV3, margin)
}

func TestHardwareMarginPrefersMediaEngineWithinMargin(t *testing.T) {
	fast, slow := 5.0, 60.0
	x265 := modeCandidate("x265", "libx265", 50, 95, &slow)
	vt := func(bytes int64) EvaluatedCandidate {
		return modeCandidate("vt", "hevc_videotoolbox", bytes, 95, &fast)
	}
	for _, tc := range []struct {
		name       string
		margin     *float64
		candidates []EvaluatedCandidate
		winner     string
		note       bool
	}{
		{"no margin keeps the smallest", nil, []EvaluatedCandidate{x265, vt(55)}, "x265", false},
		{"within the margin hardware wins", hardwareMargin(15), []EvaluatedCandidate{x265, vt(57)}, "vt", true},
		{"exactly at the margin hardware wins", hardwareMargin(20), []EvaluatedCandidate{x265, vt(60)}, "vt", true},
		{"just outside the margin software wins", hardwareMargin(15), []EvaluatedCandidate{x265, vt(58)}, "x265", false},
		// Beyond the selector's 2% equivalence band, which already prefers
		// the faster of two near-equal results.
		{"zero margin and a larger hardware file", hardwareMargin(0), []EvaluatedCandidate{x265, vt(52)}, "x265", false},
		// Inside the 2% band but past the margin: it must not win on speed.
		{"zero margin and a hardware file inside the band", hardwareMargin(0), []EvaluatedCandidate{modeCandidate("x265", "libx265", 80, 95, &slow), modeCandidate("vt", "hevc_videotoolbox", 81, 95, &fast)}, "x265", false},
		{"one percent margin and a hardware file inside the band", hardwareMargin(1), []EvaluatedCandidate{modeCandidate("x265", "libx265", 80, 95, &slow), modeCandidate("vt", "hevc_videotoolbox", 81, 95, &fast)}, "x265", false},
		{"smaller hardware wins at zero margin", hardwareMargin(0), []EvaluatedCandidate{x265, vt(40)}, "vt", true},
		{"hardware alone wins", hardwareMargin(0), []EvaluatedCandidate{vt(80)}, "vt", true},
		{"software alone is unchanged", hardwareMargin(50), []EvaluatedCandidate{x265}, "x265", false},
	} {
		r := selectSizeForTest(tc.candidates, tc.margin)
		if r.Winner == nil || r.Winner.CandidateID != tc.winner {
			t.Fatalf("%s: %+v", tc.name, r.Winner)
		}
		noted := false
		for _, u := range r.Winner.Uncertainties {
			noted = noted || strings.HasPrefix(u, "hardware candidate selected")
		}
		if noted != tc.note {
			t.Fatalf("%s: uncertainties %v", tc.name, r.Winner.Uncertainties)
		}
	}
}

// The smallest qualifying hardware result wins among hardware results, and a
// hardware result that did not qualify never takes part.
func TestHardwareMarginChoosesSmallestQualifyingHardware(t *testing.T) {
	x265 := modeCandidate("x265", "libx265", 50, 95, nil)
	a, b := modeCandidate("vt-q60", "hevc_videotoolbox", 58, 95, nil), modeCandidate("vt-q50", "hevc_videotoolbox", 54, 95, nil)
	failed := modeCandidate("vt-q45", "hevc_videotoolbox", 40, 70, nil)
	failed.MinimumMet, failed.Eligible = false, false
	r := selectSizeForTest([]EvaluatedCandidate{x265, a, b, failed}, hardwareMargin(15))
	if r.Winner == nil || r.Winner.CandidateID != "vt-q50" {
		t.Fatalf("winner %+v", r.Winner)
	}
}

// A hardware result that is too big for the size minimum is not a candidate,
// so a wide margin cannot make it win.
func TestHardwareMarginKeepsTheSizeMinimum(t *testing.T) {
	x265 := modeCandidate("x265", "libx265", 50, 95, nil)
	vt := modeCandidate("vt", "hevc_videotoolbox", 90, 95, nil)
	r := selectSizeForTest([]EvaluatedCandidate{x265, vt}, hardwareMargin(50))
	if r.Winner == nil || r.Winner.CandidateID != "x265" {
		t.Fatalf("winner %+v", r.Winner)
	}
}

// Only size mode has the rule; the other modes ignore the input.
func TestHardwareMarginAppliesToSizeModeOnly(t *testing.T) {
	x265 := modeCandidate("x265", "libx265", 50, 99, nil)
	vt := modeCandidate("vt", "hevc_videotoolbox", 55, 99, nil)
	r := selectMeasuredCandidates([]EvaluatedCandidate{x265, vt}, "quality", 2, 100, "", hardwareMargin(50))
	if r.Winner == nil || r.Winner.CandidateID != "x265" {
		t.Fatalf("quality winner %+v", r.Winner)
	}
}

// The 2% speed-equivalence band runs inside the hardware contest, so a faster
// hardware result just outside the margin must not slip in behind a smaller one
// that is inside it, and the note describes the winner's own size.
func TestHardwareMarginBoundsTheSpeedBand(t *testing.T) {
	fast, slow := 5.0, 40.0
	x265 := modeCandidate("x265", "libx265", 50, 95, &slow)
	for _, tc := range []struct {
		name         string
		margin       float64
		inside, over int64
	}{
		{"fifteen percent", 15, 57, 58},
		{"zero margin", 0, 50, 51},
	} {
		inside := modeCandidate("vt-inside", "hevc_videotoolbox", tc.inside, 95, &slow)
		over := modeCandidate("vt-over", "hevc_videotoolbox", tc.over, 95, &fast)
		r := selectSizeForTest([]EvaluatedCandidate{x265, inside, over}, hardwareMargin(tc.margin))
		if r.Winner == nil || r.Winner.CandidateID != "vt-inside" {
			t.Fatalf("%s: winner %+v", tc.name, r.Winner)
		}
		want := fmt.Sprintf("%d bytes against 50", r.Winner.EstimatedBytes)
		found := false
		for _, u := range r.Winner.Uncertainties {
			found = found || strings.Contains(u, want)
		}
		if !found {
			t.Fatalf("%s: note does not describe the winner: %v", tc.name, r.Winner.Uncertainties)
		}
	}
}
