package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

const workerDamageReport = "ffmpeg_input_corrupt: the source file is damaged: reading its video the demuxer reported 3 errors, the first 41% into the file ([matroska,webm] EBML header parsing failed); download or remux the file again, a retry fails the same way"

// recheckFixture is a source file and a fake ffprobe that answers the
// coordinator's packet scan of it.
type recheckFixture struct {
	e       *Engine
	ec      *ExecutionContext
	path    string
	ffprobe string
}

// A clean scan of a 60 second video.
const cleanScan = "packet|pts_time=0.000000|duration_time=0.040000\npacket|pts_time=59.960000|duration_time=0.040000\nformat|format_name=matroska,webm\n"

// newRecheckFixture's ffprobe prints stdout (packets and the format name) and
// stderr, then exits with exit. extra runs before it in the script.
func newRecheckFixture(t *testing.T, stdout, stderr string, exit int, extra string) *recheckFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Deleted Scenes.mkv")
	if err := os.WriteFile(path, make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &recheckFixture{path: path, ffprobe: filepath.Join(dir, "ffprobe")}
	f.script(t, stdout, stderr, exit, extra)
	f.e = NewEngine(EngineDeps{Ffprobe: f.ffprobe})
	f.ec = &ExecutionContext{InstanceID: "recheck", Outputs: map[string]any{}, State: map[string]any{
		"job_id":        "job-1",
		"resolved_path": path,
		"original": map[string]any{
			"path": path, "size_bytes": float64(64), "video_seconds": 60.0,
			"video": []any{map[string]any{"index": float64(0), "kind": "video"}},
		},
		"original_stat": map[string]any{"size_bytes": int64(64), "mod_time": fi.ModTime().UTC().Format(time.RFC3339Nano)},
	}}
	return f
}

// script replaces the fake ffprobe.
func (f *recheckFixture) script(t *testing.T, stdout, stderr string, exit int, extra string) {
	t.Helper()
	body := "#!/bin/sh\n" + strings.ReplaceAll(extra, "$FILE", `"`+f.path+`"`) +
		"\ncat <<'OUT'\n" + stdout + "OUT\ncat >&2 <<'ERR'\n" + stderr + "ERR\nexit " + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(f.ffprobe, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func (f *recheckFixture) original() map[string]any { return f.ec.State["original"].(map[string]any) }

func (f *recheckFixture) verdict() string {
	record, _ := f.ec.State[sourceRecheckKey].(map[string]any)
	return getString(record, "verdict")
}

func TestWorkerDamageReportOverturnedByACleanRead(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	class, msg, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport)
	if !overturned || class != string(resilience.WorkerSourceReadFault) {
		t.Fatalf("a file that reads clean must overturn the worker: %q %q %v", class, msg, overturned)
	}
	// The verdict must not read as a damaged file again, or Retry stays blocked.
	if SourceDamaged(msg) || SourceDamaged("Transcode failed ("+class+"): "+msg) {
		t.Fatalf("the verdict still reads as damage: %s", msg)
	}
	if !strings.Contains(msg, "intact") || !strings.Contains(msg, "EBML header parsing failed") || !strings.Contains(msg, "60 of 60 seconds") {
		t.Fatalf("the verdict lost its evidence: %s", msg)
	}
	record, _ := f.ec.State[sourceRecheckKey].(map[string]any)
	if record["verdict"] != "intact" || record["packets"] != 2 || record["job_id"] != "job-1" || !strings.Contains(getString(record, "worker_report"), "ffmpeg_input_corrupt") {
		t.Fatalf("record = %v", record)
	}
}

// Anything but a clean, complete read of the file the job started with leaves
// the worker's verdict.
func TestWorkerDamageReportStandsUnlessTheCoordinatorReadsTheFileCleanAndWhole(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr, extra string
		exit                        int
		mutate                      func(f *recheckFixture)
		verdict                     string
	}{
		{name: "the coordinator finds the damage too", stdout: cleanScan, stderr: "[matroska,webm @ 0x7f] EBML header parsing failed\n[matroska,webm @ 0x7f] Packet corrupt (stream = 0, dts = 42).\n", verdict: "damaged"},
		{name: "errors of another context are not the container's", stdout: cleanScan, stderr: "[h264 @ 0x7f] corrupt decoded frame\n", verdict: "intact"},
		{name: "the read fails", stderr: "Invalid data found when processing input\n", exit: 1, verdict: "unchecked"},
		{name: "the file changes while it is read", stdout: cleanScan, extra: `printf x >> $FILE`, verdict: "unchecked"},
		{name: "nothing to read", stdout: "format|format_name=matroska,webm\n", verdict: "unchecked"},
		// ffprobe takes a read error for the end of the file and exits 0.
		{name: "the read failed part way", stdout: cleanScan, stderr: "[matroska,webm @ 0x7f] Read error at pos. 4021 (0xfb5)\n", verdict: "unchecked"},
		{name: "the read ended early without a word", stdout: "packet|pts_time=0.000000|duration_time=0.040000\npacket|pts_time=29.960000|duration_time=0.040000\nformat|format_name=matroska,webm\n", verdict: "unchecked"},
		{name: "the video's length was never measured", stdout: cleanScan, mutate: func(f *recheckFixture) { delete(f.original(), "video_seconds") }, verdict: "unchecked"},
		{name: "the file is not the one the job started with", stdout: cleanScan, mutate: func(f *recheckFixture) {
			f.ec.State["original_stat"] = map[string]any{"size_bytes": int64(9999), "mod_time": time.Now().UTC().Format(time.RFC3339Nano)}
		}, verdict: "unchecked"},
		{name: "modified since the job started", stdout: cleanScan, mutate: func(f *recheckFixture) {
			f.ec.State["original_stat"] = map[string]any{"size_bytes": int64(64), "mod_time": "2020-01-01T00:00:00Z"}
		}, verdict: "unchecked"},
		{name: "nothing tells this file from another", stdout: cleanScan, mutate: func(f *recheckFixture) {
			delete(f.ec.State, "original_stat")
			delete(f.original(), "size_bytes")
		}, verdict: "unchecked"},
		{name: "only a size was recorded", stdout: cleanScan, mutate: func(f *recheckFixture) { delete(f.ec.State, "original_stat") }, verdict: "intact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t, tc.stdout, tc.stderr, tc.exit, tc.extra)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			_, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport)
			if want := tc.verdict == "intact"; overturned != want {
				t.Fatalf("overturned = %v, want %v (record %v)", overturned, want, f.ec.State[sourceRecheckKey])
			}
			if got := f.verdict(); got != tc.verdict {
				t.Fatalf("verdict = %q, want %q (record %v)", got, tc.verdict, f.ec.State[sourceRecheckKey])
			}
		})
	}
}

// A cover picture stored before the video is not the video: the scan reads the
// programme's stream.
func TestRecheckReadsTheProgrammeVideoNotACoverPicture(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, `case "$*" in *"-select_streams 1 "*) ;; *) echo "read stream" >&2; exit 1;; esac`)
	f.original()["video"] = []any{
		map[string]any{"index": float64(0), "kind": "video", "disposition": map[string]any{"attached_pic": float64(1)}},
		map[string]any{"index": float64(1), "kind": "video"},
	}
	if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport); !overturned {
		t.Fatalf("the programme video was not the stream scanned: %v", f.ec.State[sourceRecheckKey])
	}
	// A file with no programme video gives the scan nothing to read.
	f.ec.State[sourceRecheckKey] = nil
	f.original()["video"] = []any{map[string]any{"index": float64(0), "kind": "video", "disposition": map[string]any{"attached_pic": float64(1)}}}
	if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport); overturned || f.verdict() != "unchecked" {
		t.Fatalf("a file with only a cover picture was judged: %v", f.ec.State[sourceRecheckKey])
	}
}

// Only the worker's own scan of the file is answered by a packet scan: a
// failure that merely carries the class (an encode that hit corrupt input) is
// left alone, and so is anything that does not blame the file.
func TestOnlyScanReportsAreReread(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	for _, msg := range []string{
		"share is full",
		"candidate probe: moov atom not found",
		"Invalid data found when processing input",
		"ffmpeg_input_corrupt: ffmpeg exited with status 1: Invalid data found when processing input",
	} {
		if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, msg); overturned || recheckable(msg) {
			t.Fatalf("%q was re-read", msg)
		}
	}
	if _, ok := f.ec.State[sourceRecheckKey]; ok {
		t.Fatal("a failure that does not blame the file left a record")
	}
	if !recheckable(workerDamageReport) || !recheckable("failed to probe original media /media/a.mp4 (fail closed): moov atom not found") {
		t.Fatal("the worker's scan report and a missing index must be re-read")
	}
}

// Only a verdict is kept for a job: a read that could not judge is made again,
// and another job never inherits one.
func TestRecheckVerdictBelongsToItsJob(t *testing.T) {
	f := newRecheckFixture(t, "", "Invalid data found when processing input\n", 1, "")
	if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport); overturned || f.verdict() != "unchecked" {
		t.Fatalf("first read: %v", f.ec.State[sourceRecheckKey])
	}
	// The read that could not judge is made again, and clears the file.
	f.script(t, cleanScan, "", 0, "")
	if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport); !overturned {
		t.Fatalf("an unchecked read was kept: %v", f.ec.State[sourceRecheckKey])
	}
	// A verdict is not read twice.
	f.script(t, "", "", 1, "")
	if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport); !overturned {
		t.Fatal("a kept verdict was read again")
	}
	// Another job's failure is a different claim about the file.
	f.ec.State["job_id"] = "job-2"
	f.script(t, cleanScan, "[matroska,webm @ 0x7f] EBML header parsing failed\n", 0, "")
	if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport); overturned || f.verdict() != "damaged" {
		t.Fatalf("a later job inherited the verdict: %v", f.ec.State[sourceRecheckKey])
	}
	if coordinatorFoundSourceIntact(f.ec.State, "job-1") || coordinatorFoundSourceIntact(f.ec.State, "") {
		t.Fatal("a damaged verdict reads as intact")
	}
}

// The step a user sees: the worker fails the job as damaged, the coordinator
// reads the file, and the job ends with a class that allows running it again.
func TestWaitStepOverturnsADamageReportAndKeepsRetryOpen(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	e := f.waitEngine(t, workerDamageReport)
	res, err := e.stepTranscodeWait(context.Background(), f.ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepFailed || !strings.Contains(res.Error, "("+string(resilience.WorkerSourceReadFault)+")") {
		t.Fatalf("result = %+v", res)
	}
	if SourceDamaged(res.Error) {
		t.Fatalf("the failure still blocks every retry: %s", res.Error)
	}
	if res.Outputs["failure_classification"] != string(resilience.WorkerSourceReadFault) {
		t.Fatalf("outputs = %v", res.Outputs)
	}
	// The history keeps what the worker said, whole, and what became of it.
	history, _ := f.ec.State["failure_history"].([]any)
	if len(history) != 2 {
		t.Fatalf("history = %v", history)
	}
	first, second := history[0].(map[string]any), history[1].(map[string]any)
	if first["classification"] != "ffmpeg_input_corrupt" || first["error"] != workerDamageReport || second["classification"] != string(resilience.WorkerSourceReadFault) {
		t.Fatalf("history = %v", history)
	}
}

// The same step with a file that is damaged keeps the worker's verdict.
func TestWaitStepKeepsADamageReportTheCoordinatorConfirms(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "[matroska,webm @ 0x7f] EBML header parsing failed\n", 0, "")
	e := f.waitEngine(t, workerDamageReport)
	res, err := e.stepTranscodeWait(context.Background(), f.ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepFailed || !SourceDamaged(res.Error) || res.Outputs["failure_classification"] != string(resilience.FFmpegInputCorrupt) {
		t.Fatalf("a confirmed damaged file must stay damaged: %+v", res)
	}
}

// A read cut short by a restart or a timeout judged nothing: no verdict is
// kept, no failure is recorded, and the step reads the file again when it
// runs again.
func TestWaitStepCutShortRecordsNothing(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	ctx, cancel := context.WithCancel(context.Background())
	// The worker answers, then the coordinator is stopped before it reads.
	tc := &mockTranscodeExecutor{statusFunc: func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		cancel()
		return transcode.JobStatus{ID: jobID, Status: transcode.StatusFailed, Error: workerDamageReport, FailureClassification: string(resilience.FFmpegInputCorrupt)}, nil
	}}
	e, st := setupTranscodeEngine(t, tc, f.ffprobe, []string{filepath.Dir(f.path)}, []string{filepath.Dir(f.path)}, false)
	defer st.Close()
	e.deps.Ffprobe = f.ffprobe
	res, err := e.stepTranscodeWait(ctx, f.ec)
	if err == nil || res.Status == StepFailed {
		t.Fatalf("a cut short read failed the job: %+v %v", res, err)
	}
	if _, ok := f.ec.State[sourceRecheckKey]; ok {
		t.Fatalf("a verdict was kept: %v", f.ec.State[sourceRecheckKey])
	}
	if _, ok := f.ec.State["failure_history"]; ok {
		t.Fatalf("a failure was recorded: %v", f.ec.State["failure_history"])
	}
}

// A failure that carries the class without the worker's scan report is not
// something a packet scan can clear.
func TestWaitStepLeavesAnEncodeThatHitCorruptInputBlamed(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	e := f.waitEngine(t, "ffmpeg exited with status 1: Invalid data found when processing input")
	res, err := e.stepTranscodeWait(context.Background(), f.ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepFailed || res.Outputs["failure_classification"] != string(resilience.FFmpegInputCorrupt) {
		t.Fatalf("result = %+v", res)
	}
	if _, ok := f.ec.State[sourceRecheckKey]; ok {
		t.Fatal("an encode failure was re-read")
	}
}

// Running the job again: a worker that still says damaged is overruled only by
// the coordinator's own whole, clean read of that same job's file.
func TestManualRetryFollowsTheCoordinatorsRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		record    map[string]any
		wantRetry bool
	}{
		{"read clean", map[string]any{"job_id": "job-old", "verdict": "intact"}, true},
		{"read damaged", map[string]any{"job_id": "job-old", "verdict": "damaged"}, false},
		{"read could not judge", map[string]any{"job_id": "job-old", "verdict": "unchecked"}, false},
		{"another job's verdict", map[string]any{"job_id": "job-older", "verdict": "intact"}, false},
		{"never read", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := setupTestStore(t)
			mock := &mockTranscodeExecutor{
				statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
					return transcode.JobStatus{ID: id, Status: transcode.StatusFailed, FailureClassification: string(resilience.FFmpegInputCorrupt), Error: workerDamageReport}, nil
				},
			}
			e := NewEngine(EngineDeps{Store: st, Transcode: mock})
			ec := testReconcileEC("")
			ec.State["resolved_path"] = "/media/source/Deleted Scenes.mkv"
			ec.State["job_id"] = "job-old"
			if tc.record != nil {
				ec.State[sourceRecheckKey] = tc.record
			}
			inst := store.ActionInstance{ID: ec.InstanceID, ActionName: "transcode_media", Status: StatusFailed, CurrentStep: 2, StateJSON: toJSON(ec.State)}
			if err := st.CreateActionInstance(inst); err != nil {
				t.Fatal(err)
			}
			tmpl := ActionTemplate{Steps: []StepDefinition{{Name: "preflight"}, {Name: "submit_transcode"}, {Name: "wait"}}}
			resume, err := e.prepareRemoteRetry(context.Background(), &inst, ec, tmpl, 2)
			if tc.wantRetry {
				if err != nil || resume != 1 {
					t.Fatalf("retry refused: resume=%d err=%v", resume, err)
				}
				if _, kept := ec.State[sourceRecheckKey]; kept {
					t.Fatal("the next attempt inherited the verdict")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "retry blocked") || resume != 2 {
				t.Fatalf("retry allowed for a file nobody cleared: resume=%d err=%v", resume, err)
			}
		})
	}
	// The stored failure of an overturned job does not trip the guard of Retry.
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	_, msg, _ := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport)
	if SourceDamaged(`{"error":"Transcode failed (worker_source_read_fault): ` + msg + `"}`) {
		t.Fatal("the stored error blocks Retry")
	}
	if err := rejectUnchangedPermanentRetry(string(resilience.WorkerSourceReadFault)); err != nil {
		t.Fatalf("the new class must not be a permanent one: %v", err)
	}
}

// waitEngine is an engine whose worker reports the job failed with message,
// classified as damaged input, over the fixture's ffprobe.
func (f *recheckFixture) waitEngine(t *testing.T, message string) *Engine {
	t.Helper()
	tc := &mockTranscodeExecutor{statusFunc: func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: jobID, Status: transcode.StatusFailed, Error: message, FailureClassification: string(resilience.FFmpegInputCorrupt)}, nil
	}}
	e, st := setupTranscodeEngine(t, tc, f.ffprobe, []string{filepath.Dir(f.path)}, []string{filepath.Dir(f.path)}, false)
	t.Cleanup(func() { st.Close() })
	e.deps.Ffprobe = f.ffprobe
	return e
}
