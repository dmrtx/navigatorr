package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

const workerDamageReport = "ffmpeg_input_corrupt: the source file is damaged: reading its video the demuxer reported 3 errors, the first 41% into the file ([matroska,webm] EBML header parsing failed); download or remux the file again, a retry fails the same way"

// recheckFixture is a source file and a fake ffprobe that answers the
// coordinator's packet scan of it.
type recheckFixture struct {
	e    *Engine
	ec   *ExecutionContext
	path string
}

// newRecheckFixture's ffprobe prints packets and the format name on stdout and
// stderr on stderr, then exits with exit. extra runs before it in the script.
func newRecheckFixture(t *testing.T, stdout, stderr string, exit int, extra string) *recheckFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Deleted Scenes.mkv")
	if err := os.WriteFile(path, make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" + strings.ReplaceAll(extra, "$FILE", `"`+path+`"`) +
		"\ncat <<'OUT'\n" + stdout + "OUT\ncat >&2 <<'ERR'\n" + stderr + "ERR\nexit " + string(rune('0'+exit)) + "\n"
	ffprobe := filepath.Join(dir, "ffprobe")
	if err := os.WriteFile(ffprobe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return &recheckFixture{
		e:    NewEngine(EngineDeps{Ffprobe: ffprobe}),
		path: path,
		ec: &ExecutionContext{InstanceID: "recheck", Outputs: map[string]any{}, State: map[string]any{
			"resolved_path": path,
			"original":      map[string]any{"path": path, "video": []any{map[string]any{"index": float64(0)}}},
		}},
	}
}

const cleanScan = "packet|pts_time=0.000000\npacket|pts_time=0.041000\nformat|format_name=matroska,webm\n"

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
	if !strings.Contains(msg, "intact") || !strings.Contains(msg, "EBML header parsing failed") {
		t.Fatalf("the verdict lost its evidence: %s", msg)
	}
	record := stateObject(f.ec.State, sourceRecheckKey)
	if record["verdict"] != "intact" || record["packets"] != 2 || !strings.Contains(getString(record, "worker_report"), "ffmpeg_input_corrupt") {
		t.Fatalf("record = %v", record)
	}
}

// Anything but a clean read of an unchanged file leaves the worker's verdict.
func TestWorkerDamageReportStandsUnlessTheCoordinatorReadsTheFileClean(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr, extra string
		exit                        int
		verdict                     string
	}{
		{"the coordinator finds the damage too", cleanScan, "[matroska,webm @ 0x7f] EBML header parsing failed\n[matroska,webm @ 0x7f] Read error at pos. 4021 (0xfb5)\n", "", 0, "damaged"},
		{"errors of another context are not the container's", cleanScan, "[h264 @ 0x7f] corrupt decoded frame\n", "", 0, "intact"},
		{"the read fails", "", "Invalid data found when processing input\n", "", 1, "unchecked"},
		{"the file changes while it is read", cleanScan, "", `printf x >> $FILE`, 0, "unchecked"},
		{"nothing to read", "format|format_name=matroska,webm\n", "", "", 0, "unchecked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t, tc.stdout, tc.stderr, tc.exit, tc.extra)
			_, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, workerDamageReport)
			want := tc.verdict == "intact"
			if overturned != want {
				t.Fatalf("overturned = %v, want %v (record %v)", overturned, want, f.ec.State[sourceRecheckKey])
			}
			if got := getString(stateObject(f.ec.State, sourceRecheckKey), "verdict"); got != tc.verdict {
				t.Fatalf("verdict = %q, want %q", got, tc.verdict)
			}
		})
	}
}

func TestOnlyDamageReportsAreReread(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, "share is full"); overturned {
		t.Fatal("a failure that does not blame the file was re-read")
	}
	if _, ok := f.ec.State[sourceRecheckKey]; ok {
		t.Fatal("a failure that does not blame the file left a record")
	}
	// A candidate's missing moov atom is the encode's failure, not the file's.
	if _, _, overturned := f.e.recheckDamagedSource(context.Background(), f.ec, "candidate probe: moov atom not found"); overturned {
		t.Fatal("a candidate failure was re-read")
	}
}

// The step a user sees: the worker fails the job as damaged, the coordinator
// reads the file, and the job ends with a class that allows running it again.
func TestWaitStepOverturnsADamageReportAndKeepsRetryOpen(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "", 0, "")
	tc := &mockTranscodeExecutor{statusFunc: func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: jobID, Status: transcode.StatusFailed, Error: workerDamageReport, FailureClassification: string(resilience.FFmpegInputCorrupt)}, nil
	}}
	e, st := setupTranscodeEngine(t, tc, f.e.deps.Ffprobe, []string{filepath.Dir(f.path)}, []string{filepath.Dir(f.path)}, false)
	defer st.Close()
	e.deps.Ffprobe = f.e.deps.Ffprobe
	f.ec.State["job_id"] = "job-1"
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
	if err := rejectUnchangedPermanentRetry(string(resilience.WorkerSourceReadFault)); err != nil {
		t.Fatalf("the new class must not be a permanent one: %v", err)
	}
}

// The same step with a file that is damaged keeps the worker's verdict.
func TestWaitStepKeepsADamageReportTheCoordinatorConfirms(t *testing.T) {
	f := newRecheckFixture(t, cleanScan, "[matroska,webm @ 0x7f] EBML header parsing failed\n", 0, "")
	tc := &mockTranscodeExecutor{statusFunc: func(_ context.Context, jobID string) (transcode.JobStatus, error) {
		return transcode.JobStatus{ID: jobID, Status: transcode.StatusFailed, Error: workerDamageReport, FailureClassification: string(resilience.FFmpegInputCorrupt)}, nil
	}}
	e, st := setupTranscodeEngine(t, tc, f.e.deps.Ffprobe, []string{filepath.Dir(f.path)}, []string{filepath.Dir(f.path)}, false)
	defer st.Close()
	e.deps.Ffprobe = f.e.deps.Ffprobe
	f.ec.State["job_id"] = "job-2"
	res, err := e.stepTranscodeWait(context.Background(), f.ec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StepFailed || !SourceDamaged(res.Error) || res.Outputs["failure_classification"] != string(resilience.FFmpegInputCorrupt) {
		t.Fatalf("a confirmed damaged file must stay damaged: %+v", res)
	}
}
