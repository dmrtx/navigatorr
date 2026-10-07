package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestPodcastDurableOrchestratorClassification(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp3")
	os.WriteFile(source, []byte("original bytes"), 0600)
	out := filepath.Join(dir, "clean.mp3")
	requests := map[string]transcode.Request{}
	statuses := map[string]transcode.JobStatus{}
	submits := 0
	failTransport := true
	tc := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{Podcast: &transcode.PodcastCapabilities{Available: true, NativeTimingVerified: true, MP3RenderVerified: true, VerifiedLanguage: "en_US"}}, nil
	}, statusFunc: func(ctx context.Context, id string) (transcode.JobStatus, error) {
		if s, ok := statuses[id]; ok {
			return s, nil
		}
		return transcode.JobStatus{}, &transcode.HTTPError{StatusCode: 404}
	}, submitFunc: func(ctx context.Context, r transcode.Request) (transcode.Job, error) {
		submits++
		requests[r.ID] = r
		statuses[r.ID] = transcode.JobStatus{ID: r.ID, Status: transcode.StatusQueued}
		if failTransport {
			failTransport = false
			return transcode.Job{}, &transcode.UncertainError{Op: "submit", JobID: r.ID, Err: fmt.Errorf("connection closed")}
		}
		return transcode.Job{ID: r.ID}, nil
	}}
	e, st := setupTranscodeEngine(t, tc, "", []string{dir}, []string{dir}, false)
	defer st.Close()
	e.deps.Config.Podcasts = config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "private"), Podcasts: map[string]config.PodcastSettings{"genwhy": {Enabled: true, WindowMS: 60000}}}
	ctx := context.Background()
	r, err := e.Run(ctx, "clean_podcast_ads", map[string]any{"path": source, "output_path": out, "podcast_id": "genwhy"})
	if err != nil || r.Status != StatusWaitingExternal {
		t.Fatalf("run=%+v err=%v", r, err)
	}
	id := getString(r.State, "podcast_transcribe_job")
	req := requests[id]
	// A lost submission ACK and coordinator restart keep the existing identity.
	e = NewEngine(e.Deps())
	r, err = e.Resume(ctx, r.ID, "", nil)
	if err != nil || submits != 1 {
		t.Fatalf("duplicate submit: %d %v", submits, err)
	}
	tr := podcast.Transcript{SchemaVersion: 1, Provider: "apple_speech", ProviderVersion: "test", Language: "en_US", SourceHash: "sha256:" + req.SourceSHA256, DurationMS: 40000}
	for i := 0; i < 100; i++ {
		tr.Units = append(tr.Units, podcast.Unit{ID: fmt.Sprintf("u%06d", i+1), StartMS: int64(i * 400), EndMS: int64(i*400 + 350), Text: "case discussion", Timing: "native_attributed_run"})
	}
	if err := podcast.WriteJSON(req.CandidatePath, tr); err != nil {
		t.Fatal(err)
	}
	attest := func(path string) (string, int64) {
		b, _ := os.ReadFile(path)
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:]), int64(len(b))
	}
	hash, size := attest(req.CandidatePath)
	statuses[id] = transcode.JobStatus{ID: id, Status: transcode.StatusCompleted, CandidatePath: req.CandidatePath, CandidateSHA256: hash, CandidateSizeBytes: size, Podcast: &podcast.Result{Operation: "transcribe", SourceHash: tr.SourceHash, TranscriptDigest: podcast.Digest(tr)}}
	r, err = e.Resume(ctx, r.ID, "", nil)
	if err != nil || r.WaitingCondition != "podcast_classification" {
		t.Fatalf("classify=%+v %v", r, err)
	}
	s, err := loadPodcastSession(parseExecutionContextMust(t, e, r.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range s.Blocks {
		c := podcast.Classification{BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(tr), PromptVersion: podcast.PromptVersion, Model: "orchestrator", Decisions: []podcast.Decision{{FirstID: tr.Units[b.First].ID, LastID: tr.Units[b.Last].ID, Label: "content", Reason: "Case discussion"}}}
		if _, err := e.PodcastClassify(ctx, r.ID, c); err == nil {
			t.Fatal("unread block accepted")
		}
		for offset := 0; ; {
			page, err := e.PodcastBlock(ctx, r.ID, b.ID, offset)
			if err != nil {
				t.Fatal(err)
			}
			if page["has_more"] == false {
				break
			}
			offset = page["next_offset"].(int)
		}
		if _, err := e.PodcastClassify(ctx, r.ID, c); err != nil {
			t.Fatal(err)
		}
	}
	e = NewEngine(e.Deps())
	r, err = e.Resume(ctx, r.ID, "plan", nil)
	if err != nil || r.WaitingCondition != "podcast_review" {
		t.Fatalf("review=%+v %v", r, err)
	}
	review, err := e.PodcastReview(ctx, r.ID, "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PodcastReview(ctx, r.ID, "wrong", true, 0); err == nil {
		t.Fatal("stale approval accepted")
	}
	if _, err := e.PodcastReview(ctx, r.ID, review["digest"].(string), true, 0); err != nil {
		t.Fatal(err)
	}
	// The approval is a durable artifact, independent of the coordinator process.
	e = NewEngine(e.Deps())
	r, err = e.Resume(ctx, r.ID, "render", nil)
	if err != nil || r.Status != StatusWaitingExternal || submits != 2 {
		t.Fatalf("render=%+v %v", r, err)
	}
	renderID := getString(r.State, "podcast_render_job")
	render := requests[renderID]
	os.WriteFile(render.CandidatePath, []byte("validated audio"), 0600)
	hash, size = attest(out)
	statuses[renderID] = transcode.JobStatus{ID: renderID, Status: transcode.StatusCompleted, CandidatePath: render.CandidatePath, CandidateSHA256: hash, CandidateSizeBytes: size, Podcast: &podcast.Result{Operation: "render", SourceHash: tr.SourceHash, TranscriptDigest: podcast.Digest(tr), OutputDurationMS: 40000, DecodePassed: true}}
	r, err = e.Resume(ctx, r.ID, "", nil)
	if err != nil || r.Status != StatusCompleted || submits != 2 {
		t.Fatalf("complete=%+v %v", r, err)
	}
	b, _ := os.ReadFile(source)
	if string(b) != "original bytes" {
		t.Fatal("original changed")
	}
	if r.State["podcast"].(map[string]any)["coverage"] != float64(1) {
		t.Fatal("coverage lost")
	}
}

func TestPodcastTerminalSubmissionIdentity(t *testing.T) {
	for _, status := range []string{StatusCompleted, StatusFailed, StatusCancelled} {
		t.Run(status, func(t *testing.T) {
			dir := t.TempDir()
			source, output := filepath.Join(dir, "source.mp3"), filepath.Join(dir, "clean.mp3")
			if err := os.WriteFile(source, []byte("original audio"), 0600); err != nil {
				t.Fatal(err)
			}
			submits := 0
			tc := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
				return transcode.WorkerCapabilities{Podcast: &transcode.PodcastCapabilities{Available: true, NativeTimingVerified: true, MP3RenderVerified: true, VerifiedLanguage: "en_US"}}, nil
			}, statusFunc: func(context.Context, string) (transcode.JobStatus, error) {
				return transcode.JobStatus{}, &transcode.HTTPError{StatusCode: 404}
			}, submitFunc: func(context.Context, transcode.Request) (transcode.Job, error) {
				submits++
				return transcode.Job{}, nil
			}}
			e, st := setupTranscodeEngine(t, tc, "", []string{dir}, []string{dir}, false)
			defer st.Close()
			e.deps.Config.Podcasts = config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "private"), Podcasts: map[string]config.PodcastSettings{"genwhy": {Enabled: true}}}
			inputs := map[string]any{"path": source, "output_path": output, "podcast_id": "genwhy"}
			ctx := context.Background()
			first, err := e.Run(ctx, "clean_podcast_ads", inputs, "same-episode-policy")
			if err != nil || first.Status != StatusWaitingExternal {
				t.Fatalf("first submission=%+v err=%v", first, err)
			}
			inst, err := st.GetActionInstance(first.ID)
			if err != nil {
				t.Fatal(err)
			}
			inst.Status = status
			if status == StatusCompleted {
				inst.CurrentStep = 6
				if err := os.WriteFile(output, []byte("validated output"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.UpdateActionInstance(*inst); err != nil {
				t.Fatal(err)
			}
			e = NewEngine(e.Deps())
			second, err := e.Run(ctx, "clean_podcast_ads", inputs, "same-episode-policy")
			if err != nil || second.ID != first.ID || second.Status != status || submits != 1 {
				t.Fatalf("terminal submission duplicated: first=%s second=%+v submits=%d err=%v", first.ID, second, submits, err)
			}
			// HTTP/background admission and synchronous MCP share the receipt.
			background, err := e.Enqueue(ctx, "clean_podcast_ads", inputs, "same-episode-policy")
			if err != nil || background.ID != first.ID || background.Status != status {
				t.Fatalf("background identity diverged: %+v err=%v", background, err)
			}
			changed := map[string]any{"path": source, "output_path": filepath.Join(dir, "other.mp3"), "podcast_id": "genwhy"}
			if _, err := e.Run(ctx, "clean_podcast_ads", changed, "same-episode-policy"); err == nil {
				t.Fatal("terminal key accepted different immutable inputs")
			}
			e.deps.Config.Podcasts.Podcasts["genwhy"] = config.PodcastSettings{Enabled: true, MaxRemovedFraction: .2}
			if _, err := e.Run(ctx, "clean_podcast_ads", inputs, "same-episode-policy"); err == nil {
				t.Fatal("terminal key accepted changed policy")
			}
			if submits != 1 {
				t.Fatalf("replayed submission repeated ASR: %d", submits)
			}
		})
	}
}

func TestPodcastExplicitRetryRetainsSubmissionIdentity(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp3")
	if err := os.WriteFile(source, []byte("original audio"), 0600); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]transcode.JobStatus{}
	submits := 0
	tc := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{Podcast: &transcode.PodcastCapabilities{Available: true, NativeTimingVerified: true, MP3RenderVerified: true, VerifiedLanguage: "en_US"}}, nil
	}, statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
		if st, ok := statuses[id]; ok {
			return st, nil
		}
		return transcode.JobStatus{}, &transcode.HTTPError{StatusCode: 404}
	}, submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
		submits++
		statuses[req.ID] = transcode.JobStatus{ID: req.ID, Status: transcode.StatusQueued}
		return transcode.Job{ID: req.ID}, nil
	}}
	e, st := setupTranscodeEngine(t, tc, "", []string{dir}, []string{dir}, false)
	defer st.Close()
	e.deps.Config.Podcasts = config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "private"), Podcasts: map[string]config.PodcastSettings{"genwhy": {Enabled: true}}}
	inputs := map[string]any{"path": source, "output_path": filepath.Join(dir, "clean.mp3"), "podcast_id": "genwhy"}
	ctx := context.Background()
	first, err := e.Run(ctx, "clean_podcast_ads", inputs, "same-episode-policy")
	if err != nil {
		t.Fatal(err)
	}
	job := getString(first.State, "podcast_transcribe_job")
	statuses[job] = transcode.JobStatus{ID: job, Status: transcode.StatusFailed, Error: "temporary ASR failure"}
	failed, err := e.Resume(ctx, first.ID, "", nil)
	if err != nil || failed.Status != StatusFailed {
		t.Fatalf("failed action=%+v err=%v", failed, err)
	}
	replayed, err := e.Run(ctx, "clean_podcast_ads", inputs, "same-episode-policy")
	if err != nil || replayed.ID != first.ID || replayed.Status != StatusFailed || submits != 1 {
		t.Fatalf("failure replay duplicated work: %+v submits=%d err=%v", replayed, submits, err)
	}
	e = NewEngine(e.Deps())
	retried, err := e.Retry(ctx, first.ID)
	if err != nil || retried.ID != first.ID || retried.Status != StatusWaitingExternal || submits != 2 || getString(retried.State, "podcast_transcribe_job") == job {
		t.Fatalf("explicit retry=%+v submits=%d err=%v", retried, submits, err)
	}
	replayed, err = e.Run(ctx, "clean_podcast_ads", inputs, "same-episode-policy")
	if err != nil || replayed.ID != retried.ID || getString(replayed.State, "podcast_transcribe_job") != getString(retried.State, "podcast_transcribe_job") || submits != 2 {
		t.Fatalf("retry receipt diverged: %+v submits=%d err=%v", replayed, submits, err)
	}
}

func TestPodcastRetryAfterRejectedWorkerAdmission(t *testing.T) {
	for _, kind := range []string{"missing", "wrapped_missing", "uncertain", "uncertain_wrapped_missing"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.mp3")
			if err := os.WriteFile(source, []byte("original audio"), 0600); err != nil {
				t.Fatal(err)
			}
			var statusErr error
			var requests []transcode.Request
			var accepted *transcode.Request
			var completed *transcode.JobStatus
			tc := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
				return transcode.WorkerCapabilities{Podcast: &transcode.PodcastCapabilities{Available: true, NativeTimingVerified: true, MP3RenderVerified: true, VerifiedLanguage: "en_US"}}, nil
			}, statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
				if statusErr != nil {
					return transcode.JobStatus{}, statusErr
				}
				if completed != nil {
					return *completed, nil
				}
				if accepted != nil {
					return transcode.JobStatus{ID: id, Status: transcode.StatusQueued}, nil
				}
				return transcode.JobStatus{}, &transcode.HTTPError{StatusCode: 404}
			}, submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
				requests = append(requests, req)
				if len(requests) == 1 {
					// The worker rejects before admission: no job or ASR side effect.
					return transcode.Job{}, &transcode.HTTPError{StatusCode: 400, Message: "source SMB path temporarily unavailable"}
				}
				accepted, statusErr = &req, nil
				return transcode.Job{ID: req.ID}, nil
			}}
			e, st := setupTranscodeEngine(t, tc, "", []string{dir}, []string{dir}, false)
			defer st.Close()
			e.deps.Config.Podcasts = config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "private"), Podcasts: map[string]config.PodcastSettings{"genwhy": {Enabled: true}}}
			ctx := context.Background()
			failed, err := e.Run(ctx, "clean_podcast_ads", map[string]any{"path": source, "output_path": filepath.Join(dir, "clean.mp3"), "podcast_id": "genwhy"}, "same-episode-policy")
			if err != nil || failed.Status != StatusFailed || failed.CurrentStep != 1 || accepted != nil {
				t.Fatalf("rejected admission=%+v err=%v", failed, err)
			}
			jobID := getString(failed.State, "podcast_transcribe_job")
			switch kind {
			case "wrapped_missing":
				statusErr = fmt.Errorf("worker status: %w", &transcode.HTTPError{StatusCode: 404})
			case "uncertain":
				statusErr = &transcode.UncertainError{Op: "status", JobID: jobID, Err: fmt.Errorf("connection closed")}
			case "uncertain_wrapped_missing":
				statusErr = &transcode.UncertainError{Op: "status", JobID: jobID, Err: &transcode.HTTPError{StatusCode: 404}}
			}
			e = NewEngine(e.Deps())
			if transcode.IsTransportUncertain(statusErr) {
				if _, err := e.Retry(ctx, failed.ID); !errors.Is(err, statusErr) {
					t.Fatalf("uncertain status did not stop retry: %v", err)
				}
				blocked, err := e.Status(ctx, failed.ID)
				if err != nil || blocked.Status != StatusFailed || getString(blocked.State, "podcast_transcribe_job") != jobID || getInt(blocked.State, "podcast_transcribe_attempt") != 0 || len(requests) != 1 || accepted != nil {
					t.Fatalf("uncertain retry changed admission: %+v requests=%d err=%v", blocked, len(requests), err)
				}
				statusErr = nil // A later, definitive read proves the original ID absent.
			}
			retried, err := e.Retry(ctx, failed.ID)
			if err != nil || retried.ID != failed.ID || retried.Status != StatusWaitingExternal || getString(retried.State, "podcast_transcribe_job") != jobID || getInt(retried.State, "podcast_transcribe_attempt") != 0 || len(requests) != 2 || accepted == nil || podcast.Digest(requests[0]) != podcast.Digest(requests[1]) {
				t.Fatalf("missing-job retry changed identity: %+v requests=%d err=%v", retried, len(requests), err)
			}
			// Import the one admitted transcription and prove polling never submits
			// another ASR operation or allocates a new attempt.
			tr := podcast.Transcript{SchemaVersion: podcast.Version, Provider: "apple_speech", ProviderVersion: "fixture", Language: "en_US", SourceHash: "sha256:" + accepted.SourceSHA256, DurationMS: 40000, Units: []podcast.Unit{{ID: "u000001", StartMS: 0, EndMS: 39900, Text: "episode content", Timing: "native_result"}}}
			if err := podcast.WriteJSON(accepted.CandidatePath, tr); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(accepted.CandidatePath)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(b)
			completed = &transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted, CandidatePath: accepted.CandidatePath, CandidateSHA256: hex.EncodeToString(hash[:]), CandidateSizeBytes: int64(len(b)), Podcast: &podcast.Result{Operation: "transcribe", SourceHash: tr.SourceHash, TranscriptDigest: podcast.Digest(tr)}}
			classified, err := e.Resume(ctx, failed.ID, "", nil)
			if err != nil || classified.WaitingCondition != "podcast_classification" || getString(classified.State, "podcast_transcribe_job") != jobID || len(requests) != 2 {
				t.Fatalf("admitted transcription was repeated: %+v requests=%d err=%v", classified, len(requests), err)
			}
		})
	}
}

func TestPodcastConcurrentSubmissionReceipt(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp3")
	if err := os.WriteFile(source, []byte("original audio"), 0600); err != nil {
		t.Fatal(err)
	}
	var submits int32
	var workerMu sync.Mutex
	statuses := map[string]transcode.JobStatus{}
	var holdPoll int32
	pollEntered, releasePoll := make(chan struct{}), make(chan struct{})
	tc := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{Podcast: &transcode.PodcastCapabilities{Available: true, NativeTimingVerified: true, MP3RenderVerified: true, VerifiedLanguage: "en_US"}}, nil
	}, statusFunc: func(ctx context.Context, id string) (transcode.JobStatus, error) {
		workerMu.Lock()
		status, ok := statuses[id]
		workerMu.Unlock()
		if ok {
			if atomic.CompareAndSwapInt32(&holdPoll, 1, 2) {
				close(pollEntered)
				select {
				case <-releasePoll:
				case <-ctx.Done():
					return transcode.JobStatus{}, ctx.Err()
				}
			}
			return status, nil
		}
		return transcode.JobStatus{}, &transcode.HTTPError{StatusCode: 404}
	}, submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
		atomic.AddInt32(&submits, 1)
		workerMu.Lock()
		defer workerMu.Unlock()
		statuses[req.ID] = transcode.JobStatus{ID: req.ID, Status: transcode.StatusQueued}
		return transcode.Job{}, nil
	}}
	e, st := setupTranscodeEngine(t, tc, "", []string{dir}, []string{dir}, false)
	defer st.Close()
	e.deps.Config.Podcasts = config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "private"), Podcasts: map[string]config.PodcastSettings{"genwhy": {Enabled: true}}}
	// The existing service reconciler may race the synchronous request to claim
	// its new durable receipt. Both must keep the same saved worker identity.
	e.deps.ReconcileInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := e.StartReconciler(ctx)
	defer func() { cancel(); <-done }()
	inputs := map[string]any{"path": source, "output_path": filepath.Join(dir, "clean.mp3"), "podcast_id": "genwhy"}
	const count = 12
	results := make([]*ActionResult, count)
	errors := make([]error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errors[i] = e.Run(context.Background(), "clean_podcast_ads", inputs, "same-episode-policy")
		}(i)
	}
	wg.Wait()
	for i := range count {
		if errors[i] != nil || results[i].ID != results[0].ID {
			t.Fatalf("submission %d diverged: %+v err=%v", i, results[i], errors[i])
		}
	}
	if atomic.LoadInt32(&submits) != 1 {
		t.Fatalf("concurrent replay repeated ASR: %d", submits)
	}
	// Reproduce the CI interleaving: a status snapshot during another worker
	// poll can legitimately show running. Inspect the stable checkpoint under
	// the same execution lease instead of racing that transition.
	atomic.StoreInt32(&holdPoll, 1)
	select {
	case <-pollEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("service reconciler did not poll the existing worker job")
	}
	duringPoll, err := e.Status(ctx, results[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("unleased snapshot during worker poll: status=%s step=%d", duringPoll.Status, duringPoll.CurrentStep)
	close(releasePoll)
	leaseCtx, release, err := e.claimExecution(ctx, results[0].ID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	current, err := e.Status(leaseCtx, results[0].ID)
	if err != nil || current.Status != StatusWaitingExternal || current.CurrentStep != 1 || getString(current.State, "podcast_session") == "" || getString(current.State, "podcast_transcribe_job") == "" || getString(current.State, "resolved_path") == "" {
		t.Fatalf("service/request admission lost progress: %+v err=%v", current, err)
	}
	if atomic.LoadInt32(&submits) != 1 {
		t.Fatalf("service reconciliation repeated ASR: %d", submits)
	}
}

func parseExecutionContextMust(t *testing.T, e *Engine, id string) *ExecutionContext {
	t.Helper()
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	return parseExecutionContext(inst, e)
}
