package transcodeworker

import "testing"

// Sources may be 4:2:0 at either range, 4:2:2 or 4:4:4: every encode writes
// 4:2:0 and the metric graph converts both inputs before measuring luma. The
// candidate stays 4:2:0; a full-range source gives a full-range candidate.
func TestSourceChromaClasses(t *testing.T) {
	for _, c := range []struct {
		pixFmt  string
		depth   int
		source  bool
		encoded bool
	}{
		{"yuv420p", 8, true, true},
		{"nv12", 8, true, true},
		{"yuv420p10le", 10, true, true},
		{"p010le", 10, true, true},
		{"yuvj420p", 8, true, true},
		{"yuv422p", 8, true, false},
		{"yuvj422p", 8, true, false},
		{"yuv444p", 8, true, false},
		{"yuvj444p", 8, true, false},
		{"yuv422p10le", 10, true, false},
		{"yuv444p10le", 10, true, false},
		{"yuv444p10le", 8, false, false},
		{"yuv420p", 10, false, false},
		{"yuv422p12le", 12, false, false},
		{"gbrp", 8, false, false},
		{"gray", 8, false, false},
	} {
		if got := isMeasurableSourceChroma(c.pixFmt, c.depth); got != c.source {
			t.Errorf("source %s/%d: got %v, want %v", c.pixFmt, c.depth, got, c.source)
		}
		if got := isSupportedSourceChroma(c.pixFmt, c.depth); got != c.encoded {
			t.Errorf("candidate %s/%d: got %v, want %v", c.pixFmt, c.depth, got, c.encoded)
		}
	}
}

// An encode requested as yuv420p keeps a full-range source's range, so the
// candidate probes as yuvj420p; that is the same 8-bit 4:2:0 family.
func TestFullRangeCandidateMatchesRequestedFormat(t *testing.T) {
	if err := pixelFormatCompatible("yuv420p", "yuvj420p", 8); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"yuv422p", "p010le", "yuvj444p"} {
		if err := pixelFormatCompatible("yuv420p", bad, 8); err == nil {
			t.Errorf("%s accepted for yuv420p", bad)
		}
	}
}
