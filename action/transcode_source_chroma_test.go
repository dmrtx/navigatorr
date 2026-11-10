package action

import (
	"context"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// The coordinator refuses a picture no measurement class covers (RGB, 4:1:1,
// alpha, big-endian, 12-bit), with the worker's message word for word, and
// leaves everything else to the worker: a 4:2:2 or 4:4:4 source is the
// worker's to admit with whatever 4:2:0 output a later step pins, a probe
// without a pixel format or depth is the worker's own probe to judge, an
// audio-only plan is never measured, a size budget may end before any worker
// sees the file, and x265_preserve refuses an HEVC source as already_hevc
// first.
func TestModeSourceChromaErrorMatchesTheWorker(t *testing.T) {
	pinned := &transcode.Plan{Mode: "size", PixelFormat: "yuv420p"}
	pinned10 := &transcode.Plan{Mode: "quality", PixelFormat: "p010le"}
	open := &transcode.Plan{Mode: "size"}
	// A mode plan before its encode is planned carries no perceptual
	// validation yet; the worker still checks its picture first.
	unvalidated := &transcode.Plan{Mode: "quality", PixelFormat: "yuv420p"}
	audioOnly := &transcode.Plan{Mode: "size", ModePolicyVersion: transcode.ModePolicyV7, VideoCodec: transcode.VideoCodecCopy}
	preserve := &transcode.Plan{Mode: "x265_preserve", PixelFormat: "yuv420p"}
	noMode := &transcode.Plan{}
	sizeGoal := &ModePolicySnapshot{Mode: "size", Size: &ModeSizePolicy{AudioOnlyFallback: true}}
	budget := &ModePolicySnapshot{Mode: "size", Size: &ModeSizePolicy{AudioOnlyFallback: true, SizeBudget: &SizeBudgetPolicy{}}}
	budgetNoAudioOnly := &ModePolicySnapshot{Mode: "size", Size: &ModeSizePolicy{SizeBudget: &SizeBudgetPolicy{}}}
	audioOnlyGoal := &ModePolicySnapshot{Mode: "size", Size: &ModeSizePolicy{AudioOnly: true}}
	refusal := transcode.SourceClassUnsupportedError().Error()
	for _, c := range []struct {
		name     string
		plan     *transcode.Plan
		snapshot *ModePolicySnapshot
		codec    string
		pixFmt   string
		depth    int
		refuse   bool
	}{
		{"4:2:0 on an open plan", open, sizeGoal, "h264", "yuv420p", 8, false},
		{"full-range 4:2:0", open, sizeGoal, "h264", "yuvj420p", 8, false},
		{"10-bit 4:2:0", open, nil, "h264", "yuv420p10le", 10, false},
		{"4:4:4 on a pinned plan", pinned, sizeGoal, "h264", "yuv444p", 8, false},
		{"4:2:2 on a pinned plan", pinned, sizeGoal, "h264", "yuv422p", 8, false},
		{"10-bit 4:4:4 on a pinned plan", pinned10, nil, "h264", "yuv444p10le", 10, false},
		{"10-bit 4:2:2 on a pinned plan", pinned10, nil, "h264", "yuv422p10le", 10, false},
		{"4:4:4 on an open plan", open, sizeGoal, "h264", "yuv444p", 8, false},
		{"8-bit 4:2:2 on an open size plan", open, sizeGoal, "h264", "yuv422p", 8, false},
		{"10-bit 4:2:2 on an open plan", open, nil, "h264", "yuv422p10le", 10, false},
		{"4:2:2 on a plan without perceptual validation", unvalidated, nil, "h264", "yuv422p", 8, false},
		// A probe that named no pixel format or depth (late headers, a shallow
		// probe) is no evidence for a permanent refusal: the worker probes the
		// staged copy itself.
		{"no pixel format", pinned, sizeGoal, "h264", "", 8, false},
		{"an unknown pixel format", open, sizeGoal, "h264", "unknown", 8, false},
		{"no bit depth", pinned, sizeGoal, "h264", "gbrp", 0, false},
		{"no bit depth on 4:2:0", pinned, sizeGoal, "h264", "yuv420p", 0, false},
		{"a format that does not match the depth", pinned, nil, "h264", "yuv420p10le", 8, true},
		{"big-endian 10-bit", pinned10, nil, "h264", "yuv420p10be", 10, true},
		{"12-bit", pinned10, nil, "h264", "yuv420p12le", 12, true},
		{"RGB", pinned, sizeGoal, "h264", "gbrp", 8, true},
		{"RGB on a plan without perceptual validation", unvalidated, nil, "h264", "gbrp", 8, true},
		{"4:1:1", pinned, sizeGoal, "h264", "yuv411p", 8, true},
		{"packed 4:2:2", pinned, sizeGoal, "h264", "yuyv422", 8, true},
		{"4:4:4 with alpha", pinned10, nil, "h264", "yuva444p10le", 10, true},
		{"RGB under x265_preserve", preserve, nil, "h264", "gbrp", 8, true},
		{"HEVC under x265_preserve is already_hevc first", preserve, nil, "hevc", "gbrp", 8, false},
		{"audio-only copies the picture", audioOnly, sizeGoal, "h264", "gbrp", 8, false},
		{"an audio-only goal", pinned, audioOnlyGoal, "h264", "gbrp", 8, false},
		{"a size budget may still copy the picture", pinned, budget, "h264", "gbrp", 8, false},
		// A size budget may end with its own reason (no budget, unreachable,
		// worker unsupported) before any worker sees the file.
		{"a size budget without audio-only", pinned, budgetNoAudioOnly, "h264", "gbrp", 8, false},
		{"not a mode plan", noMode, nil, "h264", "gbrp", 8, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: c.codec, PixelFormat: c.pixFmt, BitDepth: c.depth}}}
			err := modeSourceChromaError(c.plan, c.snapshot, rep)
			if c.refuse != (err != nil) {
				t.Fatalf("refused=%v, want %v (%v)", err != nil, c.refuse, err)
			}
			if err != nil && err.Error() != refusal {
				t.Fatalf("message %q, want the worker's %q", err, refusal)
			}
		})
	}
	if modeSourceChromaError(pinned, sizeGoal, nil) != nil {
		t.Fatal("a missing report was refused")
	}
	if modeSourceChromaError(pinned, sizeGoal, &mediainspect.DetailedReport{}) != nil {
		t.Fatal("a report without a main video stream was refused")
	}
}

// Whatever output pixel format a later step pins (none, the size plans'
// yuv420p, the Main10 switch's p010le, or anything else), a picture the
// coordinator refuses is one the worker's own admission refuses too: the
// coordinator never refuses what a worker would accept.
func TestModeSourceChromaErrorNeverRefusesWhatTheWorkerAdmits(t *testing.T) {
	formats := []string{"", "unknown", "yuv420p10be", "yuv444p10be", "yuv420p", "yuvj420p", "nv12", "yuv422p", "yuvj422p", "yuv444p", "yuvj444p", "yuv420p10le", "p010le", "yuv422p10le", "yuv444p10le", "yuv411p", "yuv410p", "gbrp", "gbrp10le", "rgb24", "yuva420p", "yuva444p10le", "gray", "gray10le", "yuyv422", "uyvy422", "yuv440p", "yuv420p12le", "nv16"}
	for _, pixFmt := range formats {
		for _, depth := range []int{0, 8, 10, 12} {
			rep := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", PixelFormat: pixFmt, BitDepth: depth}}}
			if modeSourceChromaError(&transcode.Plan{Mode: "size"}, nil, rep) == nil {
				continue
			}
			for _, out := range append([]string{""}, formats...) {
				if transcode.SourceChromaAdmitted(&transcode.Plan{Mode: "size", PixelFormat: out}, pixFmt, depth) {
					t.Fatalf("%s %d-bit: refused, but the worker admits it on a plan pinned to %q", pixFmt, depth, out)
				}
			}
		}
	}
}

// Through the early check of a conversion waiting for a worker: a picture no
// worker measures fails at once, with the worker's code and message, as a
// permanent source rejection, and no work is sent. A 4:4:4 or 4:2:2 source
// (8-bit included) is never refused here, early or in the real preflight: the
// worker admits it with the 4:2:0 output its plan pins. The real preflight on
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
		{"8-bit 4:2:2", "yuv422p", "8", false},
		{"10-bit 4:4:4", "yuv444p10le", "10", false},
		{"10-bit 4:2:2", "yuv422p10le", "10", false},
		{"unknown pixel format", "unknown", "8", false},
		{"no pixel format", "", "8", false},
		{"RGB", "gbrp", "8", true},
		{"big-endian 10-bit 4:2:0", "yuv420p10be", "10", true},
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
