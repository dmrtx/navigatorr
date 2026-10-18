package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// A PCM track copied from MOV/MP4/AVI into Matroska reports no channel
// layout; only a compact AAC conversion is held to the source layout.
func TestDetailedValidationChecksLayoutOnlyForCompactConversions(t *testing.T) {
	for _, tc := range []struct {
		name, mode, srcCodec, srcLayout, outCodec, outLayout string
		want                                                 StepStatus
	}{
		{"copied pcm stereo", "copy", "pcm_s16le", "stereo", "pcm_s16le", "", StepCompleted},
		{"copied pcm 5.1", "copy", "pcm_s24le", "5.1", "pcm_s24le", "", StepCompleted},
		{"compact keeps 5.1", "compact", "pcm_s24le", "5.1(side)", "aac", "5.1", StepCompleted},
		{"compact remapped 7.1", "compact", "truehd", "7.1", "aac", "7.1(wide)", StepWaitingDecision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			candidate := filepath.Join(dir, "candidate.mkv")
			if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
				t.Fatal(err)
			}
			channels := map[string]string{"stereo": "2", "5.1": "6", "5.1(side)": "6", "7.1": "8"}[tc.srcLayout]
			probe := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bits_per_raw_sample": "8"},{"index": 1, "codec_type": "audio", "codec_name": "` + tc.outCodec + `", "channels": ` + channels + `, "channel_layout": "` + tc.outLayout + `"}], "format": {"format_name": "matroska,webm", "duration": "1420"}}`
			ffprobe := filepath.Join(dir, "ffprobe")
			if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\ncat <<'JSON'\n"+probe+"\nJSON\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			n := map[string]int{"2": 2, "6": 6, "8": 8}[channels]
			e := NewEngine(EngineDeps{Ffprobe: ffprobe})
			ec := &ExecutionContext{Inputs: map[string]any{}, State: map[string]any{
				"candidate_path": candidate,
				"plan":           &transcode.Plan{VideoCodec: "libx265", AudioMode: tc.mode},
				"original": map[string]any{"size_bytes": int64(1 << 30), "duration_sec": 1420.0,
					"video": []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", PixelFormat: "yuv420p", Width: 1920, Height: 1080, BitDepth: 8}},
					"audio": []mediainspect.DetailedStream{{Index: 1, Kind: "audio", Codec: tc.srcCodec, Channels: n, ChannelLayout: tc.srcLayout}}},
			}}
			result, err := e.validateCandidateDetailed(context.Background(), ec)
			if err != nil || result.Status != tc.want {
				t.Fatalf("status %s (%s%s): %v", result.Status, result.Error, result.WaitingReason, err)
			}
			if tc.want == StepWaitingDecision && !strings.Contains(result.WaitingReason, "Audio layout mismatch") {
				t.Fatalf("reason %q", result.WaitingReason)
			}
		})
	}
}
