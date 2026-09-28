package transcode

import "testing"

func TestCompactAudioPolicy(t *testing.T) {
	for _, tc := range []struct {
		mode, codec string
		channels    int
		target      string
		kbps        int
	}{
		{"copy", "dts", 2, "copy", 0}, {"compact", "dts", 2, "aac", 192},
		{"compact", "flac", 1, "aac", 128}, {"compact", "truehd", 6, "aac", 384},
		{"compact", "pcm_s24le", 8, "aac", 512}, {"compact", "alac", 2, "aac", 192},
		{"compact", "aac", 2, "copy", 0}, {"compact", "eac3", 6, "copy", 0},
		{"compact", "ac3", 6, "copy", 0}, {"compact", "opus", 2, "copy", 0},
		{"compact", "dts", 0, "copy", 0}, {"compact", "truehd", 16, "copy", 0},
		{"compact", "unknown", 2, "copy", 0},
	} {
		target, kbps := AudioTarget(tc.mode, tc.codec, tc.channels)
		if target != tc.target || kbps != tc.kbps {
			t.Errorf("%+v -> %s %d", tc, target, kbps)
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
