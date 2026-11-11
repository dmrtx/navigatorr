package maintenanceui

import (
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestArchiveHidesWorkflowAndRestoresHistoryAndSavings(t *testing.T) {
	s, h := testUI(t)
	seedOperation(t, s.engine.Deps().Store, "candidate", "transcode_media", "completed", map[string]any{"path": "/media/one.mkv"}, map[string]any{"original_intact": true}, map[string]any{"original": map[string]any{"size_bytes": 1000}, "result": map[string]any{"size_bytes": 300}, "original_intact": true})
	seedOperation(t, s.engine.Deps().Store, "preview", "transcode_batch", "completed", nil, map[string]any{"dry_run": true}, nil)
	read := func(path string) operationsPage {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/operations"+path, "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return page
	}
	before := read("?group=workflow&status=all")
	if before.Total != 2 || before.Savings.CandidateBytes != 700 {
		t.Fatal(before)
	}
	for _, id := range []string{"candidate", "preview"} {
		w := request(h, "POST", "/api/maintenance/archive", `{"id":"`+id+`","archived":true}`, true)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	main := read("?group=workflow&status=all")
	// Archiving discards a job: its copy no longer waits for a decision, but it
	// is still reported (and can be deleted) while it uses disk.
	if main.Total != 0 || main.Savings.CandidateBytes != 0 || main.Savings.CandidateCount != 0 || main.Savings.ArchivedCandidateCount != 1 || main.Savings.ArchivedCandidateBytes != 300 || len(main.Ready) != 0 || len(main.ArchivedReady) != 1 {
		t.Fatal("archived copy still waits or is not reported", main)
	}
	archived := read("?group=workflow&status=archived&limit=1")
	if archived.Total != 2 || !archived.HasMore || archived.Jobs[0]["archived"] != true {
		t.Fatal(archived)
	}
	detail := read("?id=candidate")
	if len(detail.Jobs) != 1 || detail.Jobs[0]["archived"] != true {
		t.Fatal("detail link lost history")
	}
	if w := request(h, "POST", "/api/maintenance/archive", `{"id":"candidate","archived":false}`, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	main = read("?group=workflow&status=all")
	if main.Total != 1 || main.Jobs[0]["id"] != "candidate" || main.Jobs[0]["archived"] != false {
		t.Fatal(main)
	}
	if main.Savings.CandidateBytes != 700 || main.Savings.ArchivedCandidateCount != 0 {
		t.Fatal("a restored job waits again", main.Savings)
	}
	// Another client can retry an archived action; active work must stay visible.
	inst, _ := s.engine.Deps().Store.GetActionInstance("preview")
	inst.Status = store.ActionStatusRunning
	s.engine.Deps().Store.UpdateActionInstance(*inst)
	main = read("?group=workflow&status=all")
	if main.Total != 2 {
		t.Fatal("archived active work hidden")
	}
}

func TestArchiveRejectsActiveMembersAndRequiresAuthenticatedExplicitInput(t *testing.T) {
	s, h := testUI(t)
	seedOperation(t, s.engine.Deps().Store, "batch", "transcode_batch", "completed", nil, nil, nil)
	seedOperation(t, s.engine.Deps().Store, "child", "transcode_media", "waiting_external", map[string]any{"parent_action_id": "batch"}, nil, nil)
	seedOperation(t, s.engine.Deps().Store, "source", "transcode_media", "completed", nil, nil, nil)
	seedOperation(t, s.engine.Deps().Store, "promotion", "promote_transcode_candidate", "waiting_decision", map[string]any{"transcode_action_id": "source"}, nil, nil)
	for _, id := range []string{"batch", "child", "source", "promotion"} {
		if w := request(h, "POST", "/api/maintenance/archive", `{"id":"`+id+`","archived":true}`, true); w.Code != 409 {
			t.Fatal(id, w.Code, w.Body.String())
		}
	}
	if w := request(h, "POST", "/api/maintenance/archive", `{"id":"batch","archived":true}`, false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(h, "POST", "/api/maintenance/archive", `{"id":"missing","archived":true}`, true); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := request(h, "POST", "/api/maintenance/archive", `{"id":"batch"}`, true); w.Code != 400 {
		t.Fatal(w.Code)
	}
	items, _ := s.engine.Deps().Store.MaintenanceArchives()
	if len(items) != 0 {
		t.Fatal("rejected archive persisted")
	}
	child, _ := s.engine.Deps().Store.GetActionInstance("child")
	child.Status = store.ActionStatusCancelled
	s.engine.Deps().Store.UpdateActionInstance(*child)
	if w := request(h, "POST", "/api/maintenance/archive", `{"id":"child","archived":true}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	items, _ = s.engine.Deps().Store.MaintenanceArchives()
	if items["batch"] == "" || items["child"] != "" {
		t.Fatal("archive not attached to stable workflow", items)
	}
}

func TestJobLeftoversReportsAndRequiresAuthenticatedKnownJob(t *testing.T) {
	s, h := testUI(t)
	seedOperation(t, s.engine.Deps().Store, "batch", "transcode_batch", "cancelled", nil, nil, nil)
	seedOperation(t, s.engine.Deps().Store, "child", "transcode_media", "cancelled", map[string]any{"parent_action_id": "batch"}, nil, nil)
	if w := request(h, "GET", "/api/maintenance/job-leftovers?id=batch", "", false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(h, "GET", "/api/maintenance/job-leftovers?id=missing", "", true); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := request(h, "GET", "/api/maintenance/job-leftovers", "", true); w.Code != 400 {
		t.Fatal(w.Code)
	}
	w := request(h, "GET", "/api/maintenance/job-leftovers?id=batch", "", true)
	var found struct {
		Files     int   `json:"files"`
		Bytes     int64 `json:"bytes"`
		CanDelete bool  `json:"can_delete"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &found) != nil || found.Files != 0 || found.Bytes != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Removing files needs the server to allow it.
	if w := request(h, "POST", "/api/maintenance/job-leftovers", `{"id":"batch"}`, true); found.CanDelete == (w.Code == 403) {
		t.Fatal("deletion must follow allow_destructive", w.Code, found.CanDelete)
	}
}

// Archiving used to decode every action's JSON again on each tap. The index it
// reads from must come from the decoded records the server already keeps.
func TestArchiveReadsTheKeptRecordsInsteadOfDecodingAgain(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "candidate", "transcode_media", "completed", map[string]any{"path": "/media/one.mkv"}, map[string]any{"original_intact": true}, nil)
	if w := request(h, "POST", "/api/maintenance/archive", `{"id":"candidate","archived":true}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	s.recordMemo.mu.Lock()
	kept := len(s.recordMemo.byID)
	s.recordMemo.mu.Unlock()
	if kept == 0 {
		t.Fatal("archiving must decode through the shared record memo")
	}
	s.recordMemo.mu.Lock()
	s.recordMemo.byID = nil
	s.recordMemo.mu.Unlock()
	if err := s.WarmRecords(); err != nil {
		t.Fatal(err)
	}
	s.recordMemo.mu.Lock()
	warmed := len(s.recordMemo.byID)
	s.recordMemo.mu.Unlock()
	if warmed != kept {
		t.Fatal("warming at start must fill the memo with every action", warmed, kept)
	}
}

func TestFinishedMCPPreviewArchivesItselfButWebPreviewStays(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	dry := map[string]any{"dry_run": true}
	seedOperation(t, st, "mcp-preview", "transcode_batch", "completed", dry, nil, map[string]any{"request_origin": "mcp"})
	seedOperation(t, st, "web-preview", "transcode_batch", "completed", dry, nil, map[string]any{"request_origin": "web"})
	seedOperation(t, st, "mcp-started", "transcode_batch", "completed", dry, nil, map[string]any{"request_origin": "mcp"})
	seedOperation(t, st, "run", "transcode_batch", "waiting_external", map[string]any{"dry_run": false}, nil, nil)
	run, _ := st.GetActionInstance("run")
	run.IdempotencyKey = previewExecutionPrefix + "mcp-started"
	if err := st.UpdateActionInstance(*run); err != nil {
		t.Fatal(err)
	}
	read := func(path string) operationsPage {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/operations"+path, "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return page
	}
	ids := func(page operationsPage) map[string]bool {
		out := map[string]bool{}
		for _, j := range page.Jobs {
			out[j["id"].(string)] = true
		}
		return out
	}
	// A preview still running when the caches were filled is archived once it completes:
	// the origin must survive the refresh of its row.
	seedOperation(t, st, "late-preview", "transcode_batch", "running", dry, nil, map[string]any{"request_origin": "mcp"})
	if !ids(read("?group=workflow&status=all"))["late-preview"] {
		t.Fatal("an active preview must stay visible")
	}
	late, _ := st.GetActionInstance("late-preview")
	late.Status = store.ActionStatusCompleted
	if err := st.UpdateActionInstance(*late); err != nil {
		t.Fatal(err)
	}
	if ids(read("?group=workflow&status=all"))["late-preview"] || !ids(read("?group=workflow&status=archived"))["late-preview"] {
		t.Fatal("a refreshed row lost its origin")
	}
	main := ids(read("?group=workflow&status=all"))
	if main["mcp-preview"] || !main["web-preview"] {
		t.Fatal("only the finished MCP preview leaves the queue", main)
	}
	if !ids(read("?group=workflow&status=archived"))["mcp-preview"] {
		t.Fatal("the MCP preview is not under Archived")
	}
	if main["mcp-started"] || !main["run"] {
		t.Fatal("a started preview shows its run, not itself", main)
	}
	detail := read("?id=mcp-preview")
	if len(detail.Jobs) != 1 {
		t.Fatal("a direct link to the preview lost its history")
	}
	// Nothing to unarchive: the clients are told so, not offered a button that does nothing.
	if job := detail.Jobs[0]; job["archived"] != true || job["archived_automatically"] != true || job["can_archive"] != false {
		t.Fatal("an automatic archive is not reported as such", job)
	}
	// Restoring it would report success and be undone by the next read, so it is refused.
	if w := request(h, "POST", "/api/maintenance/archive", `{"id":"mcp-preview","archived":false}`, true); w.Code != 409 {
		t.Fatal("an automatic archive was reported as restored", w.Code, w.Body.String())
	}
	// A web preview is still archived and restored by hand.
	for _, want := range []bool{true, false} {
		body := `{"id":"web-preview","archived":` + map[bool]string{true: "true", false: "false"}[want] + `}`
		if w := request(h, "POST", "/api/maintenance/archive", body, true); w.Code != 200 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	// The row of a manual archive does not override the rule once it is removed.
	if w := request(h, "POST", "/api/maintenance/archive", `{"id":"mcp-preview","archived":true}`, true); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request(h, "POST", "/api/maintenance/archive", `{"id":"mcp-preview","archived":false}`, true); w.Code != 409 {
		t.Fatal("a manual row on an automatic archive was reported as restored", w.Code)
	}
	// With the manual row present the job still reads as automatic, so no client offers Restore.
	if job := read("?id=mcp-preview").Jobs[0]; job["archived_automatically"] != true || job["can_archive"] != false || job["archived"] != true {
		t.Fatal("a manual row hid the automatic archive", job)
	}
	if job := read("?id=web-preview").Jobs[0]; job["archived"] != false || job["archived_automatically"] != false || job["can_archive"] != true {
		t.Fatal("a web preview must stay a normal job", job)
	}
}
