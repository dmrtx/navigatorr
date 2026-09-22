package transcodeworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicationRejectsChangedLocalCheckpoint(t *testing.T) {
	for _, mode := range []string{"same_size", "missing_identity", "restart"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ext := filepath.Join(dir, "nas")
			src := ioOptWriteFile(t, filepath.Join(ext, "src.mkv"), "original")
			dest := filepath.Join(ext, "out.mkv")
			cfg := pr6b2Config(dir, func(c *WorkerConfig) { c.ExternalRoots = []string{ext} })
			w := NewWorker(cfg)
			newStubSpawn().install(w)
			rec := &pr6b2Recorder{createOut: true}
			rec.install(w)
			publishes := 0
			w.SetFinalizeOutput(func(ctx context.Context, local, dest, id string) error {
				publishes++
				return FinalizeOutputAtomic(ctx, local, dest, id)
			})
			w.afterEncodeCheckpoint = func(jobDir string, job *JobRecord) {
				if mode == "missing_identity" {
					job.CandidateSHA256 = ""
					if err := SaveJobAtomic(filepath.Join(jobDir, "job.json"), job); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(job.LocalCandidate(), []byte(strings.Repeat("x", int(job.CandidateSizeBytes))), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			const id = "job-integrity"
			if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: dest}, "exe", ""); err != nil {
				t.Fatal(err)
			}
			if err := w.InternalRun(context.Background(), id); err == nil {
				t.Fatal("changed checkpoint accepted")
			}
			if mode == "restart" {
				w2 := NewWorker(cfg)
				w2.SetFinalizeOutput(func(ctx context.Context, local, dest, id string) error {
					publishes++
					return FinalizeOutputAtomic(ctx, local, dest, id)
				})
				if err := w2.InternalRun(context.Background(), id); err == nil {
					t.Fatal("restart accepted changed local file")
				}
			}
			if publishes != 0 {
				t.Fatalf("invalid candidate reached NAS: %d publications", publishes)
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatalf("destination created: %v", err)
			}
			if rec.encodeCalls != 1 {
				t.Fatalf("encode replayed: %d", rec.encodeCalls)
			}
		})
	}
}

func TestPublicationRejectsWrongStagedSourceWithoutCache(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	src := ioOptWriteFile(t, filepath.Join(ext, "src.mkv"), "original")
	expected := ioOptSHA(t, src)
	cfg := pr6b2Config(dir, func(c *WorkerConfig) { c.ExternalRoots = []string{ext}; c.DisableSourceCache = true })
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)
	const id = "job-staged-integrity"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: filepath.Join(ext, "out.mkv"), SourceSHA256: expected}, "exe", ""); err != nil {
		t.Fatal(err)
	}
	// A resumed staged file exists, but is not the approved source.
	job := pr6b1LoadJob(t, cfg.StateDir, id)
	ioOptWriteFile(t, job.StagedInputPath, "tampered")
	if err := w.InternalRun(context.Background(), id); err == nil {
		t.Fatal("wrong staged source accepted")
	}
	if rec.encodeCalls != 0 || len(rec.probePaths) != 0 {
		t.Fatal("unverified source reached probe/encoder")
	}
}

func TestCandidateValidationRejectsMutationDuringProbe(t *testing.T) {
	dir := t.TempDir()
	path := ioOptWriteFile(t, filepath.Join(dir, "candidate.mkv"), "before")
	w := NewWorker(pr6b2Config(dir, nil))
	w.SetProbeSource(func(context.Context, string) ([]SourceStream, float64, error) {
		if err := os.WriteFile(path, []byte("after!"), 0600); err != nil {
			t.Fatal(err)
		}
		return []SourceStream{{Kind: "video", Codec: "hevc"}}, 60, nil
	})
	plan := ioOptPlan(t)
	src := SourceProbe{Streams: []SourceStream{{Kind: "video", Codec: "hevc"}}, DurationSec: 60}
	ep, err := BuildExecutionPlan(plan, src.Streams, 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.validateEncodedCandidateFull(context.Background(), path, ep, src, 0); err == nil {
		t.Fatal("probe mutation blessed as validated candidate")
	}
}

func TestPublicationRequiresScratchOnNode(t *testing.T) {
	for _, mode := range []string{"nas", "symlink", "local"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ext := filepath.Join(dir, "nas")
			if err := os.MkdirAll(ext, 0700); err != nil {
				t.Fatal(err)
			}
			scratch := filepath.Join(dir, "work")
			if mode == "nas" {
				scratch = filepath.Join(ext, "work")
			}
			if mode == "symlink" {
				if err := os.Symlink(ext, scratch); err != nil {
					t.Fatal(err)
				}
			}
			cfg := pr6b2Config(dir, func(c *WorkerConfig) { c.ExternalRoots = []string{ext}; c.LocalWorkDir = scratch })
			w := NewWorker(cfg)
			_, err := w.operationalMetadataFor(filepath.Join(ext, "src.mkv"), filepath.Join(ext, "out.mkv"), "job-local")
			if mode == "local" && err != nil {
				t.Fatal(err)
			}
			if mode != "local" && err == nil {
				t.Fatal("NAS workspace accepted as node scratch")
			}
		})
	}
}

func TestPublicationWorksLocallyAndRemovesOnlyOwnScratch(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	src := ioOptWriteFile(t, filepath.Join(ext, "src.mkv"), "original")
	dest := filepath.Join(ext, "out.mkv")
	cfg := pr6b2Config(dir, func(c *WorkerConfig) { c.ExternalRoots = []string{ext} })
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)
	other := ioOptWriteFile(t, filepath.Join(w.localWorkDir(), "other-job", "candidate.mkv"), "keep")
	const id = "job-node-first"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: dest, SourceSHA256: ioOptSHA(t, src)}, "exe", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	for _, path := range append(rec.probePaths, append(rec.encodeIns, rec.encodeOuts...)...) {
		if IsExternalPath(path, []string{ext}) {
			t.Fatalf("heavy operation accessed NAS directly: %s", path)
		}
	}
	workspace, err := JobWorkDir(w.localWorkDir(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("empty job workspace remains: %v", err)
	}
	if b, err := os.ReadFile(other); err != nil || string(b) != "keep" {
		t.Fatalf("another job changed: %q %v", b, err)
	}
	if b, err := os.ReadFile(src); err != nil || string(b) != "original" {
		t.Fatalf("source changed: %q %v", b, err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	}
}
