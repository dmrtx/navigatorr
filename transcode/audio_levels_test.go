package transcode

import "testing"

// The size goal's levels are one per-track rule each. A track at or below its
// level's target is copied; E-AC-3 JOC, layouts AAC cannot keep, more than 8
// channels and a lossy track of unknown bitrate are copied at every level.
func TestSizeAudioLevelsPerTrackRule(t *testing.T) {
	type want struct {
		out  string // copy, aac, he_aac
		kbps int
	}
	tracks := []struct {
		name   string
		track  AudioTrack
		l1, l2 want
	}{
		{"E-AC-3 5.1 640", AudioTrack{Codec: "eac3", Channels: 6, Layout: "5.1(side)", BitrateKbps: 640}, want{"aac", 384}, want{"he_aac", 160}},
		{"AC-3 5.1 448", AudioTrack{Codec: "ac3", Channels: 6, Layout: "5.1", BitrateKbps: 448}, want{"aac", 384}, want{"he_aac", 160}},
		{"AC-3 5.1 384 is at its target", AudioTrack{Codec: "ac3", Channels: 6, Layout: "5.1", BitrateKbps: 384}, want{"copy", 0}, want{"he_aac", 160}},
		{"AAC 5.1 256", AudioTrack{Codec: "aac", Profile: "LC", Channels: 6, Layout: "5.1", BitrateKbps: 256}, want{"copy", 0}, want{"he_aac", 160}},
		{"AC-3 stereo 224", AudioTrack{Codec: "ac3", Channels: 2, Layout: "stereo", BitrateKbps: 224}, want{"aac", 192}, want{"he_aac", 64}},
		{"AAC stereo 128", AudioTrack{Codec: "aac", Profile: "LC", Channels: 2, Layout: "stereo", BitrateKbps: 128}, want{"copy", 0}, want{"he_aac", 64}},
		{"AAC stereo 64 is at the HE-AAC target", AudioTrack{Codec: "aac", Channels: 2, Layout: "stereo", BitrateKbps: 64}, want{"copy", 0}, want{"copy", 0}},
		{"AAC mono 96", AudioTrack{Codec: "aac", Channels: 1, Layout: "mono", BitrateKbps: 96}, want{"copy", 0}, want{"he_aac", 64}},
		{"TrueHD 7.1", AudioTrack{Codec: "truehd", Channels: 8, Layout: "7.1", BitrateKbps: 4000}, want{"aac", 512}, want{"aac", 512}},
		{"TrueHD 5.1", AudioTrack{Codec: "truehd", Channels: 6, Layout: "5.1(side)", BitrateKbps: 3000}, want{"aac", 384}, want{"he_aac", 160}},
		{"FLAC stereo of unknown bitrate", AudioTrack{Codec: "flac", Channels: 2, Layout: "stereo"}, want{"aac", 192}, want{"he_aac", 64}},
		{"lossy 7.1 1024", AudioTrack{Codec: "eac3", Channels: 8, Layout: "7.1", BitrateKbps: 1024}, want{"aac", 512}, want{"aac", 512}},
		{"lossy 7.1 at its target", AudioTrack{Codec: "ac3", Channels: 8, Layout: "7.1", BitrateKbps: 512}, want{"copy", 0}, want{"copy", 0}},
		{"E-AC-3 JOC", AudioTrack{Codec: "eac3", Profile: "Dolby Digital Plus + Dolby Atmos", Channels: 6, Layout: "5.1", BitrateKbps: 768}, want{"copy", 0}, want{"copy", 0}},
		{"lossy of unknown bitrate", AudioTrack{Codec: "eac3", Channels: 6, Layout: "5.1"}, want{"copy", 0}, want{"copy", 0}},
		{"layout AAC cannot keep", AudioTrack{Codec: "ac3", Channels: 7, Layout: "6.1", BitrateKbps: 640}, want{"copy", 0}, want{"copy", 0}},
		{"more than 8 channels", AudioTrack{Codec: "truehd", Channels: 16, BitrateKbps: 5000}, want{"copy", 0}, want{"copy", 0}},
	}
	read := func(out AudioOutput) want {
		switch {
		case out.Codec == "copy":
			return want{"copy", 0}
		case out.HEAAC:
			return want{"he_aac", out.Kbps}
		}
		return want{"aac", out.Kbps}
	}
	for _, tc := range tracks {
		if got := read(AudioOutputFor(AudioModeSizeAAC, tc.track)); got != tc.l1 {
			t.Errorf("%s at level 1: %+v, want %+v", tc.name, got, tc.l1)
		}
		if got := read(AudioOutputFor(AudioModeSizeHEAAC, tc.track)); got != tc.l2 {
			t.Errorf("%s at level 2: %+v, want %+v", tc.name, got, tc.l2)
		}
	}
	// Level 0 is unchanged: a lossy track is copied whatever its bitrate.
	if out := AudioOutputFor(AudioModeCompactLossless, AudioTrack{Codec: "eac3", Channels: 6, Layout: "5.1", BitrateKbps: 640}); out.Codec != "copy" {
		t.Errorf("level 0 converted a lossy track: %+v", out)
	}
	if !IsAudioLevelMode(AudioModeSizeAAC) || !IsAudioLevelMode(AudioModeSizeHEAAC) || IsAudioLevelMode(AudioModeCompactLossless) || IsAudioLevelMode("compact") {
		t.Error("IsAudioLevelMode")
	}
}

// A level never decides from a bitrate the probe did not state, and the
// coordinator and the worker read it the same way.
func TestSourceAudioKbps(t *testing.T) {
	if got := SourceAudioKbps(640_000, nil, 0); got != 640 {
		t.Errorf("bit_rate: %d", got)
	}
	if got := SourceAudioKbps(0, map[string]string{"NUMBER_OF_BYTES": "144000000"}, 1800); got != 640 {
		t.Errorf("bytes over duration: %d", got)
	}
	if got := SourceAudioKbps(0, map[string]string{"NUMBER_OF_BYTES-eng": "144000000"}, 1800); got != 640 {
		t.Errorf("language variant: %d", got)
	}
	if got := SourceAudioKbps(0, nil, 1800); got != 0 {
		t.Errorf("unknown: %d", got)
	}
	if got := SourceAudioKbps(0, map[string]string{"NUMBER_OF_BYTES": "144000000"}, 0); got != 0 {
		t.Errorf("no duration: %d", got)
	}
}

// Plan version 3 keeps its one audio mode and version 5 takes the levels only,
// so a plan cannot carry the audio of the other version.
func TestSizePolicyVersionsAndAudioModes(t *testing.T) {
	for _, tc := range []struct {
		version, mode string
		ok            bool
	}{
		{ModePolicyV3, AudioModeCompactLossless, true}, {ModePolicyV3, AudioModeSizeAAC, false}, {ModePolicyV3, AudioModeSizeHEAAC, false},
		{ModePolicyV5, AudioModeSizeAAC, true}, {ModePolicyV5, AudioModeSizeHEAAC, true}, {ModePolicyV5, AudioModeCompactLossless, false},
		{ModePolicyV2, AudioModeSizeAAC, false}, {"", AudioModeSizeAAC, false},
	} {
		if SizeAudioMode(tc.version, tc.mode) != tc.ok {
			t.Errorf("%s with %s: want %v", tc.version, tc.mode, tc.ok)
		}
	}
	if !IsSizePolicyVersion(ModePolicyV3) || !IsSizePolicyVersion(ModePolicyV5) || IsSizePolicyVersion(ModePolicyV2) {
		t.Error("IsSizePolicyVersion")
	}
	if got, ok := ModeMinimumSavings("size", ModePolicyV5); !ok || got != 15 {
		t.Errorf("v5 savings %d %v", got, ok)
	}
	if _, ok := ModeMinimumSavings("quality", ModePolicyV5); ok {
		t.Error("v5 admitted a goal other than size")
	}
}
