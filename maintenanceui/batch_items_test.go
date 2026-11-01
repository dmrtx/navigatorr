package maintenanceui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestBatchItemsDurablePaginationAndOwnership(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "completed", nil, map[string]any{"items": []map[string]any{{"item_key": "truncated-only"}}}, nil)
	seedOperation(t, st, "file", "transcode_media", "completed", nil, nil, nil)
	for i := 0; i < 30; i++ {
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: fmt.Sprintf("file-%02d", i), FilePath: fmt.Sprintf("/media/%02d.mkv", i), Status: "failed", ChildActionID: fmt.Sprintf("child-%02d", i), Decision: "transcode", Reasons: []string{"review file"}, Error: "worker error", EpisodeInfo: "PRIVATE_METADATA"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query string
		count int
		more  bool
	}{{"", 25, true}, {"&offset=25", 5, false}, {"&offset=30", 0, false}} {
		w := request(h, "GET", "/api/maintenance/batch-items?id=batch"+tc.query, "", true)
		var page struct {
			Items   []batchItemView `json:"items"`
			Total   int             `json:"total"`
			HasMore bool            `json:"has_more"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != tc.count || page.Total != 30 || page.HasMore != tc.more || strings.Contains(w.Body.String(), "PRIVATE_METADATA") || strings.Contains(w.Body.String(), "truncated-only") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, id := range []string{"file", "missing"} {
		if w := request(h, "GET", "/api/maintenance/batch-items?id="+id, "", true); w.Code != 404 {
			t.Fatal("unrelated records exposed", w.Body.String())
		}
	}
	if w := request(h, "GET", "/api/maintenance/batch-items?id=batch&limit=101", "", true); w.Code != 400 {
		t.Fatal("unbounded page accepted")
	}
	if w := request(h, "GET", "/api/maintenance/batch-items?id=batch", "", false); w.Code != 401 {
		t.Fatal("unauthenticated batch exposed")
	}
}

// A finished encode that could not be saved is tried again as it is: the row
// carries the decisions its attempt offers, and is not offered a goal change.
func TestBatchItemsOfferTryAgainForAnEncodeThatWasNotSaved(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "waiting_decision", map[string]any{"mode": "size"}, nil, nil)
	for key, options := range map[string]string{"unsaved": `[{"decision":"recheck"},{"decision":"cancel"}]`, "unsure": `[{"decision":"cancel"}]`} {
		path := "/media/" + key + ".mkv"
		seedOperation(t, st, key, "transcode_media", "waiting_decision", map[string]any{"parent_action_id": "batch", "path": path}, nil, nil)
		child, _ := st.GetActionInstance(key)
		child.WaitingReason, child.WaitingOptionsJSON = "Encoding finished", options
		if err := st.UpdateActionInstance(*child); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: key, FilePath: path, DisplayLabel: key, ChildActionID: key, Status: "waiting_decision", Decision: "transcode"}); err != nil {
			t.Fatal(err)
		}
	}
	w := request(h, "GET", "/api/maintenance/batch-items?id=batch", "", true)
	var page struct{ Items []batchItemView }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	views := map[string]batchItemView{}
	for _, item := range page.Items {
		views[item.ItemKey] = item
	}
	if got := views["unsaved"]; !slices.Equal(got.WaitingDecisions, []string{"recheck", "cancel"}) || got.CanReconfigure {
		t.Fatalf("an unsaved encode is retried, not reconfigured: %+v", got)
	}
	if got := views["unsure"]; !slices.Equal(got.WaitingDecisions, []string{"cancel"}) || !got.CanReconfigure {
		t.Fatalf("other waiting files keep their goal change: %+v", got)
	}
}

// A batch says what its selection weighs from the moment its files are planned,
// before any of them is encoded: each file's size, and the sum with how many
// files have a size on record.
func TestBatchReportsTheSizeOfItsSelectionBeforeEncoding(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "sized", "transcode_batch", "running", nil, nil, nil)
	for i, size := range []int64{4_000_000_000, 2_500_000_000, 0} {
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "sized", ItemKey: fmt.Sprintf("file-%d", i), FilePath: fmt.Sprintf("/media/%d.mkv", i), Status: "queued", Decision: "transcode", SourceBytes: size}); err != nil {
			t.Fatal(err)
		}
	}
	w := request(h, "GET", "/api/maintenance/operations?id=sized", "", true)
	var page struct {
		Jobs []struct {
			ID        string `json:"id"`
			Selection *struct {
				SourceBytes int64 `json:"source_bytes"`
				Files       int   `json:"files"`
				SizedFiles  int   `json:"sized_files"`
			} `json:"selection"`
		} `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 || page.Jobs[0].Selection == nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if sel := page.Jobs[0].Selection; sel.SourceBytes != 6_500_000_000 || sel.Files != 3 || sel.SizedFiles != 2 {
		t.Fatalf("%+v", sel)
	}
	w = request(h, "GET", "/api/maintenance/batch-items?id=sized", "", true)
	var items struct {
		Items []batchItemView `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &items) != nil || len(items.Items) != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
	got := []int64{}
	for _, item := range items.Items {
		got = append(got, item.SourceBytes)
	}
	if !slices.Equal(got, []int64{4_000_000_000, 2_500_000_000, 0}) {
		t.Fatal(got)
	}
}
