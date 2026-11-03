package maintenanceui

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

type sizedRow struct {
	ID                 string `json:"id"`
	OriginalBytes      *int64 `json:"original_bytes"`
	ResultBytes        *int64 `json:"result_bytes"`
	OriginalBytesTotal *int64 `json:"original_bytes_total"`
	ResultBytesTotal   *int64 `json:"result_bytes_total"`
	FilesWithResult    *int   `json:"files_with_result"`
	FilesWithOriginal  *int   `json:"files_with_original"`
}

func sizedRows(t *testing.T, h http.Handler, path string) map[string]sizedRow {
	t.Helper()
	w := request(h, "GET", path, "", true)
	var page struct{ Jobs []sizedRow }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	rows := map[string]sizedRow{}
	for _, j := range page.Jobs {
		rows[j.ID] = j
	}
	return rows
}

func bytesOf(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// Every file job row carries its original's size from the moment it is known,
// whatever the job's state, and the new file's size once there is one.
func TestJobRowsCarryTheOriginalSizeFromTheStart(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	original := func(n int64) map[string]any { return map[string]any{"size_bytes": n, "duration_sec": 60} }
	// Queued: only the stat taken at submission.
	seedOperation(t, st, "queued", "transcode_media", "pending", map[string]any{"path": "/media/queued.mkv"}, nil, map[string]any{"request_origin": "web", "submitted_source_bytes": 9000})
	// Encoding: the preflight's record of the original wins over the stat.
	seedOperation(t, st, "running", "transcode_media", "running", map[string]any{"path": "/media/running.mkv"}, nil, map[string]any{"submitted_source_bytes": 1, "original": original(8000), "transcode_phase": "encoding"})
	// Failed before its preflight recorded the original.
	seedOperation(t, st, "failed", "transcode_media", "failed", map[string]any{"path": "/media/failed.mkv"}, map[string]any{"submitted_source_bytes": 7000}, map[string]any{"submitted_source_bytes": 7000})
	// Failed after a result was measured: a failed job has no new file.
	seedOperation(t, st, "failed-late", "transcode_media", "failed", map[string]any{"path": "/media/late.mkv"}, map[string]any{"original": original(6500), "result": map[string]any{"size_bytes": 3000}}, nil)
	// Ready to replace.
	seedOperation(t, st, "ready", "transcode_media", "completed", map[string]any{"path": "/media/ready.mkv"}, map[string]any{"original": original(6000), "result": map[string]any{"size_bytes": 2000}, "original_intact": true, "original_sha256": "ready"}, nil)
	// Replaced: its candidate became the file, and the row keeps both sizes.
	seedOperation(t, st, "replaced", "transcode_media", "completed", map[string]any{"path": "/media/replaced.mkv"}, map[string]any{"original": original(5000), "result": map[string]any{"size_bytes": 1500}, "original_intact": true, "original_sha256": "replaced", "candidate_removed": true}, nil)
	seedOperation(t, st, "promoted", "promote_transcode_candidate", "completed", map[string]any{"transcode_action_id": "replaced"}, map[string]any{"promoted": true, "recovery_retained": false, "promotion": map[string]any{"original_path": "/media/replaced.mkv", "original_sha256": "replaced", "original_bytes": 5000, "candidate_bytes": 1500, "transcode_action_id": "replaced", "recovery_cleanup_completed": true, "recovery_verified_before_replacement": true}}, nil)
	// Discarded: the candidate was deleted without replacing anything.
	seedOperation(t, st, "discarded", "transcode_media", "completed", map[string]any{"path": "/media/discarded.mkv"}, map[string]any{"original": original(4000), "result": map[string]any{"size_bytes": 1000}, "original_intact": true, "original_sha256": "discarded", "candidate_removed": true}, nil)
	// A replacement still planning has no checkpoint: its transcode's sizes.
	seedOperation(t, st, "planning", "promote_transcode_candidate", "running", map[string]any{"transcode_action_id": "ready"}, nil, nil)

	rows := sizedRows(t, h, "/api/maintenance/operations?limit=100")
	for id, want := range map[string][2]int64{
		"queued":      {9000, -1},
		"running":     {8000, -1},
		"failed":      {7000, -1},
		"failed-late": {6500, -1},
		"ready":       {6000, 2000},
		"replaced":    {5000, 1500},
		"promoted":    {5000, 1500},
		"discarded":   {4000, -1},
		"planning":    {6000, 2000},
	} {
		row, ok := rows[id]
		if !ok {
			t.Fatalf("%s not listed: %v", id, rows)
		}
		if got := [2]int64{bytesOf(row.OriginalBytes), bytesOf(row.ResultBytes)}; got != want {
			t.Errorf("%s: original/result %v, want %v", id, got, want)
		}
	}
}

// A batch row sums its files' original sizes and, separately, the results of
// the files that have one; each batch file row says the same of its file.
func TestBatchRowsCarrySizeTotals(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "batch", "transcode_batch", "running", map[string]any{"paths": []string{"/media/a.mkv"}}, nil, nil)
	seedOperation(t, st, "a", "transcode_media", "completed", map[string]any{"parent_action_id": "batch", "path": "/media/a.mkv"}, map[string]any{"original": map[string]any{"size_bytes": 4000}, "result": map[string]any{"size_bytes": 1000}, "original_intact": true, "original_sha256": "a"}, nil)
	seedOperation(t, st, "b", "transcode_media", "running", map[string]any{"parent_action_id": "batch", "path": "/media/b.mkv"}, nil, map[string]any{"original": map[string]any{"size_bytes": 3000}, "transcode_phase": "encoding"})
	// A child of another batch never counts here.
	seedOperation(t, st, "stranger", "transcode_media", "completed", map[string]any{"parent_action_id": "other", "path": "/media/c.mkv"}, map[string]any{"original": map[string]any{"size_bytes": 99}, "result": map[string]any{"size_bytes": 9}}, nil)
	for _, item := range []store.TranscodeBatchItem{
		{ItemKey: "a", FilePath: "/media/a.mkv", ChildActionID: "a", Status: "completed", Decision: "transcode", SourceBytes: 3999},
		{ItemKey: "b", FilePath: "/media/b.mkv", ChildActionID: "b", Status: "running", Decision: "transcode"},
		{ItemKey: "c", FilePath: "/media/c.mkv", ChildActionID: "stranger", Status: "queued", Decision: "transcode", SourceBytes: 2000},
		{ItemKey: "d", FilePath: "/media/d.mkv", Status: "queued", Decision: "transcode"},
	} {
		item.BatchID = "batch"
		if err := st.CreateTranscodeBatchItem(item); err != nil {
			t.Fatal(err)
		}
	}
	rows := sizedRows(t, h, "/api/maintenance/operations?limit=100")
	batch := rows["batch"]
	if bytesOf(batch.OriginalBytesTotal) != 9000 || bytesOf(batch.ResultBytesTotal) != 1000 || batch.FilesWithResult == nil || *batch.FilesWithResult != 1 || batch.FilesWithOriginal == nil || *batch.FilesWithOriginal != 3 {
		t.Fatalf("batch totals %+v", batch)
	}
	if a := rows["a"]; bytesOf(a.OriginalBytes) != 4000 || bytesOf(a.ResultBytes) != 1000 {
		t.Fatalf("batch file row %+v", a)
	}

	w := request(h, "GET", "/api/maintenance/batch-items?id=batch", "", true)
	var page struct{ Items []batchItemView }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	got := map[string][3]int64{}
	for _, item := range page.Items {
		got[item.ItemKey] = [3]int64{item.SourceBytes, item.OriginalBytes, item.ResultBytes}
	}
	want := map[string][3]int64{"a": {3999, 4000, 1000}, "b": {0, 3000, 0}, "c": {2000, 2000, 0}, "d": {0, 0, 0}}
	for key, w := range want {
		if got[key] != w {
			t.Errorf("%s: source/original/result %v, want %v", key, got[key], w)
		}
	}
	// No results yet: the total is absent, not zero, and counts no file.
	seedOperation(t, st, "fresh", "transcode_batch", "running", nil, nil, nil)
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "fresh", ItemKey: "x", FilePath: "/media/x.mkv", Status: "queued", Decision: "transcode", SourceBytes: 500}); err != nil {
		t.Fatal(err)
	}
	fresh := sizedRows(t, h, "/api/maintenance/operations?id=fresh")["fresh"]
	if bytesOf(fresh.OriginalBytesTotal) != 500 || fresh.ResultBytesTotal != nil || fresh.FilesWithResult == nil || *fresh.FilesWithResult != 0 {
		t.Fatalf("fresh batch %+v", fresh)
	}
}
