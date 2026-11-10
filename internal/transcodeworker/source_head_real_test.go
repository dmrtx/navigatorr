package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An MP4 cut inside a long group of pictures (`-ss` with stream copy) keeps
// the packets from the keyframe before the cut and marks those before it to
// be discarded by an edit list: here 360 of them, more than the head's
// headAfterKey packets after its keyframe. The head decode reaches past them
// to pictures that show, and counts the source exactly: a conversion keeping
// every picture passes, one that lost a second is refused.
func TestRealEditListHeadPastDiscardedPackets(t *testing.T) {
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
	long, src := filepath.Join(dir, "long.mp4"), filepath.Join(dir, "cut.mp4")
	run("-f", "lavfi", "-i", "testsrc2=size=128x72:rate=30:duration=40", "-c:v", "libx264", "-preset", "ultrafast", "-g", "900", long)
	run("-ss", "12", "-i", long, "-c", "copy", src)
	good, lost := src+".good.mkv", src+".lost.mkv"
	run(append(append([]string{"-i", src}, sourceTimestampArgs...), "-c:v", "libx264", "-preset", "ultrafast", good)...)
	run(append(append([]string{"-i", src, "-vf", "select='not(between(t,20,20.99))'"}, sourceTimestampArgs...), "-c:v", "libx264", "-preset", "ultrafast", lost)...)

	s := scan(src)
	if s.Discarded <= headAfterKey {
		t.Fatalf("scan %d packets, %d discarded: the cut does not exercise the head", s.Packets, s.Discarded)
	}
	if err := decodeVideoHead(ctx, ffprobe, src, 0, &s); err != nil {
		t.Fatalf("head decode of an edit-listed source: %v", err)
	}
	want := s.Packets - s.Discarded
	if p := sourcePictures(s, "h264", 30); p.fields || p.least != want || p.most != want {
		t.Fatalf("source counted %+v, want exactly %d pictures", p, want)
	}
	if err := videoCompleteness(s, scan(good), "h264", 30); err != nil {
		t.Fatalf("a conversion keeping every picture refused: %v", err)
	}
	if err := videoCompleteness(s, scan(lost), "h264", 30); err == nil || !strings.Contains(err.Error(), "the source shows ") {
		t.Fatalf("a conversion that lost a second: %v, want refused", err)
	}
}

// An x264 intra-refresh stream has no IDR picture after its first: its
// keyframes are recovery points, and a decoder starting at one (a transport
// stream cut anywhere) drops the pictures after it until the picture is
// whole again. Those are not the second fields of field-coded pictures: the
// head decode's first shown picture says where the decoder started, the
// source is counted exactly, a conversion keeping every picture it shows
// passes, and one that lost a second is refused.
func TestRealIntraRefreshCutHead(t *testing.T) {
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
	full, src := filepath.Join(dir, "full.ts"), filepath.Join(dir, "cut.ts")
	run("-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=20", "-c:v", "libx264", "-preset", "ultrafast", "-x264-params", "intra-refresh=1:keyint=48", "-pix_fmt", "yuv420p", full)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, data[(len(data)*37/100)/188*188:], 0o600); err != nil { // a cut on a transport packet
		t.Fatal(err)
	}
	good, lost := src+".good.mkv", src+".lost.mkv"
	run(append(append([]string{"-i", src}, sourceTimestampArgs...), "-c:v", "libx264", "-preset", "ultrafast", good)...)
	run(append(append([]string{"-i", src, "-vf", "select='not(between(t,5,5.99))'"}, sourceTimestampArgs...), "-c:v", "libx264", "-preset", "ultrafast", lost)...)
	scan := func(path string) sourceVideoScan {
		t.Helper()
		s, err := scanSourceVideo(ctx, ffprobe, path, 0)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := scan(src)
	if err := decodeVideoHead(ctx, ffprobe, src, 0, &s); err != nil {
		t.Fatal(err)
	}
	nonKey, leading, discarded := s.headStats(s.headPackets)
	if s.headPackets-s.headFrames <= nonKey {
		t.Fatalf("head shows %d of %d packets, %d before the keyframe: the cut does not start at a recovery point", s.headFrames, s.headPackets, nonKey)
	}
	if s.headShowsFields(nonKey, leading, discarded) {
		t.Fatalf("an intra-refresh head read as fields: %d of %d shown, %d before the keyframe, %d after it unshown", s.headFrames, s.headPackets, nonKey, leading)
	}
	if err := videoCompleteness(s, scan(good), "h264", 24); err != nil {
		t.Fatalf("a conversion keeping every picture refused: %v", err)
	}
	if err := videoCompleteness(s, scan(lost), "h264", 24); err == nil || !strings.Contains(err.Error(), "the source shows ") {
		t.Fatalf("a conversion that lost a second: %v, want refused (%+v, lost %d packets, first %.3f last %.3f)", err, sourcePictures(s, "h264", 24), scan(lost).Packets, s.First, s.Last)
	}
}
