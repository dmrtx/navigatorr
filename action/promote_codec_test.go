package action

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// The size goal's audio-only plan copies the video, so the promoted file keeps
// the source codec and must not be held to HEVC.
func (h *promotionHarness) useAudioOnlyPlan(codec string) {
	h.t.Helper()
	source, err := h.st.GetActionInstance("source-transcode")
	if err != nil {
		h.t.Fatal(err)
	}
	state := parseExecutionContext(source, h.engine).State
	state["plan"] = &transcode.Plan{Mode: "size", ModePolicyVersion: transcode.ModePolicyV6, VideoCodec: transcode.VideoCodecCopy, ExpectedBitDepth: 8, AudioMode: "copy"}
	source.StateJSON = toJSON(state)
	source.InputsJSON = `{}`
	if err := h.st.UpdateActionInstance(*source); err != nil {
		h.t.Fatal(err)
	}
	probe, err := os.ReadFile(h.engine.deps.Ffprobe)
	if err != nil {
		h.t.Fatal(err)
	}
	probe = []byte(strings.ReplaceAll(string(probe), `"codec_name":"hevc"`, `"codec_name":"`+codec+`"`))
	if err := os.WriteFile(h.engine.deps.Ffprobe, probe, 0700); err != nil {
		h.t.Fatal(err)
	}
	h.importedCodec = codec
}

func TestPromotionAcceptsCopiedVideoOfAnAudioOnlyPlan(t *testing.T) {
	h := newPromotionHarness(t)
	h.useAudioOnlyPlan("h264")
	r, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": "source-transcode", "series_id": 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusWaitingDecision || h.imports != 0 {
		t.Fatalf("audio-only promotion did not reach approval: status=%s error=%s", r.Status, r.Error)
	}
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted || h.imports != 1 {
		t.Fatalf("copied H.264 video blocked an audio-only promotion: status=%s error=%s", r.Status, r.Error)
	}
}

// A promotion checkpointed before the plan recorded the codec resumes under
// the source plan's rule.
func TestPromotionExpectedCodecReadsTheSourcePlanForOlderCheckpoints(t *testing.T) {
	h := newPromotionHarness(t)
	if got := h.engine.promotionExpectedCodec(&promotionState{SourceActionID: "source-transcode"}); got != "hevc" {
		t.Fatalf("encoded plan expects %q", got)
	}
	h.useAudioOnlyPlan("h264")
	p := &promotionState{SourceActionID: "source-transcode"}
	if got := h.engine.promotionExpectedCodec(p); got != "h264" || p.ExpectedVideoCodec != "h264" {
		t.Fatalf("audio-only plan expects %q (kept %q)", got, p.ExpectedVideoCodec)
	}
	if got := h.engine.promotionExpectedCodec(&promotionState{SourceActionID: "missing"}); got != "hevc" {
		t.Fatalf("unreadable source expects %q, not the historical HEVC", got)
	}
	if got := h.engine.promotionExpectedCodec(&promotionState{ExpectedVideoCodec: "av1"}); got != "av1" {
		t.Fatalf("recorded codec ignored: %q", got)
	}
}

func TestPromotionCodecFamilyAliases(t *testing.T) {
	for in, want := range map[string]string{"HEVC": "hevc", "x265": "hevc", "libx265": "hevc", " h.265 ": "hevc", "AVC": "h264", "x264": "h264", "h264": "h264", "AV1": "av1", "": "",
		// ffprobe and the library service spell these differently.
		"mpeg2video": "mpeg2", "MPEG2": "mpeg2", "mpeg4": "mpeg4", "XviD": "mpeg4", "DivX": "mpeg4", "vc1": "vc1", "VC-1": "vc1"} {
		if got := promotionCodecFamily(in); got != want {
			t.Errorf("promotionCodecFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

// Compact converts to its planned size, so the full validation holds the
// candidate to that target and still rejects any other size.
func TestDetailedValidationHoldsCompactToItsTargetResolution(t *testing.T) {
	for _, tc := range []struct {
		name          string
		compact       *transcode.CompactPlan
		width, height int
		want          StepStatus
		reason        string
	}{
		{"compact at target", &transcode.CompactPlan{TargetWidth: 1920, TargetHeight: 1080}, 1920, 1080, StepCompleted, ""},
		{"compact left at source size", &transcode.CompactPlan{TargetWidth: 1920, TargetHeight: 1080}, 3840, 2160, StepWaitingDecision, "expected=1920x1080"},
		{"compact at another size", &transcode.CompactPlan{TargetWidth: 1920, TargetHeight: 1080}, 1280, 720, StepWaitingDecision, "expected=1920x1080"},
		{"not compact downscaled", nil, 1920, 1080, StepWaitingDecision, "Video resolution mismatch"},
		{"not compact at source size", nil, 3840, 2160, StepCompleted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			candidate := filepath.Join(dir, "candidate.mkv")
			if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
				t.Fatal(err)
			}
			probe := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": ` + strconv.Itoa(tc.width) + `, "height": ` + strconv.Itoa(tc.height) + `, "bits_per_raw_sample": "8"}], "format": {"format_name": "matroska,webm", "duration": "1420"}}`
			ffprobe := filepath.Join(dir, "ffprobe")
			if err := os.WriteFile(ffprobe, []byte(probeScriptWithPackets(t, probe)), 0o700); err != nil {
				t.Fatal(err)
			}
			e := NewEngine(EngineDeps{Ffprobe: ffprobe})
			ec := &ExecutionContext{Inputs: map[string]any{}, State: map[string]any{
				"candidate_path": candidate,
				"plan":           &transcode.Plan{VideoCodec: "libx265", AudioMode: "copy", Compact: tc.compact},
				"original": map[string]any{"size_bytes": int64(1 << 30), "duration_sec": 1420.0,
					"video": []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "hevc", PixelFormat: "yuv420p", Width: 3840, Height: 2160, BitDepth: 8}}},
			}}
			result, err := e.validateCandidateDetailed(context.Background(), ec)
			if err != nil || result.Status != tc.want {
				t.Fatalf("status %s (%s%s): %v", result.Status, result.Error, result.WaitingReason, err)
			}
			if tc.reason != "" && !strings.Contains(result.WaitingReason, tc.reason) {
				t.Fatalf("reason %q lacks %q", result.WaitingReason, tc.reason)
			}
		})
	}
}
