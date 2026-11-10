package action

import (
	"context"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// The coordinator applies the worker's pixel-format admission to the resolved
// mode plan: a source beyond 4:2:0 needs a plan that pins a 4:2:0 output, and
// a picture no class covers (RGB, 4:1:1, a format the probe could not name)
// is never admitted. The refusal is the worker's, word for word.
func TestModeSourceChromaErrorMatchesTheWorker(t *testing.T) {
	pinned := &transcode.Plan{Mode: "size", PixelFormat: "yuv420p"}
	pinned10 := &transcode.Plan{Mode: "quality", PixelFormat: "p010le"}
	open := &transcode.Plan{Mode: "size"}
	audioOnly := &transcode.Plan{Mode: "size", ModePolicyVersion: transcode.ModePolicyV7, VideoCodec: transcode.VideoCodecCopy}
	noMode := &transcode.Plan{}
	refusal := transcode.SourceClassUnsupportedError().Error()
	for _, c := range []struct {
		name   string
		plan   *transcode.Plan
		pixFmt string
		depth  int
		main10 bool
		refuse bool
	}{
		{"4:2:0 on an open plan", open, "yuv420p", 8, false, false},
		{"full-range 4:2:0", open, "yuvj420p", 8, false, false},
		{"10-bit 4:2:0", open, "yuv420p10le", 10, false, false},
		{"4:4:4 on a pinned plan", pinned, "yuv444p", 8, false, false},
		{"4:2:2 on a pinned plan", pinned, "yuv422p", 8, false, false},
		{"10-bit 4:4:4 on a pinned plan", pinned10, "yuv444p10le", 10, false, false},
		{"10-bit 4:2:2 on a pinned plan", pinned10, "yuv422p10le", 10, false, false},
		{"4:4:4 on an open plan", open, "yuv444p", 8, false, true},
		{"4:2:2 on an open plan", open, "yuv422p", 8, false, true},
		{"10-bit 4:2:2 on an open plan", open, "yuv422p10le", 10, false, true},
		{"4:2:2 an early Main10 switch would pin", open, "yuv422p", 8, true, false},
		{"unknown pixel format", pinned, "", 8, false, true},
		{"unknown pixel format an early Main10 switch cannot admit", open, "", 8, true, true},
		{"RGB", pinned, "gbrp", 8, false, true},
		{"4:1:1", pinned, "yuv411p", 8, false, true},
		{"packed 4:2:2", pinned, "yuyv422", 8, false, true},
		{"4:4:4 with alpha", pinned10, "yuva444p10le", 10, false, true},
		{"audio-only copies the picture", audioOnly, "gbrp", 8, false, false},
		{"not a mode plan", noMode, "gbrp", 8, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", PixelFormat: c.pixFmt, BitDepth: c.depth}}}
			err := modeSourceChromaError(c.plan, rep, c.main10)
			if c.refuse != (err != nil) {
				t.Fatalf("refused=%v, want %v (%v)", err != nil, c.refuse, err)
			}
			if err != nil && err.Error() != refusal {
				t.Fatalf("message %q, want the worker's %q", err, refusal)
			}
			// The worker's own rule, on the same plan, agrees.
			if c.plan.Mode != "" && !transcode.IsSizeAudioOnlyPlan(c.plan) && !c.main10 && transcode.SourceChromaAdmitted(c.plan, c.pixFmt, c.depth) == c.refuse {
				t.Fatal("coordinator and worker admission disagree")
			}
		})
	}
}

// Through the early check of a conversion waiting for a worker: a picture no
// worker measures fails at once, with the worker's code and message, as a
// permanent source rejection, and no work is sent. A 4:4:4 or 4:2:2 source
// keeps waiting: every goal's plan pins a 4:2:0 output. The real preflight on
// a worker refuses the same files the same way before submitting anything.
func TestPreflightRefusesAPictureNoWorkerMeasures(t *testing.T) {
	probe := func(pixFmt, depth string) string {
		out := strings.Replace(ordinaryProbeJSON, `"pix_fmt": "yuv420p"`, `"pix_fmt": "`+pixFmt+`"`, 1)
		return strings.Replace(out, `"bits_per_raw_sample": "8"`, `"bits_per_raw_sample": "`+depth+`"`, 1)
	}
	refusal := transcode.SourceClassUnsupportedError().Error()
	for _, c := range []struct {
		name, pixFmt, depth string
		refuse              bool
	}{
		{"4:4:4", "yuv444p", "8", false},
		{"4:2:2", "yuv422p", "8", false},
		{"10-bit 4:4:4", "yuv444p10le", "10", false},
		{"10-bit 4:2:2", "yuv422p10le", "10", false},
		{"unknown pixel format", "unknown", "8", true},
		{"no pixel format", "", "8", true},
		{"RGB", "gbrp", "8", true},
		{"4:1:1", "yuv411p", "8", true},
		{"10-bit 4:4:4 with alpha", "yuva444p10le", "10", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			mock := &mockTranscodeExecutor{}
			e, _, media, _ := setupProbeTestEnv(t, mock, probe(c.pixFmt, c.depth))
			ec := earlyCheckEC(t, e, "early", map[string]any{"path": media, "mode": "size"})
			res, decided := e.earlyPreflight(context.Background(), ec)
			if !c.refuse {
				if decided {
					t.Fatalf("a source the pinned plan measures was decided early: %+v", res)
				}
			} else {
				if !decided || res.Status != StepFailed || res.Error != refusal {
					t.Fatalf("decided=%v %+v", decided, res)
				}
				if code, ok := PermanentSourceRejection(res.Error, ec.Inputs, ec.State); !ok || !strings.HasPrefix(code, "quality_source_class_unsupported") {
					t.Fatalf("refusal is not a permanent source rejection: %q %v", code, ok)
				}
			}
			real := earlyCheckEC(t, e, "real", map[string]any{"path": media, "mode": "size"})
			full, err := e.stepTranscodePreflight(context.Background(), real)
			if err != nil {
				t.Fatal(err)
			}
			if c.refuse != (full.Status == StepFailed && full.Error == refusal) {
				t.Fatalf("real preflight: %+v", full)
			}
			if c.refuse && real.State["plan"] != nil {
				t.Fatal("a refused source left a plan to submit")
			}
			if mock.submitCalls != 0 {
				t.Fatal("preflight submitted work")
			}
		})
	}
}
