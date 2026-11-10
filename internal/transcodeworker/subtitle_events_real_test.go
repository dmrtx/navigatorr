package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// mov_text writes an empty two-byte sample into every gap between lines, and
// converting the track to SubRip drops them; a sample of spaces alone may be
// no line either. The packets cannot tell which samples a conversion writes,
// so the conversion runs before the encode and its lines are counted: the
// count is exactly what the encode's converted track has, on files ffmpeg
// made.
func TestRealSubtitlePacketScan(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	srt := filepath.Join(dir, "lines.srt")
	body := "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n2\n00:00:03,000 --> 00:00:03,500\n   \n\n3\n00:00:04,000 --> 00:00:05,000\nWorld\n\n4\n00:00:07,000 --> 00:00:08,500\nAgain\n"
	if err := os.WriteFile(srt, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ffmpeg, append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	mp4, mkv := filepath.Join(dir, "source.mp4"), filepath.Join(dir, "converted.mkv")
	run("-f", "lavfi", "-i", "testsrc=d=10:s=64x48:r=10", "-i", srt, "-map", "0", "-map", "1", "-c:v", "mpeg4", "-c:s", "mov_text", mp4)
	run("-i", mp4, "-map", "0", "-c:v", "copy", "-c:s", "subrip", mkv)
	scan := func(path string) map[int]subtitlePackets {
		t.Helper()
		// The merged scan, as the job runs it: the video and the subtitle
		// in one pass.
		got, err := scanFilePackets(context.Background(), ffprobe, path, packetScanRequest{video: 0, subtitles: map[int]bool{1: true}})
		if err != nil {
			t.Fatal(err)
		}
		if got.video.Packets != 100 {
			t.Fatalf("%s: merged scan counted %d video packets, want 100", filepath.Base(path), got.video.Packets)
		}
		return got.subtitles
	}
	plan := &transcode.Plan{SubtitleActions: []transcode.SubtitleAction{{TypeIndex: 0, SourceStreamIndex: 1, SourceCodec: "mov_text", Operation: "transcode", Codec: "subrip"}}}
	srcStreams := []SourceStream{{Kind: "video", Index: 0, Codec: "mpeg4"}, {Kind: "subtitle", Index: 1, TypeIndex: 0, Codec: "mov_text"}}
	candStreams := []SourceStream{{Kind: "video", Index: 0, Codec: "mpeg4"}, {Kind: "subtitle", Index: 1, TypeIndex: 0, Codec: "subrip"}}
	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
	probe := SourceProbe{Streams: srcStreams, subtitlePackets: scan(mp4)}
	if err := w.countSubtitleEvents(context.Background(), plan, probe, mp4); err != nil {
		t.Fatal(err)
	}
	src, cand := probe.subtitlePackets, scan(mkv)
	if !src[1].counted || src[1].packets <= src[1].events || src[1].events != cand[1].packets {
		t.Fatalf("mov_text source %+v, converted track %+v: want the conversion's lines counted, fewer than the samples", src[1], cand[1])
	}
	if cand[1].packets < 3 {
		t.Fatalf("converted track %+v: want the lines with text", cand[1])
	}
	if err := subtitleEvents(plan, srcStreams, candStreams, src, cand); err != nil {
		t.Fatalf("a faithful conversion was rejected: %v", err)
	}
	lost := map[int]subtitlePackets{1: {packets: cand[1].packets - 1}}
	if err := subtitleEvents(plan, srcStreams, candStreams, src, lost); err == nil || !strings.Contains(err.Error(), "converted subtitle stream 0") {
		t.Fatalf("a conversion that lost a line passed: %v", err)
	}
}
