package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// preExtensionSizeV3Digest is the size v3 digest from before search
// extensions existed. Operations frozen then still carry it.
const preExtensionSizeV3Digest = "sha256:c7a085119dcbdfca077be0f37cb27f7aff8158afc038a32a1b5061af63a26ca3"

// legacySizeSnapshot is a size v3 policy as frozen before search extensions:
// the same version string, no extension, and the digest it had then.
func legacySizeSnapshot(t *testing.T) ModePolicySnapshot {
	t.Helper()
	p, err := modePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	p.Size.SearchExtension = nil
	p.Size.AutomaticSearch = nil
	p.Digest = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return p
}

func TestSizeModePolicyV3FreezesSearchExtension(t *testing.T) {
	p, err := modePolicy("size", false)
	if err != nil {
		t.Fatal(err)
	}
	ext := p.Size.SearchExtension
	if ext == nil || ext.MaxSeconds != 2400 || ext.SteadySeconds != 300 || ext.VerifySeconds != 600 || p.SearchBudgetSeconds != 600 {
		t.Fatalf("size policy extension %+v base=%d", ext, p.SearchBudgetSeconds)
	}
	if got := p.searchExtension(); got == nil || got.TotalSeconds() != 3000 {
		t.Fatalf("policy does not expose its extension: %+v", got)
	}

	legacy := legacySizeSnapshot(t)
	if legacy.Digest != preExtensionSizeV3Digest {
		t.Fatalf("an operation frozen before extensions no longer re-hashes to its digest: %s", legacy.Digest)
	}
	if legacy.Digest == p.Digest || legacy.Version != p.Version {
		t.Fatalf("a new policy must differ in digest only: new=%s legacy=%s", p.Digest, legacy.Digest)
	}
	if raw, _ := json.Marshal(legacy); strings.Contains(string(raw), "search_extension") {
		t.Fatalf("legacy snapshot mentions the extension: %s", raw)
	}

	// The legacy snapshot verifies, and keeps its fixed budget.
	ec := &ExecutionContext{InstanceID: "legacy-size", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": legacy}}
	frozen, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatalf("a frozen pre-extension policy was rejected: %v", err)
	}
	if frozen.Digest != preExtensionSizeV3Digest || frozen.searchExtension() != nil {
		t.Fatalf("legacy policy gained an extension: %+v", frozen)
	}
	// So does a stored (JSON round-tripped) copy of the new policy.
	stored := map[string]any{}
	raw, _ := json.Marshal(p)
	_ = json.Unmarshal(raw, &stored)
	ec = &ExecutionContext{InstanceID: "new-size", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": stored}}
	if frozen, err = freezeModePolicy(ec); err != nil || frozen.searchExtension() == nil {
		t.Fatalf("stored new policy did not verify with its extension: %+v %v", frozen, err)
	}
	// Tampering with the frozen extension breaks the digest.
	stored["size"].(map[string]any)["search_extension"].(map[string]any)["max_seconds"] = float64(3600)
	if _, err := freezeModePolicy(&ExecutionContext{InstanceID: "tampered", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"mode_policy": stored}}); err == nil {
		t.Fatal("an edited extension passed the digest check")
	}
}

func TestOtherModesKeepAFixedSearchBudget(t *testing.T) {
	for _, mode := range []string{"quality", "x265_preserve", "compact"} {
		p, err := modePolicy(mode, false)
		if err != nil {
			t.Fatal(err)
		}
		if p.searchExtension() != nil || searchCeilingSeconds(p, p.SearchBudgetSeconds) != 600 {
			t.Fatalf("%s gained an extended budget", mode)
		}
	}
}

func TestSearchCeilingSeconds(t *testing.T) {
	elastic, _ := modePolicy("size", false)
	legacy := legacySizeSnapshot(t)
	quality, _ := modePolicy("quality", false)
	narrow := elastic
	narrow.Size = &ModeSizePolicy{SearchExtension: &transcode.SearchExtension{MaxSeconds: 500, SteadySeconds: 180}}
	for name, tc := range map[string]struct {
		policy any
		want   int
	}{
		"extended size":                 {elastic, 3000},
		"stored extended size":          {map[string]any{"version": elastic.Version, "mode": "size", "size": map[string]any{"search_extension": map[string]any{"max_seconds": 2400, "steady_seconds": 180, "verify_seconds": 600}}}, 3000},
		"size frozen before extensions": {legacy, 600},
		"other mode":                    {quality, 600},
		"cap not above the base":        {narrow, 600},
		"no policy":                     {nil, 600},
		"unreadable policy":             {"nonsense", 600},
	} {
		if got := searchCeilingSeconds(tc.policy, 600); got != tc.want {
			t.Errorf("%s: ceiling %d want %d", name, got, tc.want)
		}
	}
}

func TestReserveModeSearchReservesTheLongestSearch(t *testing.T) {
	reserved := func(state map[string]any) float64 {
		ec := &ExecutionContext{InstanceID: "reserve", Inputs: map[string]any{"mode": "size"}, State: state}
		if !reserveModeSearch(ec, "child") {
			t.Fatal("reservation refused")
		}
		return modeBudgetEntries(ec)["child"].Reserved
	}
	elastic, _ := modePolicy("size", false)
	legacy := legacySizeSnapshot(t)
	if got := reserved(map[string]any{"mode_policy": elastic, "search_budget_seconds": 600}); got != 3000 {
		t.Errorf("extended search reserved %v, want its hard cap", got)
	}
	if got := reserved(map[string]any{"mode_policy": legacy, "search_budget_seconds": 600}); got != 600 {
		t.Errorf("fixed search reserved %v", got)
	}
	// State written before policies were frozen into it keeps the base.
	if got := reserved(map[string]any{"search_budget_seconds": 600}); got != 600 {
		t.Errorf("unfrozen state reserved %v", got)
	}
	// No base budget, nothing to reserve.
	ec := &ExecutionContext{State: map[string]any{"mode_policy": elastic}}
	if reserveModeSearch(ec, "child") {
		t.Fatal("reserved without a search budget")
	}
}

func TestBatchItemReservationFollowsItsOwnFrozenGoal(t *testing.T) {
	elastic, _ := modePolicy("size", false)
	legacy := legacySizeSnapshot(t)
	quality, _ := modePolicy("quality", false)
	ec := &ExecutionContext{State: map[string]any{"search_budget_seconds": 600, "mode_policy": legacy, "batch_item_settings": map[string]any{
		"revised":   map[string]any{"mode": "size", "mode_policy": elastic},
		"requality": map[string]any{"mode": "quality", "mode_policy": quality},
		"plain":     map[string]any{"profile": "x"},
	}}}
	for key, want := range map[string]int{"revised": 3000, "requality": 600, "plain": 600, "untouched": 600} {
		if got := batchItemSearchCeiling(ec, store.TranscodeBatchItem{ItemKey: key}); got != want {
			t.Errorf("%s: ceiling %d want %d", key, got, want)
		}
	}
	ec.State["mode_policy"] = elastic
	if got := batchItemSearchCeiling(ec, store.TranscodeBatchItem{ItemKey: "untouched"}); got != 3000 {
		t.Errorf("an item on the batch's extended goal: ceiling %d", got)
	}
}

func extensionRequestFor(t *testing.T, snapshot *ModePolicySnapshot, caps transcode.WorkerCapabilities) *transcode.BenchmarkRequest {
	t.Helper()
	ec := &ExecutionContext{InstanceID: "mode-ext", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
	if snapshot != nil {
		ec.State["mode_policy"] = *snapshot
	}
	policy, err := freezeModePolicy(ec)
	if err != nil {
		t.Fatal(err)
	}
	ec.State["plan"] = &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", ExpectedBitDepth: 8, AudioMode: policy.Profile.Audio.Mode}
	rep := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 24, BitRate: 5000000}}}
	req, err := buildBenchmarkRequest(ec, "/media/Sample Show - S01E01.mkv", rep, policy.Profile.Optimization, caps)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestBenchmarkRequestSendsTheSearchExtensionOnlyWhereItIsUnderstood(t *testing.T) {
	capable := modeTestCapabilities()
	capable.SearchBudgetExtension = true
	legacy := legacySizeSnapshot(t)

	req := extensionRequestFor(t, nil, capable)
	if req.SearchBudgetSeconds != 600 || req.SearchExtension == nil || req.SearchExtension.MaxSeconds != 2400 || req.SearchExtension.SteadySeconds != 300 || req.SearchExtension.VerifySeconds != 600 {
		t.Fatalf("a new operation on a capable worker: %+v", req.SearchExtension)
	}
	if err := req.SearchExtension.Validate(req.SearchBudgetSeconds); err != nil {
		t.Fatal(err)
	}
	// Mutating the request must not reach the frozen policy.
	req.SearchExtension.MaxSeconds = 1
	if again := extensionRequestFor(t, nil, capable); again.SearchExtension.MaxSeconds != 2400 {
		t.Fatal("request shares the frozen extension")
	}

	// An older worker rejects the unknown field, so it keeps the fixed budget.
	if old := extensionRequestFor(t, nil, modeTestCapabilities()); old.SearchExtension != nil {
		t.Fatalf("a worker without the capability was sent the field: %+v", old.SearchExtension)
	}
	// An operation frozen before extensions keeps its fixed budget anywhere.
	if got := extensionRequestFor(t, &legacy, capable); got.SearchExtension != nil || got.SearchBudgetSeconds != 600 {
		t.Fatalf("a legacy frozen operation was extended: %+v", got.SearchExtension)
	}
}

func busyExhaustedExecutor(busy bool) *mockTranscodeExecutor {
	return &mockTranscodeExecutor{benchmarkStatusFunc: func(_ context.Context, jobID string) (transcode.BenchmarkStatus, error) {
		return transcode.BenchmarkStatus{
			ProtocolVersion: transcode.WorkerProtocolVersion, ID: jobID, Status: transcode.StatusFailed,
			ReasonCode: "search_budget_exhausted", Error: "search_budget_exhausted: bounded search stopped; no claim about untested candidates",
			SearchWorkerBusy: busy, SearchExtension: &transcode.SearchExtension{MaxSeconds: 2400, SteadySeconds: 180, VerifySeconds: 600},
		}, nil
	}}
}

func exhaustedSearch(t *testing.T, busy bool, earlierBusy ...bool) (StepResult, *ExecutionContext) {
	t.Helper()
	e := NewEngine(EngineDeps{Transcode: busyExhaustedExecutor(busy)})
	ec := &ExecutionContext{InstanceID: "bench-exhausted", ActionName: "transcode_media", Inputs: map[string]any{"mode": "size"}, Outputs: map[string]any{}, State: map[string]any{"optimization_enabled": true, "benchmark_job_id": "bench-exhausted", "search_budget_seconds": 600, "policy_digest": "sha256:test"}}
	if len(earlierBusy) > 0 && earlierBusy[0] {
		ec.State["search_worker_busy"] = true
	}
	res, err := e.stepBenchmarkWait(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	return res, ec
}

func TestSearchOutOfTimeOnABusyWorkerSaysSoAndKeepsTheSameDecision(t *testing.T) {
	res, ec := exhaustedSearch(t, true)
	if res.Status != StepWaitingDecision || res.WaitingCondition != "search_budget_exhausted" || res.Outputs["reason_code"] != "search_budget_exhausted" {
		t.Fatalf("busy exhaustion changed the outcome: %+v", res)
	}
	if res.WaitingReason != busySearchBudgetReason || !strings.Contains(res.WaitingReason, "busy") || !strings.Contains(res.WaitingReason, "Try again") || !strings.Contains(res.WaitingReason, "Original preserved") {
		t.Fatalf("hint %q", res.WaitingReason)
	}
	// The UI shows reasons up to 140 characters whole.
	if len(res.WaitingReason) > 140 {
		t.Fatalf("hint is %d characters and would be truncated", len(res.WaitingReason))
	}
	// Still only a cancel option: no search or replacement starts by itself.
	if len(res.WaitingOptions) != 1 || res.WaitingOptions[0].Decision != "cancel" {
		t.Fatalf("options %+v", res.WaitingOptions)
	}
	if !getBool(ec.State, "search_worker_busy") || getInt(ec.State, "search_budget_max_seconds") != 2400 || getInt(ec.State, "search_budget_verify_seconds") != 600 {
		t.Fatalf("telemetry not recorded: %+v", ec.State)
	}
}

func TestSearchOutOfTimeOnAnIdleWorkerKeepsTheGenericMessage(t *testing.T) {
	res, ec := exhaustedSearch(t, false)
	if res.Status != StepWaitingDecision || res.WaitingCondition != "search_budget_exhausted" {
		t.Fatalf("outcome changed: %+v", res)
	}
	if res.WaitingReason != "Finite search budget exhausted without a reliable conclusion; original preserved" {
		t.Fatalf("an idle worker must not be blamed on other jobs: %q", res.WaitingReason)
	}
	if getBool(ec.State, "search_worker_busy") {
		t.Fatal("busy flag set without evidence")
	}
	// A busy flag left by an earlier attempt must not outlive a calm one.
	if _, ec := exhaustedSearch(t, false, true); getBool(ec.State, "search_worker_busy") {
		t.Fatal("an earlier attempt's busy flag was inherited")
	}
}

func TestObservedStatusDropsSearchTelemetryTheNewerAttemptLacks(t *testing.T) {
	e := NewEngine(EngineDeps{})
	ec := &ExecutionContext{InstanceID: "bench-stale", ActionName: "transcode_media", Inputs: map[string]any{}, Outputs: map[string]any{}, State: map[string]any{}}
	e.observeBenchmarkStatus(ec, transcode.BenchmarkStatus{Status: transcode.StatusFailed, SearchBudgetSeconds: 600, SearchWorkerBusy: true, SearchExtension: &transcode.SearchExtension{MaxSeconds: 2400, SteadySeconds: 180, VerifySeconds: 600}})
	if getInt(ec.State, "search_budget_max_seconds") != 2400 || getInt(ec.State, "search_budget_verify_seconds") != 600 {
		t.Fatalf("setup did not record telemetry: %+v", ec.State)
	}
	ec.State["search_worker_busy"] = true // as the exhausted wait leaves it
	// A later attempt on a worker without the extension, that finished in time.
	e.observeBenchmarkStatus(ec, transcode.BenchmarkStatus{Status: transcode.StatusCompleted, SearchBudgetSeconds: 600})
	if _, ok := ec.State["search_budget_verify_seconds"]; ok {
		t.Fatalf("the verification allowance outlived the attempt that had it: %+v", ec.State)
	}
	if _, ok := ec.State["search_budget_max_seconds"]; ok {
		t.Fatalf("the extended cap outlived the attempt that had it: %+v", ec.State)
	}
	if _, ok := ec.State["search_worker_busy"]; ok {
		t.Fatalf("the busy flag outlived the attempt that had it: %+v", ec.State)
	}
}

// A search whose verification the time limit cut short completes on the
// worker with its evidence and no winner. The action waits for a decision, as
// for any search out of time: it neither falls back to audio only nor ends as
// "no suitable candidate", and a busy worker is still named.
func TestVerificationCutShortByTheTimeLimitWaitsForADecision(t *testing.T) {
	for _, busy := range []bool{true, false} {
		exec := &mockTranscodeExecutor{benchmarkStatusFunc: func(_ context.Context, jobID string) (transcode.BenchmarkStatus, error) {
			return transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion, ID: jobID, Status: transcode.StatusCompleted,
				ReasonCode: "search_budget_exhausted", SearchWorkerBusy: busy,
				Automatic: &transcode.BenchmarkAutomaticEvidence{Verification: &transcode.BenchmarkVerificationEvidence{StoppedBy: transcode.VerificationStoppedTimeCap}},
			}, nil
		}}
		e := NewEngine(EngineDeps{Transcode: exec})
		ec := &ExecutionContext{InstanceID: "bench-cut-short", ActionName: "transcode_media", Inputs: map[string]any{"mode": "size"}, Outputs: map[string]any{}, State: map[string]any{"optimization_enabled": true, "benchmark_job_id": "bench-cut-short", "search_budget_seconds": 600, "policy_digest": "sha256:test"}}
		res, err := e.stepBenchmarkWait(context.Background(), ec)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != StepWaitingDecision || res.WaitingCondition != "search_budget_exhausted" || ec.State["skip_transcode"] != nil {
			t.Fatalf("busy=%v: %+v", busy, res)
		}
		if busy != getBool(ec.State, "search_worker_busy") {
			t.Fatalf("busy=%v but the action recorded %v", busy, ec.State["search_worker_busy"])
		}
		if ec.State["automatic_search"] == nil {
			t.Fatal("the search evidence was not kept")
		}
	}
}
