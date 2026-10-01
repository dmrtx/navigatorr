package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestExplicitMain10OnlyPermitsApprovedUpconversion(t *testing.T) {
	for _, tc := range []struct {
		name           string
		source         int
		allow          bool
		profile, pixel string
		want           int
	}{
		{"default_rejects", 8, false, "main10", "p010le", 0},
		{"explicit_conversion", 8, true, "main10", "p010le", 10},
		{"inconsistent_shape", 8, true, "main10", "yuv420p", 0},
		{"downgrade_rejected", 10, true, "main", "yuv420p", 0},
		{"automatic_main_preserved", 8, true, "", "", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &transcode.BenchmarkCandidate{ID: "candidate", VideoProfile: tc.profile, PixelFormat: tc.pixel}
			_, _, depth, err := resolveBenchmarkCandidateBitDepth(c, tc.source, tc.allow)
			if tc.want == 0 && err == nil || tc.want != 0 && (err != nil || depth != tc.want) {
				t.Fatalf("depth=%d err=%v", depth, err)
			}
		})
	}
}

func TestExplicitMain10ProductionPipelineRequiresNativeMetrics(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			dir := t.TempDir()
			ffmpeg, ffprobe, calls := setupMockTools(t, dir, sdr8BitProbeJSON, "")
			info, err := os.Stat(ffmpeg)
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%s:%d:%d", ffmpeg, info.Size(), info.ModTime().UnixNano())
			qualityProbeCache.Lock()
			qualityProbeCache.results[key] = struct {
				caps transcode.QualityCapabilities
				at   time.Time
			}{transcode.QualityCapabilities{Native10Bit: native, CAMBIFullRef: true, Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true}}}, time.Now()}
			qualityProbeCache.Unlock()
			t.Cleanup(func() { qualityProbeCache.Lock(); delete(qualityProbeCache.results, key); qualityProbeCache.Unlock() })
			source := filepath.Join(dir, "source.mkv")
			if err := os.WriteFile(source, []byte("unchanged original"), 0600); err != nil {
				t.Fatal(err)
			}
			w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{dir}, MaxParallelJobs: 1, FFmpeg: ffmpeg, FFprobe: ffprobe})
			record := &BenchmarkRecord{ProtocolVersion: transcode.WorkerProtocolVersion, ID: "explicit-main10", Status: "running", Source: source, Metric: "vmaf", Allow8BitTo10Bit: true, Attempt: 1,
				Samples:    []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 5, DurationSeconds: 10}},
				Candidates: []transcode.BenchmarkCandidate{{ID: "main10", Quality: 60, VideoProfile: "main10", PixelFormat: "p010le"}},
				Quality:    &transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Minimum: 88, Target: 92}},
			}
			err = (&ProductionBenchmarkRunner{}).RunBenchmark(context.Background(), w, record)
			if !native {
				if err == nil || !strings.Contains(err.Error(), "verified 10-bit VMAF") {
					t.Fatalf("unverified native metrics accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if record.Evidence.SourceBitDepth != 8 || len(record.Evidence.CandidateSamples) != 1 || record.Evidence.CandidateSamples[0].VideoProfile != "main10" {
				t.Fatalf("source/candidate depth evidence lost: %+v", record.Evidence)
			}
			data, err := os.ReadFile(calls)
			if err != nil || !strings.Contains(string(data), "-profile:v main10 -pix_fmt p010le") || !strings.Contains(string(data), "format=yuv420p10le") {
				t.Fatal("encoder or metric silently changed depth")
			}
			original, err := os.ReadFile(source)
			if err != nil || string(original) != "unchanged original" {
				t.Fatal("benchmark mutated original")
			}
		})
	}
}
