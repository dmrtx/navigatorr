package transcodeworker

import (
	"context"
	"encoding/binary"
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

// pgsDisplaySet is one PGS display set as a .sup file stores it: a
// presentation, a window, then, when it shows a picture, a palette and a
// 64x16 object, and the END segment, every segment with its own header.
func pgsDisplaySet(pts uint32, number uint16, show bool) []byte {
	var out []byte
	segment := func(kind byte, payload []byte) {
		out = append(out, 'P', 'G')
		out = binary.BigEndian.AppendUint32(out, pts)
		out = binary.BigEndian.AppendUint32(out, 0)
		out = append(out, kind)
		out = binary.BigEndian.AppendUint16(out, uint16(len(payload)))
		out = append(out, payload...)
	}
	pcs := binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, 1920), 1080)
	pcs = binary.BigEndian.AppendUint16(append(pcs, 0x10), number)
	if show {
		pcs = append(pcs, 0x80, 0, 0, 1, 0, 0, 0, 0, 100, 0, 100) // epoch start, one object at 100,100
	} else {
		pcs = append(pcs, 0, 0, 0, 0) // no object: clears the screen
	}
	segment(0x16, pcs)
	segment(0x17, []byte{1, 0, 0, 100, 0, 100, 0, 64, 0, 16})
	if show {
		segment(0x14, []byte{0, 0, 0, 16, 128, 128, 0, 1, 235, 128, 128, 255})
		data := []byte{0, 64, 0, 16}
		for y := 0; y < 16; y++ {
			data = append(data, 0, 0xC0, 64, 1, 0, 0) // 64 pixels of colour 1, end of line
		}
		ods := append([]byte{0, 0, 0, 0xC0, 0, byte(len(data) >> 8), byte(len(data))}, data...)
		segment(0x15, ods)
	}
	segment(0x80, nil)
	return out
}

// A Blu-ray transport stream (and a .sup file) carries each segment of a
// PGS display set as a packet of its own, and ffmpeg's Matroska muxer merges
// each display set into one packet: a faithful copy of the track into the
// candidate has a packet per display set, not per segment. A copy from
// Matroska keeps its packets. A copy that lost a display set is refused
// either way.
func TestRealCopiedPGSDisplaySets(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	var sup []byte
	for i := 0; i < 10; i++ { // ten lines, each shown then cleared: 20 sets, 80 segments
		sup = append(sup, pgsDisplaySet(uint32(90000*(1+2*i)), uint16(2*i), true)...)
		sup = append(sup, pgsDisplaySet(uint32(90000*(2+2*i)), uint16(2*i+1), false)...)
	}
	supPath := filepath.Join(dir, "lines.sup")
	if err := os.WriteFile(supPath, sup, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ffmpeg, append([]string{"-nostdin", "-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	m2ts, mkv := filepath.Join(dir, "source.m2ts"), filepath.Join(dir, "source.mkv")
	run("-f", "lavfi", "-i", "testsrc=d=22:s=64x48:r=10", "-i", supPath, "-map", "0", "-map", "1", "-c:v", "mpeg2video", "-c:s", "copy", "-f", "mpegts", "-mpegts_m2ts_mode", "1", m2ts)
	run("-f", "lavfi", "-i", "testsrc=d=22:s=64x48:r=10", "-i", supPath, "-map", "0", "-map", "1", "-c:v", "mpeg2video", "-c:s", "copy", mkv)
	scan := func(path string) map[int]subtitlePackets {
		t.Helper()
		got, err := scanFilePackets(context.Background(), ffprobe, path, packetScanRequest{video: 0, subtitles: map[int]bool{1: true}})
		if err != nil {
			t.Fatal(err)
		}
		return got.subtitles
	}
	plan := &transcode.Plan{SubtitleActions: []transcode.SubtitleAction{{TypeIndex: 0, SourceStreamIndex: 1, SourceCodec: "hdmv_pgs_subtitle", Operation: "copy", Codec: "copy"}}}
	streams := []SourceStream{{Kind: "video", Index: 0, Codec: "mpeg2video"}, {Kind: "subtitle", Index: 1, TypeIndex: 0, Codec: "hdmv_pgs_subtitle"}}
	for _, source := range []string{m2ts, mkv} {
		candidate := filepath.Join(dir, "candidate-"+filepath.Base(source)+".mkv")
		// The worker's argument shape for a copied track.
		run("-i", source, "-map", "0:v", "-map", "0:s", "-c", "copy", candidate)
		src, cand := scan(source), scan(candidate)
		if cand[1].packets != 20 {
			t.Fatalf("%s: the copy has %d packets, want a display set each (20)", filepath.Base(source), cand[1].packets)
		}
		if err := subtitleEvents(plan, streams, streams, src, cand); err != nil {
			t.Fatalf("%s (%+v): a faithful copy was refused: %v", filepath.Base(source), src[1], err)
		}
		lost := map[int]subtitlePackets{1: {packets: cand[1].packets - 1}}
		if err := subtitleEvents(plan, streams, streams, src, lost); err == nil || !strings.Contains(err.Error(), "copied subtitle stream 0") {
			t.Fatalf("%s: a copy that lost a display set passed: %v", filepath.Base(source), err)
		}
	}
}
