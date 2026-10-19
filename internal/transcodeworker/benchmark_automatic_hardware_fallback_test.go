package transcodeworker

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

// A hard episode: the media engine ladder tops out at q60 and fails quality
// there, so q45-55 are skipped as dominated and nothing above q60 exists. The
// higher-quality rungs (q65/70/75) are a fallback family measured beside the
// first x265 quality fallback stage, before the slow x265 stage.

func hardwareFallbackLadder() []validatedCandidate {
	vcs := hardwareReplayLadder(true)
	gop, bframes := 240, 1
	for _, q := range []int{65, 70, 75} {
		vcs = append(vcs, validatedCandidate{index: len(vcs), candidate: transcode.BenchmarkCandidate{ID: fmt.Sprintf("%scand_q%d", fallbackHardwarePrefix, q), VideoCodec: "hevc_videotoolbox", Quality: q, VideoProfile: "main10", PixelFormat: "p010le", GOPSize: &gop, BFrames: &bframes}, bitDepth: 10, profile: "main10", pixelFormat: "p010le"})
	}
	return vcs
}

func runHardwareFallbackReplay(t *testing.T, settings map[string]replaySetting, marginPercent float64) replayResult {
	t.Helper()
	hw := &transcode.HardwareSearchRules{MarginPercent: marginPercent, QualityValues: []int{45, 50, 55, 60}, FallbackQualityValues: []int{65, 70, 75}, GOPSize: 240, BFrames: 1}
	return runReplayHardware(t, settings, 10_000_000, replayRules(), hw, hardwareFallbackLadder())
}

var (
	replayFails  = replaySetting{bytes: 20000, discovery: replayScore{78, 85}, holdout: 80}
	replayPasses = func(bytes int64) replaySetting {
		return replaySetting{bytes: bytes, discovery: replayScore{89.4, 91.2}, holdout: 90}
	}
)

// hardEpisode is a batch where no initial setting of either encoder passes
// quality, the slow x265 fallback is the only thing that always would, and the
// caller overrides the fallback rungs.
func hardEpisode() map[string]replaySetting {
	settings := map[string]replaySetting{}
	for _, crf := range []int{22, 24, 26, 28, 30, 16, 18, 20} {
		settings[replayKey("medium", crf)] = replayFails
	}
	for _, q := range []int{45, 50, 55, 60, 65, 70, 75} {
		settings[replayKey("", q)] = replayFails
	}
	for _, crf := range []int{16, 18, 20} {
		settings[replayKey("slow", crf)] = replayPasses(22000)
	}
	return settings
}

func measuredHas(res replayResult, id string) bool {
	for _, m := range res.measured {
		if m == id {
			return true
		}
	}
	return false
}

// The ladder fails, a higher rung passes within the margin of the medium x265
// result: it wins and is the only candidate verified, and the slow stage is
// never paid for.
func TestReplayHardwareFallbackWinsWithinMargin(t *testing.T) {
	settings := hardEpisode()
	settings[replayKey("medium", 16)] = replayPasses(23500)
	// 26000 is 10.6% above 23500; q70 is 19% above, q75 28% above.
	settings[replayKey("", 65)] = replayPasses(26000)
	settings[replayKey("", 70)] = replayPasses(28000)
	settings[replayKey("", 75)] = replayPasses(30000)
	res := runHardwareFallbackReplay(t, settings, 15)
	t.Logf("hardware fallback within margin: %s measured=%v", res.summary(), res.measured)
	if res.winner != "fallback-hardware-cand_q65" || strings.Join(res.verified, ",") != "fallback-hardware-cand_q65" {
		t.Fatalf("a higher-quality media engine rung within the margin did not win: %s", res.summary())
	}
	if res.slowEncodes != 0 {
		t.Fatalf("the slow x265 stage ran although a fallback result qualified: %s", res.summary())
	}
	// The ladder's top rung failed, so its siblings were never measured.
	for _, id := range []string{"vt-cand_q60"} {
		if !measuredHas(res, id) {
			t.Fatalf("%s not measured: %v", id, res.measured)
		}
	}
	for _, id := range []string{"vt-cand_q45", "vt-cand_q50", "vt-cand_q55"} {
		if measuredHas(res, id) || res.reason(id) != reasonSkippedQualityDominated {
			t.Fatalf("%s measured or labelled %q", id, res.reason(id))
		}
	}
	// The medium rung and the hardware rungs were measured in one stage.
	if !measuredHas(res, "fallback-quality-medium-cand_crf16") || !measuredHas(res, "fallback-hardware-cand_q75") {
		t.Fatalf("the first fallback stage did not carry both families: %v", res.measured)
	}
	noted := false
	for _, u := range res.decision.Winner.Uncertainties {
		noted = noted || strings.HasPrefix(u, optimization.HardwareSelectedNote) && strings.Contains(u, "within the 15% margin")
	}
	if !noted {
		t.Fatalf("hardware reason lost from the verified winner: %v", res.decision.Winner.Uncertainties)
	}
}

// The margin still decides: the passing rung is past it, so x265 medium wins
// and the media engine result is not even verified.
func TestReplayHardwareFallbackOutsideMarginLoses(t *testing.T) {
	settings := hardEpisode()
	settings[replayKey("medium", 16)] = replayPasses(23500)
	settings[replayKey("", 65)] = replayPasses(29000) // 23% above
	settings[replayKey("", 70)] = replayPasses(31000)
	settings[replayKey("", 75)] = replayPasses(33000)
	res := runHardwareFallbackReplay(t, settings, 15)
	t.Logf("hardware fallback outside margin: %s measured=%v", res.summary(), res.measured)
	if res.winner != "fallback-quality-medium-cand_crf16" || strings.Join(res.verified, ",") != "fallback-quality-medium-cand_crf16" {
		t.Fatalf("a media engine result past the margin beat x265: %s", res.summary())
	}
	if res.slowEncodes != 0 {
		t.Fatalf("the slow stage ran: %s", res.summary())
	}
	// A wide enough margin takes the same rung.
	res = runHardwareFallbackReplay(t, settings, 30)
	if res.winner != "fallback-hardware-cand_q65" {
		t.Fatalf("a 30%% margin did not take the media engine result: %s", res.summary())
	}
}

// The fallback rungs fail too: the family is spent (q75 failing skips q65/q70)
// and the search proceeds to the slow x265 stage as before.
func TestReplayHardwareFallbackFailsToo(t *testing.T) {
	settings := hardEpisode()
	res := runHardwareFallbackReplay(t, settings, 15)
	t.Logf("hardware fallback fails too: %s measured=%v", res.summary(), res.measured)
	if res.winner != "fallback-quality-cand_crf16" || res.slowEncodes == 0 {
		t.Fatalf("the slow x265 stage did not rescue the search: %s", res.summary())
	}
	if !measuredHas(res, "fallback-hardware-cand_q75") {
		t.Fatalf("the best fallback rung was not measured: %v", res.measured)
	}
	for _, id := range []string{"fallback-hardware-cand_q65", "fallback-hardware-cand_q70"} {
		if measuredHas(res, id) || res.reason(id) != reasonSkippedQualityDominated {
			t.Fatalf("%s measured or labelled %q", id, res.reason(id))
		}
	}
	if got := res.reason("fallback-hardware-cand_q75"); !qualityRejection(got) {
		t.Fatalf("fallback-hardware-cand_q75 labelled %q", got)
	}
}

// No media engine result for x265 to be compared with: when medium x265 finds
// nothing and a fallback rung qualifies, it wins instead of the slow stage.
func TestReplayHardwareFallbackWinsWhenMediumFindsNothing(t *testing.T) {
	settings := hardEpisode()
	settings[replayKey("", 70)] = replayPasses(28000)
	settings[replayKey("", 75)] = replayPasses(30000)
	res := runHardwareFallbackReplay(t, settings, 15)
	t.Logf("medium finds nothing: %s", res.summary())
	if res.winner != "fallback-hardware-cand_q70" || res.slowEncodes != 0 {
		t.Fatalf("unexpected result: %s", res.summary())
	}
}

// A ladder rung that passes quality means the fallback is not needed, whatever
// x265 does.
func TestReplayHardwareFallbackNotMeasuredWhenLadderPasses(t *testing.T) {
	settings := hardEpisode()
	settings[replayKey("", 60)] = replayPasses(26000)
	settings[replayKey("", 55)] = replayPasses(24000)
	settings[replayKey("", 50)] = replayFails
	res := runHardwareFallbackReplay(t, settings, 15)
	t.Logf("ladder passes: %s measured=%v", res.summary(), res.measured)
	if res.winner != "vt-cand_q55" {
		t.Fatalf("unexpected result: %s", res.summary())
	}
	for _, id := range res.measured {
		if strings.HasPrefix(id, fallbackHardwarePrefix) {
			t.Fatalf("fallback measured although the ladder passed: %v", res.measured)
		}
	}
}

// A ladder rung that passes discovery and fails its holdout on quality leaves
// nothing above it: the higher rungs are then measured and the retry verifies
// the safest of them still within the margin (the existing rule for a retry
// after a quality failure).
func TestReplayHardwareFallbackAfterLadderHoldoutFailure(t *testing.T) {
	settings := hardEpisode()
	settings[replayKey("", 60)] = replaySetting{bytes: 26000, discovery: replayScore{89.4, 91.2}, holdout: 80}
	settings[replayKey("", 65)] = replayPasses(27000)
	settings[replayKey("", 70)] = replayPasses(29000)
	settings[replayKey("", 75)] = replayPasses(31000)
	settings[replayKey("medium", 16)] = replayPasses(25000)
	res := runHardwareFallbackReplay(t, settings, 30)
	t.Logf("ladder fails its holdout: %s measured=%v", res.summary(), res.measured)
	if strings.Join(res.verified, ",") != "vt-cand_q60,fallback-hardware-cand_q75" || res.winner != "fallback-hardware-cand_q75" {
		t.Fatalf("the retry did not reach a higher rung: %s", res.summary())
	}
	if res.slowEncodes != 0 {
		t.Fatalf("the slow stage ran: %s", res.summary())
	}
}

// Without the higher-quality values in the rules, or without the candidates,
// the search is the one that shipped: x265 fallbacks only.
func TestReplayHardwareWithoutFallbackRungsKeepsX265Fallback(t *testing.T) {
	settings := hardEpisode()
	settings[replayKey("medium", 16)] = replayPasses(23500)
	hw := &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, GOPSize: 240, BFrames: 1}
	res := runReplayHardware(t, settings, 10_000_000, replayRules(), hw, hardwareReplayLadder(true))
	if res.winner != "fallback-quality-medium-cand_crf16" {
		t.Fatalf("unexpected result: %s", res.summary())
	}
	for _, id := range res.measured {
		if strings.HasPrefix(id, fallbackHardwarePrefix) {
			t.Fatalf("hardware fallback measured: %v", res.measured)
		}
	}
}

func TestHardwareQualityFallbackRequiresAFailedLadder(t *testing.T) {
	gop, bframes := 240, 1
	vt := func(id string, q int) validatedCandidate {
		return validatedCandidate{candidate: transcode.BenchmarkCandidate{ID: id, VideoCodec: "hevc_videotoolbox", Quality: q, GOPSize: &gop, BFrames: &bframes}, bitDepth: 10, profile: "main10", pixelFormat: "p010le"}
	}
	x265 := validatedCandidate{candidate: transcode.BenchmarkCandidate{ID: "x", VideoCodec: "libx265", Quality: 22, Preset: "medium"}, bitDepth: 10}
	initial := []validatedCandidate{x265, vt("vt-cand_q55", 55), vt("vt-cand_q60", 60)}
	all := append(append([]validatedCandidate(nil), initial...), vt(fallbackHardwarePrefix+"cand_q65", 65), vt(fallbackHardwarePrefix+"cand_q70", 70))
	for _, tc := range []struct {
		name     string
		failed   map[string]bool
		failures []validatedCandidate
		want     int
	}{
		{"every rung failed quality", map[string]bool{"vt-cand_q55": true, "vt-cand_q60": true}, nil, 2},
		{"the best rung failed its holdout, the rest are ruled out", nil, []validatedCandidate{initial[2]}, 2},
		{"one rung did not fail on quality", map[string]bool{"vt-cand_q60": true}, nil, 0},
		{"nothing failed", nil, nil, 0},
		{"a failed x265 setting says nothing about the ladder", nil, []validatedCandidate{x265}, 0},
	} {
		if got := hardwareQualityFallback(all, initial, tc.failed, tc.failures); len(got) != tc.want {
			t.Errorf("%s: %d fallback candidates, want %d", tc.name, len(got), tc.want)
		}
	}
	if got := hardwareQualityFallback(all, []validatedCandidate{x265}, map[string]bool{"x": true}, nil); got != nil {
		t.Errorf("an x265-only search got hardware fallbacks: %v", got)
	}
	if got := hardwareQualityFallback(initial, initial, map[string]bool{"vt-cand_q55": true, "vt-cand_q60": true}, nil); len(got) != 0 {
		t.Errorf("a search without fallback candidates got %v", got)
	}
}
