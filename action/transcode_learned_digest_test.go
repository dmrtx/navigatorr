package action

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// learnsDigestWorker advertises LearnSourceDigest on top of the mock's default
// capabilities.
func learnsDigestWorker() *mockTranscodeExecutor {
	m := &mockTranscodeExecutor{}
	base := (&mockTranscodeExecutor{}).Capabilities
	m.capabilitiesFunc = func(ctx context.Context) (transcode.WorkerCapabilities, error) {
		caps, err := base(ctx)
		caps.LearnSourceDigest = true
		return caps, err
	}
	return m
}

func learnedPreflight(t *testing.T, worker *mockTranscodeExecutor, content []byte, state map[string]any) (*Engine, *ExecutionContext, StepResult, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, worker, writeProbeScript(t, dir, sdrProbeJSON), []string{dir}, []string{dir}, false)
	t.Cleanup(func() { st.Close() })
	if state == nil {
		state = map[string]any{}
	}
	ec := &ExecutionContext{InstanceID: "learn-" + t.Name(), ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "size"}, State: state, Outputs: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	// A hash slot can never be taken, so a coordinator hash would end the step
	// in a failure at the deadline instead of passing silently.
	for i := 0; i < mediaHashConcurrency; i++ {
		e.hashSlots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := e.stepTranscodePreflight(ctx, ec)
	if err != nil {
		t.Fatal(err)
	}
	return e, ec, res, file
}

func TestPreflightForALearningWorkerRecordsStatAndReadsNothing(t *testing.T) {
	content := []byte("source bytes the coordinator never reads")
	_, ec, res, file := learnedPreflight(t, learnsDigestWorker(), content, nil)
	if res.Status != StepCompleted || getBool(ec.State, "skip_transcode") {
		t.Fatalf("preflight %+v", res)
	}
	if _, ok := ec.State["original_sha256"]; ok {
		t.Fatalf("preflight recorded a digest it did not compute: %+v", ec.State)
	}
	if _, ok := res.Outputs["original_sha256"]; ok {
		t.Fatal("preflight reported a digest")
	}
	if orig, _ := ec.State["original"].(map[string]any); orig == nil || orig["sha256"] != nil {
		t.Fatalf("original summary carries a digest: %+v", orig)
	}
	if _, hashed := phaseCostMap(ec)["coordinator_source_hash"]; hashed {
		t.Fatal("a source hash phase was recorded")
	}
	fi, _ := os.Stat(file)
	size, mod, ok := recordedOriginalStat(ec.State)
	if !ok || size != fi.Size() || !mod.Equal(fi.ModTime()) {
		t.Fatalf("recorded stat %d %v %v, want %d %v", size, mod, ok, fi.Size(), fi.ModTime())
	}
	if !hasOriginalBaseline(ec.State) {
		t.Fatal("a recorded stat is a baseline for submit")
	}
}

func TestPreflightForAnOldWorkerKeepsTheCoordinatorHash(t *testing.T) {
	content := []byte("source bytes hashed by the coordinator")
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, writeProbeScript(t, dir, sdrProbeJSON), []string{dir}, []string{dir}, false)
	defer st.Close()
	// State left by a run against a learning worker: stat and a pinned digest.
	stale := map[string]any{"original_sha256": "pinned-by-an-earlier-run"}
	fi, _ := os.Stat(file)
	recordOriginalStat(stale, fi)
	ec := &ExecutionContext{InstanceID: "old-worker", ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "size"}, State: stale, Outputs: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	res, err := e.stepTranscodePreflight(context.Background(), ec)
	if err != nil || res.Status != StepCompleted {
		t.Fatalf("preflight %+v %v", res, err)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256(content)); getString(ec.State, "original_sha256") != want {
		t.Fatalf("hash %q, want %q", getString(ec.State, "original_sha256"), want)
	}
	if _, ok := ec.State[originalStatKey]; ok {
		t.Fatal("a coordinator-hashed action kept a stat, which would turn its accept into a metadata check")
	}
}

func TestPreflightRerunKeepsAPinOnlyForTheSameFileVersion(t *testing.T) {
	content := []byte("source bytes for a rerun")
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, learnsDigestWorker(), writeProbeScript(t, dir, sdrProbeJSON), []string{dir}, []string{dir}, false)
	defer st.Close()
	fi, _ := os.Stat(file)
	run := func(id string, mutate func(map[string]any)) *ExecutionContext {
		state := map[string]any{"original_sha256": strings.Repeat("ab", 32)}
		recordOriginalStat(state, fi)
		mutate(state)
		ec := &ExecutionContext{InstanceID: id, ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "size"}, State: state, Outputs: map[string]any{}}
		seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
		if res, err := e.stepTranscodePreflight(context.Background(), ec); err != nil || res.Status != StepCompleted {
			t.Fatalf("preflight %+v %v", res, err)
		}
		return ec
	}
	if ec := run("same-version", func(map[string]any) {}); getString(ec.State, "original_sha256") != strings.Repeat("ab", 32) {
		t.Fatalf("the pin of an unchanged file was dropped: %+v", ec.State)
	}
	changed := run("other-version", func(s map[string]any) {
		s[originalStatKey] = map[string]any{"size_bytes": fi.Size() + 1, "mod_time": fi.ModTime().UTC().Format(time.RFC3339Nano)}
	})
	if _, ok := changed.State["original_sha256"]; ok {
		t.Fatal("a pin for a different file version must not be kept")
	}
	if size, _, _ := recordedOriginalStat(changed.State); size != fi.Size() {
		t.Fatalf("stat not refreshed: %d", size)
	}
}

// Under drain a learning worker's preflight reads nothing, takes no worker
// work, and the file is still held at submit.
func TestDrainStillHoldsALearningFileBeforeItSubmits(t *testing.T) {
	worker := learnsDigestWorker()
	e, ec, closeStore, _ := drainPreflightFixtureWith(t, "drain-learn", worker)
	defer closeStore()
	setDrain(t, e, true)
	for i := 0; i < mediaHashConcurrency; i++ {
		e.hashSlots <- struct{}{}
	}
	res, err := e.stepTranscodePreflight(context.Background(), ec)
	if err != nil || res.Status != StepCompleted {
		t.Fatalf("preflight reads nothing, so it has nothing to hold: %+v %v", res, err)
	}
	if _, ok := ec.State["original_sha256"]; ok {
		t.Fatal("a source hash was taken")
	}
	// Pretend the plan steps ran; submit is the choke point.
	ec.State["candidate_extension"] = ".mkv"
	ec.State["profile"] = "hevc-vt"
	ec.State["plan"] = testReconcilePlan()
	sub, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil || sub.Status != StepWaitingExternal || sub.WaitingCondition != drainWaitCondition || worker.submitCalls != 0 {
		t.Fatalf("a held file must take no worker work: %+v %v submits=%d", sub, err, worker.submitCalls)
	}
}

func drainPreflightFixtureWith(t *testing.T, id string, worker *mockTranscodeExecutor) (*Engine, *ExecutionContext, func(), []byte) {
	t.Helper()
	content := []byte("source bytes that would be encoded")
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, worker, writeProbeScript(t, dir, sdrProbeJSON), []string{dir}, []string{dir}, false)
	ec := &ExecutionContext{InstanceID: id, ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "size"}, State: map[string]any{}, Outputs: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	return e, ec, func() { st.Close() }, content
}

func learnedSubmitState(size int64) map[string]any {
	state := submitStepState()
	delete(state, "original_sha256")
	state[originalStatKey] = map[string]any{"size_bytes": size, "mod_time": time.Unix(1700000000, 123456789).UTC().Format(time.RFC3339Nano)}
	return state
}

func TestSubmitSendsNoDigestAndTheProbedSizeUntilOneIsPinned(t *testing.T) {
	var got []transcode.Request
	worker := learnsDigestWorker()
	worker.submitFunc = func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
		got = append(got, req)
		return transcode.Job{ID: req.ID}, nil
	}
	st := setupTestStore(t)
	seedActionInstance(t, st, "learn-submit", "transcode_media", StatusRunning, 3, "", map[string]any{"path": "/Volumes/media/ep.mkv"}, learnedSubmitState(4096))
	e := NewEngine(EngineDeps{Store: st, Transcode: worker})

	ec := childECFor("learn-submit", "", learnedSubmitState(4096))
	delete(ec.State, "original_sha256")
	if res, err := e.stepTranscodeSubmit(context.Background(), ec); err != nil || res.Status != StepCompleted {
		t.Fatalf("submit %+v %v", res, err)
	}
	if len(got) != 1 || got[0].SourceSHA256 != "" || got[0].SourceSizeBytes != 4096 {
		t.Fatalf("first submit = %+v", got)
	}

	// Once pinned, a later rung sends the digest and no size.
	pinned := childECFor("learn-submit", "", learnedSubmitState(4096))
	pinned.State["original_sha256"] = strings.Repeat("cd", 32)
	pinned.State["job_id"] = ""
	if res, err := e.stepTranscodeSubmit(context.Background(), pinned); err != nil || res.Status != StepCompleted {
		t.Fatalf("pinned submit %+v %v", res, err)
	}
	if len(got) != 2 || got[1].SourceSHA256 != strings.Repeat("cd", 32) || got[1].SourceSizeBytes != 0 {
		t.Fatalf("pinned submit = %+v", got[len(got)-1])
	}
}

func TestSubmitWithoutADigestFailsClosedOnAWorkerThatCannotLearnIt(t *testing.T) {
	worker := &mockTranscodeExecutor{}
	st := setupTestStore(t)
	seedActionInstance(t, st, "learn-old", "transcode_media", StatusRunning, 3, "", map[string]any{"path": "/Volumes/media/ep.mkv"}, learnedSubmitState(1))
	e := NewEngine(EngineDeps{Store: st, Transcode: worker})
	ec := childECFor("learn-old", "", learnedSubmitState(1))
	delete(ec.State, "original_sha256")
	res, err := e.stepTranscodeSubmit(context.Background(), ec)
	if err != nil || res.Status != StepFailed || !strings.Contains(res.Error, "upgrade the worker") || worker.submitCalls != 0 {
		t.Fatalf("%+v %v submits=%d", res, err, worker.submitCalls)
	}
}

func TestFirstStatusPinsTheLearnedDigestAndItNeverChanges(t *testing.T) {
	e := NewEngine(EngineDeps{})
	ec := testReconcileEC("pin")
	delete(ec.State, "original_sha256")
	ec.State["original"] = map[string]any{"path": "/Volumes/media/ep.mkv"}
	recordOriginalStat(ec.State, fakeFileInfo{size: 10})
	sha := strings.Repeat("ab", 32)

	e.observeTranscodeStatus(ec, transcode.JobStatus{ID: "job-1", Status: transcode.StatusRunning})
	if _, ok := ec.State["original_sha256"]; ok {
		t.Fatal("a status without a digest pinned something")
	}
	e.observeTranscodeStatus(ec, transcode.JobStatus{ID: "job-1", Status: transcode.StatusRunning, SourceSHA256: "sha256:" + strings.ToUpper(sha)})
	if getString(ec.State, "original_sha256") != sha || getString(ec.Outputs, "original_sha256") != sha {
		t.Fatalf("not pinned: %+v %+v", ec.State, ec.Outputs)
	}
	if orig, _ := ec.State["original"].(map[string]any); orig["sha256"] != sha {
		t.Fatalf("original summary not pinned: %+v", orig)
	}
	e.observeTranscodeStatus(ec, transcode.JobStatus{ID: "job-2", Status: transcode.StatusRunning, SourceSHA256: sha})
	if _, conflict := originalDigestConflict(ec); conflict {
		t.Fatal("the same digest again is not a conflict")
	}
	e.observeTranscodeStatus(ec, transcode.JobStatus{ID: "job-3", Status: transcode.StatusRunning, SourceSHA256: strings.Repeat("cd", 32)})
	if getString(ec.State, "original_sha256") != sha {
		t.Fatal("a pinned digest changed")
	}
	res, conflict := originalDigestConflict(ec)
	if !conflict || res.Status != StepFailed || getString(ec.State, "failure_classification") != string(resilience.SourceChanged) {
		t.Fatalf("a different digest must be an integrity failure: %+v", res)
	}
	// Accept refuses too, whatever the metadata says.
	if r, err := e.stepTranscodeAccept(context.Background(), ec); err != nil || r.Status != StepFailed {
		t.Fatalf("accept %+v %v", r, err)
	}
}

func TestWaitStepFailsWhenAStatusContradictsThePinnedDigest(t *testing.T) {
	worker := &mockTranscodeExecutor{statusFunc: func(ctx context.Context, id string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: id, Status: transcode.StatusRunning, SourceSHA256: strings.Repeat("cd", 32)}, nil
	}}
	st := setupTestStore(t)
	seedActionInstance(t, st, "wait-conflict", "transcode_media", StatusRunning, 4, "", map[string]any{"path": "/Volumes/media/ep.mkv"}, learnedSubmitState(1))
	e := NewEngine(EngineDeps{Store: st, Transcode: worker})
	ec := childECFor("wait-conflict", "", learnedSubmitState(1))
	ec.State["original_sha256"] = strings.Repeat("ab", 32)
	ec.State["job_id"] = "job-1"
	res, err := e.stepTranscodeWait(context.Background(), ec)
	if err != nil || res.Status != StepFailed || !strings.Contains(res.Error, "integrity violation") {
		t.Fatalf("%+v %v", res, err)
	}
}

type fakeFileInfo struct {
	size int64
	mod  time.Time
}

func (f fakeFileInfo) Name() string       { return "f" }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() os.FileMode  { return 0600 }
func (f fakeFileInfo) ModTime() time.Time { return f.mod }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func acceptByMetadataFixture(t *testing.T, content []byte, withDigest bool) (*Engine, *ExecutionContext, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "original.mkv")
	if err := os.WriteFile(p, content, 0600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	ec := testReconcileEC("")
	ec.State["resolved_path"] = p
	ec.State["job_id"], ec.State["candidate_path"] = "job-1", "/cand.mkv"
	delete(ec.State, "original_sha256")
	if withDigest {
		ec.State["original_sha256"] = fmt.Sprintf("%x", sha256.Sum256(content))
	}
	recordOriginalStat(ec.State, fi)
	return NewEngine(EngineDeps{}), ec, p
}

func TestAcceptChecksMetadataInsteadOfRereadingTheOriginal(t *testing.T) {
	content := []byte("original bytes read once, by the worker")
	e, ec, p := acceptByMetadataFixture(t, content, true)
	// A same-size, same-mtime content change is exactly what the cheaper check
	// cannot see; the full re-hash path would. This pins that the new flow does
	// not read the file.
	fi, _ := os.Stat(p)
	if err := os.WriteFile(p, []byte(strings.Repeat("x", len(content))), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	r, err := e.stepTranscodeAccept(context.Background(), ec)
	if err != nil || r.Status != StepCompleted {
		t.Fatalf("accept %+v %v", r, err)
	}
	if r.Outputs["original_integrity"] != "verified" || r.Outputs["original_integrity_check"] != "metadata" || ec.State["original_integrity_check"] != "metadata" {
		t.Fatalf("the result must say the check was by metadata: %+v", r.Outputs)
	}
	validation, _ := ec.State["validation"].(map[string]any)
	if validation["original_sha256"] != "pinned_at_staging" || validation["original_integrity_check"] != "metadata" {
		t.Fatalf("validation = %+v", validation)
	}
	if msg := getString(r.Outputs, "message"); strings.Contains(msg, "SHA-256 unchanged") {
		t.Fatalf("the message still claims a hash comparison: %q", msg)
	}
	if _, hashed := ec.State["work_progress"]; hashed {
		t.Fatal("accept left hash progress behind")
	}
}

func TestAcceptByMetadataFailsClosedWhenTheOriginalChanged(t *testing.T) {
	content := []byte("original bytes")
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, p string, fi os.FileInfo)
	}{
		{"size", func(t *testing.T, p string, fi os.FileInfo) {
			if err := os.WriteFile(p, append(content, '!'), 0600); err != nil {
				t.Fatal(err)
			}
			_ = os.Chtimes(p, fi.ModTime(), fi.ModTime())
		}},
		{"mtime", func(t *testing.T, p string, fi os.FileInfo) {
			if err := os.Chtimes(p, fi.ModTime().Add(time.Second), fi.ModTime().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
		}},
		{"removed", func(t *testing.T, p string, fi os.FileInfo) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, ec, p := acceptByMetadataFixture(t, content, true)
			fi, _ := os.Stat(p)
			tc.change(t, p, fi)
			r, err := e.stepTranscodeAccept(context.Background(), ec)
			if err != nil || r.Status != StepFailed || !strings.Contains(r.Error, "integrity violation") {
				t.Fatalf("%+v %v", r, err)
			}
			if tc.name != "removed" && getString(ec.State, "failure_classification") != string(resilience.SourceChanged) {
				t.Fatalf("classification %q", getString(ec.State, "failure_classification"))
			}
			if ec.State["original_integrity"] == "verified" {
				t.Fatal("a failed check recorded a verification")
			}
		})
	}
}

func TestAcceptByMetadataNeedsALearnedDigestUnlessItSkipped(t *testing.T) {
	e, ec, _ := acceptByMetadataFixture(t, []byte("original"), false)
	if r, err := e.stepTranscodeAccept(context.Background(), ec); err != nil || r.Status != StepFailed || !strings.Contains(r.Error, "never reported") {
		t.Fatalf("an encode accepted without a digest for promotion: %+v %v", r, err)
	}
	e, ec, _ = acceptByMetadataFixture(t, []byte("original"), false)
	ec.State["skip_transcode"] = true
	if r, err := e.stepTranscodeAccept(context.Background(), ec); err != nil || r.Status != StepCompleted || r.Outputs["original_integrity"] != "verified" {
		t.Fatalf("skip after a worker job: %+v %v", r, err)
	}
}

// An action preflighted with a coordinator hash carries no stat, so it keeps
// the full re-read: a changed file of the same size and mtime is still caught.
func TestLegacyActionKeepsTheFullRehashAtAccept(t *testing.T) {
	content := []byte("original bytes hashed by the coordinator")
	p := filepath.Join(t.TempDir(), "original.mkv")
	if err := os.WriteFile(p, content, 0600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	e := NewEngine(EngineDeps{})
	ec := testReconcileEC("")
	ec.State["resolved_path"] = p
	ec.State["job_id"] = "job-1"
	ec.State["original_sha256"] = fmt.Sprintf("%x", sha256.Sum256(content))
	if err := os.WriteFile(p, []byte(strings.Repeat("x", len(content))), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(p, fi.ModTime(), fi.ModTime())
	if r, err := e.stepTranscodeAccept(context.Background(), ec); err != nil || r.Status != StepFailed || getString(ec.State, "failure_classification") != string(resilience.SourceChanged) {
		t.Fatalf("the re-hash must catch a content change metadata cannot see: %+v %v", r, err)
	}
	// And a clean one passes, labelled as a hash.
	if err := os.WriteFile(p, content, 0600); err != nil {
		t.Fatal(err)
	}
	ec = testReconcileEC("")
	ec.State["resolved_path"], ec.State["job_id"] = p, "job-1"
	ec.State["original_sha256"] = fmt.Sprintf("%x", sha256.Sum256(content))
	r, err := e.stepTranscodeAccept(context.Background(), ec)
	if err != nil || r.Status != StepCompleted || r.Outputs["original_integrity_check"] != "sha256" {
		t.Fatalf("%+v %v", r, err)
	}
}
