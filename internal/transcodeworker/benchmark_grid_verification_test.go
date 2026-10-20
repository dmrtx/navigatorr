package transcodeworker

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// Replays of verification on the final validation grid and of verification
// that goes on until a setting passes. The source is about an hour long, so
// the grid has seven windows: the three discovery windows (grid positions 1,
// 3 and 5), the two edges the earlier rules verified on (0 and 6) and two
// interior windows only the grid measures (2 and 4).

const gridReplayDuration = 3700.0

// gridSourceBytes scales the replay source with its length, so every setting
// saves as much as in the short replays.
const gridSourceBytes = int64(61_666_667)

func gridReplayRules() *transcode.AutomaticSearchRules {
	return &transcode.AutomaticSearchRules{SelectionMarginVMAF: 1.5, HoldoutWindows: transcode.HoldoutWindowsFinalGrid, VerifyUntilExhausted: true, FinalFreshWindows: 2, QualityFallbackPresets: []string{"medium", "slow"}}
}

// edgeReplayRules are the rules the grid replaced: two edge holdouts, two attempts.
func edgeReplayRules() *transcode.AutomaticSearchRules {
	return &transcode.AutomaticSearchRules{SelectionMarginVMAF: 1.5, HoldoutWindows: transcode.HoldoutWindowsFinalGridEdges, QualityFallbackPresets: []string{"medium", "slow"}}
}

// gridCenter is the centre of grid window i (positions in time order) of the
// hour-long source.
func gridCenter(i int) float64 {
	return transcode.PlanFinalValidationWindows(gridReplayDuration, 10, 600, 5, 12)[i].CenterSeconds
}

// gridSettings is a ladder whose every setting measures well at discovery and
// passes verification, ordered by size; a test overrides the ones it needs.
func gridSettings(overrides map[string]replaySetting) map[string]replaySetting {
	good := func(bytes int64) replaySetting {
		return replaySetting{bytes: bytes, discovery: replayScore{88, 90}, holdout: 90}
	}
	settings := map[string]replaySetting{
		replayKey("medium", 22): good(23500), replayKey("medium", 24): good(21000), replayKey("medium", 26): good(19000),
		replayKey("medium", 28): good(17000), replayKey("medium", 30): good(15500),
		replayKey("medium", 16): good(32000), replayKey("medium", 18): good(29000), replayKey("medium", 20): good(26000),
		replayKey("slow", 16): good(31000), replayKey("slow", 18): good(28000), replayKey("slow", 20): good(25000),
		replayKey("slow", 24): good(20000), replayKey("slow", 27): good(17500), replayKey("slow", 30): good(15000),
	}
	for k, v := range overrides {
		settings[k] = v
	}
	return settings
}

// failAt makes a setting score low on the verification window at grid position i.
func failAt(base replaySetting, score float64, positions ...int) replaySetting {
	base.holdoutAt = func(center float64) (float64, bool) {
		for _, i := range positions {
			if math.Abs(center-gridCenter(i)) < 1e-6 {
				return score, true
			}
		}
		return 0, false
	}
	return base
}

func replayGrid(t *testing.T, settings map[string]replaySetting, rules *transcode.AutomaticSearchRules) replayResult {
	t.Helper()
	return runReplayConfigured(t, replayConfig{settings: settings, sourceBytes: gridSourceBytes, rules: rules, ladder: replayLadder(true), duration: gridReplayDuration})
}

// A hard episode: the smallest setting holds up on the first and last windows
// the earlier rules verified on but fails on an interior window only the grid
// measures. The earlier rules accept it and the full encode then fails its
// final validation; the grid rejects it before the encode and the safer
// setting wins.
func TestGridVerificationRejectsWhatEdgeHoldoutsAccept(t *testing.T) {
	settings := gridSettings(map[string]replaySetting{
		// Passes discovery with the margin (p5 87, mean 89.5) and the edges.
		replayKey("medium", 24): failAt(replaySetting{bytes: 21000, discovery: replayScore{87, 89.5}, holdout: 90}, 78, 2, 4),
		replayKey("medium", 26): {bytes: 19000, discovery: replayScore{85.2, 88.4}, holdout: 90},
		replayKey("medium", 28): {bytes: 17000, discovery: replayScore{83.5, 88.1}, holdout: 90},
		replayKey("medium", 30): {bytes: 15500, discovery: replayScore{82.0, 88.0}, holdout: 90},
	})
	edges := replayGrid(t, settings, edgeReplayRules())
	if edges.winner != "cand_crf24" {
		t.Fatalf("the earlier holdouts no longer accept the weak setting: %s", edges.summary())
	}
	grid := replayGrid(t, settings, gridReplayRules())
	t.Logf("grid: %s", grid.summary())
	if grid.winner != "cand_crf22" || strings.Join(grid.verified, ",") != "cand_crf24,cand_crf22" {
		t.Fatalf("grid verification did not reject the weak setting: %s", grid.summary())
	}
	if got := grid.reason("cand_crf24"); !qualityRejection(got) {
		t.Fatalf("rejected setting labelled %q", got)
	}
	a := grid.evidence.Automatic
	if a.VerificationWindows != transcode.HoldoutWindowsFinalGrid || a.VerifiedSamples != 7 || len(grid.record.Samples) != 7 {
		t.Fatalf("automatic evidence %+v", a)
	}
	v := a.Verification
	if len(v.Attempts) != 2 || v.Attempts[0].CandidateID != "cand_crf24" || v.Attempts[0].Role != transcode.VerificationRoleSmallest || v.Attempts[0].Outcome != transcode.VerificationQualityFailed || v.Attempts[0].Windows != 4 ||
		v.Attempts[1].CandidateID != "cand_crf22" || v.Attempts[1].Role != transcode.VerificationRoleSafest || v.Attempts[1].Outcome != transcode.VerificationPassed || v.StoppedBy != "" {
		t.Fatalf("verification evidence %+v", v)
	}
	// The winner's per-window sample scores: three discovery windows and four
	// verification windows, in time order.
	if len(a.WinnerWindows) != 7 {
		t.Fatalf("winner windows %+v", a.WinnerWindows)
	}
	discovery := 0
	for i, w := range a.WinnerWindows {
		if w.Role == "discovery" {
			discovery++
		}
		if i > 0 && w.StartSeconds <= a.WinnerWindows[i-1].StartSeconds {
			t.Fatalf("winner windows out of order: %+v", a.WinnerWindows)
		}
	}
	if discovery != 3 {
		t.Fatalf("%d discovery windows in %+v", discovery, a.WinnerWindows)
	}
}

// The earlier rules stopped verifying after two attempts. Here the two
// settings the search can verify first both fail; the quality fallback is then
// measured and its winner verified, a third attempt the earlier rules never
// made.
func TestGridVerificationGoesPastTwoAttempts(t *testing.T) {
	settings := gridSettings(map[string]replaySetting{
		replayKey("medium", 24): failAt(replaySetting{bytes: 21000, discovery: replayScore{87, 89.5}}, 78, 2),
		replayKey("medium", 22): failAt(replaySetting{bytes: 23500, discovery: replayScore{88, 90}}, 79, 4),
		replayKey("medium", 26): {bytes: 19000, discovery: replayScore{85.2, 88.4}},
		replayKey("medium", 28): {bytes: 17000, discovery: replayScore{83.5, 88.1}},
		replayKey("medium", 30): {bytes: 15500, discovery: replayScore{82.0, 88.0}},
	})
	edges := replayGrid(t, settings, edgeReplayRules())
	t.Logf("two attempts: %s", edges.summary())
	if edges.winner != "cand_crf24" && edges.winner != "" {
		t.Fatalf("unexpected earlier result %s", edges.summary())
	}
	grid := replayGrid(t, settings, gridReplayRules())
	t.Logf("until exhausted: %s", grid.summary())
	if grid.winner == "" || len(grid.verified) < 3 {
		t.Fatalf("verification stopped early: %s", grid.summary())
	}
	v := grid.evidence.Automatic.Verification
	if len(v.Attempts) != len(grid.verified) || v.Attempts[len(v.Attempts)-1].Outcome != transcode.VerificationPassed || v.StoppedBy != "" {
		t.Fatalf("verification evidence %+v", v)
	}
	// The fallback winner is the only verified setting, so nothing smaller was missed.
	if !strings.HasPrefix(grid.winner, "fallback-quality-medium-") {
		t.Fatalf("winner %s", grid.winner)
	}
}

// With no earlier cap, verification still ends: with nothing eligible left it
// reports no verified candidate, naming every attempt.
func TestGridVerificationEndsWhenNothingIsEligible(t *testing.T) {
	bad := func(s replaySetting) replaySetting { s.holdout = 80; return s }
	settings := gridSettings(nil)
	for k, v := range settings {
		settings[k] = bad(v)
	}
	grid := replayGrid(t, settings, gridReplayRules())
	if grid.winner != "" || grid.decision.DecisionReason != "no_verified_candidate" {
		t.Fatalf("expected no verified candidate: %s", grid.summary())
	}
	v := grid.evidence.Automatic.Verification
	if v.StoppedBy != "" || len(v.Attempts) < 3 || len(v.Unverified) != 0 {
		t.Fatalf("verification evidence %+v", v)
	}
	for _, a := range v.Attempts {
		if a.Outcome != transcode.VerificationQualityFailed {
			t.Fatalf("attempt %+v", a)
		}
	}
}

// After the smallest setting fails, the safest passes; the settings between
// them are bisected, the middle one first and leaning to the more compressed.
func TestGridVerificationBisectsBetweenFailureAndPass(t *testing.T) {
	settings := gridSettings(map[string]replaySetting{
		replayKey("medium", 30): failAt(replaySetting{bytes: 15500, discovery: replayScore{88, 90}}, 70, 2),
		replayKey("medium", 28): failAt(replaySetting{bytes: 17000, discovery: replayScore{88, 90}}, 70, 4),
	})
	grid := replayGrid(t, settings, gridReplayRules())
	t.Logf("bisect: %s", grid.summary())
	if got := strings.Join(grid.verified, ","); got != "cand_crf30,cand_crf22,cand_crf26,cand_crf28" || grid.winner != "cand_crf26" {
		t.Fatalf("verification order %s, winner %s", got, grid.winner)
	}
	var roles []string
	for _, a := range grid.evidence.Automatic.Verification.Attempts {
		roles = append(roles, a.Role+":"+a.Outcome)
	}
	want := "smallest:quality_failed,safest:passed,bisect:passed,bisect:quality_failed"
	if strings.Join(roles, ",") != want {
		t.Fatalf("roles %v, want %s", roles, want)
	}
	// The winner is the smallest verified setting, and its evidence is its own.
	if grid.decision.Winner.CandidateID != "cand_crf26" || grid.evidence.Automatic.VerifiedSamples != 7 {
		t.Fatalf("decision %+v", grid.decision.Winner)
	}
}

// A search that passed verification keeps its winner when the time limit
// stops the hunt for a smaller one; the result says so.
func TestGridVerificationTimeLimitKeepsAVerifiedWinner(t *testing.T) {
	settings := gridSettings(map[string]replaySetting{
		replayKey("medium", 30): failAt(replaySetting{bytes: 15500, discovery: replayScore{88, 90}}, 70, 2),
	})
	stops := 0
	grid := runReplayConfigured(t, replayConfig{settings: settings, sourceBytes: gridSourceBytes, rules: gridReplayRules(), ladder: replayLadder(true), duration: gridReplayDuration,
		before: func(stage string, candidates []validatedCandidate, cancel context.CancelCauseFunc) {
			// Stop at the third verification attempt: crf30 failed, crf22 passed.
			if stage == phaseVerifyingSamples {
				stops++
				if stops == 3 {
					cancel(errSearchBudgetStopped)
				}
			}
		}})
	if grid.winner != "cand_crf22" || strings.Join(grid.verified, ",") != "cand_crf30,cand_crf22" {
		t.Fatalf("winner %q after %v", grid.winner, grid.verified)
	}
	v := grid.evidence.Automatic.Verification
	if v.StoppedBy != transcode.VerificationStoppedTimeCap || len(v.Attempts) != 2 {
		t.Fatalf("verification evidence %+v", v)
	}
	if last := grid.evidence.Automatic.Stages[len(grid.evidence.Automatic.Stages)-1]; last.Outcome != "stopped" {
		t.Fatalf("interrupted stage %+v", last)
	}
}

// When the time limit ends the search before any setting is verified, the
// result is an honest not_verified: the settings that passed discovery are
// named, with the reason.
func TestGridVerificationTimeLimitWithoutAWinnerIsNotVerified(t *testing.T) {
	settings := gridSettings(map[string]replaySetting{
		replayKey("medium", 30): failAt(replaySetting{bytes: 15500, discovery: replayScore{88, 90}}, 70, 2),
	})
	stops := 0
	grid := runReplayConfigured(t, replayConfig{settings: settings, sourceBytes: gridSourceBytes, rules: gridReplayRules(), ladder: replayLadder(true), duration: gridReplayDuration,
		before: func(stage string, candidates []validatedCandidate, cancel context.CancelCauseFunc) {
			if stage == phaseVerifyingSamples {
				stops++
				if stops == 2 {
					cancel(errSearchBudgetStopped)
				}
			}
		}})
	if grid.winner != "" || grid.decision.DecisionReason != "no_verified_candidate" {
		t.Fatalf("expected no result: %s", grid.summary())
	}
	v := grid.evidence.Automatic.Verification
	if v.StoppedBy != transcode.VerificationStoppedTimeCap || len(v.Attempts) != 1 || len(v.Unverified) == 0 {
		t.Fatalf("verification evidence %+v", v)
	}
	found := false
	for _, e := range grid.decision.Evaluations {
		if e.CandidateID == "cand_crf22" {
			found = true
			if e.EvaluationReason != reasonNotVerified || !strings.Contains(strings.Join(e.Uncertainties, " "), "time limit") {
				t.Fatalf("unverified crf22: %+v", e)
			}
		}
		if e.CandidateID == "cand_crf30" && !qualityRejection(e.EvaluationReason) {
			t.Fatalf("failed crf30 labelled %q", e.EvaluationReason)
		}
	}
	if !found {
		t.Fatal("the safer setting the time limit left unverified is not in the result")
	}
}

// Before another attempt starts, the search checks that it can finish before
// its hard deadline; an exhausted budget ends verification without starting it.
func TestGridVerificationStopsBeforeAnAttemptThatCannotFinish(t *testing.T) {
	settings := gridSettings(map[string]replaySetting{
		replayKey("medium", 30): failAt(replaySetting{bytes: 15500, discovery: replayScore{88, 90}}, 70, 2),
	})
	start := time.Unix(1_700_000_000, 0)
	now := start
	grid := runReplayConfigured(t, replayConfig{settings: settings, sourceBytes: gridSourceBytes, rules: gridReplayRules(), ladder: replayLadder(true), duration: gridReplayDuration,
		setup: func(r *BenchmarkRecord) {
			b := newSearchBudget(start, 600, &transcode.SearchExtension{MaxSeconds: 2400, SteadySeconds: 300, VerifySeconds: 600})
			b.now = func() time.Time { return now }
			r.searchBudget = b
		},
		before: func(stage string, _ []validatedCandidate, _ context.CancelCauseFunc) {
			// The first verification attempt takes the search to the hard cap.
			if stage == phaseVerifyingSamples {
				now = start.Add(2500 * time.Second)
			}
		}})
	if grid.winner != "" || len(grid.verified) != 1 {
		t.Fatalf("a second attempt started past the cap: %s", grid.summary())
	}
	if v := grid.evidence.Automatic.Verification; v.StoppedBy != transcode.VerificationStoppedTimeCap || len(v.Attempts) != 1 {
		t.Fatalf("verification evidence %+v", v)
	}
}

// A cancellation that is not the search's time limit (the user's, a shutdown)
// is still an error, never a result.
func TestGridVerificationCancellationIsNotATimeLimit(t *testing.T) {
	if searchTimeLimitReached(context.Background(), context.Canceled) {
		t.Fatal("a plain cancellation was taken for the time limit")
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errSearchBudgetStopped)
	if !searchTimeLimitReached(ctx, context.Canceled) {
		t.Fatal("the budget watcher's stop was not recognised")
	}
	user, cancelUser := context.WithCancelCause(context.Background())
	cancelUser(context.Canceled)
	if searchTimeLimitReached(user, user.Err()) {
		t.Fatal("a user cancellation was taken for the time limit")
	}
}
