package action

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// A lost response must stay recoverable. Replaying a key whose stored inputs
// are identical returns the stored action even when the configuration has
// changed since, so validation that would now fail is not consulted. A
// different request on that key, or a new key, is still judged.
func TestReplayOfStoredActionIsNotRevalidated(t *testing.T) {
	mediaDir := t.TempDir()
	source := filepath.Join(mediaDir, "ep01.mkv")
	if err := os.WriteFile(source, []byte("synthetic source bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, createFakeFFprobeScript(t, defaultValidFFprobeJSON), []string{mediaDir}, []string{mediaDir}, false)
	defer st.Close()
	ctx := context.Background()
	inputs := map[string]any{"path": source, "profile_config": twoPassProfileConfig(4000)}

	first, err := e.EnqueueWorker(ctx, "transcode_media", inputs, "replay-key")
	if err != nil {
		t.Fatal(err)
	}

	// The read roots are narrowed afterwards, so the same request would now
	// be refused as new.
	elsewhere := t.TempDir()
	narrowed, err := fsop.NewResolver([]string{elsewhere}, []string{elsewhere})
	if err != nil {
		t.Fatal(err)
	}
	e.deps.Fs = narrowed
	if _, err := e.EnqueueWorker(ctx, "transcode_media", inputs, "new-key"); err == nil {
		t.Fatal("narrowed roots did not make the request invalid")
	} else {
		requireInvalid(t, err, "path")
	}
	rows := actionRows(t, e)

	// The pre-admission check used by the HTTP surface agrees with admission.
	if err := e.ValidateInputs(ctx, "transcode_media", inputs, "replay-key"); err != nil {
		t.Fatalf("replay judged by the pre-admission check: %v", err)
	}
	requireInvalid(t, e.ValidateInputs(ctx, "transcode_media", inputs, "new-key"), "path")

	for name, replay := range map[string]func() (*ActionResult, error){
		"EnqueueWorker": func() (*ActionResult, error) { return e.EnqueueWorker(ctx, "transcode_media", inputs, "replay-key") },
		"Enqueue":       func() (*ActionResult, error) { return e.Enqueue(ctx, "transcode_media", inputs, "replay-key") },
		"Run":           func() (*ActionResult, error) { return e.Run(ctx, "transcode_media", inputs, "replay-key") },
	} {
		got, err := replay()
		if err != nil || got == nil || got.ID != first.ID {
			t.Fatalf("%s: replay did not return the stored action %s: %+v %v", name, first.ID, got, err)
		}
	}
	if n := actionRows(t, e); n != rows {
		t.Fatalf("replay wrote rows: %d -> %d", rows, n)
	}

	// A different request on the same key is judged, not silently replayed.
	other := map[string]any{"path": source, "profile_config": twoPassProfileConfig(5000)}
	if _, err := e.Enqueue(ctx, "transcode_media", other, "replay-key"); err == nil {
		t.Fatal("different inputs on a bound key were accepted")
	}
	if n := actionRows(t, e); n != rows {
		t.Fatalf("conflicting request wrote rows: %d -> %d", rows, n)
	}
}

// A batch child persisted before the configuration changed is resumed or
// returned as it is; only children that would be created now are judged.
func TestExistingBatchChildIsNotRevalidatedOnResume(t *testing.T) {
	var submitCount int32
	var holdSecond atomic.Bool
	holdSecond.Store(true)
	mockExecutor := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			atomic.AddInt32(&submitCount, 1)
			if strings.Contains(req.SourcePath, "S02E02") && holdSecond.Load() {
				return transcode.Job{}, fmt.Errorf("worker busy: maximum parallel jobs (1) reached")
			}
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{ID: jobID, Status: transcode.StatusCompleted}, nil
		},
	}
	engine, st, dbPath, srv, mediaDir := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()
	ctx := context.Background()

	res, err := engine.Run(ctx, "transcode_batch", map[string]any{"service": "sonarr", "series_id": 10, "dry_run": false})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := st.ListTranscodeBatchItems(res.ID)
	children := map[string]string{}
	for _, it := range items {
		if it.ItemKey == "epfile-101" && (it.Status != "completed" || it.ChildActionID == "") {
			t.Fatalf("setup: first item not completed with a child: %+v", it)
		}
		if it.ItemKey == "epfile-103" && it.ChildActionID == "" {
			t.Fatalf("setup: busy item has no persisted child: %+v", it)
		}
		children[it.ItemKey] = it.ChildActionID
	}
	_ = st.Close()

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	tempDir := filepath.Dir(mediaDir)
	probePath := filepath.Join(tempDir, "ffprobe")

	// After the restart the read roots no longer cover the media, so a child
	// created now would be refused.
	elsewhere := t.TempDir()
	narrowed, err := fsop.NewResolver([]string{elsewhere}, []string{elsewhere})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Media: config.MediaConfig{AllowedReadRoots: []string{elsewhere}, AllowedWriteRoots: []string{elsewhere}, FfprobePath: probePath},
		Transcode: config.TranscodeConfig{
			Enabled: true, Executor: "ssh", MaxParallelJobs: 1, DefaultProfile: "general-hevc",
		},
		Services: map[string]config.ServiceConfig{
			"sonarr": {URL: srv.URL, APIKey: "mock-sonarr-key", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"},
		},
	}
	holdSecond.Store(false)
	engine2 := NewEngine(EngineDeps{Store: st2, Config: cfg, Registry: arrservice.NewRegistry(cfg), Fs: narrowed, Ffprobe: probePath, Transcode: mockExecutor, StartTime: time.Now()})
	before := atomic.LoadInt32(&submitCount)

	resumed, err := engine2.Resume(ctx, res.ID, "", nil)
	if err != nil || resumed.Status != StatusCompleted {
		t.Fatalf("resume of existing children was refused: %+v %v", resumed, err)
	}
	// Only the child that had been refused as busy is submitted again, and it
	// is the same action, not a new one.
	if after := atomic.LoadInt32(&submitCount); after != before+1 {
		t.Fatalf("expected one resubmit of the existing busy child: %d -> %d", before, after)
	}
	items2, _ := st2.ListTranscodeBatchItems(res.ID)
	for _, it := range items2 {
		want, tracked := children[it.ItemKey]
		if !tracked || want == "" {
			continue
		}
		if it.ChildActionID != want || it.Status != "completed" {
			t.Fatalf("existing child for %s replaced or unfinished: %+v (was %s)", it.ItemKey, it, want)
		}
	}
	rows, _ := st2.ListActionInstances("", 100)
	if len(rows) != 3 {
		t.Fatalf("expected the batch and its two children only, got %d actions", len(rows))
	}
}
