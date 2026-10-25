package maintenanceui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
)

func TestFileStateOfReadsTheAttemptBeforeTheItemRow(t *testing.T) {
	running, completed, failed, cancelled := store.ActionStatusRunning, store.ActionStatusCompleted, store.ActionStatusFailed, store.ActionStatusCancelled
	for _, tc := range []struct {
		name string
		row  store.FileStatusRow
		want string
	}{
		{"queued in a live batch", store.FileStatusRow{ItemStatus: "queued", BatchStatus: running}, fileQueued},
		{"waiting for a slot", store.FileStatusRow{ItemStatus: "waiting_for_slot", BatchStatus: running}, fileQueued},
		{"converting", store.FileStatusRow{ItemStatus: "running", BatchStatus: running, ChildStatus: running}, fileWorking},
		{"in the worker's line is still waiting", store.FileStatusRow{ItemStatus: "running", BatchStatus: running, ChildStatus: store.ActionStatusWaitingExternal, ChildQueuedOnWorker: true}, fileQueued},
		{"a decision is waiting", store.FileStatusRow{ItemStatus: "waiting_decision", BatchStatus: store.ActionStatusWaitingDecision}, fileAttention},
		{"the attempt asks while the item row lags", store.FileStatusRow{ItemStatus: "running", BatchStatus: running, ChildStatus: store.ActionStatusWaitingDecision}, fileAttention},
		{"queued leftovers of an ended batch say nothing", store.FileStatusRow{ItemStatus: "queued", BatchStatus: cancelled}, ""},
		{"converted copy ready", store.FileStatusRow{ItemStatus: "completed", Decision: "transcode", CandidatePath: "/m/a.new.mkv", BatchStatus: completed, ChildStatus: completed}, fileConverted},
		{"replacement running", store.FileStatusRow{ItemStatus: "completed", Decision: "transcode", CandidatePath: "/m/a.new.mkv", BatchStatus: completed, ChildStatus: completed, ReplacementStatus: running}, fileWorking},
		{"replaced", store.FileStatusRow{ItemStatus: "completed", Decision: "transcode", BatchStatus: completed, ChildStatus: completed, ReplacementStatus: completed, Replaced: true}, fileReplaced},
		{"a failed replacement keeps the converted copy", store.FileStatusRow{ItemStatus: "completed", Decision: "transcode", CandidatePath: "/m/a.new.mkv", BatchStatus: completed, ChildStatus: completed, ReplacementStatus: failed}, fileConverted},
		{"skipped by the attempt", store.FileStatusRow{ItemStatus: "completed", BatchStatus: completed, ChildStatus: completed, ChildSkipped: true}, fileKept},
		{"skipped item", store.FileStatusRow{ItemStatus: "skip", BatchStatus: completed}, fileKept},
		{"a rejected candidate is a choice, not a failure", store.FileStatusRow{ItemStatus: "failed", Error: "transcode candidate rejected by user decision", BatchStatus: completed, ChildStatus: failed}, fileKept},
		{"failed conversion", store.FileStatusRow{ItemStatus: "failed", Error: "worker exited", BatchStatus: failed, ChildStatus: failed}, fileFailed},
		{"cancelled attempt", store.FileStatusRow{ItemStatus: "running", BatchStatus: cancelled, ChildStatus: cancelled}, ""},
	} {
		if got := fileStateOf(tc.row); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFileStatusIndexRollsFoldersUpToWhatNeedsAttention(t *testing.T) {
	live, done := store.ActionStatusRunning, store.ActionStatusCompleted
	index := buildFileStatusIndex([]store.FileStatusRow{
		{FilePath: "/media/show/Season 1/one.mkv", ItemStatus: "completed", Decision: "transcode", Replaced: true, BatchStatus: done, ChildStatus: done},
		// An older failed attempt and a newer queued one: the file is queued.
		{FilePath: "/media/show/Season 1/two.mkv", ItemStatus: "failed", BatchStatus: store.ActionStatusFailed, ChildStatus: store.ActionStatusFailed},
		{FilePath: "/media/show/Season 1/two.mkv", ItemStatus: "queued", BatchStatus: live},
		{FilePath: "/media/show/Season 2/three.mkv", ItemStatus: "failed", BatchStatus: store.ActionStatusFailed},
		{FilePath: "/media/show-extras/four.mkv", ItemStatus: "queued", BatchStatus: live},
	})
	if got := index.file("/media/show/Season 1/two.mkv"); got == nil || got.State != fileQueued || got.Count != 0 {
		t.Fatalf("file: %+v", got)
	}
	if index.file("/media/show/Season 1/untouched.mkv") != nil {
		t.Fatal("an untouched file has a mark")
	}
	season := index.under("/media/show/Season 1/")
	if season == nil || season.State != fileQueued || season.Count != 1 || season.Counts[fileReplaced] != 1 {
		t.Fatalf("season: %+v", season)
	}
	show := index.under("/media/show")
	if show == nil || show.State != fileQueued || show.Counts[fileFailed] != 1 || show.Counts[fileReplaced] != 1 {
		t.Fatalf("show: %+v", show)
	}
	// A sibling folder that merely shares a name prefix is not inside it.
	if extras := index.under("/media/show-extras"); extras == nil || extras.State != fileQueued || extras.Counts != nil {
		t.Fatalf("extras: %+v", extras)
	}
	if index.under("/media/other") != nil {
		t.Fatal("empty folder has a mark")
	}
	failedOnly := index.under("/media/show/Season 2")
	if failedOnly == nil || failedOnly.State != fileFailed {
		t.Fatalf("failed only: %+v", failedOnly)
	}
	if got := index.among([]string{"/media/show/Season 1/one.mkv", "/media/show/Season 1/zero.mkv"}); got == nil || got.State != fileReplaced {
		t.Fatalf("among: %+v", got)
	}
	var none *fileStatusIndex
	if none.file("/x") != nil || none.under("/x") != nil || none.among([]string{"/x"}) != nil {
		t.Fatal("a missing index must say nothing")
	}
}

func TestFolderListingMarksFilesAndFoldersByTheirConversions(t *testing.T) {
	s, h := testUI(t)
	root := t.TempDir()
	season := filepath.Join(root, "Season 1")
	if err := os.Mkdir(season, 0700); err != nil {
		t.Fatal(err)
	}
	queued, replaced := filepath.Join(season, "queued.mkv"), filepath.Join(season, "replaced.mkv")
	for _, path := range []string{queued, replaced, filepath.Join(root, "untouched.mkv")} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	deps := s.engine.Deps()
	deps.Fs, _ = fsop.NewResolver([]string{root}, nil)
	s.engine = action.NewEngine(deps)
	// The batch records the resolved path, which can differ from the temp dir's own spelling.
	real, err := deps.Fs.ResolveRead(root)
	if err != nil {
		t.Fatal(err)
	}
	st := deps.Store
	for _, inst := range []store.ActionInstance{
		{ID: "batch", ActionName: "transcode_batch", Status: store.ActionStatusRunning, InputsJSON: `{}`},
		{ID: "child", ActionName: "transcode_media", Status: store.ActionStatusCompleted},
		{ID: "promote", ActionName: "promote_transcode_candidate", Status: store.ActionStatusCompleted, IdempotencyKey: "promote:filesystem:child", OutputsJSON: `{"promoted":true}`},
	} {
		if err := st.CreateActionInstance(inst); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []store.TranscodeBatchItem{
		{BatchID: "batch", ItemKey: "q", FilePath: filepath.Join(real, "Season 1", "queued.mkv"), Status: "queued"},
		{BatchID: "batch", ItemKey: "r", FilePath: filepath.Join(real, "Season 1", "replaced.mkv"), Status: "completed", Decision: "transcode", ChildActionID: "child"},
	} {
		if err := st.CreateTranscodeBatchItem(item); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) map[string]map[string]any {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/folder?path="+url.QueryEscape(path), "", true)
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		byName := map[string]map[string]any{}
		for _, item := range page.Items {
			byName[filepath.Base(item["path"].(string))] = item
		}
		return byName
	}
	top := read(root)
	folder, _ := top["Season 1"]["nav_status"].(map[string]any)
	if folder["state"] != fileQueued || folder["count"] != float64(1) {
		t.Fatalf("folder mark: %+v", top["Season 1"])
	}
	if _, marked := top["untouched.mkv"]["nav_status"]; marked {
		t.Fatal("an untouched file has a mark")
	}
	inside := read(season)
	if mark, _ := inside["queued.mkv"]["nav_status"].(map[string]any); mark["state"] != fileQueued {
		t.Fatalf("queued file: %+v", inside["queued.mkv"])
	}
	if mark, _ := inside["replaced.mkv"]["nav_status"].(map[string]any); mark["state"] != fileReplaced {
		t.Fatalf("replaced file: %+v", inside["replaced.mkv"])
	}
}

func TestLibraryMarksEpisodeFilesTitlesAndSeasons(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	if err := st.CreateActionInstance(store.ActionInstance{ID: "batch", ActionName: "transcode_batch", Status: store.ActionStatusRunning, InputsJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "one", FilePath: "/media/show/Season 1/one.mkv", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/series" {
			json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "title": "Show", "path": "/media/show"}, {"id": 2, "title": "Other", "path": "/media/other"}})
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 10, "path": "/media/show/Season 1/one.mkv", "seasonNumber": 1, "size": 10},
			{"id": 11, "path": "/media/show/Season 1/two.mkv", "seasonNumber": 1, "size": 10},
			{"id": 20, "path": "/media/show/Season 2/three.mkv", "seasonNumber": 2, "size": 10},
		})
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"sonarr": {URL: upstream.URL}}})
	read := func(query string) map[string]any {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/library?service=sonarr&"+query, "", true)
		var page map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return page
	}
	marks := func(items []any) map[string]any {
		out := map[string]any{}
		for _, raw := range items {
			item := raw.(map[string]any)
			out[item["path"].(string)] = item["nav_status"]
		}
		return out
	}
	titles := marks(read("")["items"].([]any))
	if mark, _ := titles["/media/show"].(map[string]any); mark["state"] != fileQueued {
		t.Fatalf("title: %+v", titles)
	}
	if titles["/media/other"] != nil {
		t.Fatalf("untouched title marked: %+v", titles)
	}
	files := marks(read("id=1&season=1")["items"].([]any))
	if mark, _ := files["/media/show/Season 1/one.mkv"].(map[string]any); mark["state"] != fileQueued {
		t.Fatalf("file: %+v", files)
	}
	if files["/media/show/Season 1/two.mkv"] != nil {
		t.Fatalf("untouched file marked: %+v", files)
	}
	for _, raw := range read("id=1")["season_groups"].([]any) {
		group := raw.(map[string]any)
		mark, _ := group["nav_status"].(map[string]any)
		if group["seasonNumber"] == float64(1) && (mark == nil || mark["state"] != fileQueued) {
			t.Fatalf("season 1: %+v", group)
		}
		if group["seasonNumber"] == float64(2) && mark != nil {
			t.Fatalf("season 2 marked: %+v", group)
		}
	}
}

func TestQueuedOnWorkerCountsOnlyRunningItemsStillInTheWorkersLine(t *testing.T) {
	record := func(status, phase string) operationRecord {
		return operationRecord{inst: store.ActionInstance{Status: status}, state: map[string]any{"transcode_phase": phase}}
	}
	records := map[string]operationRecord{
		"line":     record(store.ActionStatusWaitingExternal, "queued"),
		"encoding": record(store.ActionStatusWaitingExternal, "encoding"),
		"done":     record(store.ActionStatusCompleted, "queued"),
	}
	items := []store.TranscodeBatchItem{
		{Status: "running", ChildActionID: "line"},
		{Status: "running", ChildActionID: "encoding"},
		{Status: "running", ChildActionID: "done"},
		{Status: "queued", ChildActionID: "line"},
		{Status: "running", ChildActionID: "none"},
	}
	if got := queuedOnWorker(items, records); got != 1 {
		t.Fatalf("queuedOnWorker = %d, want 1", got)
	}
}
