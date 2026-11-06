package action

import (
	"context"
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
		"size, a hair short":      {size, 0.4, []int{22, 21, 19, 16}},
		"size, a point short":     {size, 1.0, []int{22, 21, 19, 16}},
		"size, over a point":      {size, 1.2, []int{22, 20, 19, 16}},
		"size, 2.5 short":         {size, 2.5, []int{22, 20, 19, 16}},
		"size, too far":           {size, 2.6, []int{22, 19, 16}},
		"size, far short":         {size, 8, []int{22, 19, 16}},
		"quality":                 {map[string]any{"mode": "quality"}, 1, []int{20, 19, 17, 14}},
		"x265_preserve":           {map[string]any{"mode": "x265_preserve"}, 2, []int{24, 22, 21, 18}},
		"compact, one value fits": {map[string]any{"mode": "compact"}, 1, []int{18, 17, 16, 14}},
		"compact, the anchor":     {map[string]any{"mode": "compact"}, 1.2, []int{18, 16, 14}},
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
// pooled scores and the fresh windows say nothing about it.
func TestQualityLadderShortfallOfSampleCheck(t *testing.T) {
	window := func(role string, mean float64) transcode.FinalWindowScore {
		return transcode.FinalWindowScore{Role: role, VMAFMean: mean}
	}
	for name, tc := range map[string]struct {
		windows []transcode.FinalWindowScore
		values  []int
	}{
		"just under":      {[]transcode.FinalWindowScore{window("grid", 92), window("grid", 87.5), window("fresh", 70)}, []int{22, 21, 19, 16}},
		"further under":   {[]transcode.FinalWindowScore{window("grid", 92), window("grid", 86.6)}, []int{22, 20, 19, 16}},
		"far under":       {[]transcode.FinalWindowScore{window("grid", 80)}, []int{22, 19, 16}},
		"no windows":      {nil, []int{22, 19, 16}},
		"only fresh ones": {[]transcode.FinalWindowScore{window("fresh", 87)}, []int{22, 19, 16}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
			planRecipe(t, c)
			advanceRung(t, c, transcode.JobStatus{Status: transcode.StatusFailed, ReasonCode: reasonQualityNotMet, QualityEvidence: &transcode.FinalQualityEvidence{
				Verdict: "fail", ReasonCodes: []string{"quality_vmaf_sample_below_minimum"}, VMAF: &quality.VMAFStats{Mean: 90.63, P5: 84.33, WorstWindowMean: 85.1}, Windows: tc.windows}})
			if rec := QualityLadderOf(c.ec.State); !slices.Equal(rec.Values, tc.values) || rec.Rung != 1 {
				t.Fatalf("%+v", rec)
			}
		})
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
			got, ok := qualityLadderShortfall(&FinalQualitySummary{FailedChecks: tc.checks, Measured: tc.scores, Limits: tc.limits}, nil)
			if ok != (tc.want != 0) || got != tc.want {
				t.Fatalf("got %v %v, want %v", got, ok, tc.want)
			}
		})
	}
	if _, ok := qualityLadderShortfall(nil, nil); ok {
		t.Fatal("a rung with no scores has a shortfall")
	}
}

// The inserted rung is a step of the ladder like any other: it can pass, and
// when it fails the next planned rung follows, with no second insertion.
func TestQualityLadderInsertedRungOutcomes(t *testing.T) {
	start := func(t *testing.T) *budgetCase {
		c := newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
		planRecipe(t, c)
		advanceRung(t, c, failedP5(c, 1.5))
		return c
	}
	t.Run("passes", func(t *testing.T) {
		c := start(t)
		rec := passQualityLadder(c.ec, 30_000_000_000)
		if rec.Outcome != QualityLadderPassed || !strings.Contains(rec.Message, "x265 CRF 20") || !strings.Contains(rec.Message, "(step 2 of 4)") {
			t.Fatalf("%+v", rec)
		}
	})
	t.Run("narrow miss goes to the next planned rung", func(t *testing.T) {
		c := start(t)
		res := advanceRung(t, c, failedP5(c, 0.5))
		rec := QualityLadderOf(c.ec.State)
		plan := getPlan(c.ec.State["plan"])
		if res.RewindTo != "submit_transcode" || rec.Rung != 2 || !slices.Equal(rec.Values, []int{22, 20, 19, 16}) || plan.Quality != 19 {
			t.Fatalf("%+v %+v plan quality %d", res, rec, plan.Quality)
		}
		if !strings.Contains(rec.Message, "(step 3 of 4)") || !qualityLadderPlanMatches(c.snapshot, c.ec.State, plan, rec) {
			t.Fatalf("message %q", rec.Message)
		}
		if got := c.ec.State["job_id"]; got != "job-"+c.ec.InstanceID+"-recipe-r2" {
			t.Fatalf("job %v", got)
		}
		advanceRung(t, c, failedP5(c, 0.2))
		if rec = QualityLadderOf(c.ec.State); rec.Rung != 3 || getPlan(c.ec.State["plan"]).Quality != 16 {
			t.Fatalf("%+v", rec)
		}
		res = advanceRung(t, c, failedP5(c, 0.1))
		if rec = QualityLadderOf(c.ec.State); res.RewindTo != "" || rec.Outcome != reasonQualityNotMet || len(rec.Tried) != 4 {
			t.Fatalf("%+v %+v", res, rec)
		}
	})
	t.Run("the trend is scaled to the distance left", func(t *testing.T) {
		c := newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
		planRecipe(t, c)
		advanceRung(t, c, failedP5(c, 2.5))
		// p5 rose 0.2 over two CRF steps, so about 0.1 over the one step to
		// CRF 19: more than a point short.
		res := advanceRung(t, c, failedP5(c, 2.3))
		rec := QualityLadderOf(c.ec.State)
		if res.RewindTo != "" || rec.Outcome != reasonQualityNotMet || rec.Rung != 1 || !strings.Contains(rec.Message, "stops before CRF 19") || !strings.Contains(rec.Message, "about 81.8 at CRF 19") {
			t.Fatalf("%+v %+v", res, rec)
		}
	})
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
	if rec.Choice == nil || !slices.Equal(rec.Values, []int{22, 21, 19, 16}) || plan.Quality != 21 || !qualityLadderPlanMatches(c.snapshot, c.ec.State, plan, rec) {
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
	compact := &ModeRecipe{VideoCodec: transcode.VideoCodecLibX265, QualityValues: []int{18, 16, 14}}
	vt := &ModeRecipe{VideoCodec: transcode.VideoCodecHEVCVideoToolbox, QualityValues: []int{60, 65, 70}}
	for name, tc := range map[string]struct {
		r      *ModeRecipe
		values []int
		want   bool
	}{
		"frozen":                    {x265, []int{22, 19, 16}, true},
		"inserted 21":               {x265, []int{22, 21, 19, 16}, true},
		"inserted 20":               {x265, []int{22, 20, 19, 16}, true},
		"compact inserted":          {compact, []int{18, 17, 16, 14}, true},
		"VideoToolbox frozen":       {vt, []int{60, 65, 70}, true},
		"VideoToolbox inserted":     {vt, []int{60, 62, 65, 70}, false},
		"empty":                     {x265, nil, false},
		"a rung missing":            {x265, []int{22, 19}, false},
		"repeats the first":         {x265, []int{22, 22, 19, 16}, false},
		"repeats the second":        {x265, []int{22, 19, 19, 16}, false},
		"above the first":           {x265, []int{22, 23, 19, 16}, false},
		"below the second":          {x265, []int{22, 18, 19, 16}, false},
		"between later rungs":       {x265, []int{22, 19, 18, 16}, false},
		"two inserted":              {x265, []int{22, 21, 20, 19, 16}, false},
		"first rung changed":        {x265, []int{21, 20, 19, 16}, false},
		"last rung changed":         {x265, []int{22, 20, 19, 15}, false},
		"same length, other values": {x265, []int{22, 20, 16}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ladderValuesMatch(tc.values, tc.r); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

// A record or plan that drifted from each other fails closed.
func TestQualityLadderPlanMatchesInsertedRung(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"allow_hardware": false}, nil)
	planRecipe(t, c)
	advanceRung(t, c, failedP5(c, 1.5))
	rec := QualityLadderOf(c.ec.State)
	plan := getPlan(c.ec.State["plan"])
	for name, mutate := range map[string]func(*QualityLadderRecord){
		"another rung":      func(r *QualityLadderRecord) { r.Rung = 2 },
		"rung out of range": func(r *QualityLadderRecord) { r.Rung = 4 },
		"negative rung":     func(r *QualityLadderRecord) { r.Rung = -1 },
		"inserted changed":  func(r *QualityLadderRecord) { r.Values = []int{22, 21, 19, 16} },
		"values invalid":    func(r *QualityLadderRecord) { r.Values = []int{22, 18, 19, 16} },
		"frozen values":     func(r *QualityLadderRecord) { r.Values, r.Rung = []int{22, 19, 16}, 1 },
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
	tampered.Values = []int{22, 18, 19, 16}
	c.ec.State[qualityLadderKey] = &tampered
	res, handled := c.engine.advanceQualityLadder(context.Background(), c.ec, failedP5(c, 0.5))
	if !handled || res.Status != StepFailed {
		t.Fatalf("%+v %v", res, handled)
	}
}

func TestQualityLadderStepScale(t *testing.T) {
	for _, tc := range []struct {
		prev, last, next int
		want             float64
	}{
		{22, 19, 16, 1},
		{22, 21, 19, 2},
		{22, 20, 19, 0.5},
		{21, 19, 16, 1.5},
		{18, 17, 16, 1},
		{60, 65, 70, 1},
		{22, 22, 19, 1},
	} {
		if got := qualityLadderStepScale(tc.prev, tc.last, tc.next); got != tc.want {
			t.Errorf("%d %d %d: got %v, want %v", tc.prev, tc.last, tc.next, got, tc.want)
		}
	}
	f := func(v float64) *float64 { return &v }
	limits := &QualityLimits{VMAFP5: f(84)}
	rung := func(v float64) *FinalQualitySummary {
		return &FinalQualitySummary{FailedChecks: []string{"quality_vmaf_p5_below_minimum"}, Measured: QualityNumbers{VMAFP5: f(v)}, Limits: limits}
	}
	// One point gained: at twice the distance it reaches the margin, at half
	// it falls short.
	if got := qualityLadderProjectedMiss(rung(80), rung(81), 2); got != nil {
		t.Fatalf("scale 2 stopped on %+v", got)
	}
	if got := qualityLadderProjectedMiss(rung(80), rung(81), 0.5); got == nil || got.Projected != 81.5 {
		t.Fatalf("scale 0.5: %+v", got)
	}
}
