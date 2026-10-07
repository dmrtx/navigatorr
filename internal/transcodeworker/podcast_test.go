package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestPodcastWorkerRenderAndCheckpointRecovery(t *testing.T) { podcastWorkerRoundtrip(t, false) }
func TestPodcastNativeWorkerRoundtrip(t *testing.T) {
	if os.Getenv("NAVIGATORR_PODCAST_NATIVE_ASR") == "" {
		t.Skip("opt-in native Apple ASR fixture")
	}
	podcastWorkerRoundtrip(t, true)
}

func podcastCancellationFixture(t *testing.T, external bool) (*Worker, SubmitRequest) {
	t.Helper()
	dir := t.TempDir()
	source, candidate := filepath.Join(dir, "source.mp3"), filepath.Join(dir, "candidate.json")
	if err := os.WriteFile(source, []byte("immutable source"), 0600); err != nil {
		t.Fatal(err)
	}
	hash, err := hashLocalFileSHA256(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultWorkerConfig()
	cfg.StateDir, cfg.LocalWorkDir = filepath.Join(dir, "jobs"), filepath.Join(dir, "work")
	cfg.AllowedRoots, cfg.ExternalRoots = []string{dir}, nil
	cfg.StagingPolicy, cfg.AppleSpeechPath = StagingPolicy("never"), "/not-executed"
	if external {
		cfg.ExternalRoots = []string{filepath.Join(dir, "external")}
		candidate = filepath.Join(dir, "external", "candidate.json")
		if err := os.MkdirAll(filepath.Dir(candidate), 0700); err != nil {
			t.Fatal(err)
		}
	}
	w := NewWorker(cfg)
	w.SetTranscodeSpawner(func(string, string, string) (int, string, error) { return 999999, "", nil })
	w.SetAliveFunc(func(*JobRecord) bool { return true })
	plan := &transcode.Plan{Container: "json", Podcast: &podcast.Task{Version: 1, Operation: "transcribe", Language: "en_US"}}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	req := SubmitRequest{ID: "podcast-cancel", SourcePath: source, CandidatePath: candidate, SourceSHA256: hash, Profile: "podcast-v1", Plan: plan}
	if _, err := w.Submit(context.Background(), req, "unused", "unused"); err != nil {
		t.Fatal(err)
	}
	// The stub runner is never signalled by Cancel.
	w.SetAliveFunc(func(*JobRecord) bool { return false })
	return w, req
}

func TestPodcastCancellationPreservesSemanticCandidate(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprintf("external=%t", external), func(t *testing.T) {
			w, req := podcastCancellationFixture(t, external)
			if err := os.WriteFile(req.CandidatePath, []byte("created by another producer after enqueue"), 0600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(w.cfg.StateDir, req.ID)
			privateRender := filepath.Join(dir, "podcast-render.mp3")
			if err := os.WriteFile(privateRender, []byte("private scratch"), 0600); err != nil {
				t.Fatal(err)
			}
			job, err := LoadJob(filepath.Join(dir, "job.json"))
			if err != nil {
				t.Fatal(err)
			}
			if external {
				if err := os.MkdirAll(filepath.Dir(job.LocalCandidatePath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(job.LocalCandidatePath, []byte("private candidate"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(job.PartialPath, []byte("private partial"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := w.Cancel(context.Background(), req.ID); err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]string{req.SourcePath: "immutable source", req.CandidatePath: "created by another producer after enqueue"} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("semantic file changed during cancellation: %s: %q %v", path, got, err)
				}
			}
			private := []string{privateRender}
			if external {
				private = append(private, job.LocalCandidatePath, job.PartialPath)
			}
			for _, path := range private {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("private scratch survived cancellation: %s: %v", path, err)
				}
			}
		})
	}
}

func TestPodcastCancellationWinsBeforePublicationCommit(t *testing.T) {
	w, req := podcastCancellationFixture(t, false)
	dir, file := filepath.Join(w.cfg.StateDir, req.ID), filepath.Join(w.cfg.StateDir, req.ID, "job.json")
	lock, err := acquireJobLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	type outcome struct {
		cancelled bool
		err       error
	}
	done := make(chan outcome, 1)
	go func() {
		cancelled, err := w.publishPodcastFile(context.Background(), dir, file, req.SourcePath, req.CandidatePath)
		done <- outcome{cancelled, err}
	}()
	// A synced publication temp proves copying can finish while Cancel owns
	// the lock. Commit must reread the cancellation saved under that lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		paths, err := filepath.Glob(filepath.Join(filepath.Dir(req.CandidatePath), ".podcast-publish-*"))
		if err != nil {
			t.Fatal(err)
		}
		ready := false
		for _, path := range paths {
			if bytes, err := os.ReadFile(path); err == nil && string(bytes) == "immutable source" {
				ready = true
			}
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("publication did not reach commit")
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, err := LoadJob(file)
	if err != nil {
		t.Fatal(err)
	}
	job.Status, job.FinishedAt = "cancelled", time.Now().UTC()
	if err := SaveJobAtomic(file, job); err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
	select {
	case result := <-done:
		if result.err != nil || !result.cancelled {
			t.Fatalf("cancellation lost to publication: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not stop after cancellation")
	}
	if _, err := os.Stat(req.CandidatePath); !os.IsNotExist(err) {
		t.Fatalf("cancelled job published a candidate: %v", err)
	}
	paths, _ := filepath.Glob(filepath.Join(filepath.Dir(req.CandidatePath), ".podcast-publish-*"))
	if len(paths) != 0 {
		t.Fatalf("publication temps survived: %v", paths)
	}
}

func TestPodcastCancellationPreservesAlreadyPublishedCandidate(t *testing.T) {
	w, req := podcastCancellationFixture(t, false)
	dir := filepath.Join(w.cfg.StateDir, req.ID)
	if cancelled, err := w.publishPodcastFile(context.Background(), dir, filepath.Join(dir, "job.json"), req.SourcePath, req.CandidatePath); err != nil || cancelled {
		t.Fatalf("publication: cancelled=%t err=%v", cancelled, err)
	}
	if _, err := w.Cancel(context.Background(), req.ID); err != nil {
		t.Fatal(err)
	}
	if bytes, err := os.ReadFile(req.CandidatePath); err != nil || string(bytes) != "immutable source" {
		t.Fatalf("validated published candidate changed: %q %v", bytes, err)
	}
}

type podcastGuardedDirectStore struct {
	fakeDirectStore
	ready   chan struct{}
	release chan struct{}
}

func (s *podcastGuardedDirectStore) PublishGuarded(_ context.Context, local, destination, _ string, guard func(func() error) error) error {
	if _, err := os.ReadFile(local); err != nil {
		return err
	}
	close(s.ready)
	<-s.release
	return guard(func() error {
		s.published++
		s.publishTo = destination
		return nil
	})
}

func TestPodcastExternalPublicationStopsAfterDurableCancellation(t *testing.T) {
	for _, directSMB := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct_smb=%t", directSMB), func(t *testing.T) {
			w, req := podcastCancellationFixture(t, true)
			dir := filepath.Join(w.cfg.StateDir, req.ID)
			file := filepath.Join(dir, "job.json")
			job, err := LoadJob(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(job.LocalCandidatePath), 0700); err != nil {
				t.Fatal(err)
			}
			data := []byte("validated result")
			if err := os.WriteFile(job.LocalCandidatePath, data, 0600); err != nil {
				t.Fatal(err)
			}
			job.CandidateSHA256, err = hashLocalFileSHA256(context.Background(), job.LocalCandidatePath)
			if err != nil {
				t.Fatal(err)
			}
			job.CandidateSizeBytes, job.EncodeComplete, job.Status, job.PID = int64(len(data)), true, "running", 0
			if err := SaveJobAtomic(file, job); err != nil {
				t.Fatal(err)
			}
			r, err := resolveOperationalForExecution(job)
			if err != nil {
				t.Fatal(err)
			}
			ready, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			w.SetFinalizeOutput(func(ctx context.Context, local, dest, id string) error {
				return finalizeOutputAtomic(ctx, local, dest, id, func(string) error {
					close(ready)
					<-release
					return nil
				})
			})
			var direct *podcastGuardedDirectStore
			if directSMB {
				direct = &podcastGuardedDirectStore{fakeDirectStore: fakeDirectStore{root: filepath.Dir(req.CandidatePath)}, ready: ready, release: release}
				w.mediaStore = direct
			}
			done := make(chan error, 1)
			go func() { done <- w.finalizeOperational(context.Background(), dir, file, job, r) }()
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("external copy did not reach atomic commit")
			}
			// Cancel is durable before this secondary checkpoint fails. Publication
			// must stop even though cleanup/signalling cannot be relied upon here.
			if err := os.MkdirAll(filepath.Join(dir, "terminal.json", "blocked"), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Cancel(context.Background(), req.ID); err == nil {
				t.Fatal("terminal marker failure did not occur")
			}
			saved, err := LoadJob(file)
			if err != nil || saved.Status != "cancelled" {
				t.Fatalf("cancellation was not durable: %+v %v", saved, err)
			}
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("publication did not stop after durable cancellation")
			}
			if _, err := os.Stat(req.CandidatePath); !os.IsNotExist(err) {
				t.Fatalf("external destination appeared after cancellation: %v", err)
			}
			if _, err := os.Stat(job.PartialPath); !os.IsNotExist(err) {
				t.Fatalf("private partial survived cancelled commit: %v", err)
			}
			if direct != nil && direct.published != 0 {
				t.Fatalf("SMB published after cancellation: %d", direct.published)
			}
		})
	}
}

func TestPodcastPublicationRejectsNonAtomicFallback(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "partial"), filepath.Join(dir, "final")
	if err := os.WriteFile(src, []byte("complete output"), 0600); err != nil {
		t.Fatal(err)
	}
	guardCalls := 0
	ctx := context.WithValue(context.Background(), podcastPublicationGuardKey{}, podcastPublicationGuard(func(commit func() error) error {
		guardCalls++
		return commit()
	}))
	err := commitNoReplaceWith(ctx, src, dst, func(string, string) error { return errNoReplaceUnsupported }, func(string, string) error { return errNoReplaceUnsupported })
	if err != errPodcastAtomicPublicationUnsupported || guardCalls != 2 {
		t.Fatalf("unsupported publication: guards=%d error=%v", guardCalls, err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("non-atomic fallback exposed output: %v", err)
	}
	if automaticFinalizationAllowed(&JobRecord{FailureClassification: "podcast_atomic_publication_unsupported"}) {
		t.Fatal("unsupported storage must not repeatedly retry")
	}
}

func TestPodcastRejectsSemanticPathsInsidePrivateScratch(t *testing.T) {
	for _, rootName := range []string{"state", "work"} {
		for _, field := range []string{"source", "candidate"} {
			for _, alias := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/alias=%t", rootName, field, alias), func(t *testing.T) {
					w, req := podcastCancellationFixture(t, false)
					root := w.cfg.StateDir
					if rootName == "work" {
						root = w.cfg.LocalWorkDir
					}
					if err := os.MkdirAll(root, 0700); err != nil {
						t.Fatal(err)
					}
					if alias {
						link := filepath.Join(filepath.Dir(root), "scratch-alias")
						if err := os.Symlink(root, link); err != nil {
							t.Fatal(err)
						}
						root = link
					}
					req.ID = "private-path"
					if field == "candidate" {
						req.CandidatePath = filepath.Join(root, req.ID, "podcast-transcript.json")
					} else {
						req.SourcePath = filepath.Join(root, "source.mp3")
						if err := os.WriteFile(req.SourcePath, []byte("private source"), 0600); err != nil {
							t.Fatal(err)
						}
						hash, err := hashLocalFileSHA256(context.Background(), req.SourcePath)
						if err != nil {
							t.Fatal(err)
						}
						req.SourceSHA256 = hash
					}
					if _, err := w.Submit(context.Background(), req, "unused", "unused"); err == nil || !strings.Contains(err.Error(), "outside worker state/scratch") {
						t.Fatalf("private media path was admitted: %v", err)
					}
				})
			}
		}
	}
}

func TestPodcastCapabilityCacheTracksExecutableBytes(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	audio := filepath.Join(dir, "spoken.mp3")
	if b, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:a", "libmp3lame", audio).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, b)
	}
	hash, err := hashLocalFileSHA256(context.Background(), audio)
	if err != nil {
		t.Fatal(err)
	}
	tr := podcast.Transcript{SchemaVersion: 1, SourceHash: "sha256:" + hash, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", DurationMS: 2000, Units: []podcast.Unit{{ID: "u000001", StartMS: 0, EndMS: 1000, Text: "probe fixture", Timing: "native_result"}}}
	transcript, count := filepath.Join(dir, "fixture.json"), filepath.Join(dir, "asr-count")
	if err := podcast.WriteJSON(transcript, tr); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	adapter := filepath.Join(dir, "adapter")
	write(adapter, "#!/bin/sh\nif [ \"$1\" = probe ]; then\n echo '{\"available\":true,\"os_version\":\"fixture\"}'\nelse\n cp "+quote(transcript)+" \"$2\"\n echo call >> "+quote(count)+"\nfi\n")
	encoder, prober := filepath.Join(dir, "ffmpeg-real"), filepath.Join(dir, "ffprobe-real")
	write(encoder, "#!/bin/sh\nexec "+quote(ffmpeg)+" \"$@\"\n")
	write(prober, "#!/bin/sh\nexec "+quote(ffprobe)+" \"$@\"\n")
	encoderLink, proberLink := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "ffprobe")
	for link, target := range map[string]string{encoderLink: encoder, proberLink: prober} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultWorkerConfig()
	cfg.StateDir, cfg.AllowedRoots = filepath.Join(dir, "jobs"), []string{dir}
	cfg.FFmpeg, cfg.FFprobe = encoderLink, proberLink
	cfg.AppleSpeechPath, cfg.AppleSpeechProbeAudio = adapter, audio
	w := NewWorker(cfg)
	check := func(wantAvailable bool, wantCalls int) {
		t.Helper()
		caps := w.podcastCapabilities(context.Background())
		if caps == nil || caps.Available != wantAvailable {
			t.Fatalf("capability available=%t: %+v", wantAvailable, caps)
		}
		b, err := os.ReadFile(count)
		if err != nil || strings.Count(string(b), "call\n") != wantCalls {
			t.Fatalf("ASR calls: want=%d got=%q err=%v", wantCalls, b, err)
		}
	}
	check(true, 1)
	check(true, 1)
	// A substituted encoder must not inherit the old executable's proof.
	write(encoder, "#!/bin/sh\nexit 1\n")
	check(false, 2)
	write(encoder, "#!/bin/sh\n# encoder revision\nexec "+quote(ffmpeg)+" \"$@\"\n")
	check(true, 3)
	// A ffprobe upgrade at the same path also reruns the native/render proof.
	write(prober, "#!/bin/sh\n# prober revision\nexec "+quote(ffprobe)+" \"$@\"\n")
	check(true, 4)
	check(true, 4)
}

func podcastWorkerRoundtrip(t *testing.T, native bool) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	source := filepath.Join(dir, "source.mp3")
	if native {
		b, err := os.ReadFile(os.Getenv("NAVIGATORR_PODCAST_AUDIO"))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(source, b, 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		if b, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=12.13", "-ac", "2", "-c:a", "libmp3lame", source).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, b)
		}
	}
	cfg := DefaultWorkerConfig()
	cfg.StateDir = filepath.Join(dir, "jobs")
	cfg.AllowedRoots = []string{dir}
	cfg.ExternalRoots = []string{}
	cfg.StagingPolicy = StagingPolicy("always")
	cfg.LocalWorkDir = filepath.Join(dir, "work")
	cfg.FFmpeg = ffmpeg
	cfg.FFprobe = ffprobe
	cfg.AppleSpeechPath = os.Getenv("NAVIGATORR_PODCAST_NATIVE_ASR")
	cfg.AppleSpeechProbeAudio = source
	cfg.AppleSpeechInstallAssets = true
	w := NewWorker(cfg)
	w.SetTranscodeSpawner(func(string, string, string) (int, string, error) { return os.Getpid(), "", nil })
	w.SetAliveFunc(func(*JobRecord) bool { return true })
	if native {
		caps := w.podcastCapabilities(ctx)
		if caps == nil || !caps.Available || !caps.NativeTimingVerified || !caps.MP3RenderVerified {
			t.Fatalf("native capabilities: %+v", caps)
		}
	}
	hash, err := hashLocalFileSHA256(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	duration, err := w.podcastDuration(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if !native {
		tr := podcast.Transcript{SchemaVersion: 1, SourceHash: "sha256:" + hash, Provider: "apple_speech", ProviderVersion: "test-fixture", Language: "en_US", DurationMS: duration, Units: []podcast.Unit{{ID: "u000001", StartMS: 0, EndMS: 800, Text: "content", Timing: "native_result"}, {ID: "u000002", StartMS: 1000, EndMS: 3000, Text: "commercial sales read", Timing: "native_result"}, {ID: "u000003", StartMS: 3100, EndMS: 8000, Text: "episode", Timing: "native_result"}, {ID: "u000004", StartMS: 8100, EndMS: 11500, Text: "ending", Timing: "native_result"}}}
		fixture := filepath.Join(dir, "fixture.json")
		podcast.WriteJSON(fixture, tr)
		helper := filepath.Join(dir, "asr")
		os.WriteFile(helper, []byte(fmt.Sprintf("#!/bin/sh\ncp '%s' \"$2\"\n", fixture)), 0700)
		cfg.AppleSpeechPath = helper
		w.cfg.AppleSpeechPath = helper
	}
	plan := &transcode.Plan{Container: "json", Podcast: &podcast.Task{Version: 1, Operation: "transcribe", Language: "en_US"}}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	asrPath := filepath.Join(dir, "asr.json")
	req := SubmitRequest{ID: "podcast-asr", SourcePath: source, CandidatePath: asrPath, Profile: "podcast-v1", SourceSHA256: hash, Plan: plan}
	if _, err := w.Submit(ctx, req, "unused", "unused"); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	st, err := w.Status(ctx, req.ID)
	if err != nil || st.Status != "completed" || st.Podcast == nil {
		t.Fatalf("ASR %+v %v", st, err)
	}
	var tr podcast.Transcript
	if err := podcast.ReadJSON(asrPath, &tr); err != nil {
		t.Fatal(err)
	}
	// Simulate death after the native transcript was synced, before job.json.
	jobFile := filepath.Join(cfg.StateDir, req.ID, "job.json")
	job, _ := LoadJob(jobFile)
	if job.StagedInputPath != "" {
		os.MkdirAll(filepath.Dir(job.StagedInputPath), 0700)
		if err := StageInputAtomic(ctx, source, job.StagedInputPath); err != nil {
			t.Fatal(err)
		}
	}
	job.Status = "running"
	job.Podcast = nil
	job.EncodeComplete = false
	job.PID = 999999
	job.FinishedAt = job.StartedAt
	SaveJobAtomic(jobFile, job)
	os.Remove(filepath.Join(cfg.StateDir, req.ID, "terminal.json"))
	w.SetAliveFunc(func(*JobRecord) bool { return false })
	if err := w.ReconcileStartup(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ = LoadJob(jobFile)
	if job.Status != "queued" {
		t.Fatalf("checkpoint not requeued: %+v", job)
	}
	w.SetAliveFunc(func(*JobRecord) bool { return true })
	w.cfg.AppleSpeechPath = "/no/such/asr"
	if err := w.InternalRun(ctx, req.ID); err != nil {
		t.Fatalf("ASR was rerun or checkpoint rejected: %v", err)
	}
	p := podcast.DefaultPolicy()
	blocks, err := podcast.Blocks(tr, p)
	if err != nil {
		t.Fatal(err)
	}
	cs := map[string]podcast.Classification{}
	target := 1
	if native {
		target = 0
	}
	for _, b := range blocks {
		c := podcast.Classification{BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "test-orchestrator"}
		for i := b.First; i <= b.Last; i++ {
			label := "content"
			if i == target {
				label = "paid_ad"
			}
			c.Decisions = append(c.Decisions, podcast.Decision{FirstID: tr.Units[i].ID, LastID: tr.Units[i].ID, Label: label, Reason: "Test label, not an accuracy evaluation"})
		}
		cs[b.ID] = c
	}
	cuts, err := podcast.Plan(tr, p, cs)
	if err != nil {
		t.Fatal(err)
	}
	plan = &transcode.Plan{Container: "mp3", Podcast: &podcast.Task{Version: 1, Operation: "render", Language: "en_US", ASRJobID: req.ID, Cuts: &cuts}}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	output := filepath.Join(dir, "clean.mp3")
	render := SubmitRequest{ID: "podcast-render", SourcePath: source, CandidatePath: output, Profile: "podcast-v1", SourceSHA256: hash, Plan: plan}
	if _, err := w.Submit(ctx, render, "unused", "unused"); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(ctx, render.ID); err != nil {
		t.Fatal(err)
	}
	st, err = w.Status(ctx, render.ID)
	if err != nil || st.Status != "completed" || st.Podcast == nil || !st.Podcast.DecodePassed {
		t.Fatalf("render %+v %v", st, err)
	}
	after, _ := hashLocalFileSHA256(ctx, source)
	if after != hash {
		t.Fatal("original changed")
	}
	// A no-ad MP3 remains byte-identical, including original metadata.
	empty := cuts
	empty.Ranges = []podcast.Cut{}
	empty.RemovedMS = 0
	empty.ClassificationDigest = podcast.Digest("all content")
	emptyPlan := &transcode.Plan{Container: "mp3", Podcast: &podcast.Task{Version: 1, Operation: "render", Language: "en_US", ASRJobID: req.ID, Cuts: &empty}}
	emptyPlan.PlanDigest, _ = transcode.DigestPlan(emptyPlan)
	noAds := SubmitRequest{ID: "podcast-no-ads", SourcePath: source, CandidatePath: filepath.Join(dir, "no-ads.mp3"), SourceSHA256: hash, Profile: "podcast-v1", Plan: emptyPlan}
	if _, err := w.Submit(ctx, noAds, "unused", "unused"); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(ctx, noAds.ID); err != nil {
		t.Fatal(err)
	}
	sameHash, err := hashLocalFileSHA256(ctx, noAds.CandidatePath)
	if err != nil || sameHash != hash {
		t.Fatal("no-ad MP3 was unnecessarily changed", err)
	}
	// The same request is terminally idempotent; changed cut evidence conflicts.
	if res, err := w.Submit(ctx, render, "unused", "unused"); err != nil || !res.Reused {
		t.Fatalf("idempotency %+v %v", res, err)
	}
	copy := *plan
	copy.Podcast = &podcast.Task{Version: 1, Operation: "render", Language: "en_US", ASRJobID: req.ID, Cuts: &cuts}
	copy.Podcast.Cuts.ClassificationDigest = podcast.Digest("different labels")
	copy.PlanDigest, _ = transcode.DigestPlan(&copy)
	render.Plan = &copy
	if _, err := w.Submit(ctx, render, "unused", "unused"); err == nil {
		t.Fatal("changed execution spec reused")
	}
	t.Logf("native=%t units=%d source_ms=%d removed_ms=%d output_ms=%d decode=%t original_preserved=true", native, len(tr.Units), tr.DurationMS, cuts.RemovedMS, st.Podcast.OutputDurationMS, st.Podcast.DecodePassed)
}
