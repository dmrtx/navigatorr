package action

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jakenesler/navigatorr/transcode"
)

// An original with a long name must still give a candidate, and a partial,
// that the NAS accepts: both are created beside the original.
func TestSubmitNamesALongOriginalsCandidateToFit(t *testing.T) {
	stem := strings.Repeat("私は受付嬢になりたい - ", 12) + "Café"
	original := "/Volumes/media/Show/" + stem + ".mkv"
	if len(filepath.Base(original)) < 200 {
		t.Fatal("test original must be long")
	}
	var got transcode.Request
	mock := &mockTranscodeExecutor{
		submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
			got = req
			return transcode.Job{ID: req.ID}, nil
		},
	}
	e := NewEngine(EngineDeps{Transcode: mock})
	ec := testReconcileEC("")
	ec.State["resolved_path"] = original
	if _, err := e.stepTranscodeSubmit(context.Background(), ec); err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(got.CandidatePath)
	if filepath.Dir(got.CandidatePath) != "/Volumes/media/Show/.navigatorr-candidates" ||
		!strings.HasSuffix(base, "."+got.ID+".mkv") || !utf8.ValidString(base) {
		t.Fatalf("candidate lost its place or tail: %q", got.CandidatePath)
	}
	if n := len(base + ".partial." + got.ID); n > transcode.MaxFileNameBytes {
		t.Fatalf("partial name is %d bytes", n)
	}
	if getString(ec.State, "candidate_path") != got.CandidatePath {
		t.Fatalf("state records %q, worker got %q", getString(ec.State, "candidate_path"), got.CandidatePath)
	}
	// Its own files are still recognised by name, so cleanup can find them.
	ec.State["job_id"] = got.ID
	if c, p, err := ownCandidateFiles(ec.State); err != nil || c != got.CandidatePath || p != got.CandidatePath+".partial."+got.ID {
		t.Fatalf("own files: %q %q %v", c, p, err)
	}
}

// A job queued before names were bounded keeps its old long candidate in the
// action's state. When the worker shortens it to recover, the action follows.
func TestRecheckAdoptsTheCandidateTheWorkerShortened(t *testing.T) {
	const job = "job-act-transcode-media-0123456789abcdef"
	dir := "/Volumes/media/Show/.navigatorr-candidates/"
	old := dir + strings.Repeat("é", 120) + " Episode." + job + ".mkv"
	short := transcode.ShortenCandidatePath(old, job)
	if short == old {
		t.Fatal("test candidate must be too long")
	}
	for name, tc := range map[string]struct {
		status   string
		reported string
		want     string
	}{
		"shortened while recovering": {transcode.StatusRunning, short, short},
		"shortened then failed":      {transcode.StatusFailed, short, short},
		"unchanged":                  {transcode.StatusRunning, old, old},
		"elsewhere":                  {transcode.StatusRunning, "/Volumes/other/" + filepath.Base(short), old},
		"something else":             {transcode.StatusRunning, dir + "unrelated.mkv", old},
		"nothing reported":           {transcode.StatusRunning, "", old},
	} {
		t.Run(name, func(t *testing.T) {
			w := &retryingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}}
			// After a requested retry the worker is recovering on its own, so it
			// no longer reports that it needs a person.
			w.statusFunc = func(context.Context, string) (transcode.JobStatus, error) {
				return transcode.JobStatus{ID: job, Status: tc.status, CandidatePath: tc.reported, JobTelemetry: transcode.JobTelemetry{Phase: "publication_pending"}}, nil
			}
			e := NewEngine(EngineDeps{Transcode: w})
			ec := testReconcileEC("")
			ec.State["job_id"], ec.State["candidate_path"], ec.State["output_path"] = job, old, old
			ec.Decision = "recheck"
			if _, err := e.stepTranscodeWait(context.Background(), ec); err != nil {
				t.Fatal(err)
			}
			if got := getString(ec.State, "candidate_path"); got != tc.want {
				t.Fatalf("candidate_path=%q want %q", got, tc.want)
			}
		})
	}
}

// Discarding a stuck attempt whose candidate name is over the limit must not
// fail on a file that cannot exist.
func TestRemovingAnOverlongCandidateNameIsRemovingNothing(t *testing.T) {
	root := candidateRoot(t)
	e := candidateEngine(t, setupTestStore(t), root, &mockTranscodeExecutor{})
	long := filepath.Join(root, "Show", ".navigatorr-candidates", strings.Repeat("a", 300)+".job-x.mkv")
	if info, _, err := e.removeOwnCandidateFile(long); err != nil || info != nil {
		t.Fatalf("info=%v err=%v", info, err)
	}
}
