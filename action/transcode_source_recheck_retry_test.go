package action

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// failedOnDamage stores the fixture's job as failed on message, at the wait
// step, and returns its engine.
func (f *recheckFixture) failedOnDamage(t *testing.T, message string) *Engine {
	t.Helper()
	e := f.waitEngine(t, message)
	tmpl, ok := e.GetTemplate("transcode_media")
	if !ok {
		t.Fatal("no transcode_media template")
	}
	f.ec.InstanceID = "retry-me"
	f.ec.Inputs = map[string]any{"path": f.path}
	inst := store.ActionInstance{
		ID: f.ec.InstanceID, ActionName: "transcode_media", Status: StatusFailed, CurrentStep: actionStepIndex(tmpl, "wait_transcode"),
		InputsJSON: toJSON(f.ec.Inputs), StateJSON: toJSON(f.ec.State), OutputsJSON: "{}",
		ErrorJSON: toJSON(map[string]any{"step": "wait_transcode", "error": message}),
	}
	if err := e.deps.Store.CreateActionInstance(inst); err != nil {
		t.Fatal(err)
	}
	return e
}

// stored is the job as the store holds it now, and its state.
func (f *recheckFixture) stored(t *testing.T, e *Engine) (store.ActionInstance, map[string]any) {
	t.Helper()
	inst, err := e.deps.Store.GetActionInstance(f.ec.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{}
	if err := json.Unmarshal([]byte(inst.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	return *inst, state
}

// Retry of a job its worker blamed on the file reads the file here first, at
// the moment it is asked.
func TestRetryReadsAWorkersDamageReportAgain(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr string
		refusal              string // "" when the retry goes on
		verdict              string
	}{
		{name: "the file reads whole and clean", stdout: cleanScan, verdict: "intact"},
		{name: "the damage is there", stdout: cleanScan, stderr: "[matroska,webm @ 0x7f] EBML header parsing failed\n", refusal: "found the damage too: [matroska,webm] EBML header parsing failed", verdict: "damaged"},
		{name: "a read that failed part way", stdout: cleanScan, stderr: "[matroska,webm @ 0x7f] Read error at pos. 4021 (0xfb5)\n", refusal: "found the damage too", verdict: "damaged"},
		{name: "the read stopped early", stdout: "packet|pts_time=0.000000|duration_time=0.040000\nformat|format_name=matroska,webm\n", refusal: "could not clear the file: the read covered 0 of the video's 60 seconds", verdict: "unchecked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t, tc.stdout, tc.stderr, 0, "")
			e := f.failedOnDamage(t, workerDamageReport)
			_, err := e.Retry(context.Background(), f.ec.InstanceID)
			inst, state := f.stored(t, e)
			if tc.refusal != "" {
				record, _ := state[sourceRecheckKey].(map[string]any)
				if getString(record, "verdict") != tc.verdict || getString(record, "job_id") != "job-1" {
					t.Fatalf("record = %v", record)
				}
				if err == nil || !strings.Contains(err.Error(), "this file is damaged") || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("err = %v", err)
				}
				if inst.Status != StatusFailed || !strings.Contains(inst.ErrorJSON, "ffmpeg_input_corrupt") {
					t.Fatalf("a refused retry changed the job: %s %s", inst.Status, inst.ErrorJSON)
				}
				return
			}
			if err != nil && strings.Contains(err.Error(), "this file is damaged") {
				t.Fatalf("a file that reads clean was refused: %v", err)
			}
			// The retry went on as a new attempt of the same job, which starts
			// without the verdict of the one before.
			if state["previous_job_id"] != "job-1" || state[sourceRecheckKey] != nil {
				t.Fatalf("the retry did not go on: previous=%v record=%v", state["previous_job_id"], state[sourceRecheckKey])
			}
			history, _ := state["failure_history"].([]any)
			if len(history) != 2 {
				t.Fatalf("history = %v", history)
			}
			entry := history[0].(map[string]any)
			if entry["phase"] != "recheck" || entry["classification"] != string(resilience.WorkerSourceReadFault) || !strings.Contains(getString(entry, "error"), "EBML header parsing failed") {
				t.Fatalf("history = %v", history)
			}
		})
	}
}

// What was read once for a failure is not read again: a verdict of damage
// stands, and one of intact is reused.
func TestRetryDoesNotReadTheSameFailureTwice(t *testing.T) {
	for _, tc := range []struct{ name, stderr string }{{"damaged", "[matroska,webm @ 0x7f] EBML header parsing failed\n"}, {"intact", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t, cleanScan, tc.stderr, 0, `echo call >> $FILE.calls`)
			e := f.failedOnDamage(t, workerDamageReport)
			for i := 0; i < 2; i++ {
				_, _ = e.Retry(context.Background(), f.ec.InstanceID)
			}
			calls, err := os.ReadFile(f.path + ".calls")
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(calls), "call"); n != 1 {
				t.Fatalf("ffprobe ran %d times for one failure", n)
			}
		})
	}
}

// A damage claim no packet scan can answer is refused as it always was,
// without reading anything.
func TestRetryRefusesADamageClaimNoScanCanAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		forget        string // what the job never recorded
	}{
		{"a failure of no scan", "worker failed (ffmpeg_input_corrupt): ffmpeg_input_corrupt: invalid data found when processing input", ""},
		{"a preflight that never read the file", "failed to probe original media (fail closed): ffprobe tool_exit: moov atom not found", "original"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t, cleanScan, "", 0, `echo call >> $FILE.calls`)
			if tc.forget != "" {
				delete(f.ec.State, tc.forget)
			}
			e := f.failedOnDamage(t, tc.message)
			_, err := e.Retry(context.Background(), f.ec.InstanceID)
			if err == nil || !strings.HasPrefix(err.Error(), damagedFileRefusal) {
				t.Fatalf("err = %v", err)
			}
			if _, statErr := os.Stat(f.path + ".calls"); statErr == nil {
				t.Fatal("ffprobe ran for a claim no scan can answer")
			}
		})
	}
}

// A request cut short before the read finished decided nothing.
func TestRetryCutShortBeforeTheReadKeepsTheJobAsItWas(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	e := f.failedOnDamage(t, workerDamageReport)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Retry(ctx, f.ec.InstanceID); err == nil {
		t.Fatal("a cancelled retry went on")
	}
	inst, state := f.stored(t, e)
	if inst.Status != StatusFailed || state[sourceRecheckKey] != nil {
		t.Fatalf("status=%s record=%v", inst.Status, state[sourceRecheckKey])
	}
}

func TestCanRecheckSource(t *testing.T) {
	scan := toJSON(map[string]any{"error": workerDamageReport})
	// state is a job that recorded its file, with the verdict of a read of
	// job on top when there is one.
	state := func(job, verdict string) map[string]any {
		s := newRecheckFixture(t, cleanScan, "", 0, "").ec.State
		if job != "" {
			s[sourceRecheckKey] = map[string]any{"job_id": job, "verdict": verdict}
		}
		return s
	}
	unrecorded := state("", "")
	delete(unrecorded, "original")
	for _, tc := range []struct {
		name  string
		err   string
		state map[string]any
		want  bool
	}{
		{"a scan report never read here", scan, state("", ""), true},
		{"a read that could not judge", scan, state("job-1", "unchecked"), true},
		{"confirmed damaged here", scan, state("job-1", "damaged"), false},
		{"damaged for another attempt", scan, state("job-0", "damaged"), true},
		{"a bare stored message", workerDamageReport, state("", ""), true},
		{"a file the job never recorded", scan, unrecorded, false},
		{"another failure", `{"error":"worker unreachable"}`, state("", ""), false},
	} {
		if got := CanRecheckSource(tc.err, tc.state); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}
