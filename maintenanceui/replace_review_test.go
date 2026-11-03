package maintenanceui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
)

func countPromotions(rows []store.ActionInstance) int {
	n := 0
	for _, row := range rows {
		if row.ActionName == "promote_transcode_candidate" {
			n++
		}
	}
	return n
}

// readyFile is what a finished transcode keeps when its new file is ready.
func readyFile(original string) map[string]any {
	return map[string]any{"original": map[string]any{"size_bytes": 1000}, "result": map[string]any{"size_bytes": 300}, "original_intact": true, "original_sha256": "before", "candidate_path": original + ".candidate.mkv"}
}

func promotionOf(t *testing.T, st *store.Store, id string) map[string]any {
	t.Helper()
	for _, service := range []string{"sonarr", "radarr", "filesystem"} {
		inst, err := st.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:"+service+":"+id)
		if err != nil {
			t.Fatal(err)
		}
		if inst != nil {
			return decodeOperationJSON(inst.InputsJSON)
		}
	}
	return nil
}

// Replacing from a running batch replaces exactly the files listed, now; a
// file that finishes later stays ready to replace until it is reviewed too.
func TestReplaceFromARunningBatchReplacesOnlyTheListedReadyFiles(t *testing.T) {
	s, h := testUI(t)
	s.cfg.AllowDestructive = true
	st := s.engine.Deps().Store
	seedOperation(t, st, "running-batch", "transcode_batch", store.ActionStatusRunning, map[string]any{"paths": []string{"/media/a.mkv", "/media/b.mkv", "/media/c.mkv"}}, nil, nil)
	seedOperation(t, st, "file-a", "transcode_media", store.ActionStatusCompleted, map[string]any{"path": "/media/a.mkv", "parent_action_id": "running-batch"}, readyFile("/media/a.mkv"), nil)
	seedOperation(t, st, "file-b", "transcode_media", store.ActionStatusCompleted, map[string]any{"path": "/media/b.mkv", "parent_action_id": "running-batch"}, readyFile("/media/b.mkv"), nil)
	seedOperation(t, st, "file-c", "transcode_media", store.ActionStatusRunning, map[string]any{"path": "/media/c.mkv", "parent_action_id": "running-batch"}, nil, nil)
	readJob := func(id string) map[string]any {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		return page.Jobs[0]
	}

	w := request(h, "POST", "/api/maintenance/replace", `{"ids":["file-a"],"key":"review-a"}`, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var started struct {
		Replacements []struct {
			ID                  string `json:"id"`
			ReplacementActionID string `json:"replacement_action_id"`
		} `json:"replacements"`
	}
	if json.Unmarshal(w.Body.Bytes(), &started) != nil || len(started.Replacements) != 1 || started.Replacements[0].ID != "file-a" || started.Replacements[0].ReplacementActionID == "" {
		t.Fatal(w.Body.String())
	}
	if p := promotionOf(t, st, "file-a"); p == nil || p["approval"] != nil || p["service"] != "filesystem" || p["batch_promote_parent_id"] != nil {
		t.Fatal("reviewed file not replaced", p)
	}
	if a, _ := st.GetReplacementApproval("file-a"); a == nil || a.Approval != action.ApprovalReviewed {
		t.Fatal("reviewed file not approved as reviewed", a)
	}
	for _, id := range []string{"file-b", "file-c"} {
		if p := promotionOf(t, st, id); p != nil {
			t.Fatal("a file nobody listed is being replaced", id, p)
		}
	}
	// The same review sent again gets the same answer and starts nothing new;
	// the key cannot be used for other files.
	first := w.Body.String()
	if w := request(h, "POST", "/api/maintenance/replace", `{"ids":["file-a"],"key":"review-a"}`, true); w.Code != 202 || w.Body.String() != first {
		t.Fatal("a replay answered differently", w.Code, w.Body.String(), first)
	}
	if w := request(h, "POST", "/api/maintenance/replace", `{"ids":["file-b"],"key":"review-a"}`, true); w.Code != 409 || !strings.Contains(w.Body.String(), "key_reused") {
		t.Fatal("a key was reused for other files", w.Code, w.Body.String())
	}
	if rows, _ := st.ListActionInstances("", 100); countPromotions(rows) != 1 {
		t.Fatal("a replay started another replacement")
	}

	// All or nothing: one file still encoding refuses the whole request.
	w = request(h, "POST", "/api/maintenance/replace", `{"ids":["file-b","file-c"],"key":"review-bc"}`, true)
	var refused struct {
		Code     string   `json:"code"`
		Ready    []string `json:"ready"`
		NotReady []struct {
			ID, Reason string
		} `json:"not_ready"`
	}
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &refused) != nil || refused.Code != "not_ready" || len(refused.Ready) != 1 || refused.Ready[0] != "file-b" || len(refused.NotReady) != 1 || refused.NotReady[0].ID != "file-c" || refused.NotReady[0].Reason != "not_finished" {
		t.Fatal(w.Code, w.Body.String())
	}
	if p := promotionOf(t, st, "file-b"); p != nil {
		t.Fatal("a refused request replaced a file", p)
	}

	// file-c finishes after the review: nothing replaces it by itself.
	c, _ := st.GetActionInstance("file-c")
	c.Status = store.ActionStatusCompleted
	c.OutputsJSON = mustJSON(t, readyFile("/media/c.mkv"))
	if err := st.UpdateActionInstance(*c); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"file-b", "file-c"} {
		if job := readJob(id); job["conversion_status"] != "ready_to_replace" || job["replace_when_done"] != false {
			t.Fatal("a file that was not reviewed is not waiting for its review", id, job["conversion_status"], job["replace_when_done"])
		}
		if p := promotionOf(t, st, id); p != nil {
			t.Fatal("replaced without a review", id, p)
		}
	}

	if w := request(h, "POST", "/api/maintenance/replace", `{"ids":["file-b","file-b"],"key":"review-dup"}`, true); w.Code != 400 {
		t.Fatal("duplicate ids accepted", w.Code)
	}
	if w := request(h, "POST", "/api/maintenance/replace", `{"ids":["file-b"],"key":"review-b"}`, false); w.Code != 401 {
		t.Fatal("unauthenticated replacement", w.Code)
	}
	s.cfg.AllowDestructive = false
	if w := request(h, "POST", "/api/maintenance/replace", `{"ids":["file-b"],"key":"review-b"}`, true); w.Code != 403 {
		t.Fatal("replacement with destructive actions disabled", w.Code)
	}
}

// A file whose replacement was already planned and waits for approval is
// approved with the plan it waits on.
func TestReplaceApprovesAPlanAlreadyWaiting(t *testing.T) {
	s, h := testUI(t)
	s.cfg.AllowDestructive = true
	st := s.engine.Deps().Store
	seedOperation(t, st, "single", "transcode_media", store.ActionStatusCompleted, map[string]any{"path": "/media/single.mkv"}, readyFile("/media/single.mkv"), nil)
	promotion := map[string]any{"transcode_action_id": "single", "service": "filesystem", "original_path": "/media/single.mkv", "candidate_path": "/media/single.mkv.candidate.mkv", "original_sha256": "before", "candidate_sha256": "after", "original_bytes": 1000, "candidate_bytes": 300}
	if err := st.CreateActionInstance(store.ActionInstance{ID: "waiting-plan", ActionName: "promote_transcode_candidate", Status: store.ActionStatusWaitingDecision, CurrentStep: 1, IdempotencyKey: "promote:filesystem:single", InputsJSON: mustJSON(t, map[string]any{"transcode_action_id": "single", "service": "filesystem"}), StateJSON: mustJSON(t, map[string]any{"promotion": promotion}), OutputsJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	// Without the digest of the plan shown, the plan is sent back.
	w := request(h, "POST", "/api/maintenance/replace", `{"ids":["single"],"key":"review-unseen"}`, true)
	digest := action.PromotionPlanDigest(promotion)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"reason":"plan_changed"`) || !strings.Contains(w.Body.String(), digest) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(h, "POST", "/api/maintenance/replace", `{"ids":["single"],"digests":{"other":"x"},"key":"review-stray"}`, true); w.Code != 400 {
		t.Fatal("a digest for an unlisted file", w.Code)
	}
	w = request(h, "POST", "/api/maintenance/replace", mustJSON(t, map[string]any{"ids": []string{"single"}, "digests": map[string]string{"single": digest}, "key": "review-single"}), true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var started struct {
		Replacements []map[string]string `json:"replacements"`
	}
	json.Unmarshal(w.Body.Bytes(), &started)
	if len(started.Replacements) != 1 || started.Replacements[0]["replacement_action_id"] != "waiting-plan" || started.Replacements[0]["approved"] != "waiting_plan" {
		t.Fatal(w.Body.String())
	}
	command, _ := st.FindActionByIdempotencyKey("maintenance_command", "web-command:replace-review-single-waiting-plan")
	if command == nil {
		t.Fatal("no approval queued")
	}
	inputs := decodeOperationJSON(command.InputsJSON)
	if inputs["kind"] != "resume" || inputs["decision"] != "approve" || inputs["id"] != "waiting-plan" || inputs["plan_digest"] != digest {
		t.Fatal(inputs)
	}
}

// An approval of a batch's plan must quote the plan shown; a plan that
// changed is refused with the current one.
func TestBatchApprovalOfAStalePlanIsRefusedWithTheCurrentPlan(t *testing.T) {
	s, h := testUI(t)
	s.cfg.AllowDestructive = true
	st := s.engine.Deps().Store
	plan := map[string]any{"batch_id": "planned", "service": "filesystem", "series_id": 0, "digest": "sha256:current", "members": []map[string]any{{"item_key": "a", "transcode_action_id": "file-a", "original_path": "/media/a.mkv", "candidate_path": "/media/a.mkv.candidate.mkv", "candidate_sha256": "after"}}}
	if err := st.CreateActionInstance(store.ActionInstance{ID: "planned", ActionName: "transcode_batch", Status: store.ActionStatusWaitingDecision, CurrentStep: 2, InputsJSON: mustJSON(t, map[string]any{"paths": []string{"/media/a.mkv"}}), StateJSON: mustJSON(t, map[string]any{"batch_promotion_plan": plan}), OutputsJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"old digest":    `{"id":"planned","digest":"sha256:earlier","decision":"approve","key":"approve-old"}`,
		"other members": `{"id":"planned","digest":"sha256:current","item_keys":["a","b"],"decision":"approve","key":"approve-more"}`,
	} {
		w := request(h, "POST", "/api/maintenance/batch-candidates", body, true)
		var refused struct {
			Code string `json:"code"`
			Plan struct {
				Digest  string           `json:"digest"`
				Members []map[string]any `json:"members"`
			} `json:"plan"`
		}
		if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &refused) != nil || refused.Code != "plan_changed" || refused.Plan.Digest != "sha256:current" || len(refused.Plan.Members) != 1 {
			t.Fatal(name, w.Code, w.Body.String())
		}
		if cmd, _ := st.FindActionByIdempotencyKey("maintenance_command", "web-command:"+map[string]string{"old digest": "approve-old", "other members": "approve-more"}[name]); cmd != nil {
			t.Fatal(name, "a stale approval was queued")
		}
	}
	w := request(h, "POST", "/api/maintenance/batch-candidates", `{"id":"planned","digest":"sha256:current","item_keys":["a"],"decision":"approve","key":"approve-shown"}`, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Without the member list, the digest alone still approves, as before.
	w = request(h, "POST", "/api/maintenance/batch-candidates", `{"id":"planned","digest":"sha256:current","decision":"approve","key":"approve-digest"}`, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	// A lost answer replays under its key even once the plan has moved on.
	state := map[string]any{"batch_promotion_plan": plan, "batch_promotion_approved": true}
	inst, _ := st.GetActionInstance("planned")
	inst.StateJSON = mustJSON(t, state)
	st.UpdateActionInstance(*inst)
	if w := request(h, "POST", "/api/maintenance/batch-candidates", `{"id":"planned","digest":"sha256:current","item_keys":["a"],"decision":"approve","key":"approve-shown"}`, true); w.Code != 202 {
		t.Fatal("replay refused", w.Code, w.Body.String())
	}
}

func TestReplaceWhenDoneDefaultIsAServerPreference(t *testing.T) {
	s, h := testUI(t)
	read := func() map[string]any {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/preferences", "", true)
		var prefs map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &prefs) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return prefs
	}
	if prefs := read(); prefs["replace_when_done_default"] != false {
		t.Fatal("default is not off", prefs)
	}
	submit := func(key string, inputs map[string]any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(inputs)
		body := mustJSON(t, map[string]any{"name": "action_run", "background": true, "key": key, "arguments": map[string]any{"action": "transcode_media", "inputs": string(raw)}})
		w := request(h, "POST", "/api/maintenance/tool", body, true)
		var res struct{ ID string }
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &res) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		inst, _ := s.engine.Deps().Store.GetActionInstance(res.ID)
		return decodeOperationJSON(inst.InputsJSON)
	}
	if got := submit("submit-off", map[string]any{"path": "/media/off.mkv", "mode": "size"}); got["replace_when_done"] != false {
		t.Fatal("submit without the field did not take the default", got)
	}
	w := request(h, "PUT", "/api/maintenance/preferences", `{"replace_when_done_default":true}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if prefs := read(); prefs["replace_when_done_default"] != true {
		t.Fatal("preference not saved", prefs)
	}
	if got := submit("submit-on", map[string]any{"path": "/media/on.mkv", "mode": "size"}); got["replace_when_done"] != true {
		t.Fatal("submit without the field did not take the new default", got)
	}
	if got := submit("submit-said", map[string]any{"path": "/media/said.mkv", "mode": "size", "replace_when_done": false}); got["replace_when_done"] != false {
		t.Fatal("an explicit choice was overridden", got)
	}
	// A lost answer to the first submission replays after the default changed.
	if got := submit("submit-off", map[string]any{"path": "/media/off.mkv", "mode": "size"}); got["replace_when_done"] != false {
		t.Fatal("replay changed", got)
	}
	for _, bad := range []string{`{}`, `{"replace_when_done_default":"yes"}`, `{"other":true}`} {
		if w := request(h, "PUT", "/api/maintenance/preferences", bad, true); w.Code != 400 {
			t.Fatal(bad, w.Code)
		}
	}
	if w := request(h, "GET", "/api/maintenance/preferences", "", false); w.Code != 401 {
		t.Fatal("unauthenticated preferences", w.Code)
	}
}

// The switch on a running job or batch shows on the list, the detail and the
// batch's rows, in both directions; a finished job cannot be switched.
func TestReplaceWhenDoneSwitchesOnARunningJobOrBatch(t *testing.T) {
	s, h := testUI(t)
	s.cfg.AllowDestructive = true
	st := s.engine.Deps().Store
	seedOperation(t, st, "switch-batch", "transcode_batch", store.ActionStatusRunning, map[string]any{"paths": []string{"/media/s1.mkv", "/media/s2.mkv"}, "replace_when_done": false}, nil, nil)
	seedOperation(t, st, "switch-ready", "transcode_media", store.ActionStatusCompleted, map[string]any{"path": "/media/s1.mkv", "parent_action_id": "switch-batch"}, readyFile("/media/s1.mkv"), nil)
	seedOperation(t, st, "switch-encoding", "transcode_media", store.ActionStatusRunning, map[string]any{"path": "/media/s2.mkv", "parent_action_id": "switch-batch"}, nil, nil)
	seedOperation(t, st, "switch-finished", "transcode_media", store.ActionStatusCompleted, map[string]any{"path": "/media/s3.mkv", "replace_when_done": false}, readyFile("/media/s3.mkv"), nil)
	for _, item := range []store.TranscodeBatchItem{{BatchID: "switch-batch", ItemKey: "s1", FilePath: "/media/s1.mkv", Status: "completed", Decision: "transcode", ChildActionID: "switch-ready", SourceBytes: 1000}, {BatchID: "switch-batch", ItemKey: "s2", FilePath: "/media/s2.mkv", Status: "running", Decision: "transcode", ChildActionID: "switch-encoding", SourceBytes: 2000}} {
		if err := st.CreateTranscodeBatchItem(item); err != nil {
			t.Fatal(err)
		}
	}
	flags := func() (list map[string]any, detail map[string]any, rows map[string]map[string]any) {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/operations", "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		list = map[string]any{}
		for _, job := range page.Jobs {
			list[operationString(job["id"])] = job["replace_when_done"]
		}
		w = request(h, "GET", "/api/maintenance/operations?id=switch-batch", "", true)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		detail = page.Jobs[0]
		w = request(h, "GET", "/api/maintenance/batch-items?id=switch-batch", "", true)
		var items struct {
			Items           []map[string]any `json:"items"`
			ReplaceWhenDone bool             `json:"replace_when_done"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &items) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		rows = map[string]map[string]any{"batch": {"replace_when_done": items.ReplaceWhenDone}}
		for _, row := range items.Items {
			rows[operationString(row["item_key"])] = row
		}
		return
	}
	list, detail, rows := flags()
	if list["switch-batch"] != false || list["switch-encoding"] != false || detail["replace_when_done"] != false || rows["batch"]["replace_when_done"] != false || rows["s2"]["replace_when_done"] != false {
		t.Fatal("off at submit not shown", list, detail["replace_when_done"], rows)
	}
	// Each file's row carries its own before -> after for a batch review.
	if rows["s1"]["source_bytes"] != float64(1000) || rows["s1"]["candidate_bytes"] != float64(300) || rows["s2"]["source_bytes"] != float64(2000) || rows["s2"]["candidate_bytes"] != nil {
		t.Fatal("per-file sizes", rows["s1"], rows["s2"])
	}

	switchTo := func(id string, value bool) int {
		t.Helper()
		w := request(h, "POST", "/api/maintenance/replace-when-done", mustJSON(t, map[string]any{"id": id, "replace_when_done": value}), true)
		var got map[string]any
		json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code == 200 && got["replace_when_done"] != value {
			t.Fatal("answer does not say the new value", got)
		}
		return w.Code
	}
	if code := switchTo("switch-batch", true); code != 200 {
		t.Fatal(code)
	}
	list, detail, rows = flags()
	if list["switch-batch"] != true || list["switch-encoding"] != true || detail["replace_when_done"] != true || rows["batch"]["replace_when_done"] != true || rows["s2"]["replace_when_done"] != true {
		t.Fatal("switching on not shown", list, detail["replace_when_done"], rows)
	}
	// Switching on does not replace a file that was already ready.
	if p := promotionOf(t, st, "switch-ready"); p != nil {
		t.Fatal("switching on replaced a ready file", p)
	}
	if code := switchTo("switch-batch", false); code != 200 {
		t.Fatal(code)
	}
	if list, _, _ = flags(); list["switch-batch"] != false || list["switch-encoding"] != false {
		t.Fatal("switching off not shown", list)
	}
	if code := switchTo("switch-finished", true); code != 409 {
		t.Fatal("a finished job was switched", code)
	}
	if code := switchTo("missing", true); code != 404 {
		t.Fatal("unknown job", code)
	}
	if w := request(h, "POST", "/api/maintenance/replace-when-done", `{"id":"switch-batch"}`, true); w.Code != 400 {
		t.Fatal("missing value accepted", w.Code)
	}
}

// The web's generic endpoint cannot approve a replacement ahead either.
func TestToolEndpointRefusesAPromotionApproval(t *testing.T) {
	s, h := testUI(t)
	s.cfg.AllowDestructive = true
	st := s.engine.Deps().Store
	seedOperation(t, st, "ready", "transcode_media", store.ActionStatusCompleted, map[string]any{"path": "/media/ready.mkv"}, readyFile("/media/ready.mkv"), nil)
	for _, approval := range []string{"reviewed", "automatic"} {
		body := mustJSON(t, map[string]any{"name": "action_run", "arguments": map[string]any{"action": "promote_transcode_candidate", "inputs": `{"transcode_action_id":"ready","service":"filesystem","approval":"` + approval + `"}`, "idempotency_key": "self-" + approval}})
		w := request(h, "POST", "/api/maintenance/tool", body, true)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"field":"approval"`) {
			t.Fatal(approval, w.Code, w.Body.String())
		}
	}
	if p := promotionOf(t, st, "ready"); p != nil {
		t.Fatal("a self-approved replacement was admitted", p)
	}
}

// A batch's file reads replace_when_done from its batch's switch even when
// the batch is not on the page.
func TestReplaceWhenDoneOfAFileWhoseBatchIsNotListed(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "orphan-file", "transcode_media", store.ActionStatusRunning, map[string]any{"path": "/media/o.mkv", "parent_action_id": "gone-batch", "replace_when_done": false}, nil, nil)
	if err := st.SetReplaceWhenDone("gone-batch", true); err != nil {
		t.Fatal(err)
	}
	seedOperation(t, st, "paged-batch", "transcode_batch", store.ActionStatusRunning, map[string]any{"paths": []string{"/media/p.mkv"}, "replace_when_done": true}, nil, nil)
	seedOperation(t, st, "paged-file", "transcode_media", store.ActionStatusRunning, map[string]any{"path": "/media/p.mkv", "parent_action_id": "paged-batch", "replace_when_done": false}, nil, nil)
	for id, want := range map[string]bool{"orphan-file": true, "paged-file": true} {
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		if page.Jobs[0]["replace_when_done"] != want {
			t.Fatal(id, "does not show its batch's switch", page.Jobs[0]["replace_when_done"])
		}
	}
}
