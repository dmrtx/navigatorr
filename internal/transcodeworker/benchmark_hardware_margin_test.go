package transcodeworker

import (
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// The media engine's sample scores came out a few points higher than the same
// encode scores over the whole file, so a hardware candidate must clear the
// p5 and worst-window limits by a larger margin than an x265 one. The numbers
// are those of a hard 1080p episode whose hardware winner passed the sample
// search with p5 87.1 and worst window 89.2 against limits of 84 and 85, and
// then failed the full-file check.
func runHardwareMarginReplay(t *testing.T, settings map[string]replaySetting, extra float64) replayResult {
	t.Helper()
	hw := &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, FallbackQualityValues: []int{65, 70, 75}, GOPSize: 240, BFrames: 1, ExtraSelectionMarginVMAF: extra}
	return runReplayHardware(t, settings, 10_000_000, replayRules(), hw, hardwareFallbackLadder())
}

func TestReplayHardwareSelectionMarginRejectsAThinHardwareResult(t *testing.T) {
	for name, thin := range map[string]replayScore{
		"p5 under the larger margin":     {87.1, 89.2},
		"window under the larger margin": {89.0, 88.2},
	} {
		t.Run(name, func(t *testing.T) {
			settings := hardEpisode()
			settings[replayKey("medium", 16)] = replayPasses(23500)
			// The hardware rung (the highest, so measured first) is smaller and clears the x265 margin of 1.5
			// (p5 85.5, window 86.5) but not the hardware margin of 3.5 (87.5, 88.5).
			settings[replayKey("", 75)] = replaySetting{bytes: 20000, discovery: thin, holdout: 88.5}

			earlier := runHardwareMarginReplay(t, settings, 0)
			if earlier.winner != "fallback-hardware-cand_q75" {
				t.Fatalf("a snapshot without the extra margin no longer selects the thin hardware result: %s", earlier.summary())
			}

			res := runHardwareMarginReplay(t, settings, 2)
			t.Logf("with the extra margin: %s", res.summary())
			if got := res.reason("fallback-hardware-cand_q75"); got != reasonSelectionMarginNotMet {
				t.Fatalf("thin hardware result labelled %q", got)
			}
			if res.winner != "fallback-quality-medium-cand_crf16" {
				t.Fatalf("the x265 result did not win over the thin hardware one: %s", res.summary())
			}
		})
	}
}

// The extra margin is for the media engine only: an x265 candidate with the
// very scores a hardware candidate just failed on is still selectable.
func TestReplayHardwareExtraMarginDoesNotTouchX265(t *testing.T) {
	settings := hardEpisode()
	settings[replayKey("medium", 16)] = replaySetting{bytes: 20000, discovery: replayScore{87.1, 89.2}, holdout: 88.5}
	res := runHardwareMarginReplay(t, settings, 2)
	if res.winner != "fallback-quality-medium-cand_crf16" || res.reason("fallback-quality-medium-cand_crf16") == reasonSelectionMarginNotMet {
		t.Fatalf("the extra hardware margin applied to x265: %s", res.summary())
	}
}

// A hardware result that clears the larger margin still wins.
func TestReplayHardwareResultClearingTheLargerMarginStillWins(t *testing.T) {
	settings := hardEpisode()
	settings[replayKey("medium", 16)] = replayPasses(23500)
	settings[replayKey("", 75)] = replaySetting{bytes: 20000, discovery: replayScore{88.0, 90.1}, holdout: 89.5}
	res := runHardwareMarginReplay(t, settings, 2)
	if res.winner != "fallback-hardware-cand_q75" {
		t.Fatalf("a hardware result with margin to spare was refused: %s", res.summary())
	}
}
