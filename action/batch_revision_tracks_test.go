package action

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// Creating a goal records the languages it froze, an empty list included.
func TestFreezingAGoalRecordsItsLanguages(t *testing.T) {
	st := setupTestStore(t)
	if _, err := st.SetKeepLanguages([]string{"fra"}); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineDeps{Store: st})
	for name, tc := range map[string]struct {
		inputs map[string]any
		want   []string
	}{
		"the global list": {map[string]any{"mode": "size"}, []string{"fra"}},
		"its own list":    {map[string]any{"mode": "size", "keep_languages": []any{"jpn"}}, []string{"jpn"}},
		"an empty list":   {map[string]any{"mode": "compact", "keep_languages": []any{}}, []string{}},
	} {
		ec := &ExecutionContext{Engine: e, InstanceID: "job", Inputs: tc.inputs, State: map[string]any{}}
		if _, err := freezeModePolicy(ec); err != nil {
			t.Fatal(name, err)
		}
		got, ok := ec.State[trackLanguagesKey].([]string)
		if !ok || !slices.Equal(got, tc.want) {
			t.Errorf("%s: recorded %v, want %v", name, ec.State[trackLanguagesKey], tc.want)
		}
	}
	quality := &ExecutionContext{Engine: e, InstanceID: "job", Inputs: map[string]any{"mode": "quality"}, State: map[string]any{}}
	if _, err := freezeModePolicy(quality); err != nil {
		t.Fatal(err)
	}
	if _, has := quality.State[trackLanguagesKey]; has {
		t.Error("a quality goal recorded languages")
	}
}

// A goal change of a batch keeps the batch's own choice of languages: its
// keep_languages input (an empty list included), else what it froze when it was
// created, else, for a batch from before the setting existed, the global list.
// Changing the budget or switching size to compact never reads the global list
// over a choice the batch made.
func TestBatchGoalChangeKeepsTheBatchsLanguages(t *testing.T) {
	legacy, err := modePolicyWithTracks("size", false, 0, nil, false, nil, &transcode.TrackPolicy{Languages: []string{"jpn", "kor"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		inputs map[string]any
		state  map[string]any
		change map[string]any
		want   []string // nil: no selection is frozen (every track is kept)
	}{
		{"keep every track survives a switch to compact", map[string]any{"mode": "size", "keep_languages": []any{}}, nil, map[string]any{"mode": "compact"}, nil},
		{"keep every track survives a budget change", map[string]any{"mode": "size", "keep_languages": []any{}}, nil, map[string]any{"mode": "size", "size_budget_percent": 40.0}, nil},
		{"an explicit list survives a switch to compact", map[string]any{"mode": "size", "keep_languages": []any{"fra", "ger"}}, nil, map[string]any{"mode": "compact"}, []string{"deu", "fra"}},
		{"an explicit list survives a budget change", map[string]any{"mode": "size", "keep_languages": []any{"kor"}}, nil, map[string]any{"mode": "size", "size_budget_percent": 40.0}, []string{"kor"}},
		{"no input keeps the list the batch froze", map[string]any{"mode": "size"}, map[string]any{trackLanguagesKey: []any{"eng", "spa"}}, map[string]any{"mode": "compact"}, []string{"eng", "spa"}},
		{"a frozen empty list stays empty", map[string]any{"mode": "size"}, map[string]any{trackLanguagesKey: []any{}}, map[string]any{"mode": "compact"}, nil},
		{"an older batch keeps the languages of its snapshot", map[string]any{"mode": "size"}, map[string]any{"mode_policy": legacy}, map[string]any{"mode": "compact"}, []string{"jpn", "kor"}},
		{"a batch with neither reads the global list", map[string]any{"mode": "size"}, nil, map[string]any{"mode": "compact"}, []string{"fra"}},
		{"a goal without tracks freezes none", map[string]any{"mode": "size", "keep_languages": []any{"kor"}}, nil, map[string]any{"mode": "quality"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := setupTestStore(t)
			if _, err := st.SetKeepLanguages([]string{"fra"}); err != nil {
				t.Fatal(err)
			}
			e := NewEngine(EngineDeps{Store: st})
			inputs := map[string]any{"paths": []string{"/media/a.mkv"}}
			for k, v := range tc.inputs {
				inputs[k] = v
			}
			state := map[string]any{"series_title": "Season 1"}
			for k, v := range tc.state {
				state[k] = v
			}
			seedActionInstance(t, st, "batch", "transcode_batch", StatusWaitingExternal, 1, "", inputs, state)
			if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "file-a", FilePath: "/media/a.mkv", Status: "failed", Decision: "transcode", Profile: "old"}); err != nil {
				t.Fatal(err)
			}
			root, _ := st.GetActionInstance("batch")
			items, _ := st.ListTranscodeBatchItems("batch")
			if _, err := e.ReconfigureBatch(context.Background(), "batch", "all", "", BatchSelectionVersion(root, items), "", "goal", tc.change); err != nil {
				t.Fatal(err)
			}
			root, _ = st.GetActionInstance("batch")
			var after map[string]any
			if err := json.Unmarshal([]byte(root.StateJSON), &after); err != nil {
				t.Fatal(err)
			}
			settings, _ := stateObject(after, "batch_item_settings")["file-a"].(map[string]any)
			policy := stateModePolicy(settings)
			if policy == nil {
				t.Fatalf("no policy was frozen: %s", root.StateJSON)
			}
			var got []string
			if policy.Tracks != nil {
				got = policy.Tracks.Languages
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("the goal froze languages %v, want %v", got, tc.want)
			}
		})
	}
}
