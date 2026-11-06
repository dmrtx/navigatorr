package action

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

func saveSpaceHDRFixture(t *testing.T, probe string) (*Engine, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, make([]byte, 100), 0600); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, writeProbeScript(t, dir, probe), []string{dir}, []string{dir}, false)
	t.Cleanup(func() { st.Close() })
	return e, st, file
}

func saveSpaceHDRPreflight(t *testing.T, e *Engine, ec *ExecutionContext) StepResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := e.stepTranscodePreflight(ctx, ec)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertRoutedToShrink(t *testing.T, ec *ExecutionContext, keepHDR bool) *ModePolicySnapshot {
	t.Helper()
	if ec.Inputs["mode"] != "compact" || (ec.Inputs["keep_hdr"] == true) != keepHDR || ec.Inputs["max_height"] != float64(2160) || ec.Inputs[saveSpaceKeepHDRInput] != nil {
		t.Fatalf("inputs %+v", ec.Inputs)
	}
	if err := validateModeInputs(ec.Inputs); err != nil {
		t.Fatalf("the routed inputs are refused: %v", err)
	}
	policy, err := freezeModePolicy(ec)
	if err != nil || policy.Mode != "compact" || policy.Compact.KeepHDR != keepHDR || policy.Compact.MaxHeight != 2160 || policy.Compact.ChainVersion != transcode.CompactChainVersion {
		t.Fatalf("frozen %+v %v", policy, err)
	}
	return policy
}

// The file decides in preflight: every later step, the worker and the history
// see the goal that really runs, and the request stays as it was sent.
func TestSaveSpacePreflightRunsAnHDRSourceAsShrinkInSDR(t *testing.T) {
	e, st, file := saveSpaceHDRFixture(t, hdrProbeJSON)
	ec := &ExecutionContext{InstanceID: "routed", ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "size", "allow_hardware": true, "keep_languages": []any{"jpn"}}, State: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	result := saveSpaceHDRPreflight(t, e, ec)
	if getString(result.Outputs, "reason_code") == "hdr_requires_compact" {
		t.Fatalf("skipped: %+v", result)
	}
	policy := assertRoutedToShrink(t, ec, false)
	if policy.Tracks == nil || len(policy.Tracks.Languages) != 1 || policy.Tracks.Languages[0] != "jpn" {
		t.Fatalf("languages lost: %+v", policy.Tracks)
	}
	if ec.Inputs["allow_hardware"] != nil {
		t.Fatalf("hardware kept on a goal without it: %+v", ec.Inputs)
	}
	request, _ := ec.Inputs[SaveSpaceRequestKey].(map[string]any)
	if request["mode"] != "size" || request["allow_hardware"] != true {
		t.Fatalf("request lost: %+v", request)
	}
	digest := policy.Digest
	again := requeuedSaveSpaceHDR(t, e, ec)
	if policy := assertRoutedToShrink(t, again, false); policy.Digest != digest {
		t.Fatalf("a repeated preflight froze %s, then %s", digest, policy.Digest)
	}
	if request, _ := again.Inputs[SaveSpaceRequestKey].(map[string]any); request["mode"] != "size" {
		t.Fatalf("a repeated preflight replaced the request: %+v", request)
	}
	if _, err := validateTranscodeMedia(context.Background(), e, map[string]any{"path": file, "mode": "size", SaveSpaceRequestKey: map[string]any{}}); err == nil {
		t.Fatal("a client sent the routing record")
	}
}

// requeuedSaveSpaceHDR runs a routed action's preflight again from what a node
// pause leaves stored: its inputs, and of its state only the frozen policy.
func requeuedSaveSpaceHDR(t *testing.T, e *Engine, ec *ExecutionContext) *ExecutionContext {
	t.Helper()
	var inputs, state map[string]any
	if json.Unmarshal([]byte(toJSON(ec.Inputs)), &inputs) != nil || json.Unmarshal([]byte(toJSON(ec.State)), &state) != nil {
		t.Fatal("stored round trip")
	}
	again := &ExecutionContext{InstanceID: ec.InstanceID, ActionName: ec.ActionName, Inputs: inputs, State: map[string]any{"mode_policy": state["mode_policy"]}}
	if result := saveSpaceHDRPreflight(t, e, again); result.Status == StepFailed {
		t.Fatalf("preflight again: %+v", result)
	}
	return again
}

// A retry of a lost response finds the action without reading the file, which
// may be gone by then; a different request under the same key does not.
func TestSaveSpaceHDRReplayFindsTheActionWithoutTheFile(t *testing.T) {
	e, st, file := saveSpaceHDRFixture(t, hdrProbeJSON)
	sent := map[string]any{"path": file, "mode": "size", "allow_hardware": true}
	ctx := WithOrigin(context.Background(), "web")
	first, err := e.Enqueue(ctx, "transcode_media", sent, "replay-key")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.GetActionInstance(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(inst, e)
	saveSpaceHDRPreflight(t, e, ec)
	assertRoutedToShrink(t, ec, false)
	inst.InputsJSON = toJSON(ec.Inputs)
	if err := st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	second, err := e.Enqueue(ctx, "transcode_media", sent, "replay-key")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay made %s and %s", first.ID, second.ID)
	}
	if _, err := e.Enqueue(ctx, "transcode_media", map[string]any{"path": file, "mode": "size"}, "replay-key"); err == nil {
		t.Fatal("a different request took the routed action's key")
	}
	matches, err := immutableActionInputsMatch(inst.InputsJSON, ec.Inputs)
	if err != nil || !matches {
		t.Fatalf("the stored inputs no longer match themselves: %v %v", matches, err)
	}
}

func TestSaveSpaceRouteLeavesEveryOtherRequestAlone(t *testing.T) {
	e, st, file := saveSpaceHDRFixture(t, hdrProbeJSON)
	for name, extra := range map[string]map[string]any{
		"quality":        {"mode": "quality"},
		"percent budget": {"mode": "size", "size_budget_percent": float64(40)},
		"bytes budget":   {"mode": "size", "size_budget_bytes": float64(1e9)},
		"smaller saving": {"mode": "size", "accept_smaller_saving": true},
	} {
		extra["path"] = file
		ec := &ExecutionContext{InstanceID: "left-" + name, ActionName: "transcode_media", Inputs: extra, State: map[string]any{}}
		seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
		if result := saveSpaceHDRPreflight(t, e, ec); getString(result.Outputs, "reason_code") != "hdr_requires_compact" || ec.Inputs["keep_hdr"] != nil || ec.Inputs[SaveSpaceRequestKey] != nil {
			t.Errorf("%s: %+v inputs %+v", name, result, ec.Inputs)
		}
	}
	audio := map[string]any{"mode": "size", "audio_only": true, "path": file}
	if saveSpaceTakesHDR(audio) {
		t.Error("audio only takes the HDR route")
	}
	sdr, sdrStore, sdrFile := saveSpaceHDRFixture(t, sdrProbeJSON)
	ec := &ExecutionContext{InstanceID: "sdr", ActionName: "transcode_media", Inputs: map[string]any{"path": sdrFile, "mode": "size"}, State: map[string]any{}}
	seedActionInstance(t, sdrStore, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	saveSpaceHDRPreflight(t, sdr, ec)
	if ec.Inputs["mode"] != "size" || ec.Inputs[SaveSpaceRequestKey] != nil {
		t.Errorf("SDR source routed: %+v", ec.Inputs)
	}
}

// The batch lists the file under the goal it was asked for and writes nothing
// per file, so a crash mid-inventory cannot lose a decision; the child routes
// itself, past the parent's goal, with the parent's languages.
func TestSaveSpaceBatchChildRoutesItself(t *testing.T) {
	e, st, file := saveSpaceHDRFixture(t, hdrProbeJSON)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := e.Run(ctx, "transcode_batch", map[string]any{"paths": []any{file}, "mode": "size", "keep_languages": []any{"jpn"}, "dry_run": true})
	if err != nil {
		t.Fatal(err)
	}
	items, err := st.ListTranscodeBatchItems(res.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("items %+v %v", items, err)
	}
	if items[0].Decision != "transcode" || items[0].Profile != "mode-size" {
		t.Fatalf("item %+v", items[0])
	}
	inst, err := st.GetActionInstance(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows := stateObject(parseExecutionContext(inst, e).State, "batch_item_settings"); len(rows) != 0 {
		t.Fatalf("per-file rows %+v", rows)
	}
	child := &ExecutionContext{InstanceID: "child", ActionName: "transcode_media", Inputs: map[string]any{"path": items[0].FilePath, "mode": "size", "parent_action_id": res.ID, "batch_item_key": items[0].ItemKey}, State: map[string]any{}}
	seedActionInstance(t, st, child.InstanceID, child.ActionName, StatusRunning, 0, "", child.Inputs, child.State)
	saveSpaceHDRPreflight(t, e, child)
	policy := assertRoutedToShrink(t, child, false)
	if policy.Tracks == nil || len(policy.Tracks.Languages) != 1 || policy.Tracks.Languages[0] != "jpn" {
		t.Fatalf("languages lost: %+v", policy.Tracks)
	}
	// A requeued child must not fight the parent's goal.
	assertRoutedToShrink(t, requeuedSaveSpaceHDR(t, e, child), false)
	// It still answers to the parent: a file taken out of the batch stops.
	items[0].Decision = "skip"
	if err := st.UpdateTranscodeBatchItem(items[0]); err != nil {
		t.Fatal(err)
	}
	if result := saveSpaceHDRPreflight(t, e, child); result.Status != StepFailed || result.Error != "mode source is not a member of parent inventory" {
		t.Fatalf("a removed file ran again: %+v", result)
	}
}

func TestSaveSpaceBatchKeepsSkippingWhatTheRouteDoesNotTake(t *testing.T) {
	e, st, file := saveSpaceHDRFixture(t, hdrProbeJSON)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for name, extra := range map[string]map[string]any{
		"size limit":     {"size_budget_percent": float64(40)},
		"smaller saving": {"accept_smaller_saving": true},
	} {
		inputs := map[string]any{"paths": []any{file}, "mode": "size", "dry_run": true}
		for key, value := range extra {
			inputs[key] = value
		}
		res, err := e.Run(ctx, "transcode_batch", inputs)
		if err != nil {
			t.Fatal(err)
		}
		items, err := st.ListTranscodeBatchItems(res.ID)
		if err != nil || len(items) != 1 || items[0].Status != "skip" || len(items[0].Reasons) != 1 || items[0].Reasons[0] != "hdr_requires_compact" {
			t.Fatalf("%s: %+v %v", name, items, err)
		}
	}
}

// A batch moved to Save space later reaches the file through its goal row.
func TestSaveSpaceGoalChangeRoutesAnHDRChild(t *testing.T) {
	e, st, file := saveSpaceHDRFixture(t, hdrProbeJSON)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := e.Run(ctx, "transcode_batch", map[string]any{"paths": []any{file}, "mode": "compact", "keep_hdr": true, "dry_run": true})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.GetActionInstance(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	items, err := st.ListTranscodeBatchItems(res.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("items %+v %v", items, err)
	}
	ec := parseExecutionContext(inst, e)
	sizePolicy, err := modePolicyGoal("size", false, 0, nil, false, nil, nil, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	stateObject(ec.State, "batch_item_settings")[items[0].ItemKey] = map[string]any{"mode": "size", "mode_policy": sizePolicy}
	inst.StateJSON = toJSON(ec.State)
	if err := st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	child := &ExecutionContext{InstanceID: "goal-child", ActionName: "transcode_media", Inputs: map[string]any{"path": items[0].FilePath, "mode": "size", "parent_action_id": res.ID, "batch_item_key": items[0].ItemKey}, State: map[string]any{}}
	seedActionInstance(t, st, child.InstanceID, child.ActionName, StatusRunning, 0, "", child.Inputs, child.State)
	saveSpaceHDRPreflight(t, e, child)
	assertRoutedToShrink(t, child, false)
}

func TestRoutedRequestSurvivesTheStoredRoundTrip(t *testing.T) {
	stored := map[string]any{"path": "x", "mode": "compact", "keep_hdr": true, "replace_when_done": true, SaveSpaceRequestKey: map[string]any{"path": "x", "mode": "size", "replace_when_done": true, "idempotency_key": "k"}}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	for _, requested := range []map[string]any{
		{"path": "x", "mode": "size"},
		{"path": "x", "mode": "size", "replace_when_done": true},
	} {
		if matches, err := immutableActionInputsMatch(string(raw), requested); err != nil || !matches {
			t.Errorf("%+v did not match: %v", requested, err)
		}
	}
	if matches, _ := immutableActionInputsMatch(string(raw), map[string]any{"path": "x", "mode": "size", "replace_when_done": false}); matches {
		t.Error("a replay that says not to replace found the job that does")
	}
	if matches, _ := immutableActionInputsMatch(string(raw), map[string]any{"path": "x", "mode": "compact", "keep_hdr": true}); matches {
		t.Error("a Shrink request found a routed Save space job")
	}
}

// Keeping HDR is the request's choice, and a batch child inherits it.
func TestSaveSpaceKeepsHDROnlyWhenAsked(t *testing.T) {
	e, st, file := saveSpaceHDRFixture(t, hdrProbeJSON)
	ec := &ExecutionContext{InstanceID: "kept", ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "size", saveSpaceKeepHDRInput: true}, State: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	saveSpaceHDRPreflight(t, e, ec)
	policy := assertRoutedToShrink(t, ec, true)
	if request, _ := ec.Inputs[SaveSpaceRequestKey].(map[string]any); request[saveSpaceKeepHDRInput] != true {
		t.Fatalf("request lost the choice: %+v", request)
	}
	if again := assertRoutedToShrink(t, requeuedSaveSpaceHDR(t, e, ec), true); again.Digest != policy.Digest {
		t.Fatalf("a repeated preflight froze %s, then %s", policy.Digest, again.Digest)
	}
	if !slices.Contains(modeChildInputs, saveSpaceKeepHDRInput) {
		t.Fatal("a batch child would lose the choice")
	}
	if err := validateModeInputs(map[string]any{"path": file, "mode": "compact", saveSpaceKeepHDRInput: true}); err == nil {
		t.Fatal("a Shrink request accepted save_space_keep_hdr")
	}
}
