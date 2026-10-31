package mediainspect

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A late subtitle keeps the container running six seconds past its video,
// as an attachment did in the library file whose final windows misaligned.
func TestMeasureVideoSecondsFindsTheVideoEndInsideALongerContainer(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	srt := filepath.Join(dir, "late.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:08,000 --> 00:00:10,000\nlate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	long, full := filepath.Join(dir, "long.mkv"), filepath.Join(dir, "full.mkv")
	for _, args := range [][]string{
		{"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=4", "-i", srt, "-map", "0", "-map", "1", "-c:v", "mpeg4", "-c:s", "srt", "-y", long},
		{"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=40", "-c:v", "mpeg4", "-y", full},
	} {
		if out, err := exec.Command(ffmpeg, append([]string{"-nostdin", "-v", "error"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("creating test media: %v: %s", err, out)
		}
	}
	for _, c := range []struct {
		path      string
		container float64
		want      float64
	}{{long, 10, 4}, {full, 40, 40}} {
		rep, err := InspectDetailed(context.Background(), ffprobe, c.path)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(rep.DurationSec-c.container) > 0.05 {
			t.Fatalf("%s: container duration %.3f, want %.0f", c.path, rep.DurationSec, c.container)
		}
		seconds, err := MeasureVideoSeconds(context.Background(), ffprobe, c.path, rep.MainVideo()[0].Index, rep.DurationSec)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(seconds-c.want) > 0.05 {
			t.Fatalf("%s: video seconds %.3f, want %.0f", c.path, seconds, c.want)
		}
	}
}

func TestWindowSecondsKeepsTheContainerUnlessTheVideoIsShorter(t *testing.T) {
	for _, c := range []struct{ duration, video, want float64 }{
		{100, 0, 100},      // not measured
		{100, 99.5, 100},   // a frame or two short: windows unchanged
		{100, 100.04, 100}, // the last frame's end
		{2115.59, 1855.87, 1855.87},
	} {
		rep := DetailedReport{DurationSec: c.duration, VideoSeconds: c.video}
		if got := rep.WindowSeconds(); got != c.want {
			t.Fatalf("WindowSeconds(%v, %v) = %v, want %v", c.duration, c.video, got, c.want)
		}
	}
}
