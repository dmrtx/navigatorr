package transcodeworker

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A progressive H.264 source whose rate rises after its head (30 or 24
// frames a second, then 60) has packets half a head frame apart, which the
// timing alone takes for fields; the stated lengths may say the same. The
// sampled decode shows a picture per packet, so the source is counted
// exactly: a conversion that keeps every picture passes, and one that loses a
// second of the 60 frame section is refused. Matroska, MP4 and MPEG-TS seek
// differently; each is read.
func TestRealFieldSampleTellsHigherFrameRate(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	encoders, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil || !strings.Contains(string(encoders), " libx264 ") {
		t.Skip("libx264 unavailable")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.CommandContext(ctx, ffmpeg, append([]string{"-nostdin", "-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	scan := func(path string) sourceVideoScan {
		t.Helper()
		s, err := scanSourceVideo(ctx, ffprobe, path, 0)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, tc := range []struct {
		name, ext, head string
		pictures        int
	}{
		// 12 s of the head's rate, then 10 s at 60.
		{"30 then 60, Matroska", "mkv", "not(mod(n,2))", 960},
		{"24 then 60, Matroska", "mkv", "not(mod(n,5))+eq(mod(n,5),2)", 888},
		{"30 then 60, MPEG-TS", "ts", "not(mod(n,2))", 960},
		{"30 then 60, MP4", "mp4", "not(mod(n,2))", 960},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := filepath.Join(dir, strings.NewReplacer(" ", "", ",", "").Replace(tc.name)+"."+tc.ext)
			run("-f", "lavfi", "-i", "testsrc2=size=128x72:rate=60:duration=22",
				"-vf", "select='lt(t,12)*("+tc.head+")+gte(t,12)'", "-fps_mode", "vfr",
				"-c:v", "libx264", "-preset", "ultrafast", "-bf", "2", "-g", "60", src)
			good, lost := src+".good.mkv", src+".lost.mkv"
			run(append(append([]string{"-i", src}, sourceTimestampArgs...), "-c:v", "libx264", "-preset", "ultrafast", good)...)
			run(append(append([]string{"-i", src, "-vf", "select='not(between(t,16,16.99))'"}, sourceTimestampArgs...), "-c:v", "libx264", "-preset", "ultrafast", lost)...)

			s := scan(src)
			if err := decodeVideoHead(ctx, ffprobe, src, 0, &s); err != nil {
				t.Fatal(err)
			}
			if s.possibleFields(s.headFrame()) == 0 {
				t.Fatal("no packet lies close to another: the source does not exercise the sample")
			}
			if err := sampleFieldPictures(ctx, ffprobe, src, 0, &s, "h264", 0); err != nil {
				t.Fatal(err)
			}
			if s.fieldSample != fieldsSampledFrames {
				t.Fatalf("sample found %d, want frames", s.fieldSample)
			}
			if p := sourcePictures(s, "h264", 0); p.fields || p.least != tc.pictures || p.most != tc.pictures {
				t.Fatalf("source counted %+v, want exactly %d pictures", p, tc.pictures)
			}
			if err := videoCompleteness(s, scan(good), "h264", 0); err != nil {
				t.Fatalf("a conversion keeping every picture refused: %v", err)
			}
			err := videoCompleteness(s, scan(lost), "h264", 0)
			if err == nil || !strings.Contains(err.Error(), "the source shows ") {
				t.Fatalf("a conversion that lost a second of the 60 frame section: %v, want refused", err)
			}
		})
	}
}
