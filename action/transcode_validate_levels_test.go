package action

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// The server's final validation holds every output track to its level: the
// codec, the AAC profile, a bitrate near the plan's, and the track itself.
func TestDetailedValidationHoldsTracksToTheirAudioLevel(t *testing.T) {
	type out struct {
		codec, profile, lang string
		bitRate              int
		channels             int
		layout               string
	}
	// Source: E-AC-3 5.1 640 kbps (converted at both levels), AAC stereo 128
	// (copied at level 1, HE-AAC at level 2), E-AC-3 JOC (copied at both).
	src := []mediainspect.DetailedStream{
		{Index: 1, Kind: "audio", Codec: "eac3", Channels: 6, ChannelLayout: "5.1(side)", Language: "eng", BitRate: 640_000},
		{Index: 2, Kind: "audio", Codec: "aac", Profile: "LC", Channels: 2, ChannelLayout: "stereo", Language: "spa", BitRate: 128_000},
		{Index: 3, Kind: "audio", Codec: "eac3", Profile: "Dolby Digital Plus + Dolby Atmos", Channels: 6, ChannelLayout: "5.1", Language: "ita", BitRate: 768_000},
	}
	good := func(level string) []out {
		if level == transcode.AudioModeSizeHEAAC {
			return []out{{"aac", "HE-AAC", "eng", 158_000, 6, "5.1"}, {"aac", "HE-AAC", "spa", 63_000, 2, "stereo"}, {"eac3", "", "ita", 768_000, 6, "5.1"}}
		}
		return []out{{"aac", "LC", "eng", 380_000, 6, "5.1"}, {"aac", "LC", "spa", 128_000, 2, "stereo"}, {"eac3", "", "ita", 768_000, 6, "5.1"}}
	}
	// level 1 copies the 128 kbps AAC, so its output is the source's profile.
	change := func(outs []out, i int, f func(*out)) []out {
		c := append([]out(nil), outs...)
		f(&c[i])
		return c
	}
	for _, tc := range []struct {
		name, mode string
		out        []out
		want       StepStatus
		reason     string
	}{
		{"level 1 as planned", transcode.AudioModeSizeAAC, good(transcode.AudioModeSizeAAC), StepCompleted, ""},
		{"level 2 as planned", transcode.AudioModeSizeHEAAC, good(transcode.AudioModeSizeHEAAC), StepCompleted, ""},
		{"level 1 track left as E-AC-3", transcode.AudioModeSizeAAC, change(good(transcode.AudioModeSizeAAC), 0, func(o *out) { o.codec, o.profile = "eac3", "" }), StepWaitingDecision, "Audio codec mismatch at stream 0"},
		{"level 1 copied track converted", transcode.AudioModeSizeAAC, change(good(transcode.AudioModeSizeAAC), 2, func(o *out) { o.codec, o.profile = "aac", "LC" }), StepWaitingDecision, "Audio codec mismatch at stream 2"},
		{"level 1 got HE-AAC", transcode.AudioModeSizeAAC, change(good(transcode.AudioModeSizeAAC), 0, func(o *out) { o.profile = "HE-AAC" }), StepWaitingDecision, "Audio profile mismatch at stream 0"},
		{"level 2 got AAC-LC", transcode.AudioModeSizeHEAAC, change(good(transcode.AudioModeSizeHEAAC), 0, func(o *out) { o.profile = "LC" }), StepWaitingDecision, "Audio profile mismatch at stream 0"},
		{"level 2 stereo not converted", transcode.AudioModeSizeHEAAC, change(good(transcode.AudioModeSizeHEAAC), 1, func(o *out) { o.profile = "LC" }), StepWaitingDecision, "Audio profile mismatch at stream 1"},
		{"level 1 bitrate far above the plan", transcode.AudioModeSizeAAC, change(good(transcode.AudioModeSizeAAC), 0, func(o *out) { o.bitRate = 900_000 }), StepWaitingDecision, "Audio bitrate mismatch at stream 0"},
		{"level 2 bitrate far above the plan", transcode.AudioModeSizeHEAAC, change(good(transcode.AudioModeSizeHEAAC), 0, func(o *out) { o.bitRate = 384_000 }), StepWaitingDecision, "Audio bitrate mismatch at stream 0"},
		{"level 2 packets not measurable leave codec profile and channels", transcode.AudioModeSizeHEAAC, change(good(transcode.AudioModeSizeHEAAC), 0, func(o *out) { o.bitRate = 0 }), StepCompleted, ""},
		{"level 2 unmeasured packets still need the profile", transcode.AudioModeSizeHEAAC, change(change(good(transcode.AudioModeSizeHEAAC), 0, func(o *out) { o.bitRate = 0 }), 0, func(o *out) { o.profile = "LC" }), StepWaitingDecision, "Audio profile mismatch at stream 0"},
		{"a track missing", transcode.AudioModeSizeAAC, good(transcode.AudioModeSizeAAC)[:2], StepWaitingDecision, "Audio stream lost"},
		{"swapped languages", transcode.AudioModeSizeHEAAC, change(change(good(transcode.AudioModeSizeHEAAC), 0, func(o *out) { o.lang = "spa" }), 1, func(o *out) { o.lang = "eng" }), StepWaitingDecision, "Audio language mismatch at stream 0"},
		{"channels reduced", transcode.AudioModeSizeHEAAC, change(good(transcode.AudioModeSizeHEAAC), 0, func(o *out) { o.channels, o.layout = 2, "stereo" }), StepWaitingDecision, "Audio channel mismatch at stream 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			candidate := filepath.Join(dir, "candidate.mkv")
			if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
				t.Fatal(err)
			}
			streams := `{"index": 0, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bits_per_raw_sample": "8"}`
			for i, a := range tc.out {
				streams += fmt.Sprintf(`,{"index": %d, "codec_type": "audio", "codec_name": %q, "profile": %q, "channels": %d, "channel_layout": %q, "bit_rate": "%d", "tags": {"language": %q}}`, i+1, a.codec, a.profile, a.channels, a.layout, a.bitRate, a.lang)
			}
			probe := `{"streams": [` + streams + `], "format": {"format_name": "matroska,webm", "duration": "1420"}}`
			// The packet scan reads each track at its bitrate over the whole
			// file; a zero bitrate is a track whose packets cannot be read.
			packets := ""
			for i, a := range tc.out {
				if a.bitRate == 0 {
					packets = "stream_index=1|pts_time=N/A|duration_time=N/A|size=0\n"
					break
				}
				half := int64(a.bitRate) * 1420 / 16
				packets += fmt.Sprintf("stream_index=%d|pts_time=0.000000|duration_time=1.000000|size=%d\nstream_index=%d|pts_time=1419.000000|duration_time=1.000000|size=%d\n", i+1, half, i+1, half)
			}
			ffprobe := filepath.Join(dir, "ffprobe")
			script := "#!/bin/sh\ncase \"$*\" in *-show_packets*) cat <<'PACKETS'\n" + packets + "PACKETS\nexit 0;; esac\ncat <<'JSON'\n" + probe + "\nJSON\n"
			if err := os.WriteFile(ffprobe, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			e := NewEngine(EngineDeps{Ffprobe: ffprobe})
			ec := &ExecutionContext{Inputs: map[string]any{}, State: map[string]any{
				"candidate_path": candidate,
				"plan":           &transcode.Plan{VideoCodec: "libx265", AudioMode: tc.mode, ModePolicyVersion: transcode.ModePolicyV5},
				"original": map[string]any{"size_bytes": int64(1 << 30), "duration_sec": 1420.0,
					"video": []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", PixelFormat: "yuv420p", Width: 1920, Height: 1080, BitDepth: 8}},
					"audio": src},
			}}
			result, err := e.validateCandidateDetailed(context.Background(), ec)
			if err != nil || result.Status != tc.want {
				t.Fatalf("status %s (%s%s): %v", result.Status, result.Error, result.WaitingReason, err)
			}
			if tc.reason != "" && !strings.Contains(result.WaitingReason, tc.reason) {
				t.Fatalf("reason %q, want %q", result.WaitingReason, tc.reason)
			}
		})
	}
}
