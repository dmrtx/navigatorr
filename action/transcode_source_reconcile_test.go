package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// queuedFile is a job that waited in the queue for a file, and what became of
// that file meanwhile.
type queuedFile struct {
	e   *Engine
	st  *store.Store
	dir string
}

func newQueuedFile(t *testing.T) *queuedFile {
	t.Helper()
	// The preflight works on the resolved path, as the promotions it compares
	// with do; on macOS the temp dir is behind a symlink.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, writeProbeScript(t, dir, sdrProbeJSON), []string{dir}, []string{dir}, false)
	t.Cleanup(func() { st.Close() })
	return &queuedFile{e: e, st: st, dir: dir}
}

func (q *queuedFile) write(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(q.dir, name)
	if err := os.WriteFile(p, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// queue admits a conversion of path; the stores stamp creation time, so a
// promotion seeded afterwards finished after the job was queued.
func (q *queuedFile) queue(t *testing.T, id, path string, extra map[string]any) *ExecutionContext {
	t.Helper()
	inputs := map[string]any{"path": path, "mode": "size"}
	for k, v := range extra {
		inputs[k] = v
	}
	ec := &ExecutionContext{InstanceID: id, ActionName: "transcode_media", Inputs: inputs, State: map[string]any{}, Outputs: map[string]any{}}
	seedActionInstance(t, q.st, id, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	time.Sleep(3 * time.Millisecond)
	return ec
}

// promoted records a completed promotion that replaced original with newPath.
func (q *queuedFile) promoted(t *testing.T, id, original, newPath string) {
	t.Helper()
	seedActionInstance(t, q.st, id, "promote_transcode_candidate", StatusCompleted, 0, "", map[string]any{}, map[string]any{
		"promotion": map[string]any{"transcode_action_id": "earlier-conversion", "original_path": original, "new_path": newPath},
	})
}

func (q *queuedFile) preflight(t *testing.T, ec *ExecutionContext) StepResult {
	t.Helper()
	res, err := q.e.stepTranscodePreflight(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The file was converted and replaced by an earlier job while this one waited,
// and the replacement has another extension. That is work already done.
func TestQueuedJobWhoseFileWasReplacedIsSkippedNotFailed(t *testing.T) {
	q := newQueuedFile(t)
	original := filepath.Join(q.dir, "Show S01E01.mp4")
	ec := q.queue(t, "queued-1", original, nil)
	replacement := q.write(t, "Show S01E01.mkv")
	q.promoted(t, "promotion-1", original, replacement)

	res := q.preflight(t, ec)
	if res.Status != StepCompleted || res.Outputs["reason_code"] != reasonSourceReplaced {
		t.Fatalf("a replaced file must end the job as done: %+v", res)
	}
	if !getBool(ec.State, "skip_transcode") || getString(ec.State, supersededByKey) != "promotion-1" || getString(ec.State, "resolved_path") != replacement {
		t.Fatalf("the job did not record what replaced its file: %v", ec.State)
	}
}

func TestQueuedJobWithAMissingFileAndNoReplacementFailsAsMissing(t *testing.T) {
	q := newQueuedFile(t)
	ec := q.queue(t, "queued-2", filepath.Join(q.dir, "gone.mkv"), nil)
	res := q.preflight(t, ec)
	if res.Status != StepFailed || !strings.Contains(res.Error, "no longer exists") {
		t.Fatalf("a deleted file is a failure that says so: %+v", res)
	}
	if getString(ec.State, "failure_classification") != string(resilience.SourceUnreachable) {
		t.Fatalf("classification = %v", ec.State["failure_classification"])
	}
}

// A promotion that finished before the job existed does not explain a missing
// file: the job was admitted against what the promotion left.
func TestPromotionOlderThanTheJobDoesNotSupersedeIt(t *testing.T) {
	q := newQueuedFile(t)
	original := filepath.Join(q.dir, "Older.mp4")
	replacement := q.write(t, "Older.mkv")
	q.promoted(t, "promotion-old", original, replacement)
	time.Sleep(3 * time.Millisecond)
	ec := q.queue(t, "queued-3", original, nil)
	if res := q.preflight(t, ec); res.Status != StepFailed {
		t.Fatalf("an older promotion superseded a newer job: %+v", res)
	}
}

// The history names a replacement that is not on disk: nothing proves the work
// is done, so the job reports the missing file instead of skipping.
func TestReplacementThatIsNotOnDiskDoesNotSkip(t *testing.T) {
	q := newQueuedFile(t)
	original := filepath.Join(q.dir, "Vanished.mp4")
	ec := q.queue(t, "queued-4", original, nil)
	q.promoted(t, "promotion-4", original, filepath.Join(q.dir, "Vanished.mkv"))
	if res := q.preflight(t, ec); res.Status != StepFailed {
		t.Fatalf("skipped on a replacement that is not there: %+v", res)
	}
}

// Stream picks frozen for the file as it was queued name streams the file no
// longer has: a changed source, with the picks never applied to the new file.
func TestStalePicksAreReportedAsChangedSource(t *testing.T) {
	q := newQueuedFile(t)
	file := q.write(t, "Picked.mkv")
	ec := q.queue(t, "queued-5", file, map[string]any{"keep_audio_streams": []any{float64(4)}})
	res := q.preflight(t, ec)
	if res.Status != StepFailed || !strings.Contains(res.Error, "choosing the tracks to keep") || !strings.Contains(res.Error, "not applied to the new file") {
		t.Fatalf("stale picks: %+v", res)
	}
	if getString(ec.State, "failure_classification") != string(resilience.SourceChanged) {
		t.Fatalf("classification = %v", ec.State["failure_classification"])
	}
}

// The same job, but another job already replaced the file after this one was
// queued: its picks belong to the previous file, whether or not their indexes
// still happen to exist, and the job is done.
func TestStalePicksOfAReplacedFileEndTheJobAsDone(t *testing.T) {
	q := newQueuedFile(t)
	file := q.write(t, "Replaced.mkv")
	ec := q.queue(t, "queued-6", file, map[string]any{"keep_audio_streams": []any{float64(4)}})
	q.promoted(t, "promotion-6", file, file)
	res := q.preflight(t, ec)
	if res.Status != StepCompleted || res.Outputs["reason_code"] != reasonSourceReplaced {
		t.Fatalf("picks of a replaced file: %+v", res)
	}
}

func TestSameLibraryFile(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"/m/s/E1.mp4", "/m/s/E1.mkv", true},
		{"/m/s/E1.MKV", "/m/s/E1.mkv", true},
		{"/m/s/E1.mkv", "/m/s/E2.mkv", false},
		{"/m/s/E1.mkv", "/m/t/E1.mkv", false},
	} {
		if got := sameLibraryFile(c.a, c.b); got != c.want {
			t.Errorf("sameLibraryFile(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
