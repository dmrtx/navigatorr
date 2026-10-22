package action

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// candidateRoot is a canonical media root: the promotion guard refuses paths
// that traverse symlinks, and t.TempDir is a symlinked alias on darwin.
func candidateRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func candidateEngine(t *testing.T, st *store.Store, root string, tc transcode.Executor) *Engine {
	t.Helper()
	resolver, err := fsop.NewResolver([]string{root}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	return NewEngine(EngineDeps{Store: st, Config: &config.Config{}, Fs: resolver, Transcode: tc})
}

func writeCandidateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedCandidateAttempt records a transcode attempt with the state the submit
// step writes: the original, its job id and the candidate path derived from both.
func seedCandidateAttempt(t *testing.T, st *store.Store, id, status, original, jobID string, inputs, extra map[string]any) string {
	t.Helper()
	stem := strings.TrimSuffix(filepath.Base(original), filepath.Ext(original))
	candidate := filepath.Join(filepath.Dir(original), ".navigatorr-candidates", stem+"."+jobID+".mkv")
	state := map[string]any{"resolved_path": original, "job_id": jobID, "candidate_path": candidate, "original_sha256": "baseline", "transcode_submitted": true}
	for key, value := range extra {
		state[key] = value
	}
	if inputs == nil {
		inputs = map[string]any{}
	}
	inputs["path"] = original
	seedActionInstance(t, st, id, "transcode_media", status, 5, "", inputs, state)
	return candidate
}

func retireUnderLease(t *testing.T, e *Engine, id, why string) *store.ActionInstance {
	t.Helper()
	ctx, release, err := e.claimExecution(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	e.retireCandidate(ctx, inst, parseExecutionContext(inst, e), why)
	inst, err = e.deps.Store.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func candidateCleanupOf(t *testing.T, e *Engine, inst *store.ActionInstance) map[string]any {
	t.Helper()
	cleanup, _ := parseExecutionContext(inst, e).State[candidateCleanupKey].(map[string]any)
	return cleanup
}

func TestRejectedCandidateIsRemovedOnceTheRejectionIsDurable(t *testing.T) {
	mediaDir := candidateRoot(t)
	original := filepath.Join(mediaDir, "RejectSource.mkv")
	originalBytes := []byte("synthetic original bytes that a rejection must preserve")
	writeCandidateFile(t, original, string(originalBytes))
	probe := filepath.Join(t.TempDir(), "ffprobe")
	script := `#!/bin/sh
if echo "$*" | grep -q "RejectSource.mkv"; then
cat << 'JSON'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080},{"index":1,"codec_type":"audio","codec_name":"flac","tags":{"language":"jpn"}}],"format":{"format_name":"matroska","duration":"1420.0","size":"800000000"}}
JSON
else
cat << 'JSON'
{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","width":1920,"height":1080}],"format":{"format_name":"matroska","duration":"1420.0","size":"300000000"}}
JSON
fi
`
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var candidate string
	mock := &mockTranscodeExecutor{statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
		candidate = filepath.Join(mediaDir, ".navigatorr-candidates", "RejectSource."+jobID+".mkv")
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			writeCandidateFile(t, candidate, strings.Repeat("candidate-", 30))
		}
		return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted, CandidatePath: candidate}, nil
	}}
	e, st := setupTranscodeEngine(t, mock, probe, []string{mediaDir}, []string{mediaDir}, false)
	res, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": original})
	if err != nil || res.Status != StatusWaitingDecision {
		t.Fatalf("expected a candidate awaiting review: %+v %v", res, err)
	}
	removals := 0
	e.candidateRemoveHook = func(path string) {
		removals++
		inst, _ := st.GetActionInstance(res.ID)
		if inst.Status != StatusFailed || !strings.Contains(inst.ErrorJSON, "rejected by user decision") {
			t.Fatalf("candidate removed before the rejection was persisted: %s %s", inst.Status, inst.ErrorJSON)
		}
	}
	rejected, err := e.Resume(context.Background(), res.ID, "reject", nil)
	if err != nil || rejected.Status != StatusFailed {
		t.Fatalf("reject: %+v %v", rejected, err)
	}
	if removals != 1 {
		t.Fatalf("expected one removal, got %d", removals)
	}
	if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
		t.Fatalf("rejected candidate still present: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(candidate)); !os.IsNotExist(err) {
		t.Fatalf("empty candidate directory kept: %v", err)
	}
	if got, err := os.ReadFile(original); err != nil || !bytes.Equal(got, originalBytes) {
		t.Fatalf("original changed: %v", err)
	}
	if !strings.Contains(rejected.Error, "rejected by user decision") || !strings.HasSuffix(rejected.Error, "; candidate file removed") {
		t.Fatalf("rejection message does not say the candidate was removed: %s", rejected.Error)
	}
	inst, _ := st.GetActionInstance(res.ID)
	ec := parseExecutionContext(inst, e)
	if !getBool(ec.State, candidateRemovedKey) || getInt(ec.State, "candidate_removed_bytes") != 300 || getString(candidateCleanupOf(t, e, inst), "reason") != candidateRetiredRejected {
		t.Fatalf("cleanup not recorded: %v", ec.State[candidateCleanupKey])
	}

	// A second run is harmless: nothing is removed and the record stays.
	again := retireUnderLease(t, e, res.ID, candidateRetiredRejected)
	if removals != 1 || again.ErrorJSON != inst.ErrorJSON {
		t.Fatalf("second cleanup changed something: %d %s", removals, again.ErrorJSON)
	}
	if freed, err := e.removeOwnCandidate(context.Background(), again, parseExecutionContext(again, e)); freed != 0 || err != nil {
		t.Fatalf("missing candidate is not a clean no-op: %d %v", freed, err)
	}
	if _, err := e.Retry(context.Background(), res.ID); err == nil || !strings.Contains(err.Error(), "candidate was removed") {
		t.Fatalf("retry of a removed candidate allowed: %v", err)
	}
}

func TestCancelRemovesTheAttemptsOwnFilesAfterPersisting(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		candidate    bool
		partial      bool
		child        bool
	}{
		{name: "running job partial", status: StatusWaitingExternal, partial: true},
		{name: "waiting decision batch child candidate", status: StatusWaitingDecision, candidate: true, child: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := candidateRoot(t)
			st := setupTestStore(t)
			mock := &mockTranscodeExecutor{}
			e := candidateEngine(t, st, root, mock)
			original := filepath.Join(root, "Sample Show", "Season 1", "sample-episode-01.mkv")
			writeCandidateFile(t, original, "original")
			var inputs map[string]any
			if tc.child {
				seedBatchParent(t, st, "batch", StatusWaitingDecision, map[string]any{})
				st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "one", FilePath: original, Status: "waiting_decision", ChildActionID: "attempt", Decision: "transcode"})
				inputs = map[string]any{"parent_action_id": "batch"}
			}
			candidate := seedCandidateAttempt(t, st, "attempt", tc.status, original, "job-attempt", inputs, nil)
			other := filepath.Join(filepath.Dir(candidate), "sample-episode-02.job-other.mkv")
			writeCandidateFile(t, other, "another attempt")
			if tc.candidate {
				writeCandidateFile(t, candidate, strings.Repeat("c", 40))
			}
			if tc.partial {
				writeCandidateFile(t, candidate+".partial.job-attempt", strings.Repeat("p", 25))
			}
			removed := []string{}
			e.candidateRemoveHook = func(path string) {
				inst, _ := st.GetActionInstance("attempt")
				if inst.Status != StatusCancelled {
					t.Fatalf("%s removed before the cancellation was persisted: %s", filepath.Base(path), inst.Status)
				}
				removed = append(removed, filepath.Base(path))
			}
			res, err := e.Cancel(context.Background(), "attempt", "stopped")
			if err != nil || res.Status != StatusCancelled {
				t.Fatalf("cancel: %+v %v", res, err)
			}
			if mock.cancelCalls != 1 || len(removed) != 1 {
				t.Fatalf("worker cancels %d, removals %v", mock.cancelCalls, removed)
			}
			for _, path := range []string{candidate, candidate + ".partial.job-attempt"} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("%s survived cancellation: %v", filepath.Base(path), err)
				}
			}
			for _, path := range []string{original, other} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("%s touched: %v", filepath.Base(path), err)
				}
			}
			inst, _ := st.GetActionInstance("attempt")
			if inst.WaitingReason != "stopped; candidate file removed" || getString(candidateCleanupOf(t, e, inst), "reason") != candidateRetiredCancelled {
				t.Fatalf("cancel reason does not report the removal: %q", inst.WaitingReason)
			}
		})
	}
}

func TestCancelKeepsTheCandidateWhenTheWorkerDoesNotConfirm(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	mock := &mockTranscodeExecutor{cancelFunc: func(ctx context.Context, jobID string) error { return errors.New("refused") }}
	e := candidateEngine(t, st, root, mock)
	original := filepath.Join(root, "Sample Show", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	candidate := seedCandidateAttempt(t, st, "attempt", StatusWaitingExternal, original, "job-attempt", nil, nil)
	writeCandidateFile(t, candidate+".partial.job-attempt", "still being written")
	e.candidateRemoveHook = func(path string) { t.Fatalf("removed %s while the worker may still write", filepath.Base(path)) }
	if _, err := e.Cancel(context.Background(), "attempt", "stopped"); err == nil || !strings.Contains(err.Error(), "not fully confirmed") {
		t.Fatalf("unconfirmed worker cancellation not reported: %v", err)
	}
	inst, _ := st.GetActionInstance("attempt")
	if inst.Status != StatusCancelled || !strings.Contains(inst.WaitingReason, "candidate file kept: the worker did not confirm the cancellation") {
		t.Fatalf("kept file not reported: %s %q", inst.Status, inst.WaitingReason)
	}
	if _, err := os.Stat(candidate + ".partial.job-attempt"); err != nil {
		t.Fatal(err)
	}
}

func TestBatchRevisionRemovesOnlyTheSupersededCandidate(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	e := candidateEngine(t, st, root, nil)
	season := filepath.Join(root, "Sample Show", "Season 1")
	one, two := filepath.Join(season, "sample-episode-01.mkv"), filepath.Join(season, "sample-episode-02.mkv")
	writeCandidateFile(t, one, "original one")
	writeCandidateFile(t, two, "original two")
	seedActionInstance(t, st, "batch", "transcode_batch", StatusCompleted, 2, "", map[string]any{"paths": []string{one, two}}, nil)
	child := map[string]any{"parent_action_id": "batch"}
	old := seedCandidateAttempt(t, st, "old", StatusCompleted, one, "job-old", child, map[string]any{"original_intact": true})
	reserved := seedCandidateAttempt(t, st, "reserved", StatusCompleted, two, "job-reserved", map[string]any{"parent_action_id": "batch"}, map[string]any{"original_intact": true})
	newer := filepath.Join(season, ".navigatorr-candidates", "sample-episode-01.job-new.mkv")
	for _, path := range []string{old, reserved, newer} {
		writeCandidateFile(t, path, strings.Repeat("x", 50))
	}
	seedActionInstance(t, st, "replacement", "promote_transcode_candidate", StatusWaitingDecision, 1, "promote:filesystem:reserved", map[string]any{"transcode_action_id": "reserved"}, nil)
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "one", FilePath: one, Status: "completed", ChildActionID: "old", CandidatePath: old, Decision: "transcode"})
	st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "two", FilePath: two, Status: "completed", ChildActionID: "reserved", CandidatePath: reserved, Decision: "transcode"})
	root0, _ := st.GetActionInstance("batch")
	items, _ := st.ListTranscodeBatchItems("batch")
	if _, err := e.ReconfigureBatch(context.Background(), "batch", "file", "two", BatchSelectionVersion(root0, items), "", "reserved", map[string]any{"profile": "new"}); err == nil {
		t.Fatal("a file whose replacement started was revised")
	}
	if _, err := e.ReconfigureBatch(context.Background(), "batch", "all", "", BatchSelectionVersion(root0, items), "", "receipt", map[string]any{"profile": "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("superseded candidate kept: %v", err)
	}
	for _, path := range []string{newer, reserved, one, two} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s touched: %v", filepath.Base(path), err)
		}
	}
	oldInst, _ := st.GetActionInstance("old")
	if oldInst.Status != StatusCompleted || getString(candidateCleanupOf(t, e, oldInst), "reason") != candidateRetiredSuperseded || !getBool(parseExecutionContext(oldInst, e).State, candidateRemovedKey) {
		t.Fatalf("superseded cleanup not recorded: %s %v", oldInst.Status, candidateCleanupOf(t, e, oldInst))
	}
	if cleanup := candidateCleanupOf(t, e, mustInstance(t, st, "reserved")); cleanup != nil {
		t.Fatalf("reserved attempt was retired: %v", cleanup)
	}
	batch, _ := st.GetActionInstance("batch")
	history, _ := parseExecutionContext(batch, e).State["batch_attempt_history"].(map[string]any)
	if history["one"] == nil {
		t.Fatal("history row for the replaced attempt was dropped")
	}
}

func TestPromotionPlanRefusesARemovedCandidate(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	resolver, err := fsop.NewResolver([]string{root}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineDeps{Store: st, Config: &config.Config{AllowDestructive: true}, Fs: resolver})
	original := filepath.Join(root, "sample-episode-01.mkv")
	seedCandidateAttempt(t, st, "removed", StatusCompleted, original, "job-removed", nil, map[string]any{candidateRemovedKey: true, candidateCleanupKey: map[string]any{"reason": candidateRetiredSuperseded}})
	res, err := e.stepPromotePlan(context.Background(), &ExecutionContext{Inputs: map[string]any{"transcode_action_id": "removed", "service": "filesystem"}, State: map[string]any{}, Outputs: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepFailed || !strings.Contains(res.Error, "removed after it was superseded") {
		t.Fatalf("removed candidate accepted for promotion: %+v", res)
	}
}

func mustInstance(t *testing.T, st *store.Store, id string) *store.ActionInstance {
	t.Helper()
	inst, err := st.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestCandidateCleanupGuardsKeepAndReport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, st *store.Store, original string) (recorded, onDisk string)
		detail string
	}{
		{name: "symlink", detail: "symlink", setup: func(t *testing.T, st *store.Store, original string) (string, string) {
			target := filepath.Join(filepath.Dir(original), "elsewhere.mkv")
			writeCandidateFile(t, target, "linked")
			link := filepath.Join(filepath.Dir(original), ".navigatorr-candidates", "sample-episode-01.job-guard.mkv")
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link, target
		}},
		{name: "outside candidates", detail: ".navigatorr-candidates", setup: func(t *testing.T, st *store.Store, original string) (string, string) {
			path := filepath.Join(filepath.Dir(original), "sample-episode-01.job-guard.mkv")
			writeCandidateFile(t, path, "beside the original")
			return path, path
		}},
		{name: "another directory's candidates", detail: ".navigatorr-candidates", setup: func(t *testing.T, st *store.Store, original string) (string, string) {
			path := filepath.Join(filepath.Dir(filepath.Dir(original)), ".navigatorr-candidates", "sample-episode-01.job-guard.mkv")
			writeCandidateFile(t, path, "a parent folder's candidate")
			return path, path
		}},
		{name: "no job id", detail: "job id", setup: func(t *testing.T, st *store.Store, original string) (string, string) {
			path := filepath.Join(filepath.Dir(original), ".navigatorr-candidates", "sample-episode-01.mkv")
			writeCandidateFile(t, path, "someone else's file")
			return path, path
		}},
		{name: "original", detail: ".navigatorr-candidates", setup: func(t *testing.T, st *store.Store, original string) (string, string) {
			return original, original
		}},
		{name: "recovery backup", detail: ".navigatorr-candidates", setup: func(t *testing.T, st *store.Store, original string) (string, string) {
			path := promotionRecoveryPath("filesystem", "attempt", filepath.Join(filepath.Dir(original), ".navigatorr-candidates", "sample-episode-01.job-guard.mkv"))
			writeCandidateFile(t, path, "recovery copy of the original")
			return path, path
		}},
		{name: "recovery directory named like the candidate", detail: ".navigatorr-candidates", setup: func(t *testing.T, st *store.Store, original string) (string, string) {
			path := filepath.Join(filepath.Dir(original), ".navigatorr-candidates", ".promotion-recovery", "sample-episode-01.job-guard.mkv")
			writeCandidateFile(t, path, "recovery entry")
			return path, path
		}},
		{name: "promotion started", detail: "references this candidate", setup: func(t *testing.T, st *store.Store, original string) (string, string) {
			path := filepath.Join(filepath.Dir(original), ".navigatorr-candidates", "sample-episode-01.job-guard.mkv")
			writeCandidateFile(t, path, "being promoted")
			seedActionInstance(t, st, "replacement", "promote_transcode_candidate", StatusRunning, 3, "promote:custom-service:attempt", map[string]any{"transcode_action_id": "attempt"}, nil)
			return path, path
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := candidateRoot(t)
			st := setupTestStore(t)
			e := candidateEngine(t, st, root, nil)
			original := filepath.Join(root, "Sample Show", "Season 1", "sample-episode-01.mkv")
			writeCandidateFile(t, original, "original")
			recorded, onDisk := tc.setup(t, st, original)
			seedCandidateAttempt(t, st, "attempt", StatusFailed, original, "job-guard", nil, nil)
			inst := mustInstance(t, st, "attempt")
			ec := parseExecutionContext(inst, e)
			ec.State["candidate_path"] = recorded
			inst.StateJSON = toJSON(ec.State)
			inst.ErrorJSON = toJSON(map[string]any{"step": "validate", "error": candidateRejectedError})
			if err := st.UpdateActionInstance(*inst); err != nil {
				t.Fatal(err)
			}
			e.candidateRemoveHook = func(path string) { t.Fatalf("guarded path removed: %s", filepath.Base(path)) }
			inst = retireUnderLease(t, e, "attempt", candidateRetiredRejected)
			cleanup := candidateCleanupOf(t, e, inst)
			if getString(cleanup, "outcome") != "kept" || !strings.Contains(getString(cleanup, "detail"), tc.detail) || getBool(parseExecutionContext(inst, e).State, candidateRemovedKey) {
				t.Fatalf("guard did not keep and report: %v", cleanup)
			}
			if !strings.Contains(inst.ErrorJSON, "candidate file kept: ") {
				t.Fatalf("kept file not reported in the message: %s", inst.ErrorJSON)
			}
			for _, path := range []string{onDisk, original} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("%s touched: %v", filepath.Base(path), err)
				}
			}
			page, err := e.ListLeftoverCandidates(context.Background(), 0)
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("guarded path listed as removable: %+v %v", page, err)
			}
		})
	}
}

func TestLeftoverCandidateListingIsReadOnlyAndOwnershipBased(t *testing.T) {
	root := candidateRoot(t)
	st := setupTestStore(t)
	e := candidateEngine(t, st, root, nil)
	season := filepath.Join(root, "Sample Show", "Season 1")
	original := func(n string) string {
		path := filepath.Join(season, "sample-episode-"+n+".mkv")
		writeCandidateFile(t, path, "original "+n)
		return path
	}
	var files []string
	keep := func(path, content string) {
		writeCandidateFile(t, path, content)
		files = append(files, path)
	}
	rejected := seedCandidateAttempt(t, st, "rejected", StatusFailed, original("01"), "job-rejected", nil, nil)
	keep(rejected, strings.Repeat("r", 100))
	st.UpdateActionInstance(func() store.ActionInstance {
		inst := mustInstance(t, st, "rejected")
		inst.ErrorJSON = toJSON(map[string]any{"step": "validate", "error": candidateRejectedError})
		return *inst
	}())
	cancelled := seedCandidateAttempt(t, st, "cancelled", StatusCancelled, original("02"), "job-cancelled", nil, nil)
	keep(cancelled+".partial.job-cancelled", strings.Repeat("p", 30))
	seedActionInstance(t, st, "batch", "transcode_batch", StatusCompleted, 2, "", map[string]any{}, map[string]any{"batch_attempt_history": map[string]any{"three": []any{map[string]any{"item_key": "three", "child_action_id": "superseded"}}, "four": []any{map[string]any{"item_key": "four", "child_action_id": "superseded-promoted"}}}})
	superseded := seedCandidateAttempt(t, st, "superseded", StatusCompleted, original("03"), "job-superseded", map[string]any{"parent_action_id": "batch"}, nil)
	keep(superseded, strings.Repeat("s", 70))
	supersededPromoted := seedCandidateAttempt(t, st, "superseded-promoted", StatusCompleted, original("04"), "job-superseded-promoted", map[string]any{"parent_action_id": "batch"}, nil)
	keep(supersededPromoted, "a replacement references this")
	seedActionInstance(t, st, "promotion", "promote_transcode_candidate", StatusFailed, 4, "promote:sonarr:superseded-promoted", map[string]any{"transcode_action_id": "superseded-promoted"}, nil)
	ready := seedCandidateAttempt(t, st, "ready", StatusCompleted, original("05"), "job-ready", nil, nil)
	keep(ready, "awaiting replacement")
	live := seedCandidateAttempt(t, st, "live", StatusWaitingExternal, original("06"), "job-live", nil, nil)
	keep(live+".partial.job-live", "being written")
	recovery := filepath.Join(season, ".navigatorr-candidates", ".promotion-recovery", "abc", "original.bak")
	keep(recovery, "recovery copy")
	seedCandidateAttempt(t, st, "recovery", StatusFailed, original("07"), "job-recovery", nil, nil)
	inst := mustInstance(t, st, "recovery")
	ec := parseExecutionContext(inst, e)
	ec.State["candidate_path"] = recovery
	inst.StateJSON = toJSON(ec.State)
	st.UpdateActionInstance(*inst)
	seedCandidateAttempt(t, st, "already-removed", StatusFailed, original("08"), "job-already-removed", nil, map[string]any{candidateRemovedKey: true})

	page, err := e.ListLeftoverCandidates(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]LeftoverCandidate{}
	for _, item := range page.Items {
		got[item.ActionID] = item
	}
	if len(got) != 3 || page.Bytes != 200 || page.NextOffset != nil {
		t.Fatalf("unexpected listing: %+v", page)
	}
	if item := got["rejected"]; item.Path != rejected || item.Bytes != 100 || item.Status != StatusFailed || !strings.Contains(item.Reason, "rejected") {
		t.Fatalf("rejected leftover: %+v", item)
	}
	if item := got["cancelled"]; item.Bytes != 0 || item.PartialBytes != 30 || !strings.Contains(item.Reason, "cancelled") {
		t.Fatalf("cancelled partial: %+v", item)
	}
	if item := got["superseded"]; item.Bytes != 70 || item.Status != StatusCompleted || !strings.Contains(item.Reason, "revision replaced") {
		t.Fatalf("superseded leftover: %+v", item)
	}
	for _, path := range files {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("listing touched %s: %v", filepath.Base(path), err)
		}
	}
}
