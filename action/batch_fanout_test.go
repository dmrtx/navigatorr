package action

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode"
)

// slotReportingExecutor is a worker that reports its slots like the HTTP one.
type slotReportingExecutor struct {
	*mockTranscodeExecutor
	mu    sync.Mutex
	slots transcode.WorkerSlots
	err   error
	reads int
}

func (s *slotReportingExecutor) Slots(context.Context) (transcode.WorkerSlots, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.slots, s.err
}

func sharedSlots(ceiling int) transcode.WorkerSlots {
	return transcode.WorkerSlots{Ceiling: ceiling, Video: transcode.SlotPool{Total: ceiling}, Podcast: transcode.SlotPool{Total: ceiling}}
}

func TestBatchFanOutFollowsTheWorkerPoolUnderTheConfiguredCap(t *testing.T) {
	worker := &slotReportingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}, slots: sharedSlots(3)}
	fanOut := func(cap int) int {
		e := NewEngine(EngineDeps{Transcode: worker, Config: &config.Config{Transcode: config.TranscodeConfig{MaxParallelJobs: cap}}})
		return e.batchAvailableSlots(context.Background(), nil, 0)
	}
	if got := fanOut(0); got != 3 {
		t.Errorf("0 follows the worker's 3 parallel jobs: %d", got)
	}
	if got := fanOut(2); got != 2 {
		t.Errorf("a positive value caps the worker's 3: %d", got)
	}
	if got := fanOut(5); got != 3 {
		t.Errorf("a cap above the worker's pool changes nothing: %d", got)
	}
	// With a split only the video pool takes batch children.
	worker.slots = transcode.WorkerSlots{Ceiling: 3, Split: true, Video: transcode.SlotPool{Total: 1}, Podcast: transcode.SlotPool{Total: 2}}
	if got := fanOut(0); got != 1 {
		t.Errorf("split 1 video + 2 podcast: %d", got)
	}
	// A worker that cannot report its pool: the cap, else one at a time.
	worker.err = errors.New("worker does not support per-kind slots")
	if got := fanOut(0); got != 1 {
		t.Errorf("no pool and no cap: %d", got)
	}
	if got := fanOut(2); got != 2 {
		t.Errorf("no pool keeps the configured cap: %d", got)
	}
	e := NewEngine(EngineDeps{Transcode: &mockTranscodeExecutor{}, Config: &config.Config{Transcode: config.TranscodeConfig{MaxParallelJobs: 2}}})
	if got := e.batchAvailableSlots(context.Background(), nil, 0); got != 2 {
		t.Errorf("an executor without slots keeps the configured cap: %d", got)
	}
}

func TestBatchFanOutCachesTheWorkerPoolAndKeepsItThroughAFailedRead(t *testing.T) {
	worker := &slotReportingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{}, slots: sharedSlots(2)}
	e := NewEngine(EngineDeps{Transcode: worker, Config: &config.Config{}})
	ctx := context.Background()
	if got := e.batchAvailableSlots(ctx, nil, 0); got != 2 {
		t.Fatalf("first read: %d", got)
	}
	e.batchAvailableSlots(ctx, nil, 0)
	if worker.reads != 1 {
		t.Fatalf("each batch step must not ask the worker: %d reads", worker.reads)
	}
	worker.slots = sharedSlots(4)
	e.workerStateFor(transcode.DefaultWorkerName).pool.at = time.Now().Add(-time.Hour)
	if got := e.batchAvailableSlots(ctx, nil, 0); got != 4 {
		t.Fatalf("a raised ceiling is picked up once the cache expires: %d", got)
	}
	worker.err = errors.New("connection refused")
	e.workerStateFor(transcode.DefaultWorkerName).pool.at = time.Now().Add(-time.Hour)
	if got := e.batchAvailableSlots(ctx, nil, 0); got != 4 {
		t.Fatalf("an unreachable worker keeps the last pool it reported: %d", got)
	}
}

func TestTranscodeBatchFanOutFollowsTheWorker(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cap, workers int
		want         int32
	}{
		{"zero follows the worker", 0, 2, 2},
		{"a positive value caps it", 1, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var submits int32
			worker := &slotReportingExecutor{mockTranscodeExecutor: &mockTranscodeExecutor{
				submitFunc: func(ctx context.Context, req transcode.Request) (transcode.Job, error) {
					atomic.AddInt32(&submits, 1)
					writeCandidateOutput(req.CandidatePath)
					return transcode.Job{ID: req.ID}, nil
				},
				statusFunc: func(ctx context.Context, jobID string) (transcode.JobStatus, error) {
					return transcode.JobStatus{ID: jobID, Status: transcode.StatusRunning}, nil
				},
			}, slots: sharedSlots(tc.workers)}
			engine, st, _, srv, _ := setupBatchTestEnv(t, worker, tc.cap)
			defer srv.Close()
			defer st.Close()
			res, err := engine.Run(context.Background(), "transcode_batch", map[string]any{"service": "sonarr", "series_id": 10, "dry_run": false})
			if err != nil || res.Status != StatusWaitingExternal {
				t.Fatalf("run: %+v %v", res, err)
			}
			if got := atomic.LoadInt32(&submits); got != tc.want {
				t.Fatalf("children in flight: %d, want %d", got, tc.want)
			}
		})
	}
}
