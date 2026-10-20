package transcodeworker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

func cancelFixtureShared(windows int) (*pipelineShared, []pipelineUnit) {
	record := &BenchmarkRecord{}
	for i := 0; i < windows; i++ {
		record.Samples = append(record.Samples, transcode.BenchmarkSampleWindow{Index: i, StartSeconds: float64(10 * i), DurationSeconds: 10})
	}
	vcs := []validatedCandidate{{index: 0, candidate: transcode.BenchmarkCandidate{ID: "cand-a", VideoCodec: "libx265", Quality: 22, Preset: "medium"}, bitDepth: 8}}
	var units []pipelineUnit
	for s := range record.Samples {
		units = append(units, pipelineUnit{vcOrd: 0, sampOrd: s})
	}
	return &pipelineShared{
		record:    record,
		evidence:  &BenchmarkExecutionEvidence{},
		vcs:       vcs,
		runVMAF:   true,
		encodeOut: make([]pipelineEncodeOutcome, len(units)),
		metricOut: make([]pipelineMetricOutcome, len(units)),
		fatal:     &pipelineFatal{},
	}, units
}

// A cancellation by the search budget must come back as that cancellation, not
// as a pipeline that finished with holes in its evidence.
func TestPipelineCancelledByBudgetReportsTheStop(t *testing.T) {
	sh, units := cancelFixtureShared(2)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errSearchBudgetStopped)

	sh.runUnits(ctx, units, []int{0, 1}, nil, true, 2, 2)
	err, _ := sh.fatal.get()
	if err == nil {
		err = sh.checkComplete(ctx, units)
	}
	if err == nil {
		t.Fatal("a pipeline that ran nothing reported success")
	}
	if !errors.Is(err, errSearchBudgetStopped) || !errors.Is(err, context.Canceled) {
		t.Fatalf("budget stop lost: %v", err)
	}
	if strings.Contains(err.Error(), "missing") {
		t.Fatalf("cancellation reported as missing evidence: %v", err)
	}
}

// With a gap and a cancelled context but no recorded failure, the check still
// reports the stop itself.
func TestPipelineGapUnderCancelledContextReportsTheCause(t *testing.T) {
	sh, units := cancelFixtureShared(2)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errSearchBudgetStopped)
	err := sh.checkComplete(ctx, units)
	if !errors.Is(err, errSearchBudgetStopped) || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// With no failure and no cancellation to explain the gap, the pipeline says so
// by name instead of returning success.
func TestPipelineGapWithoutCauseIsATypedIncompleteError(t *testing.T) {
	sh, units := cancelFixtureShared(3)
	// Windows 0 and 2 are complete; window 1 never ran.
	for _, i := range []int{0, 2} {
		sh.encodeOut[i] = pipelineEncodeOutcome{entry: BenchmarkCandidateSampleResult{CandidateID: "cand-a", SampleIndex: i, SizeBytes: 10}}
		sh.metricOut[i] = pipelineMetricOutcome{executed: true}
	}
	err := sh.checkComplete(context.Background(), units)
	var incomplete errPipelineIncomplete
	if !errors.As(err, &incomplete) {
		t.Fatalf("want errPipelineIncomplete, got %v", err)
	}
	if incomplete.candidate != "cand-a" || incomplete.window != 1 {
		t.Fatalf("names the wrong unit: %+v", incomplete)
	}
	if got := resilience.ClassifyError(err); got != resilience.BenchmarkEvidenceIncomplete {
		t.Fatalf("class %s", got)
	}
	// Once the gap is filled the check passes.
	sh.encodeOut[1] = pipelineEncodeOutcome{entry: BenchmarkCandidateSampleResult{CandidateID: "cand-a", SampleIndex: 1, SizeBytes: 10}}
	sh.metricOut[1] = pipelineMetricOutcome{executed: true}
	if err := sh.checkComplete(context.Background(), units); err != nil {
		t.Fatal(err)
	}
	// A recorded failure is reported by its own path, never double-reported.
	sh.metricOut[1] = pipelineMetricOutcome{}
	sh.fatal.set(errors.New("encode broke"), false)
	if err := sh.checkComplete(context.Background(), units); err != nil {
		t.Fatalf("a recorded failure must stay the reported error: %v", err)
	}
}

func TestMergeAutomaticQualityReportsContextBeforeMissingEvidence(t *testing.T) {
	record := &BenchmarkRecord{Samples: []transcode.BenchmarkSampleWindow{{Index: 3}, {Index: 4}}, Quality: &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h"}}}
	vc := validatedCandidate{candidate: transcode.BenchmarkCandidate{ID: "cand-a"}}
	evidence := &BenchmarkExecutionEvidence{}

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errSearchBudgetStopped)
	err := mergeAutomaticQuality(ctx, record, evidence, vc)
	if !errors.Is(err, errSearchBudgetStopped) || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}

	// A live context with a real gap names the candidate and the window.
	err = mergeAutomaticQuality(context.Background(), record, evidence, vc)
	if err == nil || !strings.Contains(err.Error(), "missing automatic verification sample") || !strings.Contains(err.Error(), "cand-a") || !strings.Contains(err.Error(), "window 3") {
		t.Fatalf("gap not named: %v", err)
	}
	if got := resilience.ClassifyError(err); got != resilience.BenchmarkEvidenceIncomplete {
		t.Fatalf("class %s", got)
	}
}

// The verification windows run, the budget cancels the run, and the hook leaves
// no rows. That is a bounded stop, not a missing sample.
func TestAutomaticVerificationCancelledByBudgetIsNotMissingSample(t *testing.T) {
	for _, untilExhausted := range []bool{false, true} {
		name := map[bool]string{false: "standard", true: "verify_until_exhausted"}[untilExhausted]
		t.Run(name, func(t *testing.T) {
			p5 := 93.0
			record := &BenchmarkRecord{Mode: "quality", ModePolicyVersion: optimization.ModePolicyV2, Automatic: true, Metric: "vmaf", SourceDuration: 100, Quality: &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Target: 96, Minimum: 95, P5Minimum: &p5, GuardrailEnforcement: "reject"}}}
			record.searchBudget = newSearchBudget(time.Now(), 600, testSearchExtension())
			for i, start := range []float64{15, 45, 75, 30, 60} {
				record.Samples = append(record.Samples, transcode.BenchmarkSampleWindow{Index: i, StartSeconds: start, DurationSeconds: 10})
			}
			if untilExhausted {
				record.AutomaticSearch = &transcode.AutomaticSearchRules{VerifyUntilExhausted: true}
			}
			vcs := []validatedCandidate{{index: 0, candidate: transcode.BenchmarkCandidate{ID: "initial", VideoCodec: "libx265", Quality: 20, Preset: "medium"}, bitDepth: 8}}
			evidence := &BenchmarkExecutionEvidence{SourceBitDepth: 8}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			runner := &ProductionBenchmarkRunner{automaticHook: func(_ context.Context, stage *BenchmarkRecord, e *BenchmarkExecutionEvidence, candidates []validatedCandidate) error {
				if stage.Samples[0].Index == 3 {
					cancel(errSearchBudgetStopped)
					return nil // the pipeline gave up quietly: no rows for the holdout windows
				}
				for _, vc := range candidates {
					var scores []optimization.SampleScore
					for _, window := range stage.Samples {
						frames := make([]map[string]any, 100)
						for i := range frames {
							frames[i] = map[string]any{"frameNum": i, "metrics": map[string]any{"vmaf": 98.0}}
						}
						data, _ := json.Marshal(map[string]any{"frames": frames})
						model, _ := quality.Model("v1_1080p_3h")
						stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, window.Index, window.StartSeconds, 25, 1, 4, nil)
						if err != nil {
							t.Fatal(err)
						}
						e.CandidateSamples = append(e.CandidateSamples, BenchmarkCandidateSampleResult{CandidateID: vc.candidate.ID, SampleIndex: window.Index, SizeBytes: 8000, EncodeDurationSec: 1})
						e.MetricSamples = append(e.MetricSamples, BenchmarkMetricSampleResult{CandidateID: vc.candidate.ID, SampleIndex: window.Index, VMAF: &stats.Mean, VMAFStats: &stats})
						scores = append(scores, optimization.SampleScore{SampleIndex: window.Index, Score: 98, Valid: true})
					}
					e.CandidateMetrics = append(e.CandidateMetrics, BenchmarkCandidateMetricAggregate{CandidateID: vc.candidate.ID, MetricType: optimization.MetricTypeVMAF, Aggregate: optimization.AggregateSampleScores(optimization.MetricTypeVMAF, scores)})
				}
				return nil
			}}
			progress := newBenchmarkProgressReporter(nil, record, 100)
			err := runner.runAutomaticCandidates(ctx, nil, record, evidence, "", vcs, mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 25}}}, 100000, time.Time{}, progress)
			if err != nil && strings.Contains(err.Error(), "missing automatic verification sample") {
				t.Fatalf("a budget stop was reported as missing evidence: %v", err)
			}
			if untilExhausted {
				// A graceful stop may return a result with the time cap recorded.
				if err == nil && (evidence.Automatic == nil || evidence.Automatic.Verification == nil || evidence.Automatic.Verification.StoppedBy != transcode.VerificationStoppedTimeCap) {
					t.Fatalf("stopped search did not record the time cap: %+v", evidence.Automatic)
				}
				return
			}
			if !errors.Is(err, context.Canceled) || !errors.Is(err, errSearchBudgetStopped) {
				t.Fatalf("budget stop lost: %v", err)
			}
		})
	}
}

func TestBenchmarkFailureClassOnlyFromTypedErrors(t *testing.T) {
	if got := benchmarkFailureClass(errors.New("some failure")); got != "" {
		t.Fatalf("text must not be classified by the worker: %q", got)
	}
	if got := benchmarkFailureClass(errPipelineIncomplete{candidate: "c", window: 1}); got != string(resilience.BenchmarkEvidenceIncomplete) {
		t.Fatalf("got %q", got)
	}
	if got := benchmarkFailureClass(internalInvariantError{"reference gone"}); got != string(resilience.InternalInvariant) {
		t.Fatalf("got %q", got)
	}
	// Only a failed ffmpeg process earns ffmpeg_unknown.
	proc := &ffmpegExecutionError{cause: errors.New("encoding candidate failed: exit status 1")}
	if got := benchmarkFailureClass(proc); got != string(resilience.FFmpegUnknown) {
		t.Fatalf("got %q", got)
	}
}

// The failed record carries the worker's own class for typed failures, leaves
// it empty for untyped ones, and never attaches one to a budget stop.
func TestInternalBenchmarkRecordsTheFailureClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"incomplete evidence", errPipelineIncomplete{candidate: "cand-a", window: 2}, string(resilience.BenchmarkEvidenceIncomplete)},
		{"ffmpeg process failure", &ffmpegExecutionError{cause: errors.New("encoding candidate failed: exit status 1")}, string(resilience.FFmpegUnknown)},
		{"untyped failure", errors.New("source media modified during benchmark (integrity violation, fail closed)"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newBudgetWorker(t)
			result, err := runBudgetBenchmark(t, w, 10, time.Minute, &transcode.SearchExtension{MaxSeconds: 60, SteadySeconds: 30}, func(ctx context.Context, record *BenchmarkRecord) error {
				return tc.err
			})
			if err == nil || result.Status != "failed" || result.ReasonCode != "" {
				t.Fatalf("err=%v result=%+v", err, result)
			}
			if result.FailureClassification != tc.want {
				t.Fatalf("class %q, want %q", result.FailureClassification, tc.want)
			}
		})
	}
	w := newBudgetWorker(t)
	result, _ := runBudgetBenchmark(t, w, 10, 300*time.Millisecond, &transcode.SearchExtension{MaxSeconds: 60, SteadySeconds: 30}, func(ctx context.Context, record *BenchmarkRecord) error {
		return waitDone(ctx)
	})
	if result.ReasonCode != "search_budget_exhausted" || result.FailureClassification != "" {
		t.Fatalf("a budget stop is parked as attention, not classified as a failure: %+v", result)
	}
}
