package maintenanceui

import (
	"encoding/json"
	"testing"
)

func TestOpeningAFileReadsItsRowAloneFromTheBatch(t *testing.T) {
	s, h := testUI(t)
	seedActionableFiles(t, s)
	read := func(query string) (items []batchItemView, total int) {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/batch-items?id=batch"+query, "", true)
		var page struct {
			Items []batchItemView
			Total int
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return page.Items, page.Total
	}
	if all, total := read(""); len(all) < 3 || total != len(all) {
		t.Fatal("the batch lists its files", all, total)
	}
	one, total := read("&child=wait")
	if len(one) != 1 || total != 1 || one[0].ItemKey != "wait" || one[0].ChildActionID != "wait" {
		t.Fatal("one file by its attempt", one, total)
	}
	if none, total := read("&child=not-a-file"); len(none) != 0 || total != 0 {
		t.Fatal("an attempt outside the batch finds nothing", none, total)
	}
}

func TestOpeningAJobCanReturnItsBatchAndReplacementInTheSameRead(t *testing.T) {
	s, h := testUI(t)
	seedActionableFiles(t, s)
	st := s.engine.Deps().Store
	seedOperation(t, st, "replacement", "promote_transcode_candidate", "waiting_decision", map[string]any{"transcode_action_id": "ready"}, nil, nil)
	prior, _ := st.GetActionInstance("replacement")
	prior.IdempotencyKey = "promote:filesystem:ready"
	st.UpdateActionInstance(*prior)

	ids := func(query string) []string {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/operations?id=ready"+query, "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		var got []string
		for _, job := range page.Jobs {
			got = append(got, job["id"].(string))
		}
		return got
	}
	if got := ids(""); len(got) != 1 || got[0] != "ready" {
		t.Fatal("without related only the job is returned", got)
	}
	got := ids("&related=1")
	if len(got) != 3 || got[0] != "ready" {
		t.Fatal("the job asked for comes first, with its batch and replacement", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen["batch"] || !seen["replacement"] {
		t.Fatal("batch and replacement are returned", got)
	}
}
