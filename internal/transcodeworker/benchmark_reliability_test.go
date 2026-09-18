package transcodeworker

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

func TestBenchmarkSelection_MissingTextSubtitleSize(t *testing.T) {
	for _, tc := range []struct {
		name, codec, byteTag      string
		eligible, invalidMetadata bool
	}{
		{name: "mov_text_stream_2", codec: "mov_text", eligible: true},
		{name: "unknown_size_tag", codec: "mov_text", byteTag: "N/A", eligible: true},
		{name: "bitmap_requires_size", codec: "hdmv_pgs_subtitle"},
		{name: "negative_metadata", codec: "mov_text", byteTag: "-1", invalidMetadata: true},
		{name: "malformed_metadata", codec: "mov_text", byteTag: "bad", invalidMetadata: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := transcode.BenchmarkCandidate{ID: "q65", Quality: 65}
			record := &BenchmarkRecord{Metric: "vmaf", SourceDuration: 3600,
				Candidates: []transcode.BenchmarkCandidate{candidate},
				Samples:    []transcode.BenchmarkSampleWindow{{Index: 0, DurationSeconds: 60}},
			}
			evidence := &BenchmarkExecutionEvidence{SourceBitDepth: 8,
				CandidateSamples: []BenchmarkCandidateSampleResult{{CandidateID: "q65", SampleIndex: 0, SizeBytes: 7304172}},
				CandidateMetrics: []BenchmarkCandidateMetricAggregate{{CandidateID: "q65", MetricType: optimization.MetricTypeVMAF,
					Aggregate: optimization.MetricAggregate{MetricType: optimization.MetricTypeVMAF, Valid: true, MeanScore: 96.5,
						SampleScores: []optimization.SampleScore{{SampleIndex: 0, Score: 96.5, Valid: true}},
					}}},
			}
			rep := mediainspect.DetailedReport{SizeBytes: 1786755584, DurationSec: 3600,
				Audio:     []mediainspect.DetailedStream{{Index: 1, Codec: "aac", BitRate: 192000}},
				Subtitles: []mediainspect.DetailedStream{{Index: 2, Codec: tc.codec, Tags: map[string]string{"NUMBER_OF_BYTES": tc.byteTag}}},
			}
			runner := &ProductionBenchmarkRunner{}
			err := runner.runSelection(context.Background(), record, evidence,
				[]validatedCandidate{{candidate: candidate, bitDepth: 8, profile: "main", pixelFormat: "yuv420p"}}, rep, rep.SizeBytes)
			if tc.invalidMetadata {
				if err == nil || !strings.Contains(err.Error(), optimization.ReasonInvalidEstimatorInput) {
					t.Fatalf("invalid metadata must fail closed: %v", err)
				}
				return
			}
			if err != nil || evidence.Decision == nil || len(evidence.Decision.Evaluations) != 1 {
				t.Fatalf("selection failed: %+v, %v", evidence.Decision, err)
			}
			eval := evidence.Decision.Evaluations[0]
			if eval.Eligible != tc.eligible {
				t.Fatalf("eligible = %v, want %v: %+v", eval.Eligible, tc.eligible, eval)
			}
			if tc.eligible {
				winner := evidence.Decision.Winner
				if winner == nil || math.Abs(winner.SavingsPercent-70.36) > 0.15 || winner.EstimatedSubtitleBytes != optimization.DefaultTextSubtitleSizeBytes {
					t.Fatalf("expected q65 winner with ~70%% savings: %+v", winner)
				}
				if len(winner.Uncertainties) != 1 || winner.Uncertainties[0] != "subtitle_size_estimated:stream_2" {
					t.Errorf("missing text subtitle uncertainty: %v", winner.Uncertainties)
				}
			} else if eval.EvaluationReason != optimization.ReasonMissingSubtitleSize || evidence.Decision.Winner != nil {
				t.Fatalf("bitmap payload must not be guessed: %+v", evidence.Decision)
			}
		})
	}
}

func TestBenchmarkHeartbeat_IndependentOfPolling(t *testing.T) {
	stateDir := t.TempDir()
	id := "bench-autonomous-heartbeat"
	file := filepath.Join(stateDir, id, "benchmark.json")
	old := time.Now().UTC().Add(-time.Minute)
	record := &BenchmarkRecord{ID: id, RunToken: "token", Status: "running", Progress: 25,
		Phase: "evaluating_metrics", HeartbeatAt: old, LastProgressAt: old,
		ProgressDetails: &transcode.BenchmarkProgressDetails{SampleNumber: 2, CandidateNumber: 3, CandidateID: "q65", Metric: "vmaf", CompletedUnits: 4, TotalUnits: 16},
	}
	if err := SaveBenchmarkAtomic(file, record); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(&WorkerConfig{StateDir: stateDir})
	stop := w.startBenchmarkHeartbeat(context.Background(), id, "token", time.Millisecond)
	defer stop()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var saved *BenchmarkRecord
waitHeartbeat:
	for {
		select {
		case <-deadline.C:
			t.Fatal("worker never persisted a heartbeat without status polling")
		case <-ticker.C:
			var err error
			saved, err = LoadBenchmark(file)
			if err != nil {
				t.Fatal(err)
			}
			if saved.HeartbeatAt.After(old) {
				break waitHeartbeat
			}
		}
	}
	stop()
	if saved.Progress != 25 || !saved.LastProgressAt.Equal(old) || saved.ProgressDetails.Metric != "vmaf" {
		t.Fatalf("heartbeat must not invent completed work: %+v", saved)
	}
	var status transcode.BenchmarkStatus
	applyBenchmarkProgressStatus(&status, saved)
	if !status.ProgressIsStale || !status.LastProgressAt.Equal(old) || !status.HeartbeatAt.After(old) {
		t.Fatalf("stale work and live heartbeat must be distinguishable: %+v", status)
	}
	saved.Status = "completed"
	if err := SaveBenchmarkAtomic(file, saved); err != nil {
		t.Fatal(err)
	}
	if err := w.updateBenchmarkProgress(id, "token", 0, "", nil, true); err == nil {
		t.Fatal("heartbeat should refuse terminal jobs")
	}
}

func TestBenchmarkRunner_PersistsSampleCandidateAndMetricBeforeCommands(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(source, []byte("source content"), 0600); err != nil {
		t.Fatal(err)
	}
	ffmpeg, probe, _ := setupMockTools(t, dir, sdr8BitProbeJSON, "")
	stateDir := filepath.Join(dir, "state")
	id := "bench-detail-snapshots"
	file := filepath.Join(stateDir, id, "benchmark.json")
	snapshots := filepath.Join(dir, "snapshots")
	if err := os.Mkdir(snapshots, 0700); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	// Each child records durable state before doing work; no status RPC triggers
	// the updates, and separate PID files avoid concurrent append interleaving.
	prefix := fmt.Sprintf("#!/bin/sh\ncp %q %q/\"$$.json\"\n", file, snapshots)
	if err := os.WriteFile(ffmpeg, []byte(strings.Replace(string(script), "#!/bin/sh\n", prefix, 1)), 0700); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(&WorkerConfig{StateDir: stateDir, AllowedRoots: []string{dir}, FFmpeg: ffmpeg, FFprobe: probe})
	record := &BenchmarkRecord{ID: id, RunToken: "token", Status: "running", Source: source, Metric: "vmaf",
		Samples:    []transcode.BenchmarkSampleWindow{{Index: 7, StartSeconds: 10, DurationSeconds: 10}},
		Candidates: []transcode.BenchmarkCandidate{{ID: "q65", Quality: 65}},
	}
	if err := SaveBenchmarkAtomic(file, record); err != nil {
		t.Fatal(err)
	}
	if err := (&ProductionBenchmarkRunner{}).RunBenchmark(context.Background(), w, record); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(snapshots, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, file := range files {
		snapshot, err := LoadBenchmark(file)
		if err != nil {
			t.Fatal(err)
		}
		details := snapshot.ProgressDetails
		if details == nil {
			continue
		} // capability probes precede sampled work
		if details.SampleNumber != 1 || details.TotalUnits != 3 || snapshot.LastProgressAt.IsZero() {
			t.Fatalf("bad durable work snapshot: %+v", snapshot)
		}
		switch snapshot.Phase {
		case "extracting_samples":
			seen["sample"] = true
		case "encoding_candidates":
			seen["candidate"] = details.CandidateID == "q65" && details.CandidateNumber == 1 && snapshot.Progress > 0
		case "evaluating_metrics":
			seen["metric"] = details.Metric == "vmaf" && details.CandidateID == "q65" && details.CompletedUnits == 2
		}
	}
	if !seen["sample"] || !seen["candidate"] || !seen["metric"] {
		t.Fatalf("missing autonomous stages: %v", seen)
	}
}

func TestBenchmarkCapacityReconcilePreservesNewerState(t *testing.T) {
	for _, status := range []string{"completed", "cancelled", "replaced", "stopped"} {
		t.Run(status, func(t *testing.T) {
			stateDir := t.TempDir()
			id := "bench-capacity-recheck"
			jobDir := filepath.Join(stateDir, id)
			file := filepath.Join(jobDir, "benchmark.json")
			sample := filepath.Join(jobDir, "samples", "sample.mkv")
			if err := os.MkdirAll(filepath.Dir(sample), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sample, []byte("preserve newer samples"), 0600); err != nil {
				t.Fatal(err)
			}
			old := &BenchmarkRecord{ID: id, Status: "running", RunToken: "old-token", PID: 1 << 30, Progress: 10}
			latest := *old
			latest.Progress = 99
			latest.HeartbeatAt = time.Now().UTC()
			latest.Evidence = &BenchmarkExecutionEvidence{SourceResolution: "1920x1080"}
			switch status {
			case "completed", "cancelled":
				latest.Status = status
				latest.FinishedAt = latest.HeartbeatAt
			case "replaced":
				latest.RunToken = "new-token"
			}
			if err := SaveBenchmarkAtomic(file, &latest); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			w := NewWorker(&WorkerConfig{StateDir: stateDir})
			got, err := w.reconcileStoppedBenchmark(jobDir, old)
			if err != nil {
				t.Fatal(err)
			}
			if status == "stopped" {
				if got.Status != "failed" || got.Progress != 99 || !got.HeartbeatAt.Equal(latest.HeartbeatAt) || got.Evidence.SourceResolution != "1920x1080" {
					t.Fatalf("stopped execution lost newer durable evidence: %+v", got)
				}
				if _, err := os.Stat(sample); !os.IsNotExist(err) {
					t.Fatalf("successfully failed execution should clean its samples: %v", err)
				}
				return
			}
			after, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatalf("capacity scan overwrote newer %s state: %+v", status, got)
			}
			if _, err := os.Stat(sample); err != nil {
				t.Fatalf("capacity scan removed newer execution samples: %v", err)
			}
		})
	}
}
