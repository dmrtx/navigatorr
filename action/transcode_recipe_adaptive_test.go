package action

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// failedP5 is a refusal on the p5 check, the given points under its limit.
func failedP5(c *budgetCase, shortfall float64) transcode.JobStatus {
	return failedRung(95, *stateQualityLimits(c.ec.State).VMAFP5-shortfall)
}

// A narrow miss on the first x265 rung tries a setting in between the first
// two, as close to the failed rung as the shortfall allows.
func TestQualityLadderInsertsRungAfterNarrowMiss(t *testing.T) {
	size := map[string]any{"allow_hardware": false}
	for name, tc := range map[string]struct {
		inputs    map[string]any
		shortfall float64
		values    []int
	}{
		// The first two rungs are two CRF steps apart, so CRF 23 is the only
		// setting in between: a shortfall of a point or less places it, a larger
		// one would need a step that does not fit and adds nothing.
		"size, a hair short":  {size, 0.4, []int{24, 23, 22, 19}},
		"size, a point short": {size, 1.0, []int{24, 23, 22, 19}},
		"size, over a point":  {size, 1.2, []int{24, 22, 19}},
		"size, 2.5 short":     {size, 2.5, []int{24, 22, 19}},
		"size, too far":       {size, 2.6, []int{24, 22, 19}},
		"size, far short":     {size, 8, []int{24, 22, 19}},
		"slow preset":         {map[string]any{"allow_hardware": false, "allow_slow_preset": true}, 1, []int{24, 23, 22, 19}},
		// Only a size goal trades an extra encode for bytes.
		"quality":       {map[string]any{"mode": "quality"}, 1, []int{20, 17, 14}},
		"x265_preserve": {map[string]any{"mode": "x265_preserve"}, 1, []int{24, 21, 18}},
		"compact":       {map[string]any{"mode": "compact"}, 1, []int{18, 16, 14}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newRecipeCase(t, tc.inputs, nil)
			planRecipe(t, c)
			res := advanceRung(t, c, failedP5(c, tc.shortfall))
			rec := QualityLadderOf(c.ec.State)
			plan := getPlan(c.ec.State["plan"])
			if res.RewindTo != "submit_transcode" || rec.Rung != 1 || !slices.Equal(rec.Values, tc.values) || plan.Quality != tc.values[1] {
				t.Fatalf("%+v %+v plan quality %d", res, rec, plan.Quality)
			}
			if !strings.Contains(rec.Message, "missed the quality limits") || !strings.Contains(rec.Message, "(step 2 of "+string(rune('0'+len(tc.values)))+")") {
				t.Fatalf("message %q", rec.Message)
			}
			if !qualityLadderPlanMatches(c.snapshot, c.ec.State, plan, rec) {
				t.Fatal("the rung does not rebuild from the record")
			}
			// The frozen recipe, and so the snapshot's digest, never moves.
			snapshot, err := freezeModePolicy(c.ec)
			if err != nil || snapshot.Digest != c.snapshot.Digest || len(snapshot.Recipe.QualityValues) != 3 {
				t.Fatalf("snapshot %+v %v", snapshot, err)
			}
		})
	}
}

// A window below the minimum is measured on the worst scored window: the
// pooled scores say nothing about it. The fresh windows count only on a plan
// that gated them; on one frozen before the gate they only reported.
func TestQualityLadderShortfallOfSampleCheck(t *testing.T) {
	window := func(role string, mean float64) transcode.FinalWindowScore {
		return transcode.FinalWindowScore{Role: role, VMAFMean: mean}
	}
	for name, tc := range map[string]struct {
		windows        []transcode.FinalWindowScore
		ungated, gated []int
	}{
		"just under":      {[]transcode.FinalWindowScore{window("grid", 92), window("grid", 87.5), window("fresh", 70)}, []int{24, 23, 22, 19}, []int{24, 22, 19}},
		"further under":   {[]transcode.FinalWindowScore{window("grid", 92), window("grid", 86.6)}, []int{24, 22, 19}, []int{24, 22, 19}},
		"far under":       {[]transcode.FinalWindowScore{window("grid", 80)}, []int{24, 22, 19}, []int{24, 22, 19}},
		"no windows":      {nil, []int{24, 22, 19}, []int{24, 22, 19}},
		"only fresh ones": {[]transcode.FinalWindowScore{window("fresh", 87)}, []int{24, 22, 19}, []int{24, 23, 22, 19}},
	} {
		for _, gated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, gated %v", name, gated), func(t *testing.T) {
				c := newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
				planRecipe(t, c)
				plan := getPlan(c.ec.State["plan"])
				if !planGatesFreshSamples(plan) {
					t.Fatalf("a newly frozen size goal planned ungated fresh windows: %+v", plan.QualityValidation)
				}
				// A plan frozen before the gate (its digest is not checked here).
				plan.QualityValidation.GateFreshSamples = gated
				c.ec.State["plan"] = plan
				want := tc.ungated
				if gated {
					want = tc.gated
				}
				advanceRung(t, c, transcode.JobStatus{Status: transcode.StatusFailed, ReasonCode: reasonQualityNotMet, QualityEvidence: &transcode.FinalQualityEvidence{
					Verdict: "fail", ReasonCodes: []string{"quality_vmaf_sample_below_minimum"}, VMAF: &quality.VMAFStats{Mean: 90.63, P5: 84.33, WorstWindowMean: 85.1}, Windows: tc.windows}})
				if rec := QualityLadderOf(c.ec.State); !slices.Equal(rec.Values, want) || rec.Rung != 1 {
					t.Fatalf("%+v", rec)
				}
			})
		}
	}
}

func TestQualityLadderShortfall(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	limits := &QualityLimits{VMAFMean: f(88), VMAFP5: f(84), VMAFWorstWindow: f(85), CAMBIMean: f(6), CAMBIPeak: f(16)}
	scores := QualityNumbers{VMAFMean: f(87), VMAFP5: f(83.5), VMAFWorstWindow: f(83)}
	for name, tc := range map[string]struct {
		checks []string
		scores QualityNumbers
		limits *QualityLimits
		want   float64 // 0: no shortfall to go on
	}{
		"mean":                 {[]string{"quality_vmaf_mean_below_minimum"}, scores, limits, 1},
		"legacy mean":          {[]string{"below_minimum_quality"}, scores, limits, 1},
		"p5":                   {[]string{"quality_vmaf_p5_below_minimum"}, scores, limits, 0.5},
		"worst window":         {[]string{"quality_vmaf_worst_window_below_minimum"}, scores, limits, 2},
		"the worst of several": {[]string{"quality_vmaf_p5_below_minimum", "quality_vmaf_worst_window_below_minimum", "quality_vmaf_mean_below_minimum"}, scores, limits, 2},
		// Checks not scored in VMAF points cannot place a rung, even beside
		// one that can.
		"CAMBI":              {[]string{"quality_cambi_mean_above_maximum"}, scores, limits, 0},
		"CAMBI beside p5":    {[]string{"quality_vmaf_p5_below_minimum", "quality_cambi_peak_above_maximum"}, scores, limits, 0},
		"frames below":       {[]string{"quality_vmaf_frames_below_limit_exceeded"}, scores, limits, 0},
		"SSIM":               {[]string{"quality_ssim_mean_below_minimum"}, scores, limits, 0},
		"no failed check":    {nil, scores, limits, 0},
		"no limits":          {[]string{"quality_vmaf_p5_below_minimum"}, scores, nil, 0},
		"no score":           {[]string{"quality_vmaf_p5_below_minimum"}, QualityNumbers{}, limits, 0},
		"score on the limit": {[]string{"quality_vmaf_p5_below_minimum"}, QualityNumbers{VMAFP5: f(84)}, limits, 0},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := qualityLadderShortfall(&FinalQualitySummary{FailedChecks: tc.checks, Measured: tc.scores, Limits: tc.limits}, nil, false)
			if ok != (tc.want != 0) || got != tc.want {
				t.Fatalf("got %v %v, want %v", got, ok, tc.want)
			}
		})
	}
	if _, ok := qualityLadderShortfall(nil, nil, false); ok {
		t.Fatal("a rung with no scores has a shortfall")
	}
}

// The inserted rung is a step of the ladder like any other: it can pass, and
// when it fails the next planned rung follows, with no second insertion.
func TestQualityLadderInsertedRungOutcomes(t *testing.T) {
	start := func(t *testing.T) *budgetCase {
		c := newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
		planRecipe(t, c)
		advanceRung(t, c, failedP5(c, 0.8))
		return c
	}
	t.Run("passes", func(t *testing.T) {
		c := start(t)
		rec := passQualityLadder(c.ec, 30_000_000_000)
		if rec.Outcome != QualityLadderPassed || !strings.Contains(rec.Message, "x265 CRF 23") || !strings.Contains(rec.Message, "(step 2 of 4)") {
			t.Fatalf("%+v", rec)
		}
	})
	t.Run("narrow miss goes to the next planned rung", func(t *testing.T) {
		c := start(t)
		res := advanceRung(t, c, failedP5(c, 0.5))
		rec := QualityLadderOf(c.ec.State)
		plan := getPlan(c.ec.State["plan"])
		if res.RewindTo != "submit_transcode" || rec.Rung != 2 || !slices.Equal(rec.Values, []int{24, 23, 22, 19}) || plan.Quality != 22 {
			t.Fatalf("%+v %+v plan quality %d", res, rec, plan.Quality)
		}
		if !strings.Contains(rec.Message, "(step 3 of 4)") || !qualityLadderPlanMatches(c.snapshot, c.ec.State, plan, rec) {
			t.Fatalf("message %q", rec.Message)
		}
		if got := c.ec.State["job_id"]; got != "job-"+c.ec.InstanceID+"-recipe-r2" {
			t.Fatalf("job %v", got)
		}
		advanceRung(t, c, failedP5(c, 0.2))
		if rec = QualityLadderOf(c.ec.State); rec.Rung != 3 || getPlan(c.ec.State["plan"]).Quality != 19 {
			t.Fatalf("%+v", rec)
		}
		res = advanceRung(t, c, failedP5(c, 0.1))
		if rec = QualityLadderOf(c.ec.State); res.RewindTo != "" || rec.Outcome != reasonQualityNotMet || len(rec.Tried) != 4 {
			t.Fatalf("%+v %+v", res, rec)
		}
	})
	t.Run("is rejected for its size", func(t *testing.T) {
		c := start(t)
		res := advanceRung(t, c, transcode.JobStatus{Status: transcode.StatusFailed, ReasonCode: "insufficient_savings", CandidateSizeBytes: 9_000_000_000})
		rec := QualityLadderOf(c.ec.State)
		if res.RewindTo != "" || rec.Outcome != "insufficient_savings" || rec.Rung != 1 || !strings.Contains(rec.Message, "x265 CRF 23") {
			t.Fatalf("%+v %+v", res, rec)
		}
	})
	t.Run("passes the validate step", func(t *testing.T) {
		c := start(t)
		validateRecipeCase(t, c, 50)
		res, err := c.engine.stepTranscodeValidate(context.Background(), c.ec)
		rec := QualityLadderOf(c.ec.State)
		if err != nil || res.Status != StepCompleted || rec.Outcome != QualityLadderPassed || !strings.Contains(rec.Message, "x265 CRF 23 passed") || !strings.Contains(rec.Message, "(step 2 of 4)") {
			t.Fatalf("%+v %v %+v", res, err, rec)
		}
	})
	t.Run("fails the validate step with another CRF", func(t *testing.T) {
		c := start(t)
		plan := *getPlan(c.ec.State["plan"])
		plan.Quality = 21
		plan.PlanDigest = ""
		plan.PlanDigest, _ = transcode.DigestPlan(&plan)
		c.ec.State["plan"] = &plan
		validateRecipeCase(t, c, 50)
		if res, _ := c.engine.stepTranscodeValidate(context.Background(), c.ec); res.Status != StepFailed || !strings.Contains(res.Error, "frozen recipe") {
			t.Fatalf("%+v", res)
		}
	})
}

// An added rung never costs the ladder a frozen rung it would have encoded:
// the trend reads only frozen rungs, so the decisions match a ladder without it.
func TestQualityLadderTrendSkipsInsertedRung(t *testing.T) {
	// Only a shortfall of a point or less adds CRF 23, so the first rung's miss
	// is narrow whatever the inserted rung then scores.
	for name, tc := range map[string]struct{ first, inserted float64 }{
		"barely better": {1.0, 0.8},
		"flat":          {1.0, 1.0},
		"worse":         {0.5, 3},
		"much worse":    {0.5, 6},
	} {
		t.Run(name, func(t *testing.T) {
			c := newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
			planRecipe(t, c)
			advanceRung(t, c, failedP5(c, tc.first))
			res := advanceRung(t, c, failedP5(c, tc.inserted))
			rec := QualityLadderOf(c.ec.State)
			if !slices.Equal(rec.Values, []int{24, 23, 22, 19}) || res.RewindTo != "submit_transcode" || rec.Rung != 2 || getPlan(c.ec.State["plan"]).Quality != 22 || rec.Outcome != QualityLadderEncoding {
				t.Fatalf("%+v %+v", res, rec)
			}
		})
	}
	limit := func(c *budgetCase) float64 { return *stateQualityLimits(c.ec.State).VMAFP5 }
	// Without the added rung, CRF 24 and 22 decide CRF 19.
	c := newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
	planRecipe(t, c)
	advanceRung(t, c, failedRung(95, limit(c)-2.5))
	res := advanceRung(t, c, failedRung(95, limit(c)-2.3))
	rec := QualityLadderOf(c.ec.State)
	if res.RewindTo != "" || rec.Outcome != reasonQualityNotMet || rec.Rung != 1 || len(rec.Tried) != 2 {
		t.Fatalf("%+v %+v", res, rec)
	}
	for _, want := range []string{"stops before CRF 19", "from 81.5 at x265 CRF 24 to 81.7 at CRF 22", "about 81.9 at CRF 19"} {
		if !strings.Contains(rec.Message, want) {
			t.Fatalf("message %q lacks %q", rec.Message, want)
		}
	}
	// With CRF 23 added, CRF 24 and 22 still decide CRF 19: here CRF 22 is
	// worse than CRF 24, whatever CRF 23 scored.
	c = newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
	planRecipe(t, c)
	advanceRung(t, c, failedRung(95, limit(c)-1))
	advanceRung(t, c, failedRung(95, limit(c)-0.1))
	res = advanceRung(t, c, failedRung(95, limit(c)-2))
	rec = QualityLadderOf(c.ec.State)
	if res.RewindTo != "" || rec.Outcome != reasonQualityNotMet || rec.Rung != 2 || len(rec.Tried) != 3 {
		t.Fatalf("%+v %+v", res, rec)
	}
	for _, want := range []string{"stops before CRF 19", "got worse, from 83.0 at x265 CRF 24 to 82.0 at CRF 22"} {
		if !strings.Contains(rec.Message, want) {
			t.Fatalf("message %q lacks %q", rec.Message, want)
		}
	}
	// A trend on course goes on, as it does without the added rung.
	c = newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
	planRecipe(t, c)
	advanceRung(t, c, failedRung(95, limit(c)-2.5))
	res = advanceRung(t, c, failedRung(95, limit(c)-1))
	if rec = QualityLadderOf(c.ec.State); res.RewindTo != "submit_transcode" || rec.Rung != 2 || getPlan(c.ec.State["plan"]).Quality != 19 {
		t.Fatalf("%+v %+v", res, rec)
	}
	c = newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
	planRecipe(t, c)
	advanceRung(t, c, failedRung(95, limit(c)-1))
	advanceRung(t, c, failedRung(95, limit(c)-0.5))
	res = advanceRung(t, c, failedRung(95, limit(c)-0.4))
	if rec = QualityLadderOf(c.ec.State); res.RewindTo != "submit_transcode" || rec.Rung != 3 || getPlan(c.ec.State["plan"]).Quality != 19 {
		t.Fatalf("%+v %+v", res, rec)
	}
}

// A VideoToolbox ladder keeps its frozen rungs; its x265 fallback is an x265
// ladder like any other.
func TestQualityLadderInsertionEncoders(t *testing.T) {
	c := newRecipeCase(t, nil, nil)
	planRecipe(t, c)
	advanceRung(t, c, failedP5(c, 0.5))
	if rec := QualityLadderOf(c.ec.State); !slices.Equal(rec.Values, []int{60, 65, 70}) || getPlan(c.ec.State["plan"]).Quality != 65 {
		t.Fatalf("VideoToolbox: %+v", rec)
	}
	c = newRecipeCase(t, nil, func(caps *transcode.WorkerCapabilities) { delete(caps.Encoders, transcode.VideoCodecHEVCVideoToolbox) })
	planRecipe(t, c)
	advanceRung(t, c, failedP5(c, 0.5))
	rec := QualityLadderOf(c.ec.State)
	plan := getPlan(c.ec.State["plan"])
	if rec.Choice == nil || !slices.Equal(rec.Values, []int{24, 23, 22, 19}) || plan.Quality != 23 || !qualityLadderPlanMatches(c.snapshot, c.ec.State, plan, rec) {
		t.Fatalf("fallback: %+v plan %d", rec, plan.Quality)
	}
}

// A refusal for size, or a miss without scores, inserts nothing.
func TestQualityLadderInsertionNeedsANarrowQualityMiss(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"mode": "quality"}, nil)
	planRecipe(t, c)
	advanceRung(t, c, transcode.JobStatus{Status: transcode.StatusFailed, ReasonCode: "insufficient_savings", CandidateSizeBytes: 9_000_000_000})
	if rec := QualityLadderOf(c.ec.State); len(rec.Values) != 3 || rec.Rung != 0 {
		t.Fatalf("size: %+v", rec)
	}
	c = newRecipeCase(t, map[string]any{"mode": "quality"}, nil)
	planRecipe(t, c)
	advanceRung(t, c, transcode.JobStatus{Status: transcode.StatusFailed, ReasonCode: reasonQualityNotMet})
	if rec := QualityLadderOf(c.ec.State); len(rec.Values) != 3 || rec.Rung != 0 {
		t.Fatalf("no scores: %+v", rec)
	}
}

// Validation rebuilds a rung from the record, so the record's values may only
// be the frozen ones or those plus a value the ladder could have inserted.
func TestLadderValuesMatch(t *testing.T) {
	x265 := &ModeRecipe{VideoCodec: transcode.VideoCodecLibX265, QualityValues: []int{22, 19, 16}}
	// A recipe frozen before the ladder moved to 24/22/19 stays valid.
	x265New := &ModeRecipe{VideoCodec: transcode.VideoCodecLibX265, QualityValues: []int{24, 22, 19}}
	quality := &ModeRecipe{VideoCodec: transcode.VideoCodecLibX265, QualityValues: []int{20, 17, 14}}
	vt := &ModeRecipe{VideoCodec: transcode.VideoCodecHEVCVideoToolbox, QualityValues: []int{60, 65, 70}}
	for name, tc := range map[string]struct {
		mode   string
		r      *ModeRecipe
		values []int
		want   bool
	}{
		"frozen":                    {"size", x265, []int{22, 19, 16}, true},
		"inserted 21":               {"size", x265, []int{22, 21, 19, 16}, true},
		"inserted 20":               {"size", x265, []int{22, 20, 19, 16}, true},
		"current frozen":            {"size", x265New, []int{24, 22, 19}, true},
		"current inserted 23":       {"size", x265New, []int{24, 23, 22, 19}, true},
		"current, no room below":    {"size", x265New, []int{24, 21, 22, 19}, false},
		"current, above the first":  {"size", x265New, []int{24, 25, 22, 19}, false},
		"current, old ladder":       {"size", x265New, []int{22, 19, 16}, false},
		"quality frozen":            {"quality", quality, []int{20, 17, 14}, true},
		"quality inserted":          {"quality", quality, []int{20, 19, 17, 14}, false},
		"VideoToolbox frozen":       {"size", vt, []int{60, 65, 70}, true},
		"VideoToolbox inserted":     {"size", vt, []int{60, 62, 65, 70}, false},
		"empty":                     {"size", x265, nil, false},
		"a rung missing":            {"size", x265, []int{22, 19}, false},
		"repeats the first":         {"size", x265, []int{22, 22, 19, 16}, false},
		"repeats the second":        {"size", x265, []int{22, 19, 19, 16}, false},
		"above the first":           {"size", x265, []int{22, 23, 19, 16}, false},
		"below the second":          {"size", x265, []int{22, 18, 19, 16}, false},
		"between later rungs":       {"size", x265, []int{22, 19, 18, 16}, false},
		"two inserted":              {"size", x265, []int{22, 21, 20, 19, 16}, false},
		"first rung changed":        {"size", x265, []int{21, 20, 19, 16}, false},
		"last rung changed":         {"size", x265, []int{22, 20, 19, 15}, false},
		"same length, other values": {"size", x265, []int{22, 20, 16}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ladderValuesMatch(tc.mode, tc.values, tc.r); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

// A record or plan that drifted from each other fails closed.
func TestQualityLadderPlanMatchesInsertedRung(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
	planRecipe(t, c)
	advanceRung(t, c, failedP5(c, 0.8))
	rec := QualityLadderOf(c.ec.State)
	plan := getPlan(c.ec.State["plan"])
	for name, mutate := range map[string]func(*QualityLadderRecord){
		"another rung":      func(r *QualityLadderRecord) { r.Rung = 2 },
		"rung out of range": func(r *QualityLadderRecord) { r.Rung = 4 },
		"negative rung":     func(r *QualityLadderRecord) { r.Rung = -1 },
		"inserted changed":  func(r *QualityLadderRecord) { r.Values = []int{22, 21, 19, 16} },
		"values invalid":    func(r *QualityLadderRecord) { r.Values = []int{24, 21, 22, 19} },
		"frozen values":     func(r *QualityLadderRecord) { r.Values, r.Rung = []int{24, 22, 19}, 1 },
	} {
		tampered := *rec
		tampered.Values = slices.Clone(rec.Values)
		mutate(&tampered)
		if qualityLadderPlanMatches(c.snapshot, c.ec.State, plan, &tampered) {
			t.Errorf("%s accepted", name)
		}
	}
	// The engine refuses to advance a record with values it could not have made.
	tampered := *rec
	tampered.Values = []int{24, 21, 22, 19}
	c.ec.State[qualityLadderKey] = &tampered
	res, handled := c.engine.advanceQualityLadder(context.Background(), c.ec, failedP5(c, 0.5))
	if !handled || res.Status != StepFailed {
		t.Fatalf("%+v %v", res, handled)
	}
}
