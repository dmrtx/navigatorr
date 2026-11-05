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
	q.promotedWithBytes(t, id, original, newPath, 0)
}

// promotedWithBytes records the size the promotion published, as a real one does.
func (q *queuedFile) promotedWithBytes(t *testing.T, id, original, newPath string, bytes int64) {
	t.Helper()
	seedActionInstance(t, q.st, id, "promote_transcode_candidate", StatusCompleted, 0, "", map[string]any{}, map[string]any{
		"promotion": map[string]any{"transcode_action_id": "earlier-conversion", "original_path": original, "new_path": newPath, "candidate_bytes": bytes},
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
	// Updates are stored to the second, so "older" means more than a second.
	time.Sleep(1100 * time.Millisecond)
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

// Another file with the same name and another extension is not the file that
// was replaced: E01.mp4 being gone says nothing about E01.mkv's promotion.
func TestPromotionOfASiblingExtensionDoesNotExplainAMissingFile(t *testing.T) {
	q := newQueuedFile(t)
	ec := q.queue(t, "queued-7", filepath.Join(q.dir, "Sibling.mp4"), nil)
	kept := q.write(t, "Sibling.mkv")
	q.promoted(t, "promotion-7", kept, kept)
	if res := q.preflight(t, ec); res.Status != StepFailed {
		t.Fatalf("a sibling's promotion explained a deleted file: %+v", res)
	}
}

// The file is still there under its name, but a promotion replaced it after the
// job was queued and the job has no manual picks: converting it again would
// re-encode the result.
func TestQueuedJobWhoseFileWasReplacedInPlaceIsSkipped(t *testing.T) {
	q := newQueuedFile(t)
	file := q.write(t, "InPlace.mkv")
	ec := q.queue(t, "queued-8", file, nil)
	q.promoted(t, "promotion-8", file, file)
	res := q.preflight(t, ec)
	if res.Status != StepCompleted || res.Outputs["reason_code"] != reasonSourceReplaced {
		t.Fatalf("a file replaced in place must end the job as done: %+v", res)
	}
}

// The published file is not the one the promotion recorded: it was changed or
// substituted afterwards, so nothing proves the work is what is in place.
func TestReplacementWithAnotherSizeDoesNotSkip(t *testing.T) {
	q := newQueuedFile(t)
	original := filepath.Join(q.dir, "Swapped.mp4")
	ec := q.queue(t, "queued-9", original, nil)
	replacement := q.write(t, "Swapped.mkv")
	q.promotedWithBytes(t, "promotion-9", original, replacement, 4242)
	if res := q.preflight(t, ec); res.Status != StepFailed {
		t.Fatalf("skipped on a file that is not what was published: %+v", res)
	}
}

// The shares ignore letter case, so a promotion recorded as "Show S01E01.mp4"
// explains a job queued for "show s01e01.MP4".
func TestReplacementIsFoundWhateverTheLetterCase(t *testing.T) {
	q := newQueuedFile(t)
	ec := q.queue(t, "queued-10", filepath.Join(q.dir, "show s01e01.MP4"), nil)
	replacement := q.write(t, "Show S01E01.mkv")
	q.promoted(t, "promotion-10", filepath.Join(q.dir, "Show S01E01.mp4"), replacement)
	res := q.preflight(t, ec)
	if res.Status != StepCompleted || res.Outputs["reason_code"] != reasonSourceReplaced {
		t.Fatalf("case difference hid the replacement: %+v", res)
	}
}

// A job with manual picks whose file was modified after it was queued, with no
// promotion to explain it, must not apply the picks to what may be other tracks.
func TestPicksAreRefusedForAFileModifiedAfterQueueing(t *testing.T) {
	q := newQueuedFile(t)
	file := q.write(t, "Touched.mkv")
	ec := q.queue(t, "queued-11", file, map[string]any{"keep_audio_streams": []any{float64(1)}})
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	res := q.preflight(t, ec)
	if res.Status != StepFailed || !strings.Contains(res.Error, "not applied to the new file") {
		t.Fatalf("picks applied to a modified file: %+v", res)
	}
	if getString(ec.State, "failure_classification") != string(resilience.SourceChanged) {
		t.Fatalf("classification = %v", ec.State["failure_classification"])
	}
}
