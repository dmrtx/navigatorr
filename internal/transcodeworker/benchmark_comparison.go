package transcodeworker

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

const comparisonMaxImageBytes = 4 * 1024 * 1024
const comparisonRetention = 7 * 24 * time.Hour

// Resolve each parent, rejecting symlinked job/artifact directories. Browser
// requests never contain filenames; the only accepted image names are derived
// from the persisted manifest and the two fixed sides.
func (w *Worker) comparisonDir(id string) (string, error) {
	if err := transcode.ValidateBenchmarkJobID(id); err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(w.cfg.StateDir)
	if err != nil {
		return "", err
	}
	job := filepath.Join(root, id)
	fi, err := os.Lstat(job)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("invalid benchmark directory")
	}
	dir := filepath.Join(job, "comparison")
	if fi, err := os.Lstat(dir); err == nil && (!fi.IsDir() || fi.Mode()&os.ModeSymlink != 0) {
		return "", fmt.Errorf("invalid comparison directory")
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return dir, nil
}

func (w *Worker) captureBenchmarkComparison(ctx context.Context, record *BenchmarkRecord) error {
	record.Comparison = nil
	ev := record.Evidence
	if ev == nil || ev.Decision == nil || ev.Decision.Winner == nil {
		return nil
	}
	dir, err := w.comparisonDir(record.ID)
	if err != nil {
		return err
	}
	if err = os.RemoveAll(dir); err != nil {
		return err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return err
	}
	manifest := &transcode.BenchmarkComparison{ProtocolVersion: transcode.WorkerProtocolVersion, CandidateID: ev.Decision.Winner.CandidateID, CapturedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(comparisonRetention)}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	samples := filepath.Join(filepath.Dir(dir), "samples")
	if fi, err := os.Lstat(samples); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid comparison samples")
	}
	for _, ref := range ev.ReferenceSamples {
		if len(manifest.Frames) == 3 {
			break
		}
		if ref.Index < 0 || ref.Index > 100 {
			continue
		}
		var candidate *BenchmarkCandidateSampleResult
		for i := range ev.CandidateSamples {
			sample := &ev.CandidateSamples[i]
			if sample.CandidateID == manifest.CandidateID && sample.SampleIndex == ref.Index && sample.Error == "" {
				candidate = sample
				break
			}
		}
		if candidate == nil {
			continue
		}
		frame := 0
		if ev.SourceFPS > 0 && !math.IsInf(ev.SourceFPS, 0) && !math.IsNaN(ev.SourceFPS) {
			frame = int(math.Max(0, math.Floor(ref.DurationSeconds*ev.SourceFPS/2)))
		}
		var width, height int
		valid := true
		for _, side := range []struct{ name, file string }{{"original", ref.File}, {"candidate", candidate.File}} {
			if filepath.Base(side.file) != side.file || side.file == "." || side.file == "" {
				valid = false
				break
			}
			input := filepath.Join(samples, side.file)
			fi, err := os.Lstat(input)
			if err != nil || !fi.Mode().IsRegular() {
				valid = false
				break
			}
			output := filepath.Join(dir, side.name+"-"+strconv.Itoa(ref.Index)+".png")
			imageCtx, stop := context.WithTimeout(ctx, 15*time.Second)
			// Identical frame index, from the exact sample used by the metrics. No
			// resizing or JPEG encoding that could conceal compression artifacts.
			args := []string{"-nostdin", "-v", "error", "-i", input, "-map", "0:v:0", "-vf", fmt.Sprintf("select=eq(n\\,%d),format=rgb24", frame), "-frames:v", "1", "-c:v", "png", "-threads", "1", "-fs", strconv.Itoa(comparisonMaxImageBytes), "-n", output}
			err = exec.CommandContext(imageCtx, w.ffmpegPath, args...).Run()
			stop()
			if err != nil {
				valid = false
				break
			}
			data, err := readComparisonPNG(output)
			if err != nil {
				valid = false
				break
			}
			cfg, err := png.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				valid = false
				break
			}
			if width != 0 && (width != cfg.Width || height != cfg.Height) {
				valid = false
				break
			}
			width, height = cfg.Width, cfg.Height
		}
		if !valid {
			for _, side := range []string{"original", "candidate"} {
				_ = os.Remove(filepath.Join(dir, side+"-"+strconv.Itoa(ref.Index)+".png"))
			}
			continue
		}
		sourceTime := ref.StartSeconds
		if ev.SourceFPS > 0 {
			sourceTime += float64(frame) / ev.SourceFPS
		}
		manifest.Frames = append(manifest.Frames, transcode.BenchmarkComparisonFrame{SampleIndex: ref.Index, FrameIndex: frame, SourceSeconds: sourceTime, Width: width, Height: height, VMAF: candidate.VMAF, SSIM: candidate.SSIM})
	}
	if len(manifest.Frames) == 0 {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("comparison frames unavailable")
	}
	record.Comparison = manifest
	w.pruneBenchmarkComparisons(record.ID)
	return nil
}

func readComparisonPNG(path string) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > comparisonMaxImageBytes {
		return nil, fmt.Errorf("invalid comparison image")
	}
	data, err := io.ReadAll(io.LimitReader(f, comparisonMaxImageBytes+1))
	if err != nil || len(data) > comparisonMaxImageBytes {
		return nil, fmt.Errorf("comparison image exceeds limit")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 3840 || cfg.Height > 2160 {
		return nil, fmt.Errorf("invalid comparison image dimensions")
	}
	// Decode the bounded PNG as well, rejecting truncated output from -fs.
	if _, err = png.Decode(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("incomplete comparison image")
	}
	return data, nil
}

func (w *Worker) BenchmarkComparison(id string) (*transcode.BenchmarkComparison, error) {
	dir, err := w.comparisonDir(id)
	if err != nil {
		return nil, err
	}
	record, err := LoadBenchmark(filepath.Join(filepath.Dir(dir), "benchmark.json"))
	if err != nil || record.Status != "completed" || record.Comparison == nil || time.Now().After(record.Comparison.ExpiresAt) {
		return nil, fmt.Errorf("comparison unavailable; images were not saved or have expired")
	}
	for _, frame := range record.Comparison.Frames {
		for _, side := range []string{"original", "candidate"} {
			fi, err := os.Lstat(filepath.Join(dir, side+"-"+strconv.Itoa(frame.SampleIndex)+".png"))
			if err != nil || !fi.Mode().IsRegular() || fi.Size() > comparisonMaxImageBytes {
				return nil, fmt.Errorf("comparison images expired or unavailable")
			}
		}
	}
	return record.Comparison, nil
}
func (w *Worker) BenchmarkComparisonImage(id string, index int, side string) ([]byte, error) {
	if side != "original" && side != "candidate" {
		return nil, fmt.Errorf("invalid comparison side")
	}
	manifest, err := w.BenchmarkComparison(id)
	if err != nil {
		return nil, err
	}
	found := false
	for _, frame := range manifest.Frames {
		if frame.SampleIndex == index {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("comparison sample not found")
	}
	dir, err := w.comparisonDir(id)
	if err != nil {
		return nil, err
	}
	return readComparisonPNG(filepath.Join(dir, side+"-"+strconv.Itoa(index)+".png"))
}
func (w *Worker) pruneBenchmarkComparisons(current string) {
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		return
	}
	type artifact struct {
		id       string
		captured time.Time
	}
	var retained []artifact
	for _, entry := range entries {
		if entry.Name() == current || !entry.IsDir() || transcode.ValidateBenchmarkJobID(entry.Name()) != nil {
			continue
		}
		dir, err := w.comparisonDir(entry.Name())
		if err != nil {
			continue
		}
		record, err := LoadBenchmark(filepath.Join(filepath.Dir(dir), "benchmark.json"))
		if err != nil || record.Status != "completed" || record.Comparison == nil {
			continue
		}
		if time.Now().After(record.Comparison.ExpiresAt) {
			_ = os.RemoveAll(dir)
			continue
		}
		retained = append(retained, artifact{entry.Name(), record.Comparison.CapturedAt})
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i].captured.After(retained[j].captured) })
	for i := 15; i < len(retained); i++ {
		if dir, err := w.comparisonDir(retained[i].id); err == nil {
			_ = os.RemoveAll(dir)
		}
	}
}
