package transcodeworker

import (
	"bytes"
	"context"
	"github.com/jakenesler/navigatorr/transcode"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestComparisonCapturesSameNativeFrameBeforeScratchCleanup(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable for real frame extraction")
	}
	worker, _, _, _ := newHTTPTestSetup(t, "secret")
	worker.ffmpegPath = ffmpeg
	id := "bench-visual-proof"
	dir := filepath.Join(worker.cfg.StateDir, id, "samples")
	os.MkdirAll(dir, 0700)
	ref, candidate := filepath.Join(dir, "ref.mkv"), filepath.Join(dir, "candidate.mkv")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := []string{"-v", "error", "-f", "lavfi", "-i", "color=red:size=128x72:rate=10:duration=0.5", "-f", "lavfi", "-i", "color=blue:size=128x72:rate=10:duration=0.5", "-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]", "-map", "[v]", "-c:v", "ffv1", ref}
	if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if out, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", ref, "-c:v", "libx264", "-crf", "30", candidate).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	record := &BenchmarkRecord{ID: id, Status: "completed", Evidence: &BenchmarkExecutionEvidence{SourceFPS: 10, ReferenceSamples: []BenchmarkSampleRef{{Index: 0, StartSeconds: 100, DurationSeconds: 1, File: "ref.mkv"}}, CandidateSamples: []BenchmarkCandidateSampleResult{{CandidateID: "winner", SampleIndex: 0, File: "candidate.mkv"}}, Decision: &transcode.BenchmarkDecision{Winner: &transcode.BenchmarkWinner{CandidateID: "winner"}}}}
	if err := worker.captureBenchmarkComparison(ctx, record); err != nil {
		t.Fatal(err)
	}
	if len(record.Comparison.Frames) != 1 || record.Comparison.Frames[0].FrameIndex != 5 || record.Comparison.Frames[0].SourceSeconds != 100.5 {
		t.Fatal(record.Comparison)
	}
	if err := SaveBenchmarkAtomic(filepath.Join(filepath.Dir(dir), "benchmark.json"), record); err != nil {
		t.Fatal(err)
	}
	if err := worker.CleanBenchmarkSamples(id); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"original", "candidate"} {
		data, err := worker.BenchmarkComparisonImage(id, 0, side)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != 128 || img.Bounds().Dy() != 72 {
			t.Fatal("frames were resized")
		}
		r, _, b, _ := img.At(64, 36).RGBA()
		if b < 50000 || r > 5000 {
			t.Fatal("wrong frame: expected blue second half", side, r, b)
		}
	}
}

func TestComparisonHTTPAuthBoundsExpiryAndSymlinks(t *testing.T) {
	worker, srv, _, _ := newHTTPTestSetup(t, "secret")
	id := "bench-http-preview"
	dir := filepath.Join(worker.cfg.StateDir, id, "comparison")
	os.MkdirAll(dir, 0700)
	manifest := &transcode.BenchmarkComparison{ProtocolVersion: transcode.WorkerProtocolVersion, CandidateID: "winner", ExpiresAt: time.Now().Add(time.Hour), Frames: []transcode.BenchmarkComparisonFrame{{SampleIndex: 0, Width: 2, Height: 2}}}
	record := &BenchmarkRecord{ID: id, Status: "completed", Comparison: manifest}
	SaveBenchmarkAtomic(filepath.Join(filepath.Dir(dir), "benchmark.json"), record)
	var data bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{B: 255, A: 255})
	png.Encode(&data, img)
	os.WriteFile(filepath.Join(dir, "original-0.png"), data.Bytes(), 0600)
	os.WriteFile(filepath.Join(dir, "candidate-0.png"), data.Bytes(), 0600)
	base := "/v1/benchmarks/" + id + "/comparison"
	if got := doRequest(t, srv, "GET", base, "", ""); got.Code != 401 {
		t.Fatal("unauthenticated preview", got.Code)
	}
	if got := doRequest(t, srv, "GET", base+"?image=0&side=original", "", "secret"); got.Code != 200 || got.Header().Get("Content-Type") != "image/png" || !bytes.Equal(got.Body.Bytes(), data.Bytes()) {
		t.Fatal(got.Code, got.Body.String())
	}
	for _, query := range []string{"?image=-1&side=original", "?image=0&side=../../other", "?image=99&side=original"} {
		if got := doRequest(t, srv, "GET", base+query, "", "secret"); got.Code == 200 {
			t.Fatal("accepted invalid image", query)
		}
	}
	os.Remove(filepath.Join(dir, "original-0.png"))
	os.Symlink(filepath.Join(t.TempDir(), "outside.png"), filepath.Join(dir, "original-0.png"))
	if _, err := worker.BenchmarkComparisonImage(id, 0, "original"); err == nil {
		t.Fatal("followed symlink image")
	}
	manifest.ExpiresAt = time.Now().Add(-time.Second)
	SaveBenchmarkAtomic(filepath.Join(filepath.Dir(dir), "benchmark.json"), record)
	if got := doRequest(t, srv, "GET", base, "", "secret"); got.Code != 404 {
		t.Fatal("expired preview served", got.Code)
	}
}
