package action

import "testing"

// TestExpectedVideoCodecMapsEncodersToProbeName locks the mapping used by
// transcode validation. ffprobe reports codec_name "hevc" for both the
// hevc_videotoolbox and libx265 encoders, so a libx265 plan must not be
// compared against the literal "libx265" name.
func TestExpectedVideoCodecMapsEncodersToProbeName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hevc_videotoolbox", "hevc"},
		{"libx265", "hevc"},
		{"HEVC_VideoToolbox", "hevc"},
		{"  libx265  ", "hevc"},
		{"h264", "h264"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := expectedVideoCodec(tc.in); got != tc.want {
			t.Errorf("expectedVideoCodec(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
