package transcodeworker

// Deferred candidate publication: the worker side of
// docs/transcode/DEFERRED_CANDIDATE_PUBLICATION.md. Deterministic without
// ffmpeg through the same injected seams the operational tests use.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

type retainedFixture struct {
	t         *testing.T
	cfg       *WorkerConfig
	w         *Worker
	stub      *stubSpawn
	rec       *pr6b2Recorder
	extRoot   string
	src       string
	finalized *int32
}

func newRetainedFixture(t *testing.T, mutate func(*WorkerConfig)) *retainedFixture {
	t.Helper()
	tempDir := t.TempDir()
	extRoot := filepath.Join(tempDir, "external")
	cfg := pr6b2Config(tempDir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{extRoot}
		c.StagingPolicy = StagingPolicyNever
		c.DeferredPublication = true
		if mutate != nil {
			mutate(c)
		}
	})
	f := &retainedFixture{t: t, cfg: cfg, extRoot: extRoot, src: pr6b1WriteFile(t, filepath.Join(tempDir, "local", "src.mkv")), finalized: new(int32)}
	f.w = f.newWorker()
	return f
}

// newWorker builds a worker on the fixture's state directory, as after a restart.
func (f *retainedFixture) newWorker() *Worker {
	w := NewWorker(f.cfg)
	f.stub = newStubSpawn()
	f.stub.install(w)
	f.rec = &pr6b2Recorder{createOut: true}
	f.rec.install(w)
	w.SetFinalizeOutput(func(ctx context.Context, localCandidate, destination, jobID string) error {
		atomic.AddInt32(f.finalized, 1)
		return FinalizeOutputAtomic(ctx, localCandidate, destination, jobID)
	})
	return w
}

func (f *retainedFixture) dest(id string) string { return filepath.Join(f.extRoot, id+".mkv") }

func (f *retainedFixture) request(id, policy string) SubmitRequest {
	return SubmitRequest{ID: id, SourcePath: f.src, CandidatePath: f.dest(id), PublishPolicy: policy}
}

func (f *retainedFixture) submit(id, policy string) SubmitResponse {
	f.t.Helper()
	resp, err := f.w.Submit(context.Background(), f.request(id, policy), "test-exe", "")
	if err != nil {
		f.t.Fatalf("Submit(%s, %q): %v", id, policy, err)
	}
	return resp
}

// retain submits an on_approval job and runs it to the retained state.
func (f *retainedFixture) retain(id string) {
	f.t.Helper()
	f.submit(id, "on_approval")
	if err := f.w.InternalRun(context.Background(), id); err != nil {
		f.t.Fatalf("InternalRun(%s): %v", id, err)
	}
	f.assertRetained(id)
}

func (f *retainedFixture) hiddenDir(id string) string {
	return filepath.Join(f.cfg.StateDir, retainedDirPrefix+id)
}

func (f *retainedFixture) loadHidden(id string) *JobRecord {
	f.t.Helper()
	job, err := LoadJob(filepath.Join(f.hiddenDir(id), "job.json"))
	if err != nil {
		f.t.Fatalf("loading retained job %s: %v", id, err)
	}
	return job
}

func (f *retainedFixture) assertRetained(id string) {
	f.t.Helper()
	if _, err := os.Stat(filepath.Join(f.cfg.StateDir, id)); !os.IsNotExist(err) {
		f.t.Fatalf("regular job directory must be gone for a retained job: %v", err)
	}
	job := f.loadHidden(id)
	if job.Status != "running" || job.PID != 0 || job.FinalizationState != string(FinalizationStateRetained) || !job.EncodeComplete {
		f.t.Fatalf("job = status %q pid %d finalization %q encode_complete %v, want running/0/retained/true", job.Status, job.PID, job.FinalizationState, job.EncodeComplete)
	}
	if _, err := os.Stat(job.LocalCandidatePath); err != nil {
		f.t.Fatalf("local candidate must be kept: %v", err)
	}
}

func (f *retainedFixture) rewriteHidden(id string, mutate func(*JobRecord)) {
	f.t.Helper()
	path := filepath.Join(f.hiddenDir(id), "job.json")
	job, err := LoadJob(path)
	if err != nil {
		f.t.Fatal(err)
	}
	mutate(job)
	if err := SaveJobAtomic(path, job); err != nil {
		f.t.Fatal(err)
	}
}

// publish asks for the publication and runs the runner the sweep spawns.
func (f *retainedFixture) publishAndRun(id string) {
	f.t.Helper()
	if _, err := f.w.PublishRetained(context.Background(), id); err != nil {
		f.t.Fatalf("PublishRetained: %v", err)
	}
	f.runSweepAndRunner(id)
}

func (f *retainedFixture) runSweepAndRunner(id string) {
	f.t.Helper()
	started, err := f.w.ResumePostEncode(context.Background(), "test-exe", "")
	if err != nil || started != 1 {
		f.t.Fatalf("ResumePostEncode started %d, err=%v, want 1", started, err)
	}
	if err := f.w.InternalRun(context.Background(), id); err != nil {
		f.t.Fatalf("InternalRun(publish): %v", err)
	}
}

func TestRetained_SubmitPolicyValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*WorkerConfig)
		id      string
		policy  string
		wantErr string
	}{
		{name: "default policy accepted", id: "job-policy-default", policy: ""},
		{name: "on_encode accepted", id: "job-policy-encode", policy: "on_encode"},
		{name: "on_approval accepted", id: "job-policy-approval", policy: "on_approval"},
		{name: "unknown policy refused", id: "job-policy-unknown", policy: "on_friday", wantErr: "publish_policy"},
		{name: "retention disabled refuses on_approval", mutate: func(c *WorkerConfig) { c.DeferredPublication = false }, id: "job-policy-off", policy: "on_approval", wantErr: "not enabled"},
		{name: "retention disabled still accepts on_encode", mutate: func(c *WorkerConfig) { c.DeferredPublication = false }, id: "job-policy-off-encode", policy: "on_encode"},
		{name: "dot id refused for on_approval", id: ".retained-x", policy: "on_approval", wantErr: "dot"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetainedFixture(t, tc.mutate)
			_, err := f.w.Submit(context.Background(), f.request(tc.id, tc.policy), "test-exe", "")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Submit: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Submit error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestRetained_SubmitRefusesLocalDestination(t *testing.T) {
	f := newRetainedFixture(t, nil)
	req := f.request("job-local-dest", "on_approval")
	req.CandidatePath = filepath.Join(filepath.Dir(f.src), "out.mkv") // not under an external root
	_, err := f.w.Submit(context.Background(), req, "test-exe", "")
	if err == nil || !strings.Contains(err.Error(), "external destination") {
		t.Fatalf("a candidate written in place has nothing to defer: %v", err)
	}
}

func TestRetained_SubmitRefusesPodcastJobs(t *testing.T) {
	f := newRetainedFixture(t, nil)
	plan := &transcode.Plan{Container: "mp3", Podcast: &podcast.Task{Version: 1, Operation: "transcribe", Language: "en_US"}}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	req := f.request("job-podcast-1", "on_approval")
	req.Profile, req.Plan = "podcast-v1", plan
	_, err := f.w.Submit(context.Background(), req, "test-exe", "")
	if err == nil || !strings.Contains(err.Error(), "podcast") {
		t.Fatalf("a podcast job has no candidate to retain: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(f.cfg.StateDir, "job-podcast-1")); !os.IsNotExist(serr) {
		t.Fatalf("a refused submit must not leave a job behind: %v", serr)
	}
}

func TestRetained_PolicyChangesDigestOnlyWhenOnApproval(t *testing.T) {
	f := newRetainedFixture(t, nil)
	omitted := f.submit("job-digest-a", "")
	explicit := f.submit("job-digest-b", "on_encode")
	deferred := f.submit("job-digest-c", "on_approval")
	// Same request but for the job identity: the digest covers the source,
	// candidate path and plan, so compare through the transcode package.
	plan := pr6b2Plan(t)
	for id, resp := range map[string]SubmitResponse{"job-digest-a": omitted, "job-digest-b": explicit, "job-digest-c": deferred} {
		policy := ""
		if id == "job-digest-c" {
			policy = "on_approval"
		}
		want, err := transcode.DigestTranscodeExecutionSpecWithPublishPolicy(f.src, f.dest(id), "hevc-vt", "", policy, plan)
		if err != nil {
			t.Fatal(err)
		}
		if resp.ExecutionSpecDigest != want {
			t.Fatalf("%s digest = %s, want %s", id, resp.ExecutionSpecDigest, want)
		}
		legacy, err := transcode.DigestTranscodeExecutionSpecWithSourceSHA(f.src, f.dest(id), "hevc-vt", "", plan)
		if err != nil {
			t.Fatal(err)
		}
		if policy == "" && resp.ExecutionSpecDigest != legacy {
			t.Fatalf("%s: an on_encode digest must equal the one computed without a policy", id)
		}
		if policy != "" && resp.ExecutionSpecDigest == legacy {
			t.Fatalf("%s: an on_approval digest must differ", id)
		}
	}
}

func TestRetained_EncodeStopsAtCheckpointAndFreesTheSlot(t *testing.T) {
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.StagingPolicy = StagingPolicyAlways })
	// An external source is staged; the staged copy must be released.
	extSrc := pr6b1WriteFile(t, filepath.Join(f.extRoot, "movie.mkv"))
	req := SubmitRequest{ID: "job-retain-1", SourcePath: extSrc, CandidatePath: f.dest("job-retain-1"), PublishPolicy: "on_approval"}
	if _, err := f.w.Submit(context.Background(), req, "test-exe", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	staged := filepath.Join(f.cfg.StateDir, "_work", "job-retain-1", "input.mkv")
	if err := f.w.InternalRun(context.Background(), "job-retain-1"); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	f.assertRetained("job-retain-1")

	if n := atomic.LoadInt32(f.finalized); n != 0 {
		t.Fatalf("the destination was touched %d times, want 0", n)
	}
	if _, err := os.Stat(f.dest("job-retain-1")); !os.IsNotExist(err) {
		t.Fatalf("nothing may be published before approval: %v", err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("the staged source must be released at retention: %v", err)
	}
	job := f.loadHidden("job-retain-1")
	if job.CandidateSizeBytes == 0 || job.CandidateSHA256 == "" || job.RetainedAt.IsZero() {
		t.Fatalf("checkpoint must carry the attested size, digest and retention time: %+v", job)
	}
	if use, err := f.w.countActiveJobsByKind(""); err != nil || use.Video != 0 {
		t.Fatalf("a retained job occupies no slot: %+v err=%v", use, err)
	}
	if started, err := f.w.ResumePostEncode(context.Background(), "test-exe", ""); err != nil || started != 0 {
		t.Fatalf("the post-encode sweep must not publish a retained job: started=%d err=%v", started, err)
	}
}

func TestRetained_StatusByIDReportsRetainedWithAttestedIdentity(t *testing.T) {
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedTTLHours = 48 })
	f.retain("job-status-1")
	st, err := f.w.Status(context.Background(), "job-status-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Status != "running" || !st.Retained || st.FinalizationState != "retained" || !st.EncodeComplete || st.PublishPolicy != "on_approval" {
		t.Fatalf("status = %+v, want a retained, encode-complete on_approval job", st)
	}
	if st.CandidateSizeBytes == 0 || st.CandidateSHA256 == "" {
		t.Fatalf("status must carry the attested size and digest: %+v", st)
	}
	if got := st.RetainedExpiresAt.Sub(st.RetainedAt); got != 48*time.Hour {
		t.Fatalf("expiry = retained_at + %s, want 48h", got)
	}
	if st.RecoveryRequired {
		t.Fatalf("a retained job is not waiting for an operator: %+v", st)
	}
	for _, id := range []string{".retained-job-status-1", "../job-status-1", ""} {
		if _, err := f.w.Status(context.Background(), id); !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("Status(%q) = %v, want not found: the hidden name is not an id", id, err)
		}
	}
}

func TestRetained_RestartKeepsRetainedAndNeverPublishes(t *testing.T) {
	f := newRetainedFixture(t, nil)
	f.retain("job-restart-1")

	w := f.newWorker()
	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	f.assertRetained("job-restart-1")
	if started, err := w.ResumePostEncode(context.Background(), "test-exe", ""); err != nil || started != 0 {
		t.Fatalf("restart must not publish: started=%d err=%v", started, err)
	}
	if err := w.SweepRetained(context.Background()); err != nil {
		t.Fatalf("SweepRetained: %v", err)
	}
	f.assertRetained("job-restart-1")
	st, err := w.Status(context.Background(), "job-restart-1")
	if err != nil || st.Status != "running" || !st.Retained || st.FailureClassification != "" {
		t.Fatalf("status after restart = %+v err=%v, want retained and never runner_killed", st, err)
	}
	if n := atomic.LoadInt32(f.finalized); n != 0 {
		t.Fatalf("publication ran %d times, want 0", n)
	}
}

func TestRetained_CrashWindowsRecover(t *testing.T) {
	const id = "job-crash-1"
	tests := []struct {
		name string
		// stage leaves the job as a crash would.
		stage func(t *testing.T, f *retainedFixture)
		// want is the state after the sweep.
		wantHiddenState string
		wantRegular     bool
	}{
		{
			name: "runner died after the checkpoint, before the rename",
			stage: func(t *testing.T, f *retainedFixture) {
				f.retain(id)
				// Undo the retain: back to the regular name, state pending, dead runner.
				job := f.loadHidden(id)
				job.FinalizationState = string(FinalizationStatePending)
				job.PID = 4000000
				job.ProcessStartTime = "gone"
				if err := SaveJobAtomic(filepath.Join(f.hiddenDir(id), "job.json"), job); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(f.hiddenDir(id), filepath.Join(f.cfg.StateDir, id)); err != nil {
					t.Fatal(err)
				}
			},
			wantHiddenState: string(FinalizationStateRetained),
		},
		{
			name: "renamed, state not yet saved",
			stage: func(t *testing.T, f *retainedFixture) {
				f.retain(id)
				f.rewriteHidden(id, func(j *JobRecord) { j.FinalizationState = string(FinalizationStatePending); j.Phase = "encoding" })
			},
			wantHiddenState: string(FinalizationStateRetained),
		},
		{
			name: "publish requested and saved, not yet moved back",
			stage: func(t *testing.T, f *retainedFixture) {
				f.retain(id)
				f.rewriteHidden(id, func(j *JobRecord) { j.FinalizationState = string(FinalizationStateFinalizing) })
			},
			wantRegular: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetainedFixture(t, nil)
			tc.stage(t, f)

			w := f.newWorker()
			if err := w.ReconcileStartup(context.Background()); err != nil {
				t.Fatalf("ReconcileStartup: %v", err)
			}
			st, err := w.Status(context.Background(), id)
			if err != nil || st.Status != "running" || st.FailureClassification == "runner_killed" {
				t.Fatalf("a job past its checkpoint is never runner_killed: %+v err=%v", st, err)
			}
			if tc.wantRegular {
				job := pr6b1LoadJob(t, f.cfg.StateDir, id)
				if job.FinalizationState != string(FinalizationStateFinalizing) || job.PID != 0 {
					t.Fatalf("job = %+v, want finalizing with no runner in its regular directory", job)
				}
				if _, err := os.Stat(f.hiddenDir(id)); !os.IsNotExist(err) {
					t.Fatalf("hidden directory must be gone: %v", err)
				}
				w.SetHold(false)
				f.w = w
				f.runSweepAndRunner(id)
				if got := pr6b1LoadJob(t, f.cfg.StateDir, id); got.Status != "completed" {
					t.Fatalf("the recovered publish request must complete: %+v", got)
				}
				return
			}
			if started, err := w.ResumePostEncode(context.Background(), "test-exe", ""); err != nil || started != 0 {
				t.Fatalf("nothing publishes an unapproved job: started=%d err=%v", started, err)
			}
			if got := f.loadHidden(id); got.FinalizationState != tc.wantHiddenState || got.PID != 0 {
				t.Fatalf("job = %+v, want state %q and no runner", got, tc.wantHiddenState)
			}
			if _, err := os.Stat(filepath.Join(f.cfg.StateDir, id)); !os.IsNotExist(err) {
				t.Fatalf("regular directory must be gone: %v", err)
			}
		})
	}
}

func TestRetained_PublishRunsTheExistingFinalization(t *testing.T) {
	f := newRetainedFixture(t, nil)
	f.retain("job-publish-1")
	ctx := context.Background()
	spawnsBefore, encodesBefore := f.stub.n(), f.rec.encodeCalls // the original encode

	st, err := f.w.PublishRetained(ctx, "job-publish-1")
	if err != nil {
		t.Fatalf("PublishRetained: %v", err)
	}
	if st.Retained || st.FinalizationState != "finalizing" || st.Status != "running" {
		t.Fatalf("status after the request = %+v, want finalizing", st)
	}
	// The request is durable and the job is back under its regular name before
	// any runner exists.
	job := pr6b1LoadJob(t, f.cfg.StateDir, "job-publish-1")
	if job.FinalizationState != string(FinalizationStateFinalizing) || job.PID != 0 || job.FinalizationRetryCount != 0 {
		t.Fatalf("job = %+v, want finalizing, no runner, fresh retry budget", job)
	}
	if _, err := os.Stat(f.hiddenDir("job-publish-1")); !os.IsNotExist(err) {
		t.Fatalf("hidden directory must be gone: %v", err)
	}
	if n := f.stub.n(); n != spawnsBefore {
		t.Fatalf("PublishRetained must not start a runner itself: spawns %d -> %d", spawnsBefore, n)
	}
	// Idempotent before the runner starts.
	if _, err := f.w.PublishRetained(ctx, "job-publish-1"); err != nil {
		t.Fatalf("second PublishRetained: %v", err)
	}

	f.runSweepAndRunner("job-publish-1")
	if b, err := os.ReadFile(f.dest("job-publish-1")); err != nil || string(b) != "encoded-media" {
		t.Fatalf("destination = %q err=%v, want the encoded candidate", b, err)
	}
	if f.rec.encodeCalls != encodesBefore {
		t.Fatalf("publishing must never encode again: %d -> %d", encodesBefore, f.rec.encodeCalls)
	}
	done := pr6b1LoadJob(t, f.cfg.StateDir, "job-publish-1")
	if done.Status != "completed" || done.FinalizationState != string(FinalizationStateCompleted) {
		t.Fatalf("job = %+v, want completed", done)
	}
	// And after completion.
	if st, err := f.w.PublishRetained(ctx, "job-publish-1"); err != nil || st.Status != "completed" {
		t.Fatalf("publishing a completed job = %+v err=%v, want its completed status", st, err)
	}
}

func TestRetained_PublishRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("drain freeze", func(t *testing.T) {
		f := newRetainedFixture(t, nil)
		f.retain("job-held-1")
		if err := f.w.SetHold(true); err != nil {
			t.Fatal(err)
		}
		_, err := f.w.PublishRetained(ctx, "job-held-1")
		if !errors.Is(err, ErrPublishHeld) {
			t.Fatalf("err = %v, want ErrPublishHeld", err)
		}
		f.assertRetained("job-held-1")
		if err := f.w.SetHold(false); err != nil {
			t.Fatal(err)
		}
		if _, err := f.w.PublishRetained(ctx, "job-held-1"); err != nil {
			t.Fatalf("after the freeze: %v", err)
		}
	})
	t.Run("candidate changed", func(t *testing.T) {
		f := newRetainedFixture(t, nil)
		f.retain("job-changed-1")
		local := f.loadHidden("job-changed-1").LocalCandidatePath
		if err := os.WriteFile(local, []byte("tampered-media"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := f.w.PublishRetained(ctx, "job-changed-1")
		if !errors.Is(err, ErrRetainedCandidateChanged) {
			t.Fatalf("err = %v, want ErrRetainedCandidateChanged", err)
		}
		f.assertRetained("job-changed-1")
	})
	t.Run("candidate missing", func(t *testing.T) {
		f := newRetainedFixture(t, nil)
		f.retain("job-missing-file-1")
		if err := os.Remove(f.loadHidden("job-missing-file-1").LocalCandidatePath); err != nil {
			t.Fatal(err)
		}
		if _, err := f.w.PublishRetained(ctx, "job-missing-file-1"); !errors.Is(err, ErrRetainedCandidateChanged) {
			t.Fatalf("err = %v, want ErrRetainedCandidateChanged", err)
		}
	})
	t.Run("not a deferred job", func(t *testing.T) {
		f := newRetainedFixture(t, nil)
		f.submit("job-plain-1", "")
		if _, err := f.w.PublishRetained(ctx, "job-plain-1"); !errors.Is(err, ErrNotRetained) {
			t.Fatalf("err = %v, want ErrNotRetained", err)
		}
	})
	t.Run("still encoding", func(t *testing.T) {
		f := newRetainedFixture(t, nil)
		f.submit("job-encoding-1", "on_approval")
		if _, err := f.w.PublishRetained(ctx, "job-encoding-1"); !errors.Is(err, ErrNotRetained) {
			t.Fatalf("err = %v, want ErrNotRetained", err)
		}
	})
	t.Run("unknown job", func(t *testing.T) {
		f := newRetainedFixture(t, nil)
		if _, err := f.w.PublishRetained(ctx, "job-unknown-1"); !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("err = %v, want not found", err)
		}
		if _, err := os.Stat(filepath.Join(f.cfg.StateDir, "job-unknown-1")); !os.IsNotExist(err) {
			t.Fatalf("an unknown job must not get a directory: %v", err)
		}
	})
}

func TestRetained_RestartMidPublishResumes(t *testing.T) {
	f := newRetainedFixture(t, nil)
	f.retain("job-mid-1")
	if _, err := f.w.PublishRetained(context.Background(), "job-mid-1"); err != nil {
		t.Fatal(err)
	}
	// The worker restarts before any runner starts, then again with a runner
	// that died mid-upload.
	f.w = f.newWorker()
	if err := f.w.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if job := pr6b1LoadJob(t, f.cfg.StateDir, "job-mid-1"); job.FinalizationState != string(FinalizationStateFinalizing) {
		t.Fatalf("a restart must never return the job to retained: %+v", job)
	}
	f.runSweepAndRunner("job-mid-1")
	if got := pr6b1LoadJob(t, f.cfg.StateDir, "job-mid-1"); got.Status != "completed" {
		t.Fatalf("job = %+v, want completed after the resumed publication", got)
	}
}

func TestRetained_ReleaseThroughCancel(t *testing.T) {
	ctx := context.Background()
	t.Run("retained is released and its candidate removed", func(t *testing.T) {
		f := newRetainedFixture(t, nil)
		f.retain("job-release-1")
		local := f.loadHidden("job-release-1").LocalCandidatePath
		st, err := f.w.Cancel(ctx, "job-release-1")
		if err != nil || st.Status != "cancelled" {
			t.Fatalf("Cancel = %+v err=%v, want cancelled", st, err)
		}
		if _, err := os.Stat(local); !os.IsNotExist(err) {
			t.Fatalf("the retained candidate must be removed: %v", err)
		}
		if _, err := os.Stat(f.hiddenDir("job-release-1")); !os.IsNotExist(err) {
			t.Fatalf("hidden directory must be gone: %v", err)
		}
		job := pr6b1LoadJob(t, f.cfg.StateDir, "job-release-1")
		if job.Status != "cancelled" || job.FinalizationState == string(FinalizationStateRetained) {
			t.Fatalf("job = %+v, want an ordinary cancelled job", job)
		}
		// Releasing twice is harmless.
		if st, err := f.w.Cancel(ctx, "job-release-1"); err != nil || st.Status != "cancelled" {
			t.Fatalf("second Cancel = %+v err=%v", st, err)
		}
		if _, err := f.w.PublishRetained(ctx, "job-release-1"); !errors.Is(err, ErrNotRetained) {
			t.Fatalf("a released job cannot be published: %v", err)
		}
	})
	t.Run("refused while a publication may be running", func(t *testing.T) {
		f := newRetainedFixture(t, nil)
		f.retain("job-release-2")
		local := f.loadHidden("job-release-2").LocalCandidatePath
		if _, err := f.w.PublishRetained(ctx, "job-release-2"); err != nil {
			t.Fatal(err)
		}
		// Requested but not started, then started: both are refused.
		for i := 0; i < 2; i++ {
			if _, err := f.w.Cancel(ctx, "job-release-2"); !errors.Is(err, ErrReleaseRefused) {
				t.Fatalf("round %d: Cancel err = %v, want ErrReleaseRefused", i, err)
			}
			if _, err := os.Stat(local); err != nil {
				t.Fatalf("round %d: the candidate must survive a refused release: %v", i, err)
			}
			if i == 0 {
				if started, err := f.w.ResumePostEncode(ctx, "test-exe", ""); err != nil || started != 1 {
					t.Fatalf("ResumePostEncode started %d err=%v", started, err)
				}
			}
		}
		if got := pr6b1LoadJob(t, f.cfg.StateDir, "job-release-2"); got.Status != "running" || got.FinalizationState != string(FinalizationStateFinalizing) {
			t.Fatalf("job = %+v, want the publication still pending", got)
		}
	})
	t.Run("completed job is a no-op", func(t *testing.T) {
		f := newRetainedFixture(t, nil)
		f.retain("job-release-3")
		f.publishAndRun("job-release-3")
		st, err := f.w.Cancel(ctx, "job-release-3")
		if err != nil || st.Status != "completed" {
			t.Fatalf("Cancel = %+v err=%v, want completed untouched", st, err)
		}
		if _, err := os.Stat(f.dest("job-release-3")); err != nil {
			t.Fatalf("the published file must stay: %v", err)
		}
	})
	t.Run("on_encode cancel is unchanged", func(t *testing.T) {
		f := newRetainedFixture(t, nil)
		f.submit("job-release-4", "")
		st, err := f.w.Cancel(ctx, "job-release-4")
		if err != nil || st.Status != "cancelled" {
			t.Fatalf("Cancel = %+v err=%v, want cancelled", st, err)
		}
	})
	t.Run("encoding on_approval job cancels like any other", func(t *testing.T) {
		f := newRetainedFixture(t, nil)
		f.submit("job-release-5", "on_approval")
		st, err := f.w.Cancel(ctx, "job-release-5")
		if err != nil || st.Status != "cancelled" {
			t.Fatalf("Cancel = %+v err=%v, want cancelled", st, err)
		}
	})
}

func TestRetained_ExpiryLeavesATombstoneThenForgets(t *testing.T) {
	ctx := context.Background()
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedTTLHours = 24 })
	f.retain("job-expire-1")
	f.retain("job-fresh-1")
	local := f.loadHidden("job-expire-1").LocalCandidatePath
	f.rewriteHidden("job-expire-1", func(j *JobRecord) { j.RetainedAt = time.Now().Add(-25 * time.Hour) })

	if err := f.w.sweepRetained(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("the expired candidate must be removed: %v", err)
	}
	st, err := f.w.Status(ctx, "job-expire-1")
	if err != nil || st.Status != StatusExpired || st.FinalizationState != "expired" || st.Retained {
		t.Fatalf("status = %+v err=%v, want an explicit expired answer, not 404", st, err)
	}
	f.assertRetained("job-fresh-1") // not yet due
	if _, err := f.w.PublishRetained(ctx, "job-expire-1"); !errors.Is(err, ErrNotRetained) {
		t.Fatalf("an expired job cannot be published: %v", err)
	}
	// The tombstone is bounded.
	f.rewriteHidden("job-expire-1", func(j *JobRecord) { j.ExpiredAt = time.Now().Add(-expiredTombstoneTTL - time.Hour) })
	if err := f.w.sweepRetained(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.w.Status(ctx, "job-expire-1"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("after the tombstone TTL the job is unknown: %v", err)
	}
	// The same job can be encoded again after it expired.
	again, err := f.w.Submit(ctx, f.request("job-expire-1", "on_approval"), "test-exe", "")
	if err != nil || again.Reused {
		t.Fatalf("resubmit = %+v err=%v, want a fresh job", again, err)
	}
}

func TestRetained_ExpiryNeverTouchesAPublishingJob(t *testing.T) {
	ctx := context.Background()
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedTTLHours = 1 })
	f.retain("job-busy-1")
	f.rewriteHidden("job-busy-1", func(j *JobRecord) { j.RetainedAt = time.Now().Add(-48 * time.Hour) })
	if _, err := f.w.PublishRetained(ctx, "job-busy-1"); err != nil {
		t.Fatal(err)
	}
	if started, err := f.w.ResumePostEncode(ctx, "test-exe", ""); err != nil || started != 1 {
		t.Fatalf("started=%d err=%v", started, err)
	}
	local := pr6b1LoadJob(t, f.cfg.StateDir, "job-busy-1").LocalCandidatePath
	if err := f.w.sweepRetained(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatalf("a publishing job's candidate must never be expired: %v", err)
	}
	if got := pr6b1LoadJob(t, f.cfg.StateDir, "job-busy-1"); got.Status != "running" {
		t.Fatalf("job = %+v, want still running", got)
	}
	// Even if a crash left the request saved but the directory still hidden.
	f2 := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedTTLHours = 1 })
	f2.retain("job-busy-2")
	f2.rewriteHidden("job-busy-2", func(j *JobRecord) {
		j.RetainedAt = time.Now().Add(-48 * time.Hour)
		j.FinalizationState = string(FinalizationStateFinalizing)
	})
	local2 := f2.loadHidden("job-busy-2").LocalCandidatePath
	if err := f2.w.sweepRetained(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(local2); err != nil {
		t.Fatalf("a requested publication must not expire: %v", err)
	}
	if got := pr6b1LoadJob(t, f2.cfg.StateDir, "job-busy-2"); got.FinalizationState != string(FinalizationStateFinalizing) {
		t.Fatalf("job = %+v, want finalizing under its regular name", got)
	}
}

func TestRetained_DefaultTTLIsThirtyDays(t *testing.T) {
	f := newRetainedFixture(t, nil)
	if got := f.w.retainedTTL(); got != 30*24*time.Hour {
		t.Fatalf("default retained ttl = %s, want 30 days", got)
	}
}

func TestRetained_RepeatedSubmitAnswersTheRetainedJob(t *testing.T) {
	f := newRetainedFixture(t, nil)
	f.retain("job-again-1")
	resp, err := f.w.Submit(context.Background(), f.request("job-again-1", "on_approval"), "test-exe", "")
	if err != nil || !resp.Reused || resp.ID != "job-again-1" {
		t.Fatalf("resubmit = %+v err=%v, want the same job reused", resp, err)
	}
	f.assertRetained("job-again-1")
	if f.rec.encodeCalls != 1 {
		t.Fatalf("encode calls = %d, want the single original encode", f.rec.encodeCalls)
	}
	// A different spec under the same key is a conflict, not a second job.
	conflict := f.request("job-again-1", "on_approval")
	conflict.Profile = "hevc-vt"
	conflict.CandidatePath = f.dest("elsewhere")
	if _, err := f.w.Submit(context.Background(), conflict, "test-exe", ""); err == nil {
		t.Fatal("a different spec for a retained job must conflict")
	}
	if _, err := os.Stat(filepath.Join(f.cfg.StateDir, "job-again-1")); !os.IsNotExist(err) {
		t.Fatalf("no second job directory may appear: %v", err)
	}
}

func TestRetained_AdmissionLimits(t *testing.T) {
	ctx := context.Background()
	srcSize := int64(len("dummy-media"))
	t.Run("the refusal keeps its worker busy wording", func(t *testing.T) {
		// Older coordinators tell "the retention budget is full" from "no free
		// slot" by the message; a free-disk refusal is only busy.
		f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedCandidatesMaxBytes = srcSize - 1 })
		err := f.w.admitDeferredLocked(srcSize)
		if status, code := classifySubmitError(err); status != http.StatusTooManyRequests || code != transcode.WorkerErrorBusy {
			t.Fatalf("classify = %d %q (%v), want 429 worker_busy", status, code, err)
		}
		if !transcode.IsRetentionRefusal(err) {
			t.Fatalf("IsRetentionRefusal(%v) = false, want true", err)
		}
		f = newRetainedFixture(t, func(c *WorkerConfig) { c.MinFreeDiskBytes = 95 })
		f.w.freeDisk = func(string) (int64, error) { return 100, nil }
		err = f.w.admitDeferredLocked(srcSize)
		if status, code := classifySubmitError(err); status != http.StatusTooManyRequests || code != transcode.WorkerErrorBusy || transcode.IsRetentionRefusal(err) {
			t.Fatalf("classify = %d %q, retention refusal %v (%v); want 429 worker_busy, not a retention refusal", status, code, transcode.IsRetentionRefusal(err), err)
		}
	})
	t.Run("retained bytes", func(t *testing.T) {
		f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedCandidatesMaxBytes = srcSize + srcSize/2 })
		f.submit("job-limit-1", "on_approval") // starts and reserves its expected output
		// Queued behind it, the next ones are accepted and hold nothing yet.
		f.submit("job-limit-2", "on_approval")
		f.submitWaiting("job-limit-3")
		if held, _, err := f.w.retainedBytesLocked(); err != nil || held != srcSize {
			t.Fatalf("retained bytes = %d (%v), want only the started job's %d", held, err, srcSize)
		}
		// Once encoded job-limit-1 counts by its attested size and frees its
		// slot. job-limit-2 finds the budget full and publishes on encode.
		if err := f.w.InternalRun(ctx, "job-limit-1"); err != nil {
			t.Fatal(err)
		}
		f.schedule()
		job2 := f.loadQueued("job-limit-2")
		if job2.PID == 0 || job2.PublishFallback != transcode.PublishFallbackRetentionFull || job2.deferredPublication() || job2.PublishPolicy != transcode.PublishPolicyOnApproval {
			t.Fatalf("job-limit-2 = pid %d fallback %q policy %q, want started, retention_full, policy kept", job2.PID, job2.PublishFallback, job2.PublishPolicy)
		}
		st, err := f.w.Status(ctx, "job-limit-2")
		if err != nil || st.PublishFallback != transcode.PublishFallbackRetentionFull || st.PublishPolicy != transcode.PublishPolicyOnApproval {
			t.Fatalf("status = %+v (%v), want publish_fallback retention_full", st, err)
		}
		before := atomic.LoadInt32(f.finalized)
		if err := f.w.InternalRun(ctx, "job-limit-2"); err != nil {
			t.Fatal(err)
		}
		if atomic.LoadInt32(f.finalized) != before+1 {
			t.Fatal("a job that fell back must publish on encode")
		}
		if _, err := os.Stat(f.hiddenDir("job-limit-2")); !os.IsNotExist(err) {
			t.Fatalf("a job that fell back must not be retained: %v", err)
		}
		// job-limit-3 asked to wait: it stays queued, and a job behind it
		// that needs no retention still starts.
		f.schedule()
		job3 := f.loadQueued("job-limit-3")
		if job3.PID != 0 || job3.RetentionBlocked != retentionBlockedBudget || job3.PublishFallback != "" {
			t.Fatalf("job-limit-3 = pid %d blocked %q fallback %q, want waiting on the budget", job3.PID, job3.RetentionBlocked, job3.PublishFallback)
		}
		if st, err := f.w.Status(ctx, "job-limit-3"); err != nil || st.Status != "queued" || st.RetentionBlocked != retentionBlockedBudget {
			t.Fatalf("status = %+v (%v), want queued, retention_blocked retention_budget", st, err)
		}
		f.submit("job-limit-4", "")
		if job4 := f.loadQueued("job-limit-4"); job4.PID == 0 {
			t.Fatal("an on_encode job must not wait behind one waiting for retention room")
		}
		if err := f.w.InternalRun(ctx, "job-limit-4"); err != nil {
			t.Fatal(err)
		}
		// Releasing the retained candidate makes room: job-limit-3 starts and
		// keeps its candidate.
		if _, err := f.w.Cancel(ctx, "job-limit-1"); err != nil {
			t.Fatal(err)
		}
		f.schedule()
		job3 = f.loadQueued("job-limit-3")
		if job3.PID == 0 || job3.RetentionBlocked != "" || !job3.deferredPublication() {
			t.Fatalf("job-limit-3 = pid %d blocked %q deferred %v, want started and deferred", job3.PID, job3.RetentionBlocked, job3.deferredPublication())
		}
		if err := f.w.InternalRun(ctx, "job-limit-3"); err != nil {
			t.Fatal(err)
		}
		f.assertRetained("job-limit-3")
	})
	t.Run("free disk", func(t *testing.T) {
		free := int64(100)
		f := newRetainedFixture(t, func(c *WorkerConfig) { c.MinFreeDiskBytes = 95 })
		f.w.freeDisk = func(string) (int64, error) { return free, nil }
		// Below the floor even a job that did not ask to wait waits: a job
		// that publishes on encode needs that disk too.
		f.submit("job-disk-1", "on_approval")
		if job := f.loadQueued("job-disk-1"); job.PID != 0 || job.RetentionBlocked != retentionBlockedDisk || job.PublishFallback != "" {
			t.Fatalf("job-disk-1 = pid %d blocked %q fallback %q, want waiting on the disk floor", job.PID, job.RetentionBlocked, job.PublishFallback)
		}
		f.submit("job-disk-2", "")
		if job := f.loadQueued("job-disk-2"); job.PID == 0 {
			t.Fatal("an on_encode job is not subject to the free-disk floor")
		}
		if err := f.w.InternalRun(ctx, "job-disk-2"); err != nil {
			t.Fatal(err)
		}
		free = 1000
		f.schedule()
		if job := f.loadQueued("job-disk-1"); job.PID == 0 || job.RetentionBlocked != "" || !job.deferredPublication() {
			t.Fatalf("job-disk-1 = pid %d blocked %q deferred %v, want started with room on the disk", job.PID, job.RetentionBlocked, job.deferredPublication())
		}
	})
	t.Run("an unmeasurable disk holds the job rather than guesses", func(t *testing.T) {
		f := newRetainedFixture(t, func(c *WorkerConfig) { c.MinFreeDiskBytes = 1 })
		f.w.freeDisk = func(string) (int64, error) { return 0, errors.New("statfs failed") }
		f.submit("job-disk-3", "on_approval")
		if _, err := f.w.ScheduleQueued(ctx, "test-exe", ""); err == nil {
			t.Fatal("an unknown free space must be reported")
		}
		if job := f.loadQueued("job-disk-3"); job.PID != 0 {
			t.Fatal("an unknown free space must not start the job")
		}
	})
	t.Run("no limits configured", func(t *testing.T) {
		f := newRetainedFixture(t, func(c *WorkerConfig) { c.MaxParallelJobs = 2 })
		f.submit("job-free-1", "on_approval")
		f.submit("job-free-2", "on_approval")
		for _, id := range []string{"job-free-1", "job-free-2"} {
			if job := f.loadQueued(id); job.PID == 0 || !job.deferredPublication() {
				t.Fatalf("%s = pid %d deferred %v, want started and deferred", id, job.PID, job.deferredPublication())
			}
		}
	})
}

// submitWaiting submits an on_approval job that waits for retention room.
func (f *retainedFixture) submitWaiting(id string) {
	f.t.Helper()
	req := f.request(id, "on_approval")
	req.RetentionWait = true
	if _, err := f.w.Submit(context.Background(), req, "test-exe", ""); err != nil {
		f.t.Fatalf("Submit(%s, wait): %v", id, err)
	}
}

// schedule runs one scheduler pass, as the periodic sweep does.
func (f *retainedFixture) schedule() {
	f.t.Helper()
	if _, err := f.w.ScheduleQueued(context.Background(), "test-exe", ""); err != nil {
		f.t.Fatalf("ScheduleQueued: %v", err)
	}
}

// loadQueued loads a job from its regular directory.
func (f *retainedFixture) loadQueued(id string) *JobRecord {
	f.t.Helper()
	job, err := LoadJob(filepath.Join(f.cfg.StateDir, id, "job.json"))
	if err != nil {
		f.t.Fatalf("loading job %s: %v", id, err)
	}
	return job
}

func TestRetained_ConfigValidation(t *testing.T) {
	for name, mutate := range map[string]func(*WorkerConfig){
		"negative ttl":   func(c *WorkerConfig) { c.RetainedTTLHours = -1 },
		"negative max":   func(c *WorkerConfig) { c.RetainedCandidatesMaxBytes = -1 },
		"negative floor": func(c *WorkerConfig) { c.MinFreeDiskBytes = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := pr6b2Config(t.TempDir(), mutate)
			if err := cfg.normalizeOperational(); err == nil {
				t.Fatal("a negative value must fail closed")
			}
		})
	}
}

func TestRetained_ScansAnOlderWorkerRunsIgnoreHiddenDirectories(t *testing.T) {
	// A rolled-back worker runs exactly these scans over the same state
	// directory: they must neither fail on nor act on a retained job.
	f := newRetainedFixture(t, nil)
	f.retain("job-old-1")
	w := f.newWorker()
	if err := w.ReconcileStartup(context.Background()); err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if n, err := w.countActiveJobs(""); err != nil || n != 0 {
		t.Fatalf("active jobs = %d err=%v, want 0", n, err)
	}
	if jobs, err := w.listQueuedJobsObserved(); err != nil || len(jobs) != 0 {
		t.Fatalf("queued jobs = %v err=%v, want none", jobs, err)
	}
	if match, err := w.findJobByIdempotencyKeyLocked("job-old-1"); err != nil || match != nil {
		t.Fatalf("idempotency scan = %v err=%v, want no match", match, err)
	}
	if act, err := w.activitySnapshot(); err != nil || len(act.Jobs) != 0 {
		t.Fatalf("activity = %+v err=%v, want no jobs", act, err)
	}
	f.assertRetained("job-old-1")
}

func TestRetained_StaleDirectoryCollector(t *testing.T) {
	ctx := context.Background()
	f := newRetainedFixture(t, nil)
	stale := filepath.Join(f.cfg.StateDir, "job-shell-1")
	fresh := filepath.Join(f.cfg.StateDir, "job-shell-2")
	occupied := filepath.Join(f.cfg.StateDir, "job-shell-3")
	for _, dir := range []string{stale, fresh, occupied} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(occupied, "progress.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	for _, dir := range []string{stale, occupied} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.w.sweepRetained(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("an old empty directory must be collected: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a new directory may be in use and must stay: %v", err)
	}
	if _, err := os.Stat(occupied); err != nil {
		t.Fatalf("a directory holding anything else must stay: %v", err)
	}
}

func TestRetained_LateHeartbeatDoesNotRecreateAMovedDirectory(t *testing.T) {
	f := newRetainedFixture(t, nil)
	f.retain("job-hb-1")
	regular := filepath.Join(f.cfg.StateDir, "job-hb-1")
	if err := f.w.persistJobHeartbeat(regular, filepath.Join(regular, "job.json"), time.Now()); err == nil {
		t.Fatal("a heartbeat for a moved job must fail")
	}
	if _, err := os.Stat(regular); !os.IsNotExist(err) {
		t.Fatalf("a late heartbeat recreated the regular directory: %v", err)
	}
}

func TestRetained_HTTPRoute(t *testing.T) {
	f := newRetainedFixture(t, nil)
	f.retain("job-http-1")
	srv := NewServer(f.w, "test-exe", "", "")

	assertEnvelope(t, doRequest(t, srv, http.MethodGet, "/v1/jobs/job-http-1/publish", "", ""), http.StatusMethodNotAllowed, transcode.WorkerErrorMethodNotAllowed, false)
	assertEnvelope(t, doRequest(t, srv, http.MethodPost, "/v1/jobs/job-http-missing/publish", "", ""), http.StatusNotFound, transcode.WorkerErrorNotFound, false)

	f.submit("job-http-plain", "")
	assertEnvelope(t, doRequest(t, srv, http.MethodPost, "/v1/jobs/job-http-plain/publish", "", ""), http.StatusConflict, transcode.WorkerErrorNotRetained, false)

	if err := f.w.SetHold(true); err != nil {
		t.Fatal(err)
	}
	assertEnvelope(t, doRequest(t, srv, http.MethodPost, "/v1/jobs/job-http-1/publish", "", ""), http.StatusServiceUnavailable, transcode.WorkerErrorHeld, true)
	if err := f.w.SetHold(false); err != nil {
		t.Fatal(err)
	}

	local := f.loadHidden("job-http-1").LocalCandidatePath
	if err := os.WriteFile(local, []byte("tampered-media"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertEnvelope(t, doRequest(t, srv, http.MethodPost, "/v1/jobs/job-http-1/publish", "", ""), http.StatusConflict, transcode.WorkerErrorCandidateChanged, false)
	if err := os.WriteFile(local, []byte("encoded-media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := doRequest(t, srv, http.MethodPost, "/v1/jobs/job-http-1/publish", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("publish status=%d (%s)", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, srv, http.MethodPost, "/v1/jobs/job-http-1/publish", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("second publish status=%d (%s)", rec.Code, rec.Body.String())
	}
	assertEnvelope(t, doRequest(t, srv, http.MethodPost, "/v1/jobs/job-http-1/cancel", "", ""), http.StatusConflict, transcode.WorkerErrorReleaseRefused, false)
	if rec := doRequest(t, srv, http.MethodGet, "/v1/jobs/job-http-1", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("status=%d (%s)", rec.Code, rec.Body.String())
	}
}

func TestRetained_SchedulerSweepRunsMaintenance(t *testing.T) {
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedTTLHours = 1 })
	f.retain("job-sched-1")
	f.rewriteHidden("job-sched-1", func(j *JobRecord) { j.RetainedAt = time.Now().Add(-3 * time.Hour) })
	f.w.runSchedulerSweeps(context.Background(), "test-exe", "")
	st, err := f.w.Status(context.Background(), "job-sched-1")
	if err != nil || st.Status != StatusExpired {
		t.Fatalf("status = %+v err=%v, want the scheduler's post-encode sweep to expire it", st, err)
	}
	f.w.schedulerMu.Lock()
	sweeps := len(f.w.schedulerHealth.Sweeps)
	f.w.schedulerMu.Unlock()
	if sweeps != 2 {
		t.Fatalf("scheduler health must still report exactly two sweeps, got %d", sweeps)
	}
}

// Review findings: the coordinator encodes an expired job again under the same
// id while its tombstone is still on disk.
func TestRetained_ReencodeOverAnExpiredTombstone(t *testing.T) {
	ctx := context.Background()
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedTTLHours = 1 })
	f.retain("job-again-1")
	f.rewriteHidden("job-again-1", func(j *JobRecord) { j.RetainedAt = time.Now().Add(-2 * time.Hour) })
	if err := f.w.sweepRetained(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err := f.w.Status(ctx, "job-again-1"); err != nil || st.Status != StatusExpired {
		t.Fatalf("status = %+v err=%v, want the tombstone", st, err)
	}
	// Cancel keeps the tombstone a tombstone.
	if st, err := f.w.Cancel(ctx, "job-again-1"); err != nil || st.Status != StatusExpired {
		t.Fatalf("Cancel of a tombstone = %+v err=%v, want it left expired", st, err)
	}
	if got := f.loadHidden("job-again-1"); got.Status != StatusExpired {
		t.Fatalf("tombstone = %+v, want unchanged", got)
	}

	// Encoded again while the tombstone is still there: retaining must replace it.
	f.submit("job-again-1", "on_approval")
	if err := f.w.InternalRun(ctx, "job-again-1"); err != nil {
		t.Fatalf("a re-encode over a tombstone must retain, got: %v", err)
	}
	f.assertRetained("job-again-1")
	if got := f.loadHidden("job-again-1"); got.Status == StatusExpired || got.ExpiredAt.After(time.Time{}) {
		t.Fatalf("job = %+v, want the new incarnation", got)
	}
	f.publishAndRun("job-again-1")
	if _, err := os.Stat(f.dest("job-again-1")); err != nil {
		t.Fatalf("the re-encoded candidate must publish: %v", err)
	}
}

func TestRetained_PublishTransientVerifyFailureIsNotAVerdict(t *testing.T) {
	ctx := context.Background()
	f := newRetainedFixture(t, nil)
	f.retain("job-verify-1")
	job := f.loadHidden("job-verify-1")

	// A cancelled request must not discard a good candidate.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.w.PublishRetained(cancelled, "job-verify-1"); err == nil || errors.Is(err, ErrRetainedCandidateChanged) {
		t.Fatalf("cancelled request err = %v, want a retryable failure", err)
	}
	f.assertRetained("job-verify-1")

	for name, tc := range map[string]struct {
		err       error
		transient bool
	}{
		"read error":       {&os.PathError{Op: "read", Path: job.LocalCandidatePath, Err: errors.New("input/output error")}, true},
		"missing file":     {&os.PathError{Op: "open", Path: job.LocalCandidatePath, Err: os.ErrNotExist}, false},
		"classed mismatch": {candidateInvalid(errors.New("sha changed")), false},
		"plain mismatch":   {errors.New("size policy"), false},
	} {
		if got := retainedVerifyTransient(ctx, tc.err); got != tc.transient {
			t.Errorf("%s: transient = %v, want %v", name, got, tc.transient)
		}
	}

	// A real change is still a verdict.
	if err := os.WriteFile(job.LocalCandidatePath, []byte("tampered-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.w.PublishRetained(ctx, "job-verify-1"); !errors.Is(err, ErrRetainedCandidateChanged) {
		t.Fatalf("err = %v, want ErrRetainedCandidateChanged", err)
	}
}

func TestRetained_InterruptedReleaseIsFinishedBySweepAndRetry(t *testing.T) {
	ctx := context.Background()
	for _, how := range []string{"sweep", "retried cancel"} {
		t.Run(how, func(t *testing.T) {
			f := newRetainedFixture(t, nil)
			f.retain("job-half-1")
			local := f.loadHidden("job-half-1").LocalCandidatePath
			// A release that saved "cancelled" and crashed before moving back.
			f.rewriteHidden("job-half-1", func(j *JobRecord) {
				j.Status = "cancelled"
				j.FinalizationState = string(FinalizationStatePending)
				j.FinishedAt = time.Now().UTC()
			})
			if how == "sweep" {
				if err := f.w.sweepRetained(ctx); err != nil {
					t.Fatal(err)
				}
			} else if st, err := f.w.Cancel(ctx, "job-half-1"); err != nil || st.Status != "cancelled" {
				t.Fatalf("Cancel = %+v err=%v", st, err)
			}
			if _, err := os.Stat(local); !os.IsNotExist(err) {
				t.Fatalf("the candidate of a released job must not leak: %v", err)
			}
			if _, err := os.Stat(f.hiddenDir("job-half-1")); !os.IsNotExist(err) {
				t.Fatalf("hidden directory must be gone: %v", err)
			}
			if got := pr6b1LoadJob(t, f.cfg.StateDir, "job-half-1"); got.Status != "cancelled" {
				t.Fatalf("job = %+v, want cancelled", got)
			}
		})
	}
}

func TestRetained_CollectorLeavesWorkAndScratchDirectories(t *testing.T) {
	f := newRetainedFixture(t, nil)
	old := time.Now().Add(-3 * time.Hour)
	var dirs []string
	for _, name := range []string{"_work", "quality-probe-scratch", "_podcast-probe"} {
		dir := filepath.Join(f.cfg.StateDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	if err := f.w.sweepRetained(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s is not a job directory and must stay: %v", dir, err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".lock")); !os.IsNotExist(err) {
			t.Fatalf("%s: the sweep must not drop a lock file into it: %v", dir, err)
		}
	}
}

func TestRetained_FreeDiskFloorCountsEncodesNotYetWritten(t *testing.T) {
	srcSize := int64(len("dummy-media"))
	// Room for exactly one more candidate above the floor, and two slots.
	f := newRetainedFixture(t, func(c *WorkerConfig) {
		c.MinFreeDiskBytes = 1000 - srcSize - srcSize/2
		c.MaxParallelJobs = 2
	})
	f.w.freeDisk = func(string) (int64, error) { return 1000, nil }
	f.submit("job-burst-1", "on_approval")
	f.submit("job-burst-2", "on_approval")
	if job := f.loadQueued("job-burst-1"); job.PID == 0 {
		t.Fatal("the first burst job must start")
	}
	if job := f.loadQueued("job-burst-2"); job.PID != 0 || job.RetentionBlocked != retentionBlockedDisk {
		t.Fatalf("job-burst-2 = pid %d blocked %q: a second burst start must see the first one's reservation", job.PID, job.RetentionBlocked)
	}
}

// An approved publication only copies a finished file. It must not wait for
// the encode slot (a long encode would hold an approved replacement for an
// hour), and it must not take that slot from the next encode either; its own
// lane bounds how many copies hit the share at once.
func TestRetained_ApprovedPublicationsUseTheirOwnLane(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{"default lane of one", 0, 1},
		{"configured lane of two", 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetainedFixture(t, func(c *WorkerConfig) {
				c.MaxParallelJobs = 1
				c.MaxParallelPublications = tc.limit
			})
			ctx := context.Background()
			ids := []string{"job-lane-1", "job-lane-2", "job-lane-3"}
			for _, id := range ids {
				f.retain(id)
			}
			// The only encode slot is busy with a long encode.
			seedEncoderJob(t, f.w, f.stub, "job-lane-encode", "running", "libx265", true, time.Hour)
			for _, id := range ids {
				if _, err := f.w.PublishRetained(ctx, id); err != nil {
					t.Fatalf("PublishRetained(%s): %v", id, err)
				}
			}

			started, err := f.w.ResumePostEncode(ctx, "test-exe", "")
			if err != nil || started != tc.want {
				t.Fatalf("ResumePostEncode started %d, err=%v; want %d beside a full encode slot", started, err, tc.want)
			}
			copying := 0
			for _, id := range ids {
				if jobStarted(t, f.w, id) {
					copying++
				}
			}
			if copying != tc.want {
				t.Fatalf("%d publications running, want %d", copying, tc.want)
			}
			// The lane is full: the next sweep waits for a copy to finish.
			if started, err := f.w.ResumePostEncode(ctx, "test-exe", ""); err != nil || started != 0 {
				t.Fatalf("second sweep started %d, err=%v; the lane must bound the copies", started, err)
			}
			if !jobStarted(t, f.w, "job-lane-encode") {
				t.Fatal("the running encode must be left alone")
			}

			// The encode ends while copies still run: the next encode starts at once.
			done := pr6b1LoadJob(t, f.cfg.StateDir, "job-lane-encode")
			done.Status, done.PID = "completed", 0
			pr6b2Seed(t, f.cfg.StateDir, done)
			f.stub.kill("job-lane-encode")
			f.submit("job-lane-next", "")
			if !jobStarted(t, f.w, "job-lane-next") {
				t.Fatal("a running publication must not hold the encode slot")
			}
		})
	}
}
