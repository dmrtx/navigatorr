package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// pinnedLevelsFixtureFingerprint is the fixture machine once it also runs the
// size goal's audio levels. It differs from pinnedFixtureFingerprint on
// purpose: a worker that gains the capability is a different machine to the
// server, which is how it knows to plan level plans for it and to keep old
// snapshots (frozen without the capability) valid on old workers. A machine
// without the capability keeps the old pin.
const pinnedLevelsFixtureFingerprint = "sha256:9e55e96177c29a37335f87714f2d63ad998eef7308e37d170c7494cdcbfe97f9"

func TestCapabilityFingerprintCoversTheAudioLevels(t *testing.T) {
	caps := fingerprintFixture()
	caps.SizeAudioLevels = true
	fp, err := transcode.ComputeCapabilityFingerprint(caps)
	if err != nil {
		t.Fatal(err)
	}
	if fp == pinnedFixtureFingerprint {
		t.Fatal("a worker with the audio levels has the fingerprint of one without")
	}
	if fp != pinnedLevelsFixtureFingerprint {
		t.Fatalf("levels fingerprint moved: got %s, want %s", fp, pinnedLevelsFixtureFingerprint)
	}
	caps.SizeAudioLevels = false
	if old, _ := transcode.ComputeCapabilityFingerprint(caps); old != pinnedFixtureFingerprint {
		t.Fatalf("a worker without the audio levels moved: %s", old)
	}
}

// sizeV5TestPlan is a complete size v5 plan carrying one of the audio levels.
func sizeV5TestPlan(mode string) *transcode.Plan {
	p := sizeV3TestPlan(true)
	p.ModePolicyVersion, p.AudioMode = transcode.ModePolicyV5, mode
	p.PlanDigest, _ = transcode.DigestPlan(p)
	return p
}

// Plan version 5 admits the two levels and nothing else for audio, and the
// levels are admitted by no other version or goal.
func TestSizeV5PlanAdmitsOnlyTheAudioLevels(t *testing.T) {
	for _, mode := range transcode.AudioLevelModes {
		if err := ValidatePlan(sizeV5TestPlan(mode)); err != nil {
			t.Fatalf("%s plan rejected: %v", mode, err)
		}
	}
	for name, mutate := range map[string]func(*transcode.Plan){
		"v5 compacting only lossless": func(p *transcode.Plan) { p.AudioMode = transcode.AudioModeCompactLossless },
		"v5 copying audio":            func(p *transcode.Plan) { p.AudioMode = "copy" },
		"v5 lossy compact":            func(p *transcode.Plan) { p.AudioMode = "compact" },
		"v3 with a level":             func(p *transcode.Plan) { p.ModePolicyVersion = transcode.ModePolicyV3 },
		"v2 with a level":             func(p *transcode.Plan) { p.ModePolicyVersion = transcode.ModePolicyV2 },
		"quality goal with a level":   func(p *transcode.Plan) { p.Mode = "quality" },
		"unknown level":               func(p *transcode.Plan) { p.AudioMode = "size_opus" },
	} {
		p := sizeV5TestPlan(transcode.AudioModeSizeAAC)
		mutate(p)
		p.PlanDigest, _ = transcode.DigestPlan(p)
		if err := ValidatePlan(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// The mode is part of the plan's identity: swapping the level of a signed
	// plan without re-signing it is caught by the digest.
	p := sizeV5TestPlan(transcode.AudioModeSizeAAC)
	p.AudioMode = transcode.AudioModeSizeHEAAC
	if err := ValidatePlan(p); err == nil {
		t.Error("a plan whose level changed after signing was accepted")
	}
}

func levelSource() []SourceStream {
	return []SourceStream{
		{Kind: "video", Index: 0, TypeIndex: 0, Codec: "h264", Width: 1920, Height: 1080},
		{Kind: "audio", Index: 1, TypeIndex: 0, Codec: "eac3", Channels: 6, Layout: "5.1(side)", Language: "eng", BitrateKbps: 640},
		{Kind: "audio", Index: 2, TypeIndex: 1, Codec: "aac", Profile: "LC", Channels: 2, Layout: "stereo", Language: "spa", BitrateKbps: 128},
		{Kind: "audio", Index: 3, TypeIndex: 2, Codec: "truehd", Channels: 8, Layout: "7.1", Language: "ita", BitrateKbps: 4000},
		{Kind: "audio", Index: 4, TypeIndex: 3, Codec: "eac3", Profile: "Dolby Digital Plus + Dolby Atmos", Channels: 6, Layout: "5.1", Language: "fra", BitrateKbps: 768},
	}
}

// The execution plan and the ffmpeg arguments follow the level: AAC-LC and
// HE-AAC through AudioToolbox's encoder (profile 2 and 4) where it keeps the
// layout, FFmpeg's native encoder for 7.1, and a track under 1.25 times its
// target or E-AC-3 JOC copied. Track order is the source's.
func TestSizeAudioLevelsExecutionPlanAndArguments(t *testing.T) {
	type want struct {
		target  string
		kbps    int
		heaac   bool
		encoder string
		profile string // the -profile:a value, empty for the native encoder
	}
	for mode, tracks := range map[string][]want{
		transcode.AudioModeSizeAAC:   {{"aac", 384, false, "aac_at", "2"}, {"copy", 0, false, "", ""}, {"aac", 512, false, "aac", ""}, {"copy", 0, false, "", ""}},
		transcode.AudioModeSizeHEAAC: {{"aac", 160, true, "aac_at", "4"}, {"aac", 64, true, "aac_at", "4"}, {"aac", 512, false, "aac", ""}, {"copy", 0, false, "", ""}},
	} {
		ep, err := BuildExecutionPlan(sizeV5TestPlan(mode), levelSource(), 1800)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		args, err := BuildFFmpegArgs(ep, "source.mkv", "candidate.mkv", "progress")
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		joined := " " + strings.Join(args, " ") + " "
		var audio []StreamAction
		for _, s := range ep.Streams {
			if s.Kind == "audio" {
				audio = append(audio, s)
			}
		}
		if len(audio) != len(tracks) {
			t.Fatalf("%s: %d audio actions", mode, len(audio))
		}
		for i, w := range tracks {
			a := audio[i]
			if a.TypeIndex != i || a.TargetCodec != w.target || a.BitrateKbps != w.kbps || a.HEAAC != w.heaac {
				t.Errorf("%s track %d: %+v, want %+v", mode, i, a, w)
			}
			if w.target == "copy" {
				if strings.Contains(joined, fmt.Sprintf(" -c:a:%d ", i)) {
					t.Errorf("%s track %d is copied but has its own encoder", mode, i)
				}
				continue
			}
			if !strings.Contains(joined, fmt.Sprintf(" -c:a:%d %s ", i, w.encoder)) || !strings.Contains(joined, fmt.Sprintf(" -b:a:%d %dk ", i, w.kbps)) {
				t.Errorf("%s track %d: %s", mode, i, joined)
			}
			for _, profile := range []string{"2", "4"} {
				if got := strings.Contains(joined, fmt.Sprintf(" -profile:a:%d %s ", i, profile)); got != (profile == w.profile) {
					t.Errorf("%s track %d profile %s flag %v, want %q: %s", mode, i, profile, got, w.profile, joined)
				}
			}
		}
		if strings.Contains(joined, " -ac ") || strings.Contains(joined, " -ar ") {
			t.Errorf("%s: downmix or resample requested: %s", mode, joined)
		}
	}
}

// A lossy track of a bitrate the probe could not read is copied, never guessed.
func TestSizeAudioLevelsCopyTracksOfUnknownBitrate(t *testing.T) {
	src := []SourceStream{
		{Kind: "video", Index: 0, Codec: "h264", Width: 1920, Height: 1080},
		{Kind: "audio", Index: 1, TypeIndex: 0, Codec: "eac3", Channels: 6, Layout: "5.1", Language: "eng"},
	}
	ep, err := BuildExecutionPlan(sizeV5TestPlan(transcode.AudioModeSizeAAC), src, 1800)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ep.Streams {
		if s.Kind == "audio" && s.TargetCodec != "copy" {
			t.Fatalf("a lossy track of unknown bitrate was converted: %+v", s)
		}
	}
}

// The worker holds each candidate track to its level: codec, profile, track
// count, language, channels and layout.
func TestSizeAudioLevelsCandidateValidation(t *testing.T) {
	src := levelSource()[1:]
	candidate := func(profileA, profileB string) []SourceStream {
		return []SourceStream{
			{Kind: "audio", Codec: "aac", Profile: profileA, Channels: 6, Layout: "5.1", Language: "eng"},
			{Kind: "audio", Codec: "aac", Profile: profileB, Channels: 2, Layout: "stereo", Language: "spa"},
			{Kind: "audio", Codec: "aac", Profile: "LC", Channels: 8, Layout: "7.1", Language: "ita"},
			{Kind: "audio", Codec: "eac3", Profile: "Dolby Digital Plus + Dolby Atmos", Channels: 6, Layout: "5.1", Language: "fra"},
		}
	}
	level2 := sizeV5TestPlan(transcode.AudioModeSizeHEAAC)
	if err := validateAudioPreservation(level2, src, candidate("HE-AAC", "HE-AAC")); err != nil {
		t.Fatalf("level 2 candidate rejected: %v", err)
	}
	if err := validateAudioPreservation(level2, src, candidate("LC", "HE-AAC")); err == nil {
		t.Error("a level 2 track encoded as AAC-LC was accepted")
	}
	// At level 1 the stereo AAC 128 is copied and the 5.1 track is AAC-LC.
	level1 := sizeV5TestPlan(transcode.AudioModeSizeAAC)
	one := candidate("LC", "LC")
	if err := validateAudioPreservation(level1, src, one); err != nil {
		t.Fatalf("level 1 candidate rejected: %v", err)
	}
	if err := validateAudioPreservation(level1, src, candidate("HE-AAC", "LC")); err == nil {
		t.Error("a level 1 track encoded as HE-AAC was accepted")
	}
	wrong := candidate("LC", "LC")
	wrong[0].Codec = "eac3"
	if err := validateAudioPreservation(level1, src, wrong); err == nil {
		t.Error("a track left unconverted was accepted")
	}
	missing := candidate("LC", "LC")[:3]
	if err := validateAudioPreservation(level1, src, missing); err == nil {
		t.Error("a missing track was accepted")
	}
	swapped := candidate("LC", "LC")
	swapped[0].Language, swapped[1].Language = "spa", "eng"
	if err := validateAudioPreservation(level1, src, swapped); err == nil {
		t.Error("swapped languages were accepted")
	}
	remapped := candidate("LC", "LC")
	remapped[2].Layout = "7.1(wide)"
	if err := validateAudioPreservation(level1, src, remapped); err == nil {
		t.Error("a remapped layout was accepted")
	}
}

// A real encode at each level: the E-AC-3 640 kbps 5.1 track becomes AAC-LC
// near 384 kbps or HE-AAC near 160 kbps, the AAC stereo track is copied at level
// 1 and HE-AAC at level 2, and the worker's own validation accepts exactly the
// level it planned, not the other one.
func TestSizeAudioLevelsRealEncode(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	available, err := ProbeAvailableEncoders(ctx, ffmpeg)
	if err != nil || !available["libx265"] || !available["libx264"] || !available["aac"] || !available["aac_at"] || !available["eac3"] {
		t.Skip("libx265/libx264/AAC/AudioToolbox AAC/E-AC-3 unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	args := []string{"-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=14",
		"-f", "lavfi", "-i", "aevalsrc=sin(440*2*PI*t)|sin(550*2*PI*t)|sin(660*2*PI*t)|sin(110*2*PI*t)|sin(770*2*PI*t)|sin(880*2*PI*t):s=48000:c=5.1:d=14",
		"-f", "lavfi", "-i", "sine=frequency=330:sample_rate=48000:duration=14",
		"-map", "0:v", "-map", "1:a", "-map", "2:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "18", "-pix_fmt", "yuv420p",
		"-c:a:0", "eac3", "-b:a:0", "640k", "-c:a:1", "aac", "-ac:a:1", "2", "-b:a:1", "128k",
		"-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=jpn", "-metadata:s:a:1", "BPS=128000", source}
	if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("source fixture: %v %s", err, out)
	}
	srcProbe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	if audio := streamsOfKind(srcProbe.Streams, "audio"); len(audio) != 2 || audio[0].BitrateKbps < 400 || audio[1].BitrateKbps < 80 || audio[1].BitrateKbps > 160 {
		t.Fatalf("source audio bitrates %+v", audio)
	}
	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
	candidates := map[string]string{}
	for _, mode := range transcode.AudioLevelModes {
		plan := sizeV5TestPlan(mode)
		plan.Preset = "ultrafast"
		windows := transcode.PlanFinalValidationWindows(srcProbe.DurationSec, 1, 2.4, 5, 12)
		plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "ssim", Samples: windows, Quality: transcode.BenchmarkQualityConfig{FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}, BenchmarkRequestDigest: "sha256:" + strings.Repeat("2", 64)}
		plan.PlanDigest, _ = transcode.DigestPlan(plan)
		ep, err := BuildExecutionPlan(plan, srcProbe.Streams, srcProbe.DurationSec)
		if err != nil {
			t.Fatal(err)
		}
		jobDir := filepath.Join(dir, mode)
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		candidate := filepath.Join(jobDir, "candidate.mkv")
		if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(jobDir, "progress"), filepath.Join(jobDir, "ffmpeg.log")); err != nil {
			log, _ := os.ReadFile(filepath.Join(jobDir, "ffmpeg.log"))
			t.Fatalf("%s encode: %v\n%s", mode, err, log)
		}
		candidates[mode] = candidate
		candProbe, err := ProbeSourceDetails(ctx, ffprobe, candidate)
		if err != nil {
			t.Fatal(err)
		}
		audio := streamsOfKind(candProbe.Streams, "audio")
		if len(audio) != 2 || audio[0].Codec != "aac" || audio[0].Channels != 6 || audio[0].Language != "eng" || audio[1].Language != "jpn" || audio[1].Channels != 2 {
			t.Fatalf("%s: audio %+v", mode, audio)
		}
		if mode == transcode.AudioModeSizeAAC && (audio[0].Profile != "LC" || audio[1].Profile != "LC") {
			t.Fatalf("level 1 profiles %q %q", audio[0].Profile, audio[1].Profile)
		}
		if mode == transcode.AudioModeSizeHEAAC && (audio[0].Profile != "HE-AAC" || audio[1].Profile != "HE-AAC") {
			t.Fatalf("level 2 profiles %q %q", audio[0].Profile, audio[1].Profile)
		}
		if _, err := w.validateEncodedCandidateFull(ctx, candidate, ep, srcProbe, 0); err != nil {
			t.Fatalf("%s: pre-publish validation: %v", mode, err)
		}
	}
	// The candidate of one level is not the other level's: the profile and the
	// bitrate each tell them apart.
	for mode, other := range map[string]string{transcode.AudioModeSizeAAC: transcode.AudioModeSizeHEAAC, transcode.AudioModeSizeHEAAC: transcode.AudioModeSizeAAC} {
		ep, err := BuildExecutionPlan(sizeV5TestPlan(other), srcProbe.Streams, srcProbe.DurationSec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.validateEncodedCandidateFull(ctx, candidates[mode], ep, srcProbe, 0); err == nil {
			t.Errorf("the %s candidate passed the %s plan", mode, other)
		}
	}
}

// The bitrate of a converted track is measured from its packets, the one place
// that shows what the encoder really wrote: a level plan always asks for it,
// and the ceiling is the nominal bitrate's, so a 160 kbps HE-AAC track that
// came out at 384 kbps is refused while ordinary variable-bitrate swing is not.
func TestSizeAudioLevelsBitrateIsMeasuredByPacket(t *testing.T) {
	for _, mode := range transcode.AudioLevelModes {
		if !needsAudioPacketValidation(sizeV5TestPlan(mode), SourceProbe{Streams: levelSource()}) {
			t.Errorf("%s plan skips the packet bitrate check", mode)
		}
	}
	if needsAudioPacketValidation(sizeV5TestPlan(transcode.AudioModeSizeAAC), SourceProbe{Streams: levelSource()[:1]}) {
		t.Error("a source without audio asks for the packet check")
	}
	if ceiling := transcode.AudioBitrateCeilingBps(160); ceiling < 200_000 || ceiling >= 384_000 {
		t.Errorf("ceiling for a 160 kbps track is %d bps", ceiling)
	}
	if ceiling := transcode.AudioBitrateCeilingBps(64); ceiling < 128_000 || ceiling >= 192_000 {
		t.Errorf("ceiling for a 64 kbps track is %d bps", ceiling)
	}
}

// The worker advertises the audio levels only when it has both AAC encoders:
// the native one for AAC-LC and AudioToolbox's for HE-AAC.
func TestProbedCapabilitiesAdvertiseTheAudioLevels(t *testing.T) {
	for name, tc := range map[string]struct {
		encoders string
		want     bool
	}{
		"both encoders": {" A..... aac                  AAC\n A..... aac_at               AudioToolbox AAC\n", true},
		"no HE-AAC":     {" A..... aac                  AAC\n", false},
		"no AAC":        {" A..... aac_at               AudioToolbox AAC\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			ffmpeg := filepath.Join(dir, "fake_ffmpeg.sh")
			script := "#!/bin/sh\ncase \"$*\" in\n  *\"-version\"*) echo \"ffmpeg version 7.1\" ;;\n  *\"-encoders\"*) cat <<'EOF2'\nEncoders:\n V..... libx265 x265\n" + tc.encoders + "EOF2\n ;;\n  *\"-filters\"*) cat <<'EOF2'\nFilters:\n ... scale V->V Scale\nEOF2\n ;;\n  *) exit 1 ;;\nesac\n"
			if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			caps, err := ProbeWorkerCapabilities(context.Background(), ffmpeg)
			if err != nil {
				t.Fatal(err)
			}
			if caps.SizeAudioLevels != tc.want {
				t.Fatalf("size_audio_levels = %v, want %v (compact %v, he-aac %v)", caps.SizeAudioLevels, tc.want, caps.CompactAudio, caps.HEAACAudio)
			}
		})
	}
}
