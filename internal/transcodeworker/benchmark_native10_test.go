package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestNative10BitLegacyBenchmarkUsesVerifiedCapabilities(t *testing.T) {
	for _, metric := range []string{"vmaf", "both", "vmaf+ssim", ""} {
		t.Run(metric, func(t *testing.T) {
			dir := t.TempDir()
			ffmpeg, ffprobe, calls := setupMockTools(t, dir, sdr10BitProbeJSON, "")
			info, err := os.Stat(ffmpeg)
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%s:%d:%d", ffmpeg, info.Size(), info.ModTime().UnixNano())
			qualityProbeCache.Lock()
			qualityProbeCache.results[key] = struct {
				caps transcode.QualityCapabilities
				at   time.Time
			}{transcode.QualityCapabilities{Native10Bit: true, CAMBIFullRef: true, Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true}}}, time.Now()}
			qualityProbeCache.Unlock()
			t.Cleanup(func() {
				qualityProbeCache.Lock()
				delete(qualityProbeCache.results, key)
				qualityProbeCache.Unlock()
			})
			source := filepath.Join(dir, "source.mkv")
			if err := os.WriteFile(source, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{dir}, MaxParallelJobs: 1, FFmpeg: ffmpeg, FFprobe: ffprobe})
			record := &BenchmarkRecord{ProtocolVersion: transcode.WorkerProtocolVersion, ID: "legacy", Status: "running", Source: source, Metric: metric, Attempt: 1,
				Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 5, DurationSeconds: 10}}, Candidates: []transcode.BenchmarkCandidate{{ID: "main10", Quality: 60}},
			}
			if err := (&ProductionBenchmarkRunner{}).RunBenchmark(context.Background(), w, record); err != nil {
				t.Fatal(err)
			}
			if len(record.Evidence.MetricSamples) != 1 || record.Evidence.MetricSamples[0].VMAF == nil {
				t.Fatal("legacy benchmark did not measure VMAF")
			}
			data, err := os.ReadFile(calls)
			if err != nil || !strings.Contains(string(data), "-noauto_conversion_filters") {
				t.Fatal("native VMAF allowed automatic pixel conversion")
			}
		})
	}
}

// Exercise the production pipeline, not just the capability endpoint: the
// endpoint already passed while legacy Main10 benchmarks skipped the probe.
func TestNative10BitRealBenchmark(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	caps, err := ProbeWorkerCapabilitiesWithScratch(ctx, ffmpeg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.SupportsNative10BitQuality() || !caps.Encoders["libx265"] {
		t.Skip("native VMAF/CAMBI or libx265 unavailable")
	}
	source := filepath.Join(dir, "source.mkv")
	args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "nullsrc=size=640x360:rate=24:duration=2,format=yuv420p10le,geq=lum='64+mod(X+3*Y+N,877)':cb=512:cr=512", "-c:v", "ffv1", source}
	if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	before, err := fileSHA256(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range []string{"vmaf", "both", "vmaf+ssim", "", "v1_cambi"} {
		t.Run(metric, func(t *testing.T) {
			w := NewWorker(&WorkerConfig{StateDir: t.TempDir(), AllowedRoots: []string{dir}, MaxParallelJobs: 1, FFmpeg: ffmpeg, FFprobe: ffprobe})
			record := &BenchmarkRecord{
				ProtocolVersion: transcode.WorkerProtocolVersion, ID: "native10", Status: "running", Source: source, Metric: metric, Attempt: 1,
				Samples:    []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 0.5}},
				Candidates: []transcode.BenchmarkCandidate{{ID: "main10", VideoCodec: "libx265", Quality: 18, Preset: "ultrafast", VideoProfile: "main10", PixelFormat: "p010le"}},
				Quality:    &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Minimum: 95, Target: 96}},
			}
			if metric == "" {
				record.Quality = nil // No policy at all still needs native-depth proof.
			}
			if metric == "v1_cambi" {
				record.Metric = "vmaf"
				record.Quality.VMAF.Model = "v1_1080p_3h"
				record.Quality.VMAF.WorstWindowSeconds = 0.2
				record.Quality.Banding = &transcode.BenchmarkBandingConfig{Enabled: true, Mode: "full_reference"}
			}
			if err := (&ProductionBenchmarkRunner{}).RunBenchmark(ctx, w, record); err != nil {
				t.Fatal(err)
			}
			e := record.Evidence
			if e == nil || e.SourceBitDepth != 10 || len(e.MetricSamples) != 1 || e.MetricSamples[0].VMAF == nil || *e.MetricSamples[0].VMAF <= 0 || e.MetricSamples[0].Error != "" {
				t.Fatalf("missing native VMAF evidence: %+v", e)
			}
			if metric == "both" || metric == "vmaf+ssim" {
				if e.MetricSamples[0].SSIM == nil {
					t.Fatal("combined metric omitted SSIM")
				}
			}
			if metric == "v1_cambi" && (e.MetricSamples[0].VMAFStats == nil || e.MetricSamples[0].CAMBI == nil || e.MetricSamples[0].CAMBIError != "") {
				t.Fatalf("missing VMAF v1/CAMBI evidence: %+v", e.MetricSamples[0])
			}
		})
	}
	if after, err := fileSHA256(source); err != nil || after != before {
		t.Fatal("source changed during benchmark")
	}
}
