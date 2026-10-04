package maintenanceui

import (
	"encoding/json"
	"fmt"
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
