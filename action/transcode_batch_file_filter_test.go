package action

import (
	"context"
	"testing"
)

func TestTranscodeBatchSelectedFilesAreConfinedAndDeduplicated(t *testing.T) {
	engine, st, _, srv, _ := setupBatchTestEnv(t, &mockTranscodeExecutor{}, 1)
	defer srv.Close()
	defer st.Close()
	for _, tc := range []struct {
		name    string
		ids     []int
		season  int
		want    []string
		failure bool
	}{
		{"multi-episode", []int{101, 101}, 0, []string{"epfile-101"}, false},
		{"season-intersection", []int{101, 103}, 2, []string{"epfile-103"}, false},
		{"foreign-file", []int{999}, 0, nil, true},
		{"empty-selection", []int{}, 0, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := map[string]any{"service": "sonarr", "series_id": 10, "dry_run": true, "episode_file_ids": tc.ids}
			if tc.season > 0 {
				inputs["season"] = tc.season
			}
			r, err := engine.Run(context.Background(), "transcode_batch", inputs)
			if err != nil {
				t.Fatal(err)
			}
			if tc.failure {
				if r.Status != StatusFailed {
					t.Fatalf("invalid selection accepted: %+v", r)
				}
				return
			}
			if r.Status != StatusCompleted {
				t.Fatalf("batch failed: %+v", r)
			}
			items, err := st.ListTranscodeBatchItems(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != len(tc.want) {
				t.Fatalf("unexpected selection: %+v", items)
			}
			for i, it := range items {
				if it.ItemKey != tc.want[i] {
					t.Fatalf("unexpected item: %+v", it)
				}
			}
		})
	}
}
