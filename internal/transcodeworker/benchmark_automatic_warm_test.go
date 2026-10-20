package transcodeworker

import (
	"slices"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// Replays of a warm start: the initial x265 ladder measured from the
// siblings' winning CRF instead of all at once. Each scenario runs with and
// without the prior; the prior may only measure fewer rungs, never pick a
// different winner.

// warmSettings scores every medium and slow CRF: a rung passes discovery up to
// passUpTo and fails above it; holdoutFails lists the CRFs that pass discovery
// but fail verification on quality. A higher CRF is a smaller file.
func warmSettings(passUpTo int, holdoutFails ...int) map[string]replaySetting {
	settings := map[string]replaySetting{}
	for _, preset := range []string{"medium", "slow"} {
		for crf := 14; crf <= 30; crf++ {
			bytes := int64(50000 - 1000*crf)
			s := failing(bytes)
			if crf <= passUpTo {
				s = passing(bytes)
				if slices.Contains(holdoutFails, crf) {
					s.holdout = 84
				}
			}
			settings[replayKey(preset, crf)] = s
		}
	}
	return settings
}

func runWarmReplay(t *testing.T, settings map[string]replaySetting, prior int) replayResult {
	t.Helper()
	cfg := replayConfig{settings: settings, sourceBytes: 10_000_000, rules: walkRules(false), ladder: walkReplayLadder(false)}
	if prior > 0 {
		cfg.setup = func(record *BenchmarkRecord) {
			record.WarmStart = &transcode.WarmStart{QualityValue: prior, Basis: transcode.WarmStartSeriesSeason, Siblings: 3}
		}
	}
	return runReplayConfigured(t, cfg)
}

// warmAndCold runs a scenario both ways and checks the prior changed nothing
// but the measuring.
func warmAndCold(t *testing.T, settings map[string]replaySetting, prior int) (warm, cold replayResult) {
	t.Helper()
	cold = runWarmReplay(t, settings, 0)
	warm = runWarmReplay(t, settings, prior)
	t.Logf("cold: %s; measured %v", cold.summary(), cold.measured)
	t.Logf("warm: %s; measured %v", warm.summary(), warm.measured)
	if warm.winner != cold.winner {
		t.Fatalf("the prior changed the winner: %q, without it %q", warm.winner, cold.winner)
	}
	if len(warm.measured) > len(cold.measured) {
		t.Fatalf("the prior measured more rungs (%d) than the full ladder (%d)", len(warm.measured), len(cold.measured))
	}
	if cold.evidence.Automatic.WarmStart != nil {
		t.Fatal("a search without a prior reports one")
	}
	return warm, cold
}

func warmEvidence(t *testing.T, res replayResult) *transcode.BenchmarkWarmStartEvidence {
	t.Helper()
	ws := res.evidence.Automatic.WarmStart
	if ws == nil {
		t.Fatal("the search does not report its warm start")
	}
	return ws
}

// The prior lands on the boundary: its rung passes and the next fails, so the
// rungs on either side are not measured.
func TestReplayWarmStartBracketsAtThePrior(t *testing.T) {
	warm, _ := warmAndCold(t, warmSettings(26), 26)
	if warm.winner != "cand_crf26" {
		t.Fatalf("winner %q", warm.winner)
	}
	if got := strings.Join(warm.measured, ","); got != "cand_crf22,cand_crf26,cand_crf28,fallback-refine-cand_crf27" {
		t.Fatalf("measured %s", got)
	}
	if got := warm.reason("cand_crf24"); got != reasonSkippedLargerThanPassing {
		t.Fatalf("CRF 24 labelled %q", got)
	}
	if got := warm.reason("cand_crf30"); got != reasonSkippedQualityDominated {
		t.Fatalf("CRF 30 labelled %q", got)
	}
	ws := warmEvidence(t, warm)
	if ws.QualityValue != 26 || ws.Start != "cand_crf26" || ws.NotUsed != "" || strings.Join(ws.Skipped, ",") != "cand_crf24,cand_crf30" {
		t.Fatalf("warm start evidence %+v", ws)
	}
	// The initial stage is the leader, then one stage for the walk.
	var initial [][]string
	for _, st := range warm.stages {
		if st.Stage == "testing_initial" {
			initial = append(initial, st.Candidates)
		}
	}
	if len(initial) != 2 || strings.Join(initial[1], ",") != "cand_crf26,cand_crf28" {
		t.Fatalf("initial stages %v", initial)
	}
}

// A source harder than its siblings: the prior's rungs both fail, so the walk
// goes down until a rung passes.
func TestReplayWarmStartWalksDownFromAFailingPrior(t *testing.T) {
	warm, _ := warmAndCold(t, warmSettings(24), 28)
	if warm.winner != "cand_crf24" {
		t.Fatalf("winner %q", warm.winner)
	}
	// CRF 28 and 30 fail together, 26 fails, and 24, the first to pass, ends
	// the walk.
	if got := strings.Join(warm.measured[:5], ","); got != "cand_crf22,cand_crf28,cand_crf30,cand_crf26,cand_crf24" {
		t.Fatalf("measured %v", warm.measured)
	}
}

// An easier source: the walk goes up from the prior while rungs pass.
func TestReplayWarmStartWalksUpFromAPassingPrior(t *testing.T) {
	warm, _ := warmAndCold(t, warmSettings(28), 22)
	if warm.winner != "cand_crf28" {
		t.Fatalf("winner %q", warm.winner)
	}
	if got := strings.Join(warm.measured, ","); got != "cand_crf22,cand_crf24,cand_crf26,cand_crf28,cand_crf30,fallback-refine-cand_crf29" {
		t.Fatalf("measured %s", got)
	}
	if ws := warmEvidence(t, warm); ws.Start != "cand_crf24" || len(ws.Skipped) != 0 {
		t.Fatalf("a prior below the ladder starts at its first follower: %+v", ws)
	}
}

// The rung the walk stopped at fails verification on quality: the rungs it had
// left unmeasured below are measured then, and the search settles where the
// full ladder would have.
func TestReplayWarmStartVerificationFailureMeasuresTheRungsBelow(t *testing.T) {
	warm, _ := warmAndCold(t, warmSettings(26, 26), 26)
	if !slices.Contains(warm.measured, "cand_crf24") {
		t.Fatalf("CRF 24 must be measured after CRF 26 failed verification: %v", warm.measured)
	}
	if warm.reason("cand_crf24") == reasonSkippedLargerThanPassing {
		t.Fatal("a measured rung is still labelled as skipped")
	}
}

// When the ladder's highest-quality rung fails quality there is nothing to
// order, and the search says so.
func TestReplayWarmStartNotUsedWhenTheLeaderFails(t *testing.T) {
	warm, _ := warmAndCold(t, warmSettings(19), 26)
	if ws := warmEvidence(t, warm); ws.NotUsed != transcode.WarmStartNoRungs || ws.Start != "" {
		t.Fatalf("warm start evidence %+v", ws)
	}
}
