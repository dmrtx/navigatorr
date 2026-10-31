package maintenanceui

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
	"github.com/mark3labs/mcp-go/mcp"
)

// Savings describe content bytes, rather than filesystem allocation or free
// disk measurements. A smaller candidate is only potential savings: its
// original and recovery copy may still exist. Realized savings require the
// promotion's verified final cleanup to have completed.
type operationSavings struct {
	SourceBytes         *int64 `json:"source_bytes,omitempty"`
	CandidateBytes      *int64 `json:"candidate_bytes,omitempty"`
	EstimatedBytes      *int64 `json:"estimated_bytes,omitempty"`
	EstimatedSavedBytes *int64 `json:"estimated_saved_bytes,omitempty"`
	CandidateSavedBytes *int64 `json:"candidate_saved_bytes,omitempty"`
	RealizedSavedBytes  *int64 `json:"realized_saved_bytes,omitempty"`
	EstimateKind        string `json:"estimate_kind,omitempty"`
	ETASeconds          *int64 `json:"eta_seconds,omitempty"`
	MeasuredFiles       int    `json:"measured_files,omitempty"`
	Partial             bool   `json:"partial,omitempty"`
}

type savingsReplacement struct {
	ActionID        string `json:"action_id"`
	SourcePath      string `json:"source_path"`
	CompletedAt     string `json:"completed_at"`
	SourceBytes     int64  `json:"source_bytes"`
	CandidateBytes  int64  `json:"candidate_bytes"`
	SavedBytes      int64  `json:"saved_bytes"`
	CumulativeBytes int64  `json:"cumulative_bytes"`
}

type operationsPage struct {
	Jobs        []map[string]any `json:"jobs"`
	Offset      int              `json:"offset"`
	HasMore     bool             `json:"has_more"`
	Total       int              `json:"total"`
	ActiveCount int              `json:"active_count"`
	// DecisionCount is every workflow that waits on a person, whatever the
	// status filter or page, so none can be missed in a long history.
	DecisionCount int `json:"decision_count"`
	Savings       struct {
		RealizedBytes         int64  `json:"realized_bytes"`
		CompletedReplacements int    `json:"completed_replacements"`
		CandidateBytes        int64  `json:"candidate_bytes"`
		CandidateSavedBytes   int64  `json:"candidate_saved_bytes"`
		CandidateCount        int    `json:"candidate_count"`
		EstimatedBytes        int64  `json:"estimated_bytes"`
		EstimatedSavedBytes   int64  `json:"estimated_saved_bytes"`
		Measurement           string `json:"measurement"`
	} `json:"savings"`
	History []savingsReplacement `json:"history"`
	// Daily is the space freed per local day over the last month; Ready lists
	// the largest finished encodes still waiting to replace their originals.
	Daily      []savingsDay       `json:"daily"`
	Ready      []savingsCandidate `json:"ready"`
	ObservedAt string             `json:"observed_at"`
}

type operationRecord struct {
	inst                   store.ActionInstance
	inputs, outputs, state map[string]any
	savings                operationSavings
}

func decodeOperationJSON(raw string) map[string]any {
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}
func operationMap(v any) map[string]any { out, _ := v.(map[string]any); return out }
func operationNumber(v any) float64     { f, _ := v.(float64); return f }
func operationString(v any) string      { s, _ := v.(string); return s }
func operationValue(r operationRecord, key string) any {
	if v, ok := r.outputs[key]; ok {
		return v
	}
	return r.state[key]
}

// operationQueuedOnWorker is true while the worker has accepted the job but is
// still working through the jobs ahead of it.
func operationQueuedOnWorker(r operationRecord) bool {
	return operationActive(r.inst.Status) && operationString(operationValue(r, "transcode_phase")) == "queued"
}

// queuedOnWorker counts the batch items that are running only on paper: handed
// to a worker, in its line, not encoding.
func queuedOnWorker(items []store.TranscodeBatchItem, records map[string]operationRecord) int {
	n := 0
	for _, item := range items {
		if item.Status == "running" && operationQueuedOnWorker(records[item.ChildActionID]) {
			n++
		}
	}
	return n
}
func operationActive(status string) bool {
	return status != store.ActionStatusCompleted && status != store.ActionStatusFailed && status != store.ActionStatusCancelled
}
func operationSourceKey(r operationRecord) string {
	path := operationString(r.state["resolved_path"])
	if path == "" {
		path = operationString(r.inputs["path"])
	}
	sha := operationString(operationValue(r, "original_sha256"))
	if path == "" {
		return "action:" + r.inst.ID
	}
	return path + "\x00" + sha
}

// Import context is a durable routing hint, never evidence that replacement is
// safe. The existing promotion workflow must independently resolve the exact
// original file and validate the candidate again before any mutation.
func operationLibraryID(value any) int64 {
	if text, ok := value.(string); ok {
		n, err := strconv.ParseInt(text, 10, 32)
		if err == nil && n > 0 {
			return n
		}
		return 0
	}
	n, ok := value.(float64)
	if ok && n > 0 && n <= math.MaxInt32 && math.Trunc(n) == n {
		return int64(n)
	}
	return 0
}

func operationReplacementContext(r operationRecord, recordsByID map[string]operationRecord) map[string]any {
	context := func(service string, id int64, source string) map[string]any {
		if id <= 0 || (service != "sonarr" && service != "radarr") {
			return nil
		}
		key := "series_id"
		if service == "radarr" {
			key = "movie_id"
		}
		return map[string]any{"service": service, key: id, "source": source}
	}
	if library := operationMap(r.inputs["library_context"]); library != nil {
		if hint := context(operationString(library["service"]), operationLibraryID(library["id"]), "job_inputs"); hint != nil {
			return hint
		}
	}
	service := operationString(r.inputs["service"])
	key := "series_id"
	if service == "radarr" {
		key = "movie_id"
	}
	id := operationLibraryID(r.inputs[key])
	if id == 0 {
		id = operationLibraryID(r.inputs["media_id"])
	}
	if hint := context(service, id, "job_inputs"); hint != nil {
		return hint
	}
	if parent, found := recordsByID[operationString(r.inputs["parent_action_id"])]; found && parent.inst.ActionName == "transcode_batch" {
		if hint := context(operationString(parent.inputs["service"]), operationLibraryID(parent.inputs["series_id"]), "parent_batch"); hint != nil {
			return hint
		}
		if _, filesystemBatch := parent.inputs["paths"]; filesystemBatch {
			return map[string]any{"service": "filesystem", "source": "parent_batch"}
		}
	}
	return nil
}

// operationTracks is what a file's job kept of the source's audio and subtitle
// tracks: the report the final validation gives, else the one the preflight
// stored when it chose them. It is nil when the goal kept every track.
func operationTracks(r operationRecord) map[string]any {
	if r.inst.ActionName != "transcode_media" {
		return nil
	}
	for _, key := range []string{"tracks", "track_report"} {
		if report := operationMap(operationValue(r, key)); report != nil {
			return report
		}
	}
	return nil
}

// operationGoalKeys are the inputs that make up a file job's goal, the ones a
// new job for the same file needs to run the same goal: Choose tracks starts one
// from them. Paths, receipts and profiles are not among them.
var operationGoalKeys = []string{"mode", "max_height", "allow_hardware", "allow_slow_preset", "size_budget_percent", "size_budget_bytes", "size_budget_source", "size_budget_sibling_multiplier", "audio_only", "media_type", "is_anime"}

// operationGoal is the goal a Save space or Shrink job was submitted with, as
// the job's inputs hold it. It is nil for any other job.
func operationGoal(r operationRecord) map[string]any {
	if r.inst.ActionName != "transcode_media" {
		return nil
	}
	if mode := operationString(r.inputs["mode"]); mode != "size" && mode != "compact" {
		return nil
	}
	goal := map[string]any{}
	for _, key := range operationGoalKeys {
		if value, ok := r.inputs[key]; ok && value != nil {
			goal[key] = value
		}
	}
	return goal
}

// operationSizeMode summarises what mode=size decided for one file: the
// sampled output estimate, the final quality measurement and the audio
// tracks it compacted. It is nil for every other mode.
func operationSizeMode(r operationRecord) map[string]any {
	if r.inst.ActionName != "transcode_media" || operationString(r.state["mode"]) != "size" {
		return nil
	}
	out := map[string]any{}
	if r.savings.EstimatedBytes != nil && (r.savings.EstimateKind == "sampled_benchmark" || r.savings.EstimateKind == "audio_only_estimate" || r.savings.EstimateKind == "size_budget") {
		out["expected_bytes"] = *r.savings.EstimatedBytes
	}
	// A frozen budget that was not applied says why: the siblings gave none,
	// or no encode at it could be planned.
	if operationNumber(operationMap(operationValue(r, "size_budget"))["budget_bytes"]) <= 0 {
		if note := operationString(operationValue(r, "size_budget_skip_message")); note != "" {
			out["size_budget_note"] = note
		} else if siblings := operationMap(operationValue(r, "size_budget_siblings")); operationString(siblings["reason"]) != "" {
			out["size_budget_note"] = operationString(siblings["message"])
		}
	}
	if r.state["size_main10"] == true {
		out["main10"] = true
	}
	// Which encoder produced the winning settings, so a hardware (VideoToolbox)
	// result is told apart from x265 before anything is replaced.
	winner := operationMap(operationMap(operationValue(r, "benchmark_decision"))["winner"])
	budget := operationMap(operationValue(r, "size_budget"))
	if operationNumber(budget["budget_bytes"]) > 0 {
		// The search's winner (if any) was over the budget: the file is
		// encoded at the budget instead, as x265 unless the budget's own
		// choice was the media engine.
		out["encoder"] = transcode.VideoCodecLibX265
		if operationString(operationMap(budget["encoder_choice"])["video_codec"]) == transcode.VideoCodecHEVCVideoToolbox {
			out["encoder"], out["hardware"] = transcode.VideoCodecHEVCVideoToolbox, true
		}
		out["size_budget"] = operationSizeBudget(budget)
	} else if codec := operationString(winner["video_codec"]); codec != "" {
		out["encoder"] = codec
		if codec == transcode.VideoCodecHEVCVideoToolbox {
			out["hardware"] = true
			// Why the media engine result was chosen, from the verified decision.
			var notes []string
			uncertainties, _ := winner["uncertainties"].([]any)
			for _, u := range uncertainties {
				notes = append(notes, operationString(u))
			}
			if reason := transcode.HardwareReason(notes); reason != "" {
				out["hardware_reason"] = reason
			}
		}
	}
	if r.state["size_audio_only"] == true {
		// The worker's packet proof decides; until it reports, the copy is
		// planned, not verified.
		out["video_copy"] = "pending"
		switch operationMap(operationMap(operationValue(r, "quality_evidence"))["video_copy"])["verdict"] {
		case "pass":
			out["video_copy"] = "verified"
		case "fail":
			out["video_copy"] = "failed"
		}
	}
	if vmaf := operationMap(operationMap(operationValue(r, "quality_evidence"))["vmaf"]); vmaf["mean"] != nil {
		final := map[string]any{"vmaf_mean": operationNumber(vmaf["mean"])}
		if vmaf["p5"] != nil {
			final["vmaf_p5"] = operationNumber(vmaf["p5"])
		}
		if worst, ok := vmaf["worst_window_mean"].(float64); ok {
			final["worst_window_mean"] = worst
			final["worst_window_seconds"] = operationNumber(vmaf["worst_window_seconds"])
		}
		// Which full-file checks the result failed, so a rejected result names
		// the metric and not only the verdict.
		if codes, _ := operationMap(operationValue(r, "quality_evidence"))["reason_codes"].([]any); len(codes) > 0 {
			final["failed_checks"] = codes
		}
		out["final_quality"] = final
	}
	if fresh := operationMap(operationMap(operationValue(r, "quality_evidence"))["fresh_vmaf"]); fresh["mean"] != nil {
		if final, ok := out["final_quality"].(map[string]any); ok {
			final["fresh_vmaf_mean"] = operationNumber(fresh["mean"])
			if fresh["p5"] != nil {
				final["fresh_vmaf_p5"] = operationNumber(fresh["p5"])
			}
		}
	}
	// How the winner was verified, and the search and final scores of every
	// window side by side.
	var search transcode.BenchmarkAutomaticEvidence
	var final transcode.FinalQualityEvidence
	if raw, err := json.Marshal(operationValue(r, "automatic_search")); err == nil {
		_ = json.Unmarshal(raw, &search)
	}
	if raw, err := json.Marshal(operationValue(r, "quality_evidence")); err == nil {
		_ = json.Unmarshal(raw, &final)
	}
	if v := search.Verification; v != nil && len(v.Attempts) > 0 || search.VerificationWindows != "" {
		verification := map[string]any{"windows": search.VerificationWindows}
		if v := search.Verification; v != nil {
			verification["attempts"] = v.Attempts
			if v.StoppedBy != "" {
				verification["stopped_by"] = v.StoppedBy
				verification["unverified"] = len(v.Unverified)
			}
		}
		out["verification"] = verification
	}
	if windows := transcode.CompareWindows(search.WinnerWindows, final.Windows); len(windows) > 0 {
		out["windows"] = windows
	}
	// A VideoToolbox result that failed the full-file check and the x265
	// search that followed: the failed attempt's numbers stay visible after
	// the x265 result replaces its evidence.
	if hc := operationMap(r.state["hardware_continuation"]); operationString(hc["outcome"]) != "" {
		note := map[string]any{"outcome": hc["outcome"], "message": operationString(hc["message"])}
		if id := operationString(hc["failed_candidate_id"]); id != "" {
			note["failed_candidate_id"] = id
		}
		if final := operationMap(hc["final"]); final != nil {
			failed := map[string]any{}
			measured := operationMap(final["measured"])
			for from, to := range map[string]string{"vmaf_mean": "vmaf_mean", "vmaf_p5": "vmaf_p5", "vmaf_worst_window": "worst_window_mean"} {
				if measured[from] != nil {
					failed[to] = operationNumber(measured[from])
				}
			}
			if limits := operationMap(final["limits"]); limits != nil && limits["worst_window_seconds"] != nil {
				failed["worst_window_seconds"] = operationNumber(limits["worst_window_seconds"])
			}
			if checks, _ := final["failed_checks"].([]any); len(checks) > 0 {
				failed["failed_checks"] = checks
			}
			if failures, _ := final["failures"].([]any); len(failures) > 0 {
				failed["failures"] = failures
			}
			note["failed_quality"] = failed
		}
		if windows, _ := hc["windows"].([]any); len(windows) > 0 {
			note["windows"] = windows
		}
		out["hardware_continuation"] = note
	}
	audio := []map[string]any{}
	conversions, _ := operationValue(r, "conversions").([]any)
	for _, raw := range conversions {
		c := operationMap(raw)
		if operationString(c["stream_type"]) == "audio" {
			audio = append(audio, map[string]any{"stream_index": int(operationNumber(c["stream_index"])), "from_codec": operationString(c["from_codec"]), "to_codec": operationString(c["to_codec"])})
		}
	}
	if len(audio) > 0 {
		out["compacted_audio"] = audio
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// operationSizeBudget is the budget block of a size projection: the budget, what
// the encode aimed at, how it ended and the rate-quality points to draw.
func operationSizeBudget(budget map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"trigger", "source_bytes", "budget_bytes", "budget_percent", "target_bytes", "video_bitrate_kbps", "over_budget_candidate_id", "over_budget_estimated_bytes", "quality_target", "quality_floor", "siblings", "outcome", "candidate_bytes", "below_target", "message", "encoder_choice", "retry_without_hardware"} {
		if v, ok := budget[key]; ok && v != nil {
			out[key] = v
		}
	}
	if final := operationMap(budget["final"]); final != nil {
		out["final"] = map[string]any{"verdict": final["verdict"], "failures": final["failures"], "measured": final["measured"]}
	}
	if curve, _ := budget["curve"].([]any); len(curve) > 0 {
		out["curve"] = curve
	}
	return out
}

// operationSizeBudgetOutcome is how a size budget encode ended ("" without one),
// so a candidate that is below the quality target is labelled wherever it is
// offered.
func operationSizeBudgetOutcome(r operationRecord) string {
	if r.inst.ActionName != "transcode_media" {
		return ""
	}
	return operationString(operationMap(operationValue(r, "size_budget"))["outcome"])
}

func projectOperationSavings(r operationRecord, now time.Time) operationSavings {
	var s operationSavings
	original := operationMap(operationValue(r, "original"))
	source := int64(operationNumber(original["size_bytes"]))
	if source > 0 {
		s.SourceBytes = &source
	}
	candidate := int64(operationNumber(operationMap(operationValue(r, "result"))["size_bytes"]))
	if candidate <= 0 {
		candidate = int64(operationNumber(operationValue(r, "candidate_size_bytes")))
	}
	if r.inst.ActionName == "promote_transcode_candidate" {
		p := operationMap(operationValue(r, "promotion"))
		source = int64(operationNumber(p["original_bytes"]))
		candidate = int64(operationNumber(p["candidate_bytes"]))
		if source > 0 {
			s.SourceBytes = &source
		}
		cleaned := p["recovery_cleanup_completed"] == true || p["recovery_cleanup_started"] == true && r.outputs["recovery_retained"] == false
		verified := p["recovery_verified_before_replacement"] == true || r.outputs["original_integrity"] == "verified_before_replacement"
		if r.inst.Status == store.ActionStatusCompleted && r.outputs["promoted"] == true && r.outputs["recovery_retained"] == false && cleaned && verified && source > 0 && candidate > 0 {
			saved := source - candidate
			s.RealizedSavedBytes = &saved
		}
	}
	if source > 0 && candidate > 0 {
		s.CandidateBytes = &candidate
		saved := source - candidate
		s.CandidateSavedBytes = &saved
	}
	winner := operationMap(operationMap(operationValue(r, "benchmark_decision"))["winner"])
	estimated := int64(operationNumber(winner["estimated_bytes"]))
	if budget := operationMap(operationValue(r, "size_budget")); operationNumber(budget["budget_bytes"]) > 0 && source > 0 {
		// The budget encode replaced the search's outcome: its planned size, or
		// its measured size once there is one, not an over-budget winner's.
		estimated = int64(operationNumber(budget["candidate_bytes"]))
		if estimated <= 0 {
			estimated = int64(operationNumber(budget["target_bytes"]))
		}
		if estimated > 0 {
			s.EstimateKind = "size_budget"
		}
	} else if estimated > 0 && source > 0 {
		s.EstimateKind = "sampled_benchmark"
	} else if audio := int64(operationNumber(operationValue(r, "audio_only_estimated_bytes"))); audio > 0 && source > 0 && operationValue(r, "size_audio_only") == true {
		estimated = audio
		s.EstimateKind = "audio_only_estimate"
	} else if percent, ok := operationValue(r, "expected_savings_percent").(float64); ok && source > 0 && percent <= 100 {
		estimated = int64(float64(source) * (1 - percent/100))
		s.EstimateKind = "profile_heuristic"
	}
	if estimated > 0 && source > 0 {
		s.EstimatedBytes = &estimated
		saved := source - estimated
		s.EstimatedSavedBytes = &saved
	}
	phase := operationString(operationValue(r, "transcode_phase"))
	if phase == "encoding" && operationActive(r.inst.Status) && operationValue(r, "progress_is_stale") != true {
		progress := operationNumber(operationValue(r, "progress"))
		speed := operationNumber(operationValue(r, "speed"))
		duration := operationNumber(original["duration_sec"])
		last, err := time.Parse(time.RFC3339Nano, operationString(operationValue(r, "last_progress_at")))
		if err == nil && now.Sub(last) >= 0 && now.Sub(last) <= 30*time.Second && progress >= 0 && progress < 100 && speed > 0 && duration > 0 {
			eta := int64(math.Ceil(duration * (1 - progress/100) / speed))
			s.ETASeconds = &eta
		}
	}
	return s
}

func operationPaging(r *http.Request) (offset, limit int, err error) {
	limit = 25
	for key, target := range map[string]*int{"offset": &offset, "limit": &limit} {
		if raw := r.URL.Query().Get(key); raw != "" {
			*target, err = strconv.Atoi(raw)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid %s", key)
			}
		}
	}
	if offset < 0 || limit < 1 || limit > 100 {
		return 0, 0, fmt.Errorf("offset must be nonnegative and limit between 1 and 100")
	}
	return
}

// Batch figures project each real encoding child once. They never enter the
// global totals a second time. Unknown queued files are explicitly partial,
// rather than extrapolating a sampled episode's result across a whole season.
func aggregateBatchSavings(records []operationRecord) {
	children := map[string][]operationRecord{}
	parentsByChild := map[string]string{}
	for _, r := range records {
		if r.inst.ActionName == "transcode_media" {
			if parent := operationString(r.inputs["parent_action_id"]); parent != "" {
				children[parent] = append(children[parent], r)
				parentsByChild[r.inst.ID] = parent
			}
		}
	}
	promotions := map[string][]operationRecord{}
	for _, r := range records {
		if r.savings.RealizedSavedBytes != nil {
			p := operationMap(operationValue(r, "promotion"))
			if parent := parentsByChild[operationString(p["transcode_action_id"])]; parent != "" {
				promotions[parent] = append(promotions[parent], r)
			}
		}
	}
	for i := range records {
		if records[i].inst.ActionName != "transcode_batch" {
			continue
		}
		s := operationSavings{}
		add := func(dst **int64, src *int64) {
			if src != nil {
				if *dst == nil {
					n := int64(0)
					*dst = &n
				}
				**dst += *src
			}
		}
		seen := map[string]bool{}
		previousAttempts := map[string]bool{}
		for _, entries := range operationMap(records[i].state["batch_attempt_history"]) {
			attempts, _ := entries.([]any)
			for _, entry := range attempts {
				previousAttempts[operationString(operationMap(entry)["child_action_id"])] = true
			}
		}
		for _, child := range children[records[i].inst.ID] {
			if previousAttempts[child.inst.ID] {
				continue
			}
			generations := operationMap(records[i].state["batch_item_generations"])
			base := "batch-" + records[i].inst.ID + "-"
			if strings.HasPrefix(child.inst.IdempotencyKey, base) {
				key := strings.TrimPrefix(child.inst.IdempotencyKey, base)
				itemKey := strings.Split(key, "-attempt-")[0]
				expected := base + itemKey
				if gen := operationLibraryID(generations[itemKey]); gen > 0 {
					expected += fmt.Sprintf("-attempt-%d", int(gen))
				}
				if child.inst.IdempotencyKey != expected {
					continue
				}
			}

			key := operationSourceKey(child)
			if seen[key] {
				continue
			}
			seen[key] = true
			add(&s.SourceBytes, child.savings.SourceBytes)
			add(&s.CandidateBytes, child.savings.CandidateBytes)
			add(&s.CandidateSavedBytes, child.savings.CandidateSavedBytes)
			add(&s.EstimatedBytes, child.savings.EstimatedBytes)
			add(&s.EstimatedSavedBytes, child.savings.EstimatedSavedBytes)
			if child.savings.CandidateBytes != nil {
				s.MeasuredFiles++
			}
		}
		seen = map[string]bool{}
		for _, promoted := range promotions[records[i].inst.ID] {
			p := operationMap(operationValue(promoted, "promotion"))
			key := operationString(p["original_path"]) + "\x00" + operationString(p["original_sha256"])
			if seen[key] {
				continue
			}
			seen[key] = true
			add(&s.RealizedSavedBytes, promoted.savings.RealizedSavedBytes)
		}
		counts := operationMap(operationValue(records[i], "counts"))
		planned := int(operationNumber(counts["transcode"]))
		if planned == 0 {
			planned = int(operationNumber(counts["total"])) - int(operationNumber(counts["skip"])) - int(operationNumber(counts["review"]))
		}
		s.Partial = s.MeasuredFiles < planned
		if s.EstimatedBytes != nil {
			s.EstimateKind = "encoding_children"
		}
		records[i].savings = s
	}
}

func (s *Server) operations(w http.ResponseWriter, req *http.Request) {
	offset, limit, err := operationPaging(req)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	status := strings.TrimSpace(req.URL.Query().Get("status"))
	selectedID := strings.TrimSpace(req.URL.Query().Get("id"))
	grouped := req.URL.Query().Get("group") == "workflow" && selectedID == ""
	if status != "" && status != "all" && status != "archived" && status != "active" && status != "pending" && status != "running" && status != "waiting_external" && status != "waiting_decision" && status != "completed" && status != "failed" && status != "cancelled" {
		fail(w, 400, "invalid workflow status")
		return
	}
	if s.engine.Deps().Store == nil {
		fail(w, 503, "maintenance store is required")
		return
	}
	instances, err := s.engine.Deps().Store.ListMaintenanceActionSnapshot()
	if err != nil {
		fail(w, 500, "read maintenance history")
		return
	}
	archives, err := s.engine.Deps().Store.MaintenanceArchives()
	if err != nil {
		fail(w, 500, "read archive state")
		return
	}
	now := time.Now().UTC()
	page := operationsPage{Jobs: []map[string]any{}, Offset: offset, History: []savingsReplacement{}, Daily: []savingsDay{}, Ready: []savingsCandidate{}, ObservedAt: now.Format(time.RFC3339Nano)}
	page.Savings.Measurement = "verified_content_bytes_after_replacement_cleanup; not filesystem free space"
	records := make([]operationRecord, 0, len(instances))
	for _, inst := range instances {
		r := operationRecord{inst: inst, inputs: decodeOperationJSON(inst.InputsJSON), outputs: decodeOperationJSON(inst.OutputsJSON), state: decodeOperationJSON(inst.StateJSON)}
		r.savings = projectOperationSavings(r, now)
		records = append(records, r)
		if operationActive(inst.Status) {
			page.ActiveCount++
		}
	}
	aggregateBatchSavings(records)
	rounds := noWinnerRounds(records)
	recordsByID := make(map[string]operationRecord, len(records))
	existingReplacements := queueReplacementLinks(records)
	previewExecutions := map[string]string{}
	for _, r := range records {
		recordsByID[r.inst.ID] = r
	}
	roots, numbers, members := queueWorkflows(records, recordsByID, existingReplacements)
	for _, r := range records {
		root := roots[r.inst.ID]
		if r.inst.ActionName == "transcode_batch" && r.inst.ID != root && recordsByID[root].inputs["dry_run"] == true {
			previewExecutions[root] = r.inst.ID
		}
	}
	activeWorkflows := map[string]bool{}
	// A workflow needs a person when any of its actions waits for a decision,
	// even if its representative row shows another status: a goal batch keeps
	// encoding its other files while one of them waits for a decision.
	decisionWorkflows := map[string]bool{}
	for _, r := range records {
		if operationActive(r.inst.Status) {
			activeWorkflows[roots[r.inst.ID]] = true
		}
		// A batch paused on request waits for Resume, not for a decision about a file.
		if r.inst.Status == store.ActionStatusWaitingDecision && !(r.inst.ActionName == "transcode_batch" && operationValue(r, "paused") == true) {
			decisionWorkflows[roots[r.inst.ID]] = true
		}
	}
	page.DecisionCount = len(decisionWorkflows)
	representatives := map[string]string{}
	for root := range members {
		representatives[root] = root
		if replacement := existingReplacements[root]; replacement != "" {
			representatives[root] = replacement
		}
		if execution := previewExecutions[root]; execution != "" {
			representatives[root] = execution
		}
	}
	if grouped {
		page.ActiveCount = 0
		for _, id := range representatives {
			if operationActive(recordsByID[id].inst.Status) {
				page.ActiveCount++
			}
		}
	}
	// Oldest verified replacement wins an identical original-content claim.
	// Later transcodes of the new content have a different original digest and
	// remain distinct. Failed cleanups and candidate-only jobs are excluded.
	chronological := append([]operationRecord(nil), records...)
	sort.Slice(chronological, func(i, j int) bool {
		if chronological[i].inst.UpdatedAt == chronological[j].inst.UpdatedAt {
			return chronological[i].inst.ID < chronological[j].inst.ID
		}
		return chronological[i].inst.UpdatedAt < chronological[j].inst.UpdatedAt
	})
	replaced := map[string]bool{}
	replacedActions := map[string]bool{}
	for _, r := range chronological {
		if r.savings.RealizedSavedBytes == nil {
			continue
		}
		p := operationMap(operationValue(r, "promotion"))
		path, sha := operationString(p["original_path"]), operationString(p["original_sha256"])
		if path == "" || sha == "" {
			continue
		}
		key := path + "\x00" + sha
		replacedActions[operationString(p["transcode_action_id"])] = true
		if replaced[key] {
			continue
		}
		replaced[key] = true
		page.Savings.RealizedBytes += *r.savings.RealizedSavedBytes
		page.Savings.CompletedReplacements++
		page.History = append(page.History, savingsReplacement{ActionID: r.inst.ID, SourcePath: path, CompletedAt: r.inst.UpdatedAt, SourceBytes: *r.savings.SourceBytes, CandidateBytes: *r.savings.CandidateBytes, SavedBytes: *r.savings.RealizedSavedBytes, CumulativeBytes: page.Savings.RealizedBytes})
	}
	page.Daily = dailySavings(page.History, now, clientZone(req))
	ready := []savingsCandidate{}
	seenCandidates, seenEstimates := map[string]bool{}, map[string]bool{}
	filtered := []operationRecord{}
	for _, r := range records {
		key := operationSourceKey(r)
		if r.inst.ActionName == "transcode_media" && !replaced[key] && !replacedActions[r.inst.ID] {
			if r.inst.Status == store.ActionStatusCompleted && r.outputs["original_intact"] == true && operationValue(r, "candidate_removed") != true && r.savings.CandidateSavedBytes != nil && !seenCandidates[key] {
				page.Savings.CandidateBytes += *r.savings.CandidateSavedBytes
				seenCandidates[key] = true
				if r.savings.SourceBytes != nil && r.savings.CandidateBytes != nil {
					page.Savings.CandidateCount++
					ready = append(ready, savingsCandidate{ActionID: r.inst.ID, SourcePath: operationString(r.inputs["path"]), SourceBytes: *r.savings.SourceBytes, CandidateBytes: *r.savings.CandidateBytes, SavedBytes: *r.savings.CandidateSavedBytes})
				}
			}
			if operationActive(r.inst.Status) && r.savings.EstimatedSavedBytes != nil && !seenEstimates[key] {
				page.Savings.EstimatedBytes += *r.savings.EstimatedSavedBytes
				seenEstimates[key] = true
			}
		}
		if grouped && representatives[roots[r.inst.ID]] != r.inst.ID {
			continue
		}
		archived := archives[roots[r.inst.ID]] != "" && !activeWorkflows[roots[r.inst.ID]]
		// An archived workflow that resumes through another client must remain
		// visible while active. Direct detail links can always read its history.
		if selectedID == "" && archived != (status == "archived") {
			continue
		}
		matches := status == "" || status == "all" || status == "archived" || r.inst.Status == status || status == "active" && operationActive(r.inst.Status)
		if grouped && status == store.ActionStatusWaitingDecision {
			matches = decisionWorkflows[roots[r.inst.ID]]
		}
		if grouped && status == "completed" {
			matches = r.inst.Status == "completed" || r.inst.Status == "failed" || r.inst.Status == "cancelled"
		}
		if grouped && status == "failed" {
			matches = r.inst.Status == "failed" && !strings.Contains(r.inst.ErrorJSON, "transcode candidate rejected by user decision") && !savingsFailurePattern.MatchString(r.inst.ErrorJSON)
			if r.inst.ActionName == "transcode_batch" && r.inst.Status == "completed" {
				items, err := s.engine.Deps().Store.ListTranscodeBatchItems(r.inst.ID)
				if err != nil {
					fail(w, 500, "read batch results")
					return
				}
				for _, item := range items {
					if item.Status == "failed" && !strings.Contains(item.Error, "transcode candidate rejected by user decision") && !savingsFailurePattern.MatchString(item.Error) {
						matches = true
						break
					}
				}
			}
		}
		if (selectedID == "" || selectedID == r.inst.ID) && matches {
			filtered = append(filtered, r)
		}
	}
	if grouped {
		// One fixed order: the newest submission first, by workflow number. It
		// survives polling, state changes and a conversion becoming a
		// replacement, and it never moves a row because a file in it started or
		// stopped waiting. Work that waits on a person is flagged on its row,
		// counted, and found by the Needs decision filter.
		sort.SliceStable(filtered, func(i, j int) bool {
			return numbers[roots[filtered[i].inst.ID]] > numbers[roots[filtered[j].inst.ID]]
		})
	}
	page.Savings.EstimatedSavedBytes = page.Savings.EstimatedBytes
	page.Savings.CandidateSavedBytes = page.Savings.CandidateBytes
	page.Total = len(filtered)
	end := min(offset, len(filtered)) + min(limit, max(0, len(filtered)-offset))
	page.HasMore = end < len(filtered)
	if offset < len(filtered) {
		for _, r := range filtered[offset:end] {
			if err := req.Context().Err(); err != nil {
				return
			}
			t := s.mcp.GetTool("action_status")
			if t == nil {
				fail(w, 503, "action_status is not configured")
				return
			}
			res, err := t.Handler(req.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "action_status", Arguments: map[string]any{"id": r.inst.ID}}})
			if err != nil || res == nil || res.IsError {
				fail(w, 500, "read action status")
				return
			}
			var summary map[string]any
			for _, content := range res.Content {
				if text, ok := content.(mcp.TextContent); ok {
					_ = json.Unmarshal([]byte(text.Text), &summary)
					break
				}
			}
			job := operationMap(summary["action"])
			if job == nil {
				fail(w, 500, "invalid action status projection")
				return
			}
			job["savings"] = r.savings
			if endedWithoutResult(r) && !rerunHasResult(r, existingReplacements) && !remedyTaken(r, records) {
				if inst, err := s.engine.Deps().Store.GetActionInstanceIfExists(r.inst.ID); err == nil && inst != nil {
					full := operationRecord{inst: *inst, inputs: decodeOperationJSON(inst.InputsJSON), outputs: decodeOperationJSON(inst.OutputsJSON), state: decodeOperationJSON(inst.StateJSON)}
					job["explanation"] = explainResult(full)
					// A batch file takes its steps from its batch row, which knows
					// whether this job is still the file's latest attempt.
					if operationString(full.inputs["parent_action_id"]) == "" {
						steps := nextStepsFor(full)
						if s.rerunRefusal(full) != "" {
							// Its own settings cannot run again; a new goal still can.
							steps = slices.DeleteFunc(steps, func(st nextStep) bool { return st.Inputs == nil && st.ID != "tracks" })
						}
						job["next_steps"] = steps
					}
				}
			}
			if failedFileJob(r) {
				if inst, err := s.engine.Deps().Store.GetActionInstanceIfExists(r.inst.ID); err == nil && inst != nil {
					full := operationRecord{inst: *inst, inputs: decodeOperationJSON(inst.InputsJSON), outputs: decodeOperationJSON(inst.OutputsJSON), state: decodeOperationJSON(inst.StateJSON)}
					e, blocked, steps := explainFailure(full)
					job["explanation"] = e
					// A batch file is run again from its batch row.
					if blocked != "" && operationString(full.inputs["parent_action_id"]) == "" {
						job["retry_blocked"] = blocked
						job["next_steps"] = append([]nextStep{}, steps...)
					}
				}
			}
			if rerunEligible(r) && !rerunHasResult(r, existingReplacements) && !remedyTaken(r, records) {
				// The snapshot keeps only the keys the queue lists, so whether
				// running it again would be refused, and what its ladder offers,
				// is read from the job's full record, as the rerun itself does.
				// A row whose record cannot be read is listed without an offer:
				// one bad row never fails the page.
				if inst, err := s.engine.Deps().Store.GetActionInstanceIfExists(r.inst.ID); err == nil && inst != nil {
					full := operationRecord{inst: *inst, inputs: decodeOperationJSON(inst.InputsJSON), outputs: decodeOperationJSON(inst.OutputsJSON), state: decodeOperationJSON(inst.StateJSON)}
					if why := s.rerunRefusal(full); why != "" {
						// Running it again would be refused; the goal form is
						// offered instead, with the reason.
						job["rerun_refused"] = why
					} else {
						job["rerun"] = rerunSummary(full, rounds)
					}
				}
			}
			if size := operationSizeMode(r); size != nil {
				job["size_mode"] = size
			}
			if outcome := operationSizeBudgetOutcome(r); outcome != "" {
				job["size_budget_outcome"] = outcome
			}
			if tracks := operationTracks(r); tracks != nil {
				// The queue lists the sentence; the lists of tracks belong to the
				// one job being opened.
				job["tracks_message"] = operationString(tracks["message"])
				if selectedID != "" {
					job["tracks"] = tracks
					// What Choose tracks needs to run the same goal again.
					if goal := operationGoal(r); goal != nil {
						job["goal"] = goal
					}
				}
			}
			root := roots[r.inst.ID]
			job["workflow_id"] = root
			job["archive_id"] = root
			job["needs_decision"] = decisionWorkflows[root]
			job["can_archive"] = !activeWorkflows[root]
			job["archived"] = archives[root] != "" && !activeWorkflows[root]
			if r.inst.ID != root && recordsByID[root].inputs["dry_run"] == true {
				job["preview_action_id"] = root
			}
			job["number"] = numbers[root]
			job["stages"] = s.queueStages(r)
			// Planning can hash large files before a promotion checkpoint exists.
			// Its durable source link already identifies the file during that stage.
			if r.inst.ActionName == "promote_transcode_candidate" {
				if original, ok := recordsByID[root]; ok && original.inst.ActionName == "transcode_media" {
					path := operationString(original.inputs["path"])
					if resolved := operationString(original.state["resolved_path"]); resolved != "" {
						path = resolved
					}
					job["source_path"] = path
					if outcome := operationSizeBudgetOutcome(original); outcome != "" {
						job["size_budget_outcome"] = outcome
					}
					if r.savings.SourceBytes == nil {
						job["savings"] = original.savings
					}
				}
			}
			if r.inst.ActionName == "promote_transcode_candidate" {
				// What the person decides on, and where the files end up, come
				// from the transcode job being replaced and this job's record.
				if source, ok := recordsByID[operationString(r.inputs["transcode_action_id"])]; ok && source.inst.ActionName == "transcode_media" {
					job["replacement_facts"] = replacementFactsOf(source)
				}
				if where := whereOf(r); where != nil {
					job["where"] = where
				}
			}
			if recordsByID[root].inst.ActionName != "transcode_batch" && len(members[root]) > 1 {
				related := []map[string]any{}
				for _, id := range []string{root, existingReplacements[root]} {
					if member, ok := recordsByID[id]; ok {
						related = append(related, map[string]any{"id": id, "action_name": member.inst.ActionName, "status": member.inst.Status, "created_at": member.inst.CreatedAt, "stages": s.queueStages(member)})
					}
				}
				job["workflow_actions"] = related
			}
			job["candidate_ready"] = r.inst.ActionName == "transcode_media" && r.inst.Status == store.ActionStatusCompleted && operationValue(r, "skip_transcode") != true && operationValue(r, "original_intact") == true && operationValue(r, "candidate_removed") != true && r.savings.CandidateBytes != nil && !replacedActions[r.inst.ID] && !replaced[operationSourceKey(r)]
			if cleanup := operationMap(operationValue(r, "candidate_cleanup")); cleanup != nil {
				job["candidate_cleanup"] = cleanup
			}
			if parent, found := recordsByID[operationString(r.inputs["parent_action_id"])]; found && parent.inst.ActionName == "transcode_batch" {
				job["parent_batch_id"] = parent.inst.ID
			}
			if r.inst.ActionName == "transcode_media" {
				job["replaced"] = replacedActions[r.inst.ID] || replaced[operationSourceKey(r)]
			}
			if r.inst.ActionName == "transcode_media" {
				if parent := recordsByID[operationString(r.inputs["parent_action_id"])]; parent.inst.ActionName == "transcode_batch" && parent.inputs["dry_run"] != true && parent.state["batch_promotion_plan"] == nil {
					items, err := s.engine.Deps().Store.ListTranscodeBatchItems(parent.inst.ID)
					if err != nil {
						fail(w, 500, "read batch file selection")
						return
					}
					for _, item := range items {
						if item.ChildActionID == r.inst.ID && item.FilePath == operationString(r.inputs["path"]) {
							reserved, err := s.engine.BatchItemReplacementReserved(item)
							if err != nil {
								fail(w, 500, "read file replacement")
								return
							}
							if operationMap(parent.state["batch_retry_pending"])[item.ItemKey] == true {
								job["candidate_ready"] = false
							}
							if !reserved {
								job["parent_item_key"] = item.ItemKey
							}
							break
						}
					}
				}
				// A candidate in the batch's prepared replacement plan is replaced
				// by that plan; it offers no replacement of its own.
				if parent := recordsByID[operationString(r.inputs["parent_action_id"])]; parent.inst.ActionName == "transcode_batch" && existingReplacements[r.inst.ID] == "" {
					if plan := replacementPlanState(parent.state, r.inst.ID); plan != "" {
						job["replacement_plan"], job["candidate_ready"] = plan, false
					}
				}
				if context := operationReplacementContext(r, recordsByID); context != nil {
					job["replacement_context"] = context
				}
				if existing := existingReplacements[r.inst.ID]; existing != "" {
					job["replacement_action_id"] = existing
				}
			}
			_, httpWorker := s.engine.ActionWorkerExecutor(r.state).(*httpclient.HTTPExecutor)
			if worker := operationString(r.state["worker"]); worker != "" {
				job["worker"], job["worker_name"] = worker, worker
			}
			job["logs_available"] = httpWorker && (operationString(r.state["job_id"]) != "" || operationString(r.state["transcode_job_id"]) != "")
			if r.inst.ActionName == "transcode_batch" {
				if r.inputs["dry_run"] == true && r.inst.Status == store.ActionStatusCompleted {
					job["preview_execution_action_id"] = previewExecutions[r.inst.ID]
				}
				job["replacement_requested"] = r.inputs["promote_candidates"] == true || r.state["post_batch_promotion"] == true
				job["settings_revision"] = r.state["batch_settings_revision"]
				job["paused"] = operationValue(r, "paused") == true
				items, err := s.engine.Deps().Store.ListTranscodeBatchItems(r.inst.ID)
				inferred := false
				if err == nil {
					items, inferred, err = s.previewPendingItems(r.inst, items)
				}
				if err != nil {
					fail(w, 500, "read batch contents")
					return
				}
				job["batch_files"] = batchQueueFeedback(items, s.cfg.Media.AllowedReadRoots)
				if progress := s.batchReplacementProgress(r, recordsByID, existingReplacements); progress != nil {
					job["replacement"] = progress
				}
				remaining, replacedFiles := 0, 0
				for _, item := range items {
					child := recordsByID[item.ChildActionID]
					if replacement := recordsByID[existingReplacements[item.ChildActionID]]; replacement.inst.Status == "completed" && replacement.savings.RealizedSavedBytes != nil {
						replacedFiles++
					}
					if item.Status == "completed" && item.Decision == "transcode" && child.inst.Status == "completed" && child.outputs["original_intact"] == true && item.CandidatePath != "" && existingReplacements[item.ChildActionID] == "" {
						remaining++
					}
				}
				job["remaining_candidates"] = remaining
				job["replaced_files"] = replacedFiles
				// A file handed to a worker counts as running in the batch, but one
				// that is still in the worker's line is not being encoded yet.
				if queued := queuedOnWorker(items, recordsByID); queued > 0 {
					if batch := operationMap(job["batch"]); batch != nil {
						batch["queued_on_worker"] = queued
					}
				}
				if inferred {
					batch := operationMap(job["batch"])
					if batch == nil {
						batch = map[string]any{}
					}
					batch["total"] = len(items)
					batch[items[0].Status] = len(items)
					job["batch"] = batch
				}
				if operationActive(r.inst.Status) {
					activities := []map[string]any{}
					for _, child := range members[root] {
						if child.inst.ID == r.inst.ID || (child.inst.Status != store.ActionStatusRunning && child.inst.Status != store.ActionStatusWaitingExternal) {
							continue
						}
						res, err := t.Handler(req.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "action_status", Arguments: map[string]any{"id": child.inst.ID}}})
						if err == nil && res != nil && !res.IsError {
							var data map[string]any
							for _, content := range res.Content {
								if text, ok := content.(mcp.TextContent); ok {
									_ = json.Unmarshal([]byte(text.Text), &data)
									break
								}
							}
							childJob := operationMap(data["action"])
							file := operationString(child.inputs["path"])
							if file == "" {
								file = operationString(operationMap(operationValue(child, "promotion"))["original_path"])
							}
							activities = append(activities, map[string]any{"id": child.inst.ID, "parent_action_id": child.inputs["parent_action_id"], "action_name": child.inst.ActionName, "current_step": child.inst.CurrentStep, "stages": s.queueStages(child), "file": file, "worker": childJob["worker"], "work": childJob["work"], "waiting_condition": childJob["waiting_condition"], "eta_seconds": child.savings.ETASeconds})
							job["activity_waiting_condition"] = childJob["waiting_condition"]
							if worker := operationMap(childJob["worker"]); worker != nil {
								job["worker"] = worker
								if name := operationString(child.state["worker"]); name != "" {
									job["worker_name"] = name
								}
								job["activity_file"] = operationString(child.inputs["path"])
							}
						}
					}
					job["activities"] = activities
				}
			}
			page.Jobs = append(page.Jobs, job)
		}
	}
	page.Ready = largestCandidates(ready)
	if len(page.History) > 25 {
		page.History = page.History[len(page.History)-25:]
	}
	writeJSON(w, http.StatusOK, page)
}
