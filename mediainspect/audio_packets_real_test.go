package mediainspect

import (
	"context"
	"math"
	"os/exec"
	"path/filepath"
	"testing"
)

// Real files measure as long as they play. A transport stream whose 33-bit
// clock wraps mid-file is read with the wrap undone (the times before it come
// back negative, and the span is the file's, not a day short or long), and a
// TrueHD track in Matroska, whose packets state no duration, ends one packet
// gap after its last packet start: never more than a millisecond past the
// sound.
func TestRealAudioPacketSpans(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx := context.Background()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.CommandContext(ctx, ffmpeg, append([]string{"-nostdin", "-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	wrap := filepath.Join(dir, "wrap.ts")
	// 2^33 / 90 kHz is 95443.7 s: the clock wraps 10.7 s into this file.
	run("-f", "lavfi", "-i", "testsrc2=size=64x64:rate=25:duration=20", "-f", "lavfi", "-i", "sine=duration=20",
		"-c:v", "libx264", "-g", "25", "-c:a", "aac", "-output_ts_offset", "95433", wrap)
	truehd := filepath.Join(dir, "truehd.mkv")
	run("-f", "lavfi", "-i", "sine=duration=10:sample_rate=48000", "-ac", "2", "-c:a", "truehd", "-strict", "-2", truehd)

	for _, tc := range []struct {
		path    string
		seconds float64
		within  float64
	}{
		{path: wrap, seconds: 20, within: 0.05},
		{path: truehd, seconds: 10, within: 0.002},
	} {
		spans, err := MeasureAudioPackets(ctx, ffprobe, tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if len(spans) != 1 {
			t.Fatalf("%s: %d audio spans", filepath.Base(tc.path), len(spans))
		}
		for _, s := range spans {
			if math.Abs(s.Seconds()-tc.seconds) > tc.within || s.Untimed != 0 || s.Discarded != 0 || s.Timed != s.Packets {
				t.Errorf("%s: span %+v (%.4fs), want %.0fs of timed packets", filepath.Base(tc.path), s, s.Seconds(), tc.seconds)
			}
		}
	}
}
