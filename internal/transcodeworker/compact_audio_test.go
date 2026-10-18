package transcodeworker

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestCompactAudioArgumentsAndPreservation(t *testing.T) {
	plan := &transcode.Plan{VideoCodec: "libx265", Quality: 23, VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, AudioMode: "compact"}
	ep := &ExecutionPlan{Plan: plan, Streams: []StreamAction{
		{Kind: "audio", TypeIndex: 0, TargetCodec: "aac", BitrateKbps: 192},
		{Kind: "audio", TypeIndex: 1, TargetCodec: "copy"},
	}}
	args, err := BuildFFmpegArgs(ep, "source.mkv", "candidate.mkv", "progress")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-pix_fmt yuv420p10le", "-c:a copy", "-c:a:0 aac", "-b:a:0 192k", "-metadata:s:a:0 BPS="} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s: %s", want, joined)
		}
	}
	if strings.Contains(joined, "-ac ") || strings.Contains(joined, "-c:a:1 aac") {
		t.Fatal("downmix or lossy re-encode requested")
	}
	src := []SourceStream{{Kind: "audio", Codec: "dts", Channels: 2, Language: "eng"}, {Kind: "audio", Codec: "aac", Channels: 2, Language: "jpn"}}
	cand := append([]SourceStream(nil), src...)
	cand[0].Codec = "aac"
	if err := validateAudioPreservation(plan, src, cand); err != nil {
		t.Fatal(err)
	}
	cand[0].Language = ""
	if err := validateAudioPreservation(plan, src, cand); err == nil {
		t.Fatal("known language lost")
	}
	cand[0].Language = "eng"
	cand[0].Codec = "dts"
	if err := validateAudioPreservation(plan, src, cand); err == nil {
		t.Fatal("ignored compact mode accepted")
	}
	cand[0].Codec = "aac"
	cand[1].Codec = "mp3"
	if err := validateAudioPreservation(plan, src, cand); err == nil {
		t.Fatal("copied track changed codec")
	}
}

func TestCompactAudioMain10RealEncode(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	available, err := ProbeAvailableEncoders(context.Background(), ffmpeg)
	if err != nil || !available["libx265"] || !available["aac"] {
		t.Skip("libx265/AAC unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	candidate := filepath.Join(dir, "candidate.mkv")
	args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "nullsrc=size=320x180:rate=24:duration=12,format=yuv420p10le,geq=lum='64+mod(X+3*Y,877)':cb=512:cr=512", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=12", "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000:duration=12", "-map", "0:v", "-map", "1:a", "-map", "2:a", "-c:v", "ffv1", "-c:a", "flac", "-ac", "2", "-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=jpn", "-disposition:a:0", "0", "-disposition:a:1", "default", source}
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	probe, err := ProbeSourceDetails(context.Background(), ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	plan := &transcode.Plan{Container: "mkv", VideoCodec: "libx265", Quality: 23, Preset: "ultrafast", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, AudioMode: "compact", SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "test", RecipeDigest: "sha256:" + strings.Repeat("0", 64), Resilience: transcode.ResiliencePlan{MaxAttempts: 1}}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	args, err = BuildFFmpegArgs(ep, source, candidate, filepath.Join(dir, "progress"))
	if err != nil {
		t.Fatal(err)
	}
	args = append([]string{"-nostdin", "-v", "error"}, args...)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("encode: %v %s", err, out)
	}
	result, err := ProbeSourceDetails(context.Background(), ffprobe, candidate)
	if err != nil {
		t.Fatal(err)
	}
	video := firstStreamOfKind(result.Streams, "video")
	if video == nil || video.Codec != "hevc" || video.BitDepth != 10 {
		t.Fatalf("video not Main10: %+v", video)
	}
	if err := validateAudioPreservation(plan, probe.Streams, result.Streams); err != nil {
		t.Fatal(err)
	}
	audio := streamsOfKind(result.Streams, "audio")
	if len(audio) != 2 || audio[0].Codec != "aac" || audio[1].Codec != "aac" || audio[0].Language != "eng" || audio[1].Language != "jpn" {
		t.Fatalf("audio policy mismatch: %+v", audio)
	}
	if len(ep.Conversions) != 2 {
		t.Fatalf("missing conversion evidence: %+v", ep.Conversions)
	}
	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
	if err := w.validateAudioPackets(context.Background(), plan, source, candidate, probe, result); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []string{"atrim=duration=0.3", "asetpts=PTS+0.6/TB"} {
		broken := filepath.Join(t.TempDir(), "broken.mkv")
		out, err := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-i", candidate, "-map", "0", "-c:v", "copy", "-c:a", "aac", "-af", filter, broken).CombinedOutput()
		if err != nil {
			t.Fatalf("damaged fixture: %v %s", err, out)
		}
		badProbe, err := ProbeSourceDetails(context.Background(), ffprobe, broken)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.validateAudioPackets(context.Background(), plan, source, broken, probe, badProbe); err == nil {
			t.Fatalf("accepted %s", filter)
		}
	}
}

func TestNative10BitRealQualityProbe(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	filters, err := exec.Command(ffmpeg, "-hide_banner", "-filters").CombinedOutput()
	if err != nil || !strings.Contains(string(filters), "libvmaf") {
		t.Skip("libvmaf unavailable")
	}
	caps := probeQualityCapabilities(context.Background(), ffmpeg, t.TempDir())
	if !caps.Models["v1_1080p_3h"].Available && caps.ProbeError == "" {
		t.Skip("installed FFmpeg does not support VMAF v1")
	}
	if !caps.Native10Bit || !caps.CAMBIFullRef || caps.ProbeError != "" {
		t.Fatalf("native 10-bit executable probe failed: %+v", caps)
	}
}

func TestCompactAudioDispositionValidation(t *testing.T) {
	plan := &transcode.Plan{AudioMode: "compact"}
	src := []SourceStream{{Kind: "audio", Codec: "dts", Channels: 2, Language: "eng", Disposition: map[string]int{"default": 1, "comment": 0}}}
	cand := []SourceStream{{Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", Disposition: map[string]int{"default": 0, "comment": 0}}}
	if err := validateAudioPreservation(plan, src, cand); err == nil {
		t.Fatal("lost default track accepted")
	}
	cand[0].Disposition["default"] = 1
	if err := validateAudioPreservation(plan, src, cand); err != nil {
		t.Fatal(err)
	}
	cand[0].Disposition["comment"] = 1
	if err := validateAudioPreservation(plan, src, cand); err == nil {
		t.Fatal("invented commentary flag accepted")
	}
}

// A copied track keeps its bitstream; Matroska reports no layout for PCM, so
// a copy from MOV/MP4/AVI comes back without one and must still publish.
// Only AAC conversions are held to the source layout.
func TestAudioLayoutCheckedOnlyForAACConversions(t *testing.T) {
	for _, tc := range []struct {
		mode, codec, srcLayout, outCodec, outLayout string
		channels                                    int
		ok                                          bool
	}{
		{"copy", "pcm_s16le", "stereo", "pcm_s16le", "", 2, true},
		{"copy", "pcm_s24le", "5.1", "pcm_s24le", "", 6, true},
		{"compact", "pcm_s24le", "5.1", "aac", "5.1", 6, true},
		{"compact", "pcm_s24le", "5.1(side)", "aac", "5.1", 6, true},
		{"compact", "truehd", "7.1", "aac", "7.1(wide)", 8, false},
		{"compact", "eac3", "5.1", "eac3", "", 6, true}, // copied under compact
		// HE-AAC cannot keep 6.0 either: the track is copied, and an AAC
		// remap is rejected for every conversion, not only compact.
		{"he_aac", "pcm_s16le", "6.0", "aac", "hexagonal", 6, false},
		{"he_aac", "pcm_s16le", "6.0", "pcm_s16le", "", 6, true},
		{"he_aac", "pcm_s16le", "5.1", "aac", "5.1", 6, true},
		{"he_aac", "pcm_s16le", "5.1", "aac", "5.1(side)", 6, true},
		{"he_aac", "flac", "4.0", "aac", "quad", 4, false},
		{transcode.AudioModeCompactLossless, "truehd", "7.1", "aac", "7.1", 8, true},
		{transcode.AudioModeCompactLossless, "truehd", "7.1", "aac", "7.1(wide)", 8, false},
		{transcode.AudioModeCompactLossless, "pcm_s24le", "6.0", "pcm_s24le", "", 6, true}, // kept: AAC would remap 6.0
	} {
		plan := &transcode.Plan{AudioMode: tc.mode}
		src := []SourceStream{{Kind: "audio", Codec: tc.codec, Channels: tc.channels, Layout: tc.srcLayout}}
		cand := []SourceStream{{Kind: "audio", Codec: tc.outCodec, Channels: tc.channels, Layout: tc.outLayout}}
		if tc.mode == "he_aac" {
			cand[0].Profile = "HE-AAC"
		}
		if err := validateAudioPreservation(plan, src, cand); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

func TestConvertedAudioTitle(t *testing.T) {
	for _, tc := range []struct {
		title, layout string
		channels      int
		want          string
		ok            bool
	}{
		{"TrueHD Atmos 7.1", "7.1", 8, "AAC 7.1", true},
		{"DTS-HD MA 5.1", "5.1(side)", 6, "AAC 5.1", true},
		{"LPCM 5.1", "", 6, "AAC 5.1", true},
		{"FLAC 2.0", "stereo", 2, "AAC Stereo", true},
		{"Dolby TrueHD", "", 3, "AAC", true},
		{"Commentary", "stereo", 2, "", false},
		{"Director's Commentary DTS-HD MA 5.1", "5.1(side)", 6, "Director's Commentary AAC 5.1", true},
		{"Commentary (DTS-HD Master Audio 2.0)", "stereo", 2, "Commentary AAC Stereo", true},
		{"English - TrueHD Atmos 7.1", "7.1", 8, "English AAC 7.1", true},
		{"PCM Stereo", "", 2, "AAC Stereo", true},
		{"Audio Description FLAC 2.0", "stereo", 2, "Audio Description AAC Stereo", true},
		{"", "7.1", 8, "", false},
		// Codec variants, sample specs and whole-word matching.
		{"DTS-ES 5.1", "5.1(side)", 6, "AAC 5.1", true},
		{"LPCM 24-bit 2.0", "stereo", 2, "AAC Stereo", true},
		{"FLAC 24/96", "stereo", 2, "AAC Stereo", true},
		{"FLAC 24-bit/96kHz Stereo", "stereo", 2, "AAC Stereo", true},
		{"DTS-HD High Resolution Audio", "5.1", 6, "AAC 5.1", true},
		{"DTS-HD HRA 7.1", "7.1", 8, "AAC 7.1", true},
		{"Dolby Digital 5.1 Commentary", "5.1", 6, "Commentary AAC 5.1", true},
		{"Atmosphere", "stereo", 2, "", false},
		{"Atmospheric Score", "5.1", 6, "", false},
	} {
		got, ok := convertedAudioTitle(tc.title, tc.layout, tc.channels)
		if got != tc.want || ok != tc.ok {
			t.Errorf("convertedAudioTitle(%q, %q, %d) = %q, %v", tc.title, tc.layout, tc.channels, got, ok)
		}
	}
}

func TestCompactEncodeArgsClearTitlesAndDisableDolbyVision(t *testing.T) {
	useTempCompactLUTDir(t)
	plan := &transcode.Plan{VideoCodec: "libx265", Quality: 23, VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, AudioMode: "compact", Compact: compactPQPlan()}
	ep := &ExecutionPlan{Plan: plan, Streams: []StreamAction{
		{Kind: "audio", TypeIndex: 0, TargetCodec: "aac", BitrateKbps: 512, Title: "TrueHD Atmos 7.1", Channels: 8, Layout: "7.1"},
		{Kind: "audio", TypeIndex: 1, TargetCodec: "copy", Title: "E-AC-3 Atmos 5.1"},
	}}
	args, err := BuildFFmpegArgs(ep, "source.mkv", "candidate.mkv", "progress")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-dolbyvision:v:0 0", "-metadata:s:v:0 title=", "-metadata:s:v:0 BPS=", "-metadata:s:a:0 title=AAC 7.1", "-metadata:s:a:0 NUMBER_OF_BYTES=", "-b:a:0 512k"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q: %s", want, joined)
		}
	}
	// A copied track (E-AC-3 JOC keeps Atmos) keeps its title and tags.
	if strings.Contains(joined, "-metadata:s:a:1") {
		t.Fatalf("copied track rewritten: %s", joined)
	}
	vt := *plan
	vt.VideoCodec, vt.Quality = "hevc_videotoolbox", 65
	if args, err := BuildVideoEncoderArgs(&vt); err != nil || strings.Contains(strings.Join(args, " "), "dolbyvision") {
		t.Fatalf("VideoToolbox has no -dolbyvision option: %v %v", args, err)
	}
	plain := *plan
	plain.Compact = nil
	if args, _ := BuildVideoEncoderArgs(&plain); strings.Contains(strings.Join(args, " "), "dolbyvision") {
		t.Fatal("non-compact encodes keep the encoder default")
	}
}

// Mixed copy and convert tracks pair by audio index: each conversion keeps
// its own language, layout and disposition, and each copy its codec.
func TestCompactAudioMultiTrackPairing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		src     []SourceStream
		targets []string
		kbps    []int
	}{
		{"compact mixed", "compact", []SourceStream{
			{Index: 1, Kind: "audio", Codec: "pcm_s24le", Channels: 6, Language: "eng", Disposition: map[string]int{"default": 1}},
			{Index: 2, Kind: "audio", Codec: "eac3", Channels: 6, Layout: "", Language: "spa"},
			{Index: 3, Kind: "audio", Codec: "truehd", Channels: 8, Layout: "7.1", Language: "fra", Disposition: map[string]int{"comment": 1}},
		}, []string{"aac", "copy", "aac"}, []int{384, 0, 512}},
		{"size two DTS-HD MA", transcode.AudioModeCompactLossless, []SourceStream{
			{Index: 1, Kind: "audio", Codec: "dts", Profile: "DTS-HD MA", Channels: 6, Layout: "5.1(side)", Language: "jpn", Disposition: map[string]int{"default": 1}},
			{Index: 2, Kind: "audio", Codec: "dts", Profile: "DTS-HD MA", Channels: 6, Layout: "5.1(side)", Language: "eng"},
		}, []string{"aac", "aac"}, []int{384, 384}},
		{"size mixed", transcode.AudioModeCompactLossless, []SourceStream{
			{Index: 1, Kind: "audio", Codec: "pcm_s16le", Channels: 2, Language: "eng"},
			{Index: 2, Kind: "audio", Codec: "dts", Profile: "DTS", Channels: 6, Layout: "5.1(side)", Language: "deu"},
			{Index: 3, Kind: "audio", Codec: "eac3", Channels: 6, Layout: "", Language: "spa"},
			{Index: 4, Kind: "audio", Codec: "truehd", Channels: 8, Layout: "7.1", Language: "fra"},
			{Index: 5, Kind: "audio", Codec: "truehd", Profile: "Dolby TrueHD + Dolby Atmos", Channels: 8, Layout: "7.1", Language: "ita"},
		}, []string{"aac", "copy", "copy", "aac", "copy"}, []int{192, 0, 0, 512, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := sizeV3TestPlan(true)
			if tc.mode == "compact" {
				// The audio rules alone; compact's video chain is not involved.
				plan = &transcode.Plan{Container: "mkv", VideoCodec: "libx265", Quality: 23, AudioMode: "compact", SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "test", RecipeDigest: "sha256:" + strings.Repeat("0", 64), Resilience: transcode.ResiliencePlan{MaxAttempts: 1}}
				plan.PlanDigest, _ = transcode.DigestPlan(plan)
			}
			streams := append([]SourceStream{{Index: 0, Kind: "video", Codec: "h264", TypeIndex: 0}}, tc.src...)
			for i := range streams[1:] {
				streams[i+1].TypeIndex = i
			}
			ep, err := BuildExecutionPlan(plan, streams, 600)
			if err != nil {
				t.Fatal(err)
			}
			var audio []StreamAction
			for _, s := range ep.Streams {
				if s.Kind == "audio" {
					audio = append(audio, s)
				}
			}
			if len(audio) != len(tc.targets) {
				t.Fatalf("audio actions %d", len(audio))
			}
			cand := make([]SourceStream, len(tc.src))
			for i, a := range audio {
				if a.TargetCodec != tc.targets[i] || a.BitrateKbps != tc.kbps[i] || a.SourceIndex != tc.src[i].Index || a.TypeIndex != i {
					t.Fatalf("track %d: %+v", i, a)
				}
				cand[i] = tc.src[i]
				cand[i].Profile = ""
				if a.TargetCodec == "aac" {
					cand[i].Codec = "aac"
					cand[i].Layout = map[string]string{"5.1(side)": "5.1"}[tc.src[i].Layout]
					if cand[i].Layout == "" {
						cand[i].Layout = tc.src[i].Layout
					}
				}
			}
			if err := validateAudioPreservation(plan, tc.src, cand); err != nil {
				t.Fatal(err)
			}
			// Swapping two tracks' languages, or remapping one converted
			// layout, must be caught at its own index.
			swapped := append([]SourceStream(nil), cand...)
			swapped[0].Language, swapped[1].Language = cand[1].Language, cand[0].Language
			if err := validateAudioPreservation(plan, tc.src, swapped); err == nil {
				t.Fatal("swapped languages accepted")
			}
			last := len(cand) - 1
			for i := last; i >= 0; i-- {
				if audio[i].TargetCodec == "aac" && cand[i].Channels == 8 {
					remapped := append([]SourceStream(nil), cand...)
					remapped[i].Layout = "7.1(wide)"
					if err := validateAudioPreservation(plan, tc.src, remapped); err == nil {
						t.Fatalf("remapped layout at %d accepted", i)
					}
				}
			}
		})
	}
}
