package action

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
	"github.com/jakenesler/navigatorr/transcode/selector"
)

// legacyModePolicy* freeze what policies v2 (quality, x265_preserve, compact)
// and v3 (size) froze, digest included: the snapshots operations admitted
// before v4 carry and keep running under.
func legacyModePolicyWithBudget(mode string, anime bool, maxHeight int, hardware *transcode.HardwareSearchRules, allowSlow bool, budget *SizeBudgetPolicy) (ModePolicySnapshot, error) {
	snapshot, err := modePolicyContent(mode, anime, maxHeight, hardware, allowSlow, budget)
	if err != nil {
		return snapshot, err
	}
	return snapshot, digestModePolicy(&snapshot)
}

func legacyModePolicy(mode string, anime bool) (ModePolicySnapshot, error) {
	return legacyModePolicyWithBudget(mode, anime, 0, nil, false, nil)
}

func legacyModePolicyFor(mode string, anime bool, maxHeight int) (ModePolicySnapshot, error) {
	return legacyModePolicyWithBudget(mode, anime, maxHeight, nil, false, nil)
}

func legacyModePolicyWithHardware(mode string, anime bool, maxHeight int, hardware *transcode.HardwareSearchRules) (ModePolicySnapshot, error) {
	return legacyModePolicyWithBudget(mode, anime, maxHeight, hardware, false, nil)
}

func legacyModePolicyWithOptions(mode string, anime bool, maxHeight int, hardware *transcode.HardwareSearchRules, allowSlow bool) (ModePolicySnapshot, error) {
	return legacyModePolicyWithBudget(mode, anime, maxHeight, hardware, allowSlow, nil)
}

func freezeFresh(t *testing.T, inputs map[string]any) *ModePolicySnapshot {
	t.Helper()
	ec := &ExecutionContext{InstanceID: "recipe", Inputs: inputs, State: map[string]any{}}
	p, err := freezeModePolicy(ec)
	if err != nil || p == nil {
		t.Fatalf("freeze %v: %+v %v", inputs, p, err)
	}
	return p
}

func TestModePolicyV4FreezesOneRecipePerGoal(t *testing.T) {
	for name, tc := range map[string]struct {
		inputs  map[string]any
		kind    string
		codec   string
		preset  string
		values  []int
		workerV string
	}{
		"size with the media engine": {map[string]any{"mode": "size"}, RecipeQualityLadder, transcode.VideoCodecHEVCVideoToolbox, "", []int{60, 65, 70}, optimization.ModePolicyV3},
		"size without it":            {map[string]any{"mode": "size", "allow_hardware": false}, RecipeQualityLadder, transcode.VideoCodecLibX265, "medium", []int{22, 19, 16}, optimization.ModePolicyV3},
		"size, slow preset":          {map[string]any{"mode": "size", "allow_hardware": false, "allow_slow_preset": true}, RecipeQualityLadder, transcode.VideoCodecLibX265, "slow", []int{22, 19, 16}, optimization.ModePolicyV3},
		"quality":                    {map[string]any{"mode": "quality"}, RecipeQualityLadder, transcode.VideoCodecLibX265, "medium", []int{20, 17, 14}, ModePolicyVersion},
		"x265_preserve":              {map[string]any{"mode": "x265_preserve"}, RecipeQualityLadder, transcode.VideoCodecLibX265, "medium", []int{24, 21, 18}, ModePolicyVersion},
		"compact":                    {map[string]any{"mode": "compact"}, RecipeQualityLadder, transcode.VideoCodecLibX265, "medium", []int{18, 16, 14}, ModePolicyVersion},
		"size budget, media engine":  {map[string]any{"mode": "size", "size_budget_percent": 40.0}, RecipeSizeBudget, transcode.VideoCodecHEVCVideoToolbox, "", nil, optimization.ModePolicyV3},
		"size budget, x265":          {map[string]any{"mode": "size", "size_budget_percent": 40.0, "allow_hardware": false}, RecipeSizeBudget, transcode.VideoCodecLibX265, "", nil, optimization.ModePolicyV3},
	} {
		t.Run(name, func(t *testing.T) {
			p := freezeFresh(t, tc.inputs)
			r := p.Recipe
			if p.Version != ModePolicyV4 || p.OperationBudgetSeconds != 0 || r == nil || r.Kind != tc.kind || r.VideoCodec != tc.codec || r.Preset != tc.preset || !slices.Equal(r.QualityValues, tc.values) {
				t.Fatalf("snapshot %s budget %d recipe %+v", p.Version, p.OperationBudgetSeconds, r)
			}
			if p.workerVersion() != tc.workerV {
				t.Fatalf("worker sees %s", p.workerVersion())
			}
			// Only the VideoToolbox ladder carries a fallback: the x265 size
			// ladder a worker without VideoToolbox runs instead.
			if f := r.Fallback; (r.Kind == RecipeQualityLadder && r.VideoCodec == transcode.VideoCodecHEVCVideoToolbox) != (f != nil) ||
				f != nil && (f.Kind != RecipeQualityLadder || f.VideoCodec != transcode.VideoCodecLibX265 || f.Preset != "medium" || !slices.Equal(f.QualityValues, []int{22, 19, 16}) || f.Fallback != nil) {
				t.Fatalf("fallback %+v", r.Fallback)
			}
			// A stored (JSON round-tripped) snapshot verifies with its recipe.
			stored := map[string]any{}
			raw, _ := json.Marshal(p)
			_ = json.Unmarshal(raw, &stored)
			again, err := freezeModePolicy(&ExecutionContext{Inputs: tc.inputs, State: map[string]any{"mode_policy": stored}})
			if err != nil || !slices.Equal(again.Recipe.QualityValues, tc.values) || again.Digest != p.Digest {
				t.Fatalf("stored snapshot: %+v %v", again, err)
			}
		})
	}
}

// A recipe is part of the frozen identity: one rewritten under a fresh digest,
// a v4 snapshot without one, or an older snapshot that gained one is refused.
func TestFrozenRecipeFailsClosed(t *testing.T) {
	redigest := func(p ModePolicySnapshot) map[string]any {
		if err := digestModePolicy(&p); err != nil {
			t.Fatal(err)
		}
		stored := map[string]any{}
		raw, _ := json.Marshal(p)
		_ = json.Unmarshal(raw, &stored)
		return stored
	}
	p := *freezeFresh(t, map[string]any{"mode": "quality"})
	legacy, err := legacyModePolicy("quality", false)
	if err != nil {
		t.Fatal(err)
	}
	reordered := p
	reordered.Recipe = &ModeRecipe{Kind: RecipeQualityLadder, VideoCodec: transcode.VideoCodecLibX265, Preset: "medium", QualityValues: []int{14, 17, 20}}
	slow := p
	slow.Recipe = &ModeRecipe{Kind: RecipeQualityLadder, VideoCodec: transcode.VideoCodecLibX265, Preset: "slow", QualityValues: []int{20, 17, 14}}
	hardware := p
	hardware.Recipe = &ModeRecipe{Kind: RecipeQualityLadder, VideoCodec: transcode.VideoCodecHEVCVideoToolbox, QualityValues: []int{60, 65, 70}}
	budget := p
	budget.Recipe = &ModeRecipe{Kind: RecipeSizeBudget, VideoCodec: transcode.VideoCodecLibX265}
	missing := p
	missing.Recipe = nil
	gained := legacy
	gained.Recipe = p.Recipe
	for name, snapshot := range map[string]ModePolicySnapshot{"values out of order": reordered, "slow outside size": slow, "VideoToolbox outside size": hardware, "budget without one": budget, "v4 without a recipe": missing, "v2 with a recipe": gained} {
		ec := &ExecutionContext{Inputs: map[string]any{"mode": "quality"}, State: map[string]any{"mode_policy": redigest(snapshot)}}
		if _, err := freezeModePolicy(ec); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// The snapshot a v2 operation carries still verifies, with no recipe.
	ec := &ExecutionContext{Inputs: map[string]any{"mode": "quality"}, State: map[string]any{"mode_policy": redigest(legacy)}}
	if got, err := freezeModePolicy(ec); err != nil || got.Recipe != nil || got.Version != ModePolicyVersion {
		t.Fatalf("v2 snapshot: %+v %v", got, err)
	}
}

// newRecipeCase is a v4 goal at the point the benchmark submit step runs for
// it, on a worker that can run every recipe.
func newRecipeCase(t *testing.T, inputs map[string]any, mutate func(*transcode.WorkerCapabilities)) *budgetCase {
	t.Helper()
	caps := budgetCaps()
	caps.CompactAudio = true
	caps.CompactTranscode = true
	if mutate != nil {
		mutate(&caps)
	}
	in := map[string]any{"policy_v4": true}
	for k, v := range inputs {
		in[k] = v
	}
	c := newBudgetCase(t, in, caps)
	if c.snapshot.Version != ModePolicyV4 {
		t.Fatalf("case frozen under %s", c.snapshot.Version)
	}
	delete(c.ec.State, "benchmark_job_id")
	delete(c.ec.State, "benchmark_request")
	delete(c.ec.State, "benchmark_request_digest")
	c.ec.State["optimization_policy"] = c.snapshot.Profile.Optimization
	c.ec.State["resolved_path"] = "/media/source.mkv"
	if c.snapshot.Mode != "size" {
		// Only size goals encode 8-bit sources as Main10.
		delete(c.ec.State, "size_main10")
		base := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, AudioMode: c.snapshot.Profile.Audio.Mode}
		base.QualityValidation = c.base.QualityValidation
		if c.snapshot.Compact != nil {
			base.Compact = &transcode.CompactPlan{ChainVersion: transcode.CompactChainVersion, SourceClass: mediainspect.DynamicRangeSDR, SourceSignal: mediainspect.SignalSDR, SourceWidth: 3840, SourceHeight: 2160, MaxHeight: c.snapshot.Compact.MaxHeight, TargetWidth: 1920, TargetHeight: 1080}
		}
		if err := applyModePlan(base, c.snapshot); err != nil {
			t.Fatal(err)
		}
		c.base, c.ec.State["plan"] = base, base
	}
	return c
}

// planRecipe runs the plan step for a goal and checks it submitted no job.
func planRecipe(t *testing.T, c *budgetCase) {
	t.Helper()
	res, err := c.engine.stepPlanEncode(context.Background(), c.ec)
	if err != nil || res.Status != StepCompleted {
		t.Fatalf("plan %+v %v", res, err)
	}
	mergeMap(c.ec.State, res.Outputs)
	if getString(c.ec.State, "benchmark_job_id") != "" {
		t.Fatalf("a job was recorded: %+v", c.ec.State)
	}
}

func failedRung(vmaf, p5 float64) transcode.JobStatus {
	return transcode.JobStatus{Status: transcode.StatusFailed, ReasonCode: reasonQualityNotMet, QualityEvidence: &transcode.FinalQualityEvidence{Verdict: "fail", ReasonCodes: []string{"quality_vmaf_p5_below_minimum"}, VMAF: &quality.VMAFStats{Mean: vmaf, P5: p5}}}
}

func advanceRung(t *testing.T, c *budgetCase, st transcode.JobStatus) StepResult {
	t.Helper()
	res, handled := c.engine.advanceQualityLadder(context.Background(), c.ec, st)
	if !handled {
		t.Fatal("refusal not handled by the ladder")
	}
	mergeMap(c.ec.State, res.Outputs)
	return res
}

func TestQualityLadderRecipes(t *testing.T) {
	for name, tc := range map[string]struct {
		inputs map[string]any
		codec  string
		preset string
		values []int
	}{
		"quality":            {map[string]any{"mode": "quality"}, transcode.VideoCodecLibX265, "medium", []int{20, 17, 14}},
		"x265_preserve":      {map[string]any{"mode": "x265_preserve"}, transcode.VideoCodecLibX265, "medium", []int{24, 21, 18}},
		"compact":            {map[string]any{"mode": "compact"}, transcode.VideoCodecLibX265, "medium", []int{18, 16, 14}},
		"size, x265":         {map[string]any{"allow_hardware": false}, transcode.VideoCodecLibX265, "medium", []int{22, 19, 16}},
		"size, x265 slow":    {map[string]any{"allow_hardware": false, "allow_slow_preset": true}, transcode.VideoCodecLibX265, "slow", []int{22, 19, 16}},
		"size, VideoToolbox": {nil, transcode.VideoCodecHEVCVideoToolbox, "", []int{60, 65, 70}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newRecipeCase(t, tc.inputs, nil)
			planRecipe(t, c)
			rec := QualityLadderOf(c.ec.State)
			plan := getPlan(c.ec.State["plan"])
			if rec == nil || rec.Rung != 0 || rec.Outcome != QualityLadderEncoding || !slices.Equal(rec.Values, tc.values) || c.ec.State["plan_source"] != qualityLadderPlanSource {
				t.Fatalf("record %+v source %v", rec, c.ec.State["plan_source"])
			}
			if plan.VideoCodec != tc.codec || plan.Quality != tc.values[0] || plan.Preset != tc.preset || plan.AverageBitrateKbps != 0 || plan.SizePolicy == nil || plan.SizePolicy.MaxCandidateBytes != 0 {
				t.Fatalf("first rung plan %+v", plan)
			}
			if plan.PolicyDigest != c.snapshot.Digest || plan.ModePolicyVersion != c.snapshot.workerVersion() || plan.QualityValidation == nil || plan.QualityValidation.BenchmarkRequestDigest != c.ec.State["benchmark_request_digest"] {
				t.Fatalf("plan is not pinned to the snapshot and request: %+v", plan)
			}
			if !qualityLadderPlanMatches(c.snapshot, c.ec.State, plan, rec) {
				t.Fatal("the planned rung does not rebuild from the frozen recipe")
			}
			// A second pass of the submit step keeps the plan.
			if again, _ := c.engine.stepPlanEncode(context.Background(), c.ec); again.Status != StepCompleted || getPlan(c.ec.State["plan"]).PlanDigest != plan.PlanDigest {
				t.Fatalf("a second pass replanned: %+v", again)
			}
			// Each quality miss encodes one rung better, under a new job id.
			for rung := 1; rung < len(tc.values); rung++ {
				res := advanceRung(t, c, failedRung(85, 80))
				rec = QualityLadderOf(c.ec.State)
				next := getPlan(c.ec.State["plan"])
				if res.Status != StepCompleted || res.RewindTo != "submit_transcode" || rec.Rung != rung || next.Quality != tc.values[rung] || c.ec.State["job_id"] != fmt.Sprintf("job-%s-recipe-r%d", c.ec.InstanceID, rung) {
					t.Fatalf("rung %d: %+v %+v plan %+v job %v", rung, res, rec, next, c.ec.State["job_id"])
				}
				if !strings.Contains(rec.Message, "missed the quality limits") || !strings.Contains(rec.Message, fmt.Sprintf("step %d of 3", rung+1)) {
					t.Fatalf("message %q", rec.Message)
				}
				if !qualityLadderPlanMatches(c.snapshot, c.ec.State, next, rec) {
					t.Fatal("the next rung does not rebuild")
				}
			}
			// The last rung's miss ends the goal and names every rung tried.
			res := advanceRung(t, c, failedRung(84, 79))
			rec = QualityLadderOf(c.ec.State)
			if res.RewindTo != "" || !getBool(c.ec.State, "skip_transcode") || getString(c.ec.State, "reason_code") != reasonQualityNotMet || rec.Outcome != reasonQualityNotMet || len(rec.Tried) != 3 {
				t.Fatalf("last rung: %+v %+v", res, rec)
			}
			for _, q := range tc.values {
				if !strings.Contains(rec.Message, recipeRungName(tc.codec, q)) {
					t.Fatalf("message %q does not name rung %d", rec.Message, q)
				}
			}
			if !strings.Contains(rec.Message, "VMAF p5 79.0") || !strings.Contains(rec.Message, "original preserved") {
				t.Fatalf("message %q", rec.Message)
			}
			// Only a VideoToolbox ladder that missed offers x265 next.
			if rec.RetryWithoutHardware != (tc.codec == transcode.VideoCodecHEVCVideoToolbox) || QualityLadderOf(res.Outputs).RetryWithoutHardware != rec.RetryWithoutHardware {
				t.Fatalf("retry without hardware %v for %s", rec.RetryWithoutHardware, tc.codec)
			}
		})
	}
}

// A rung refused for its size stops the ladder: a better rung is larger.
func TestQualityLadderStopsOnSize(t *testing.T) {
	for _, reason := range []string{"insufficient_savings", "size_growth_exceeded"} {
		c := newRecipeCase(t, map[string]any{"mode": "quality"}, nil)
		planRecipe(t, c)
		res := advanceRung(t, c, transcode.JobStatus{Status: transcode.StatusFailed, ReasonCode: reason, CandidateSizeBytes: 9_000_000_000})
		rec := QualityLadderOf(c.ec.State)
		want := map[string]string{"insufficient_savings": "insufficient_savings", "size_growth_exceeded": "size_growth"}[reason]
		if res.RewindTo != "" || !getBool(c.ec.State, "skip_transcode") || getString(c.ec.State, "reason_code") != want || rec.Outcome != want || rec.Rung != 0 || len(rec.Tried) != 1 {
			t.Fatalf("%s: %+v %+v", reason, res, rec)
		}
		if !strings.Contains(rec.Message, "x265 CRF 20") || !strings.Contains(rec.Message, "stops here") {
			t.Fatalf("message %q", rec.Message)
		}
	}
	// A quality miss without scores ends the goal rather than guessing.
	c := newRecipeCase(t, map[string]any{"mode": "quality"}, nil)
	planRecipe(t, c)
	advanceRung(t, c, transcode.JobStatus{Status: transcode.StatusFailed, ReasonCode: reasonQualityNotMet})
	if rec := QualityLadderOf(c.ec.State); rec.Outcome != reasonQualityNotMet || rec.Rung != 0 || !strings.Contains(rec.Message, "no scores") {
		t.Fatalf("no evidence: %+v", rec)
	}
	// Other worker failures are not the ladder's.
	c = newRecipeCase(t, map[string]any{"mode": "quality"}, nil)
	planRecipe(t, c)
	if _, handled := c.engine.advanceQualityLadder(context.Background(), c.ec, transcode.JobStatus{Status: transcode.StatusFailed, ReasonCode: "encoder_error"}); handled {
		t.Fatal("an encoder failure advanced the ladder")
	}
}

func TestQualityLadderNeedsItsEncoder(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"mode": "quality"}, func(caps *transcode.WorkerCapabilities) {
		encoders := map[string]bool{}
		for k, v := range caps.Encoders {
			encoders[k] = v
		}
		encoders[transcode.VideoCodecLibX265] = false
		caps.Encoders = encoders
	})
	res, err := c.engine.stepPlanEncode(context.Background(), c.ec)
	if err != nil || res.Status != StepWaitingDecision || c.executor.benchmarkSubmitCalls != 0 || QualityLadderOf(c.ec.State) != nil {
		t.Fatalf("missing encoder: %+v %v", res, err)
	}
}

// validateRecipeCase readies the encoded rung for the validate step.
func validateRecipeCase(t *testing.T, c *budgetCase, candidateBytes int) {
	t.Helper()
	candidate := t.TempDir() + "/candidate.mkv"
	if err := os.WriteFile(candidate, make([]byte, candidateBytes), 0600); err != nil {
		t.Fatal(err)
	}
	plan := getPlan(c.ec.State["plan"])
	c.ec.State["candidate_path"], c.ec.State["candidate_sha256"] = candidate, "candidate"
	c.ec.State["original_size"], c.ec.State["original"] = int64(100), map[string]any{"size_bytes": int64(100)}
	c.ec.State["quality_evidence"] = map[string]any{"verdict": "pass", "candidate_sha256": "candidate", "plan_digest": plan.PlanDigest, "benchmark_request_digest": c.ec.State["benchmark_request_digest"], "vmaf": map[string]any{"mean": 93.0, "p5": 88.0}}
}

func TestQualityLadderValidation(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"mode": "quality"}, nil)
	planRecipe(t, c)
	advanceRung(t, c, failedRung(85, 80))
	validateRecipeCase(t, c, 50)
	res, err := c.engine.stepTranscodeValidate(context.Background(), c.ec)
	if err != nil || res.Status != StepCompleted || getBool(c.ec.State, "skip_transcode") {
		t.Fatalf("validate %+v %v", res, err)
	}
	rec := QualityLadderOf(c.ec.State)
	if rec == nil || rec.Outcome != QualityLadderPassed || rec.Rung != 1 || !strings.Contains(rec.Message, "x265 CRF 17 passed") || !strings.Contains(rec.Message, "VMAF 93.0") {
		t.Fatalf("passed record %+v", rec)
	}

	// An encode that is not the recorded rung fails closed.
	for name, tamper := range map[string]func(*transcode.Plan){
		"another CRF":   func(p *transcode.Plan) { p.Quality = 15 },
		"another codec": func(p *transcode.Plan) { p.VideoCodec = transcode.VideoCodecHEVCVideoToolbox },
		"a byte limit": func(p *transcode.Plan) {
			sp := *p.SizePolicy
			sp.MaxCandidateBytes = 99
			p.SizePolicy = &sp
		},
		"another request": func(p *transcode.Plan) {
			qv := *p.QualityValidation
			qv.BenchmarkRequestDigest = "sha256:other"
			p.QualityValidation = &qv
		},
	} {
		c := newRecipeCase(t, map[string]any{"mode": "quality"}, nil)
		planRecipe(t, c)
		plan := *getPlan(c.ec.State["plan"])
		tamper(&plan)
		plan.PlanDigest = ""
		plan.PlanDigest, _ = transcode.DigestPlan(&plan)
		c.ec.State["plan"] = &plan
		validateRecipeCase(t, c, 50)
		if res, _ := c.engine.stepTranscodeValidate(context.Background(), c.ec); res.Status != StepFailed || !strings.Contains(res.Error, "frozen recipe") {
			t.Errorf("%s: %+v", name, res)
		}
	}
	// So does a v4 encode whose plan did not come from the recipe.
	c = newRecipeCase(t, map[string]any{"mode": "quality"}, nil)
	planRecipe(t, c)
	c.ec.State["plan_source"] = "benchmark_winner"
	validateRecipeCase(t, c, 50)
	if res, _ := c.engine.stepTranscodeValidate(context.Background(), c.ec); res.Status != StepFailed {
		t.Fatalf("search plan accepted under v4: %+v", res)
	}
}

// With the media engine off, a budget is one two-pass x265 encode at the
// budget, planned without a search; over the budget it ends as exceeded.
func TestX265SizeBudgetRecipe(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"size_budget_percent": 40.0, "allow_hardware": false}, nil)
	if c.snapshot.Recipe.Kind != RecipeSizeBudget || c.snapshot.Recipe.VideoCodec != transcode.VideoCodecLibX265 {
		t.Fatalf("recipe %+v", c.snapshot.Recipe)
	}
	planRecipe(t, c)
	rec := sizeBudgetOf(c.ec.State)
	plan := getPlan(c.ec.State["plan"])
	if rec == nil || rec.Trigger != sizeBudgetTriggerBudgetFirst || rec.Choice == nil || rec.Choice.Reason != SizeBudgetChoiceHardwareOff || c.ec.State["plan_source"] != sizeBudgetPlanSource {
		t.Fatalf("record %+v source %v", rec, c.ec.State["plan_source"])
	}
	if plan.VideoCodec != transcode.VideoCodecLibX265 || plan.X265 == nil || plan.SizePolicy == nil || plan.X265.BitrateKbps <= 0 || !plan.X265.TwoPass || rec.BudgetBytes != 4_000_000_000 {
		t.Fatalf("budget plan %+v x265 %+v size %+v record budget %d", plan, plan.X265, plan.SizePolicy, rec.BudgetBytes)
	}
	if !sizeBudgetPlanMatches(c.snapshot, c.ec.State, plan, rec) {
		t.Fatal("the budget encode does not rebuild from the frozen policy")
	}
	if !strings.Contains(rec.Choice.Message, "allow_hardware=false") {
		t.Fatalf("choice %q", rec.Choice.Message)
	}
	if _, handled := c.engine.advanceSizeBudgetRung(c.ec, 4_100_000_000); handled {
		t.Fatal("a one-encode budget advanced to another rung")
	}
	if _, fits := finishSizeBudget(c.ec, 4_100_000_000); fits {
		t.Fatal("an encode over the budget was accepted")
	}

	// A worker that cannot run two-pass x265 ends the goal with the reason,
	// not with a search.
	c = newRecipeCase(t, map[string]any{"size_budget_percent": 40.0, "allow_hardware": false}, func(caps *transcode.WorkerCapabilities) { caps.X265TwoPass = false })
	res, err := c.engine.stepPlanEncode(context.Background(), c.ec)
	if err != nil || res.Status != StepCompleted || !getBool(c.ec.State, "skip_transcode") || getString(c.ec.State, "reason_code") != "size_budget_worker_unsupported" || c.executor.benchmarkSubmitCalls != 0 {
		t.Fatalf("no two-pass worker: %+v %v", res, err)
	}

	// A media-engine budget on a worker without VideoToolbox is the same encode.
	c = newRecipeCase(t, map[string]any{"size_budget_percent": 40.0}, func(caps *transcode.WorkerCapabilities) {
		encoders := map[string]bool{}
		for k, v := range caps.Encoders {
			encoders[k] = v
		}
		encoders[transcode.VideoCodecHEVCVideoToolbox] = false
		caps.Encoders = encoders
	})
	planRecipe(t, c)
	if rec := sizeBudgetOf(c.ec.State); rec == nil || rec.Choice.Reason != SizeBudgetChoiceWorkerUnsupported || getPlan(c.ec.State["plan"]).VideoCodec != transcode.VideoCodecLibX265 {
		t.Fatalf("no VideoToolbox: %+v", rec)
	}
}

// A media-engine budget runs the VideoToolbox ladder as v3 did.
func TestVideoToolboxSizeBudgetRecipe(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"size_budget_percent": 40.0}, nil)
	planRecipe(t, c)
	rec := sizeBudgetLadderOf(c.ec.State)
	plan := getPlan(c.ec.State["plan"])
	if rec == nil || rec.Choice.Reason != SizeBudgetChoiceLadder || plan.VideoCodec != transcode.VideoCodecHEVCVideoToolbox || plan.Quality != 65 {
		t.Fatalf("ladder %+v plan %+v", rec, plan)
	}
}

// New requests are refused the sample search, a batch's children included.
func TestSearchInputsAreRefusedForNewRequests(t *testing.T) {
	mediaDir := t.TempDir()
	source := mediaDir + "/ep01.mkv"
	if err := os.WriteFile(source, []byte("synthetic source bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, createFakeFFprobeScript(t, defaultValidFFprobeJSON), []string{mediaDir}, []string{mediaDir}, false)
	defer st.Close()
	seedActionInstance(t, st, "parent-batch", "transcode_batch", StatusWaitingExternal, 1, "", map[string]any{"paths": []string{source}}, map[string]any{})
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "parent-batch", ItemKey: "ep01", FilePath: source, Decision: "transcode", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		action, field string
		inputs        map[string]any
	}{
		{"transcode_media", "metric", map[string]any{"path": source, "profile": "general-hevc", "metric": "vmaf"}},
		{"transcode_media", "profile_config", map[string]any{"path": source, "profile_config": optimizingProfileConfig()}},
		{"transcode_batch", "priority", map[string]any{"paths": []string{source}, "priority": "savings"}},
		{"transcode_batch", "shared_calibration", map[string]any{"paths": []string{source}, "shared_calibration": true}},
		{"transcode_batch", "calibration_items", map[string]any{"paths": []string{source}, "calibration_items": 2}},
		{"transcode_media", "batch_fixed_quality", map[string]any{"path": source, "profile": "general-hevc", "batch_fixed_quality": 23}},
		{"transcode_media", "metric", map[string]any{"path": source, "profile": "general-hevc", "metric": "vmaf", "parent_action_id": "parent-batch", "batch_item_key": "ep01"}},
	} {
		_, err := e.Enqueue(context.Background(), tc.action, tc.inputs, fmt.Sprintf("refused-%d", i))
		invalid := requireInvalid(t, err, tc.field)
		if !strings.Contains(invalid.Reason, "sample search") {
			t.Fatalf("%s %v: reason %q", tc.action, tc.inputs, invalid.Reason)
		}
	}
	// The sample test action is gone: it is refused by name, on every surface.
	for surface, call := range map[string]func() (*ActionResult, error){
		"Enqueue": func() (*ActionResult, error) {
			return e.Enqueue(context.Background(), removedBenchmarkAction, map[string]any{"path": source}, "benchmark-enqueue")
		},
		"Run": func() (*ActionResult, error) {
			return e.Run(context.Background(), removedBenchmarkAction, map[string]any{"path": source}, "benchmark-run")
		},
	} {
		res, err := call()
		if res != nil || err == nil {
			t.Fatalf("%s admitted %s: %+v %v", surface, removedBenchmarkAction, res, err)
		}
		if _, ok := AsInvalidParams(err); !ok || !strings.Contains(err.Error(), "sample tests were removed") {
			t.Fatalf("%s: want an input error naming the removal, got %v", surface, err)
		}
	}
	// Explicitly off is not a search.
	if _, err := e.Enqueue(context.Background(), "transcode_batch", map[string]any{"paths": []string{source}, "shared_calibration": false, "dry_run": true}, "off"); err != nil {
		t.Fatal(err)
	}
}

// A request with no goal and no profile encodes with the configured
// default_profile, so a default that searches is refused like a named one; a
// batch with neither runs auto, which stands for every profile the selector
// picks.
func TestSearchingDefaultProfileIsRefused(t *testing.T) {
	mediaDir := t.TempDir()
	source := mediaDir + "/ep01.mkv"
	if err := os.WriteFile(source, []byte("synthetic source bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, createFakeFFprobeScript(t, defaultValidFFprobeJSON), []string{mediaDir}, []string{mediaDir}, false)
	defer st.Close()
	tc := &e.deps.Config.Transcode
	// The shipped catalog's live-action profiles search.
	tc.DefaultProfile = "live-action-hevc"
	_, err := e.Enqueue(context.Background(), "transcode_media", map[string]any{"path": source}, "default-search")
	if invalid := requireInvalid(t, err, "profile"); !strings.Contains(invalid.Reason, "default_profile") || !strings.Contains(invalid.Reason, "live-action-hevc") {
		t.Fatalf("reason %q", invalid.Reason)
	}
	// A goal, a fixed profile or an inline fixed profile does not use it.
	for i, inputs := range []map[string]any{
		{"path": source, "mode": "quality"},
		{"path": source, "profile": "general-hevc"},
		{"path": source, "profile_config": testEphemeralBatchProfileConfig()},
	} {
		if _, err := e.Enqueue(context.Background(), "transcode_media", inputs, fmt.Sprintf("default-bypass-%d", i)); err != nil {
			t.Fatalf("%v refused: %v", inputs, err)
		}
	}
	// A fixed default, or none (hevc-vt), is admitted.
	for i, name := range []string{"hevc-vt-balanced", ""} {
		tc.DefaultProfile = name
		if _, err := e.Enqueue(context.Background(), "transcode_media", map[string]any{"path": source}, fmt.Sprintf("default-fixed-%d", i)); err != nil {
			t.Fatalf("default %q refused: %v", name, err)
		}
	}
	// auto, named or as the default, is refused once a profile the selector
	// picks searches.
	live, err := tc.ResolveProfile("live-action-hevc")
	if err != nil || live.Optimization == nil || !live.Optimization.Enabled {
		t.Fatalf("the catalog's live-action profile no longer searches: %v", err)
	}
	tc.Profiles = map[string]config.TranscodeProfileConfig{selector.AnimeProfile: {
		Container:    "mkv",
		Video:        config.VideoProfileConfig{Codec: "libx265", Quality: 22, Preset: "medium"},
		Audio:        config.AudioProfileConfig{Mode: "copy"},
		Subtitles:    config.SubtitleProfileConfig{Mode: "preserve", ConvertIncompatible: true},
		Preserve:     config.PreserveProfileConfig{Metadata: true, Chapters: true, Attachments: true},
		Optimization: live.Optimization,
	}}
	tc.DefaultProfile = "auto"
	for i, req := range []struct {
		action string
		inputs map[string]any
	}{
		{"transcode_media", map[string]any{"path": source}},
		{"transcode_media", map[string]any{"path": source, "profile": "auto"}},
		{"transcode_batch", map[string]any{"paths": []string{source}, "dry_run": true}},
	} {
		_, err := e.Enqueue(context.Background(), req.action, req.inputs, fmt.Sprintf("auto-search-%d", i))
		if invalid := requireInvalid(t, err, "profile"); !strings.Contains(invalid.Reason, selector.AnimeProfile) {
			t.Fatalf("%s %v: reason %q", req.action, req.inputs, invalid.Reason)
		}
	}
}

// A whole v4 season is admitted up to the worker's slots.
func TestModeBatchV4AdmitsWithoutReservations(t *testing.T) {
	st := setupTestStore(t)
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{Transcode: config.TranscodeConfig{MaxParallelJobs: 2}}})
	e.RegisterTemplate(ActionTemplate{Name: "transcode_media", ImmutableInputs: true, Steps: []StepDefinition{{Name: "encode", Run: func(context.Context, *ExecutionContext) (StepResult, error) {
		return StepResult{Status: StepWaitingExternal, WaitingCondition: "transcode_processing"}, nil
	}}}})
	ec := &ExecutionContext{InstanceID: "v4-season", Inputs: map[string]any{"mode": "quality"}, State: map[string]any{}}
	if _, err := freezeModePolicy(ec); err != nil {
		t.Fatal(err)
	}
	seedActionInstance(t, st, ec.InstanceID, "transcode_batch", StatusWaitingExternal, 1, "", ec.Inputs, ec.State)
	for i := 0; i < 6; i++ {
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: ec.InstanceID, ItemKey: fmt.Sprintf("file-%d", i), FilePath: fmt.Sprintf("/media/%d.mkv", i), Status: "queued", Decision: "transcode"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Resume(context.Background(), ec.InstanceID, "", nil); err != nil {
		t.Fatal(err)
	}
	children := 0
	rows, _ := st.ListActionInstances("", 100)
	for _, row := range rows {
		if row.ActionName == "transcode_media" {
			children++
		}
	}
	if children != 2 {
		t.Fatalf("children %d, want the worker's 2 slots", children)
	}
}

// The v2 snapshots operations admitted before recipes still carry verify
// byte for byte: their digests are pinned to what main computed for them.
func TestModePolicyV2DigestsArePinned(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		anime  bool
		digest string
	}{
		{"quality", false, "sha256:c9eacad8dde4c6743dfd6ae7a988473ad86949a29944284cc2e6320385c09827"},
		{"quality", true, "sha256:1556e750fe4682891d9d945f9dbf2d11d9df03a78f5ab2f13d9d73e0657eaf2b"},
		{"x265_preserve", false, "sha256:3b96cf8757b0d5cad92d5be0fc830144a5dd76437ffce67eed8c58fceafad56d"},
		{"x265_preserve", true, "sha256:3d9cf9ae99e59e1afd3aade2c76e0fbc06e7cf6c765ff4f4e134078142298895"},
		{"compact", false, "sha256:4acfc93b9caa4f8706188f6f6e4e7dd8328f451ee3b1cfc9244e184efdef14d5"},
		{"compact", true, "sha256:8f44edde64eed179bcf69b68101b2c45b7118172e6e4886bfc9cfcb86686ba01"},
	} {
		p, err := legacyModePolicy(tc.mode, tc.anime)
		if err != nil {
			t.Fatal(err)
		}
		if p.Version != ModePolicyVersion || p.Recipe != nil || p.Digest != tc.digest {
			t.Errorf("%s anime=%v: %s %s, want %s", tc.mode, tc.anime, p.Version, p.Digest, tc.digest)
		}
	}
}

// A VideoToolbox ladder's fallback is one x265 ladder, and only it has one.
func TestRecipeFallbackIsValidated(t *testing.T) {
	size := freezeFresh(t, map[string]any{"mode": "size"})
	x265 := freezeFresh(t, map[string]any{"mode": "size", "allow_hardware": false})
	budget := freezeFresh(t, map[string]any{"mode": "size", "size_budget_percent": 40.0})
	if err := size.Recipe.validate(size); err != nil {
		t.Fatal(err)
	}
	ladder := func(codec string, values ...int) *ModeRecipe {
		preset := "medium"
		if codec == transcode.VideoCodecHEVCVideoToolbox {
			preset = ""
		}
		return &ModeRecipe{Kind: RecipeQualityLadder, VideoCodec: codec, Preset: preset, QualityValues: values}
	}
	with := func(r *ModeRecipe, f *ModeRecipe) *ModeRecipe { c := *r; c.Fallback = f; return &c }
	nested := with(ladder(transcode.VideoCodecLibX265, 22, 19, 16), ladder(transcode.VideoCodecLibX265, 22, 19, 16))
	for name, tc := range map[string]struct {
		p *ModePolicySnapshot
		r *ModeRecipe
	}{
		"VideoToolbox falling back to VideoToolbox": {size, with(size.Recipe, ladder(transcode.VideoCodecHEVCVideoToolbox, 60, 65))},
		"a fallback with its own fallback":          {size, with(size.Recipe, nested)},
		"a fallback that is not a ladder":           {size, with(size.Recipe, &ModeRecipe{Kind: RecipeSizeBudget, VideoCodec: transcode.VideoCodecLibX265})},
		"a fallback with rising CRF":                {size, with(size.Recipe, ladder(transcode.VideoCodecLibX265, 16, 19, 22))},
		"an x265 ladder with a fallback":            {x265, with(x265.Recipe, ladder(transcode.VideoCodecLibX265, 20, 18))},
		"a size budget with a fallback":             {budget, with(budget.Recipe, ladder(transcode.VideoCodecLibX265, 22, 19, 16))},
	} {
		if err := tc.r.validate(tc.p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// A worker without VideoToolbox runs the size goal's x265 ladder, recorded
// as a choice with its reason, as a VideoToolbox budget falls back to x265.
func TestVideoToolboxLadderFallsBackToX265(t *testing.T) {
	noVT := func(caps *transcode.WorkerCapabilities) {
		encoders := map[string]bool{}
		for k, v := range caps.Encoders {
			encoders[k] = v
		}
		encoders[transcode.VideoCodecHEVCVideoToolbox] = false
		caps.Encoders = encoders
	}
	c := newRecipeCase(t, nil, noVT)
	if c.snapshot.Recipe.VideoCodec != transcode.VideoCodecHEVCVideoToolbox {
		t.Fatalf("recipe %+v", c.snapshot.Recipe)
	}
	planRecipe(t, c)
	rec := QualityLadderOf(c.ec.State)
	plan := getPlan(c.ec.State["plan"])
	if rec == nil || rec.VideoCodec != transcode.VideoCodecLibX265 || rec.Preset != "medium" || !slices.Equal(rec.Values, []int{22, 19, 16}) || rec.Choice == nil || rec.Choice.Reason != SizeBudgetChoiceWorkerUnsupported || rec.Choice.VideoCodec != transcode.VideoCodecLibX265 {
		t.Fatalf("record %+v", rec)
	}
	if !strings.HasPrefix(rec.Message, rec.Choice.Message) || !strings.Contains(rec.Choice.Message, "cannot encode with VideoToolbox") {
		t.Fatalf("message %q", rec.Message)
	}
	if plan.VideoCodec != transcode.VideoCodecLibX265 || plan.Quality != 22 || plan.Preset != "medium" || !qualityLadderPlanMatches(c.snapshot, c.ec.State, plan, rec) {
		t.Fatalf("first rung %+v", plan)
	}
	// The record's choice is what makes the x265 plan the frozen one.
	unchosen := *rec
	unchosen.Choice = nil
	if qualityLadderPlanMatches(c.snapshot, c.ec.State, plan, &unchosen) {
		t.Fatal("an x265 rung matched the VideoToolbox recipe without the recorded choice")
	}
	for rung, q := range []int{19, 16} {
		advanceRung(t, c, failedRung(85, 80))
		rec = QualityLadderOf(c.ec.State)
		if next := getPlan(c.ec.State["plan"]); rec.Rung != rung+1 || next.Quality != q || next.VideoCodec != transcode.VideoCodecLibX265 || !qualityLadderPlanMatches(c.snapshot, c.ec.State, next, rec) {
			t.Fatalf("rung %d: %+v %+v", rung+1, rec, next)
		}
	}
	// It was x265 already, so a miss offers nothing more.
	advanceRung(t, c, failedRung(84, 79))
	if rec = QualityLadderOf(c.ec.State); rec.Outcome != reasonQualityNotMet || rec.RetryWithoutHardware {
		t.Fatalf("last rung %+v", rec)
	}

	// A worker with neither encoder still waits on the recipe's own.
	c = newRecipeCase(t, nil, func(caps *transcode.WorkerCapabilities) {
		noVT(caps)
		caps.Encoders[transcode.VideoCodecLibX265] = false
	})
	res, err := c.engine.stepPlanEncode(context.Background(), c.ec)
	if err != nil || res.Status != StepWaitingDecision || res.WaitingCondition != "recipe_encoder_unavailable" || QualityLadderOf(c.ec.State) != nil {
		t.Fatalf("no encoder: %+v %v", res, err)
	}
}

// When the ladder's end hands over to the audio-only fallback and that does
// not complete, the ladder's outcome is still recorded: the UI never shows
// the last rung as encoding.
func TestQualityLadderOutcomeSurvivesAnUnfinishedAudioOnlyFallback(t *testing.T) {
	c := newRecipeCase(t, nil, nil)
	planRecipe(t, c)
	advanceRung(t, c, failedRung(85, 80))
	advanceRung(t, c, failedRung(85, 80))
	// Lossless audio could be compacted, but the worker cannot be asked.
	c.ec.State["source_report"], c.ec.State["original_size"] = audioOnlyReport(), int64(10_700_000_000)
	c.executor.capabilitiesFunc = func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{}, fmt.Errorf("worker unreachable")
	}
	res, handled := c.engine.advanceQualityLadder(context.Background(), c.ec, failedRung(84, 79))
	if !handled || res.Status == StepCompleted {
		t.Fatalf("fallback result %+v", res)
	}
	for where, rec := range map[string]*QualityLadderRecord{"state": QualityLadderOf(c.ec.State), "outputs": QualityLadderOf(res.Outputs)} {
		if rec == nil || rec.Outcome != reasonQualityNotMet || len(rec.Tried) != 3 || !rec.RetryWithoutHardware {
			t.Fatalf("%s record %+v", where, rec)
		}
	}
}

// optimizingProfileConfig is an inline profile that asks for the removed
// optimization, which admission refuses.
func optimizingProfileConfig() map[string]any {
	profile := testEphemeralBatchProfileConfig()
	profile["optimization"] = map[string]any{"enabled": true, "search": map[string]any{"quality_values": []int{20, 22}, "max_candidates": 2}}
	return profile
}
