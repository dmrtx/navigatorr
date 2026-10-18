package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

type benchmarkSummary struct {
	Selection *optimization.SelectionExplanation `json:"selection,omitempty"`
	Outcome   string                             `json:"outcome"`
	Message   string                             `json:"message"`
	NextStep  string                             `json:"next_step"`
	// UserNextStep says the same for a person in the UI; NextStep keeps the
	// tool-level guidance for agents.
	UserNextStep       string                            `json:"user_next_step,omitempty"`
	ProgressScope      string                            `json:"progress_scope"`
	PreviousRounds     []string                          `json:"previous_unsuccessful_actions,omitempty"`
	PreviousResults    []action.SearchReviewRound        `json:"previous_rounds,omitempty"`
	NewCandidates      []string                          `json:"new_candidates,omitempty"`
	Measured           []benchmarkMeasured               `json:"measured,omitempty"`
	ProposedCandidates []transcode.BenchmarkCandidate    `json:"proposed_candidates,omitempty"`
	ProposedQuality    *transcode.BenchmarkQualityConfig `json:"proposed_quality,omitempty"`
	ProposedSamples    []transcode.BenchmarkSampleWindow `json:"proposed_samples,omitempty"`
	SizeWarning        string                            `json:"size_warning,omitempty"`
	Rejected           []benchmarkRejection              `json:"rejected,omitempty"`
	Audio              []benchmarkAudio                  `json:"planned_audio,omitempty"`
	AudioNote          string                            `json:"audio_note,omitempty"`
	PlanSource         string                            `json:"plan_source,omitempty"`
	PlanNote           string                            `json:"plan_note,omitempty"`
}

type benchmarkRejection struct {
	FailedSamples       []transcode.BenchmarkSampleFailure `json:"failed_samples,omitempty"`
	CandidateID         string                             `json:"candidate_id"`
	Checks              []benchmarkCheck                   `json:"checks"`
	ReviewSourceSeconds *float64                           `json:"review_source_seconds,omitempty"`
}

// benchmarkMeasured is one evaluated candidate's measured quality.
type benchmarkMeasured struct {
	CandidateID        string                          `json:"candidate_id"`
	VideoCodec         string                          `json:"video_codec,omitempty"`
	Quality            int                             `json:"quality"`
	Preset             string                          `json:"preset,omitempty"`
	AverageBitrateKbps int                             `json:"average_bitrate_kbps,omitempty"`
	Passed             bool                            `json:"passed"`
	Selected           bool                            `json:"selected,omitempty"`
	VMAF               *transcode.BenchmarkVMAFSummary `json:"vmaf"`
}

type benchmarkCheck struct {
	Code   string   `json:"code"`
	Reason string   `json:"reason"`
	Actual *float64 `json:"actual,omitempty"`
	Limit  *float64 `json:"limit,omitempty"`
}

type benchmarkAudio struct {
	Stream      int    `json:"stream"`
	Language    string `json:"language,omitempty"`
	Channels    int    `json:"channels,omitempty"`
	SourceCodec string `json:"source_codec"`
	Operation   string `json:"operation"`
	OutputCodec string `json:"output_codec"`
	TargetKbps  int    `json:"target_kbps,omitempty"`
}

// Read a known projection directly: the generic operational map drops values
// above 1 KiB, including the very evidence needed to explain most rejections.
func benchmarkField(res *action.ActionResult, key string, dst any) bool {
	v, ok := res.Outputs[key]
	if !ok {
		v, ok = res.State[key]
	}
	if !ok || v == nil {
		return false
	}
	b, err := json.Marshal(v)
	return err == nil && json.Unmarshal(b, dst) == nil
}

func summaryPlanSource(res *action.ActionResult) string {
	var source string
	benchmarkField(res, "plan_source", &source)
	return source
}

func compactBenchmark(res *action.ActionResult) *benchmarkSummary {
	if res.ActionName != "benchmark_transcode" && res.ActionName != "transcode_media" {
		return nil
	}
	var enabled, review bool
	var status string
	var decision *transcode.BenchmarkDecision
	benchmarkField(res, "optimization_enabled", &enabled)
	benchmarkField(res, "benchmark_status", &status)
	benchmarkField(res, "benchmark_decision", &decision)
	benchmarkField(res, "benchmark_search_review_required", &review)
	if !enabled && status == "" && decision == nil && !review {
		return nil
	}
	s := &benchmarkSummary{ProgressScope: "Worker progress is for the benchmark only, not the full encode.", Outcome: "testing", Message: "Testing short video samples; no quality decision yet.", NextStep: "Monitor this action with action_status. A progress percentage does not mean quality passed; do not launch another search.", UserNextStep: "Nothing to do yet. Short samples are being tested; progress does not mean the quality check passed."}
	switch {
	case res.Status == action.StatusCancelled:
		s.Outcome, s.Message, s.NextStep = "cancelled", "Search cancelled.", "Keep the original; inspect action details before starting other work."
		s.UserNextStep = "Nothing else to do: the original is kept."
	case review:
		s.Outcome, s.Message = "search_review_required", "Two earlier rounds found no acceptable candidate; this new benchmark has not been submitted."
		s.NextStep = "Review the previous actions and this proposed recipe. Stop, choose direct encoding, or use the displayed run_once_after_review decision only on explicit user instruction. Do not automatically change CRF or thresholds and start another action."
		s.UserNextStep = "Keep the original, or choose “Run one more test” to try the new settings listed below with the same quality requirement."
		benchmarkField(res, "previous_unsuccessful_benchmarks", &s.PreviousRounds)
		var req transcode.BenchmarkRequest
		if benchmarkField(res, "benchmark_request", &req) {
			s.ProposedCandidates = req.Candidates
			s.ProposedQuality, s.ProposedSamples = req.Quality, req.Samples
		}
		var history action.SearchReview
		if benchmarkField(res, "benchmark_search_review", &history) {
			s.PreviousResults, s.NewCandidates = history.Rounds, history.NewCandidates
			if len(history.NewCandidates) == 0 {
				s.NextStep = "Every proposed candidate was already measured by a comparable earlier round, so run_once_after_review is not offered. Cancel to keep the original, or explicitly choose another goal or direct encoding; do not start a new action with the same ladder."
				s.UserNextStep = "Keep the original. Every setting this test would try already failed in earlier rounds, so testing them again is not offered. To process this file anyway, choose a different goal."
			} else {
				s.NextStep = "Review previous_rounds and this proposal. run_once_after_review tests only on explicit user instruction; new_candidates lists the settings no comparable round measured. Do not automatically change CRF or thresholds and start another action."
			}
		}
	case res.Status == action.StatusWaitingDecision && decision == nil:
		// A paused workflow is not testing anything. Mode attention stops
		// before or between searches and waits only for the user.
		var reason string
		benchmarkField(res, "reason_code", &reason)
		s.Outcome, s.Message = "attention", res.WaitingReason
		if s.Message == "" {
			s.Message = "Waiting for your decision; no quality result was recorded."
		}
		s.NextStep = "Review the waiting reason and choose one of the displayed decisions. Do not start another search automatically."
		s.UserNextStep = "Choose one of the options shown. Nothing runs until you decide."
		if reason == "quality_policy_unavailable" {
			s.Outcome, s.Message = reason, "No sample test was started: the worker cannot run the quality checks this goal requires. The original is preserved."
			s.NextStep = "Cancel to keep the original. To process this file, make the missing quality check available on the worker (the waiting reason names it) and start the goal again; quality requirements are never relaxed automatically."
			s.UserNextStep = "Keep the original. The worker cannot run a quality check this goal needs; once it can, start the goal again."
		}
	case decision != nil && decision.Winner == nil && summaryPlanSource(res) == "audio_only":
		s.Outcome, s.Message = "audio_only", "No video encode met this goal's quality floor and savings gate, so the video is copied unchanged and only the lossless audio is compacted to AAC."
		s.NextStep = "Monitor this action. The candidate must prove its video bit-identical to the source and still meet the same minimum savings; the original is never changed before that."
		s.UserNextStep = "Nothing to do yet. The new file keeps the video exactly as it is and only makes the audio smaller; the original is not changed before that is checked."
	case decision != nil && decision.Winner == nil:
		s.Outcome, s.Message = "no_acceptable_candidate", "The benchmark finished, but no candidate met all configured checks. Full encoding did not start."
		s.NextStep = "Inspect the rejected checks and indicated source fragment before choosing a next step. Keep the original, or explicitly choose direct encoding via a new transcode_media action with a non-optimizing profile/profile_config (optimization.enabled=false). Direct encoding does not claim a perceptual quality pass and never replaces the original. Do not automatically start another CRF ladder or relax thresholds."
		s.UserNextStep = "Keep the original. If you still want a smaller file, choose a different goal, or encode it directly without a quality check."
	case decision != nil:
		s.Outcome, s.Message = "selected", "A candidate met the configured checks on sampled video; this is not a guarantee of full-file or audio quality."
		s.NextStep = "For transcode_media, monitor the same action as it continues. A standalone benchmark produces no final file; use its selected settings explicitly for encoding."
		s.UserNextStep = "Nothing to do: the encode continues with these settings."
		if res.Status == action.StatusFailed {
			s.NextStep = "The benchmark selected a candidate, but a later action step failed. Inspect the action error; do not repeat the successful search automatically."
			s.UserNextStep = "The test found settings, but a later step failed. Check the error in the job details."
		}
		// Plain benchmarks rank by size without a savings floor, so the
		// smallest passing candidate can still be larger than the source.
		if decision.Winner != nil && decision.Winner.SavingsPercent < 0 {
			s.SizeWarning = fmt.Sprintf("The selected candidate is estimated %.1f%% larger than the original. Encoding it would not save space, and transcode_media rejects growth beyond its max_size_increase_percent (0%% by default).", -decision.Winner.SavingsPercent)
			s.NextStep = "Keep the original: this source is already efficiently encoded for these quality limits. Do not encode the selected settings to save space."
			s.UserNextStep = "Keep the original: it is already efficiently encoded, and these settings would make it larger."
		}
	case status == "failed" || res.Status == action.StatusFailed:
		s.Outcome, s.Message, s.NextStep = "execution_error", "The benchmark could not finish reliably.", "Inspect the action error and fix the execution problem before retrying; changing compression settings is not an established remedy."
		s.UserNextStep = "The test could not finish. Check the error in the job details and fix it before retrying; different settings will not help."
	}
	if getString := summaryInput(res, "mode"); getString != "" {
		if explanation := action.ExplainTranscode(res); explanation != nil && (explanation.Outcome == "no_suitable_candidate" || explanation.Outcome == "size_rejected") {
			s.Outcome = explanation.Outcome
			s.Message = explanation.Message
			s.NextStep = "Keep the original, or explicitly choose another processing goal. Technical profiles are optional; quality requirements are never relaxed automatically."
			s.UserNextStep = "Keep the original, or choose another goal. Quality requirements are never lowered automatically."
		}
	}
	// Preflight stores profile_base as a provisional plan before any search,
	// so the source is reported only once the benchmark settled it.
	benchmarkField(res, "plan_note", &s.PlanNote)
	var planSource string
	benchmarkField(res, "plan_source", &planSource)
	if planSource == "benchmark_winner" || s.PlanNote != "" {
		s.PlanSource = planSource
	}
	var evidence []transcode.BenchmarkCandidateQualityEvidence
	var request transcode.BenchmarkRequest
	benchmarkField(res, "benchmark_quality", &evidence)
	hasRequest := benchmarkField(res, "benchmark_request", &request)
	if decision != nil {
		transcode.AttachVMAFEvidence(decision, evidence)
		for _, ev := range decision.Evaluations {
			if ev.VMAF == nil || len(s.Measured) >= transcode.MaxBenchmarkCandidates {
				continue
			}
			s.Measured = append(s.Measured, benchmarkMeasured{CandidateID: ev.CandidateID, VideoCodec: ev.VideoCodec, Quality: ev.Quality, Preset: ev.Preset, AverageBitrateKbps: ev.AverageBitrateKbps, Passed: ev.Eligible, Selected: decision.Winner != nil && decision.Winner.CandidateID == ev.CandidateID, VMAF: ev.VMAF})
		}
		s.Selection = decision.Selection
		// Historical decisions can be explained from their immutable request.
		if s.Selection == nil && hasRequest && decision.DecisionReason == optimization.ReasonTargetReachedSmallestSize && decision.Winner != nil {
			tolerance := optimization.DefaultVMAFPolicy().Tolerance()
			var thresholds *transcode.BenchmarkQualityThresholds
			if decision.Winner.MetricType == "ssim" {
				tolerance = optimization.DefaultSSIMPolicy().Tolerance()
			}
			if request.Quality != nil {
				thresholds = request.Quality.VMAF
				if decision.Winner.MetricType == "ssim" {
					thresholds = request.Quality.SSIM
				}
			}
			if thresholds != nil && thresholds.MarginalTolerance != nil {
				tolerance = *thresholds.MarginalTolerance
			}
			var candidates []optimization.EvaluatedCandidate
			for _, c := range decision.Evaluations {
				candidates = append(candidates, optimization.EvaluatedCandidate{CandidateID: c.CandidateID, Eligible: c.Eligible, TargetReached: c.TargetReached, Score: c.Score})
			}
			s.Selection = optimization.ExplainTargetSelection(candidates, tolerance)
		}
		if s.Selection != nil && s.Outcome == "selected" {
			s.Message = "Selected the smallest estimated file among target-reaching candidates within the configured quality margin of the best score. Sampled video is not a guarantee of full-file or audio quality."
		}
		for _, ev := range decision.Evaluations {
			if ev.Eligible || len(s.Rejected) >= transcode.MaxBenchmarkCandidates {
				continue
			}
			r := benchmarkRejection{CandidateID: ev.CandidateID, FailedSamples: ev.FailedSamples}
			for _, q := range evidence {
				if q.CandidateID != ev.CandidateID {
					continue
				}
				for _, code := range q.ReasonCodes {
					// Observe-only warnings are not rejection reasons.
					if request.Quality != nil && strings.HasPrefix(code, "quality_cambi_") && request.Quality.Banding != nil && request.Quality.Banding.Enforcement != "reject" {
						continue
					}
					if request.Quality != nil && strings.HasPrefix(code, "quality_vmaf_") && request.Quality.VMAF != nil && request.Quality.VMAF.GuardrailEnforcement != "reject" {
						continue
					}
					c := benchmarkCheck{Code: code, Reason: benchmarkFailureText(code)}
					if q.CAMBI != nil && strings.HasPrefix(code, "quality_cambi_") {
						r.ReviewSourceSeconds = &q.CAMBI.WorstSourceSec
						if code == "quality_cambi_mean_above_maximum" {
							c.Actual = &q.CAMBI.Mean
							if request.Quality != nil && request.Quality.Banding != nil {
								c.Limit = request.Quality.Banding.MaxMean
							}
						}
						if code == "quality_cambi_peak_above_maximum" {
							c.Actual = &q.CAMBI.Max
							if request.Quality != nil && request.Quality.Banding != nil {
								c.Limit = request.Quality.Banding.MaxPeak
							}
						}
					} else if q.VMAF != nil {
						r.ReviewSourceSeconds = &q.VMAF.WorstWindowSourceSec
						if request.Quality != nil && request.Quality.VMAF != nil {
							if code == "quality_vmaf_p5_below_minimum" {
								c.Actual, c.Limit = &q.VMAF.P5, request.Quality.VMAF.P5Minimum
							}
							if code == "quality_vmaf_worst_window_below_minimum" {
								c.Actual, c.Limit = &q.VMAF.WorstWindowMean, request.Quality.VMAF.WorstWindowMinimum
							}
						}
					}
					r.Checks = append(r.Checks, c)
				}
			}
			if len(r.Checks) == 0 {
				reason := benchmarkFailureText(ev.EvaluationReason)
				if ev.EvaluationReason == "skipped_quality_dominated" {
					reason = dominatedText(ev.Uncertainties)
				}
				if ev.EvaluationReason == "skipped_savings_dominated" {
					reason = savingsDominatedText(ev.Uncertainties)
				}
				r.Checks = []benchmarkCheck{{Code: ev.EvaluationReason, Reason: reason}}
			}
			s.Rejected = append(s.Rejected, r)
		}
	}
	var plan transcode.Plan
	var source struct {
		Audio []mediainspect.DetailedStream `json:"audio"`
	}
	if benchmarkField(res, "plan", &plan) && benchmarkField(res, "original", &source) {
		for _, a := range source.Audio {
			if len(s.Audio) >= 8 {
				break
			}
			target, kbps := transcode.AudioTarget(plan.AudioMode, transcode.DetailedAudioTrack(a.Codec, a.Profile, a.Title, a.Tags, a.Channels, a.ChannelLayout))
			entry := benchmarkAudio{Stream: a.Index, Language: a.Language, Channels: a.Channels, SourceCodec: a.Codec, Operation: "copy", OutputCodec: a.Codec}
			if target != "copy" {
				entry.Operation, entry.OutputCodec, entry.TargetKbps = "convert_lossy", target, kbps
			}
			s.Audio = append(s.Audio, entry)
		}
		s.AudioNote = "Planned audio only (first 8 tracks). The benchmark measures video and estimates size; it does not encode or validate final audio. Compact mode copies already compact codecs such as EAC3."
	}
	return s
}

func benchmarkFailureText(code string) string {
	switch code {
	case "quality_cambi_mean_above_maximum":
		return "Average additional banding exceeds the configured limit; full-reference CAMBI already accounts for banding in the source."
	case "quality_cambi_peak_above_maximum":
		return "Peak additional banding exceeds the configured limit. Inspect the indicated source fragment."
	case "quality_vmaf_p5_below_minimum":
		return "The lower-quality portion of the sampled frames is below the configured VMAF limit."
	case "quality_vmaf_worst_window_below_minimum":
		return "The worst sampled time window is below the configured VMAF limit."
	case "sample_below_minimum_quality", "below_minimum_quality":
		return "Sampled video did not meet the configured minimum quality."
	case "skipped_quality_dominated":
		return dominatedText(nil)
	case "skipped_savings_dominated":
		return savingsDominatedText(nil)
	case "insufficient_savings":
		return "The estimated file does not reach the minimum savings compared with the original, so it cannot be selected whatever its quality."
	case "insufficient_savings_before_metrics":
		return "Not measured: every encoded sample was already estimated below the minimum savings, so no quality result could produce an acceptable winner."
	case "verification_not_passed":
		return "Passed the initial samples but failed the additional verification samples."
	default:
		return "This check did not pass; inspect its detailed evidence before deciding whether another encode can address it."
	}
}

// dominatedText explains a candidate that quality pruning never encoded,
// naming the failed higher-quality leader the worker recorded.
func dominatedText(uncertainties []string) string {
	for _, u := range uncertainties {
		if leader, ok := strings.CutPrefix(u, "not measured: "); ok {
			if leader, ok = strings.CutSuffix(leader, " failed quality"); ok && leader != "" {
				return "Not encoded: " + leader + " is a higher-quality setting of the same encoder and already failed quality, so this setting cannot pass."
			}
		}
	}
	return "Not encoded: a higher-quality setting of the same encoder already failed quality, so this setting cannot pass."
}

// savingsDominatedText explains a candidate the savings precheck never
// encoded, naming the more-compressed leader that already missed the minimum.
func savingsDominatedText(uncertainties []string) string {
	for _, u := range uncertainties {
		if leader, ok := strings.CutPrefix(u, "not measured: "); ok {
			if leader, ok = strings.CutSuffix(leader, " is below minimum savings"); ok && leader != "" {
				return "Not encoded: " + leader + " is a smaller setting of the same encoder and is already estimated below the minimum savings, so this setting cannot save enough."
			}
		}
	}
	return "Not encoded: a smaller setting of the same encoder is already estimated below the minimum savings, so this setting cannot save enough."
}
