package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRepeatedWaitingPollUpdatesOneRow(t *testing.T) {
	s := openTest(t)
	if err := s.CreateActionInstance(ActionInstance{ID: "a", ActionName: "transcode_batch"}); err != nil {
		t.Fatal(err)
	}
	log := func(index int, name, status, out string) {
		t.Helper()
		if err := s.LogActionStep(ActionStepLog{InstanceID: "a", StepIndex: index, StepName: name, Status: status, InputsJSON: `{"in":1}`, OutputsJSON: out, DurationMs: int64(len(out))}); err != nil {
			t.Fatal(err)
		}
	}
	log(1, "schedule", "waiting_external", `{"n":1}`)
	log(1, "schedule", "waiting_external", `{"n":2}`)
	log(1, "schedule", "waiting_external", `{"n":3}`)
	log(1, "schedule", "completed", `{"n":4}`)
	// The same step waiting again after another outcome is a new stretch.
	log(1, "schedule", "waiting_external", `{"n":5}`)
	log(1, "schedule", "waiting_external", `{"n":6}`)
	// A different step never merges, even with the same status.
	log(2, "promote", "waiting_external", `{"n":7}`)
	steps, err := s.GetActionSteps("a")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, step := range steps {
		got = append(got, fmt.Sprintf("%d/%s/%s/%d/%s", step.StepIndex, step.StepName, step.Status, step.Observations, step.OutputsJSON))
	}
	want := []string{
		`1/schedule/waiting_external/3/{"n":3}`,
		`1/schedule/completed/1/{"n":4}`,
		`1/schedule/waiting_external/2/{"n":6}`,
		`2/promote/waiting_external/1/{"n":7}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if steps[0].InputsJSON != `{"in":1}` {
		t.Fatalf("a repeated poll rewrote the inputs: %s", steps[0].InputsJSON)
	}
	_, total, err := s.GetActionStepSummaries("a", 25)
	if err != nil || total != 7 {
		t.Fatalf("total observations = %d, %v; want 7", total, err)
	}
}

// Rows written one per poll, as earlier versions did, are merged once old.
func TestCollapseOldPollsKeepsTheRecordOfEachWait(t *testing.T) {
	s := openTest(t)
	for _, id := range []string{"old", "busy"} {
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "transcode_batch"}); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(instance string, step int, status, created, out string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO action_steps_log (instance_id, step_index, step_name, primitive, outputs_json, status, created_at) VALUES (?,?,?,?,?,?,?)`,
			instance, step, "step", "step", out, status, created); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	// 250 polls (more than one page), then a failure, then two more polls of
	// the same step, one old and one recent.
	for i := 0; i < 250; i++ {
		insert("old", 0, "waiting_external", old, fmt.Sprintf(`{"n":%d}`, i))
	}
	insert("old", 0, "failed", old, `{"n":"failed"}`)
	insert("old", 0, "waiting_external", old, `{"n":"again"}`)
	insert("old", 0, "waiting_external", recent, `{"n":"live"}`)
	insert("old", 1, "waiting_external", old, `{"n":"lone"}`)
	for i := 0; i < 3; i++ {
		insert("busy", 0, "waiting_external", recent, `{"n":"busy"}`)
	}
	merged, err := s.CollapseOldPolls(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if merged != 249 {
		t.Fatalf("merged %d rows, want 249", merged)
	}
	rows := func(instance string) []string {
		steps, err := s.GetActionSteps(instance)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, step := range steps {
			out = append(out, fmt.Sprintf("%d/%s/%d/%s", step.StepIndex, step.Status, step.Observations, step.OutputsJSON))
		}
		return out
	}
	got := strings.Join(rows("old"), "\n")
	want := strings.Join([]string{
		`0/waiting_external/250/{"n":249}`,
		`0/failed/1/{"n":"failed"}`,
		`0/waiting_external/1/{"n":"again"}`,
		`0/waiting_external/1/{"n":"live"}`,
		`1/waiting_external/1/{"n":"lone"}`,
	}, "\n")
	if got != want {
		t.Fatalf("rows:\n%s\nwant:\n%s", got, want)
	}
	if got := len(rows("busy")); got != 3 {
		t.Fatalf("recent polls must stay whole: %d rows", got)
	}
	if _, total, err := s.GetActionStepSummaries("old", 25); err != nil || total != 254 {
		t.Fatalf("observations = %d, %v; want 254", total, err)
	}
	if again, err := s.CollapseOldPolls(context.Background(), time.Now().Add(-time.Hour)); err != nil || again != 0 {
		t.Fatalf("second pass merged %d, %v", again, err)
	}
}

func TestReclaimSpaceShrinksTheFile(t *testing.T) {
	s := openTest(t)
	if err := s.CreateActionInstance(ActionInstance{ID: "a", ActionName: "transcode_batch"}); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 1<<20)
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	for i := 0; i < 100; i++ {
		if _, err := s.db.Exec(`INSERT INTO action_steps_log (instance_id, step_index, step_name, primitive, outputs_json, status, created_at) VALUES ('a',0,'s','s',?, 'waiting_external', ?)`, payload, old); err != nil {
			t.Fatal(err)
		}
	}
	var path string
	if err := s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	size := func() int64 {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	before := size()
	if before < 100<<20 {
		t.Fatalf("setup: file is only %d bytes", before)
	}
	if _, err := s.CollapseOldPolls(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	freed, err := s.ReclaimSpace(context.Background())
	if err != nil || freed < 90<<20 {
		t.Fatalf("reclaimed %d bytes, %v", freed, err)
	}
	if after := size(); after > before/5 {
		t.Fatalf("file is %d bytes after reclaiming, was %d", after, before)
	}
	// Later deletes now return their pages without another VACUUM.
	var mode int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("auto_vacuum = %d, %v", mode, err)
	}
	if freed, err := s.ReclaimSpace(context.Background()); err != nil || freed != 0 {
		t.Fatalf("nothing left to reclaim, got %d, %v", freed, err)
	}
	if steps, err := s.GetActionSteps("a"); err != nil || len(steps) != 1 {
		t.Fatalf("the latest poll must remain: %d rows, %v", len(steps), err)
	}
}
