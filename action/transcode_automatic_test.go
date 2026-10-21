package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"testing"
)

func TestFinalValidationWindowsFollowSourceLengthAndLegacyFrozenPolicy(t *testing.T) {
	for _, tc := range []struct {
		duration float64
		windows  int
	}{{20, 2}, {50, 5}, {100, 5}, {3600, 5}} {
		ec := &ExecutionContext{InstanceID: "windows", Inputs: map[string]any{"mode": "quality"}, State: map[string]any{"plan": &transcode.Plan{VideoCodec: "libx265", Quality: 23, Preset: "medium", ExpectedBitDepth: 8, PixelFormat: "yuv420p", VideoProfile: "main"}}}
		policy, _ := freezeModePolicy(ec)
		req, err := finalRequestForPlan(ec, "/media/source.mkv", &mediainspect.DetailedReport{DurationSec: tc.duration, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 25}}}, policy.Profile.Optimization, modeTestCapabilities())
		if err != nil {
			t.Fatal(err)
		}
		if len(req.Samples) != tc.windows {
			t.Fatalf("%.0fs source: %d final windows, want %d", tc.duration, len(req.Samples), tc.windows)
		}
	}
	legacy, _ := legacyModePolicy("quality", false)
	legacy.Version = "transcode-modes-v1"
	legacy.MinSavingsPercent = 15
	legacy.Digest = ""
	data, _ := json.Marshal(legacy)
	sum := sha256.Sum256(data)
	legacy.Digest = "sha256:" + hex.EncodeToString(sum[:])
	ec := &ExecutionContext{Inputs: map[string]any{"mode": "quality"}, State: map[string]any{"mode_policy": legacy}}
	policy, err := freezeModePolicy(ec)
	if err != nil || policy.MinSavingsPercent != 15 || policy.Version != "transcode-modes-v1" {
		t.Fatal("existing policy was changed")
	}
}

func TestGoalRevisionFreezesNewAttemptWithoutChangingParent(t *testing.T) {
	st := setupTestStore(t)
	defer st.Close()
	e := NewEngine(EngineDeps{Store: st})
	seedActionInstance(t, st, "goal-parent", "transcode_batch", StatusCompleted, 3, "", map[string]any{"mode": "quality"}, map[string]any{})
	parent, _ := st.GetActionInstance("goal-parent")
	item := store.TranscodeBatchItem{BatchID: parent.ID, ItemKey: "file", FilePath: "/media/source.mkv", Decision: "skip", Status: "skip", Profile: "mode-quality"}
	if err := st.CreateTranscodeBatchItem(item); err != nil {
		t.Fatal(err)
	}
	items, _ := st.ListTranscodeBatchItems(parent.ID)
	_, err := e.ReconfigureBatch(context.Background(), parent.ID, "all", "", BatchSelectionVersion(parent, items), "", "new-goal", map[string]any{"mode": "size"})
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := st.GetActionInstance(parent.ID)
	ec := parseExecutionContext(updated, e)
	if getString(ec.Inputs, "mode") != "quality" || reviewedItemMode(ec, items[0]) != "size" {
		t.Fatal("original intent or new attempt changed")
	}
	child := &ExecutionContext{Inputs: map[string]any{"mode": "size", "parent_action_id": parent.ID, "batch_item_key": "file", "path": item.FilePath}, State: map[string]any{}}
	if err := e.bindModeParent(child); err != nil {
		t.Fatal(err)
	}
	policy, err := freezeModePolicy(child)
	if err != nil || policy.Mode != "size" || getString(child.State, "policy_digest") != policy.Digest {
		t.Fatal("child policy did not match reviewed goal")
	}
}
