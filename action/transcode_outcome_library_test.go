package action

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// outcomeLibraryEnv runs size-mode transcode_media against a worker stub whose
// searches pick the x265 CRF 26 rung (or nothing) and whose full encodes come
// back with the given final verdict.
type outcomeLibraryEnv struct {
	engine   *Engine
	store    *store.Store
	source   string
	mock     *mockTranscodeExecutor
	noWinner bool
	verdict  string
	encoded  []transcode.Request
}

func newOutcomeLibraryEnv(t *testing.T) *outcomeLibraryEnv {
	t.Helper()
	env := &outcomeLibraryEnv{verdict: "pass"}
	dir := t.TempDir()
	probe := dir + "/ffprobe"
	env.source = dir + "/source.mkv"
	_ = os.WriteFile(env.source, make([]byte, 1000), 0600)
	_ = os.WriteFile(probe, []byte(`#!/bin/sh
cat <<'JSON'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080,"avg_frame_rate":"24/1","r_frame_rate":"24/1","bits_per_raw_sample":"8"}],"format":{"format_name":"matroska","duration":"100","size":"1000"},"chapters":[]}
JSON
`), 0700)
	requests := map[string]transcode.BenchmarkRequest{}
	env.mock = &mockTranscodeExecutor{
		capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) { return modeTestCapabilities(), nil },
		benchmarkSubmitFunc: func(_ context.Context, req transcode.BenchmarkRequest) (transcode.BenchmarkJob, error) {
			requests[req.ID] = req
			return transcode.BenchmarkJob{ID: req.ID}, nil
		},
		benchmarkStatusFunc: func(_ context.Context, id string) (transcode.BenchmarkStatus, error) {
			req := requests[id]
			decision := &transcode.BenchmarkDecision{DecisionReason: "all_candidates_invalid"}
			for i, c := range req.Candidates {
				vmaf := &quality.VMAFStats{Mean: 94, P5: 90, Min: 85, WorstWindowMean: 91, FramesTotal: 100}
				ev := transcode.BenchmarkCandidateEvaluation{CandidateID: c.ID, CandidateIndex: i, VideoCodec: transcode.BenchmarkCandidateVideoCodec(c), Quality: c.Quality, Preset: c.Preset, EstimatedBytes: 500, SavingsPercent: 50, MetricType: "vmaf", VMAF: transcode.SummarizeVMAF(vmaf)}
				decision.Evaluations = append(decision.Evaluations, ev)
				if !env.noWinner && decision.Winner == nil && ev.VideoCodec == transcode.VideoCodecLibX265 && c.Quality == 26 && c.Preset == "medium" {
					decision.Winner = &transcode.BenchmarkWinner{CandidateID: c.ID, CandidateIndex: i, VideoCodec: ev.VideoCodec, Quality: c.Quality, Preset: c.Preset, Tune: c.Tune, X265: c.X265.Clone(),
						VideoProfile: c.VideoProfile, PixelFormat: c.PixelFormat, ExpectedBitDepth: 10, MetricType: "vmaf", Score: 94, MinimumMet: true, TargetReached: true,
						EstimatedTotalBytes: 500, SavingsBytes: 500, SavingsPercent: 50, GOPSize: c.GOPSize, BFrames: c.BFrames}
					decision.DecisionReason = "smallest passing setting"
				}
			}
			return transcode.BenchmarkStatus{ID: id, Status: transcode.StatusCompleted, Progress: 100, Decision: decision,
				Automatic: &transcode.BenchmarkAutomaticEvidence{VerifiedSamples: len(req.Samples)},
				Quality:   []transcode.BenchmarkCandidateQualityEvidence{{CandidateID: "unrelated", CAMBI: &quality.CAMBIStats{Mean: 1, Max: 2}}}}, nil
		},
		submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
			env.encoded = append(env.encoded, req)
			return transcode.Job{ID: req.ID}, nil
		},
		statusFunc: func(_ context.Context, id string) (transcode.JobStatus, error) {
			ev := &transcode.FinalQualityEvidence{Verdict: env.verdict, CandidateSizeBytes: 500, VMAF: &quality.VMAFStats{Mean: 93, P5: 89, WorstWindowMean: 90, FramesTotal: 2400}, CAMBI: &quality.CAMBIStats{Mean: 1.5, Max: 4}}
			if env.verdict == "fail" {
				return transcode.JobStatus{ID: id, Status: transcode.StatusFailed, ReasonCode: "quality_not_met", QualityEvidence: ev}, nil
			}
			return transcode.JobStatus{ID: id, Status: transcode.StatusCompleted, Progress: 100, QualityEvidence: ev}, nil
		},
	}
	env.engine, env.store = setupTranscodeEngine(t, env.mock, probe, []string{dir}, []string{dir}, true)
	t.Cleanup(func() { env.store.Close() })
	return env
}

func (env *outcomeLibraryEnv) run(t *testing.T, extra map[string]any) *ActionResult {
	t.Helper()
	inputs := map[string]any{"path": env.source, "mode": "size"}
	for k, v := range extra {
		inputs[k] = v
	}
	res, err := env.engine.Run(context.Background(), "transcode_media", inputs)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (env *outcomeLibraryEnv) rows(t *testing.T, res *ActionResult) []store.TranscodeOutcome {
	t.Helper()
	rows, err := env.store.TranscodeOutcomesForSource(getString(res.State, "original_sha256"), getString(res.State, "policy_digest"), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func libraryReport(t *testing.T, res *ActionResult) outcomeLibraryReport {
	t.Helper()
	var report outcomeLibraryReport
	b, _ := json.Marshal(res.State[outcomeLibraryKey])
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestOutcomeLibraryRecordsSearchAndFinalValidation(t *testing.T) {
	env := newOutcomeLibraryEnv(t)
	res := env.run(t, nil)
	if atomic.LoadInt32(&env.mock.benchmarkSubmitCalls) != 1 || len(env.encoded) != 1 {
		t.Fatalf("first action must search and encode: %d searches, %d encodes", env.mock.benchmarkSubmitCalls, len(env.encoded))
	}
	rows := env.rows(t, res)
	if len(rows) != 1 {
		t.Fatalf("one recorded search: %+v", rows)
	}
	row := rows[0]
	if row.Outcome != store.OutcomeWinner || row.FinalVerdict != "pass" || row.BenchmarkJobID != getString(res.State, "benchmark_job_id") {
		t.Fatalf("recorded row: %+v", row)
	}
	if row.SourceSize != 1000 || row.Width != 1920 || row.Height != 1080 || row.BitDepth != 8 || row.DurationSec != 100 || row.VideoCodec != "h264" || row.PolicyVersion == "" {
		t.Fatalf("source fingerprint: %+v", row)
	}
	if row.FolderKey == "" || strings.Contains(row.FolderKey, "/") || row.Season != -1 {
		t.Fatalf("identity without library: %+v", row)
	}
	var points []outcomePoint
	if json.Unmarshal([]byte(row.PointsJSON), &points) != nil || len(points) == 0 || points[0].VMAFMean == nil || points[0].VMAFP5 == nil || points[0].WorstWindowMean == nil {
		t.Fatalf("measured points: %s", row.PointsJSON)
	}
	var final outcomeFinal
	if json.Unmarshal([]byte(row.FinalJSON), &final) != nil || final.CandidateSizeBytes != 500 || final.CAMBIMax == nil || *final.CAMBIMax != 4 {
		t.Fatalf("final numbers: %s", row.FinalJSON)
	}
	if rec := winnerRecordOf(row); rec == nil || rec.Candidate.ID != rec.Winner.CandidateID || rec.Evaluation == nil {
		t.Fatalf("winner record: %s", row.WinnerJSON)
	}
	if strings.Contains(row.PointsJSON+row.WinnerJSON+row.FinalJSON, env.source) {
		t.Fatal("a record must not hold the media path")
	}
}

func TestOutcomeLibraryReusesValidatedWinnerAndStillValidates(t *testing.T) {
	env := newOutcomeLibraryEnv(t)
	first := env.run(t, nil)
	second := env.run(t, nil)
	if atomic.LoadInt32(&env.mock.benchmarkSubmitCalls) != 1 {
		t.Fatalf("the second action must not search again: %d searches", env.mock.benchmarkSubmitCalls)
	}
	if len(env.encoded) != 2 {
		t.Fatalf("the second action must still encode: %d", len(env.encoded))
	}
	plan := env.encoded[1].Plan
	if plan == nil || plan.QualityValidation == nil || plan.QualityValidation.BenchmarkRequestDigest != getString(second.State, "benchmark_request_digest") || len(plan.QualityValidation.Samples) == 0 {
		t.Fatalf("reused plan must carry full final validation: %+v", plan)
	}
	if plan.Quality != 26 || plan.VideoCodec != transcode.VideoCodecLibX265 || plan.Preset != "medium" {
		t.Fatalf("reused settings: %+v", plan)
	}
	reuse := libraryReuseOf(second.State)
	if reuse == nil || reuse.FromActionID != first.ID {
		t.Fatalf("reuse state: %+v", second.State[libraryReuseKey])
	}
	report := libraryReport(t, second)
	if report.Reused != first.ID || len(report.Matches) != 1 || report.Matches[0].FinalVerdict != "pass" || !strings.Contains(report.Note, "passed final validation") {
		t.Fatalf("report: %+v", report)
	}
	rows := env.rows(t, second)
	if len(rows) != 2 || rows[0].ActionID != second.ID || rows[0].ReusedFrom != first.ID || rows[0].FinalVerdict != "pass" {
		t.Fatalf("the reused search is recorded with its own final validation: %+v", rows)
	}

	// Reuse turned off searches again.
	third := env.run(t, map[string]any{reuseLibraryOutcomeInput: false})
	if atomic.LoadInt32(&env.mock.benchmarkSubmitCalls) != 2 || libraryReuseOf(third.State) != nil {
		t.Fatalf("reuse_library_outcome=false must search: %d searches", env.mock.benchmarkSubmitCalls)
	}
	if libraryReport(t, third).NotReused == "" {
		t.Fatal("the report must say why nothing was reused")
	}
}

func TestOutcomeLibraryDoesNotReuseAFailedFinalValidation(t *testing.T) {
	env := newOutcomeLibraryEnv(t)
	env.verdict = "fail"
	env.run(t, nil)
	env.verdict = "pass"
	second := env.run(t, nil)
	if atomic.LoadInt32(&env.mock.benchmarkSubmitCalls) != 2 || libraryReuseOf(second.State) != nil {
		t.Fatalf("a winner whose full encode failed must not stand in for a search: %d searches", env.mock.benchmarkSubmitCalls)
	}
	report := libraryReport(t, second)
	if !strings.Contains(report.Note, "failed final validation") || report.NotReused == "" {
		t.Fatalf("report: %+v", report)
	}
}

func TestOutcomeLibraryReportsEarlierNoWinner(t *testing.T) {
	env := newOutcomeLibraryEnv(t)
	env.noWinner = true
	first := env.run(t, nil)
	if rows := env.rows(t, first); len(rows) != 1 || rows[0].Outcome != store.OutcomeNoWinner || rows[0].WinnerJSON != "" || rows[0].PointsJSON == "" {
		t.Fatalf("no-winner row: %+v", rows)
	}
	second := env.run(t, nil)
	report := libraryReport(t, second)
	if !strings.Contains(report.Note, "This exact source already failed with policy") || !strings.Contains(report.Note, first.ID) {
		t.Fatalf("note: %q", report.Note)
	}
	if libraryReuseOf(second.State) != nil {
		t.Fatal("a no-winner record is never reused")
	}
}

func TestOutcomeLibraryRespectsSearchReview(t *testing.T) {
	env := newOutcomeLibraryEnv(t)
	env.noWinner = true
	env.run(t, nil)
	env.run(t, nil)
	third := env.run(t, nil)
	if third.Status != StatusWaitingDecision || getString(third.State, "benchmark_search_review_required") == "" {
		t.Fatalf("two no-winner rounds still need a review: %s", third.Status)
	}
	if atomic.LoadInt32(&env.mock.benchmarkSubmitCalls) != 2 {
		t.Fatalf("no third search before the review: %d", env.mock.benchmarkSubmitCalls)
	}
	if libraryReport(t, third).Note == "" {
		t.Fatal("the review shows what the library knows")
	}
}

func TestParseEpisodeInfo(t *testing.T) {
	for in, want := range map[string][2]int{"S01E02": {1, 2}, "S02E03-E04": {2, 3}, "S01E09-S02E01": {1, 9}, "": {-1, -1}, "special": {-1, -1}} {
		s, e := parseEpisodeInfo(in)
		if s != want[0] || e != want[1] {
			t.Fatalf("%q: %d %d", in, s, e)
		}
	}
	if folderKey("/a/b/c.mkv") != folderKey("/a/b/d.mkv") || folderKey("/a/b/c.mkv") == folderKey("/a/x/c.mkv") || folderKey("") != "" {
		t.Fatal("folder keys group a directory's files")
	}
}
