package action

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

// The outcome library keeps every finished size-mode search so the same
// source is not searched again for nothing. A search of the same source bytes
// under the same frozen policy is reported; when its winner later passed final
// validation, its settings can stand in for the search. Final validation of
// the new full encode always runs, and promotion keeps its approval.

const (
	outcomeLibraryKey = "outcome_library"
	// libraryReuseKey holds the recorded winner a search was skipped for. No
	// worker job exists for that benchmark identity.
	libraryReuseKey = "library_reuse"
	// reuseLibraryOutcomeInput set to false always searches.
	reuseLibraryOutcomeInput = "reuse_library_outcome"
	// outcomeLibraryLookback bounds how many exact records a report reads.
	outcomeLibraryLookback = 10
)

// outcomeWinnerRecord is what a winner row keeps: the measured candidate as the
// request sent it, the worker's winner and its evaluation.
type outcomeWinnerRecord struct {
	Candidate  transcode.BenchmarkCandidate            `json:"candidate"`
	Winner     transcode.BenchmarkWinner               `json:"winner"`
	Evaluation *transcode.BenchmarkCandidateEvaluation `json:"evaluation,omitempty"`
}

// outcomePoint is one measured candidate of a search.
type outcomePoint struct {
	CandidateID        string   `json:"candidate_id"`
	VideoCodec         string   `json:"video_codec,omitempty"`
	Preset             string   `json:"preset,omitempty"`
	Quality            int      `json:"quality"`
	AverageBitrateKbps int      `json:"average_bitrate_kbps,omitempty"`
	PixelFormat        string   `json:"pixel_format,omitempty"`
	EstimatedBytes     int64    `json:"estimated_bytes,omitempty"`
	SavingsPercent     float64  `json:"savings_percent"`
	Eligible           bool     `json:"eligible"`
	Reason             string   `json:"reason,omitempty"`
	VMAFMean           *float64 `json:"vmaf_mean,omitempty"`
	VMAFP5             *float64 `json:"vmaf_p5,omitempty"`
	WorstWindowMean    *float64 `json:"worst_window_mean,omitempty"`
	CAMBIMean          *float64 `json:"cambi_mean,omitempty"`
	CAMBIMax           *float64 `json:"cambi_max,omitempty"`
	// Verification is the outcome of this candidate's verification attempt,
	// when it had one.
	Verification string `json:"verification,omitempty"`
}

// outcomeFinal is the final validation of the full encode.
type outcomeFinal struct {
	Verdict            string   `json:"verdict"`
	ReasonCodes        []string `json:"reason_codes,omitempty"`
	CandidateSizeBytes int64    `json:"candidate_size_bytes"`
	VMAFMean           *float64 `json:"vmaf_mean,omitempty"`
	VMAFP5             *float64 `json:"vmaf_p5,omitempty"`
	WorstWindowMean    *float64 `json:"worst_window_mean,omitempty"`
	CAMBIMean          *float64 `json:"cambi_mean,omitempty"`
	CAMBIMax           *float64 `json:"cambi_max,omitempty"`
	Windows            int      `json:"windows"`
}

// outcomeLibraryReport is shown in status: what the library knows about these
// exact source bytes under this policy, and whether the search was skipped.
type outcomeLibraryReport struct {
	PolicyVersion string                `json:"policy_version"`
	PolicyDigest  string                `json:"policy_digest"`
	Matches       []outcomeLibraryMatch `json:"matches"`
	Note          string                `json:"note,omitempty"`
	// Reused names the action whose validated winner replaced this search.
	Reused string `json:"reused_from,omitempty"`
	// NotReused says why a recorded winner was not used.
	NotReused string `json:"not_reused,omitempty"`
}

type outcomeLibraryMatch struct {
	ActionID     string `json:"action_id"`
	Outcome      string `json:"outcome"`
	FinalVerdict string `json:"final_verdict,omitempty"`
	VideoCodec   string `json:"video_codec,omitempty"`
	Preset       string `json:"preset,omitempty"`
	Quality      int    `json:"quality,omitempty"`
	RecordedAt   string `json:"recorded_at"`
}

// libraryReuse is the state of a search skipped for a recorded winner.
type libraryReuse struct {
	FromActionID       string                                  `json:"from_action_id"`
	FromBenchmarkJobID string                                  `json:"from_benchmark_job_id"`
	CandidateID        string                                  `json:"candidate_id"`
	Winner             transcode.BenchmarkWinner               `json:"winner"`
	Evaluation         *transcode.BenchmarkCandidateEvaluation `json:"evaluation,omitempty"`
}

func libraryReuseOf(state map[string]any) *libraryReuse {
	raw, ok := state[libraryReuseKey]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var r libraryReuse
	if json.Unmarshal(b, &r) != nil || r.CandidateID == "" {
		return nil
	}
	return &r
}

// completedStatus is the benchmark result a skipped search stands on: the
// recorded winner, renamed to the matching candidate of this request.
func (r *libraryReuse) completedStatus(jobID string) transcode.BenchmarkStatus {
	winner := r.Winner
	winner.CandidateID = r.CandidateID
	decision := &transcode.BenchmarkDecision{Winner: &winner, DecisionReason: fmt.Sprintf("Reused the validated result of an earlier search of these exact source bytes under the same policy (action %s); final validation still measures the new encode.", r.FromActionID)}
	if r.Evaluation != nil {
		ev := *r.Evaluation
		ev.CandidateID, ev.CandidateIndex = r.CandidateID, winner.CandidateIndex
		decision.Evaluations = []transcode.BenchmarkCandidateEvaluation{ev}
	}
	return transcode.BenchmarkStatus{ID: jobID, Status: transcode.StatusCompleted, Decision: decision}
}

// outcomeLibraryApplies limits the library to size-mode transcodes.
func (e *Engine) outcomeLibraryApplies(ec *ExecutionContext) bool {
	return e.deps.Store != nil && ec.ActionName == "transcode_media" && getString(ec.Inputs, "mode") == "size" &&
		getString(ec.State, "policy_digest") != "" && getString(ec.State, "original_sha256") != ""
}

// recordSearchOutcome stores a finished search. It never fails the action;
// errors are logged without the source path.
func (e *Engine) recordSearchOutcome(ec *ExecutionContext, benchJobID, outcome string, st *transcode.BenchmarkStatus) {
	if !e.outcomeLibraryApplies(ec) || benchJobID == "" {
		return
	}
	var request transcode.BenchmarkRequest
	if raw, err := json.Marshal(ec.State["benchmark_request"]); err == nil {
		_ = json.Unmarshal(raw, &request)
	}
	row := e.outcomeRow(ec, benchJobID, outcome)
	if reuse := libraryReuseOf(ec.State); reuse != nil {
		row.ReusedFrom = reuse.FromActionID
	}
	if st != nil && st.Decision != nil {
		if w := st.Decision.Winner; w != nil && outcome == store.OutcomeWinner {
			rec := outcomeWinnerRecord{Winner: *w}
			for _, c := range request.Candidates {
				if c.ID == w.CandidateID {
					rec.Candidate = c
				}
			}
			for i := range st.Decision.Evaluations {
				if st.Decision.Evaluations[i].CandidateID == w.CandidateID {
					ev := st.Decision.Evaluations[i]
					rec.Evaluation = &ev
				}
			}
			if b, err := json.Marshal(rec); err == nil {
				row.WinnerJSON = string(b)
			}
		}
		if b, err := json.Marshal(outcomePoints(st)); err == nil {
			row.PointsJSON = string(b)
		}
	}
	if err := e.deps.Store.RecordTranscodeOutcome(row); err != nil {
		log.Printf("outcome library: recording %s for action %s: %v", outcome, ec.InstanceID, err)
	}
}

func (e *Engine) outcomeRow(ec *ExecutionContext, benchJobID, outcome string) store.TranscodeOutcome {
	identity := e.transcodeLibraryIdentity(ec)
	row := store.TranscodeOutcome{
		ActionID: ec.InstanceID, BenchmarkJobID: benchJobID,
		SourceSHA256: getString(ec.State, "original_sha256"), SourceSize: getInt64(ec.State, "original_size"),
		LibraryService: identity.Service, LibraryID: identity.ID, Season: identity.Season, Episode: identity.Episode, FolderKey: identity.FolderKey,
		Mode: getString(ec.Inputs, "mode"), PolicyDigest: getString(ec.State, "policy_digest"), Outcome: outcome,
	}
	if snapshot, err := freezeModePolicy(ec); err == nil {
		row.PolicyVersion = snapshot.Version
	}
	if rep := getSourceReport(ec.State["original"]); rep != nil {
		row.DurationSec = rep.DurationSec
		if v := rep.MainVideo(); len(v) > 0 {
			row.VideoCodec, row.BitDepth, row.Width, row.Height, row.FPS = v[0].Codec, v[0].BitDepth, v[0].Width, v[0].Height, v[0].FPS
		}
	}
	return row
}

// outcomePoints lists every measured candidate with its VMAF, banding and
// verification result.
func outcomePoints(st *transcode.BenchmarkStatus) []outcomePoint {
	verification := map[string]string{}
	if st.Automatic != nil && st.Automatic.Verification != nil {
		for _, a := range st.Automatic.Verification.Attempts {
			verification[a.CandidateID] = a.Outcome
		}
	}
	cambi := map[string][2]float64{}
	for _, q := range st.Quality {
		if q.CAMBI != nil {
			cambi[q.CandidateID] = [2]float64{q.CAMBI.Mean, q.CAMBI.Max}
		}
	}
	points := []outcomePoint{}
	for _, ev := range st.Decision.Evaluations {
		p := outcomePoint{CandidateID: ev.CandidateID, VideoCodec: ev.VideoCodec, Preset: ev.Preset, Quality: ev.Quality, AverageBitrateKbps: ev.AverageBitrateKbps,
			PixelFormat: ev.PixelFormat, EstimatedBytes: ev.EstimatedBytes, SavingsPercent: ev.SavingsPercent, Eligible: ev.Eligible, Reason: ev.EvaluationReason,
			Verification: verification[ev.CandidateID]}
		if ev.VMAF != nil {
			p.VMAFMean, p.VMAFP5 = floatPtr(ev.VMAF.Mean), floatPtr(ev.VMAF.P5)
			if ev.VMAF.WorstWindowMean > 0 {
				p.WorstWindowMean = floatPtr(ev.VMAF.WorstWindowMean)
			}
		}
		if c, ok := cambi[ev.CandidateID]; ok {
			p.CAMBIMean, p.CAMBIMax = floatPtr(c[0]), floatPtr(c[1])
		}
		points = append(points, p)
	}
	return points
}

func floatPtr(v float64) *float64 { return &v }

// recordOutcomeFinal attaches final validation to the search whose winner was
// encoded. Only a benchmark winner's encode is judged against its search. A
// worker's fail is recorded when the encode ends; a pass only once the
// coordinator has matched the evidence to the candidate and plan and the size
// is within the policy (see recordAcceptedOutcomeFinal), so a pass the
// coordinator refuses is never reused.
func (e *Engine) recordOutcomeFinal(ec *ExecutionContext, ev *transcode.FinalQualityEvidence) {
	if ev != nil && strings.EqualFold(strings.TrimSpace(ev.Verdict), "fail") {
		e.writeOutcomeFinal(ec, ev, "fail", nil)
	}
}

// recordAcceptedOutcomeFinal records the final verdict at validation: pass
// when the size is within the policy, fail (insufficient_savings) otherwise.
func (e *Engine) recordAcceptedOutcomeFinal(ec *ExecutionContext, sizeAllowed bool) {
	var ev transcode.FinalQualityEvidence
	b, err := json.Marshal(ec.State["quality_evidence"])
	if err != nil || json.Unmarshal(b, &ev) != nil || !strings.EqualFold(ev.Verdict, "pass") {
		return
	}
	if sizeAllowed {
		e.writeOutcomeFinal(ec, &ev, "pass", nil)
	} else {
		e.writeOutcomeFinal(ec, &ev, "fail", []string{"insufficient_savings"})
	}
}

func (e *Engine) writeOutcomeFinal(ec *ExecutionContext, ev *transcode.FinalQualityEvidence, verdict string, extraReasons []string) {
	if ev == nil || !e.outcomeLibraryApplies(ec) || getString(ec.State, "plan_source") != "benchmark_winner" {
		return
	}
	final := outcomeFinal{Verdict: verdict, ReasonCodes: append(append([]string(nil), ev.ReasonCodes...), extraReasons...), CandidateSizeBytes: ev.CandidateSizeBytes, Windows: len(ev.Windows)}
	if ev.VMAF != nil {
		final.VMAFMean, final.VMAFP5 = floatPtr(ev.VMAF.Mean), floatPtr(ev.VMAF.P5)
		if ev.VMAF.WorstWindowMean > 0 {
			final.WorstWindowMean = floatPtr(ev.VMAF.WorstWindowMean)
		}
	}
	if ev.CAMBI != nil {
		final.CAMBIMean, final.CAMBIMax = floatPtr(ev.CAMBI.Mean), floatPtr(ev.CAMBI.Max)
	}
	b, err := json.Marshal(final)
	if err != nil {
		return
	}
	if _, err := e.deps.Store.RecordTranscodeOutcomeFinal(ec.InstanceID, getString(ec.State, "benchmark_job_id"), verdict, string(b)); err != nil {
		log.Printf("outcome library: recording final validation for action %s: %v", ec.InstanceID, err)
	}
}

// reportOutcomeLibrary reads the exact records for this source and policy into
// status. It returns the records, newest first.
func (e *Engine) reportOutcomeLibrary(ec *ExecutionContext) []store.TranscodeOutcome {
	delete(ec.State, outcomeLibraryKey)
	delete(ec.Outputs, outcomeLibraryKey)
	if !e.outcomeLibraryApplies(ec) {
		return nil
	}
	rows, err := e.deps.Store.TranscodeOutcomesForSource(getString(ec.State, "original_sha256"), getString(ec.State, "policy_digest"), ec.InstanceID, outcomeLibraryLookback)
	if err != nil {
		log.Printf("outcome library: reading records for action %s: %v", ec.InstanceID, err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	report := outcomeLibraryReport{PolicyVersion: rows[0].PolicyVersion, PolicyDigest: rows[0].PolicyDigest, Matches: []outcomeLibraryMatch{}}
	for _, row := range rows {
		m := outcomeLibraryMatch{ActionID: row.ActionID, Outcome: row.Outcome, FinalVerdict: row.FinalVerdict, RecordedAt: row.CreatedAt}
		if rec := winnerRecordOf(row); rec != nil {
			m.VideoCodec, m.Preset, m.Quality = rec.Winner.VideoCodec, rec.Winner.Preset, rec.Winner.Quality
		}
		report.Matches = append(report.Matches, m)
	}
	report.Note = outcomeLibraryNote(rows[0])
	setOutcomeLibraryReport(ec, &report)
	return rows
}

func setOutcomeLibraryReport(ec *ExecutionContext, report *outcomeLibraryReport) {
	ec.State[outcomeLibraryKey] = report
	ec.Outputs[outcomeLibraryKey] = report
}

func outcomeLibraryNote(latest store.TranscodeOutcome) string {
	policy := latest.PolicyVersion
	if policy == "" {
		policy = "the same policy"
	}
	digest := latest.PolicyDigest
	if len(digest) > 19 {
		digest = digest[:19]
	}
	switch {
	case latest.Outcome == store.OutcomeNoWinner:
		return fmt.Sprintf("This exact source already failed with policy %s (%s) in action %s: no setting met the quality and size limits.", policy, digest, latest.ActionID)
	case latest.Outcome == store.OutcomeBudgetExhausted:
		return fmt.Sprintf("This exact source already ran out of search time with policy %s (%s) in action %s.", policy, digest, latest.ActionID)
	case latest.FinalVerdict == "fail":
		return fmt.Sprintf("This exact source already found a setting with policy %s (%s) in action %s, but its full encode failed final validation.", policy, digest, latest.ActionID)
	case latest.FinalVerdict == "pass":
		return fmt.Sprintf("This exact source already passed final validation with policy %s (%s) in action %s.", policy, digest, latest.ActionID)
	}
	return fmt.Sprintf("This exact source already found a setting with policy %s (%s) in action %s; its full encode was not validated.", policy, digest, latest.ActionID)
}

func winnerRecordOf(row store.TranscodeOutcome) *outcomeWinnerRecord {
	if row.Outcome != store.OutcomeWinner || row.WinnerJSON == "" {
		return nil
	}
	var rec outcomeWinnerRecord
	if json.Unmarshal([]byte(row.WinnerJSON), &rec) != nil || rec.Winner.CandidateID == "" {
		return nil
	}
	return &rec
}

// libraryReuseFor decides whether the newest exact record can stand in for
// this search: a libx265 winner that passed final validation within the size
// limit, whose measured settings this request proposes again. It returns why
// not otherwise ("" when nothing was recorded or reuse is off).
func (e *Engine) libraryReuseFor(ec *ExecutionContext, req *transcode.BenchmarkRequest, rows []store.TranscodeOutcome) (*libraryReuse, string) {
	if len(rows) == 0 || req == nil {
		return nil, ""
	}
	if !libraryUseAllowed(ec) {
		return nil, "reuse is turned off for this action"
	}
	if getBool(ec.State, "benchmark_search_reviewed") || getBool(ec.State, "benchmark_search_review_required") || continuationActive(ec.State) {
		return nil, "this search follows a review or a failed hardware result"
	}
	snapshot, err := freezeModePolicy(ec)
	if err != nil || snapshot.Version != optimization.ModePolicyV3 {
		return nil, "only size policy " + optimization.ModePolicyV3 + " reuses results"
	}
	var latest *store.TranscodeOutcome
	for i := range rows {
		if rows[i].FinalVerdict != "" || rows[i].Outcome != store.OutcomeWinner {
			latest = &rows[i]
			break
		}
	}
	if latest == nil {
		return nil, "no recorded winner has been validated on a full encode yet"
	}
	if latest.Outcome != store.OutcomeWinner || latest.FinalVerdict != "pass" {
		return nil, "the most recent conclusive record is not a validated winner"
	}
	rec := winnerRecordOf(*latest)
	if rec == nil {
		return nil, "the recorded winner is unreadable"
	}
	if transcode.NormalizeVideoCodec(rec.Winner.VideoCodec) != transcode.VideoCodecLibX265 {
		return nil, "hardware results are reported, not reused"
	}
	var final outcomeFinal
	if json.Unmarshal([]byte(latest.FinalJSON), &final) != nil || final.CandidateSizeBytes <= 0 ||
		!optimization.ModeSizeAllowedVersion(snapshot.Mode, getInt64(ec.State, "original_size"), final.CandidateSizeBytes, snapshot.Version) {
		return nil, "the recorded encode's size is not within this policy's limit"
	}
	want := candidateIdentity(rec.Candidate)
	for i, c := range req.Candidates {
		if candidateIdentity(c) == want {
			winner := rec.Winner
			winner.CandidateIndex = i
			return &libraryReuse{FromActionID: latest.ActionID, FromBenchmarkJobID: latest.BenchmarkJobID, CandidateID: c.ID, Winner: winner, Evaluation: rec.Evaluation}, ""
		}
	}
	return nil, "this search no longer proposes the recorded setting"
}

// applyLibraryReuse skips the search when a recorded winner stands in for it:
// the request is kept and digested as if submitted, and the wait step judges
// the recorded winner exactly as a worker's, so the winning plan and its final
// validation are the same as after a search.
func (e *Engine) applyLibraryReuse(ctx context.Context, ec *ExecutionContext, req *transcode.BenchmarkRequest, digest string, recorded []store.TranscodeOutcome) (StepResult, bool, error) {
	delete(ec.State, libraryReuseKey)
	reuse, why := e.libraryReuseFor(ec, req, recorded)
	if report, ok := ec.State[outcomeLibraryKey].(*outcomeLibraryReport); ok {
		if reuse != nil {
			report.Reused = reuse.FromActionID
		} else {
			report.NotReused = why
		}
	}
	if reuse == nil {
		return StepResult{}, false, nil
	}
	ec.State[libraryReuseKey] = reuse
	// No search ran: a batch's search budget settles at what earlier rounds
	// of this action measured (none, since reuse never follows a continuation).
	ec.State["benchmark_search_seconds"] = getFloat(ec.State, continuationPriorSecondsKey)
	ec.State["benchmark_job_id"] = req.ID
	ec.State["benchmark_request"] = req
	ec.State["benchmark_request_digest"] = digest
	ec.State["benchmark_submitted"] = true
	delete(ec.State, "benchmark_reconcile")
	delete(ec.State, benchmarkRejectedKey)
	clearWorkerBusy(ec)
	if err := e.persistExecutionState(ctx, ec); err != nil {
		return StepResult{}, true, err
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{
		"benchmark_job_id":         req.ID,
		"benchmark_request_digest": digest,
		libraryReuseKey:            reuse,
		outcomeLibraryKey:          ec.State[outcomeLibraryKey],
	}}, true, nil
}

// libraryUseAllowed reads reuse_library_outcome: absent or true (a JSON
// boolean, or the string "true") allows the library's results to be used;
// anything else turns them off.
func libraryUseAllowed(ec *ExecutionContext) bool {
	v, ok := ec.Inputs[reuseLibraryOutcomeInput]
	if !ok || v == nil {
		return true
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		return err == nil && b
	}
	return false
}
