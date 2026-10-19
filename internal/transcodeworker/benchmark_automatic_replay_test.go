package transcodeworker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// Deterministic replay of size-mode searches on recorded-style evaluations:
// synthetic numbers shaped like a batch of ~10 Mbps H.264 1080p episodes
// whose every setting saved far more than the 15% minimum, run through the
// real selection, holdout and fallback logic with and without the automatic
// search rules. No encoder runs; each setting's sample bytes and per-window
// VMAF frames are fixed.

// replayScore is one setting's discovery result: every window has 10% of its
// frames at p5 and the rest at the level that makes the window mean (which is
// also the worst window, as a window is one rolling span) equal to mean.
type replayScore struct{ p5, mean float64 }

type replaySetting struct {
	bytes     int64 // per 10 s window
	discovery replayScore
	// holdout is the constant per-frame VMAF on the holdout windows.
	holdout      float64
	holdoutBytes int64 // per holdout window; zero means bytes
}

// replayKey names a setting by preset and CRF, independent of its family ID.
func replayKey(preset string, crf int) string { return fmt.Sprintf("%s-%d", preset, crf) }

// replayLadder is the size v3 ladder as the server builds it: medium CRF
// 22-30, then (rules only) medium CRF 16-20, slow CRF 16-20 and the slow
// savings family.
func replayLadder(rules bool) []validatedCandidate {
	var vcs []validatedCandidate
	add := func(prefix, preset string, crfs ...int) {
		for _, crf := range crfs {
			vcs = append(vcs, validatedCandidate{index: len(vcs), candidate: transcode.BenchmarkCandidate{ID: fmt.Sprintf("%scand_crf%d", prefix, crf), VideoCodec: "libx265", Quality: crf, Preset: preset, VideoProfile: "main10", PixelFormat: "p010le"}, bitDepth: 10, profile: "main10", pixelFormat: "p010le"})
		}
	}
	add("", "medium", 22, 24, 26, 28, 30)
	if rules {
		add("fallback-quality-medium-", "medium", 16, 18, 20)
	}
	add("fallback-quality-", "slow", 16, 18, 20)
	add("fallback-savings-", "slow", 24, 27, 30)
	return vcs
}

type replayResult struct {
	winner      string
	verified    []string
	measured    []string
	slowEncodes int // slow sample encodes: about 4x the wall time of medium ones
	encodes     int
	decision    *transcode.BenchmarkDecision
}

func (r replayResult) reason(id string) string {
	for _, ev := range r.decision.Evaluations {
		if ev.CandidateID == id {
			return ev.EvaluationReason
		}
	}
	return ""
}

func (r replayResult) summary() string {
	winner := r.winner
	if winner == "" {
		winner = "none (" + r.decision.DecisionReason + ")"
	}
	return fmt.Sprintf("verified %s -> winner %s; %d sample encodes (%d slow)", strings.Join(r.verified, ", "), winner, r.encodes, r.slowEncodes)
}

func runReplay(t *testing.T, settings map[string]replaySetting, sourceBytes int64, rules *transcode.AutomaticSearchRules) replayResult {
	t.Helper()
	p5, worst := 84.0, 85.0
	record := &BenchmarkRecord{Mode: "size", ModePolicyVersion: optimization.ModePolicyV3, Automatic: true, QualityPruning: true, AutomaticSearch: rules, Metric: "vmaf", SourceDuration: 600,
		Quality: &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Target: 92, Minimum: 88, P5Minimum: &p5, WorstWindowMinimum: &worst, WorstWindowSeconds: 4, GuardrailEnforcement: "reject"}}}
	// Discovery at 20/50/80%; holdouts on the final grid's edges (rules) or
	// at 35/65%.
	centers := []float64{120, 300, 480, 210, 390}
	if rules != nil {
		centers[3], centers[4] = 60, 540
	}
	for i, c := range centers {
		record.Samples = append(record.Samples, transcode.BenchmarkSampleWindow{Index: i, StartSeconds: c - 5, DurationSeconds: 10, CenterSeconds: c})
	}
	model, err := quality.Model("v1_1080p_3h")
	if err != nil {
		t.Fatal(err)
	}
	var res replayResult
	runner := &ProductionBenchmarkRunner{automaticHook: func(_ context.Context, stage *BenchmarkRecord, e *BenchmarkExecutionEvidence, candidates []validatedCandidate) error {
		for _, vc := range candidates {
			key := replayKey(vc.candidate.Preset, vc.candidate.Quality)
			s, ok := settings[key]
			if !ok {
				t.Fatalf("no recorded evaluation for %s (%s)", vc.candidate.ID, key)
			}
			holdoutStage := stage.Samples[0].Index >= 3
			if holdoutStage {
				res.verified = append(res.verified, vc.candidate.ID)
			} else {
				res.measured = append(res.measured, vc.candidate.ID)
			}
			for _, window := range stage.Samples {
				res.encodes++
				if vc.candidate.Preset == "slow" {
					res.slowEncodes++
				}
				frames := make([]map[string]any, 100)
				bytes, seconds := s.bytes, 1.0
				if vc.candidate.Preset == "slow" {
					seconds = 4
				}
				for i := range frames {
					score := s.holdout
					if !holdoutStage {
						score = (s.discovery.mean - 0.1*s.discovery.p5) / 0.9
						if i < 10 {
							score = s.discovery.p5
						}
					}
					frames[i] = map[string]any{"frameNum": i, "metrics": map[string]any{"vmaf": score}}
				}
				if holdoutStage && s.holdoutBytes > 0 {
					bytes = s.holdoutBytes
				}
				data, _ := json.Marshal(map[string]any{"frames": frames})
				stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, window.Index, window.StartSeconds, 25, 1, 4, nil)
				if err != nil {
					return err
				}
				e.CandidateSamples = append(e.CandidateSamples, BenchmarkCandidateSampleResult{CandidateID: vc.candidate.ID, SampleIndex: window.Index, SizeBytes: bytes, EncodeDurationSec: seconds})
				e.MetricSamples = append(e.MetricSamples, BenchmarkMetricSampleResult{CandidateID: vc.candidate.ID, SampleIndex: window.Index, VMAF: &stats.Mean, VMAFStats: &stats})
			}
			// The same quality evaluation the pipeline runs per stage.
			if err := mergeAutomaticQuality(stage, e, vc); err != nil {
				return err
			}
		}
		return nil
	}}
	evidence := &BenchmarkExecutionEvidence{SourceBitDepth: 8}
	progress := newBenchmarkProgressReporter(nil, record, 100)
	rep := mediainspect.DetailedReport{DurationSec: 600, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 25}}}
	if err := runner.runAutomaticCandidates(context.Background(), nil, record, evidence, "", replayLadder(rules != nil), rep, sourceBytes, time.Time{}, progress); err != nil {
		t.Fatal(err)
	}
	res.decision = evidence.Decision
	if evidence.Decision.Winner != nil {
		res.winner = evidence.Decision.Winner.CandidateID
		if evidence.Automatic.VerifiedSamples != 5 {
			t.Fatal("winner without holdout verification")
		}
	}
	return res
}

func replayRules() *transcode.AutomaticSearchRules {
	return &transcode.AutomaticSearchRules{SelectionMarginVMAF: 1.5, HoldoutWindows: transcode.HoldoutWindowsFinalGridEdges, QualityFallbackPresets: []string{"medium", "slow"}}
}

// Discovery overstates a setting the holdouts then fail on quality. Without
// rules the second attempt goes to an even more compressed slow setting
// (from the savings family) and the search ends with nothing although
// medium CRF 22 and slow CRF 24 were never verified.
func TestReplaySafestUnverifiedSettingIsVerified(t *testing.T) {
	settings := map[string]replaySetting{
		replayKey("medium", 22): {bytes: 23500, discovery: replayScore{88.1, 89.3}, holdout: 90.5},
		replayKey("medium", 24): {bytes: 21000, discovery: replayScore{86.6, 89.0}, holdout: 84},
		replayKey("medium", 26): {bytes: 19000, discovery: replayScore{85.2, 88.4}, holdout: 84},
		replayKey("medium", 28): {bytes: 17000, discovery: replayScore{83.5, 88.1}, holdout: 83},
		replayKey("medium", 30): {bytes: 15500, discovery: replayScore{82.0, 88.0}, holdout: 82},
		replayKey("slow", 24):   {bytes: 20000, discovery: replayScore{87.5, 89.5}, holdout: 89.5},
		replayKey("slow", 27):   {bytes: 17500, discovery: replayScore{85.3, 88.6}, holdout: 83},
		replayKey("slow", 30):   {bytes: 15000, discovery: replayScore{83.0, 88.0}, holdout: 82},
	}
	old := runReplay(t, settings, 10_000_000, nil)
	t.Logf("without rules: %s", old.summary())
	if old.winner != "" || strings.Join(old.verified, ",") != "cand_crf26,fallback-savings-cand_crf27" {
		t.Fatalf("earlier rules no longer reproduce the recorded failure: %s", old.summary())
	}
	for _, id := range []string{"cand_crf22", "cand_crf24", "fallback-savings-cand_crf24"} {
		if got := old.reason(id); got != reasonNotVerified {
			t.Fatalf("never-verified %s labelled %q", id, got)
		}
	}

	res := runReplay(t, settings, 10_000_000, replayRules())
	t.Logf("with rules: %s", res.summary())
	if res.winner != "cand_crf22" || strings.Join(res.verified, ",") != "cand_crf24,cand_crf22" {
		t.Fatalf("safer setting not verified: %s", res.summary())
	}
	if res.slowEncodes != 0 {
		t.Fatalf("slow settings encoded although a safer measured setting remained: %v", res.measured)
	}
	if got := res.reason("cand_crf26"); got != reasonSelectionMarginNotMet {
		t.Fatalf("thin CRF 26 labelled %q", got)
	}
	if got := res.reason("cand_crf24"); !qualityRejection(got) {
		t.Fatalf("failed holdout labelled %q", got)
	}
}

// A setting that passes the p5 minimum by 0.99 is the smallest that
// qualifies. Without rules it wins; such thin winners tended to fail
// the full-file validation after a long encode. With rules it is not
// selectable: the quality fallback's medium preset finds a setting that clears
// the margin, and the slow preset is never encoded.
func TestReplayThinMarginLosesToSaferSetting(t *testing.T) {
	settings := map[string]replaySetting{
		replayKey("medium", 22): {bytes: 23500, discovery: replayScore{84.99, 89.0}, holdout: 88.5},
		replayKey("medium", 24): {bytes: 21000, discovery: replayScore{83.6, 88.6}, holdout: 84},
		replayKey("medium", 26): {bytes: 19000, discovery: replayScore{83.0, 88.2}, holdout: 84},
		replayKey("medium", 28): {bytes: 17000, discovery: replayScore{82.0, 88.0}, holdout: 83},
		replayKey("medium", 30): {bytes: 15500, discovery: replayScore{81.0, 88.0}, holdout: 82},
		replayKey("medium", 16): {bytes: 40000, discovery: replayScore{89.0, 92.0}, holdout: 92},
		replayKey("medium", 18): {bytes: 34000, discovery: replayScore{87.8, 91.0}, holdout: 91},
		replayKey("medium", 20): {bytes: 28000, discovery: replayScore{86.6, 90.0}, holdout: 89},
		replayKey("slow", 16):   {bytes: 37000, discovery: replayScore{89.5, 92.5}, holdout: 92},
		replayKey("slow", 18):   {bytes: 31000, discovery: replayScore{88.4, 91.5}, holdout: 91},
		replayKey("slow", 20):   {bytes: 26000, discovery: replayScore{87.2, 90.5}, holdout: 90},
	}
	old := runReplay(t, settings, 10_000_000, nil)
	t.Logf("without rules: %s", old.summary())
	if old.winner != "cand_crf22" {
		t.Fatalf("earlier rules no longer reproduce the thin winner: %s", old.summary())
	}
	res := runReplay(t, settings, 10_000_000, replayRules())
	t.Logf("with rules: %s", res.summary())
	if res.winner != "fallback-quality-medium-cand_crf20" || res.slowEncodes != 0 {
		t.Fatalf("thin setting not replaced by a safer medium one: %s", res.summary())
	}
	if got := res.reason("cand_crf22"); got != reasonSelectionMarginNotMet {
		t.Fatalf("thin CRF 22 labelled %q", got)
	}
}

// Slow stays available: it is measured only after every medium setting,
// the quality fallback's included, failed.
func TestReplaySlowQualityFallbackOnlyAfterMedium(t *testing.T) {
	settings := map[string]replaySetting{
		replayKey("medium", 22): {bytes: 23500, discovery: replayScore{82, 88.5}, holdout: 84},
		replayKey("medium", 16): {bytes: 40000, discovery: replayScore{84.5, 90.0}, holdout: 88},
		replayKey("medium", 18): {bytes: 34000, discovery: replayScore{84.2, 89.5}, holdout: 88},
		replayKey("medium", 20): {bytes: 28000, discovery: replayScore{83.0, 89.0}, holdout: 88},
		replayKey("slow", 16):   {bytes: 37000, discovery: replayScore{88.5, 92.0}, holdout: 91},
		replayKey("slow", 18):   {bytes: 31000, discovery: replayScore{87.6, 91.0}, holdout: 90},
		replayKey("slow", 20):   {bytes: 26000, discovery: replayScore{85.0, 90.0}, holdout: 90},
	}
	res := runReplay(t, settings, 10_000_000, replayRules())
	t.Logf("with rules: %s", res.summary())
	if res.winner != "fallback-quality-cand_crf18" {
		t.Fatalf("slow fallback not used after medium failed: %s", res.summary())
	}
	firstSlow, lastMedium := -1, -1
	for i, id := range res.measured {
		if strings.HasPrefix(id, "fallback-quality-medium-") {
			lastMedium = i
		} else if strings.HasPrefix(id, "fallback-quality-") && firstSlow < 0 {
			firstSlow = i
		}
	}
	if lastMedium < 0 || firstSlow < lastMedium {
		t.Fatalf("slow measured before medium: %v", res.measured)
	}
}

// A holdout that fails only on savings keeps the savings fallback.
func TestReplaySavingsOnlyHoldoutFailureKeepsSavingsFallback(t *testing.T) {
	settings := map[string]replaySetting{
		replayKey("medium", 22): {bytes: 160000, discovery: replayScore{90, 93}, holdout: 92},
		replayKey("medium", 24): {bytes: 150000, discovery: replayScore{89, 92}, holdout: 91},
		replayKey("medium", 26): {bytes: 145000, discovery: replayScore{88, 91}, holdout: 90},
		replayKey("medium", 28): {bytes: 142000, discovery: replayScore{87.5, 90.5}, holdout: 90},
		replayKey("medium", 30): {bytes: 135000, discovery: replayScore{87, 90}, holdout: 90, holdoutBytes: 170000},
		replayKey("slow", 24):   {bytes: 136000, discovery: replayScore{88.5, 91.5}, holdout: 91},
		replayKey("slow", 27):   {bytes: 128000, discovery: replayScore{87.5, 90.5}, holdout: 90},
		replayKey("slow", 30):   {bytes: 120000, discovery: replayScore{86.5, 89.5}, holdout: 89.5},
	}
	for _, rules := range []*transcode.AutomaticSearchRules{nil, replayRules()} {
		res := runReplay(t, settings, 10_000_000, rules)
		t.Logf("rules=%v: %s", rules != nil, res.summary())
		if got := res.reason("cand_crf30"); got != "insufficient_savings" {
			t.Fatalf("rules=%v: CRF 30 holdout failed with %q, want a savings failure: %s", rules != nil, got, res.summary())
		}
		if !strings.HasPrefix(res.winner, "fallback-savings-") {
			t.Fatalf("rules=%v: savings fallback not kept: %s", rules != nil, res.summary())
		}
		for _, id := range res.measured {
			if strings.HasPrefix(id, "fallback-quality-") {
				t.Fatalf("rules=%v: quality fallback measured after a savings failure: %v", rules != nil, res.measured)
			}
		}
	}
}

// After a quality failure the retry goes to the safest eligible setting of the
// family, not the next-smallest. Two settings failed holdout; a safer one that
// passed discovery with a wide margin was never checked before.
func TestReplayRetryGoesToSafestEligibleSetting(t *testing.T) {
	t.Run("initial ladder", func(t *testing.T) {
		settings := map[string]replaySetting{
			replayKey("medium", 22): {bytes: 23500, discovery: replayScore{89.5, 91.3}, holdout: 90},
			replayKey("medium", 24): {bytes: 21000, discovery: replayScore{87.5, 90.0}, holdout: 85},
			replayKey("medium", 26): {bytes: 19000, discovery: replayScore{86.5, 89.0}, holdout: 83},
			replayKey("medium", 28): {bytes: 17000, discovery: replayScore{83.5, 88.0}, holdout: 83},
			replayKey("medium", 30): {bytes: 15500, discovery: replayScore{82.0, 88.0}, holdout: 82},
		}
		res := runReplay(t, settings, 10_000_000, replayRules())
		t.Logf("with rules: %s", res.summary())
		if res.winner != "cand_crf22" || strings.Join(res.verified, ",") != "cand_crf26,cand_crf22" {
			t.Fatalf("safest setting not verified second: %s", res.summary())
		}
		if got := res.reason("cand_crf26"); !qualityRejection(got) {
			t.Fatalf("failed holdout labelled %q", got)
		}
		if got := res.reason("cand_crf24"); got != reasonNotVerified {
			t.Fatalf("skipped CRF 24 labelled %q", got)
		}
	})
	t.Run("quality fallback", func(t *testing.T) {
		settings := map[string]replaySetting{
			replayKey("medium", 22): {bytes: 23500, discovery: replayScore{82, 88.5}, holdout: 84},
			replayKey("medium", 16): {bytes: 40000, discovery: replayScore{92.0, 94.8}, holdout: 93},
			replayKey("medium", 18): {bytes: 34000, discovery: replayScore{88.0, 91.0}, holdout: 85},
			replayKey("medium", 20): {bytes: 28000, discovery: replayScore{86.5, 89.5}, holdout: 83},
		}
		res := runReplay(t, settings, 10_000_000, replayRules())
		t.Logf("with rules: %s", res.summary())
		if res.winner != "fallback-quality-medium-cand_crf16" || strings.Join(res.verified, ",") != "fallback-quality-medium-cand_crf20,fallback-quality-medium-cand_crf16" {
			t.Fatalf("safest fallback setting not verified second: %s", res.summary())
		}
		if res.slowEncodes != 0 {
			t.Fatalf("slow settings encoded: %v", res.measured)
		}
		if got := res.reason("fallback-quality-medium-cand_crf18"); got != reasonNotVerified {
			t.Fatalf("skipped CRF 18 labelled %q", got)
		}
	})
	t.Run("next smallest is the safest", func(t *testing.T) {
		// CRF 24 failed discovery, so after CRF 26 failed holdout the only
		// eligible setting left is CRF 22, both next-smallest and safest.
		settings := map[string]replaySetting{
			replayKey("medium", 22): {bytes: 23500, discovery: replayScore{89.0, 91.0}, holdout: 90},
			replayKey("medium", 24): {bytes: 21000, discovery: replayScore{83.5, 88.0}, holdout: 89},
			replayKey("medium", 26): {bytes: 19000, discovery: replayScore{86.5, 89.0}, holdout: 83},
			replayKey("medium", 28): {bytes: 17000, discovery: replayScore{83.5, 88.0}, holdout: 83},
			replayKey("medium", 30): {bytes: 15500, discovery: replayScore{82.0, 88.0}, holdout: 82},
		}
		res := runReplay(t, settings, 10_000_000, replayRules())
		t.Logf("with rules: %s", res.summary())
		if res.winner != "cand_crf22" || strings.Join(res.verified, ",") != "cand_crf26,cand_crf22" {
			t.Fatalf("only remaining setting not verified: %s", res.summary())
		}
		if got := res.reason("cand_crf24"); got == reasonNotVerified || got == "" {
			t.Fatalf("CRF 24 discovery failure labelled %q", got)
		}
	})
}

func TestSafestEligibleID(t *testing.T) {
	vc := func(id, codec string, crf int) validatedCandidate {
		return validatedCandidate{candidate: transcode.BenchmarkCandidate{ID: id, VideoCodec: codec, Quality: crf, Preset: "medium"}, bitDepth: 8}
	}
	decision := func(ids ...string) *transcode.BenchmarkDecision {
		d := &transcode.BenchmarkDecision{}
		for _, id := range ids {
			d.Evaluations = append(d.Evaluations, transcode.BenchmarkCandidateEvaluation{CandidateID: id, Eligible: true})
		}
		return d
	}
	x265 := []validatedCandidate{vc("m26", "libx265", 26), vc("m24", "libx265", 24), vc("m22", "libx265", 22), vc("m20", "libx265", 20)}
	vt := []validatedCandidate{vc("vt60", "hevc_videotoolbox", 60), vc("vt70", "hevc_videotoolbox", 70)}
	for _, tc := range []struct {
		name     string
		tested   []validatedCandidate
		decision *transcode.BenchmarkDecision
		selected string
		want     string
	}{
		{"lowest eligible CRF", x265, decision("m26", "m24", "m22"), "m26", "m22"},
		{"ineligible safer setting is skipped", x265, decision("m26", "m24"), "m26", "m24"},
		{"only the selected one", x265, decision("m26"), "m26", "m26"},
		{"VideoToolbox higher is safer", vt, decision("vt60", "vt70"), "vt60", "vt70"},
		{"no decision", x265, nil, "m26", "m26"},
	} {
		if got := safestEligibleID(tc.tested, tc.decision, tc.selected); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestRuledOutByQualityFailure(t *testing.T) {
	vc := func(id, preset string, crf int) validatedCandidate {
		return validatedCandidate{candidate: transcode.BenchmarkCandidate{ID: id, VideoCodec: "libx265", Quality: crf, Preset: preset}}
	}
	failed := []validatedCandidate{vc("m24", "medium", 24)}
	for _, tc := range []struct {
		vc   validatedCandidate
		want bool
	}{
		{vc("m24", "medium", 24), true}, {vc("m26", "medium", 26), true}, {vc("m22", "medium", 22), false},
		{vc("s24", "slow", 24), true}, {vc("s27", "slow", 27), true}, {vc("s20", "slow", 20), false},
		{validatedCandidate{candidate: transcode.BenchmarkCandidate{ID: "vt70", VideoCodec: "hevc_videotoolbox", Quality: 70}}, false},
	} {
		if got := ruledOutByQualityFailure(tc.vc, failed); got != tc.want {
			t.Errorf("%s ruled out = %v, want %v", tc.vc.candidate.ID, got, tc.want)
		}
	}
}

func TestAutomaticFallbackFamily(t *testing.T) {
	met := &transcode.BenchmarkDecision{Evaluations: []transcode.BenchmarkCandidateEvaluation{{CandidateID: "a", MinimumMet: true}}}
	savings := &transcode.BenchmarkDecision{Evaluations: []transcode.BenchmarkCandidateEvaluation{{CandidateID: "a", EvaluationReason: "insufficient_savings"}}}
	thin := &transcode.BenchmarkDecision{Evaluations: []transcode.BenchmarkCandidateEvaluation{{CandidateID: "a", EvaluationReason: reasonSelectionMarginNotMet}}}
	for _, tc := range []struct {
		d       *transcode.BenchmarkDecision
		quality bool
		want    string
	}{{met, false, fallbackSavingsPrefix}, {savings, false, fallbackSavingsPrefix}, {thin, false, fallbackQualityPrefix}, {nil, false, fallbackQualityPrefix}, {met, true, fallbackQualityPrefix}} {
		if got := automaticFallback(tc.d, tc.quality); got != tc.want {
			t.Errorf("automaticFallback(%+v, %v) = %s, want %s", tc.d, tc.quality, got, tc.want)
		}
	}
}
