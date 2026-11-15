package maintenanceui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSimulateQueueWaitHandsSlotsToWaitersInOrder(t *testing.T) {
	// Two slots: one frees in 100 s, one is idle. Three waiters; the second
	// has no known encode time, so the third gets no number.
	waiters := []queueWaiter{
		{id: "c", position: 3, encode: 50, known: true},
		{id: "a", position: 1, encode: 500, known: true},
		{id: "b", position: 2},
	}
	got := simulateQueueWait([]float64{100, 0}, waiters)
	if len(got) != 2 || got["a"] != 0 || got["b"] != 100 {
		t.Fatalf("waits %v", got)
	}
	if _, ok := got["c"]; ok {
		t.Fatal("a waiter behind an unknown encode must have no estimate")
	}
	// With every encode known the freed slots chain: a takes the idle slot
	// (0..500), b the slot freeing at 100 (100..130), c the one freeing at 130.
	got = simulateQueueWait([]float64{100, 0}, []queueWaiter{
		{id: "a", position: 1, encode: 500, known: true},
		{id: "b", position: 2, encode: 30, known: true},
		{id: "c", position: 3, encode: 50, known: true},
	})
	if got["a"] != 0 || got["b"] != 100 || got["c"] != 130 {
		t.Fatalf("waits %v", got)
	}
	if len(simulateQueueWait(nil, waiters)) != 0 {
		t.Fatal("no slots, no estimate")
	}
}

// transcodeSeed is the state of a transcode job in the shape the engine saves.
func transcodeSeed(worker string, resolution string, duration float64, extra map[string]any) map[string]any {
	state := map[string]any{
		"worker":      worker,
		"original":    map[string]any{"size_bytes": 1 << 30, "duration_sec": duration, "resolution": resolution, "audio": []any{map[string]any{"codec": "eac3"}}},
		"size_budget": map[string]any{"budget_bytes": 1 << 29, "encoder_choice": map[string]any{"video_codec": "libx265", "reason": "hardware_off", "message": "x265"}},
	}
	for k, v := range extra {
		state[k] = v
	}
	return state
}

func TestOperationsReportsTheQueueWaitFromTheTrimmedSnapshot(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	// History: two finished 4K x265 encodes at realtime, one 1080p at 2x.
	for _, id := range []string{"done-1", "done-2"} {
		seedOperation(t, st, id, "transcode_media", "completed", map[string]any{"path": "/media/" + id + ".mkv"}, nil, transcodeSeed("node-a", "3840x2160", 3600, map[string]any{"encode_duration_ms": 3600000}))
	}
	// One slot, busy with a job half done and 750 s from finishing.
	measured := time.Now().UTC().Add(-2 * time.Second).Format(time.RFC3339Nano)
	seedOperation(t, st, "busy", "transcode_media", "waiting_external", map[string]any{"path": "/media/busy.mkv"}, nil,
		transcodeSeed("node-a", "3840x2160", 3000, map[string]any{"transcode_phase": "encoding", "progress": 50, "speed": 2, "last_progress_at": measured, "worker_slots_total": 1}))
	waiting := func(position int) map[string]any {
		return map[string]any{"work_progress": map[string]any{"phase": "waiting_for_worker", "queue_position": position}}
	}
	seedOperation(t, st, "first", "transcode_media", "waiting_external", map[string]any{"path": "/media/first.mkv"}, nil, transcodeSeed("node-a", "3840x2160", 3600, waiting(1)))
	// No 1080p history: its own encode time is unknown, so the file behind it has no estimate.
	seedOperation(t, st, "second", "transcode_media", "waiting_external", map[string]any{"path": "/media/second.mkv"}, nil, transcodeSeed("node-a", "1920x1080", 3600, waiting(2)))
	seedOperation(t, st, "third", "transcode_media", "waiting_external", map[string]any{"path": "/media/third.mkv"}, nil, transcodeSeed("node-a", "3840x2160", 3600, waiting(3)))

	wait := func(id string) (any, bool) {
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		var page struct {
			Jobs []struct {
				Savings map[string]any `json:"savings"`
			} `json:"jobs"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(id, w.Code, w.Body.String())
		}
		v, ok := page.Jobs[0].Savings["queue_wait_seconds"]
		return v, ok
	}
	// first waits for the busy job's 750 s; second for that plus first's 3600 s.
	if v, ok := wait("first"); !ok || v != float64(750) {
		t.Fatal("first", v, ok)
	}
	if v, ok := wait("second"); !ok || v != float64(4350) {
		t.Fatal("second", v, ok)
	}
	for _, id := range []string{"third", "busy", "done-1"} {
		if v, ok := wait(id); ok {
			t.Fatal(fmt.Sprintf("%s must have no queue wait, got %v", id, v))
		}
	}
}

func TestOperationsGivesNoQueueWaitWithoutHistoryOrSlots(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	measured := time.Now().UTC().Add(-2 * time.Second).Format(time.RFC3339Nano)
	// One finished 4K encode is too little history; the busy job has no slot count either.
	seedOperation(t, st, "done", "transcode_media", "completed", nil, nil, transcodeSeed("node-a", "3840x2160", 3600, map[string]any{"encode_duration_ms": 3600000}))
	seedOperation(t, st, "busy", "transcode_media", "waiting_external", nil, nil,
		transcodeSeed("node-a", "3840x2160", 3000, map[string]any{"transcode_phase": "encoding", "progress": 50, "speed": 2, "last_progress_at": measured}))
	seedOperation(t, st, "waiter", "transcode_media", "waiting_external", nil, nil,
		transcodeSeed("node-a", "3840x2160", 3600, map[string]any{"work_progress": map[string]any{"phase": "waiting_for_worker", "queue_position": 1}}))
	w := request(h, "GET", "/api/maintenance/operations?id=waiter", "", true)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &struct{}{}) != nil || strings.Contains(w.Body.String(), "queue_wait_seconds") {
		t.Fatal("an estimate without its inputs", w.Code, w.Body.String())
	}
}
