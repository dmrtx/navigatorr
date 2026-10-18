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
		target, kbps := AudioTarget(tc.mode, tc.codec, tc.channels, tc.layout)
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
