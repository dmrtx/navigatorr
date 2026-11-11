package transcodeworker

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// tinyWebP is a 64x96 lossless WebP (no encoder is needed to build fixtures).
const tinyWebP = "UklGRp4AAABXRUJQVlA4TJEAAAAvP8AXABcgEEhMwhpyYoFscpL8F6VAIIly5rHmP/SlgbPaetsm0AJhfk5bCOEjCB0STen7ute/iP5PQHyAHb38+xZcMQCmAYArQzJlSVBQOuurIxsqY0uF24F61bPjrQZmj7IwPAtEj/z5NCGe5MDsUTZ2vFUwJ43EzQ6Vsa06sqhcaEWIh2RKJJcOgIXeEK8ZAA=="

type coverFormatCase struct {
	name      string
	container string // source container: mkv or mp4
	image     string // bmp or webp
	asAttach  bool   // Matroska attachment (true) or attached_pic video stream
}

// TestCoverArtBMPAndWebPRealEncode runs the production encode and the
// pre-publish candidate validation on sources whose cover is a BMP or WebP,
// held as a Matroska image attachment or as an attached_pic video stream.
func TestCoverArtBMPAndWebPRealEncode(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	available, err := ProbeAvailableEncoders(ctx, ffmpeg)
	if err != nil || !available["libx265"] {
		t.Skip("libx265 unavailable")
	}
	dir := t.TempDir()
	run := func(t *testing.T, args ...string) {
		t.Helper()
		if out, err := exec.CommandContext(ctx, ffmpeg, append([]string{"-nostdin", "-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("fixture not muxable by ffmpeg: %v %s", err, out)
		}
	}
	images := map[string]string{"bmp": filepath.Join(dir, "cover.bmp"), "webp": filepath.Join(dir, "cover.webp")}
	run(t, "-f", "lavfi", "-i", "testsrc2=size=160x240:rate=1", "-frames:v", "1", images["bmp"])
	webp, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(images["webp"], webp, 0o600); err != nil {
		t.Fatal(err)
	}
	mimes := map[string]string{"bmp": "image/bmp", "webp": "image/webp"}

	cases := []coverFormatCase{
		{"mkv_attachment_bmp", "mkv", "bmp", true},
		{"mkv_attachment_webp", "mkv", "webp", true},
		{"mkv_attached_pic_bmp", "mkv", "bmp", false},
		{"mkv_attached_pic_webp", "mkv", "webp", false},
		{"mp4_attached_pic_bmp", "mp4", "bmp", false},
		{"mp4_attached_pic_webp", "mp4", "webp", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := filepath.Join(dir, c.name+"."+c.container)
			img := images[c.image]
			if c.asAttach {
				run(t, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=3", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=3", "-map", "0:v", "-map", "1:a", "-c:v", "ffv1", "-c:a", "flac",
					"-attach", img, "-metadata:s:t:0", "mimetype="+mimes[c.image], "-metadata:s:t:0", "filename=cover."+c.image, source)
			} else {
				vcodec, acodec := "ffv1", "flac"
				if c.container == "mp4" {
					vcodec, acodec = "mpeg4", "aac"
				}
				run(t, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=3", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=3", "-i", img,
					"-map", "0:v", "-map", "1:a", "-map", "2:v", "-c:v:0", vcodec, "-c:a", acodec, "-c:v:1", "copy", "-disposition:v:1", "attached_pic", source)
			}
			srcProbe, err := ProbeSourceDetails(ctx, ffprobe, source)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("source streams: %+v", srcProbe.Streams)
			if countCoverArt(srcProbe.Streams)+countStreams(srcProbe.Streams, "attachment") != 1 {
				t.Fatalf("fixture has no picture: %+v", srcProbe.Streams)
			}
			plan := coverArtPlan()
			ep, err := BuildExecutionPlan(plan, srcProbe.Streams, srcProbe.DurationSec)
			if err != nil {
				t.Fatalf("RESULT plan refused: %v", err)
			}
			jobDir := filepath.Join(dir, "job_"+c.name)
			if err := os.MkdirAll(jobDir, 0o700); err != nil {
				t.Fatal(err)
			}
			candidate := filepath.Join(jobDir, "candidate.mkv")
			if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(jobDir, "progress"), filepath.Join(jobDir, "ffmpeg.log")); err != nil {
				log, _ := os.ReadFile(filepath.Join(jobDir, "ffmpeg.log"))
				t.Fatalf("RESULT encode failed: %v\n%s", err, log)
			}
			candProbe, err := ProbeSourceDetails(ctx, ffprobe, candidate)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("candidate streams: %+v", candProbe.Streams)
			w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
			if _, err := w.validateEncodedCandidateFull(ctx, candidate, ep, prescanSource(t, w, ep.Plan, srcProbe, source), 0); err != nil {
				t.Fatalf("RESULT validation refused: %v", err)
			}
			t.Logf("RESULT pass")

			// A candidate that dropped the picture must still be refused.
			lost := filepath.Join(jobDir, "lost-cover.mkv")
			run(t, "-i", candidate, "-map", "0:V", "-map", "0:a", "-c", "copy", lost)
			if _, err := w.validateEncodedCandidateFull(ctx, lost, ep, prescanSource(t, w, ep.Plan, srcProbe, source), 0); err == nil {
				t.Fatalf("accepted a candidate without its picture")
			} else {
				t.Logf("RESULT dropped picture refused: %v", err)
			}
		})
	}
}
