package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// timedMockSpec configures a mock ffmpeg with per-sample timing and VMAF scores
// plus nanosecond start/end markers for encode and metric invocations.
type timedMockSpec struct {
	probeJSON   string
	encSleep    map[int]float64 // sample index -> seconds to sleep during encode
	metSleep    map[int]float64 // sample index -> seconds to sleep during metric
	vmafScore   map[int]float64 // sample index -> VMAF mean to report
	encFailQual int             // quality value whose encodes fail (-1 disables)
}

func setupTimedMockTools(t *testing.T, dir string, spec timedMockSpec) (string, string, string) {
	t.Helper()

	mockProbe := filepath.Join(dir, "mock_ffprobe.sh")
	probeScript := fmt.Sprintf(`#!/bin/sh
cat << 'EOF'
%s
EOF
`, spec.probeJSON)
	if err := os.WriteFile(mockProbe, []byte(probeScript), 0755); err != nil {
		t.Fatalf("writing mock ffprobe: %v", err)
	}

	logFile := filepath.Join(dir, "ffmpeg_calls.log")
	tsFile := filepath.Join(dir, "ffmpeg_ts.log")
	mockFFmpeg := filepath.Join(dir, "mock_ffmpeg.sh")

	sleepFor := func(m map[int]float64, sample int) string {
		if s, ok := m[sample]; ok && s > 0 {
			return fmt.Sprintf("sleep %.1f", s)
		}
		return "true"
	}
	scoreFor := func(sample int) string {
		if s, ok := spec.vmafScore[sample]; ok {
			return strconv.FormatFloat(s, 'f', 4, 64)
		}
		return "96.5000"
	}
	var encCases, metCases strings.Builder
	for sample := 0; sample < 8; sample++ {
		fmt.Fprintf(&encCases, "      *sample_%d.mkv)\n        %s\n        ;;\n", sample, sleepFor(spec.encSleep, sample))
		fmt.Fprintf(&metCases, "      *sample_%d*)\n        score=\"%s\"\n        %s\n        ;;\n", sample, scoreFor(sample), sleepFor(spec.metSleep, sample))
	}
	encFail := ""
	if spec.encFailQual > 0 {
		encFail = fmt.Sprintf(`    case "$*" in
      *"-q:v %d"*)
        echo "simulated encode failure" >&2
        exit 1
        ;;
    esac
`, spec.encFailQual)
	}

	ffmpegScript := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q

case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1 Copyright (c) 2000-2024 the FFmpeg developers"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    cat << 'EOF'
Encoders:
 V..... hevc_videotoolbox    VideoToolbox H.265
 V..... ffv1                 FFmpeg video codec #1
EOF
    exit 0
    ;;
  *"-filters"*)
    cat << 'EOF'
Filters:
  .. libvmaf           VV->V      Calculate the VMAF between two video streams.
  TS ssim              VV->V      Calculate the SSIM between two video streams.
  .S scale             V->V       Scale the input video size and/or convert the image format.
  .. format            V->V       Convert the input video to one of the specified pixel formats.
  .. null              V->V       Pass the source unchanged to the output.
  .. fps               V->V       Force constant framerate.
EOF
    exit 0
    ;;
esac

case "$*" in
  *"-filter_complex"*"libvmaf"*)
    echo "met START $(date +%%s%%N) $*" >> %q
    score="96.5000"
    case "$*" in
%s      *)
        ;;
    esac
    for arg in "$@"; do
      case "$arg" in
        *"log_path="*)
          lpath="${arg#*log_path=}"
          lpath="${lpath%%%%:*}"
          mkdir -p "$(dirname "$lpath")"
          cat << VMAF_EOF > "$lpath"
{
  "version": "2.3.1",
  "pooled_metrics": {
    "vmaf": {
      "mean": $score
    }
  }
}
VMAF_EOF
          ;;
      esac
    done
    echo "met END $(date +%%s%%N) $*" >> %q
    exit 0
    ;;
esac

%s
out=""
for last; do out="$last"; done

case "$out" in
  -*|"")
    ;;
  *)
    case "$*" in
      *"hevc_videotoolbox"*)
        echo "enc START $(date +%%s%%N) $*" >> %q
        case "$out" in
%s          *)
            ;;
        esac
        mkdir -p "$(dirname "$out")"
        echo "fake media data payload for $out" > "$out"
        echo "enc END $(date +%%s%%N) $*" >> %q
        ;;
      *)
        mkdir -p "$(dirname "$out")"
        echo "fake media data payload for $out" > "$out"
        ;;
    esac
    ;;
esac
exit 0
`, logFile, tsFile, metCases.String(), tsFile, encFail, tsFile, encCases.String(), tsFile)

	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("writing mock ffmpeg: %v", err)
	}

	return mockFFmpeg, mockProbe, logFile
}

func timedTSFile(logFile string) string {
	return filepath.Join(filepath.Dir(logFile), "ffmpeg_ts.log")
}

type tsInterval struct {
	kind  string // "enc" or "met"
	start int64
	end   int64
}

func parseTSIntervals(t *testing.T, tsFile string) []tsInterval {
	t.Helper()
	data, err := os.ReadFile(tsFile)
	if err != nil {
		t.Fatalf("reading ts log: %v", err)
	}
	var starts = map[string]int64{}
	var intervals []tsInterval
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			t.Fatalf("malformed ts line: %q", line)
		}
		kind, edge := fields[0], fields[1]
		ts, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			t.Fatalf("parsing ts %q: %v", fields[2], err)
		}
		key := kind + "|" + strings.Join(fields[3:], " ")
		if edge == "START" {
			starts[key] = ts
		} else if edge == "END" {
			s, ok := starts[key]
			if !ok {
				t.Fatalf("END without START for %q", key)
			}
			delete(starts, key)
			intervals = append(intervals, tsInterval{kind: kind, start: s, end: ts})
		}
	}
	if len(starts) != 0 {
		t.Fatalf("unmatched START entries remain: %d", len(starts))
	}
	return intervals
}

func maxConcurrency(intervals []tsInterval, kind string) int {
	type event struct {
		ts    int64
		delta int
	}
	var events []event
	for _, in := range intervals {
		if in.kind != kind {
			continue
		}
		events = append(events, event{ts: in.start, delta: 1}, event{ts: in.end, delta: -1})
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].ts != events[j].ts {
			return events[i].ts < events[j].ts
		}
		return events[i].delta < events[j].delta
	})
	cur, max := 0, 0
	for _, e := range events {
		cur += e.delta
		if cur > max {
			max = cur
		}
	}
	return max
}

func hasOverlap(a, b []tsInterval) bool {
	for _, x := range a {
		for _, y := range b {
			if x.start < y.end && y.start < x.end {
				return true
			}
		}
	}
	return false
}

func filterIntervals(intervals []tsInterval, kind string) []tsInterval {
	var out []tsInterval
	for _, in := range intervals {
		if in.kind == kind {
			out = append(out, in)
		}
	}
	return out
}

func runPipelineBenchmarkForTest(t *testing.T, record *BenchmarkRecord, mockFFmpeg, mockProbe, dir string) {
	t.Helper()
	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)
	runner := &ProductionBenchmarkRunner{}
	if err := runner.RunBenchmark(context.Background(), worker, record); err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}
}

func TestPipeline_ConcurrencyBoundsAndOverlap(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("fake source media"), 0644); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	// 1 candidate x 4 samples; every encode and metric sleeps 0.7s so true
	// parallelism is observable well above scheduling jitter.
	spec := timedMockSpec{
		probeJSON: sdr8BitProbeJSON,
		encSleep:  map[int]float64{0: 0.7, 1: 0.7, 2: 0.7, 3: 0.7},
		metSleep:  map[int]float64{0: 0.7, 1: 0.7, 2: 0.7, 3: 0.7},
		vmafScore: map[int]float64{0: 96.5, 1: 96.5, 2: 96.5, 3: 96.5},
	}
	mockFFmpeg, mockProbe, logFile := setupTimedMockTools(t, dir, spec)
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-pipe-bounds",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 5.0},
			{Index: 1, StartSeconds: 20.0, DurationSeconds: 5.0},
			{Index: 2, StartSeconds: 40.0, DurationSeconds: 5.0},
			{Index: 3, StartSeconds: 60.0, DurationSeconds: 5.0},
		},
		Candidates: []transcode.BenchmarkCandidate{{ID: "cand_q65", Quality: 65}},
		Attempt:    1,
	}
	runPipelineBenchmarkForTest(t, record, mockFFmpeg, mockProbe, dir)

	intervals := parseTSIntervals(t, timedTSFile(logFile))
	encs := filterIntervals(intervals, "enc")
	mets := filterIntervals(intervals, "met")
	if len(encs) != 4 {
		t.Fatalf("expected 4 encode intervals, got %d", len(encs))
	}
	if len(mets) != 4 {
		t.Fatalf("expected 4 metric intervals, got %d", len(mets))
	}
	// Hard bounds: never exceed the configured defaults (2/2).
	if got := maxConcurrency(intervals, "enc"); got > 2 {
		t.Errorf("encode concurrency %d exceeds bound 2", got)
	}
	if got := maxConcurrency(intervals, "met"); got > 2 {
		t.Errorf("metric concurrency %d exceeds bound 2", got)
	}
	// Utilization: with 0.7s sleeps the pipeline must actually parallelize.
	if got := maxConcurrency(intervals, "enc"); got != 2 {
		t.Errorf("expected encode parallelism of 2, observed max %d", got)
	}
	if got := maxConcurrency(intervals, "met"); got != 2 {
		t.Errorf("expected metric parallelism of 2, observed max %d", got)
	}
	// Overlap, not just parallel-for: at least one metric must run while an
	// encode is still in flight (no barrier between stages).
	if !hasOverlap(encs, mets) {
		t.Errorf("no encode/metric overlap observed; pipeline has a stage barrier")
	}
	if record.Progress != 99.0 {
		t.Errorf("expected progress 99.0, got %v", record.Progress)
	}
}

func TestPipeline_CustomConcurrencyOneIsRespected(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("fake source media"), 0644); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	spec := timedMockSpec{
		probeJSON: sdr8BitProbeJSON,
		encSleep:  map[int]float64{0: 0.5, 1: 0.5, 2: 0.5},
		metSleep:  map[int]float64{0: 0.5, 1: 0.5, 2: 0.5},
		vmafScore: map[int]float64{0: 96.5, 1: 96.5, 2: 96.5},
	}
	mockFFmpeg, mockProbe, logFile := setupTimedMockTools(t, dir, spec)
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-pipe-one",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 5.0},
			{Index: 1, StartSeconds: 20.0, DurationSeconds: 5.0},
			{Index: 2, StartSeconds: 40.0, DurationSeconds: 5.0},
		},
		Candidates:  []transcode.BenchmarkCandidate{{ID: "cand_q65", Quality: 65}},
		Concurrency: &transcode.BenchmarkConcurrencyConfig{EncodeConcurrency: 1, MetricConcurrency: 1},
		Attempt:     1,
	}
	runPipelineBenchmarkForTest(t, record, mockFFmpeg, mockProbe, dir)

	intervals := parseTSIntervals(t, timedTSFile(logFile))
	if got := maxConcurrency(intervals, "enc"); got != 1 {
		t.Errorf("expected serial encodes with concurrency 1, observed max %d", got)
	}
	if got := maxConcurrency(intervals, "met"); got != 1 {
		t.Errorf("expected serial metrics with concurrency 1, observed max %d", got)
	}
	// Even serial stages must still pipeline: metric for sample 0 overlaps encode of sample 1.
	if !hasOverlap(filterIntervals(intervals, "enc"), filterIntervals(intervals, "met")) {
		t.Errorf("expected encode/metric overlap even with concurrency 1/1")
	}
	if record.Progress != 99.0 {
		t.Errorf("expected progress 99.0, got %v", record.Progress)
	}
}

func TestPipeline_DeterministicOrderUnderScrambledTiming(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("fake source media"), 0644); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	// Reverse completion order: sample 2 finishes first, sample 0 last.
	spec := timedMockSpec{
		probeJSON: sdr8BitProbeJSON,
		encSleep:  map[int]float64{0: 0.9, 1: 0.4, 2: 0.0},
		metSleep:  map[int]float64{0: 0.9, 1: 0.4, 2: 0.0},
		vmafScore: map[int]float64{0: 90.0, 1: 95.0, 2: 100.0},
	}
	mockFFmpeg, mockProbe, _ := setupTimedMockTools(t, dir, spec)
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-pipe-order",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 5.0},
			{Index: 1, StartSeconds: 20.0, DurationSeconds: 5.0},
			{Index: 2, StartSeconds: 40.0, DurationSeconds: 5.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q60", Quality: 60},
			{ID: "cand_q70", Quality: 70},
		},
		Attempt: 1,
	}
	runPipelineBenchmarkForTest(t, record, mockFFmpeg, mockProbe, dir)

	ev := record.Evidence
	if ev == nil {
		t.Fatalf("expected evidence")
	}
	// Candidate samples must be in (candidate, sample) order regardless of finish order.
	wantSamples := []struct {
		id  string
		idx int
	}{
		{"cand_q60", 0}, {"cand_q60", 1}, {"cand_q60", 2},
		{"cand_q70", 0}, {"cand_q70", 1}, {"cand_q70", 2},
	}
	if len(ev.CandidateSamples) != len(wantSamples) {
		t.Fatalf("expected %d candidate samples, got %d", len(wantSamples), len(ev.CandidateSamples))
	}
	for i, want := range wantSamples {
		got := ev.CandidateSamples[i]
		if got.CandidateID != want.id || got.SampleIndex != want.idx {
			t.Errorf("CandidateSamples[%d] = (%s,%d), want (%s,%d)",
				i, got.CandidateID, got.SampleIndex, want.id, want.idx)
		}
	}
	if len(ev.MetricSamples) != len(wantSamples) {
		t.Fatalf("expected %d metric samples, got %d", len(wantSamples), len(ev.MetricSamples))
	}
	for i, want := range wantSamples {
		got := ev.MetricSamples[i]
		if got.CandidateID != want.id || got.SampleIndex != want.idx {
			t.Errorf("MetricSamples[%d] = (%s,%d), want (%s,%d)",
				i, got.CandidateID, got.SampleIndex, want.id, want.idx)
		}
		if got.VMAF == nil {
			t.Errorf("MetricSamples[%d] missing VMAF score", i)
		}
	}
	// Aggregates must reflect true per-sample scores despite scrambled completion.
	if len(ev.CandidateMetrics) != 2 {
		t.Fatalf("expected 2 candidate metrics, got %d", len(ev.CandidateMetrics))
	}
	for _, cm := range ev.CandidateMetrics {
		agg := cm.Aggregate
		if !agg.Valid {
			t.Errorf("expected valid aggregate for %s", cm.CandidateID)
			continue
		}
		if agg.MeanScore != 95.0 || agg.MinScore != 90.0 || agg.MaxScore != 100.0 {
			t.Errorf("aggregate for %s = mean/min/max %.4f/%.4f/%.4f, want 95/90/100",
				cm.CandidateID, agg.MeanScore, agg.MinScore, agg.MaxScore)
		}
	}
}

func TestPipeline_CancellationStopsWork(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("fake source media"), 0644); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	spec := timedMockSpec{
		probeJSON: sdr8BitProbeJSON,
		encSleep:  map[int]float64{0: 8.0, 1: 8.0},
		metSleep:  map[int]float64{0: 8.0, 1: 8.0},
		vmafScore: map[int]float64{0: 96.5, 1: 96.5},
	}
	mockFFmpeg, mockProbe, _ := setupTimedMockTools(t, dir, spec)
	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-pipe-cancel",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 5.0},
			{Index: 1, StartSeconds: 20.0, DurationSeconds: 5.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q60", Quality: 60},
			{ID: "cand_q70", Quality: 70},
		},
		Attempt: 1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	runner := &ProductionBenchmarkRunner{}
	start := time.Now()
	err := runner.RunBenchmark(ctx, worker, record)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		t.Errorf("expected context cancellation error, got: %v", err)
	}
	// 4 encodes x 8s sequential would take far longer; cancellation must win promptly.
	if elapsed > 20*time.Second {
		t.Errorf("cancellation took too long: %v", elapsed)
	}
}

func TestPipeline_CandidateMetricFailureStaysCandidateLevel(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("fake source media"), 0644); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	// q60 reports an out-of-range VMAF score (parse failure => candidate-level,
	// matching sequential fallback/safety behavior); q65 stays healthy.
	mockFFmpeg, mockProbe, _ := setupMockToolsWithQualityVMAF(t, dir, sdr8BitProbeJSON,
		map[int]float64{60: 999.0, 65: 96.5})
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-pipe-candfail",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 5.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q60", Quality: 60},
			{ID: "cand_q65", Quality: 65},
		},
		Attempt: 1,
	}
	runPipelineBenchmarkForTest(t, record, mockFFmpeg, mockProbe, dir)

	ev := record.Evidence
	if ev == nil || ev.Decision == nil {
		t.Fatalf("expected decision despite candidate-level metric failure")
	}
	if ev.Decision.Winner == nil || ev.Decision.Winner.CandidateID != "cand_q65" {
		t.Fatalf("expected winner cand_q65, got %+v", ev.Decision.Winner)
	}
	foundInvalid := false
	for _, cm := range ev.CandidateMetrics {
		if cm.CandidateID == "cand_q60" {
			if cm.Aggregate.Valid {
				t.Errorf("expected invalid aggregate for failed candidate cand_q60")
			}
			foundInvalid = true
		}
	}
	if !foundInvalid {
		t.Errorf("missing candidate metrics for cand_q60")
	}
}

func TestPipeline_EncodeFailureRemainsJobFatal(t *testing.T) {
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("fake source media"), 0644); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	// Encodes for q60 fail; the healthy q65 must not rescue the job: encode
	// failures keep existing job-fatal behavior under the pipeline.
	spec := timedMockSpec{
		probeJSON:   sdr8BitProbeJSON,
		encFailQual: 60,
		vmafScore:   map[int]float64{0: 96.5},
	}
	mockFFmpeg, mockProbe, _ := setupTimedMockTools(t, dir, spec)
	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-pipe-encfail",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 5.0},
		},
		Candidates: []transcode.BenchmarkCandidate{
			{ID: "cand_q60", Quality: 60},
			{ID: "cand_q65", Quality: 65},
		},
		Attempt: 1,
	}
	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected job-fatal encode error, got nil")
	}
	if !strings.Contains(err.Error(), "cand_q60") {
		t.Errorf("expected error to name failed candidate, got: %v", err)
	}
	if len(err.Error()) > 2048 {
		t.Errorf("error message exceeds 2048 bytes: %d", len(err.Error()))
	}
}

func TestPipeline_ExhaustiveWinnerMatchesGolden(t *testing.T) {
	// Golden compatibility: pipelined exhaustive evaluation of the Phase 1
	// calibration set must select q65 exactly as the sequential path did.
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("fake source media"), 0644); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	mockFFmpeg, mockProbe, _ := setupMockToolsWithQualityVMAF(t, dir, sdr8BitProbeJSON, adaptiveTestScoresTypical())
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-pipe-golden",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 5.0},
		},
		Candidates: adaptiveTestCandidates7(),
		Attempt:    1,
	}
	runPipelineBenchmarkForTest(t, record, mockFFmpeg, mockProbe, dir)

	dec := record.Evidence.Decision
	if dec == nil || dec.Winner == nil {
		t.Fatalf("expected winner, got %+v", dec)
	}
	if dec.Winner.CandidateID != "cand_q65" {
		t.Fatalf("pipelined exhaustive winner = %q, want cand_q65", dec.Winner.CandidateID)
	}
	if record.Progress != 99.0 {
		t.Errorf("expected progress 99.0, got %v", record.Progress)
	}
}

func TestPipeline_PartialEvidenceCompleteWhenUnitFails(t *testing.T) {
	// Regression test: one pipelined encode unit fails fast while its sibling
	// encode is still in flight (slow success). Both the successful and the
	// failed CandidateSamples entries must be preserved deterministically in
	// (candidate, sample) order. Pre-fix, the first job-fatal error cancelled
	// the shared pipeline context, SIGKILLing the in-flight sibling encode,
	// whose outcome was then discarded (expected 2 candidate samples, got 1).
	dir := t.TempDir()
	sourceFile := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(sourceFile, []byte("fake source media"), 0644); err != nil {
		t.Fatalf("writing source: %v", err)
	}

	mockProbe := filepath.Join(dir, "mock_ffprobe.sh")
	probeScript := "#!/bin/sh\ncat << 'EOF'\n" + sdr8BitProbeJSON + "\nEOF\n"
	if err := os.WriteFile(mockProbe, []byte(probeScript), 0755); err != nil {
		t.Fatalf("writing mock ffprobe: %v", err)
	}
	mockFFmpeg := filepath.Join(dir, "mock_ffmpeg.sh")
	ffmpegScript := `#!/bin/sh
case "$*" in
  *"-version"*)
    echo "ffmpeg version 7.1 Copyright (c) 2000-2024 the FFmpeg developers"
    exit 0
    ;;
  *"-h encoder=hevc_videotoolbox"*)
    cat << 'EOF'
Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    Supported pixel formats: nv12 p010le yuv420p
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 2) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -prio_speed        <boolean>    E..V....... Prioritize encoding speed (default false)
  -spatial_aq        <boolean>    E..V....... Spatial AQ (default false)
  -realtime          <boolean>    E..V....... Realtime (default false)
EOF
    exit 0
    ;;
  *"-encoders"*)
    cat << 'EOF'
Encoders:
 V..... hevc_videotoolbox    VideoToolbox H.265
 V..... ffv1                 FFmpeg video codec #1
EOF
    exit 0
    ;;
  *"-filters"*)
    cat << 'EOF'
Filters:
  .. libvmaf           VV->V      Calculate the VMAF between two video streams.
  TS ssim              VV->V      Calculate the SSIM between two video streams.
EOF
    exit 0
    ;;
esac

case "$*" in
  *"-filter_complex"*"libvmaf"*)
    for arg in "$@"; do
      case "$arg" in
        *"log_path="*)
          lpath="${arg#*log_path=}"
          lpath="${lpath%%:*}"
          mkdir -p "$(dirname "$lpath")"
          cat << 'VMAF_EOF' > "$lpath"
{
  "version": "2.3.1",
  "pooled_metrics": {
    "vmaf": {
      "mean": 95.500000
    }
  }
}
VMAF_EOF
          ;;
      esac
    done
    exit 0
    ;;
esac

# Candidate encode for sample 1 fails fast; sibling sample 0 encode is slow
# so it is guaranteed to still be in flight when the failure lands.
case "$*" in
  *"ref_sample_1"*hevc_videotoolbox*|*hevc_videotoolbox*"ref_sample_1"*)
    echo "simulated encode failure for sample 1" >&2
    exit 1
    ;;
esac

out=""
for last; do out="$last"; done
case "$*" in
  *hevc_videotoolbox*)
    sleep 1
    ;;
esac
case "$out" in
  -*|"")
    ;;
  *)
    mkdir -p "$(dirname "$out")"
    echo "fake media data payload for $out" > "$out"
    ;;
esac
exit 0
`
	if err := os.WriteFile(mockFFmpeg, []byte(ffmpegScript), 0755); err != nil {
		t.Fatalf("writing mock ffmpeg: %v", err)
	}

	cfg := &WorkerConfig{
		StateDir:        filepath.Join(dir, "state"),
		AllowedRoots:    []string{dir},
		MaxParallelJobs: 1,
		FFmpeg:          mockFFmpeg,
		FFprobe:         mockProbe,
	}
	worker := NewWorker(cfg)
	record := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		ID:              "bench-pipe-partial",
		Status:          "running",
		Source:          sourceFile,
		Metric:          "vmaf",
		Samples: []transcode.BenchmarkSampleWindow{
			{Index: 0, StartSeconds: 5.0, DurationSeconds: 5.0},
			{Index: 1, StartSeconds: 20.0, DurationSeconds: 5.0},
		},
		Candidates: []transcode.BenchmarkCandidate{{ID: "cand_q65", Quality: 65}},
		Attempt:    1,
	}
	runner := &ProductionBenchmarkRunner{}
	err := runner.RunBenchmark(context.Background(), worker, record)
	if err == nil {
		t.Fatalf("expected job-fatal encode error, got nil")
	}

	ev := record.Evidence
	if ev == nil {
		t.Fatalf("expected partial evidence on failure (run err: %v)", err)
	}
	if len(ev.CandidateSamples) != 2 {
		t.Fatalf("expected 2 candidate samples in evidence (1 success, 1 failure), got %d", len(ev.CandidateSamples))
	}
	if got := ev.CandidateSamples[0]; got.SampleIndex != 0 || got.Error != "" {
		t.Errorf("CandidateSamples[0] = sample %d err %q, want sample 0 with no error", got.SampleIndex, got.Error)
	}
	if got := ev.CandidateSamples[1]; got.SampleIndex != 1 || got.Error == "" {
		t.Errorf("CandidateSamples[1] = sample %d err %q, want sample 1 with failure error", got.SampleIndex, got.Error)
	}
}

func TestResolvePipelineConcurrency(t *testing.T) {
	cases := []struct {
		name         string
		cfg          *transcode.BenchmarkConcurrencyConfig
		wantE, wantM int
	}{
		{"nil selects defaults", nil, 2, 2},
		{"zero selects defaults", &transcode.BenchmarkConcurrencyConfig{}, 2, 2},
		{"explicit", &transcode.BenchmarkConcurrencyConfig{EncodeConcurrency: 1, MetricConcurrency: 3}, 1, 3},
		{"max", &transcode.BenchmarkConcurrencyConfig{EncodeConcurrency: 4, MetricConcurrency: 4}, 4, 4},
		{"over max clamps", &transcode.BenchmarkConcurrencyConfig{EncodeConcurrency: 99, MetricConcurrency: 99}, 4, 4},
		{"negative selects defaults (validation rejects negatives upstream)", &transcode.BenchmarkConcurrencyConfig{EncodeConcurrency: -2, MetricConcurrency: -1}, 2, 2},
		{"partial zero", &transcode.BenchmarkConcurrencyConfig{EncodeConcurrency: 3}, 3, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotE, gotM := resolvePipelineConcurrency(&BenchmarkRecord{Concurrency: tc.cfg})
			if gotE != tc.wantE || gotM != tc.wantM {
				t.Errorf("resolvePipelineConcurrency() = (%d,%d), want (%d,%d)", gotE, gotM, tc.wantE, tc.wantM)
			}
			if gotE < 1 || gotM < 1 {
				t.Errorf("concurrency must stay positive, got (%d,%d)", gotE, gotM)
			}
		})
	}
}
