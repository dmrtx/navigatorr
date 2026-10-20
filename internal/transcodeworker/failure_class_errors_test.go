package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/internal/smbdirect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// The worker's own detections of a changed source are typed, so the class
// reaches failJobTerminal and the coordinator whatever the message says.
func TestSourceDigestMismatchIsRecordedAsSourceChanged(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	src := ioOptWriteFile(t, filepath.Join(ext, "source", "shared_clip.mkv"), "synthetic source bytes")
	cfg := pr6b2Config(dir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{ext}
		c.StagingPolicy = StagingPolicyAlways
	})
	w := NewWorker(cfg)

	// The plan froze the digest of different bytes: the source is no longer
	// the file the plan was built on.
	_, err := w.ensureSourceCached(context.Background(), filepath.Clean(src), sha256Hex([]byte("the bytes the plan was built on")))
	if err == nil {
		t.Fatal("staging accepted bytes that do not match the expected digest")
	}
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("the sentinel was lost: %v", err)
	}
	if got := resilience.ClassifyError(err); got != resilience.SourceChanged {
		t.Fatalf("class %s, want source_changed: %v", got, err)
	}
	job := &JobRecord{ID: "job-drift", Status: "running"}
	ioOptSeed(t, cfg.StateDir, job)
	jobDir := filepath.Join(cfg.StateDir, job.ID)
	_ = w.failJobTerminal(jobDir, filepath.Join(jobDir, "job.json"), job, fmt.Errorf("staging input: %w", err))
	if job.FailureClassification != string(resilience.SourceChanged) {
		t.Fatalf("job recorded %q, not source_changed", job.FailureClassification)
	}
}

// The SMB boundary names the class with its own constant; it must be the string
// the coordinator reads, and a download whose size moved is recorded as it.
func TestSMBSourceChangedMatchesTheResilienceClass(t *testing.T) {
	if smbdirect.SourceChanged != string(resilience.SourceChanged) {
		t.Fatalf("smbdirect %q != resilience %q", smbdirect.SourceChanged, resilience.SourceChanged)
	}
	err := fmt.Errorf("staging: %w", &smbdirect.Error{Class: smbdirect.SourceChanged, Op: "download", Err: errors.New("SMB source changed while downloading: copied=1 stat=2")})
	if got := resilience.ClassifyError(err); got != resilience.SourceChanged {
		t.Fatalf("class %s", got)
	}
}

// A staged file that changes while it is hashed is the source when the source
// was being checked and the candidate when the candidate was.
func TestLocalFileChangedWhileHashingIsClassedByWhatWasHashed(t *testing.T) {
	changed := fmt.Errorf("checking: %w", &localFileChangedError{path: "/work/source/shared_clip.mkv"})
	if !errors.Is(changed, ErrSourceInvalid) {
		t.Fatal("the sentinel was lost")
	}
	if got := resilience.ClassifyError(changed); got == resilience.SourceChanged || got == resilience.CandidateInvalid {
		t.Fatalf("the hasher alone must not pick a role, got %s", got)
	}
	if got := resilience.ClassifyError(sourceDigestFailure(changed)); got != resilience.SourceChanged {
		t.Fatalf("source check class %s", got)
	}
	if got := resilience.ClassifyError(candidateHashFailure(changed)); got != resilience.CandidateInvalid {
		t.Fatalf("candidate check class %s", got)
	}
	other := errors.New("read failed")
	if sourceDigestFailure(other) != other || candidateHashFailure(other) != other {
		t.Fatal("an unrelated error was reclassed")
	}
}

func TestVerifySourceUnchangedDetectionIsTyped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source", "shared_clip.mkv")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if err := verifySourceUnchanged(path, fi.Size(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	err := verifySourceUnchanged(path, fi.Size()+1, fi.ModTime())
	if got := resilience.ClassifyError(err); got != resilience.SourceChanged {
		t.Fatalf("modified source class %s: %v", got, err)
	}
	wrapped := fmt.Errorf("source media modified during benchmark (integrity violation, fail closed): %w", err)
	if got := benchmarkFailureClass(wrapped); got != string(resilience.SourceChanged) {
		t.Fatalf("benchmark record class %q", got)
	}
	// A source that cannot be statted is unreachable, not changed.
	missing := verifySourceUnchanged(filepath.Join(dir, "gone", "source_sha.mkv"), 1, time.Now())
	if got := resilience.ClassifyError(missing); got == resilience.SourceChanged {
		t.Fatalf("a missing source was classed source_changed: %v", missing)
	}
}

func TestPlanDriftAgainstTheSourceIsTyped(t *testing.T) {
	p := signedPlan(t, []transcode.SubtitleAction{{SourceStreamIndex: 2, TypeIndex: 0, SourceCodec: "mov_text", Operation: "transcode", Codec: "subrip", Reason: "matroska_compatibility"}})
	streams := []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"}, {Index: 2, TypeIndex: 0, Kind: "subtitle", Codec: "ass"}}
	_, err := BuildExecutionPlan(p, streams, 60)
	if err == nil {
		t.Fatal("codec drift not detected")
	}
	if got := resilience.ClassifyError(err); got != resilience.SourceChanged {
		t.Fatalf("codec drift class %s: %v", got, err)
	}
	// The same plan against a source that lost the subtitle stream.
	_, err = BuildExecutionPlan(p, streams[:1], 60)
	if err == nil {
		t.Fatal("missing subtitle not detected")
	}
	if got := resilience.ClassifyError(err); got != resilience.SourceChanged {
		t.Fatalf("missing subtitle class %s: %v", got, err)
	}
}

// Candidate integrity failures wrap ErrSourceInvalid for the callers that test
// for it, and are classed candidate_invalid, not worker_setup_failed and not
// anything about the source. They are never retried automatically.
func TestCandidateIntegrityFailuresAreCandidateInvalid(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "source", "shared_candidate.mkv")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("candidate bytes")
	if err := os.WriteFile(candidate, body, 0o600); err != nil {
		t.Fatal(err)
	}
	job := &JobRecord{CandidateSHA256: sha256Hex([]byte("other bytes")), CandidateSizeBytes: int64(len(body))}
	job.ValidationFinishedAt = time.Now()
	noBaseline := &JobRecord{CandidateSHA256: "x"}
	noBaseline.ValidationFinishedAt = time.Now()
	cases := map[string]error{
		"digest changed after validation": verifyCheckpointCandidate(context.Background(), job, candidate),
		"no integrity baseline":           verifyCheckpointCandidate(context.Background(), noBaseline, candidate),
		"empty encoder output path":       validateEncodedCandidate(""),
		"encoder output is a directory":   validateEncodedCandidate(dir),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if !errors.Is(err, ErrSourceInvalid) {
			t.Errorf("%s: the sentinel was lost: %v", name, err)
		}
		got := resilience.ClassifyError(err)
		if got != resilience.CandidateInvalid {
			t.Errorf("%s: class %s, want candidate_invalid: %v", name, got, err)
		}
		if resilience.Retryable(got, []string{string(got)}) {
			t.Errorf("%s: %s must not be retried automatically", name, got)
		}
	}
	// What failJobTerminal records for it.
	w := NewWorker(&WorkerConfig{StateDir: dir})
	rec := &JobRecord{ID: "job-candidate", Status: "running"}
	ioOptSeed(t, dir, rec)
	_ = w.failJobTerminal(filepath.Join(dir, rec.ID), filepath.Join(dir, rec.ID, "job.json"), rec, cases["digest changed after validation"])
	if rec.FailureClassification != string(resilience.CandidateInvalid) {
		t.Fatalf("job recorded %q", rec.FailureClassification)
	}
}
