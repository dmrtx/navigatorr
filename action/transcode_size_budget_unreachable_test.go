package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A source whose video stream is 100 MB of a 1 GB file leaves about 900 MB for
// everything else, more than a 300 MB budget.
const unreachableProbeJSON = `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080,"avg_frame_rate":"24/1","r_frame_rate":"24/1","bits_per_raw_sample":"8","tags":{"NUMBER_OF_BYTES":"100000000"}}],"format":{"format_name":"matroska","duration":"6000","size":"1000000000"},"chapters":[]}`

func TestUnreachableSizeBudgetEndsBeforeAnyWork(t *testing.T) {
	for name, inputs := range map[string]map[string]any{
		"hardware allowed":     {"size_budget_bytes": 300_000_000.0},
		"hardware not allowed": {"size_budget_bytes": 300_000_000.0, "allow_hardware": false},
	} {
		t.Run(name, func(t *testing.T) {
			c := newBudgetCase(t, inputs, budgetCaps())
			delete(c.ec.State, "benchmark_job_id")
			delete(c.ec.State, "benchmark_request")
			c.ec.State["optimization_policy"] = c.snapshot.Profile.Optimization
			c.ec.State["resolved_path"] = "/media/Show/S01E01.mkv"
			res, err := c.engine.stepBenchmarkSubmit(context.Background(), c.ec)
			if err != nil || res.Status != StepCompleted {
				t.Fatalf("submit step %+v %v", res, err)
			}
			if c.executor.benchmarkSubmitCalls != 0 || c.executor.submitCalls != 0 {
				t.Fatalf("work submitted: benchmarks=%d encodes=%d", c.executor.benchmarkSubmitCalls, c.executor.submitCalls)
			}
			if !getBool(c.ec.State, "skip_transcode") || getString(c.ec.State, "reason_code") != SizeBudgetExceeded {
				t.Fatalf("not ended: %+v", c.ec.State)
			}
			if getString(c.ec.State, "benchmark_job_id") != "" || getString(c.ec.State, "candidate_path") != "" || getString(c.ec.State, "job_id") != "" {
				t.Fatalf("job or candidate state: %+v", c.ec.State)
			}
			rec := sizeBudgetOf(c.ec.State)
			if rec == nil || rec.Outcome != SizeBudgetExceeded || rec.Trigger != sizeBudgetTriggerUnreachable || rec.BudgetBytes != 300_000_000 {
				t.Fatalf("record %+v", rec)
			}
			for _, want := range []string{"alone take", formatBudgetBytes(rec.NonVideoBytes), "300 MB", "At least", "would be needed", "Original preserved"} {
				if !strings.Contains(rec.Message, want) {
					t.Fatalf("message %q lacks %q", rec.Message, want)
				}
			}
			// The smallest budget really is plannable and one byte less is not.
			policy := c.snapshot.Size.SizeBudget
			min := sizeBudgetMinimumBudget(policy, rec.NonVideoBytes, rec.DurationSeconds)
			if enc := planSizeBudgetEncode(policy, getSourceReport(c.ec.State["source_report"]), budgetSourceBytes, min); enc.Reason != "" {
				t.Fatalf("the stated minimum %d is unreachable: %s", min, enc.Reason)
			}
			if enc := planSizeBudgetEncode(policy, getSourceReport(c.ec.State["source_report"]), budgetSourceBytes, min-1000); enc.Reason != "size_budget_unreachable" {
				t.Fatalf("a budget below the stated minimum plans: %+v", enc)
			}
		})
	}
}

func TestOtherSizeBudgetReasonsLeaveTheSearchToRun(t *testing.T) {
	for name, tc := range map[string]struct {
		inputs map[string]any
		mutate func(*budgetCase)
	}{
		"budget the file can meet": {inputs: map[string]any{"size_budget_percent": 40.0}},
		"no budget":                {},
		"estimate unavailable": {inputs: map[string]any{"size_budget_bytes": 300_000_000.0}, mutate: func(c *budgetCase) {
			rep := budgetReport()
			rep.Video[0].Tags, rep.Audio[0].BitRate = nil, 0
			c.ec.State["source_report"] = rep
		}},
		"sibling budget without evidence": {inputs: map[string]any{"size_budget_source": "siblings"}},
		"search already started":          {inputs: map[string]any{"size_budget_bytes": 300_000_000.0}, mutate: func(c *budgetCase) { c.ec.State["benchmark_job_id"] = "bench-1" }},
		"quality mode":                    {inputs: map[string]any{"size_budget_bytes": 300_000_000.0}, mutate: func(c *budgetCase) { c.ec.Inputs["mode"] = "quality" }},
	} {
		t.Run(name, func(t *testing.T) {
			c := newBudgetCase(t, tc.inputs, budgetCaps())
			if tc.mutate == nil || name != "search already started" {
				delete(c.ec.State, "benchmark_job_id")
			}
			if tc.mutate != nil {
				tc.mutate(c)
			}
			if res, ended := endUnreachableSizeBudget(c.ec, c.snapshot, getSourceReport(c.ec.State["source_report"]), budgetSourceBytes, nil); ended {
				t.Fatalf("ended: %+v", res)
			}
			if c.ec.State["skip_transcode"] != nil || c.ec.State["reason_code"] != nil || sizeBudgetOf(c.ec.State) != nil {
				t.Fatalf("state changed: %+v", c.ec.State)
			}
		})
	}
}

func unreachablePreflight(t *testing.T, inputs map[string]any, size int64) (StepResult, *ExecutionContext, *Engine) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(file, size); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{capabilitiesFunc: nil}, writeProbeScript(t, dir, unreachableProbeJSON), []string{dir}, []string{dir}, false)
	t.Cleanup(func() { st.Close() })
	for i := 0; i < mediaHashConcurrency; i++ {
		e.hashSlots <- struct{}{}
	}
	in := map[string]any{"path": file, "mode": "size"}
	for k, v := range inputs {
		in[k] = v
	}
	ec := &ExecutionContext{InstanceID: "unreachable-budget", ActionName: "transcode_media", Inputs: in, State: map[string]any{}, Outputs: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := e.stepTranscodePreflight(ctx, ec)
	if err != nil {
		t.Fatal(err)
	}
	return result, ec, e
}

func TestPreflightEndsAnUnreachableBudgetBeforeHashingTheSource(t *testing.T) {
	// Every hash slot is held: a hash would fail the step.
	result, ec, e := unreachablePreflight(t, map[string]any{"size_budget_bytes": 300_000_000.0}, 1_000_000_000)
	if result.Status != StepCompleted || getString(result.Outputs, "reason_code") != SizeBudgetExceeded || !getBool(ec.State, "skip_transcode") {
		t.Fatalf("preflight %+v state=%+v", result, ec.State)
	}
	if _, ok := ec.State["original_sha256"]; ok {
		t.Fatal("source hashed")
	}
	if rec := sizeBudgetOf(ec.State); rec == nil || rec.Outcome != SizeBudgetExceeded || rec.NonVideoBytes < 800_000_000 {
		t.Fatalf("record %+v", rec)
	}
	if getString(ec.State, "resolved_path") == "" || getInt64(ec.State, "original_size") != 1_000_000_000 {
		t.Fatalf("source identity lost: %+v", ec.State)
	}
	// The ended action accepts without reading the original.
	if err := os.Remove(getString(ec.State, "resolved_path")); err != nil {
		t.Fatal(err)
	}
	accepted, err := e.stepTranscodeAccept(context.Background(), ec)
	if err != nil || accepted.Status != StepCompleted || accepted.Outputs["reason_code"] != SizeBudgetExceeded || accepted.Outputs["original_integrity"] != "not_applicable" {
		t.Fatalf("accept %+v %v", accepted, err)
	}
}

func TestPreflightKeepsABudgetTheFileCanMeet(t *testing.T) {
	// 500 MB leaves room: 900 MB of other streams is not needed, only a
	// reachable budget (here 85% of the source) is.
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, []byte("source bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, writeProbeScript(t, dir, sdrProbeJSON), []string{dir}, []string{dir}, false)
	defer st.Close()
	ec := &ExecutionContext{InstanceID: "reachable-budget", ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "size", "size_budget_percent": 40.0}, State: map[string]any{}, Outputs: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	result, err := e.stepTranscodePreflight(context.Background(), ec)
	if err != nil || result.Status != StepCompleted || getBool(ec.State, "skip_transcode") || getString(ec.State, "original_sha256") == "" {
		t.Fatalf("reachable budget ended or skipped the hash: %+v %v state=%+v", result, err, ec.State)
	}
}
