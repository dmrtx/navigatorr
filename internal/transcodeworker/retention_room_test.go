package transcodeworker

// The room a worker has left to retain candidates, which it reports in its
// health so the coordinator keeps from giving it a conversion it cannot hold
// (transcode.RetentionRoom).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetentionRoomReportsWhatIsLeftOfTheBudget(t *testing.T) {
	const budget = int64(1000)
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedCandidatesMaxBytes = budget })
	now := time.Now().UTC()

	f.w.refreshRetentionRoom(now)
	room := f.w.SchedulerObservation(now).Retention
	if room == nil || room.CapacityBytes != budget || room.RoomBytes != budget || !room.ObservedAt.Equal(now) {
		t.Fatalf("an empty worker reports %+v, want the whole budget %d", room, budget)
	}

	f.retain("job-a")
	held, _, err := f.w.retainedBytesLocked()
	if err != nil || held <= 0 {
		t.Fatalf("held = %d (%v), want the retained candidate", held, err)
	}
	later := now.Add(10 * time.Second)
	f.w.refreshRetentionRoom(later)
	room = f.w.SchedulerObservation(later).Retention
	if room == nil || room.CapacityBytes != budget || room.RoomBytes != budget-held || !room.ObservedAt.Equal(later) {
		t.Fatalf("with %d held the worker reports %+v, want room %d", held, room, budget-held)
	}
}

func TestRetentionRoomIsNeverNegative(t *testing.T) {
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedCandidatesMaxBytes = 1 << 20 })
	f.retain("job-a")
	// The budget was lowered under what the worker already holds.
	f.cfg.RetainedCandidatesMaxBytes = 1
	now := time.Now().UTC()
	f.w.refreshRetentionRoom(now)
	if room := f.w.SchedulerObservation(now).Retention; room == nil || room.CapacityBytes != 1 || room.RoomBytes != 0 {
		t.Fatalf("over budget reports %+v, want capacity 1 and no room", room)
	}
}

func TestRetentionRoomIsNotReportedWithoutABudgetOrDeferredPublication(t *testing.T) {
	for name, mutate := range map[string]func(*WorkerConfig){
		"no limit":                 func(c *WorkerConfig) { c.RetainedCandidatesMaxBytes = 0 },
		"deferred publication off": func(c *WorkerConfig) { c.RetainedCandidatesMaxBytes = 1000; c.DeferredPublication = false },
	} {
		t.Run(name, func(t *testing.T) {
			f := newRetainedFixture(t, mutate)
			now := time.Now().UTC()
			// A previous measure is dropped when the worker stops retaining.
			f.w.schedulerMu.Lock()
			f.w.retentionRoom = nil
			f.w.schedulerMu.Unlock()
			f.w.refreshRetentionRoom(now)
			if room := f.w.SchedulerObservation(now).Retention; room != nil {
				t.Fatalf("reported %+v, want none", room)
			}
			data, err := json.Marshal(f.w.SchedulerObservation(now))
			if err != nil || strings.Contains(string(data), "retention") {
				t.Fatalf("health carries a retention field (%v): %s", err, data)
			}
		})
	}
}

func TestRetentionRoomRidesOnTheSchedulerPassAndTheHealthPayload(t *testing.T) {
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedCandidatesMaxBytes = 1000 })
	f.w.schedulerSweepHook = func(string) error { return nil }
	f.w.runSchedulerSweeps(context.Background(), "", "")

	obs := f.w.SchedulerObservation(time.Now())
	if obs.Health != "ok" || obs.Retention == nil || obs.Retention.CapacityBytes != 1000 {
		t.Fatalf("after a scheduler pass: %+v", obs)
	}
	data, err := json.Marshal(obs)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Retention *struct {
			ObservedAt    time.Time `json:"observed_at"`
			CapacityBytes int64     `json:"capacity_bytes"`
			RoomBytes     int64     `json:"room_bytes"`
		} `json:"retention"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || wire.Retention == nil || wire.Retention.CapacityBytes != 1000 || wire.Retention.RoomBytes != 1000 || wire.Retention.ObservedAt.IsZero() {
		t.Fatalf("health payload %s (%v)", data, err)
	}
}

func TestAMeasureThatFailsKeepsThePreviousRoom(t *testing.T) {
	f := newRetainedFixture(t, func(c *WorkerConfig) { c.RetainedCandidatesMaxBytes = 1000 })
	now := time.Now().UTC()
	f.w.refreshRetentionRoom(now)

	// A retained job whose record cannot be read fails the measure closed.
	f.retain("job-a")
	if err := os.WriteFile(filepath.Join(f.hiddenDir("job-a"), "job.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.w.refreshRetentionRoom(now.Add(time.Minute))
	if room := f.w.SchedulerObservation(now.Add(time.Minute)).Retention; room == nil || !room.ObservedAt.Equal(now) || room.RoomBytes != 1000 {
		t.Fatalf("a failed measure replaced the last one: %+v", room)
	}
}
