package action

import (
	"context"
	"encoding/json"
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
			if err := os.WriteFile(ffprobe, []byte(probeScriptWithPackets(t, probe)), 0o700); err != nil {
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

// Accepting a validation discrepancy still publishes a candidate made on the
// plan's track selection, so its outputs carry the report like the other
// validation paths do.
func TestAcceptedValidationLossStillReportsTheTracks(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "candidate.mkv")
	if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The kept audio track's language differs from the source's: a discrepancy
	// a user may accept, unlike a count mismatch.
	probe := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bits_per_raw_sample": "8"},
		{"index": 1, "codec_type": "audio", "codec_name": "eac3", "channels": 6, "channel_layout": "5.1", "bit_rate": "640000", "tags": {"language": "spa"}}],
		"format": {"format_name": "matroska,webm", "duration": "1420"}}`
	ffprobe := filepath.Join(dir, "ffprobe")
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\ncat <<'JSON'\n"+probe+"\nJSON\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := &transcode.Plan{VideoCodec: "libx265", AudioMode: "copy", ModePolicyVersion: transcode.ModePolicyV6, Tracks: &transcode.TrackSelection{KeepAudio: []int{1}}}
	report := &TrackSelectionReport{Message: "Kept 1 of 2 audio tracks (eng)"}
	ec := &ExecutionContext{Inputs: map[string]any{}, Decision: "accept_loss", State: map[string]any{
		"candidate_path": candidate,
		"plan":           plan,
		trackReportKey:   report,
		"original": map[string]any{"size_bytes": int64(1 << 30), "duration_sec": 1420.0,
			"video": []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", PixelFormat: "yuv420p", Width: 1920, Height: 1080, BitDepth: 8}},
			"audio": []mediainspect.DetailedStream{
				{Index: 1, Kind: "audio", Codec: "eac3", Channels: 6, ChannelLayout: "5.1", Language: "eng", BitRate: 640_000},
				{Index: 2, Kind: "audio", Codec: "eac3", Channels: 6, ChannelLayout: "5.1", Language: "fre", BitRate: 640_000},
			}},
	}}
	result, err := NewEngine(EngineDeps{Ffprobe: ffprobe}).validateCandidateDetailed(context.Background(), ec)
	if err != nil || result.Status != StepCompleted || result.Outputs["decision_applied"] != "accept_loss" {
		t.Fatalf("status %s (%s%s): %v", result.Status, result.Error, result.WaitingReason, err)
	}
	if result.Outputs["tracks"] != report || result.Outputs["tracks_message"] != report.Message {
		t.Fatalf("outputs lack the track report: %+v", result.Outputs)
	}
	// A plan that keeps every track has no report to carry.
	delete(ec.State, trackReportKey)
	plan.Tracks = nil
	result, err = NewEngine(EngineDeps{Ffprobe: ffprobe}).validateCandidateDetailed(context.Background(), ec)
	if err != nil || result.Status != StepCompleted {
		t.Fatalf("status %s: %v", result.Status, err)
	}
	if _, ok := result.Outputs["tracks"]; ok {
		t.Fatalf("a report appeared from nothing: %+v", result.Outputs)
	}
}

// A source track tagged "und" comes out with no language tag at all (the
// muxer writes none for it); that keeps the language, as the worker's check
// already says. Any other loss or change of language does not.
func TestSameLanguageTreatsUndAsNoTag(t *testing.T) {
	for _, tc := range []struct {
		source, output string
		want           bool
	}{
		{"", "", true},
		{"", "eng", true},
		{"und", "", true},
		{"UND", "", true},
		{"und", "und", true},
		{"und", "eng", false},
		{"eng", "", false},
		{"eng", "ENG", true},
		{"eng", "spa", false},
		// Codes outside ISO 639-2 come back from Matroska as they went in.
		{"pob", "pob", true},
		{"en", "en", true},
		{"pt-BR", "pt-br", true},
		{"en", "eng", false},
		{"pob", "", false},
	} {
		if got := sameLanguage(tc.source, tc.output); got != tc.want {
			t.Errorf("sameLanguage(%q, %q) = %v, want %v", tc.source, tc.output, got, tc.want)
		}
	}
}

// A copied track is the source's track, name and flags included, and a
// converted subtitle keeps its name and every flag the output can store.
// Attachments are the source's files, by name, type and size. Each
// difference waits for a decision; ffmpeg marking the first track default
// when the command set no flags passes.
func TestDetailedValidationHoldsCopiedTrackMetadata(t *testing.T) {
	// Source: audio 1 eng "Main" (default), 2 eng "Commentary" (comment);
	// subtitles 3 eng "SDH" (hearing_impaired, copied), 4 spa (mov_text,
	// converted, forced); a font.
	src := []mediainspect.DetailedStream{
		{Index: 1, Kind: "audio", Codec: "eac3", Channels: 6, ChannelLayout: "5.1", Language: "eng", Title: "Main", BitRate: 640_000, Disposition: map[string]int{"default": 1}},
		{Index: 2, Kind: "audio", Codec: "ac3", Channels: 6, ChannelLayout: "5.1", Language: "eng", Title: "Commentary", BitRate: 640_000, Disposition: map[string]int{"comment": 1, "dub": 1}},
	}
	srcSubs := []mediainspect.DetailedStream{
		{Index: 3, Kind: "subtitle", Codec: "subrip", Language: "eng", Title: "SDH", Disposition: map[string]int{"hearing_impaired": 1}},
		{Index: 4, Kind: "subtitle", Codec: "mov_text", Language: "spa", Title: "Forced", Disposition: map[string]int{"forced": 1, "visual_impaired": 1}},
	}
	font := func(name string, size int64) mediainspect.DetailedStream {
		return mediainspect.DetailedStream{Index: 5, Kind: "attachment", Codec: "ttf", ExtraDataSize: size, Tags: map[string]string{"filename": name, "mimetype": "font/ttf"}}
	}
	type stream struct {
		codec, lang, title string
		disp               map[string]int
	}
	type output struct {
		audio, subs []stream
		fontName    string
		fontSize    int64
	}
	good := func() output {
		return output{
			audio:    []stream{{"eac3", "eng", "Main", map[string]int{"default": 1}}, {"ac3", "eng", "Commentary", map[string]int{"comment": 1}}},
			subs:     []stream{{"subrip", "eng", "SDH", map[string]int{"hearing_impaired": 1}}, {"subrip", "spa", "Forced", map[string]int{"forced": 1, "visual_impaired": 1}}},
			fontName: "font.ttf", fontSize: 1234,
		}
	}
	for _, tc := range []struct {
		name     string
		mutate   func(*output)
		oldFonts bool
		reason   string
		// source changes the source's audio and subtitle records.
		source func(audio, subs []mediainspect.DetailedStream)
	}{
		{"every track as the source's", func(*output) {}, false, "", nil},
		// ffmpeg writes a plain-text track as S_TEXT/UTF8, which reads back
		// as subrip: the same bitstream.
		{"a copied text subtitle read back as subrip", func(*output) {}, false, "", func(_, s []mediainspect.DetailedStream) { s[0].Codec = "text" }},
		{"a copied subtitle changed codec", func(o *output) { o.subs[0].codec = "ass" }, false, "Copied subtitle codec changed at stream 0: original=subrip output=ass", nil},
		{"a source subtitle without a codec", func(*output) {}, false, "Copied subtitle codec changed at stream 0", func(_, s []mediainspect.DetailedStream) { s[0].Codec = "" }},
		{"a source record without attachment sizes", func(o *output) { o.fontSize = 999 }, true, "", nil},
		// The worker wrote the audio tracks' flags, so ffmpeg marked nothing
		// default on its own.
		{"a subtitle default with audio flags written", func(o *output) { o.subs[0].disp = map[string]int{"hearing_impaired": 1, "default": 1} }, false, "Copied subtitle track 0 changed its default flag", nil},
		{"ffmpeg marked the first subtitle default, no flags written", func(o *output) {
			o.audio[0].disp, o.audio[1].disp = nil, nil
			o.subs[0].disp = map[string]int{"hearing_impaired": 1, "default": 1}
		}, false, "", func(a, _ []mediainspect.DetailedStream) {
			a[0].Disposition, a[1].Disposition = nil, nil
		}},
		{"a copied audio track renamed", func(o *output) { o.audio[1].title = "Track 2" }, false, `Copied audio track 1 was renamed: the plan calls it "Commentary", the output "Track 2"`, nil},
		{"a copied audio track lost commentary", func(o *output) { o.audio[1].disp = nil }, false, "Copied audio track 1 changed its comment flag", nil},
		{"a copied audio track became forced", func(o *output) { o.audio[0].disp = map[string]int{"default": 1, "forced": 1} }, false, "Copied audio track 0 changed its forced flag", nil},
		{"a copied subtitle renamed", func(o *output) { o.subs[0].title = "" }, false, "Copied subtitle track 0 was renamed", nil},
		{"a copied subtitle lost hearing_impaired", func(o *output) { o.subs[0].disp = nil }, false, "Copied subtitle track 0 changed its hearing_impaired flag", nil},
		{"a converted subtitle lost forced", func(o *output) { o.subs[1].disp = nil }, false, "Forced subtitle disposition lost at stream 1", nil},
		{"a converted subtitle lost visual_impaired", func(o *output) { o.subs[1].disp = map[string]int{"forced": 1} }, false, "Converted subtitle track 1 changed its visual_impaired flag", nil},
		{"a converted subtitle gained a flag", func(o *output) {
			o.subs[1].disp = map[string]int{"forced": 1, "visual_impaired": 1, "hearing_impaired": 1}
		}, false, "Converted subtitle track 1 changed its hearing_impaired flag", nil},
		{"a converted subtitle renamed", func(o *output) { o.subs[1].title = "Spanish" }, false, `Converted subtitle track 1 was renamed: the source calls it "Forced", the output "Spanish"`, nil},
		{"a font replaced by another", func(o *output) { o.fontName = "other.ttf" }, false, `Attachment "font.ttf" (font/ttf, 1234 bytes) is missing from the output`, nil},
		{"a font cut short", func(o *output) { o.fontSize = 1000 }, false, `Attachment "font.ttf" (font/ttf, 1234 bytes) is missing`, nil},
		// A source whose probe reported no audio flags: the worker wrote none,
		// and ffmpeg may mark the first audio track default on its own.
		{"ffmpeg marked the first audio default, no flags in the source", func(o *output) {
			o.audio[0].disp, o.audio[1].disp = map[string]int{"default": 1}, nil
		}, false, "", func(a, _ []mediainspect.DetailedStream) {
			a[0].Disposition, a[1].Disposition = nil, nil
		}},
		{"the second audio default, no flags in the source", func(o *output) {
			o.audio[0].disp, o.audio[1].disp = nil, map[string]int{"default": 1}
		}, false, "Copied audio track 1 changed its default flag", func(a, _ []mediainspect.DetailedStream) {
			a[0].Disposition, a[1].Disposition = nil, nil
		}},
		// Flags reported, so written: a default the source has not is a change.
		{"a default added to a source without one", func(o *output) {
			o.audio[0].disp = map[string]int{"default": 1}
			o.audio[1].disp = nil
		}, false, "Copied audio track 0 changed its default flag", func(a, _ []mediainspect.DetailedStream) {
			a[0].Disposition, a[1].Disposition = map[string]int{"default": 0}, map[string]int{"default": 0}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := good()
			tc.mutate(&out)
			dir := t.TempDir()
			candidate := filepath.Join(dir, "candidate.mkv")
			if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
				t.Fatal(err)
			}
			disp := func(d map[string]int) string {
				b, _ := json.Marshal(d)
				if d == nil {
					return "{}"
				}
				return string(b)
			}
			streams := `{"index": 0, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bits_per_raw_sample": "8"}`
			n := 1
			for _, s := range out.audio {
				streams += fmt.Sprintf(`,{"index": %d, "codec_type": "audio", "codec_name": %q, "channels": 6, "channel_layout": "5.1", "bit_rate": "640000", "disposition": %s, "tags": {"language": %q, "title": %q}}`, n, s.codec, disp(s.disp), s.lang, s.title)
				n++
			}
			for _, s := range out.subs {
				streams += fmt.Sprintf(`,{"index": %d, "codec_type": "subtitle", "codec_name": %q, "disposition": %s, "tags": {"language": %q, "title": %q}}`, n, s.codec, disp(s.disp), s.lang, s.title)
				n++
			}
			streams += fmt.Sprintf(`,{"index": %d, "codec_type": "attachment", "codec_name": "ttf", "extradata_size": %d, "tags": {"filename": %q, "mimetype": "font/ttf"}}`, n, out.fontSize, out.fontName)
			probe := `{"streams": [` + streams + `], "format": {"format_name": "matroska,webm", "duration": "1420"}}`
			ffprobe := filepath.Join(dir, "ffprobe")
			if err := os.WriteFile(ffprobe, []byte(probeScriptWithPackets(t, probe)), 0o700); err != nil {
				t.Fatal(err)
			}
			srcAudio := append([]mediainspect.DetailedStream(nil), src...)
			srcSubs := append([]mediainspect.DetailedStream(nil), srcSubs...)
			if tc.source != nil {
				tc.source(srcAudio, srcSubs)
			}
			srcFont := font("font.ttf", 1234)
			if tc.oldFonts {
				srcFont.ExtraDataSize = 0
			}
			plan := &transcode.Plan{VideoCodec: "libx265", AudioMode: "copy", PreserveAttachments: true,
				SubtitleActions: []transcode.SubtitleAction{{TypeIndex: 0, SourceStreamIndex: 3, Operation: "copy"}, {TypeIndex: 1, SourceStreamIndex: 4, Operation: "transcode", Codec: "subrip"}}}
			e := NewEngine(EngineDeps{Ffprobe: ffprobe})
			ec := &ExecutionContext{Inputs: map[string]any{}, State: map[string]any{
				"candidate_path": candidate,
				"plan":           plan,
				"original": map[string]any{"size_bytes": int64(1 << 30), "duration_sec": 1420.0,
					"video":       []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", PixelFormat: "yuv420p", Width: 1920, Height: 1080, BitDepth: 8}},
					"audio":       srcAudio,
					"subtitles":   srcSubs,
					"attachments": []mediainspect.DetailedStream{srcFont}},
			}}
			result, err := e.validateCandidateDetailed(context.Background(), ec)
			if err != nil {
				t.Fatal(err)
			}
			if tc.reason == "" {
				if result.Status != StepCompleted {
					t.Fatalf("status %s (%s%s)", result.Status, result.Error, result.WaitingReason)
				}
				return
			}
			if result.Status != StepWaitingDecision || !strings.Contains(result.WaitingReason, tc.reason) {
				t.Fatalf("status %s (%s%s), want a decision on %q", result.Status, result.Error, result.WaitingReason, tc.reason)
			}
		})
	}
}
