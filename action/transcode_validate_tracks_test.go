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

// The server's final validation holds the output to the tracks the plan keeps:
// exactly those, in source order, with their languages and the default flag the
// plan gives them. A wrong count is a hard failure nobody can accept.
func TestDetailedValidationHoldsTheOutputToTheKeptTracks(t *testing.T) {
	type track struct {
		kind, codec, lang string
		disp              string // "default" or "forced"
	}
	// Source: audio 1 eng, 2 fre (the default), 3 spa; subtitles 4 eng, 5 fre,
	// 6 spa (forced). The plan keeps audio 1 and 3 and subtitles 4 and 6.
	src := []mediainspect.DetailedStream{
		{Index: 1, Kind: "audio", Codec: "eac3", Channels: 6, ChannelLayout: "5.1", Language: "eng", BitRate: 640_000},
		{Index: 2, Kind: "audio", Codec: "eac3", Channels: 6, ChannelLayout: "5.1", Language: "fre", BitRate: 640_000, Disposition: map[string]int{"default": 1}},
		{Index: 3, Kind: "audio", Codec: "eac3", Channels: 6, ChannelLayout: "5.1", Language: "spa", BitRate: 640_000},
	}
	srcSubs := []mediainspect.DetailedStream{
		{Index: 4, Kind: "subtitle", Codec: "subrip", Language: "eng"},
		{Index: 5, Kind: "subtitle", Codec: "subrip", Language: "fre"},
		{Index: 6, Kind: "subtitle", Codec: "subrip", Language: "spa", Disposition: map[string]int{"forced": 1}},
	}
	sel := &transcode.TrackSelection{KeepAudio: []int{1, 3}, KeepSubtitles: []int{4, 6}}
	audio := func(lang, disp string) track { return track{"audio", "eac3", lang, disp} }
	sub := func(lang, disp string) track { return track{"subtitle", "subrip", lang, disp} }
	good := []track{audio("eng", "default"), audio("spa", ""), sub("eng", ""), sub("spa", "forced")}
	with := func(i int, t track) []track {
		c := append([]track(nil), good...)
		c[i] = t
		return c
	}
	for _, tc := range []struct {
		name   string
		out    []track
		want   StepStatus
		reason string
		accept bool
	}{
		{"exactly the kept tracks", good, StepCompleted, "", false},
		{"every audio track kept", []track{audio("eng", "default"), audio("fre", ""), audio("spa", ""), sub("eng", ""), sub("spa", "forced")}, StepFailed, "Track selection mismatch", false},
		{"a dropped subtitle kept", []track{audio("eng", "default"), audio("spa", ""), sub("eng", ""), sub("fre", ""), sub("spa", "forced")}, StepFailed, "Track selection mismatch", false},
		{"a kept audio track missing", good[1:], StepFailed, "Track selection mismatch", false},
		{"a kept subtitle missing", good[:3], StepFailed, "Track selection mismatch", false},
		{"the count mismatch cannot be accepted", good[1:], StepFailed, "Track selection mismatch", true},
		{"audio languages swapped", []track{audio("spa", "default"), audio("eng", ""), sub("eng", ""), sub("spa", "forced")}, StepWaitingDecision, "Audio language mismatch at stream 0", false},
		{"the dropped track kept in place of a kept one", with(1, audio("fre", "")), StepWaitingDecision, "Audio language mismatch at stream 1", false},
		{"the default flag lost", with(0, audio("eng", "")), StepWaitingDecision, "Default audio disposition lost at stream 0", false},
		{"the forced subtitle lost its flag", with(3, sub("spa", "")), StepWaitingDecision, "Forced subtitle disposition lost at stream 1", false},
		{"subtitle language wrong", with(2, sub("ger", "")), StepWaitingDecision, "Subtitle language mismatch at stream 0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			candidate := filepath.Join(dir, "candidate.mkv")
			if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
				t.Fatal(err)
			}
			streams := `{"index": 0, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bits_per_raw_sample": "8"}`
			for i, tr := range tc.out {
				disp := `{}`
				if tr.disp != "" {
					disp = fmt.Sprintf(`{%q: 1}`, tr.disp)
				}
				if tr.kind == "audio" {
					streams += fmt.Sprintf(`,{"index": %d, "codec_type": "audio", "codec_name": %q, "channels": 6, "channel_layout": "5.1", "bit_rate": "640000", "disposition": %s, "tags": {"language": %q}}`, i+1, tr.codec, disp, tr.lang)
				} else {
					streams += fmt.Sprintf(`,{"index": %d, "codec_type": "subtitle", "codec_name": %q, "disposition": %s, "tags": {"language": %q}}`, i+1, tr.codec, disp, tr.lang)
				}
			}
			probe := `{"streams": [` + streams + `], "format": {"format_name": "matroska,webm", "duration": "1420"}}`
			ffprobe := filepath.Join(dir, "ffprobe")
			if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\ncat <<'JSON'\n"+probe+"\nJSON\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			plan := &transcode.Plan{VideoCodec: "libx265", AudioMode: "copy", ModePolicyVersion: transcode.ModePolicyV6, Tracks: sel,
				SubtitleActions: []transcode.SubtitleAction{{TypeIndex: 0, SourceStreamIndex: 4, Operation: "copy"}, {TypeIndex: 2, SourceStreamIndex: 6, Operation: "copy"}}}
			e := NewEngine(EngineDeps{Ffprobe: ffprobe})
			ec := &ExecutionContext{Inputs: map[string]any{}, State: map[string]any{
				"candidate_path": candidate,
				"plan":           plan,
				"original": map[string]any{"size_bytes": int64(1 << 30), "duration_sec": 1420.0,
					"video":     []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", PixelFormat: "yuv420p", Width: 1920, Height: 1080, BitDepth: 8}},
					"audio":     src,
					"subtitles": srcSubs},
			}}
			if tc.accept {
				ec.Decision = "accept_loss"
			}
			result, err := e.validateCandidateDetailed(context.Background(), ec)
			if err != nil || result.Status != tc.want {
				t.Fatalf("status %s (%s%s): %v", result.Status, result.Error, result.WaitingReason, err)
			}
			if tc.reason != "" && !strings.Contains(result.Error+result.WaitingReason, tc.reason) {
				t.Fatalf("reason %q%q, want %q", result.Error, result.WaitingReason, tc.reason)
			}
		})
	}
}
