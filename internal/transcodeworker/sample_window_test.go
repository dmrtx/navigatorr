package transcodeworker

import (
	"context"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// extractSampleWindow cuts one window from in the way the final gate does:
// from the file's first picture, on the frame grid of fps.
func extractSampleWindow(t *testing.T, ctx context.Context, ffmpeg, ffprobe, in, out string, start, duration, fps float64) {
	t.Helper()
	first, err := probeFirstFrameTime(ctx, ffprobe, in, 0)
	if err != nil {
		t.Fatal(err)
	}
	from, to := sampleFrameBounds(start, duration, fps)
	runFFmpeg(t, ctx, ffmpeg, append([]string{"-v", "error"}, BuildReferenceExtractionArgs(in, out, 0, first, from, to)...)...)
}

// frameHashes is the MD5 of every decoded picture of path, in order.
func frameHashes(t *testing.T, ctx context.Context, ffmpeg, path string) []string {
	t.Helper()
	out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", path, "-map", "0:v:0", "-f", "framemd5", "-").Output()
	if err != nil {
		t.Fatalf("framemd5 %s: %v", path, err)
	}
	var hashes []string
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Split(line, ","); len(fields) == 6 && !strings.HasPrefix(line, "#") {
			hashes = append(hashes, strings.TrimSpace(fields[5]))
		}
	}
	return hashes
}

func TestSampleFrameBoundsSitBetweenFrames(t *testing.T) {
	fps := 24000.0 / 1001
	// 200 s is 4795.2 frames in: the window starts on frame 4796 and holds
	// round(10 s × fps) = 240 frames, cut half a frame either side.
	from, to := sampleFrameBounds(200, 10, fps)
	if math.Abs(from*fps-4795.5) > 1e-6 || math.Abs(to*fps-5035.5) > 1e-6 {
		t.Fatalf("bounds %.6f..%.6f are frames %.3f..%.3f, want 4795.5..5035.5", from, to, from*fps, to*fps)
	}
	// A window at the start begins half a frame before the first picture.
	if from, to := sampleFrameBounds(0, 1, 25); from != -0.02 || math.Abs(to-0.98) > 1e-9 {
		t.Fatalf("bounds %v..%v, want -0.02..0.98", from, to)
	}
}

// Regression: an XviD AVI stamps its frames in a 1001/24000 time base, and a
// time seek there is rounded to a whole frame, while its encode's Matroska
// time base is 1 ms. A window starting between two frames, nearer the
// earlier, kept that frame from the source and not from the encode: one
// extract held a picture more than the other (240 versus 239 in production)
// or every picture one frame off. The gate's extracts of a source and of a
// lossless encode of it must hold exactly the same pictures.
func TestSampleWindowsHoldTheSamePicturesAcrossTimeBases(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	encoders, err := exec.Command(ffmpeg, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil || !strings.Contains(string(encoders), " mpeg4 ") {
		t.Skip("mpeg4 encoder unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.avi")
	candidate := filepath.Join(dir, "candidate.mkv")
	fps := 24000.0 / 1001
	// B-frames make the decoder hold each picture back a packet, so the
	// first picture is stamped a frame after the AVI stream starts.
	runFFmpeg(t, ctx, ffmpeg, "-y", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24000/1001:duration=6", "-c:v", "mpeg4", "-bf", "1", "-q:v", "3", "-vtag", "XVID", source)
	runFFmpeg(t, ctx, ffmpeg, append(append([]string{"-y", "-nostdin", "-v", "error", "-i", source, "-c:v", "ffv1"}, sourceTimestampArgs...), candidate)...)

	// 0.5, 1.01 and 2.2 s each lie less than half a frame after a frame.
	for _, start := range []float64{0, 0.5, 1.01, 2.2, 3.37} {
		ref := filepath.Join(dir, "ref.mkv")
		cand := filepath.Join(dir, "cand.mkv")
		extractSampleWindow(t, ctx, ffmpeg, ffprobe, source, ref, start, 1.5, fps)
		extractSampleWindow(t, ctx, ffmpeg, ffprobe, candidate, cand, start, 1.5, fps)
		refPTS, err := probeSamplePTS(ctx, ffprobe, ref)
		if err != nil {
			t.Fatal(err)
		}
		candPTS, err := probeSamplePTS(ctx, ffprobe, cand)
		if err != nil {
			t.Fatal(err)
		}
		if err := verifySampleAlignment(refPTS, candPTS, fps); err != nil {
			t.Fatalf("window at %.2f s: %v", start, err)
		}
		if len(refPTS) != 36 {
			t.Fatalf("window at %.2f s holds %d frames, want round(1.5 s × fps) = 36", start, len(refPTS))
		}
		if refHashes, candHashes := frameHashes(t, ctx, ffmpeg, ref), frameHashes(t, ctx, ffmpeg, cand); !slices.Equal(refHashes, candHashes) {
			t.Fatalf("window at %.2f s: source and encode extracts hold different pictures", start)
		}
	}
}
