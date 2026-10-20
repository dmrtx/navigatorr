package transcodeworker

import (
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// Replays of a size search that verifies on the final validation grid without a
// selection margin: a candidate that passes the plain limits (VMAF p5 84, worst
// window 85) goes straight to grid verification, which measures every window the
// final check re-measures. The numbers are those of a hard 1080p episode whose
// only passing candidate, a hardware setting, scored p5 85.9 and worst window
// 87.3 at discovery and was refused by the margin (1.5, and 2 more for the media
// engine) before any verification ran.

const hardwareQ75 = "fallback-hardware-cand_q75"

// noMarginRules are the rules new size snapshots freeze: the grid, no margin.
func noMarginRules() *transcode.AutomaticSearchRules {
	r := gridReplayRules()
	r.SelectionMarginVMAF = 0
	return r
}

// marginRules are the rules #182 froze: the grid with the 1.5 margin.
func marginRules() *transcode.AutomaticSearchRules {
	return gridReplayRules()
}

func hardwareGridRules(extra float64) *transcode.HardwareSearchRules {
	return &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, FallbackQualityValues: []int{65, 70, 75}, GOPSize: 240, BFrames: 1, ExtraSelectionMarginVMAF: extra}
}

// hardEpisodeOnlyHardwarePasses is the recorded episode: every x265 setting
// (slow CRF 16 included) fails the plain limits on the grid windows, and the
// highest hardware rung clears them at discovery.
func hardEpisodeOnlyHardwarePasses(q75 replaySetting) map[string]replaySetting {
	settings := hardEpisode()
	for _, crf := range []int{16, 18, 20} {
		settings[replayKey("slow", crf)] = replayFails
	}
	settings[replayKey("", 75)] = q75
	return settings
}

func replayHardwareGrid(t *testing.T, settings map[string]replaySetting, rules *transcode.AutomaticSearchRules, extra float64) replayResult {
	t.Helper()
	return runReplayConfigured(t, replayConfig{settings: settings, sourceBytes: gridSourceBytes, rules: rules, hardware: hardwareGridRules(extra), ladder: hardwareFallbackLadder(), duration: gridReplayDuration})
}

// The recorded case. With the margin the candidate is refused before it is
// verified; without it, it is verified on every grid window and wins because
// every window passes.
func TestReplayHardwareCandidateReachesGridVerificationAndWinsWhenEveryWindowPasses(t *testing.T) {
	q75 := replaySetting{bytes: 20000, discovery: replayScore{85.9, 94.3}, holdout: 90}
	settings := hardEpisodeOnlyHardwarePasses(q75)

	margin := replayHardwareGrid(t, settings, marginRules(), 2)
	if margin.winner != "" || margin.reason(hardwareQ75) != reasonSelectionMarginNotMet || len(margin.evidence.Automatic.Verification.Attempts) != 0 {
		t.Fatalf("the margin no longer reproduces the recorded refusal: %s", margin.summary())
	}

	res := replayHardwareGrid(t, settings, noMarginRules(), 0)
	t.Logf("without the margin: %s", res.summary())
	if res.winner != hardwareQ75 || strings.Join(res.verified, ",") != hardwareQ75 {
		t.Fatalf("the hardware candidate did not reach grid verification and win: %s", res.summary())
	}
	a := res.evidence.Automatic
	if a.VerificationWindows != transcode.HoldoutWindowsFinalGrid || len(a.Verification.Attempts) != 1 || a.Verification.Attempts[0].CandidateID != hardwareQ75 || a.Verification.Attempts[0].Outcome != transcode.VerificationPassed {
		t.Fatalf("verification evidence %+v", a)
	}
	// It was measured on all seven grid windows: three at discovery and the four
	// the full encode's final check re-measures.
	if a.Verification.Attempts[0].Windows != 4 || len(res.record.Samples) != 7 || len(a.WinnerWindows) != 7 {
		t.Fatalf("windows %+v / %d samples / %+v", a.Verification.Attempts[0], len(res.record.Samples), a.WinnerWindows)
	}
}

// If the candidate fails any one grid window the search ends with no winner:
// nothing reaches a full encode.
func TestReplayHardwareCandidateFailingAGridWindowEndsWithNoWinner(t *testing.T) {
	for position := 0; position < 7; position++ {
		if position == 1 || position == 3 || position == 5 {
			continue // discovery windows
		}
		q75 := failAt(replaySetting{bytes: 20000, discovery: replayScore{85.9, 94.3}, holdout: 90}, 76, position)
		res := replayHardwareGrid(t, hardEpisodeOnlyHardwarePasses(q75), noMarginRules(), 0)
		if res.winner != "" || res.decision.Winner != nil {
			t.Fatalf("window %d: a candidate failing a grid window won: %s", position, res.summary())
		}
		attempts := res.evidence.Automatic.Verification.Attempts
		if len(attempts) == 0 || attempts[0].CandidateID != hardwareQ75 || attempts[0].Outcome != transcode.VerificationQualityFailed {
			t.Fatalf("window %d: verification evidence %+v", position, attempts)
		}
		if got := res.reason(hardwareQ75); !qualityRejection(got) {
			t.Fatalf("window %d: rejected setting labelled %q", position, got)
		}
	}
}

// A thin x265 pass (p5 84.2 against a limit of 84) that fails a grid window is rejected the same way; passing every window it wins.
func TestReplayThinX265CandidateIsDecidedByTheGrid(t *testing.T) {
	thin := replayScore{84.2, 90}
	build := func(strong replaySetting) map[string]replaySetting {
		settings := map[string]replaySetting{}
		for k, v := range gridSettings(nil) {
			v.discovery = replayScore{80, 84}
			settings[k] = v
		}
		settings[replayKey("medium", 22)] = strong
		return settings
	}
	pass := replayGrid(t, build(replaySetting{bytes: 23500, discovery: thin, holdout: 88}), noMarginRules())
	if pass.winner != "cand_crf22" {
		t.Fatalf("a thin candidate that passes every window was refused: %s", pass.summary())
	}
	margin := replayGrid(t, build(replaySetting{bytes: 23500, discovery: thin, holdout: 88}), marginRules())
	if margin.winner == "cand_crf22" || margin.reason("cand_crf22") != reasonSelectionMarginNotMet {
		t.Fatalf("the margin no longer refuses the thin candidate: %s", margin.summary())
	}
	fail := replayGrid(t, build(failAt(replaySetting{bytes: 23500, discovery: thin, holdout: 88}, 79, 4)), noMarginRules())
	t.Logf("thin and failing a grid window: %s", fail.summary())
	if fail.winner != "" || fail.decision.Winner != nil {
		t.Fatalf("a thin candidate failing a grid window won: %s", fail.summary())
	}
	if got := fail.reason("cand_crf22"); !qualityRejection(got) {
		t.Fatalf("rejected setting labelled %q", got)
	}
}
