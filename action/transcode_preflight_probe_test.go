package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
)

func preflightProbeFixture(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "S01E01.mkv")
	if err := os.WriteFile(file, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, file, filepath.Join(dir, "ffprobe")
}

func shortProbeBackoff(t *testing.T) {
	t.Helper()
	old := preflightProbeBackoff
	preflightProbeBackoff = time.Millisecond
	t.Cleanup(func() { preflightProbeBackoff = old })
}

func runPreflight(t *testing.T, dir, file, ffprobe string) StepResult {
	t.Helper()
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, ffprobe, []string{dir}, []string{dir}, false)
	t.Cleanup(func() { st.Close() })
	ec := &ExecutionContext{InstanceID: "probe-retry", ActionName: "transcode_media", Inputs: map[string]any{"path": file, "mode": "quality"}, State: map[string]any{}}
	seedActionInstance(t, st, ec.InstanceID, ec.ActionName, StatusRunning, 0, "", ec.Inputs, ec.State)
	result, err := e.stepTranscodePreflight(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPreflightRetriesProbeTimeoutOnceAndNamesKind(t *testing.T) {
	shortProbeBackoff(t)
	dir, file, ffprobe := preflightProbeFixture(t)
	var calls int32
	old := preflightInspect
	preflightInspect = func(context.Context, string, string) (mediainspect.DetailedReport, error) {
		atomic.AddInt32(&calls, 1)
		pe := &mediainspect.ProbeError{Kind: mediainspect.ProbeTimeout, Err: errors.New("no result within 60s"), Elapsed: 60 * time.Second}
		return mediainspect.DetailedReport{ProbeFailure: pe}, pe
	}
	t.Cleanup(func() { preflightInspect = old })

	result := runPreflight(t, dir, file, ffprobe)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("inspections=%d, want exactly one retry (2)", got)
	}
	if result.Status != StepFailed {
		t.Fatalf("an uninspected source must fail closed, got %s", result.Status)
	}
	for _, want := range []string{"timeout", "no result within 60s", "after 2 attempts", "fail closed"} {
		if !strings.Contains(result.Error, want) {
			t.Fatalf("error %q lacks %q", result.Error, want)
		}
	}
	if strings.Contains(result.Error, "did not produce a trustworthy") {
		t.Fatalf("the real cause is hidden behind the generic message: %s", result.Error)
	}
}

func TestPreflightProbeRecoversOnRetry(t *testing.T) {
	shortProbeBackoff(t)
	dir, file, ffprobe := preflightProbeFixture(t)
	script := `#!/bin/sh
cat <<'JSON'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080,"avg_frame_rate":"24/1","r_frame_rate":"24/1","bits_per_raw_sample":"8"}],"format":{"format_name":"matroska","duration":"100","size":"100"},"chapters":[]}
JSON
`
	if err := os.WriteFile(ffprobe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var calls int32
	real := mediainspect.InspectDetailed
	old := preflightInspect
	preflightInspect = func(ctx context.Context, p, path string) (mediainspect.DetailedReport, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			pe := &mediainspect.ProbeError{Kind: mediainspect.ProbeTimeout, Err: errors.New("stalled read")}
			return mediainspect.DetailedReport{ProbeFailure: pe}, pe
		}
		return real(ctx, p, path)
	}
	t.Cleanup(func() { preflightInspect = old })

	result := runPreflight(t, dir, file, ffprobe)
	if result.Status != StepCompleted {
		t.Fatalf("a probe that works on the second attempt must pass: %s %s", result.Status, result.Error)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("inspections=%d", got)
	}
}

func TestPreflightDoesNotRetryDeterministicProbeFailures(t *testing.T) {
	shortProbeBackoff(t)
	for _, tc := range []struct {
		name string
		err  *mediainspect.ProbeError
		want string
	}{
		{"demuxer rejection", &mediainspect.ProbeError{Kind: mediainspect.ProbeToolExit, Err: errors.New("exit status 1"), Stderr: "moov atom not found"}, "moov atom not found"},
		{"bad output", &mediainspect.ProbeError{Kind: mediainspect.ProbeBadOutput, Err: errors.New("unexpected end of JSON input")}, "bad_output"},
		{"binary missing", &mediainspect.ProbeError{Kind: mediainspect.ProbeUnavailable, Err: errors.New("ffprobe not found")}, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, file, ffprobe := preflightProbeFixture(t)
			var calls int32
			old := preflightInspect
			preflightInspect = func(context.Context, string, string) (mediainspect.DetailedReport, error) {
				atomic.AddInt32(&calls, 1)
				return mediainspect.DetailedReport{ProbeFailure: tc.err}, tc.err
			}
			t.Cleanup(func() { preflightInspect = old })
			result := runPreflight(t, dir, file, ffprobe)
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("inspections=%d, want 1", got)
			}
			if result.Status != StepFailed || !strings.Contains(result.Error, tc.want) || strings.Contains(result.Error, "attempts") {
				t.Fatalf("status=%s error=%q", result.Status, result.Error)
			}
		})
	}
}

func TestPreflightRetriesStorageIOErrorFromRealProbe(t *testing.T) {
	shortProbeBackoff(t)
	dir, file, ffprobe := preflightProbeFixture(t)
	count := filepath.Join(dir, "count")
	script := "#!/bin/sh\necho x >> '" + count + "'\necho 'S01E01.mkv: Input/output error' >&2\nexit 1\n"
	if err := os.WriteFile(ffprobe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	result := runPreflight(t, dir, file, ffprobe)
	data, _ := os.ReadFile(count)
	if n := strings.Count(string(data), "x"); n != 2 {
		t.Fatalf("ffprobe ran %d times, want 2", n)
	}
	if result.Status != StepFailed || !strings.Contains(result.Error, "tool_exit") || !strings.Contains(result.Error, "Input/output error") {
		t.Fatalf("status=%s error=%q", result.Status, result.Error)
	}
	// The class is the probe's, whatever the stderr says.
	if got, _ := result.Outputs["failure_classification"].(string); got != "media_probe_failed" {
		t.Fatalf("failure_classification=%q", got)
	}
}

// A caller who gives up while preflight waits to look again ends the wait at
// once: no second inspection, no sleeping out the backoff, and the probe's
// typed cause is still what comes back.
func TestPreflightProbeRetryStopsWhenTheCallerCancels(t *testing.T) {
	old, oldBackoff := preflightInspect, preflightProbeBackoff
	preflightProbeBackoff = time.Hour
	t.Cleanup(func() { preflightInspect, preflightProbeBackoff = old, oldBackoff })
	transient := func() (mediainspect.DetailedReport, error) {
		pe := &mediainspect.ProbeError{Kind: mediainspect.ProbeTimeout, Err: errors.New("stalled read")}
		return mediainspect.DetailedReport{ProbeFailure: pe}, pe
	}
	for _, when := range []string{"during the probe", "during the backoff"} {
		t.Run(when, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls int32
			preflightInspect = func(context.Context, string, string) (mediainspect.DetailedReport, error) {
				atomic.AddInt32(&calls, 1)
				if when == "during the probe" {
					cancel()
				} else {
					go func() { time.Sleep(20 * time.Millisecond); cancel() }()
				}
				return transient()
			}
			e := NewEngine(EngineDeps{})
			ec := &ExecutionContext{State: map[string]any{}}
			done := make(chan error, 1)
			go func() {
				_, err := e.inspectSourceForPreflight(ctx, ec, "/media/clip.mkv")
				done <- err
			}()
			select {
			case err := <-done:
				if _, typed := mediainspect.AsProbeError(err); !typed {
					t.Fatalf("the probe's cause was lost: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("preflight slept out its backoff after the caller cancelled")
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("inspections=%d, want 1", got)
			}
		})
	}
}
