package transcodeworker

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// rotationFacts is a video stream's coded size and display rotation.
func rotationFacts(t *testing.T, ffprobe, path string) (w, h, rotation int) {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height:stream_side_data=rotation", "-of", "csv=p=0:s=,", path).Output()
	if err != nil {
		t.Fatalf("probing %s: %v", path, err)
	}
	fields := strings.FieldsFunc(strings.TrimSpace(string(out)), func(r rune) bool { return r == ',' || r == '\n' })
	if len(fields) < 2 {
		t.Fatalf("probe of %s: %q", path, out)
	}
	w, _ = strconv.Atoi(fields[0])
	h, _ = strconv.Atoi(fields[1])
	if len(fields) > 2 {
		rotation, _ = strconv.Atoi(fields[2])
	}
	return w, h, rotation
}

// A phone video stores 640x360 pictures and a display rotation. ffmpeg turns
// the pixels by that rotation while decoding and drops the rotation unless
// told not to, which made a good encode 360x640 and unrotated: the resolution
// check refused it after the whole encode. The re-encode keeps the stored
// pictures and the rotation as metadata, and the final quality windows (which
// decode both files the same way) see the same frame on both sides.
func TestReencodeKeepsDisplayRotationAsMetadata(t *testing.T) {
	ffmpeg, ffprobe := scanTestTools(t)
	for _, rotation := range []int{0, 90, 180, 270} {
		t.Run(fmt.Sprintf("rotation%d", rotation), func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "base.mp4")
			scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=3", "-c:v", "libx264", "-pix_fmt", "yuv420p", base)
			source := base
			if rotation != 0 {
				source = filepath.Join(dir, "source.mp4")
				scanTestMedia(t, ffmpeg, "-display_rotation", strconv.Itoa(rotation), "-i", base, "-c", "copy", source)
			}
			srcW, srcH, srcRot := rotationFacts(t, ffprobe, source)
			if srcW != 640 || srcH != 360 || (rotation != 0 && srcRot == 0) {
				t.Fatalf("fixture: %dx%d rotation %d", srcW, srcH, srcRot)
			}
			probe, err := ProbeSourceDetails(context.Background(), ffprobe, source)
			if err != nil {
				t.Fatal(err)
			}
			plan := &transcode.Plan{Container: "mkv", VideoCodec: "libx265", Quality: 23, Preset: "ultrafast", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, AudioMode: "compact", SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "test", RecipeDigest: "sha256:" + strings.Repeat("0", 64), Resilience: transcode.ResiliencePlan{MaxAttempts: 1}}
			plan.PlanDigest, _ = transcode.DigestPlan(plan)
			ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
			if err != nil {
				t.Fatal(err)
			}
			candidate := filepath.Join(dir, "candidate.mkv")
			if err := RunFFmpegPaths(context.Background(), ffmpeg, ep, source, candidate, filepath.Join(dir, "progress"), filepath.Join(dir, "ffmpeg.log")); err != nil {
				t.Fatalf("encode: %v", err)
			}
			candW, candH, candRot := rotationFacts(t, ffprobe, candidate)
			t.Logf("source %dx%d rot %d -> candidate %dx%d rot %d", srcW, srcH, srcRot, candW, candH, candRot)
			if candW != srcW || candH != srcH || candRot != srcRot {
				t.Errorf("candidate is %dx%d rotation %d, want the source's %dx%d rotation %d", candW, candH, candRot, srcW, srcH, srcRot)
			}

			w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
			if _, err := w.validateEncodedCandidateFull(context.Background(), candidate, ep, prescanSource(t, w, plan, probe, source), 0); err != nil {
				t.Errorf("a good encode was refused: %v", err)
			}

			// The quality windows decode source and candidate alike.
			var frames [2][2]int
			for i, path := range []string{source, candidate} {
				ref := filepath.Join(dir, fmt.Sprintf("window%d.mkv", i))
				scanTestMedia(t, ffmpeg, BuildReferenceExtractionArgs(path, ref, 0, 0, 0, 1)...)
				frames[i][0], frames[i][1], _ = rotationFacts(t, ffprobe, ref)
			}
			t.Logf("window frames: source %v, candidate %v", frames[0], frames[1])
			if frames[0] != frames[1] {
				t.Errorf("window frames differ: source %v, candidate %v", frames[0], frames[1])
			}
		})
	}
}
