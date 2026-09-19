package action

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

const fakeMultiProbeJSON = `#!/bin/sh
case "$*" in
  *s2e1*|*S02E01*|*s02e01*|*hevc*|*HEVC*|*candidate*)
    cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "bits_per_raw_sample": "10", "width": 1920, "height": 1080},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "subtitle", "codec_name": "ass", "tags": {"language": "eng"}}
  ],
  "format": {
    "format_name": "matroska",
    "duration": "1420.0",
    "size": "400000000"
  },
  "chapters": []
}
JSON
    ;;
  *)
    cat << 'JSON'
{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "subtitle", "codec_name": "ass", "tags": {"language": "eng"}}
  ],
  "format": {
    "format_name": "matroska",
    "duration": "1420.0",
    "size": "1400000000"
  },
  "chapters": []
}
JSON
    ;;
esac
`

func writeCandidateOutput(candidatePath string) {
	if candidatePath != "" {
		_ = os.MkdirAll(filepath.Dir(candidatePath), 0755)
		_ = os.WriteFile(candidatePath, []byte("valid-candidate-content-stream-bytes-12345"), 0644)
	}
}

func setupBatchTestEnv(t *testing.T, tc transcode.Executor, maxParallelJobs int) (*Engine, *store.Store, string, *httptest.Server, string) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		t.Fatalf("creating media dir: %v", err)
	}

	// Create probe script
	probePath := filepath.Join(tempDir, "ffprobe")
	if err := os.WriteFile(probePath, []byte(fakeMultiProbeJSON), 0755); err != nil {
		t.Fatalf("writing probe script: %v", err)
	}

	// Create synthetic sparse media files
	// File 101: S01E01-E02 H264 anime (1.4 GB)
	file101 := filepath.Join(mediaDir, "Kaguya S01E01-E02.mkv")
	f1, err := os.Create(file101)
	if err != nil {
		t.Fatalf("creating file 101: %v", err)
	}
	_ = f1.Truncate(1400000000)
	_, _ = f1.WriteAt([]byte("kaguya-s1e1-e2-sample-content"), 0)
	_ = f1.Close()

	// File 102: S02E01 HEVC 10-bit anime (400 MB)
	file102 := filepath.Join(mediaDir, "Kaguya S02E01.mkv")
	f2, err := os.Create(file102)
	if err != nil {
		t.Fatalf("creating file 102: %v", err)
	}
	_ = f2.Truncate(400000000)
	_, _ = f2.WriteAt([]byte("kaguya-s2e1-sample-content"), 0)
	_ = f2.Close()

	// File 103: S02E02 H264 anime (1.4 GB)
	file103 := filepath.Join(mediaDir, "Kaguya S02E02.mkv")
	f3, err := os.Create(file103)
	if err != nil {
		t.Fatalf("creating file 103: %v", err)
	}
	_ = f3.Truncate(1400000000)
	_, _ = f3.WriteAt([]byte("kaguya-s2e2-sample-content"), 0)
	_ = f3.Close()

	// Mock Sonarr server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/api/v3/series/10"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         10,
				"title":      "Kaguya-sama: Love Is War",
				"seriesType": "anime",
				"genres":     []string{"Animation", "Comedy", "Romance"},
			})
		case strings.HasPrefix(path, "/api/v3/episodefile"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 101, "seriesId": 10, "seasonNumber": 1, "path": file101, "size": 1400000000},
				{"id": 102, "seriesId": 10, "seasonNumber": 2, "path": file102, "size": 400000000},
				{"id": 103, "seriesId": 10, "seasonNumber": 2, "path": file103, "size": 1400000000},
			})
		case strings.HasPrefix(path, "/api/v3/episode"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "seriesId": 10, "seasonNumber": 1, "episodeNumber": 1, "title": "I Want to Make You Confess", "episodeFileId": 101, "hasFile": true},
				{"id": 2, "seriesId": 10, "seasonNumber": 1, "episodeNumber": 2, "title": "Kaguya Wants to Be Stopped", "episodeFileId": 101, "hasFile": true},
				{"id": 3, "seriesId": 10, "seasonNumber": 2, "episodeNumber": 1, "title": "Ai Hayasaka Wants to Prevent It", "episodeFileId": 102, "hasFile": true},
				{"id": 4, "seriesId": 10, "seasonNumber": 2, "episodeNumber": 2, "title": "Miyuki Shirogane Wants to Mediate", "episodeFileId": 103, "hasFile": true},
			})
		default:
			http.NotFound(w, r)
		}
	}))

	dbPath := filepath.Join(tempDir, "action_batch.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("opening test store: %v", err)
	}

	res, err := fsop.NewResolver([]string{mediaDir, tempDir}, []string{mediaDir, tempDir})
	if err != nil {
		t.Fatalf("creating resolver: %v", err)
	}

	cfg := &config.Config{
		Media: config.MediaConfig{
			AllowedReadRoots:  []string{mediaDir, tempDir},
			AllowedWriteRoots: []string{mediaDir, tempDir},
			FfprobePath:       probePath,
		},
		Transcode: config.TranscodeConfig{
			Enabled:           true,
			Executor:          "ssh",
			MaxParallelJobs:   maxParallelJobs,
			DefaultProfile:    "general-hevc",
			MinSavingsPercent: 0,
		},
		Services: map[string]config.ServiceConfig{
			"sonarr": {
				URL:        srv.URL,
				APIKey:     "mock-sonarr-key",
				AuthMethod: "header",
				AuthHeader: "X-Api-Key",
				APIVersion: "/api/v3",
			},
		},
	}

	reg := arrservice.NewRegistry(cfg)

	engine := NewEngine(EngineDeps{
		Store:     st,
		Config:    cfg,
		Registry:  reg,
		Fs:        res,
		Ffprobe:   probePath,
		Transcode: tc,
		StartTime: time.Now(),
	})

	return engine, st, dbPath, srv, mediaDir
}

// 1. Sonarr resolution & filtering (single season vs whole series, anime detection, auto profile vs skip)
func TestTranscodeBatch_SonarrResolutionAndFiltering(t *testing.T) {
	mockExecutor := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:     jobID,
				Status: transcode.StatusCompleted,
			}, nil
		},
	}

	engine, st, _, srv, mediaDir := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()

	// Sub-test A: Single Season Filter (Season 1)
	t.Run("Season1_SingleEpisodeFile_Deduplicated", func(t *testing.T) {
		res, err := engine.Run(ctx, "transcode_batch", map[string]any{
			"service":   "sonarr",
			"series_id": 10,
			"season":    1,
			"dry_run":   true,
		})
		if err != nil {
			t.Fatalf("unexpected error running batch: %v", err)
		}
		if res.Status != "completed" {
			t.Fatalf("expected completed status, got %s (error: %s)", res.Status, res.Error)
		}

		counts, ok := res.Outputs["counts"].(map[string]int)
		if !ok {
			t.Fatalf("expected counts in outputs, got %v", res.Outputs["counts"])
		}
		if counts["total"] != 1 {
			t.Errorf("expected 1 deduplicated episode file for season 1, got %d", counts["total"])
		}
		if counts["queued"] != 1 {
			t.Errorf("expected 1 queued item for transcode, got %d", counts["queued"])
		}

		items, err := st.ListTranscodeBatchItems(res.ID)
		if err != nil {
			t.Fatalf("failed to list items from store: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("expected 1 item in store, got %d", len(items))
		}
		it := items[0]
		if it.ItemKey != "epfile-101" {
			t.Errorf("expected item key epfile-101, got %s", it.ItemKey)
		}
		if it.EpisodeInfo != "S01E01-E02" {
			t.Errorf("expected deduplicated multi-episode info S01E01-E02, got %s", it.EpisodeInfo)
		}
		if it.Profile != "anime-hevc" {
			t.Errorf("expected anime-hevc profile for Kaguya 1080p anime, got %s", it.Profile)
		}
		if it.Decision != "transcode" {
			t.Errorf("expected transcode decision, got %s", it.Decision)
		}
	})

	// Sub-test B: Whole Series (All Seasons)
	t.Run("WholeSeries_AnimeDetection_SkipsHEVC", func(t *testing.T) {
		res, err := engine.Run(ctx, "transcode_batch", map[string]any{
			"service":   "sonarr",
			"series_id": "10",
			"dry_run":   true,
		})
		if err != nil {
			t.Fatalf("unexpected error running batch: %v", err)
		}
		if res.Status != "completed" {
			t.Fatalf("expected completed status, got %s (error: %s)", res.Status, res.Error)
		}

		counts := res.Outputs["counts"].(map[string]int)
		if counts["total"] != 3 {
			t.Fatalf("expected 3 total unique episode files across all seasons, got %d", counts["total"])
		}
		if counts["queued"] != 2 {
			t.Errorf("expected 2 queued (H264) items, got %d", counts["queued"])
		}
		if counts["skip"] != 1 {
			t.Errorf("expected 1 skipped (HEVC 10-bit) item, got %d", counts["skip"])
		}

		items, err := st.ListTranscodeBatchItems(res.ID)
		if err != nil {
			t.Fatalf("failed to list items from store: %v", err)
		}
		for _, it := range items {
			if it.ItemKey == "epfile-102" {
				if it.Decision != "skip" || it.Status != "skip" {
					t.Errorf("expected epfile-102 (HEVC 10-bit) to be skipped, got decision=%s, status=%s", it.Decision, it.Status)
				}
				reasonsStr := strings.Join(it.Reasons, "; ")
				if !strings.Contains(reasonsStr, "already_hevc") || !strings.Contains(reasonsStr, "10bit") {
					t.Errorf("expected HEVC 10-bit skip reasons (already_hevc, 10bit), got: %s", reasonsStr)
				}
			} else {
				if it.Profile != "anime-hevc" {
					t.Errorf("expected item %s to have profile anime-hevc, got %s", it.ItemKey, it.Profile)
				}
			}
		}
		_ = mediaDir
	})
}

// 2. Concurrency limiting (max_parallel_jobs=1 processes serially)
func TestTranscodeBatch_ConcurrencyLimitingSerial(t *testing.T) {
	var activeJobs int32
	var maxObserved int32

	mockExecutor := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			writeCandidateOutput(req.CandidatePath)
			cur := atomic.AddInt32(&activeJobs, 1)
			for {
				old := atomic.LoadInt32(&maxObserved)
				if cur <= old || atomic.CompareAndSwapInt32(&maxObserved, old, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&activeJobs, -1)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:     jobID,
				Status: transcode.StatusCompleted,
			}, nil
		},
	}

	engine, st, _, srv, _ := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()

	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 10,
		"dry_run":   false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("expected completed status, got %s (%s)", res.Status, res.Error)
	}

	if atomic.LoadInt32(&maxObserved) > 1 {
		t.Errorf("expected max_parallel_jobs=1 to sequence serially, but observed max active jobs: %d", maxObserved)
	}
	if mockExecutor.submitCalls != 2 {
		t.Errorf("expected 2 submits for the 2 H264 files, got %d", mockExecutor.submitCalls)
	}
}

// 3. worker_busy handling (item enters waiting_for_slot, resumes when slot available, doesn't exhaust retries)
func TestTranscodeBatch_WorkerBusyHandling(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)

	mockExecutor := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			if busy.Load() {
				return transcode.Job{}, fmt.Errorf("worker busy: maximum parallel jobs (1) reached")
			}
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:     jobID,
				Status: transcode.StatusCompleted,
			}, nil
		},
	}

	engine, st, _, srv, _ := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()

	// Initial run: Worker is busy
	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 10,
		"season":    1, // 1 file to transcode
		"dry_run":   false,
	})
	if err != nil {
		t.Fatalf("unexpected run error: %v", err)
	}

	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected status waiting_external when worker busy, got %s", res.Status)
	}
	if res.WaitingCondition != "worker_busy" {
		t.Errorf("expected waiting condition worker_busy, got %s", res.WaitingCondition)
	}

	items, err := st.ListTranscodeBatchItems(res.ID)
	if err != nil {
		t.Fatalf("failed listing batch items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Status != "waiting_for_slot" {
		t.Errorf("expected item status waiting_for_slot, got %s", items[0].Status)
	}
	if items[0].Attempts != 0 {
		t.Errorf("worker_busy must NOT consume item attempts / retry budget; got attempts: %d", items[0].Attempts)
	}

	// Now free the worker slot
	busy.Store(false)

	// Resume the batch action
	resumeRes, err := engine.Resume(ctx, res.ID, "", nil)
	if err != nil {
		t.Fatalf("unexpected resume error: %v", err)
	}
	if resumeRes.Status != StatusCompleted {
		t.Fatalf("expected completed status after resume, got %s (%s)", resumeRes.Status, resumeRes.Error)
	}

	resumedItems, err := st.ListTranscodeBatchItems(res.ID)
	if err != nil {
		t.Fatalf("failed listing batch items: %v", err)
	}
	if resumedItems[0].Status != "completed" {
		t.Errorf("expected item status completed, got %s", resumedItems[0].Status)
	}
	if resumedItems[0].Attempts != 1 {
		t.Errorf("expected item attempts=1 after successful transcode, got %d", resumedItems[0].Attempts)
	}
}

// 4. Restart/recovery (restarting engine/store mid-batch resumes without duplicate transcode_media jobs)
func TestTranscodeBatch_RestartRecovery(t *testing.T) {
	var submitCount int32
	var shouldFailSecond atomic.Bool
	shouldFailSecond.Store(true)

	mockExecutor := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			atomic.AddInt32(&submitCount, 1)
			if strings.Contains(req.SourcePath, "S02E02") && shouldFailSecond.Load() {
				// Second item hits worker_busy
				return transcode.Job{}, fmt.Errorf("worker busy: maximum parallel jobs (1) reached")
			}
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:     jobID,
				Status: transcode.StatusCompleted,
			}, nil
		},
	}

	engine, st, dbPath, srv, mediaDir := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()

	ctx := context.Background()

	// Run whole series (items 101, 102 [skip], 103)
	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 10,
		"dry_run":   false,
	})
	if err != nil {
		t.Fatalf("unexpected run error: %v", err)
	}
	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected batch to wait on second item busy, got %s", res.Status)
	}

	// Verify item 101 completed, item 103 waiting_for_slot
	items1, _ := st.ListTranscodeBatchItems(res.ID)
	for _, it := range items1 {
		if it.ItemKey == "epfile-101" && it.Status != "completed" {
			t.Errorf("expected epfile-101 completed, got %s", it.Status)
		}
		if it.ItemKey == "epfile-103" && it.Status != "waiting_for_slot" {
			t.Errorf("expected epfile-103 waiting_for_slot, got %s", it.Status)
		}
	}

	// Simulate engine/daemon restart by closing store and opening fresh engine on same db
	_ = st.Close()

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	defer st2.Close()

	tempDir := filepath.Dir(mediaDir)
	probePath := filepath.Join(tempDir, "ffprobe")
	resResolver, _ := fsop.NewResolver([]string{mediaDir, tempDir}, []string{mediaDir, tempDir})

	cfg := &config.Config{
		Media: config.MediaConfig{
			AllowedReadRoots:  []string{mediaDir, tempDir},
			AllowedWriteRoots: []string{mediaDir, tempDir},
			FfprobePath:       probePath,
		},
		Transcode: config.TranscodeConfig{
			Enabled:           true,
			Executor:          "ssh",
			MaxParallelJobs:   1,
			DefaultProfile:    "general-hevc",
			MinSavingsPercent: 0,
		},
		Services: map[string]config.ServiceConfig{
			"sonarr": {
				URL:        srv.URL,
				APIKey:     "mock-sonarr-key",
				AuthMethod: "header",
				AuthHeader: "X-Api-Key",
				APIVersion: "/api/v3",
			},
		},
	}
	reg2 := arrservice.NewRegistry(cfg)

	// Now free worker slot
	shouldFailSecond.Store(false)

	engine2 := NewEngine(EngineDeps{
		Store:     st2,
		Config:    cfg,
		Registry:  reg2,
		Fs:        resResolver,
		Ffprobe:   probePath,
		Transcode: mockExecutor,
		StartTime: time.Now(),
	})

	submitsBeforeResume := atomic.LoadInt32(&submitCount)

	resumeRes, err := engine2.Resume(ctx, res.ID, "", nil)
	if err != nil {
		t.Fatalf("error resuming batch on new engine: %v", err)
	}
	if resumeRes.Status != StatusCompleted {
		t.Fatalf("expected completed status after resume on new engine, got %s (%s)", resumeRes.Status, resumeRes.Error)
	}

	submitsAfterResume := atomic.LoadInt32(&submitCount)
	// Item 101 must NOT have been submitted again! Exactly 1 new submit for item 103!
	if submitsAfterResume != submitsBeforeResume+1 {
		t.Errorf("expected exactly 1 additional submit for remaining item, before=%d, after=%d", submitsBeforeResume, submitsAfterResume)
	}

	items2, _ := st2.ListTranscodeBatchItems(res.ID)
	for _, it := range items2 {
		if it.ItemKey == "epfile-101" && it.Status != "completed" {
			t.Errorf("expected epfile-101 still completed, got %s", it.Status)
		}
		if it.ItemKey == "epfile-103" && it.Status != "completed" {
			t.Errorf("expected epfile-103 completed after resume, got %s", it.Status)
		}
	}
}

// 5. Dry-run mode (runs resolution, inspects, selects profiles, but enqueues no jobs)
func TestTranscodeBatch_DryRunMode(t *testing.T) {
	mockExecutor := &mockTranscodeExecutor{}

	engine, st, _, srv, _ := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()

	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 10,
		"dry_run":   true,
	})
	if err != nil {
		t.Fatalf("unexpected run error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected dry run completed, got %s (%s)", res.Status, res.Error)
	}

	if mockExecutor.submitCalls != 0 {
		t.Errorf("dry_run mode must NOT enqueue or submit transcode jobs; got submitCalls=%d", mockExecutor.submitCalls)
	}

	counts, ok := res.Outputs["counts"].(map[string]int)
	if !ok {
		t.Fatalf("expected counts map in outputs, got: %v", res.Outputs["counts"])
	}
	if counts["total"] != 3 {
		t.Errorf("expected total 3 items in dry-run, got %d", counts["total"])
	}
	if counts["queued"] != 2 {
		t.Errorf("expected 2 queued items, got %d", counts["queued"])
	}
	if counts["skip"] != 1 {
		t.Errorf("expected 1 skipped item, got %d", counts["skip"])
	}
}

// 6. replace_original=true validation rejection
func TestTranscodeBatch_ReplaceOriginalRejected(t *testing.T) {
	mockExecutor := &mockTranscodeExecutor{}

	engine, st, _, srv, _ := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()

	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":          "sonarr",
		"series_id":        10,
		"replace_original": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected validation failure with replace_original: true, got %s", res.Status)
	}
	if !strings.Contains(res.Error, "destructive replacement (replace_original: true) is not supported") {
		t.Errorf("expected destructive replacement error message, got: %s", res.Error)
	}
	if mockExecutor.submitCalls != 0 {
		t.Errorf("expected 0 submit calls on rejection, got %d", mockExecutor.submitCalls)
	}
}

// 7. Pause and Resume semantics
func TestTranscodeBatch_PauseAndResume(t *testing.T) {
	mockExecutor := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:     jobID,
				Status: transcode.StatusCompleted,
			}, nil
		},
	}

	engine, st, _, srv, _ := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()

	// Run with paused: true
	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 10,
		"paused":    true,
	})
	if err != nil {
		t.Fatalf("unexpected run error: %v", err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected StatusWaitingDecision when paused, got %s", res.Status)
	}

	// Resume the paused batch
	resumeRes, err := engine.Resume(ctx, res.ID, "resume", nil)
	if err != nil {
		t.Fatalf("unexpected resume error: %v", err)
	}
	if resumeRes.Status != StatusCompleted {
		t.Fatalf("expected StatusCompleted after resuming, got %s (%s)", resumeRes.Status, resumeRes.Error)
	}
	if mockExecutor.submitCalls != 2 {
		t.Errorf("expected 2 submits after resume, got %d", mockExecutor.submitCalls)
	}
}

// 8. Bounded summaries and truncation metadata
func TestTranscodeBatch_BoundedSummariesAndTruncation(t *testing.T) {
	tempDir := t.TempDir()
	mediaDir := filepath.Join(tempDir, "media")
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		t.Fatalf("creating media dir: %v", err)
	}

	probePath := filepath.Join(tempDir, "ffprobe")
	if err := os.WriteFile(probePath, []byte(fakeMultiProbeJSON), 0755); err != nil {
		t.Fatalf("writing probe script: %v", err)
	}

	const totalEpisodes = 30
	var epFiles []map[string]any
	var eps []map[string]any

	for i := 1; i <= totalEpisodes; i++ {
		filePath := filepath.Join(mediaDir, fmt.Sprintf("BigShow S01E%02d.mkv", i))
		f, err := os.Create(filePath)
		if err != nil {
			t.Fatalf("creating file %d: %v", i, err)
		}
		_ = f.Truncate(1400000000)
		_, _ = f.WriteAt([]byte(fmt.Sprintf("ep-%d-content", i)), 0)
		_ = f.Close()

		fID := int64(2000 + i)
		epFiles = append(epFiles, map[string]any{
			"id":           fID,
			"seriesId":     20,
			"seasonNumber": 1,
			"path":         filePath,
			"size":         1400000000,
		})
		eps = append(eps, map[string]any{
			"id":            int64(i),
			"seriesId":      20,
			"seasonNumber":  1,
			"episodeNumber": i,
			"title":         fmt.Sprintf("Episode %d", i),
			"episodeFileId": fID,
			"hasFile":       true,
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/api/v3/series/20"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         20,
				"title":      "Big Show",
				"seriesType": "standard",
			})
		case strings.HasPrefix(path, "/api/v3/episodefile"):
			_ = json.NewEncoder(w).Encode(epFiles)
		case strings.HasPrefix(path, "/api/v3/episode"):
			_ = json.NewEncoder(w).Encode(eps)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dbPath := filepath.Join(tempDir, "action_batch_bounded.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("opening test store: %v", err)
	}
	defer st.Close()

	resolver, err := fsop.NewResolver([]string{mediaDir, tempDir}, []string{mediaDir, tempDir})
	if err != nil {
		t.Fatalf("creating resolver: %v", err)
	}

	cfg := &config.Config{
		Media: config.MediaConfig{
			AllowedReadRoots:  []string{mediaDir, tempDir},
			AllowedWriteRoots: []string{mediaDir, tempDir},
			FfprobePath:       probePath,
		},
		Transcode: config.TranscodeConfig{
			Enabled:           true,
			Executor:          "ssh",
			MaxParallelJobs:   1,
			DefaultProfile:    "general-hevc",
			MinSavingsPercent: 0,
		},
		Services: map[string]config.ServiceConfig{
			"sonarr": {
				URL:        srv.URL,
				APIKey:     "mock-sonarr-key",
				AuthMethod: "header",
				AuthHeader: "X-Api-Key",
				APIVersion: "/api/v3",
			},
		},
	}

	engine := NewEngine(EngineDeps{
		Store:     st,
		Config:    cfg,
		Registry:  arrservice.NewRegistry(cfg),
		Fs:        resolver,
		Ffprobe:   probePath,
		Transcode: &mockTranscodeExecutor{},
		StartTime: time.Now(),
	})

	ctx := context.Background()

	// 1. Run with default limit (DefaultMaxBatchOutputItems = 25)
	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 20,
		"dry_run":   true,
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("expected completed status, got %s (%s)", res.Status, res.Error)
	}

	totalItems, ok := res.Outputs["total_items"].(int)
	if !ok || totalItems != 30 {
		t.Errorf("expected total_items == 30, got %v", res.Outputs["total_items"])
	}
	returnedItems, ok := res.Outputs["returned_items"].(int)
	if !ok || returnedItems != 25 {
		t.Errorf("expected returned_items == 25, got %v", res.Outputs["returned_items"])
	}
	itemsLimit, ok := res.Outputs["items_limit"].(int)
	if !ok || itemsLimit != 25 {
		t.Errorf("expected items_limit == 25, got %v", res.Outputs["items_limit"])
	}
	truncated, ok := res.Outputs["truncated"].(bool)
	if !ok || !truncated {
		t.Errorf("expected truncated == true, got %v", res.Outputs["truncated"])
	}
	itemsList, ok := res.Outputs["items"].([]TranscodeBatchItemSummary)
	if !ok || len(itemsList) != 25 {
		t.Fatalf("expected 25 bounded summaries in items, got %d", len(itemsList))
	}

	// Verify counts reflect the full 30 items
	counts, ok := res.Outputs["counts"].(map[string]int)
	if !ok || counts["total"] != 30 {
		t.Errorf("expected counts.total == 30, got %v", counts)
	}

	// Verify that ALL 30 items are persistent in SQLite
	dbItems, err := st.ListTranscodeBatchItems(res.ID)
	if err != nil {
		t.Fatalf("ListTranscodeBatchItems failed: %v", err)
	}
	if len(dbItems) != 30 {
		t.Errorf("expected all 30 items persistent in SQLite, got %d", len(dbItems))
	}

	// 2. Run with caller-specified limit (max_items: 10)
	res2, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 20,
		"dry_run":   true,
		"max_items": 10,
	})
	if err != nil {
		t.Fatalf("run with max_items failed: %v", err)
	}
	if res2.Outputs["returned_items"] != 10 {
		t.Errorf("expected returned_items == 10, got %v", res2.Outputs["returned_items"])
	}
	if res2.Outputs["items_limit"] != 10 {
		t.Errorf("expected items_limit == 10, got %v", res2.Outputs["items_limit"])
	}
	if res2.Outputs["truncated"] != true {
		t.Errorf("expected truncated == true, got %v", res2.Outputs["truncated"])
	}
	itemsList2 := res2.Outputs["items"].([]TranscodeBatchItemSummary)
	if len(itemsList2) != 10 {
		t.Errorf("expected 10 items in output with max_items=10, got %d", len(itemsList2))
	}
}

// 9. Job ID and Action ID traceability
func TestTranscodeBatch_JobIDAndActionIDTraceability(t *testing.T) {
	const syntheticJobID = "worker-job-custom-uuid-12345"
	mockExecutor := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: syntheticJobID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:     jobID,
				Status: transcode.StatusCompleted,
			}, nil
		},
	}

	engine, st, _, srv, _ := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()
	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 10,
		"season":    1,
		"dry_run":   false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("expected completed status, got %s (%s)", res.Status, res.Error)
	}

	// 1. Verify bounded summary in res.Outputs
	items, ok := res.Outputs["items"].([]TranscodeBatchItemSummary)
	if !ok {
		t.Fatalf("expected []TranscodeBatchItemSummary in outputs, got %T", res.Outputs["items"])
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item summary, got %d", len(items))
	}
	summary := items[0]
	if summary.ChildActionID == "" {
		t.Error("expected non-empty ChildActionID in item summary")
	}
	if summary.JobID != syntheticJobID {
		t.Errorf("expected JobID %q in summary, got %q", syntheticJobID, summary.JobID)
	}
	if summary.CandidatePath == "" {
		t.Error("expected non-empty CandidatePath in summary")
	}
	if summary.Error != "" {
		t.Errorf("expected empty error in summary, got %q", summary.Error)
	}
	if summary.Decision != "transcode" {
		t.Errorf("expected decision transcode, got %q", summary.Decision)
	}
	if summary.Status != "completed" {
		t.Errorf("expected status completed, got %q", summary.Status)
	}

	// 2. Verify persistence in SQLite
	dbItems, err := st.ListTranscodeBatchItems(res.ID)
	if err != nil {
		t.Fatalf("ListTranscodeBatchItems error: %v", err)
	}
	if len(dbItems) != 1 {
		t.Fatalf("expected 1 item in db, got %d", len(dbItems))
	}
	dbItem := dbItems[0]
	if dbItem.ChildActionID != summary.ChildActionID {
		t.Errorf("expected db ChildActionID %q, got %q", summary.ChildActionID, dbItem.ChildActionID)
	}
	if dbItem.JobID != syntheticJobID {
		t.Errorf("expected db JobID %q, got %q", syntheticJobID, dbItem.JobID)
	}
}

// 10. Async state transition: normal child waiting_external transitions item to "running", NOT "waiting_for_slot"
func TestTranscodeBatch_AsyncStateTransition(t *testing.T) {
	var jobStatus atomic.Value
	jobStatus.Store(transcode.StatusRunning)

	mockExecutor := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			st := jobStatus.Load().(string)
			return transcode.JobStatus{
				ID:     jobID,
				Status: st,
			}, nil
		},
	}

	engine, st, _, srv, _ := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()

	// Initial run: job starts and is running asynchronously
	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 10,
		"season":    1,
		"dry_run":   false,
	})
	if err != nil {
		t.Fatalf("unexpected run error: %v", err)
	}
	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected batch status %s when job running, got %s", StatusWaitingExternal, res.Status)
	}
	if res.WaitingCondition != "transcode_running" {
		t.Errorf("expected waiting condition transcode_running, got %q", res.WaitingCondition)
	}

	items, err := st.ListTranscodeBatchItems(res.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Status != "running" {
		t.Errorf("expected item status running for async job, got %q (must NOT be waiting_for_slot)", items[0].Status)
	}
	if items[0].Attempts != 0 {
		t.Errorf("expected attempts 0 while still in flight, got %d", items[0].Attempts)
	}

	// Remote job finishes
	jobStatus.Store(transcode.StatusCompleted)

	// Resume batch
	resumed, err := engine.Resume(ctx, res.ID, "", nil)
	if err != nil {
		t.Fatalf("unexpected resume error: %v", err)
	}
	if resumed.Status != StatusCompleted {
		t.Fatalf("expected batch status completed after job finish, got %s (%s)", resumed.Status, resumed.Error)
	}

	itemsAfter, _ := st.ListTranscodeBatchItems(res.ID)
	if itemsAfter[0].Status != "completed" {
		t.Errorf("expected item status completed, got %q", itemsAfter[0].Status)
	}
	if itemsAfter[0].Attempts != 1 {
		t.Errorf("expected item attempts 1 after completion, got %d", itemsAfter[0].Attempts)
	}
}

// 11. Concurrency with max_parallel_jobs=2: active running children occupy slots across resumes
func TestTranscodeBatch_MaxParallel2_SlotOccupancy(t *testing.T) {
	var job1Status atomic.Value
	job1Status.Store(transcode.StatusRunning)
	var job2Status atomic.Value
	job2Status.Store(transcode.StatusRunning)

	var submits int32
	var jobMap sync.Map
	mockExecutor := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			atomic.AddInt32(&submits, 1)
			jobMap.Store(req.ID, req.SourcePath)
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			val, _ := jobMap.Load(jobID)
			src, _ := val.(string)
			if strings.Contains(src, "S01E01") {
				return transcode.JobStatus{ID: jobID, Status: job1Status.Load().(string)}, nil
			}
			return transcode.JobStatus{ID: jobID, Status: job2Status.Load().(string)}, nil
		},
	}

	// Setup with maxParallelJobs = 2
	engine, st, _, srv, _ := setupBatchTestEnv(t, mockExecutor, 2)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()

	// Run series (items 101 [h264], 102 [hevc skip], 103 [h264])
	// Both items 101 and 103 need transcode. maxParallel=2 allows both to run simultaneously.
	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 10,
		"dry_run":   false,
	})
	if err != nil {
		t.Fatalf("unexpected run error: %v", err)
	}
	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected batch status waiting_external with 2 jobs in flight, got %s", res.Status)
	}
	if atomic.LoadInt32(&submits) != 2 {
		t.Fatalf("expected exactly 2 submits to fill the 2 parallel slots, got %d", submits)
	}

	items, _ := st.ListTranscodeBatchItems(res.ID)
	runningCount := 0
	for _, it := range items {
		if it.Status == "running" {
			runningCount++
		}
	}
	if runningCount != 2 {
		t.Errorf("expected exactly 2 items running, got %d", runningCount)
	}

	// Job 1 completes, Job 2 remains running
	job1Status.Store(transcode.StatusCompleted)

	resumed1, err := engine.Resume(ctx, res.ID, "", nil)
	if err != nil {
		t.Fatalf("unexpected resume1 error: %v", err)
	}
	// Batch should still be waiting_external because Job 2 is still running
	if resumed1.Status != StatusWaitingExternal {
		t.Fatalf("expected batch to wait while job 2 is running, got %s", resumed1.Status)
	}

	items1, _ := st.ListTranscodeBatchItems(res.ID)
	for _, it := range items1 {
		if it.ItemKey == "epfile-101" && it.Status != "completed" {
			t.Errorf("expected epfile-101 completed, got %s", it.Status)
		}
		if it.ItemKey == "epfile-103" && it.Status != "running" {
			t.Errorf("expected epfile-103 running, got %s", it.Status)
		}
	}

	// Job 2 completes
	job2Status.Store(transcode.StatusCompleted)

	resumed2, err := engine.Resume(ctx, res.ID, "", nil)
	if err != nil {
		t.Fatalf("unexpected resume2 error: %v", err)
	}
	if resumed2.Status != StatusCompleted {
		t.Fatalf("expected batch completed when all jobs done, got %s", resumed2.Status)
	}
}

// 12. Waiting decision propagation and decision forwarding via resume
func TestTranscodeBatch_WaitingDecisionPropagationAndForwarding(t *testing.T) {
	var candPathCreated string
	mockExecutor := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			candPathCreated = req.CandidatePath
			// Write candidate file that is significantly larger than source (source is 1.4 GB = 1400000000 bytes)
			// Make candidate 2.0 GB so it exceeds max_size_increase_percent (default 0.0)
			if candPathCreated != "" {
				_ = os.MkdirAll(filepath.Dir(candPathCreated), 0755)
				f, _ := os.Create(candPathCreated)
				_ = f.Truncate(2000000000)
				_, _ = f.WriteAt([]byte("large-candidate-media-content"), 0)
				_ = f.Close()
			}
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:            jobID,
				Status:        transcode.StatusCompleted,
				CandidatePath: candPathCreated,
			}, nil
		},
	}

	engine, st, _, srv, _ := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()

	// Initial run: candidate is larger, max_size_increase_percent is forwarded (default 0.0)
	// Should enter waiting_decision!
	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 10,
		"season":    1,
		"dry_run":   false,
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Status != StatusWaitingDecision {
		t.Fatalf("expected status %s on size increase, got %s (reason: %s)", StatusWaitingDecision, res.Status, res.WaitingReason)
	}

	// Verify counts reflect waiting_decision
	counts, ok := res.Outputs["counts"].(map[string]int)
	if !ok || counts["waiting_decision"] != 1 {
		t.Errorf("expected counts.waiting_decision == 1, got %v", counts)
	}

	items, _ := st.ListTranscodeBatchItems(res.ID)
	if len(items) != 1 || items[0].Status != "waiting_decision" {
		t.Fatalf("expected item status waiting_decision, got %s", items[0].Status)
	}

	// Resume batch with "accept_loss" decision: should forward to child action and complete!
	resumed, err := engine.Resume(ctx, res.ID, "accept_loss", nil)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if resumed.Status != StatusCompleted {
		t.Fatalf("expected completed status after forwarding accept_loss, got %s (%s)", resumed.Status, resumed.Error)
	}

	itemsAfter, _ := st.ListTranscodeBatchItems(res.ID)
	if itemsAfter[0].Status != "completed" {
		t.Errorf("expected item status completed after accepting loss, got %s", itemsAfter[0].Status)
	}
}

// 13. Batch cancel stops already-admitted remote jobs instead of merely
// cancelling pending items and waiting for active encodes to finish.
func TestTranscodeBatch_CancelStopsActiveRemoteJobs(t *testing.T) {
	var jobStatus atomic.Value
	jobStatus.Store(transcode.StatusRunning)

	mockExecutor := &mockTranscodeExecutor{
		submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
			writeCandidateOutput(req.CandidatePath)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
			return transcode.JobStatus{
				ID:     jobID,
				Status: jobStatus.Load().(string),
			}, nil
		},
		cancelFunc: func(ctx context.Context, jobID string) error {
			jobStatus.Store(transcode.StatusCancelled)
			return nil
		},
	}

	// maxParallel = 1 so item 101 is admitted and item 103 stays queued.
	engine, st, _, srv, _ := setupBatchTestEnv(t, mockExecutor, 1)
	defer srv.Close()
	defer st.Close()

	ctx := context.Background()

	res, err := engine.Run(ctx, "transcode_batch", map[string]any{
		"service":   "sonarr",
		"series_id": 10,
		"dry_run":   false,
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Status != StatusWaitingExternal {
		t.Fatalf("expected waiting_external, got %s", res.Status)
	}

	cancelledRes, err := engine.Resume(ctx, res.ID, "cancel", nil)
	if err != nil {
		t.Fatalf("cancel resume failed: %v", err)
	}
	if cancelledRes.Status != StatusCompleted {
		t.Fatalf("expected batch cancellation to complete after stopping active remote job, got %s", cancelledRes.Status)
	}
	if atomic.LoadInt32(&mockExecutor.cancelCalls) != 1 {
		t.Fatalf("expected exactly one remote cancel for admitted job, got %d", mockExecutor.cancelCalls)
	}

	note, _ := cancelledRes.Outputs["note"].(string)
	if !strings.Contains(note, "already-admitted remote transcode jobs") {
		t.Errorf("expected note to confirm active remote jobs are stopped, got %q", note)
	}

	items, _ := st.ListTranscodeBatchItems(res.ID)
	for _, it := range items {
		if it.ItemKey == "epfile-103" {
			if it.Status != "failed" || !strings.Contains(it.Error, "cancelled") {
				t.Errorf("expected epfile-103 failed due to cancellation, got status=%s err=%s", it.Status, it.Error)
			}
		}
		if it.ItemKey == "epfile-101" {
			if it.Status != "failed" || !strings.Contains(it.Error, "cancelled") {
				t.Errorf("expected epfile-101 cancelled after remote worker stop, got status=%s err=%s", it.Status, it.Error)
			}
		}
	}
}

// 14. Output bounding strictly caps at MaxBatchOutputItems (100) and outputs include waiting_decision
func TestTranscodeBatch_OutputCap100AndWaitingDecision(t *testing.T) {
	// Build a synthetic list of 120 batch items
	var items []store.TranscodeBatchItem
	for i := 0; i < 120; i++ {
		st := "queued"
		if i == 0 {
			st = "waiting_decision"
		}
		items = append(items, store.TranscodeBatchItem{
			BatchID:  "batch-cap-test",
			ItemKey:  fmt.Sprintf("ep-%03d", i),
			FilePath: fmt.Sprintf("/media/ep-%03d.mkv", i),
			Status:   st,
		})
	}

	// Requesting max_items = 200 should be capped at MaxBatchOutputItems (100)
	outputs := buildBatchOutputs("batch-cap-test", items, "Cap Test", false, false, 200)

	totalItems := outputs["total_items"].(int)
	if totalItems != 120 {
		t.Errorf("expected total_items == 120, got %d", totalItems)
	}
	returnedItems := outputs["returned_items"].(int)
	if returnedItems != 100 {
		t.Errorf("expected returned_items strictly capped at 100, got %d", returnedItems)
	}
	itemsLimit := outputs["items_limit"].(int)
	if itemsLimit != 100 {
		t.Errorf("expected items_limit capped at 100, got %d", itemsLimit)
	}
	truncated := outputs["truncated"].(bool)
	if !truncated {
		t.Errorf("expected truncated == true")
	}

	// Verify waiting_decision count in counts map and top-level outputs
	counts := outputs["counts"].(map[string]int)
	if counts["waiting_decision"] != 1 {
		t.Errorf("expected counts.waiting_decision == 1, got %d", counts["waiting_decision"])
	}
	if outputs["waiting_decision"] != 1 {
		t.Errorf("expected top-level waiting_decision == 1, got %v", outputs["waiting_decision"])
	}
}

func TestTranscodeBatchUnadvertisedInputs(t *testing.T) {
	engine := NewEngine(EngineDeps{})
	tmpl, ok := engine.GetTemplate("transcode_batch")
	if !ok {
		t.Fatalf("transcode_batch template not found")
	}

	disallowed := []string{"idempotency_key", "surface_worker_busy", "limit"}
	for _, input := range disallowed {
		for _, opt := range tmpl.OptionalInputs {
			if opt == input {
				t.Errorf("expected %q not to be advertised in transcode_batch OptionalInputs", input)
			}
		}
		for _, req := range tmpl.RequiredInputs {
			if req == input {
				t.Errorf("expected %q not to be advertised in transcode_batch RequiredInputs", input)
			}
		}
	}

	// Verify that getMaxOutputItems ignores the removed "limit" alias but respects max_output_items and max_items
	if got := getMaxOutputItems(map[string]any{"limit": 10}); got != DefaultMaxBatchOutputItems {
		t.Errorf("expected limit alias to be ignored by getMaxOutputItems, got %d", got)
	}
	if got := getMaxOutputItems(map[string]any{"max_output_items": 12}); got != 12 {
		t.Errorf("expected max_output_items to return 12, got %d", got)
	}
	if got := getMaxOutputItems(map[string]any{"max_items": 15}); got != 15 {
		t.Errorf("expected max_items to return 15, got %d", got)
	}
}
