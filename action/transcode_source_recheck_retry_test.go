package action

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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
		{name: "a read that failed part way", stdout: cleanScan, stderr: "[matroska,webm @ 0x7f] Read error at pos. 4021 (0xfb5)\n", refusal: "could not clear the file: the demuxer's read of the file failed 1 time(s)", verdict: "unchecked"},
		{name: "a file that ends in the middle of an element", stdout: cleanScan, stderr: "[matroska,webm @ 0x7f] File ended prematurely at pos. 4021 (0xfb5)\n", refusal: "found the damage too: [matroska,webm] File ended prematurely", verdict: "damaged"},
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

// The decision itself, asked twice of the same failure: the verdict of the
// first read is reused for the second while it still holds.
func TestRecheckOnRetryReusesItsVerdictWhileItHolds(t *testing.T) {
	for _, tc := range []struct{ name, stderr string }{{"damaged", "[matroska,webm @ 0x7f] EBML header parsing failed\n"}, {"intact", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t, cleanScan, tc.stderr, 0, `echo call >> $FILE.calls`)
			e := f.failedOnDamage(t, workerDamageReport)
			inst, err := e.deps.Store.GetActionInstance(f.ec.InstanceID)
			if err != nil {
				t.Fatal(err)
			}
			first := e.recheckOnRetry(context.Background(), inst, f.ec)
			second := e.recheckOnRetry(context.Background(), inst, f.ec)
			if (first == nil) != (tc.stderr == "") || (second == nil) != (tc.stderr == "") {
				t.Fatalf("first=%v second=%v", first, second)
			}
			if first != nil && first.Error() != second.Error() {
				t.Fatalf("the refusal changed between asks:\n%v\n%v", first, second)
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

// A clean read of a file is a statement about that file: once the file is not
// the one it read, the verdict is read again instead of trusted.
func TestAnIntactVerdictIsReadAgainOnceTheFileChanged(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, `echo call >> $FILE.calls`)
	if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport); !overturned {
		t.Fatalf("the first read did not clear the file: %v", f.ec.State[sourceRecheckKey])
	}
	// The file is replaced by a longer one.
	if err := os.WriteFile(f.path, make([]byte, 65), 0o600); err != nil {
		t.Fatal(err)
	}
	if coordinatorFoundSourceIntact(f.ec.State, "job-1") {
		t.Fatal("a verdict about a file that changed was kept as intact")
	}
	_, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport)
	record, _ := f.ec.State[sourceRecheckKey].(map[string]any)
	if overturned || f.verdict() != "unchecked" || !strings.Contains(getString(record, "detail"), "not the one the job started with") {
		t.Fatalf("a changed file was cleared by an old verdict: %v", record)
	}
	// The new file was not read either: it is not the one the job recorded.
	calls, _ := os.ReadFile(f.path + ".calls")
	if n := strings.Count(string(calls), "call"); n != 1 {
		t.Fatalf("ffprobe ran %d times, want only the first read", n)
	}
}

// A job that recorded no id has no verdict: a record with no id belongs to no job.
func TestARecheckRecordWithoutAJobBelongsToNoJob(t *testing.T) {
	state := map[string]any{sourceRecheckKey: map[string]any{"verdict": "intact"}}
	if got := recheckVerdict(state, ""); got != "" {
		t.Fatalf("verdict = %q for a job with no id", got)
	}
	if coordinatorFoundSourceIntact(state, "") {
		t.Fatal("a record with no job was taken for a clean read")
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

// A client that gives up on its request while the file is being read (an MCP
// call times out long before a file of tens of gigabytes is read) does not
// throw the read away: it runs to its end and its verdict is kept.
func TestRetryReadsToTheEndWhenTheClientGivesUp(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, `touch $FILE.started; sleep 1`)
	e := f.failedOnDamage(t, workerDamageReport)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(f.path + ".started"); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	_, err := e.Retry(ctx, f.ec.InstanceID)
	if err != nil && strings.Contains(err.Error(), "this file is damaged") {
		t.Fatalf("a read the client abandoned was refused: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the client never gave up")
	}
	_, state := f.stored(t, e)
	if state["previous_job_id"] != "job-1" {
		t.Fatalf("the read did not finish and let the retry go on: %v", state)
	}
	history, _ := state["failure_history"].([]any)
	if len(history) == 0 || history[0].(map[string]any)["phase"] != "recheck" {
		t.Fatalf("history = %v", history)
	}
}

// waitForFile is whether path appeared within two seconds.
func waitForFile(path string) bool {
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// A second Retry that finds the first one reading waits only as long as its own
// client does: it is not queued behind a read nobody asked it to repeat.
func TestRetryBehindARunningReadWaitsOnlyAsLongAsItsClient(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, `touch $FILE.started; sleep 2`)
	e := f.failedOnDamage(t, workerDamageReport)
	first := make(chan error, 1)
	go func() { _, err := e.Retry(context.Background(), f.ec.InstanceID); first <- err }()
	defer func() { <-first }()
	if !waitForFile(f.path + ".started") {
		t.Fatal("the first retry never began reading")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := e.Retry(ctx, f.ec.InstanceID)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(began) > time.Second {
		t.Fatalf("the second retry waited %v for the first one's read: %v", time.Since(began), err)
	}
}

// A read that outlives its client still ends with the service: shutting down
// does not wait for twenty minutes of reading, and nothing is stored from it.
func TestRetryReadEndsWithTheService(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, `touch $FILE.started; exec sleep 5`)
	e := f.failedOnDamage(t, workerDamageReport)
	service, shutDown := context.WithCancel(context.Background())
	defer shutDown()
	e.lifecycle.Store(&service)
	request, giveUp := context.WithCancel(context.Background())
	defer giveUp()
	go func() {
		waitForFile(f.path + ".started")
		giveUp()
		shutDown()
	}()
	began := time.Now()
	_, err := e.Retry(request, f.ec.InstanceID)
	if err == nil || time.Since(began) > 3*time.Second {
		t.Fatalf("the read outlived the service (%v): %v", time.Since(began), err)
	}
	stored, state := f.stored(t, e)
	if stored.Status != StatusFailed || state[sourceRecheckKey] != nil {
		t.Fatalf("status=%s record=%v", stored.Status, state[sourceRecheckKey])
	}
}

// A file replaced after the job started is not damaged: what the worker said
// was about the file that was there.
func TestRetryOfAReplacedFileDoesNotCallItDamaged(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	f.ec.State["original_stat"] = map[string]any{"size_bytes": int64(9999), "mod_time": time.Now().UTC().Format(time.RFC3339Nano)}
	e := f.failedOnDamage(t, workerDamageReport)
	_, err := e.Retry(context.Background(), f.ec.InstanceID)
	if err == nil || !strings.Contains(err.Error(), "changed after this job started") || strings.Contains(err.Error(), "this file is damaged") {
		t.Fatalf("err = %v", err)
	}
}

// A file read clean and then gone or different has changed; one that cannot be
// reached at this moment has not been shown to.
func TestVerdictFileChangedNeedsAFileThatIsGoneOrDifferent(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport); !overturned {
		t.Fatalf("the file did not read clean: %v", f.ec.State[sourceRecheckKey])
	}
	if verdictFileChanged(f.ec.State, "job-1") {
		t.Fatal("a file as it was read counts as changed")
	}
	reachable := f.ec.State["resolved_path"]
	f.ec.State["resolved_path"] = f.path + "/inside" // a path under a file: not "does not exist"
	if verdictFileChanged(f.ec.State, "job-1") {
		t.Fatal("a file that cannot be reached counts as changed")
	}
	f.ec.State["resolved_path"] = reachable
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(f.path, later, later); err != nil {
		t.Fatal(err)
	}
	// The recorded modification time no longer matches the file's.
	if !verdictFileChanged(f.ec.State, "job-1") {
		t.Fatal("a file touched since the read does not count as changed")
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if !verdictFileChanged(f.ec.State, "job-1") {
		t.Fatal("a file removed since the read does not count as changed")
	}
}

// A read cut short by the lease or a shutdown (not by the client) decided
// nothing: nothing is stored, and the next Retry reads the file again.
func TestRecheckOnRetryCutShortKeepsTheJobAsItWas(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	e := f.failedOnDamage(t, workerDamageReport)
	inst, err := e.deps.Store.GetActionInstance(f.ec.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.recheckOnRetry(ctx, inst, f.ec); err == nil {
		t.Fatal("a cut short read let the retry go on")
	}
	stored, state := f.stored(t, e)
	if stored.Status != StatusFailed || state[sourceRecheckKey] != nil {
		t.Fatalf("status=%s record=%v", stored.Status, state[sourceRecheckKey])
	}
}

// A clean read is a statement about the file as the read saw it: a job whose
// file was only recorded by size cannot have the verdict carried to a file that
// was touched since.
func TestAnIntactVerdictIsBoundToTheFileTheReadSaw(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	delete(f.ec.State, "original_stat")
	if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport); !overturned || !coordinatorFoundSourceIntact(f.ec.State, "job-1") {
		t.Fatalf("the file did not read clean: %v", f.ec.State[sourceRecheckKey])
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(f.path, later, later); err != nil {
		t.Fatal(err)
	}
	if coordinatorFoundSourceIntact(f.ec.State, "job-1") {
		t.Fatal("a file touched since the read kept its verdict")
	}
	// An old record that never stored what it read is not trusted either.
	delete(f.ec.State[sourceRecheckKey].(map[string]any), "file_mod")
	if err := os.Chtimes(f.path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if coordinatorFoundSourceIntact(f.ec.State, "job-1") {
		t.Fatal("a verdict with no file to compare was trusted")
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
	unmeasured := state("", "")
	delete(unmeasured["original"].(map[string]any), "video_seconds")
	overturned := toJSON(map[string]any{"error": "Transcode failed (worker_source_read_fault): the worker reported the source file as damaged, but the coordinator read the same file end to end. The worker said: [worker read error] the source file is damaged: reading its video the demuxer reported 3 errors"})
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
		// Without the video's length a read that stopped early looks whole.
		{"a video whose length was never measured", scan, unmeasured, false},
		{"a failure the read already overturned", overturned, state("job-1", "intact"), false},
		{"another failure", `{"error":"worker unreachable"}`, state("", ""), false},
	} {
		if got := CanRecheckSource(tc.err, tc.state); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}
