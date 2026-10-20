package transcodeworker

import (
	"strconv"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// Replays of the size v3 quality walk: the medium CRF 14-21 fallback measured
// from the most compressed rung, stopping at the first that meets quality, and
// the in-between refinement rungs measured only next to a quality failure.

// walkReplayLadder is the ladder as the server builds it for a worker that
// advertises the walk: medium CRF 22-30, medium CRF 14-21 (slow too when the
// operation opted in), the slow savings family and the refinement rungs.
func walkReplayLadder(slow bool) []validatedCandidate {
	var vcs []validatedCandidate
	add := func(prefix, preset string, crfs ...int) {
		for _, crf := range crfs {
			vcs = append(vcs, validatedCandidate{index: len(vcs), candidate: transcode.BenchmarkCandidate{ID: prefix + "cand_crf" + itoa(crf), VideoCodec: "libx265", Quality: crf, Preset: preset, VideoProfile: "main10", PixelFormat: "p010le"}, bitDepth: 10, profile: "main10", pixelFormat: "p010le"})
		}
	}
	add("", "medium", 22, 24, 26, 28, 30)
	add("fallback-quality-medium-", "medium", 14, 15, 16, 17, 18, 19, 20, 21)
	if slow {
		add("fallback-quality-", "slow", 14, 15, 16, 17, 18, 19, 20, 21)
	}
	add("fallback-savings-", "slow", 24, 27, 30)
	add(fallbackRefinePrefix, "medium", 23, 25, 27, 29)
	return vcs
}

func itoa(n int) string { return strconv.Itoa(n) }

func walkRules(slow bool) *transcode.AutomaticSearchRules {
	presets := []string{"medium"}
	if slow {
		presets = append(presets, "slow")
	}
	return &transcode.AutomaticSearchRules{SelectionMarginVMAF: 1.5, HoldoutWindows: transcode.HoldoutWindowsFinalGridEdges, QualityFallbackPresets: presets, QualityWalk: true, RefinementQualityValues: []int{23, 25, 27, 29}, VerifyUntilExhausted: true}
}

func runWalkReplay(t *testing.T, settings map[string]replaySetting, slow bool) replayResult {
	t.Helper()
	return runReplayLadder(t, settings, 10_000_000, walkRules(slow), walkReplayLadder(slow))
}

var (
	walkFail = replayScore{82, 88.5}
	walkPass = replayScore{87.5, 90}
)

func failing(bytes int64) replaySetting {
	return replaySetting{bytes: bytes, discovery: walkFail, holdout: 84}
}
func passing(bytes int64) replaySetting {
	return replaySetting{bytes: bytes, discovery: walkPass, holdout: 90}
}

func indexOf(ids []string, id string) int {
	for i, v := range ids {
		if v == id {
			return i
		}
	}
	return -1
}

// The walk measures the leader (CRF 14), then the rungs from CRF 21 down, and
// stops at the first that passes: the lower rungs only spend more bits.
func TestReplayWalkStopsAtFirstPassingRung(t *testing.T) {
	settings := map[string]replaySetting{
		replayKey("medium", 22): failing(23500),
		replayKey("medium", 14): passing(46000),
		replayKey("medium", 21): failing(25500),
		replayKey("medium", 20): failing(28000),
		replayKey("medium", 19): passing(31000),
	}
	res := runWalkReplay(t, settings, false)
	t.Logf("%s; measured %v", res.summary(), res.measured)
	if res.winner != "fallback-quality-medium-cand_crf19" {
		t.Fatalf("winner %q, want medium CRF 19: %s", res.winner, res.summary())
	}
	want := []string{"cand_crf22", "fallback-quality-medium-cand_crf14", "fallback-quality-medium-cand_crf21", "fallback-quality-medium-cand_crf20", "fallback-quality-medium-cand_crf19"}
	if strings.Join(res.measured, ",") != strings.Join(want, ",") {
		t.Fatalf("measured %v, want %v", res.measured, want)
	}
	for _, crf := range []string{"18", "17", "16", "15"} {
		id := "fallback-quality-medium-cand_crf" + crf
		if got := res.reason(id); got != reasonSkippedLargerThanPassing {
			t.Fatalf("%s labelled %q, want %s", id, got, reasonSkippedLargerThanPassing)
		}
	}
	for _, ev := range res.decision.Evaluations {
		if ev.EvaluationReason == reasonSkippedLargerThanPassing && (len(ev.Uncertainties) != 1 || !strings.Contains(ev.Uncertainties[0], "fallback-quality-medium-cand_crf19")) {
			t.Fatalf("skip does not name the rung that passed: %v", ev.Uncertainties)
		}
	}
	// The leader is one stage and the walk one more, not one per rung.
	var walk []string
	for _, st := range res.stages {
		if st.Stage == "testing_alternatives" {
			walk = st.Candidates
		}
	}
	if strings.Join(walk, ",") != "fallback-quality-medium-cand_crf21,fallback-quality-medium-cand_crf20,fallback-quality-medium-cand_crf19" {
		t.Fatalf("walk stage %v: %+v", walk, res.stages)
	}
}

// The leader keeps its pruning: when CRF 14 fails quality no other rung of the
// family is measured.
func TestReplayWalkLeaderFailureSkipsTheFamily(t *testing.T) {
	settings := map[string]replaySetting{
		replayKey("medium", 22): failing(23500),
		replayKey("medium", 14): failing(46000),
	}
	res := runWalkReplay(t, settings, false)
	if res.winner != "" {
		t.Fatalf("a family whose best rung failed produced %q", res.winner)
	}
	if strings.Join(res.measured, ",") != "cand_crf22,fallback-quality-medium-cand_crf14" {
		t.Fatalf("measured %v", res.measured)
	}
	if got := res.reason("fallback-quality-medium-cand_crf18"); got != reasonSkippedQualityDominated {
		t.Fatalf("CRF 18 labelled %q", got)
	}
}

// A rung that passed discovery but fails verification on quality sends the
// search down: the rungs the walk had not measured are measured, then the
// search settles on the most compressed one that verifies.
func TestReplayWalkVerificationFailureMeasuresTheRestOfTheChain(t *testing.T) {
	settings := map[string]replaySetting{
		replayKey("medium", 22): failing(23500),
		replayKey("medium", 14): passing(46000),
		replayKey("medium", 21): failing(25500),
		replayKey("medium", 20): failing(28000),
		replayKey("medium", 19): {bytes: 31000, discovery: walkPass, holdout: 84},
		replayKey("medium", 18): {bytes: 34000, discovery: walkPass, holdout: 84},
		replayKey("medium", 17): passing(37000),
		replayKey("medium", 16): passing(40000),
		replayKey("medium", 15): passing(43000),
	}
	res := runWalkReplay(t, settings, false)
	t.Logf("%s; measured %v", res.summary(), res.measured)
	if res.winner != "fallback-quality-medium-cand_crf17" {
		t.Fatalf("winner %q, want medium CRF 17: %s", res.winner, res.summary())
	}
	first19 := indexOf(res.measured, "fallback-quality-medium-cand_crf19")
	for _, crf := range []string{"18", "17", "16", "15"} {
		at := indexOf(res.measured, "fallback-quality-medium-cand_crf"+crf)
		if at < 0 {
			t.Fatalf("CRF %s never measured after the verification failure: %v", crf, res.measured)
		}
		if at < first19 {
			t.Fatalf("CRF %s measured before the walk stopped", crf)
		}
	}
	for _, ev := range res.decision.Evaluations {
		if ev.EvaluationReason == reasonSkippedLargerThanPassing {
			t.Fatalf("%s skipped as larger than a passing rung although the chain was measured", ev.CandidateID)
		}
	}
}

// The first refinement happens when the initial ladder found a passing and a
// failing rung next to each other: only the rungs between them are measured.
func TestReplayRefinementBetweenPassAndFail(t *testing.T) {
	settings := map[string]replaySetting{
		replayKey("medium", 22): passing(23500),
		replayKey("medium", 24): passing(21000),
		replayKey("medium", 26): passing(19000),
		replayKey("medium", 28): failing(17000),
		replayKey("medium", 30): failing(15500),
		replayKey("medium", 27): passing(18000),
	}
	res := runWalkReplay(t, settings, false)
	t.Logf("%s; measured %v", res.summary(), res.measured)
	if res.winner != "fallback-refine-cand_crf27" {
		t.Fatalf("winner %q, want refinement CRF 27: %s", res.winner, res.summary())
	}
	for _, id := range []string{"fallback-refine-cand_crf23", "fallback-refine-cand_crf25", "fallback-refine-cand_crf29"} {
		if indexOf(res.measured, id) >= 0 {
			t.Fatalf("%s measured although it does not lie between a pass and a failure", id)
		}
	}
	if res.slowEncodes != 0 {
		t.Fatalf("slow encodes: %d", res.slowEncodes)
	}
}

// Nothing to refine when the ladder passes all the way up: no failure bounds the
// gap.
func TestReplayNoRefinementWithoutAFailureAbove(t *testing.T) {
	settings := map[string]replaySetting{
		replayKey("medium", 22): passing(23500),
		replayKey("medium", 24): passing(21000),
		replayKey("medium", 26): passing(19000),
		replayKey("medium", 28): passing(17000),
		replayKey("medium", 30): passing(15500),
	}
	res := runWalkReplay(t, settings, false)
	if res.winner != "cand_crf30" {
		t.Fatalf("winner %q: %s", res.winner, res.summary())
	}
	for _, id := range res.measured {
		if strings.HasPrefix(id, fallbackRefinePrefix) {
			t.Fatalf("%s measured with nothing to refine", id)
		}
	}
}

// A verification failure opens a gap the initial stage could not see: the
// rung between the best verified setting and the one that just failed is
// measured then, and no rung beyond the failure is.
func TestReplayRefinementAfterVerificationFailure(t *testing.T) {
	settings := map[string]replaySetting{
		replayKey("medium", 22): passing(23500),
		replayKey("medium", 24): passing(21000),
		replayKey("medium", 26): passing(19000),
		replayKey("medium", 28): {bytes: 17000, discovery: walkPass, holdout: 84},
		replayKey("medium", 30): {bytes: 15500, discovery: walkPass, holdout: 84},
		replayKey("medium", 27): passing(18000),
	}
	res := runWalkReplay(t, settings, false)
	t.Logf("%s; measured %v; verified %v", res.summary(), res.measured, res.verified)
	if res.winner != "fallback-refine-cand_crf27" {
		t.Fatalf("winner %q, want refinement CRF 27: %s", res.winner, res.summary())
	}
	at27 := indexOf(res.measured, "fallback-refine-cand_crf27")
	if at27 < 0 || len(res.verified) == 0 || res.verified[0] != "cand_crf30" {
		t.Fatalf("refinement measured %d before the first verification: %v", at27, res.verified)
	}
	for _, id := range []string{"fallback-refine-cand_crf25", "fallback-refine-cand_crf29", "fallback-refine-cand_crf23"} {
		if indexOf(res.measured, id) >= 0 {
			t.Fatalf("%s measured outside the gap", id)
		}
	}
}

// Slow takes part only when the operation opted in; then it follows the medium
// walk and is walked the same way.
func TestReplayWalkSlowOnlyWhenOptedIn(t *testing.T) {
	settings := map[string]replaySetting{
		replayKey("medium", 22): failing(23500),
		replayKey("medium", 14): failing(46000),
	}
	res := runWalkReplay(t, settings, false)
	if res.slowEncodes != 0 || res.winner != "" {
		t.Fatalf("slow encoded without an opt-in: %s", res.summary())
	}
	settings[replayKey("slow", 14)] = replaySetting{bytes: 44000, discovery: walkPass, holdout: 90}
	settings[replayKey("slow", 21)] = failing(25000)
	settings[replayKey("slow", 20)] = replaySetting{bytes: 26500, discovery: walkPass, holdout: 90}
	res = runWalkReplay(t, settings, true)
	t.Logf("%s; measured %v", res.summary(), res.measured)
	if res.winner != "fallback-quality-cand_crf20" {
		t.Fatalf("winner %q, want slow CRF 20: %s", res.winner, res.summary())
	}
	if indexOf(res.measured, "fallback-quality-cand_crf14") < indexOf(res.measured, "fallback-quality-medium-cand_crf14") {
		t.Fatalf("slow measured before medium: %v", res.measured)
	}
	if got := res.reason("fallback-quality-cand_crf19"); got != reasonSkippedLargerThanPassing {
		t.Fatalf("slow CRF 19 labelled %q", got)
	}
}
