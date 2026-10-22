package maintenanceui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

// The target follows the engine's retry guard: the batch re-runs only the
// replacements that failed in plan_promotion, a failure in approve_promotion
// has no retry (the approved plan no longer matches), and one that failed later
// is retried on its own.
func TestReplacementRetryTargetFollowsTheEnginesRetryGuard(t *testing.T) {
	s, _ := testUI(t)
	plan, approve, preserve := s.promotionStep("plan_promotion"), s.promotionStep("approve_promotion"), s.promotionStep("preserve_original")
	if plan != 0 || approve != 1 || preserve != 2 {
		t.Fatalf("the replacement workflow changed: plan %d approve %d preserve %d", plan, approve, preserve)
	}
	failed, running := store.ActionStatusFailed, store.ActionStatusRunning
	for _, tc := range []struct {
		name      string
		batch     string
		promotion string
		step      int
		want      string
	}{
		{"failed planning", failed, failed, plan, "batch-a"},
		{"failed at approval", failed, failed, approve, ""},
		{"failed while preserving the original", failed, failed, preserve, "promotion-a"},
		{"failed importing", failed, failed, s.promotionStep("import_candidate"), "promotion-a"},
		{"failed rescanning", failed, failed, s.promotionStep("rescan_library"), "promotion-a"},
		{"batch still running", running, failed, preserve, ""},
		{"member not failed", failed, store.ActionStatusCompleted, preserve, ""},
		{"member cancelled", failed, store.ActionStatusCancelled, plan, ""},
	} {
		got := s.replacementRetryTarget(store.ActionInstance{ID: "batch-a", Status: tc.batch}, store.ActionInstance{ID: "promotion-a", Status: tc.promotion, CurrentStep: tc.step})
		if got != tc.want {
			t.Errorf("%s: retry target %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Where the original is comes from what the replacement did. "In place" is said
// only when nothing was moved, published or removed, "in recovery" only when a
// recovery copy holds it, and anything else stays unclear.
func TestOriginalStateIsDecidedFromThePromotionsOwnState(t *testing.T) {
	s, _ := testUI(t)
	preserve, importStep, remove := s.promotionStep("preserve_original"), s.promotionStep("import_candidate"), s.promotionStep("remove_old_file")
	record := func(step int, promotion, state map[string]any) operationRecord {
		return operationRecord{inst: store.ActionInstance{ID: "p", Status: store.ActionStatusFailed, CurrentStep: step}, outputs: map[string]any{"promotion": promotion}, state: state}
	}
	copyMode := func(extra map[string]any) map[string]any {
		out := map[string]any{"original_path": "/media/a.mkv", "recovery_mode": "copy"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	moveAside := func(extra map[string]any) map[string]any {
		out := map[string]any{"original_path": "/media/a.mkv", "recovery_mode": "move_aside"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for _, tc := range []struct {
		name string
		r    operationRecord
		want string
	}{
		{"no promotion record", operationRecord{inst: store.ActionInstance{Status: store.ActionStatusFailed, CurrentStep: importStep}, outputs: map[string]any{}}, originalInPlace},
		{"stopped at the first step", record(0, map[string]any{"original_path": "/media/a.mkv"}, nil), originalInPlace},
		{"copy mode, stopped while copying the backup", record(preserve, copyMode(nil), nil), originalInPlace},
		// A same-name publish renames the candidate over the original and none of
		// the older flags is set; the step and the publish marker say so.
		{"copy mode, same-name publish then a failed check", record(importStep, copyMode(map[string]any{"new_path": "/media/a.mkv"}), map[string]any{"filesystem_publish_started": true, "filesystem_published": true}), originalInRecovery},
		{"copy mode, past the backup without a marker", record(importStep, copyMode(nil), nil), originalInRecovery},
		{"copy mode, old file removed", record(remove, copyMode(map[string]any{"old_removed": true}), nil), originalInRecovery},
		// Move-aside mode makes no copy: before the move the original is simply
		// where it was, and a failure there must not point at a recovery copy.
		{"move-aside mode, failed importing before any move", record(importStep, moveAside(nil), nil), originalInPlace},
		// Sonarr and Radarr may remove the old file themselves while importing, and
		// in move-aside mode no recovery copy exists yet.
		{"move-aside mode, arr import sent (snapshot flag), failed importing", record(importStep, moveAside(map[string]any{"service": "sonarr", "import_sent": true}), nil), originalUnclear},
		{"move-aside mode, arr import sent (full command), failed importing", record(importStep, moveAside(map[string]any{"service": "radarr", "commands": map[string]any{"import": map[string]any{"id": 7, "sent_at": "2026-01-01T00:00:00Z"}}}), nil), originalUnclear},
		{"move-aside mode, arr import not sent, failed importing", record(importStep, moveAside(map[string]any{"service": "sonarr", "commands": map[string]any{}}), nil), originalInPlace},
		// A refused move-aside switches to copy; until its fallback copy is
		// verified the original is still in place and no recovery copy holds it.
		{"copy after a refused move-aside, fallback copy not verified", record(importStep, copyMode(map[string]any{"move_aside_refused": "rename not permitted", "recovery_verified": false}), nil), originalUnclear},
		{"copy after a refused move-aside, fallback copy verified", record(importStep, copyMode(map[string]any{"move_aside_refused": "rename not permitted", "recovery_verified": true}), nil), originalInRecovery},
		// Once the recovery copy started to be removed it may be gone.
		{"recovery copy removal started", record(s.promotionStep("finalize_promotion"), copyMode(map[string]any{"recovery_cleanup_started": true}), nil), originalUnclear},
		{"recovery copy removal started, moved aside", record(s.promotionStep("finalize_promotion"), moveAside(map[string]any{"moved_aside": true, "recovery_cleanup_started": true}), nil), originalUnclear},
		{"move-aside mode, move started", record(importStep, moveAside(map[string]any{"move_aside_started_at": "2026-01-01T00:00:00Z"}), nil), originalUnclear},
		{"move-aside mode, moved aside", record(remove, moveAside(map[string]any{"moved_aside": true}), nil), originalInRecovery},
		{"move-aside mode, past import without a marker", record(remove, moveAside(nil), nil), originalUnclear},
		{"publish started, mode unknown", record(preserve, map[string]any{"original_path": "/media/a.mkv"}, map[string]any{"filesystem_publish_started": true}), originalUnclear},
		{"past the backup, mode unknown", record(importStep, map[string]any{"original_path": "/media/a.mkv"}, nil), originalUnclear},
		{"delete sent, mode unknown", record(preserve, map[string]any{"delete_sent_at": "2026-01-01T00:00:00Z"}, nil), originalUnclear},
	} {
		if got := s.originalState(tc.r); got != tc.want {
			t.Errorf("%s: original %q, want %q", tc.name, got, tc.want)
		}
	}
	failure := s.failureOf(record(importStep, copyMode(nil), map[string]any{"filesystem_published": true}))
	if failure == nil || failure.OriginalIntact || failure.OriginalState != originalInRecovery {
		t.Fatalf("a published replacement must not be reported as leaving the original in place: %+v", failure)
	}
}

func TestBatchPromotionFailureReadsOnlyThatFilesEntry(t *testing.T) {
	outputs := map[string]any{"batch_promotion": map[string]any{"failed": []any{"file-a: promotion validation failed: audio bitrate mismatch", "file-ab: disk full"}}}
	if got := batchPromotionFailure(outputs, "file-a"); got != "promotion validation failed: audio bitrate mismatch" {
		t.Fatal(got)
	}
	if got := batchPromotionFailure(outputs, "file-ab"); got != "disk full" {
		t.Fatal(got)
	}
	if got := batchPromotionFailure(outputs, "file"); got != "" {
		t.Fatalf("a key must not match another file's longer key, got %q", got)
	}
	if got := batchPromotionFailure(map[string]any{}, "file-a"); got != "" {
		t.Fatal(got)
	}
}

func TestPlainReplacementReasonNeverShowsServerText(t *testing.T) {
	for detail, want := range map[string]string{
		"promotion validation failed: audio bitrate mismatch (x)": "final check before replacing found an audio mismatch",
		"promotion validation failed: duration":                   "final check before replacing did not pass",
		"write: no space left on device":                          "there was not enough free space for the backup copy",
		"candidate sha256 changed":                                "a file changed while it was being replaced",
		"sonarr rescan failed":                                    "library did not accept the new file",
		"something unforeseen":                                    "replacement did not finish",
	} {
		got := plainReplacementReason(detail)
		if got != want || strings.Contains(got, detail) && detail != want {
			t.Fatalf("%q -> %q, want %q", detail, got, want)
		}
	}
}

func TestReplacementPlanStateSeparatesReviewQueuedAndUnplanned(t *testing.T) {
	plan := func(approved bool) map[string]any {
		return map[string]any{"batch_promotion_approved": approved, "batch_promotion_plan": map[string]any{"members": []any{map[string]any{"item_key": "k", "transcode_action_id": "t-a"}}}}
	}
	if got := replacementPlanState(plan(false), "t-a"); got != "review" {
		t.Fatal(got)
	}
	if got := replacementPlanState(plan(true), "t-a"); got != "queued" {
		t.Fatal(got)
	}
	if got := replacementPlanState(plan(true), "t-other"); got != "" {
		t.Fatal(got)
	}
	if got := replacementPlanState(map[string]any{}, "t-a"); got != "" {
		t.Fatal(got)
	}
}

func TestReplacementFactsAreReadFromTheTranscodeRecord(t *testing.T) {
	outputs := map[string]any{
		"quality_evidence": map[string]any{"vmaf": map[string]any{"mean": 93.4, "p5": 91.0, "worst_window_mean": 90.2}},
		"tracks_message":   "Kept English and Japanese audio",
		"conversions":      []any{map[string]any{"stream_type": "audio", "to_codec": "eac3"}, map[string]any{"stream_type": "audio", "to_codec": "eac3"}},
	}
	facts := replacementFactsOf(operationRecord{inst: store.ActionInstance{ID: "t-a", ActionName: "transcode_media", Status: store.ActionStatusCompleted}, outputs: outputs})
	if facts.VMAFMean == nil || *facts.VMAFMean != 93.4 || facts.VMAFWorstWindow == nil || *facts.VMAFWorstWindow != 90.2 {
		t.Fatalf("%+v", facts)
	}
	if facts.TracksMessage != "Kept English and Japanese audio" || facts.AudioSummary != "2 audio tracks converted to E-AC-3" {
		t.Fatalf("%+v", facts)
	}
	encoded, _ := json.Marshal(facts)
	if strings.Contains(string(encoded), "/") {
		t.Fatalf("display facts carry no paths: %s", encoded)
	}
}

// A failed batch lists its files in plain words: the replaced one, the one
// still queued, and the one whose promotion failed without an action of its
// own, which is retried through the batch.
func TestBatchItemsReportReplacementStateInPlainWords(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	planMembers := []any{}
	for _, key := range []string{"file-a", "file-b", "file-c"} {
		planMembers = append(planMembers, map[string]any{"item_key": key, "episode": "Sample Show " + key, "transcode_action_id": "t-" + key, "original_path": "/media/" + key + ".mkv", "candidate_path": "/media/.candidates/" + key + ".mkv", "candidate_sha256": "sha-" + key})
	}
	state := map[string]any{"batch_promotion_approved": true, "batch_promotion_plan": map[string]any{"members": planMembers, "digest": "sha256:plan"}}
	outputs := map[string]any{"batch_promotion": map[string]any{"failed": []any{"file-c: promotion validation failed: audio bitrate mismatch"}}}
	seedOperation(t, st, "batch", "transcode_batch", "failed", map[string]any{"mode": "size"}, outputs, state)
	for _, key := range []string{"file-a", "file-b", "file-c"} {
		child := "t-" + key
		seedOperation(t, st, child, "transcode_media", "completed", map[string]any{"parent_action_id": "batch", "path": "/media/" + key + ".mkv"}, map[string]any{"original_intact": true}, nil)
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: key, FilePath: "/media/" + key + ".mkv", Status: "completed", ChildActionID: child, Decision: "transcode", DisplayLabel: "Sample Show " + key}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateActionInstance(store.ActionInstance{ID: "promotion-a", ActionName: "promote_transcode_candidate", Status: "completed", InputsJSON: "{}", OutputsJSON: `{"promoted":true,"recovery_retained":false}`, StateJSON: "{}", IdempotencyKey: "promote:filesystem:t-file-a"}); err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/api/maintenance/batch-items?id=batch", "", true)
	var page struct {
		Items []batchItemView `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
	byKey := map[string]batchItemView{}
	for _, item := range page.Items {
		byKey[item.ItemKey] = item
	}
	if a := byKey["file-a"]; !a.Replaced || a.ReplacementFailure != nil || a.RetryActionID != "" {
		t.Fatalf("replaced file: %+v", a)
	}
	if b := byKey["file-b"]; b.ReplacementPlan != "queued" || b.CandidateReady || b.ReplacementFailure != nil {
		t.Fatalf("queued file: %+v", b)
	}
	c := byKey["file-c"]
	if c.ReplacementFailure == nil || c.ReplacementFailure.Reason != "final check before replacing found an audio mismatch" || !c.ReplacementFailure.OriginalIntact || c.RetryActionID != "" || c.CandidateReady {
		t.Fatalf("failed file: %+v", c)
	}
	if strings.Contains(c.ReplacementFailure.Reason, "file-c") {
		t.Fatal("the reason must not name the item key")
	}
}

// One Retry on the batch re-runs only the files that failed in plan_promotion; the count the screen quotes ("Retry 2 failed replacements") must be
// exactly those, not every failed file.
func TestBatchRetryCountCountsOnlyFilesTheBatchRetryReruns(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	members := []any{}
	for _, key := range []string{"file-a", "file-b", "file-c", "file-d"} {
		members = append(members, map[string]any{"item_key": key, "episode": "Sample Show " + key, "transcode_action_id": "t-" + key, "original_path": "/media/" + key + ".mkv", "candidate_path": "/media/.candidates/" + key + ".mkv", "candidate_sha256": "sha-" + key})
	}
	state := map[string]any{"batch_promotion_approved": true, "batch_promotion_plan": map[string]any{"members": members, "digest": "sha256:plan"}}
	seedOperation(t, st, "batch", "transcode_batch", "failed", map[string]any{"mode": "size"}, nil, state)
	steps := map[string]struct {
		status string
		step   int
	}{"file-a": {"failed", 0}, "file-b": {"failed", 1}, "file-c": {"failed", 3}, "file-d": {"completed", 7}}
	for key, want := range steps {
		seedOperation(t, st, "t-"+key, "transcode_media", "completed", map[string]any{"parent_action_id": "batch", "path": "/media/" + key + ".mkv"}, map[string]any{"original_intact": true}, nil)
		if err := st.CreateActionInstance(store.ActionInstance{ID: "promotion-" + key, ActionName: "promote_transcode_candidate", Status: want.status, CurrentStep: want.step, InputsJSON: `{"transcode_action_id":"t-` + key + `"}`, OutputsJSON: "{}", StateJSON: "{}", IdempotencyKey: "promote:filesystem:t-" + key}); err != nil {
			t.Fatal(err)
		}
	}
	w := request(h, "GET", "/api/maintenance/operations?id=batch", "", true)
	var page struct {
		Jobs []struct {
			Replacement *replacementProgress `json:"replacement"`
		} `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 || page.Jobs[0].Replacement == nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if r := page.Jobs[0].Replacement; r.Failed != 3 || r.Done != 1 || r.BatchRetryCount != 1 || r.Retryable != 2 || r.BatchRecordedFailed != 0 {
		t.Fatalf("failed %d done %d batch retry %d, want 3 failed, 1 done, 1 retried by the batch (only the plan_promotion failure)", r.Failed, r.Done, r.BatchRetryCount)
	}
}

// While the batch holds a prepared replacement plan its files are replaced by
// that plan: they offer no Change goal and no retry-pending override.
func TestBatchWithAPreparedPlanOffersChildrenNoGoalChange(t *testing.T) {
	s, h := testUI(t)
	seedActionableFiles(t, s)
	st := s.engine.Deps().Store
	read := func() map[string]any {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/operations?id=wait", "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		return page.Jobs[0]
	}
	if read()["parent_item_key"] != "wait" {
		t.Fatal("a batch without a plan must let the file change its goal")
	}
	parent, _ := st.GetActionInstance("batch")
	parent.StateJSON = `{"batch_promotion_plan":{"members":[{"item_key":"ready","transcode_action_id":"ready"}],"digest":"sha256:plan"}}`
	if err := st.UpdateActionInstance(*parent); err != nil {
		t.Fatal(err)
	}
	if key, ok := read()["parent_item_key"]; ok {
		t.Fatalf("a batch with a prepared plan offered Change goal for %v", key)
	}
}

func TestPreparedPlanReadIsAuthenticatedAndEmptyBeforePreparing(t *testing.T) {
	s, h := testUI(t)
	seedOperation(t, s.engine.Deps().Store, "finished", "transcode_batch", "completed", map[string]any{"paths": []string{"/media/file.mkv"}}, nil, nil)
	w := request(h, "GET", "/api/maintenance/batch-candidates?id=finished&plan=1", "", true)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"prepared":false}` {
		t.Fatal(w.Code, w.Body.String())
	}
	if request(h, "GET", "/api/maintenance/batch-candidates?id=finished&plan=1", "", false).Code != 401 {
		t.Fatal("unauthenticated plan read")
	}
	if request(h, "GET", "/api/maintenance/batch-candidates?id=missing&plan=1", "", true).Code == 200 {
		t.Fatal("a plan was read for a batch that does not exist")
	}
}
