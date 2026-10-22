package maintenanceui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestReplacementRetryTargetIsAlwaysTheFailedBatch(t *testing.T) {
	failedBatch := store.ActionInstance{ID: "batch-a", Status: store.ActionStatusFailed}
	for _, step := range []int{0, 3} {
		failedMember := store.ActionInstance{ID: "promotion-a", Status: store.ActionStatusFailed, CurrentStep: step}
		if got := replacementRetryTarget(failedBatch, failedMember); got != "batch-a" {
			t.Fatalf("step %d: a member's retry must go through the batch, got %q", step, got)
		}
	}
	running := store.ActionInstance{ID: "batch-a", Status: store.ActionStatusRunning}
	if got := replacementRetryTarget(running, store.ActionInstance{ID: "promotion-a", Status: store.ActionStatusFailed}); got != "" {
		t.Fatalf("a batch that is still running cannot be retried, got %q", got)
	}
	if got := replacementRetryTarget(failedBatch, store.ActionInstance{ID: "promotion-a", Status: store.ActionStatusCompleted}); got != "" {
		t.Fatalf("a finished replacement has nothing to retry, got %q", got)
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
	if c.ReplacementFailure == nil || c.ReplacementFailure.Reason != "final check before replacing found an audio mismatch" || !c.ReplacementFailure.OriginalIntact || c.RetryActionID != "batch" || c.CandidateReady || c.CanKeepOriginal {
		t.Fatalf("failed file: %+v", c)
	}
	if strings.Contains(c.ReplacementFailure.Reason, "file-c") {
		t.Fatal("the reason must not name the item key")
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
