package transcodeworker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

const (
	continuationFingerprint = "fp-current"
	continuationPriorID     = "bench-prior"
	continuationPriorDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
)

func continuationX265Ladder(full []validatedCandidate) []validatedCandidate {
	var out []validatedCandidate
	for _, vc := range full {
		if isVideoToolbox(vc.candidate.VideoCodec) || strings.HasPrefix(vc.candidate.ID, fallbackHardwarePrefix) {
			continue
		}
		vc.index = len(out)
		out = append(out, vc)
	}
	return out
}

func recordCandidates(vcs []validatedCandidate) []transcode.BenchmarkCandidate {
	out := make([]transcode.BenchmarkCandidate, 0, len(vcs))
	for _, vc := range vcs {
		if !strings.HasPrefix(vc.candidate.ID, fallbackHardwarePrefix) {
			out = append(out, vc.candidate)
		}
	}
	return out
}

// continuationHardEpisode is a hard episode whose only result in the first
// search is a thin media engine one: every medium x265 setting fails quality,
// and the slow ones, which that search never measured, would pass.
func continuationHardEpisode() map[string]replaySetting {
	settings := hardEpisode()
	settings[replayKey("", 75)] = replaySetting{bytes: 20000, discovery: replayScore{87.1, 89.2}, holdout: 88.5}
	return settings
}

// priorSearch runs the VideoToolbox search and persists it as the worker does.
func priorSearch(t *testing.T, stateDir string) (*BenchmarkRecord, *BenchmarkExecutionEvidence) {
	t.Helper()
	hw := &transcode.HardwareSearchRules{MarginPercent: 15, QualityValues: []int{45, 50, 55, 60}, FallbackQualityValues: []int{65, 70, 75}, GOPSize: 240, BFrames: 1}
	record := replayBenchmarkRecord(replayRules(), hw)
	ladder := hardwareFallbackLadder()
	record.ID, record.Source, record.SourceSHA256, record.RequestDigest, record.Status = continuationPriorID, "/media/show.mkv", strings.Repeat("a", 64), continuationPriorDigest, "completed"
	record.Candidates = recordCandidates(ladder)
	var res replayResult
	runner := &ProductionBenchmarkRunner{automaticHook: replayAutomaticHook(t, continuationHardEpisode(), &res)}
	evidence := &BenchmarkExecutionEvidence{SourceBitDepth: 8}
	rep := mediainspect.DetailedReport{DurationSec: 600, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 25}}}
	if err := runner.runAutomaticCandidates(context.Background(), nil, record, evidence, "", ladder, rep, 10_000_000, time.Time{}, newBenchmarkProgressReporter(nil, record, 100)); err != nil {
		t.Fatal(err)
	}
	if evidence.Decision.Winner == nil || evidence.Decision.Winner.CandidateID != "fallback-hardware-cand_q75" {
		t.Fatalf("the earlier search did not pick the hardware result: %+v", evidence.Decision)
	}
	for i := range evidence.Quality {
		evidence.Quality[i].CapabilityFingerprint = continuationFingerprint
	}
	// The metric step of a real search records the decimation it measured
	// with; the continued search has not run it yet when it looks for reuse.
	evidence.VMAFTemporalDecimation = 1
	record.Evidence = evidence
	if err := os.MkdirAll(filepath.Join(stateDir, continuationPriorID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveBenchmarkAtomic(filepath.Join(stateDir, continuationPriorID, "benchmark.json"), record); err != nil {
		t.Fatal(err)
	}
	return record, evidence
}

type continuedRun struct {
	res      replayResult
	evidence *BenchmarkExecutionEvidence
}

func runContinuedSearch(t *testing.T, stateDir string, mutate func(*BenchmarkRecord, []validatedCandidate), fingerprint string) continuedRun {
	t.Helper()
	record := replayBenchmarkRecord(replayRules(), nil)
	ladder := continuationX265Ladder(hardwareFallbackLadder())
	record.ID, record.Source, record.SourceSHA256, record.Status = "bench-prior-x265", "/media/show.mkv", strings.Repeat("a", 64), "running"
	record.Candidates = recordCandidates(ladder)
	record.ContinuesBenchmark = &transcode.BenchmarkContinuation{ID: continuationPriorID, RequestDigest: continuationPriorDigest}
	if mutate != nil {
		mutate(record, ladder)
	}
	var run continuedRun
	runner := &ProductionBenchmarkRunner{
		automaticHook:         replayAutomaticHook(t, continuationHardEpisode(), &run.res),
		capabilityFingerprint: func(context.Context, *Worker) (string, error) { return fingerprint, nil },
	}
	w := &Worker{cfg: &WorkerConfig{StateDir: stateDir}}
	run.evidence = &BenchmarkExecutionEvidence{SourceBitDepth: 8}
	rep := mediainspect.DetailedReport{DurationSec: 600, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 25}}}
	if err := runner.runAutomaticCandidates(context.Background(), w, record, run.evidence, "", ladder, rep, 10_000_000, time.Time{}, newBenchmarkProgressReporter(nil, record, 100)); err != nil {
		t.Fatal(err)
	}
	run.res.decision = run.evidence.Decision
	run.res.stages = run.evidence.Automatic.Stages
	if run.evidence.Decision.Winner != nil {
		run.res.winner = run.evidence.Decision.Winner.CandidateID
	}
	return run
}

func reasons(res replayResult) map[string]string {
	out := map[string]string{}
	for _, ev := range res.decision.Evaluations {
		out[ev.CandidateID] = ev.EvaluationReason
	}
	return out
}

func reusedIDs(run continuedRun) []string {
	var ids []string
	for _, stage := range run.evidence.Automatic.Stages {
		ids = append(ids, stage.Reused...)
	}
	sort.Strings(ids)
	return ids
}

// A continued search measures only what the earlier search did not, and ends
// where a search measuring everything would have.
func TestContinuedSearchReusesMeasuredX265AndEndsWhereAFreshSearchWould(t *testing.T) {
	dir := t.TempDir()
	priorSearch(t, dir)

	fresh := runContinuedSearch(t, dir, func(r *BenchmarkRecord, _ []validatedCandidate) { r.ContinuesBenchmark = nil }, continuationFingerprint)
	continued := runContinuedSearch(t, dir, nil, continuationFingerprint)
	t.Logf("fresh: %s measured=%v", fresh.res.summary(), fresh.res.measured)
	t.Logf("continued: %s measured=%v reused=%v", continued.res.summary(), continued.res.measured, reusedIDs(continued))

	if len(reusedIDs(fresh)) != 0 {
		t.Fatalf("a search without a continuation reused %v", reusedIDs(fresh))
	}
	if strings.Contains(strings.Join(fresh.res.measured, ","), "vt-") || fresh.res.winner == "" {
		t.Fatalf("the reference x265 search: %s %v", fresh.res.summary(), fresh.res.measured)
	}
	wantReused := []string{"cand_crf22", "fallback-quality-medium-cand_crf16"}
	if !reflect.DeepEqual(reusedIDs(continued), wantReused) {
		t.Fatalf("reused %v, want %v", reusedIDs(continued), wantReused)
	}
	for _, id := range wantReused {
		if measuredHas(continued.res, id) {
			t.Fatalf("%s was reused and measured again", id)
		}
		if !measuredHas(fresh.res, id) {
			t.Fatalf("%s was not measured by the reference search", id)
		}
	}
	if len(continued.res.measured) >= len(fresh.res.measured) {
		t.Fatalf("the continuation did not save any measurement: %v against %v", continued.res.measured, fresh.res.measured)
	}
	if continued.res.winner != fresh.res.winner || !reflect.DeepEqual(reasons(continued.res), reasons(fresh.res)) {
		t.Fatalf("reuse changed the outcome:\ncontinued %s %v\nfresh     %s %v", continued.res.winner, reasons(continued.res), fresh.res.winner, reasons(fresh.res))
	}
	if continued.res.slowEncodes == 0 || !strings.HasPrefix(continued.res.winner, "fallback-quality-cand_") {
		t.Fatalf("the unmeasured slow family did not run to find the winner: %s", continued.res.summary())
	}
	// The reused candidates sit under their index in this search.
	index := map[string]int{}
	for _, vc := range continuationX265Ladder(hardwareFallbackLadder()) {
		index[vc.candidate.ID] = vc.index
	}
	for _, m := range continued.evidence.CandidateMetrics {
		if m.CandidateIndex != index[m.CandidateID] {
			t.Fatalf("%s carries index %d, want %d", m.CandidateID, m.CandidateIndex, index[m.CandidateID])
		}
	}
	if len(continued.evidence.Quality) != len(fresh.evidence.Quality) || len(continued.evidence.CandidateSamples) != len(fresh.evidence.CandidateSamples) {
		t.Fatalf("evidence rows differ: quality %d/%d samples %d/%d", len(continued.evidence.Quality), len(fresh.evidence.Quality), len(continued.evidence.CandidateSamples), len(fresh.evidence.CandidateSamples))
	}
}

// Reuse is an optimisation, never an error: anything that does not match
// exactly measures again and the search still ends where a fresh one would.
func TestContinuedSearchMeasuresAgainWhenTheEarlierSearchDoesNotMatch(t *testing.T) {
	dir := t.TempDir()
	priorSearch(t, dir)
	reference := runContinuedSearch(t, dir, func(r *BenchmarkRecord, _ []validatedCandidate) { r.ContinuesBenchmark = nil }, continuationFingerprint)
	for name, tc := range map[string]struct {
		mutate      func(*BenchmarkRecord, []validatedCandidate)
		fingerprint string
	}{
		"different source":     {mutate: func(r *BenchmarkRecord, _ []validatedCandidate) { r.SourceSHA256 = strings.Repeat("b", 64) }, fingerprint: continuationFingerprint},
		"no source hash":       {mutate: func(r *BenchmarkRecord, _ []validatedCandidate) { r.SourceSHA256 = "" }, fingerprint: continuationFingerprint},
		"different thresholds": {mutate: func(r *BenchmarkRecord, _ []validatedCandidate) { *r.Quality.VMAF.P5Minimum = 83 }, fingerprint: continuationFingerprint},
		"different windows":    {mutate: func(r *BenchmarkRecord, _ []validatedCandidate) { r.Samples[1].StartSeconds += 1 }, fingerprint: continuationFingerprint},
		"different rules":      {mutate: func(r *BenchmarkRecord, _ []validatedCandidate) { r.AutomaticSearch.SelectionMarginVMAF = 2 }, fingerprint: continuationFingerprint},
		"different digest": {mutate: func(r *BenchmarkRecord, _ []validatedCandidate) {
			r.ContinuesBenchmark.RequestDigest = "sha256:" + strings.Repeat("9", 64)
		}, fingerprint: continuationFingerprint},
		"unknown earlier search": {mutate: func(r *BenchmarkRecord, _ []validatedCandidate) { r.ContinuesBenchmark.ID = "bench-other" }, fingerprint: continuationFingerprint},
		"different capabilities": {fingerprint: "fp-other"},
		"capabilities unknown":   {fingerprint: ""},
		"candidate settings changed": {mutate: func(r *BenchmarkRecord, vcs []validatedCandidate) {
			for i := range r.Candidates {
				if r.Candidates[i].ID == "cand_crf22" {
					r.Candidates[i].Tune = "grain"
				}
			}
			for i := range vcs {
				if vcs[i].candidate.ID == "cand_crf22" {
					vcs[i].candidate.Tune = "grain"
				}
			}
		}, fingerprint: continuationFingerprint},
	} {
		t.Run(name, func(t *testing.T) {
			run := runContinuedSearch(t, dir, tc.mutate, tc.fingerprint)
			if name == "candidate settings changed" {
				// Only the changed candidate is measured again.
				if !measuredHas(run.res, "cand_crf22") || measuredHas(run.res, "fallback-quality-medium-cand_crf16") {
					t.Fatalf("measured %v", run.res.measured)
				}
				return
			}
			if len(reusedIDs(run)) != 0 || !reflect.DeepEqual(run.res.measured, reference.res.measured) {
				t.Fatalf("reused %v, measured %v; a fresh search measures %v", reusedIDs(run), run.res.measured, reference.res.measured)
			}
			if run.res.winner != reference.res.winner {
				t.Fatalf("winner %s, want %s", run.res.winner, reference.res.winner)
			}
		})
	}
}

// A candidate the earlier search verified on the holdout windows is measured
// again, never reused from rows that were merged with those windows.
func TestContinuedSearchNeverReusesAHoldoutVerifiedCandidate(t *testing.T) {
	dir := t.TempDir()
	prior, evidence := priorSearch(t, dir)
	// The earlier search verified a medium setting on the holdout windows too.
	for i := range prior.Evidence.CandidateSamples {
		if prior.Evidence.CandidateSamples[i].CandidateID == "cand_crf22" {
			row := prior.Evidence.CandidateSamples[i]
			row.SampleIndex = 3
			evidence.CandidateSamples = append(evidence.CandidateSamples, row)
			break
		}
	}
	if err := SaveBenchmarkAtomic(filepath.Join(dir, continuationPriorID, "benchmark.json"), prior); err != nil {
		t.Fatal(err)
	}
	run := runContinuedSearch(t, dir, nil, continuationFingerprint)
	for _, id := range reusedIDs(run) {
		if id == "cand_crf22" {
			t.Fatalf("reused a candidate with holdout rows: %v", reusedIDs(run))
		}
	}
	if !measuredHas(run.res, "cand_crf22") {
		t.Fatalf("cand_crf22 was neither reused nor measured: %v", run.res.measured)
	}
}
