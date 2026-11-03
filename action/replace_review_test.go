package action

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
)

// reviewEngine is an engine with replacement allowed and no library service,
// so files are replaced as plain files.
func reviewEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewEngine(EngineDeps{Store: st, Config: &config.Config{AllowDestructive: true}}), st
}

// seedReady adds a finished transcode whose new file is ready to replace.
func seedReady(t *testing.T, st *store.Store, id, parent string) {
	t.Helper()
	path := "/media/" + id + ".mkv"
	inputs := map[string]any{"path": path}
	if parent != "" {
		inputs["parent_action_id"] = parent
	}
	outputs := map[string]any{"original_intact": true, "candidate_path": path + ".candidate.mkv"}
	state := map[string]any{"resolved_path": path, "candidate_sha256": "after-" + id}
	if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "transcode_media", Status: StatusCompleted, InputsJSON: toJSON(inputs), OutputsJSON: toJSON(outputs), StateJSON: toJSON(state)}); err != nil {
		t.Fatal(err)
	}
}

func seedPromotion(t *testing.T, st *store.Store, id, source, status string, step int, state map[string]any) {
	t.Helper()
	if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: "promote_transcode_candidate", Status: status, CurrentStep: step, IdempotencyKey: "promote:filesystem:" + source, InputsJSON: toJSON(map[string]any{"transcode_action_id": source, "service": "filesystem"}), StateJSON: toJSON(state), OutputsJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
}

func promotions(t *testing.T, st *store.Store) []store.ActionInstance {
	t.Helper()
	rows, err := st.ListActionInstances("", 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := []store.ActionInstance{}
	for _, row := range rows {
		if row.ActionName == "promote_transcode_candidate" {
			out = append(out, row)
		}
	}
	return out
}

func replaceIDs(ids ...string) ReplaceRequest {
	return ReplaceRequest{IDs: ids, Key: "review-" + strings.Join(ids, "-")}
}

func mustReplace(t *testing.T, e *Engine, req ReplaceRequest) ReplaceAnswer {
	t.Helper()
	answer, err := e.ReplaceReviewed(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

func answerFiles(t *testing.T, a ReplaceAnswer) []ReplaceFile {
	t.Helper()
	var out struct {
		Replacements []ReplaceFile `json:"replacements"`
	}
	b, _ := json.Marshal(a.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out.Replacements
}

// Review item 1: an approval is never an input. Whoever starts a promotion
// (action_run, the web's generic endpoint) cannot approve it ahead.
func TestPromoteRefusesAnApprovalInput(t *testing.T) {
	e, st := reviewEngine(t)
	seedReady(t, st, "file-a", "")
	for _, approval := range []string{ApprovalReviewed, ApprovalAutomatic} {
		_, err := e.Enqueue(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": "file-a", "service": "filesystem", "approval": approval}, "outside-"+approval)
		var invalid *InvalidParamsError
		if err == nil || !errors.As(err, &invalid) || invalid.Field != "approval" {
			t.Fatalf("approval %q accepted: %v", approval, err)
		}
	}
	if rows := promotions(t, st); len(rows) != 0 {
		t.Fatal("a promotion was admitted", rows)
	}
	if a, _ := st.GetReplacementApproval("file-a"); a != nil {
		t.Fatal("an approval was recorded", a)
	}
}

// Review item 2: a plan already waiting is approved only with the digest the
// person was shown, and never past a regrab warning.
func TestReplaceApprovesAWaitingPlanOnlyWithTheDigestShown(t *testing.T) {
	e, st := reviewEngine(t)
	seedReady(t, st, "single", "")
	promotion := map[string]any{"transcode_action_id": "single", "service": "filesystem", "original_path": "/media/single.mkv", "candidate_path": "/media/single.mkv.candidate.mkv", "original_sha256": "before", "candidate_sha256": "after"}
	seedPromotion(t, st, "waiting", "single", StatusWaitingDecision, 1, map[string]any{"promotion": promotion})
	digest := PromotionPlanDigest(promotion)
	refusedAs := func(a ReplaceAnswer) ReplaceRefused {
		t.Helper()
		refused, _ := a.Body["not_ready"].([]ReplaceRefused)
		if a.Status != 409 || len(refused) != 1 {
			t.Fatalf("%d %v", a.Status, a.Body)
		}
		return refused[0]
	}
	for name, sent := range map[string]map[string]string{"none": nil, "another": {"single": "sha256:other"}} {
		r := refusedAs(mustReplace(t, e, ReplaceRequest{IDs: []string{"single"}, Digests: sent, Key: "review-" + name}))
		if r.Reason != ReplacePlanChanged || r.PlanDigest != digest {
			t.Fatal(name, r)
		}
	}
	if cmd, _ := st.FindActionByIdempotencyKey("maintenance_command", "web-command:replace-review-none-waiting"); cmd != nil {
		t.Fatal("an approval nobody saw was queued")
	}

	warned := map[string]any{}
	for k, v := range promotion {
		warned[k] = v
	}
	warned["regrab_warning"] = CompactRegrabWarning
	inst, _ := st.GetActionInstance("waiting")
	inst.StateJSON = toJSON(map[string]any{"promotion": warned})
	if err := st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	warnedDigest := PromotionPlanDigest(warned)
	if r := refusedAs(mustReplace(t, e, ReplaceRequest{IDs: []string{"single"}, Digests: map[string]string{"single": warnedDigest}, Key: "review-warned"})); r.Reason != ReplaceNeedsDecision {
		t.Fatal("a regrab warning was approved from the list", r)
	}

	inst.StateJSON = toJSON(map[string]any{"promotion": promotion})
	if err := st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	a := mustReplace(t, e, ReplaceRequest{IDs: []string{"single"}, Digests: map[string]string{"single": digest}, Key: "review-shown"})
	if files := answerFiles(t, a); a.Status != 202 || len(files) != 1 || files[0].Approved != replaceWaiting || files[0].ReplacementActionID != "waiting" {
		t.Fatal(a.Status, a.Body)
	}
	cmd, _ := st.FindActionByIdempotencyKey("maintenance_command", "web-command:replace-review-shown-waiting")
	if cmd == nil || decodeInputs(cmd.InputsJSON)["plan_digest"] != digest {
		t.Fatal("approval not queued with the digest shown", cmd)
	}
}

// Review item 3, scenario B: a file whose own automatic replacement is
// queued is approved as reviewed, not refused nor replaced twice.
func TestReplaceApprovesAQueuedAutomaticReplacement(t *testing.T) {
	e, st := reviewEngine(t)
	seedReady(t, st, "auto", "")
	if err := st.SetReplaceWhenDone("auto", true); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAutoReplacePending("auto"); err != nil {
		t.Fatal(err)
	}
	e.submitAutoReplacement(context.Background(), "auto", false)
	rows := promotions(t, st)
	if len(rows) != 1 {
		t.Fatal("automatic replacement not queued", rows)
	}
	if a, _ := st.GetReplacementApproval("auto"); a == nil || a.Approval != ApprovalAutomatic {
		t.Fatal("automatic approval not recorded", a)
	}
	a := mustReplace(t, e, replaceIDs("auto"))
	if files := answerFiles(t, a); a.Status != 202 || len(files) != 1 || files[0].Approved != replaceQueued || files[0].ReplacementActionID != rows[0].ID {
		t.Fatal(a.Status, a.Body)
	}
	if rows := promotions(t, st); len(rows) != 1 {
		t.Fatal("replaced twice", rows)
	}
	if a, _ := st.GetReplacementApproval("auto"); a == nil || a.Approval != ApprovalReviewed {
		t.Fatal("not approved as reviewed", a)
	}
}

// seedBatch adds a finished batch of plain files whose two files are ready.
func seedBatch(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.CreateActionInstance(store.ActionInstance{ID: "batch", ActionName: "transcode_batch", Status: StatusCompleted, InputsJSON: toJSON(map[string]any{"paths": []string{"/media/file-a.mkv", "/media/file-b.mkv"}}), StateJSON: "{}", OutputsJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"file-a", "file-b"} {
		seedReady(t, st, id, "batch")
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: id, FilePath: "/media/" + id + ".mkv", CandidatePath: "/media/" + id + ".mkv.candidate.mkv", Status: "completed", Decision: "transcode", ChildActionID: id}); err != nil {
			t.Fatal(err)
		}
	}
}

func memberIDs(members []batchPromotionMember) []string {
	out := []string{}
	for _, m := range members {
		out = append(out, m.SourceAction)
	}
	return out
}

// Review item 3, scenario C: a batch plan prepared while a reviewed
// replacement is being submitted waits for it, then leaves its file out.
func TestABatchPlanCannotClaimAFileWhileItsReviewIsSubmitted(t *testing.T) {
	e, st := reviewEngine(t)
	seedBatch(t, st)
	review, err := e.BatchCandidates(context.Background(), "batch")
	if err != nil || len(review.Members) != 2 {
		t.Fatal(review, err)
	}
	prepared := make(chan error, 1)
	e.afterReplaceCheck = func() {
		go func() {
			_, err := e.PrepareBatchPromotion(context.Background(), "batch", review.Version, "receipt", []string{"file-a", "file-b"})
			prepared <- err
		}()
		select {
		case err := <-prepared:
			t.Errorf("the batch plan was prepared between the review's check and its submission: %v", err)
			prepared <- err
		case <-time.After(150 * time.Millisecond):
		}
	}
	if a := mustReplace(t, e, replaceIDs("file-a")); a.Status != 202 {
		t.Fatal(a.Status, a.Body)
	}
	if err := <-prepared; err == nil {
		t.Fatal("a plan with a file already being replaced was prepared")
	}
	review, err = e.BatchCandidates(context.Background(), "batch")
	if err != nil || !reflect.DeepEqual(memberIDs(review.Members), []string{"file-b"}) {
		t.Fatal("the reviewed file is still offered to the batch", review, err)
	}
}

// Review item 3: when a submission fails partway, the answer says exactly
// which files started; when none did, nothing is kept under the key.
func TestReplaceSaysWhichFilesStartedWhenOneFails(t *testing.T) {
	e, st := reviewEngine(t)
	seedReady(t, st, "file-a", "")
	seedReady(t, st, "file-b", "")
	e.afterReplaceCheck = func() {
		// Something outside the lock takes file-b's key with other inputs.
		if err := st.CreateActionInstance(store.ActionInstance{ID: "squatter", ActionName: "promote_transcode_candidate", Status: StatusCompleted, IdempotencyKey: "promote:filesystem:file-b", InputsJSON: toJSON(map[string]any{"transcode_action_id": "file-b", "service": "filesystem", "series_id": 9}), StateJSON: "{}", OutputsJSON: "{}"}); err != nil {
			t.Error(err)
		}
	}
	a := mustReplace(t, e, replaceIDs("file-a", "file-b"))
	files := answerFiles(t, a)
	if a.Status != 207 || len(files) != 2 || !files[0].Started || files[0].ID != "file-a" || files[1].Started || files[1].ID != "file-b" || files[1].Error == "" {
		t.Fatal(a.Status, a.Body)
	}
	if got, _ := st.GetReplacementApproval("file-b"); got != nil {
		t.Fatal("the file that did not start keeps an approval", got)
	}
	if replay := mustReplace(t, e, replaceIDs("file-a", "file-b")); replay.Status != 207 {
		t.Fatal("the partial answer was not kept", replay.Status)
	}
}

// Review item 4: a key's answer is kept; the same key for other files is
// refused.
func TestReplaceKeepsTheAnswerToAKey(t *testing.T) {
	e, st := reviewEngine(t)
	seedReady(t, st, "file-a", "")
	seedReady(t, st, "file-b", "")
	first := mustReplace(t, e, ReplaceRequest{IDs: []string{"file-a"}, Key: "lost-answer"})
	if first.Status != 202 {
		t.Fatal(first.Status, first.Body)
	}
	replay := mustReplace(t, e, ReplaceRequest{IDs: []string{"file-a"}, Key: "lost-answer"})
	normal := func(body map[string]any) string {
		b, _ := json.Marshal(body)
		var v any
		_ = json.Unmarshal(b, &v)
		b, _ = json.Marshal(v)
		return string(b)
	}
	firstJSON, replayJSON := normal(first.Body), normal(replay.Body)
	if replay.Status != 202 || string(firstJSON) != string(replayJSON) {
		t.Fatalf("replay answered differently:\n%s\n%s", firstJSON, replayJSON)
	}
	other := mustReplace(t, e, ReplaceRequest{IDs: []string{"file-b"}, Key: "lost-answer"})
	if other.Status != 409 || other.Body["code"] != "key_reused" {
		t.Fatal(other.Status, other.Body)
	}
	if p, _ := st.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:filesystem:file-b"); p != nil {
		t.Fatal("a reused key replaced another file")
	}
}

// Review item 8: a replacement that was rejected, or refused before its
// approval, can be replaced again from the list. Review 2, item 7: its new
// plan is one nobody saw, so it is planned again and waits for a decision.
func TestReplaceRetriesARejectedOrRefusedReplacement(t *testing.T) {
	for _, step := range []int{0, 1} {
		e, st := reviewEngine(t)
		seedReady(t, st, "file-a", "")
		seedPromotion(t, st, "earlier", "file-a", StatusFailed, step, map[string]any{})
		if err := st.GrantReplacementApproval("file-a", ApprovalReviewed, "earlier"); err != nil {
			t.Fatal(err)
		}
		a := mustReplace(t, e, replaceIDs("file-a"))
		if files := answerFiles(t, a); a.Status != 202 || len(files) != 1 || files[0].Approved != "" || files[0].Reason != ReplaceNeedsReview || !files[0].Started || files[0].ReplacementActionID != "earlier" {
			t.Fatal(step, a.Status, a.Body)
		}
		if inst, _ := st.GetActionInstance("earlier"); inst.CurrentStep != 0 {
			t.Fatal("not planned again", inst.CurrentStep)
		}
		cmd, _ := st.FindActionByIdempotencyKey("maintenance_command", "web-command:replace-review-file-a-earlier")
		if cmd == nil || decodeInputs(cmd.InputsJSON)["kind"] != "retry" {
			t.Fatal("no retry queued", cmd)
		}
		if got, _ := st.GetReplacementApproval("file-a"); got != nil {
			t.Fatal("a plan nobody saw was approved", got)
		}
	}
	// Past its approval, a failed replacement is not started again from here.
	e, st := reviewEngine(t)
	seedReady(t, st, "file-a", "")
	seedPromotion(t, st, "earlier", "file-a", StatusFailed, 3, map[string]any{})
	if refused, _ := mustReplace(t, e, replaceIDs("file-a")).Body["not_ready"].([]ReplaceRefused); len(refused) != 1 || refused[0].Reason != ReplaceStarted {
		t.Fatal(refused)
	}
}

// Review item 7: a file whose own job replaces it by itself, and has yet to
// submit that, is left out of its batch's plan.
func TestABatchPlanLeavesOutAFileReplacedByItself(t *testing.T) {
	e, st := reviewEngine(t)
	seedBatch(t, st)
	if err := st.AddAutoReplacePending("file-a"); err != nil {
		t.Fatal(err)
	}
	review, err := e.BatchCandidates(context.Background(), "batch")
	if err != nil || !reflect.DeepEqual(memberIDs(review.Members), []string{"file-b"}) {
		t.Fatal(review, err)
	}
	// The batch's own plan step builds under the same lock.
	e.replaceMu.Lock()
	built := make(chan struct{})
	go func() {
		inst, _ := st.GetActionInstance("batch")
		_, _ = e.planBatchPromotion(context.Background(), parseExecutionContext(inst, e))
		close(built)
	}()
	select {
	case <-built:
		t.Fatal("the batch plan was built without the replacement lock")
	case <-time.After(100 * time.Millisecond):
	}
	e.replaceMu.Unlock()
	<-built
}

// Review 2, item 1: the service's own reconciler loop submits an automatic
// replacement a crash left behind.
func TestTheReconcilerLoopSubmitsALeftoverAutomaticReplacement(t *testing.T) {
	e, st := reviewEngine(t)
	e.deps.ReconcileInterval = 20 * time.Millisecond
	seedReady(t, st, "file-a", "")
	if err := st.SetReplaceWhenDone("file-a", true); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAutoReplacePending("file-a"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := e.StartReconciler(ctx)
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if pending, _, _ := st.AutoReplacePending("file-a"); !pending {
			if p, _ := st.FindActionByIdempotencyKey("promote_transcode_candidate", "promote:filesystem:file-a"); p != nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the reconciler loop never submitted it")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Review 2, item 6, and the final review: a note that has to wait for the
// job's lease is still written, neither under the replacement lock nor
// holding up the reconciler's tick.
func TestASkippedNoteWaitsForTheJobsLease(t *testing.T) {
	e, st := reviewEngine(t)
	seedReady(t, st, "file-a", "")
	if err := st.SetReplaceWhenDone("file-a", true); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAutoReplacePending("file-a"); err != nil {
		t.Fatal(err)
	}
	e.beforeAutoEnqueue = func(string) error { return errors.New("admission is closed") }
	_, release, err := e.claimExecution(context.Background(), "file-a", true)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(3 * time.Second); release() }()
	began := time.Now()
	e.drainAutoReplacements(context.Background())
	if waited := time.Since(began); waited > time.Second {
		t.Fatalf("the drain waited %s for a job's lease", waited)
	}
	locked := make(chan struct{})
	go func() { e.replaceMu.Lock(); e.replaceMu.Unlock(); close(locked) }()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("the replacement lock is held while waiting for a job's lease")
	}
	e.notes.Wait()
	inst, _ := st.GetActionInstance("file-a")
	if !strings.Contains(getString(decodeInputs(inst.OutputsJSON), "replace_when_done_skipped"), "admission is closed") {
		t.Fatal("the note was dropped")
	}
}

// Review 2, item 8, and the final review: a replay that omits
// replace_when_done matches whatever the job took; one that sends a
// different value is refused, never answered with a job that replaces when
// it was asked not to.
func TestAReplayMatchesReplaceWhenDoneOnlyWhenOmitted(t *testing.T) {
	e, media := finishingTranscode(t, true)
	first, err := e.Enqueue(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt", "replace_when_done": true}, "rerun-key")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := e.Enqueue(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt"}, "rerun-key"); err != nil || again.ID != first.ID {
		t.Fatalf("a replay without the field was refused: %v %v", again, err)
	}
	if again, err := e.Enqueue(context.Background(), "transcode_media", map[string]any{"path": media, "profile": "hevc-vt", "replace_when_done": false}, "rerun-key"); err == nil {
		t.Fatalf("an explicit false returned a job that replaces by itself: %v", again)
	}
}
