package transcodeworker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// pipelineEndToEnd runs the real pipelined encode/metric stage against the
// mock ffmpeg, with deferred banding, and lets stage decide what happens when
// the workers have drained.
func pipelineEndToEnd(t *testing.T, ctx context.Context, stage func(string)) error {
	t.Helper()
	e := newSampleCacheEnv(t)
	e.seedCaps()
	record := e.record()
	record.SourceSHA256 = ""
	record.Candidates = record.Candidates[2:3] // cand_q80
	record.Quality.Banding = &transcode.BenchmarkBandingConfig{Enabled: true, Mode: "full_reference"}
	record.bandingGate = bandingGateMonotone
	info, err := os.Stat(e.source)
	if err != nil {
		t.Fatal(err)
	}
	samples := filepath.Join(e.dir, "samples")
	if err := os.MkdirAll(samples, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range record.Samples {
		if err := os.WriteFile(filepath.Join(samples, "ref_sample_"+strconv.Itoa(s.Index)+".mkv"), make([]byte, 2048), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := record.Candidates[0]
	plan := &transcode.Plan{VideoCodec: transcode.BenchmarkCandidateVideoCodec(c), Quality: c.Quality, VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8}
	vcs := []validatedCandidate{{index: 0, candidate: c, plan: plan, profile: "main", pixelFormat: "yuv420p", bitDepth: 8, fileKey: c.ID}}
	evidence := &BenchmarkExecutionEvidence{SourceBitDepth: 8, SourcePixelFormat: "yuv420p", SourceResolution: "1920x1080", SourceFPS: 24, SourceFieldOrder: "progressive"}
	w := NewWorker(e.cfg)
	runner := &ProductionBenchmarkRunner{pipelineStageHook: stage}
	progress := newBenchmarkProgressReporter(nil, record, 100)
	return runner.runPipelinedEncodeMetrics(ctx, w, record, evidence, samples, vcs, info.Size(), info.ModTime(), progress, nil)
}

// The banding units are deferred until the VMAF results are in. When the
// search ends between those stages no worker has failed, so only the
// pipeline's own completeness check can keep it from reporting success with
// unmeasured banding.
func TestPipelineEndedBetweenStagesIsNotReportedAsSuccess(t *testing.T) {
	// Control: the same search left alone completes.
	if err := pipelineEndToEnd(t, context.Background(), nil); err != nil {
		t.Fatalf("control search failed: %v", err)
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	err := pipelineEndToEnd(t, ctx, func(stage string) {
		if stage == "measured" {
			cancel(errSearchBudgetStopped)
		}
	})
	if err == nil {
		t.Fatal("a search stopped before its banding was measured reported success")
	}
	if !errors.Is(err, errSearchBudgetStopped) || !errors.Is(err, context.Canceled) {
		t.Fatalf("budget stop lost: %v", err)
	}
	if strings.Contains(err.Error(), "missing") {
		t.Fatalf("budget stop reported as missing evidence: %v", err)
	}
}
