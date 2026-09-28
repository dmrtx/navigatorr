package tools

import (
	"encoding/json"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

type benchmarkSummary struct {
	Outcome            string                            `json:"outcome"`
	Message            string                            `json:"message"`
	NextStep           string                            `json:"next_step"`
	ProgressScope      string                            `json:"progress_scope"`
	PreviousRounds     []string                          `json:"previous_unsuccessful_actions,omitempty"`
	ProposedCandidates []transcode.BenchmarkCandidate    `json:"proposed_candidates,omitempty"`
	ProposedQuality    *transcode.BenchmarkQualityConfig `json:"proposed_quality,omitempty"`
	ProposedSamples    []transcode.BenchmarkSampleWindow `json:"proposed_samples,omitempty"`
	Rejected           []benchmarkRejection              `json:"rejected,omitempty"`
	Audio              []benchmarkAudio                  `json:"planned_audio,omitempty"`
	AudioNote          string                            `json:"audio_note,omitempty"`
}

type benchmarkRejection struct {
	CandidateID         string           `json:"candidate_id"`
	Checks              []benchmarkCheck `json:"checks"`
	ReviewSourceSeconds *float64         `json:"review_source_seconds,omitempty"`
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
	s := &benchmarkSummary{ProgressScope: "Worker progress is for the benchmark only, not the full encode.", Outcome: "testing", Message: "Testing short video samples; no quality decision yet.", NextStep: "Monitor this action with action_status. A progress percentage does not mean quality passed; do not launch another search."}
	switch {
	case res.Status == action.StatusCancelled:
		s.Outcome, s.Message, s.NextStep = "cancelled", "Search cancelled.", "Keep the original; inspect action details before starting other work."
	case review:
		s.Outcome, s.Message = "search_review_required", "Two earlier rounds found no acceptable candidate; this new benchmark has not been submitted."
		s.NextStep = "Review the previous actions and this proposed recipe. Stop, choose direct encoding, or use the displayed run_once_after_review decision only on explicit user instruction. Do not automatically change CRF or thresholds and start another action."
		benchmarkField(res, "previous_unsuccessful_benchmarks", &s.PreviousRounds)
		var req transcode.BenchmarkRequest
		if benchmarkField(res, "benchmark_request", &req) {
			s.ProposedCandidates = req.Candidates
			s.ProposedQuality, s.ProposedSamples = req.Quality, req.Samples
		}
	case decision != nil && decision.Winner == nil:
		s.Outcome, s.Message = "no_acceptable_candidate", "The benchmark finished, but no candidate met all configured checks. Full encoding did not start."
		s.NextStep = "Inspect the rejected checks and indicated source fragment before choosing a next step. Keep the original, or explicitly choose direct encoding via a new transcode_media action with a non-optimizing profile/profile_config (optimization.enabled=false). Direct encoding does not claim a perceptual quality pass and never replaces the original. Do not automatically start another CRF ladder or relax thresholds."
	case decision != nil:
		s.Outcome, s.Message = "selected", "A candidate met the configured checks on sampled video; this is not a guarantee of full-file or audio quality."
		s.NextStep = "For transcode_media, monitor the same action as it continues. A standalone benchmark produces no final file; use its selected settings explicitly for encoding."
		if res.Status == action.StatusFailed {
			s.NextStep = "The benchmark selected a candidate, but a later action step failed. Inspect the action error; do not repeat the successful search automatically."
		}
	case status == "failed" || res.Status == action.StatusFailed:
		s.Outcome, s.Message, s.NextStep = "execution_error", "The benchmark could not finish reliably.", "Inspect the action error and fix the execution problem before retrying; changing compression settings is not an established remedy."
	}
	var evidence []transcode.BenchmarkCandidateQualityEvidence
	var request transcode.BenchmarkRequest
	benchmarkField(res, "benchmark_quality", &evidence)
	benchmarkField(res, "benchmark_request", &request)
	if decision != nil {
		for _, ev := range decision.Evaluations {
			if ev.Eligible || len(s.Rejected) >= transcode.MaxBenchmarkCandidates {
				continue
			}
			r := benchmarkRejection{CandidateID: ev.CandidateID}
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
				r.Checks = []benchmarkCheck{{Code: ev.EvaluationReason, Reason: benchmarkFailureText(ev.EvaluationReason)}}
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
			target, kbps := transcode.AudioTarget(plan.AudioMode, a.Codec, a.Channels)
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
	default:
		return "This check did not pass; inspect its detailed evidence before deciding whether another encode can address it."
	}
}
