package transcodeworker

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An encode that lost one frame before a window keeps regular timestamps, so
// only the pictures can show the shift; an aligned lossy encode must pass.
func TestVerifyContentAlignmentDetectsOneFrameShift(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	aligned := filepath.Join(dir, "aligned.mkv")
	shifted := filepath.Join(dir, "shifted.mkv")
	runFFmpeg(t, ctx, ffmpeg, "-y", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=4", "-c:v", "ffv1", source)
	runFFmpeg(t, ctx, ffmpeg, "-y", "-nostdin", "-v", "error", "-i", source, "-c:v", "mpeg2video", "-q:v", "4", aligned)
	runFFmpeg(t, ctx, ffmpeg, "-y", "-nostdin", "-v", "error", "-i", source, "-vf", "select=gte(n\\,1),setpts=N/(25*TB)", "-c:v", "mpeg2video", "-q:v", "4", shifted)

	extract := func(in, name string) string {
		out := filepath.Join(dir, name)
		runFFmpeg(t, ctx, ffmpeg, append([]string{"-v", "error"}, BuildReferenceExtractionArgs(in, out, 0, 1, 1.5)...)...)
		return out
	}
	ref := extract(source, "ref.mkv")
	alignedCand := extract(aligned, "aligned_cand.mkv")
	shiftedCand := extract(shifted, "shifted_cand.mkv")

	refPTS, err := probeSamplePTS(ctx, ffprobe, ref)
	if err != nil {
		t.Fatal(err)
	}
	shiftedPTS, err := probeSamplePTS(ctx, ffprobe, shiftedCand)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySampleAlignment(refPTS, shiftedPTS, 25); err != nil {
		t.Fatalf("the shift must be invisible to the timestamp check, got %v", err)
	}
	if err := verifyContentAlignment(ctx, ffmpeg, alignedCand, ref); err != nil {
		t.Fatalf("aligned encode rejected: %v", err)
	}
	err = verifyContentAlignment(ctx, ffmpeg, shiftedCand, ref)
	if err == nil || !strings.Contains(err.Error(), "shifted -1 frame") {
		t.Fatalf("shifted encode = %v, want the candidate one frame ahead (-1)", err)
	}
}
