package maintenanceui

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/jakenesler/navigatorr/store"
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
	// The attempt is not the file's key: a row whose two differ is found by the attempt only.
	if err := s.engine.Deps().Store.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "key-x", FilePath: "/media/x.mkv", DisplayLabel: "x", ChildActionID: "attempt-x", Status: "pending", Decision: "transcode"}); err != nil {
		t.Fatal(err)
	}
	if byKey, _ := read("&child=key-x"); len(byKey) != 0 {
		t.Fatal("child names an attempt, not a file key", byKey)
	}
	if byAttempt, _ := read("&child=attempt-x"); len(byAttempt) != 1 || byAttempt[0].ItemKey != "key-x" {
		t.Fatal("the attempt finds its row", byAttempt)
	}
}

func TestOpeningAJobCanReturnItsBatchAndReplacementInTheSameRead(t *testing.T) {
	s, h := testUI(t)
	seedActionableFiles(t, s)
	st := s.engine.Deps().Store
	seedOperation(t, st, "replacement", "promote_transcode_candidate", "waiting_decision", map[string]any{"transcode_action_id": "ready"}, nil, nil)
	prior, err := st.GetActionInstance("replacement")
	if err != nil {
		t.Fatal(err)
	}
	prior.IdempotencyKey = "promote:filesystem:ready"
	if err := st.UpdateActionInstance(*prior); err != nil {
		t.Fatal(err)
	}

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
	if paged := ids("&related=1&limit=1"); len(paged) != 1 || paged[0] != "ready" {
		t.Fatal("paging keeps the job asked for, which is first", paged)
	}
	// Opened from the replacement's side, the attempt it replaces comes with it.
	if fromReplacement := idsOf(t, h, "replacement"); len(fromReplacement) < 2 || fromReplacement[0] != "replacement" || !slices.Contains(fromReplacement, "ready") {
		t.Fatal("a replacement returns the attempt it replaces", fromReplacement)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen["batch"] || !seen["replacement"] {
		t.Fatal("batch and replacement are returned", got)
	}
}

func TestAskingForRelatedJobsOfAMissingJobIsEmpty(t *testing.T) {
	s, h := testUI(t)
	seedActionableFiles(t, s)
	w := request(h, "GET", "/api/maintenance/operations?id=missing&related=1", "", true)
	var page operationsPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func idsOf(t *testing.T, h http.Handler, id string) []string {
	t.Helper()
	w := request(h, "GET", "/api/maintenance/operations?related=1&id="+id, "", true)
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
