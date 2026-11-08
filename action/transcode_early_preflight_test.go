package action

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

var ordinaryProbeJSON = strings.Replace(unconvertibleSubtitleProbeJSON, "made_up_sub", "subrip", 1)

const unconvertibleSubtitleProbeJSON = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "profile": "High", "pix_fmt": "yuv420p", "bits_per_raw_sample": "8", "r_frame_rate": "24000/1001", "avg_frame_rate": "24000/1001", "field_order": "progressive", "width": 1920, "height": 1080, "bit_rate": "5000000"},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "jpn"}},
    {"index": 2, "codec_type": "subtitle", "codec_name": "made_up_sub", "tags": {"language": "eng"}}
  ],
  "format": {"format_name": "matroska", "duration": "1200.0", "size": "750000000", "bit_rate": "5000000"},
  "chapters": []
}`

func earlyCheckEC(t *testing.T, e *Engine, id string, inputs map[string]any) *ExecutionContext {
	t.Helper()
	seedActionInstance(t, e.deps.Store, id, "transcode_media", StatusRunning, 0, "", inputs, map[string]any{})
	return parseExecutionContextMust(t, e, id)
}

// The early check of a conversion waiting for a worker decides it only when
// no worker could change the answer, and asks no worker anything.
func TestEarlyPreflightDecidesOnlyWhatNoWorkerChanges(t *testing.T) {
	var caps int32
	mock := func() *mockTranscodeExecutor {
		return &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
			atomic.AddInt32(&caps, 1)
			return transcode.WorkerCapabilities{}, nil
		}}
	}

	t.Run("a subtitle no recipe converts fails now", func(t *testing.T) {
		m := mock()
		e, _, media, _ := setupProbeTestEnv(t, m, unconvertibleSubtitleProbeJSON)
		ec := earlyCheckEC(t, e, "sub", map[string]any{"path": media, "mode": "size"})
		res, decided := e.earlyPreflight(context.Background(), ec)
		if !decided || res.Status != StepFailed || !strings.Contains(res.Error, "unsupported subtitle codec") {
			t.Fatalf("decided=%v %+v", decided, res)
		}
		if m.submitCalls != 0 {
			t.Fatal("the early check submitted work")
		}
	})

	t.Run("an HDR file its goal skips is skipped now and needs no worker", func(t *testing.T) {
		e, _, media, _ := setupProbeTestEnv(t, mock(), hdr10ProbeJSON)
		ec := earlyCheckEC(t, e, "hdr", map[string]any{"path": media, "mode": "quality"})
		res, decided := e.earlyPreflight(context.Background(), ec)
		if !decided || res.Status != StepCompleted || !getBool(ec.State, "skip_transcode") || getString(ec.State, "reason_code") != "hdr_requires_compact" {
			t.Fatalf("decided=%v %+v state=%v", decided, res, ec.State)
		}
		if _, waiting := e.ensureWorkerAssigned(context.Background(), ec); waiting {
			t.Fatal("a skipped conversion still waits for a worker")
		}
	})

	t.Run("an ordinary file keeps waiting with its state untouched, checked once", func(t *testing.T) {
		e, _, media, _ := setupProbeTestEnv(t, mock(), ordinaryProbeJSON)
		ec := earlyCheckEC(t, e, "ok", map[string]any{"path": media, "mode": "size"})
		if _, decided := e.earlyPreflight(context.Background(), ec); decided {
			t.Fatal("an ordinary file was decided early")
		}
		for _, key := range []string{"plan", "resolved_path", "original_sha256", "original", "skip_transcode"} {
			if _, ok := ec.State[key]; ok {
				t.Fatalf("the early check left %s in the action's state", key)
			}
		}
		if !getBool(ec.State, earlyPreflightKey) {
			t.Fatal("the early check is not marked as done")
		}
		// The next poll does not read the file again.
		if _, decided := e.earlyPreflight(context.Background(), ec); decided {
			t.Fatal("decided on the second poll")
		}
	})

	if n := atomic.LoadInt32(&caps); n != 0 {
		t.Fatalf("the early check read worker capabilities %d times", n)
	}
}

func TestEarlyRejectionLeavesPassingFailuresWaiting(t *testing.T) {
	for _, msg := range []string{
		"failed to probe original media /m/a.mkv (fail closed): timeout",
		"path \"/m/a.mkv\" is outside allowed read roots: x",
		"waiting for worker capabilities",
	} {
		if earlyRejection(msg) {
			t.Errorf("decided early: %q", msg)
		}
	}
	if !earlyRejection(`resolving transcode profile "mode-size": unsupported subtitle codec "x"`) {
		t.Error("a recipe rejection is not decided early")
	}
}

// Through the engine, with every worker slot taken: a file its goal can never
// take ends at once, a file its goal skips finishes without a worker, and an
// ordinary file keeps waiting for one.
func TestAFullQueueStillEndsWhatNoWorkerCanTake(t *testing.T) {
	for _, tc := range []struct {
		name, probe, mode string
		want              string
	}{
		{"unconvertible subtitle", unconvertibleSubtitleProbeJSON, "size", StatusFailed},
		{"HDR its goal skips", hdr10ProbeJSON, "quality", StatusCompleted},
		{"ordinary file", ordinaryProbeJSON, "size", StatusWaitingExternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockTranscodeExecutor{}
			e, _, media, _ := setupProbeTestEnv(t, mock, tc.probe)
			worker := &slotReportingExecutor{mockTranscodeExecutor: mock, slots: sharedSlots(1)}
			set, err := transcode.NewWorkerSet([]transcode.NamedWorker{{Name: "m1", Executor: worker, MaxParallelJobs: 1, Podcast: true}})
			if err != nil {
				t.Fatal(err)
			}
			e.deps.Workers = set
			if _, _, waiting := assignNew(t, e, "holder", "transcode_media"); waiting {
				t.Fatal("the holder got no slot")
			}

			res, err := e.Run(context.Background(), "transcode_media", map[string]any{"path": media, "mode": tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != tc.want {
				t.Fatalf("status %s, want %s: %+v", res.Status, tc.want, res)
			}
			if n := atomic.LoadInt32(&mock.submitCalls); n != 0 {
				t.Fatalf("submitted %d jobs with every slot taken", n)
			}
		})
	}
}
