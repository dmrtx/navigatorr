package transcode

import "testing"

func TestCompactAudioPolicy(t *testing.T) {
	for _, tc := range []struct {
		mode, codec string
		channels    int
		layout      string
		target      string
		kbps        int
	}{
		{"copy", "dts", 2, "stereo", "copy", 0}, {"compact", "dts", 2, "stereo", "aac", 192},
		{"compact", "flac", 1, "mono", "aac", 128}, {"compact", "truehd", 6, "5.1(side)", "aac", 384},
		{"compact", "pcm_s24le", 8, "7.1", "aac", 512}, {"compact", "truehd", 8, "7.1", "aac", 512},
		{"compact", "alac", 2, "stereo", "aac", 192},
		{"compact", "aac", 2, "stereo", "copy", 0}, {"compact", "eac3", 6, "5.1", "copy", 0},
		{"compact", "eac3", 8, "7.1", "copy", 0}, // E-AC-3 JOC (Atmos) is copied
		{"compact", "ac3", 6, "5.1", "copy", 0}, {"compact", "opus", 2, "stereo", "copy", 0},
		{"compact", "dts", 0, "", "copy", 0}, {"compact", "truehd", 16, "", "copy", 0},
		{"compact", "unknown", 2, "stereo", "copy", 0},
		// Layouts AAC would remap, or that disagree with the channel count,
		// are copied; an unreported layout (PCM in Matroska) converts.
		{"compact", "dts", 7, "6.1", "copy", 0}, {"compact", "truehd", 8, "7.1(wide-side)", "copy", 0},
		{"compact", "flac", 6, "7.1", "copy", 0}, {"compact", "pcm_s24le", 6, "", "aac", 384},
	} {
		target, kbps := AudioTarget(tc.mode, AudioTrack{Codec: tc.codec, Channels: tc.channels, Layout: tc.layout})
		if target != tc.target || kbps != tc.kbps {
			t.Errorf("%+v -> %s %d", tc, target, kbps)
		}
	}
}

func TestCompactLosslessAudioPolicy(t *testing.T) {
	for _, tc := range []struct {
		codec, profile string
		channels       int
		layout         string
		target         string
		kbps           int
	}{
		// Lossless tracks convert at compact's bitrates, keeping the layout.
		{"truehd", "", 6, "5.1(side)", "aac", 384}, {"truehd", "", 8, "7.1", "aac", 512},
		{"dts", "DTS-HD MA", 6, "5.1(side)", "aac", 384}, {"dts", "DTS-HD MA", 8, "7.1", "aac", 512},
		{"pcm_s24le", "", 6, "", "aac", 384}, {"pcm_s16le", "", 2, "stereo", "aac", 192},
		{"flac", "", 2, "stereo", "aac", 192}, {"alac", "", 1, "mono", "aac", 128},
		// Lossy tracks are copied: DTS core and DTS-HD HRA included, and a
		// DTS track whose profile is unreported counts as core.
		{"dts", "DTS", 6, "5.1(side)", "copy", 0}, {"dts", "DTS-HD HRA", 6, "5.1", "copy", 0},
		{"dts", "", 6, "5.1", "copy", 0}, {"dts", "DTS-ES", 7, "6.1(back)", "copy", 0},
		{"ac3", "", 6, "5.1", "copy", 0}, {"eac3", "", 6, "5.1", "copy", 0}, {"aac", "LC", 2, "stereo", "copy", 0},
		// Lossless object audio converts on its channel bed; lossy object
		// audio (E-AC-3 JOC) and layouts AAC would remap are copied.
		{"truehd", "Dolby TrueHD + Dolby Atmos", 8, "7.1", "aac", 512}, {"dts", "DTS-HD MA + DTS:X", 8, "7.1", "aac", 512},
		{"truehd", "Dolby TrueHD + Dolby Atmos", 6, "5.1", "aac", 384}, {"eac3", "Dolby Digital Plus + Dolby Atmos", 8, "7.1", "copy", 0},
		{"truehd", "Dolby TrueHD + Dolby Atmos", 7, "6.1", "copy", 0},
		{"dts", "DTS-HD MA", 7, "6.1", "copy", 0}, {"truehd", "", 16, "", "copy", 0},
	} {
		target, kbps := AudioTarget(AudioModeCompactLossless, AudioTrack{Codec: tc.codec, Profile: tc.profile, Channels: tc.channels, Layout: tc.layout})
		if target != tc.target || kbps != tc.kbps {
			t.Errorf("%+v -> %s %d", tc, target, kbps)
		}
	}
}

func TestSameAudioLayout(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{{"5.1(side)", "5.1", true}, {"7.1", "7.1", true}, {"", "", true}, {"", "5.1", true}, {"6.1", "6.1(back)", false}, {"7.1(wide-side)", "7.1(wide)", false}, {"7.1", "", false}} {
		if SameAudioLayout(tc.a, tc.b) != tc.same {
			t.Errorf("SameAudioLayout(%q, %q) != %v", tc.a, tc.b, tc.same)
		}
	}
}

func TestNative10BitRequiresExecutableQualityProof(t *testing.T) {
	caps := WorkerCapabilities{Quality: &QualityCapabilities{Native10Bit: true, CAMBIFullRef: true, Models: map[string]QualityModelCapability{"v1_1080p_3h": {Available: true}}}}
	if !caps.SupportsNative10BitQuality() {
		t.Fatal("verified native 10-bit unavailable")
	}
	caps.Quality.Native10Bit = false
	if caps.SupportsNative10BitQuality() {
		t.Fatal("legacy worker implicitly trusted")
	}
	caps.Quality.Native10Bit = true
	caps.Quality.ProbeError = "timeout"
	if caps.SupportsNative10BitQuality() {
		t.Fatal("failed probe implicitly trusted")
	}
}

// Size mode converts lossless object audio like any lossless track, whether
// the profile, title or handler names it; lossy object audio stays copied.
// Compact mode is unchanged.
func TestCompactLosslessConvertsLosslessObjectAudio(t *testing.T) {
	for _, tc := range []struct {
		track  AudioTrack
		target string
	}{
		{AudioTrack{Codec: "truehd", Title: "TrueHD Atmos 7.1"}, "aac"},
		{AudioTrack{Codec: "truehd", Handler: "Dolby Atmos"}, "aac"},
		{AudioTrack{Codec: "dts", Profile: "DTS-HD MA", Title: "DTS:X 7.1"}, "aac"},
		{AudioTrack{Codec: "dts", Profile: "DTS-HD MA", Title: "English DTS-X"}, "aac"},
		{AudioTrack{Codec: "dts", Profile: "DTS-HD MA", Handler: "DTS X"}, "aac"},
		{AudioTrack{Codec: "truehd", Title: "Atmosphere (TrueHD 7.1)"}, "aac"},
		{AudioTrack{Codec: "eac3", Title: "DD+ Atmos 7.1"}, "copy"},
		{AudioTrack{Codec: "eac3", Profile: "Dolby Digital Plus + Dolby Atmos"}, "copy"},
		{AudioTrack{Codec: "dts", Title: "DTS:X 7.1"}, "copy"},
	} {
		tc.track.Channels, tc.track.Layout = 8, "7.1"
		if target, _ := AudioTarget(AudioModeCompactLossless, tc.track); target != tc.target {
			t.Errorf("%+v -> %s, want %s", tc.track, target, tc.target)
		}
	}
	for _, tc := range []AudioTrack{{Codec: "truehd", Profile: "Dolby TrueHD + Dolby Atmos"}, {Codec: "eac3", Profile: "Dolby Digital Plus + Dolby Atmos"}} {
		tc.Channels, tc.Layout = 8, "7.1"
		want, _ := AudioTarget("compact", AudioTrack{Codec: tc.Codec, Channels: 8, Layout: "7.1"})
		if got, _ := AudioTarget("compact", tc); got != want {
			t.Errorf("compact changed for %+v: %s, want %s", tc, got, want)
		}
	}
	if got := AudioHandler(map[string]string{"HANDLER_NAME": " Dolby Atmos "}); got != "Dolby Atmos" {
		t.Fatalf("handler %q", got)
	}
}
