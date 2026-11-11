package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// A program stream joined from two clips whose clocks start at 100 s and 60 s
// restarts its clock in the middle; a single clip does not.
func clockRestartScans(t *testing.T) (restarted, continuous sourceVideoScan) {
	t.Helper()
	ffmpeg, ffprobe := scanTestTools(t)
	dir := t.TempDir()
	var joined []byte
	for _, part := range []struct{ name, src, offset string }{{"a.mpg", "testsrc", "100"}, {"b.mpg", "testsrc2", "60"}} {
		path := filepath.Join(dir, part.name)
		scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", part.src+"=d=12:s=320x240:r=25", "-c:v", "mpeg2video", "-g", "12", "-bf", "2",
			"-output_ts_offset", part.offset, "-f", "vob", "-y", path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		joined = append(joined, data...)
	}
	joinedPath := filepath.Join(dir, "joined.mpg")
	if err := os.WriteFile(joinedPath, joined, 0o600); err != nil {
		t.Fatal(err)
	}
	var scans [2]sourceVideoScan
	for i, path := range []string{joinedPath, filepath.Join(dir, "a.mpg")} {
		scan, err := scanSourceVideo(context.Background(), ffprobe, path, 0)
		if err != nil {
			t.Fatal(err)
		}
		scans[i] = scan
	}
	return scans[0], scans[1]
}

func TestSourceClockRestartedIsRefusedBeforeAVideoReencode(t *testing.T) {
	restarted, continuous := clockRestartScans(t)
	if !restarted.Discontinuous || continuous.Discontinuous {
		t.Fatalf("fixture: restarted %v, continuous %v", restarted.Discontinuous, continuous.Discontinuous)
	}
	reencode := &transcode.Plan{Mode: "size", VideoCodec: "libx265"}
	err := sourceClockRestarted(reencode, restarted)
	if err == nil || !strings.HasPrefix(err.Error(), "source_clock_restarted:") || !strings.Contains(err.Error(), "restart about 12 s") {
		t.Fatalf("err = %v, want source_clock_restarted naming the restart near 12 s", err)
	}
	if class := resilience.ClassifyError(err); class != resilience.FFmpegInputCorrupt {
		t.Fatalf("class = %s, want %s (a retry fails the same way)", class, resilience.FFmpegInputCorrupt)
	}
	if err := sourceClockRestarted(reencode, continuous); err != nil {
		t.Fatalf("a continuous source was refused: %v", err)
	}
	copyPlan := &transcode.Plan{Mode: "size", VideoCodec: transcode.VideoCodecCopy}
	if err := sourceClockRestarted(copyPlan, restarted); err != nil {
		t.Fatalf("a video-copy plan was refused: %v", err)
	}
	if err := sourceClockRestarted(nil, restarted); err != nil {
		t.Fatalf("a nil plan was refused: %v", err)
	}
}

// A transport stream cut in the middle of a picture group starts with
// pictures stamped far ahead of the programme (the tail of the previous
// recording). No decoder shows them: the programme begins at the first
// keyframe, so the clock does not count as restarted.
func TestSourceClockIgnoresStalePicturesBeforeTheFirstKeyframe(t *testing.T) {
	ffmpeg, ffprobe := scanTestTools(t)
	dir := t.TempDir()
	tail := filepath.Join(dir, "tail.ts")
	scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", "testsrc=d=2:s=320x240:r=25", "-c:v", "mpeg2video", "-g", "12", "-bf", "0",
		"-output_ts_offset", "20", "-f", "mpegts", "-y", tail)
	main := filepath.Join(dir, "main.ts")
	scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=d=12:s=320x240:r=25", "-c:v", "mpeg2video", "-g", "12", "-bf", "0",
		"-f", "mpegts", "-y", main)
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v", "-show_entries", "packet=pos,flags", "-of", "csv", tail).Output()
	if err != nil {
		t.Fatal(err)
	}
	var positions []int
	keyAfterFirst := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, ",")
		pos, perr := strconv.Atoi(fields[1])
		if perr != nil {
			t.Fatalf("packet line %q: %v", line, perr)
		}
		positions = append(positions, pos)
		if len(positions) > 1 && keyAfterFirst == 0 && strings.Contains(fields[2], "K") {
			keyAfterFirst = pos
		}
	}
	tailData, err := os.ReadFile(tail)
	if err != nil {
		t.Fatal(err)
	}
	mainData, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	// The tables, the tail's pictures between its first and second
	// keyframes (none of them a keyframe), then the programme.
	cut := append([]byte{}, tailData[:positions[0]]...)
	cut = append(cut, tailData[positions[1]:keyAfterFirst]...)
	cut = append(cut, mainData...)
	joined := filepath.Join(dir, "cut.ts")
	if err := os.WriteFile(joined, cut, 0o600); err != nil {
		t.Fatal(err)
	}
	scan, err := scanSourceVideo(context.Background(), ffprobe, joined, 0)
	if err != nil {
		t.Fatal(err)
	}
	if scan.LeadingNonKey == 0 {
		t.Fatalf("fixture: no pictures before the first keyframe: %+v", scan)
	}
	if scan.Discontinuous {
		t.Fatalf("stale pictures before the first keyframe marked the clock restarted: %+v", scan)
	}
	reencode := &transcode.Plan{Mode: "size", VideoCodec: "libx265"}
	if err := sourceClockRestarted(reencode, scan); err != nil {
		t.Fatalf("a convertible source was refused: %v", err)
	}
}
