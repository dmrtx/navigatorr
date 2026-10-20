package maintenanceui

import (
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/transcode/optimization"
)

func TestOperationsProjectSizeModeSummary(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	state := map[string]any{
		"mode":               "size",
		"size_main10":        true,
		"original":           map[string]any{"size_bytes": 9000},
		"benchmark_decision": map[string]any{"winner": map[string]any{"estimated_bytes": 4000}},
		"quality_evidence":   map[string]any{"verdict": "pass", "vmaf": map[string]any{"mean": 93.4, "p5": 89.1, "worst_window_mean": 88.2, "worst_window_seconds": 10}},
		"conversions": []any{
			map[string]any{"stream_type": "audio", "stream_index": 0, "from_codec": "dts", "to_codec": "aac"},
			map[string]any{"stream_type": "subtitle", "stream_index": 0, "from_codec": "mov_text", "to_codec": "srt"},
			map[string]any{"stream_type": "audio", "stream_index": 2, "from_codec": "pcm_s24le", "to_codec": "aac"},
		},
	}
	seedOperation(t, st, "size-job", "transcode_media", "completed", map[string]any{"path": "/media/series/Show/S01E01.mkv", "mode": "size"}, nil, state)
	seedOperation(t, st, "quality-job", "transcode_media", "completed", map[string]any{"path": "/media/series/Show/S01E02.mkv", "mode": "quality"}, nil, map[string]any{"mode": "quality", "quality_evidence": state["quality_evidence"], "conversions": state["conversions"]})

	read := func(id string) map[string]any {
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		var page struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		return page.Jobs[0]
	}
	job := read("size-job")
	size := operationMap(job["size_mode"])
	if size["expected_bytes"] != float64(4000) || size["main10"] != true {
		t.Fatalf("expected size and main10 missing: %v", size)
	}
	final := operationMap(size["final_quality"])
	if final["vmaf_mean"] != 93.4 || final["vmaf_p5"] != 89.1 || final["worst_window_mean"] != 88.2 || final["worst_window_seconds"] != float64(10) {
		t.Fatalf("final quality wrong: %v", final)
	}
	audio, _ := size["compacted_audio"].([]any)
	if len(audio) != 2 || operationMap(audio[0])["from_codec"] != "dts" || operationMap(audio[1])["stream_index"] != float64(2) {
		t.Fatalf("compacted audio wrong: %v", size["compacted_audio"])
	}
	if got, ok := read("quality-job")["size_mode"]; ok {
		t.Fatalf("other modes carry no size summary: %v", got)
	}
}

func TestOperationsProjectSizeHardwareContinuation(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	continuation := map[string]any{
		"outcome":             "continued",
		"message":             "The VideoToolbox result failed the final quality check; continued with x265.",
		"failed_candidate_id": "fallback-hardware-cand_q70",
		"final": map[string]any{
			"verdict":       "fail",
			"failed_checks": []any{"quality_vmaf_p5_below_minimum"},
			"failures":      []any{"VMAF p5 80.2 is below the minimum 84.0"},
			"measured":      map[string]any{"vmaf_mean": 93.0, "vmaf_p5": 80.2, "vmaf_worst_window": 88.1},
			"limits":        map[string]any{"worst_window_seconds": 10},
		},
		"windows": []any{map[string]any{"start_seconds": 600.0, "duration_seconds": 10.0, "role": "discovery",
			"search": map[string]any{"vmaf_mean": 93.0}, "final": map[string]any{"vmaf_mean": 84.0}}},
	}
	state := map[string]any{
		"mode":                  "size",
		"original":              map[string]any{"size_bytes": 9000},
		"benchmark_decision":    map[string]any{"winner": map[string]any{"video_codec": "libx265", "estimated_bytes": 4000}},
		"hardware_continuation": continuation,
		"quality_evidence":      map[string]any{"verdict": "pass", "vmaf": map[string]any{"mean": 94.0, "p5": 88.0}},
	}
	seedOperation(t, st, "continued-job", "transcode_media", "completed", map[string]any{"path": "/media/series/Show/S01E03.mkv", "mode": "size"}, nil, state)
	rejected := map[string]any{"mode": "size", "original": state["original"], "hardware_continuation": continuation,
		"quality_evidence": map[string]any{"verdict": "fail", "reason_codes": []any{"quality_vmaf_p5_below_minimum"}, "vmaf": map[string]any{"mean": 91.0, "p5": 82.0}}}
	seedOperation(t, st, "rejected-job", "transcode_media", "completed", map[string]any{"path": "/media/series/Show/S01E04.mkv", "mode": "size"}, nil, rejected)
	read := func(id string) map[string]any {
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		var page struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		return operationMap(page.Jobs[0]["size_mode"])
	}
	size := read("continued-job")
	note := operationMap(size["hardware_continuation"])
	failed := operationMap(note["failed_quality"])
	if note["outcome"] != "continued" || failed["vmaf_p5"] != 80.2 || failed["worst_window_mean"] != 88.1 || failed["worst_window_seconds"] != float64(10) || len(failed["failures"].([]any)) != 1 {
		t.Fatalf("failed hardware attempt not kept: %v / %v", note, size)
	}
	windows, _ := note["windows"].([]any)
	if len(windows) != 1 || operationMap(operationMap(windows[0])["search"])["vmaf_mean"] != 93.0 || operationMap(operationMap(windows[0])["final"])["vmaf_mean"] != 84.0 {
		t.Fatalf("the failed attempt's window scores were not kept: %v", note)
	}
	if size["encoder"] != "libx265" || operationMap(size["final_quality"])["vmaf_p5"] != 88.0 {
		t.Fatalf("x265 result must show beside the failed attempt: %v", size)
	}
	checks, _ := operationMap(read("rejected-job")["final_quality"])["failed_checks"].([]any)
	if len(checks) != 1 || checks[0] != "quality_vmaf_p5_below_minimum" {
		t.Fatalf("rejected result must name the failed metric: %v", checks)
	}
}

func TestOperationsProjectSizeAudioOnly(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	state := map[string]any{
		"mode":                       "size",
		"size_audio_only":            true,
		"plan_source":                "audio_only",
		"original":                   map[string]any{"size_bytes": 9000},
		"audio_only_estimated_bytes": 6500,
		"benchmark_decision":         map[string]any{"winner": nil},
		"quality_evidence":           map[string]any{"verdict": "pass", "video_copy": map[string]any{"verdict": "pass", "source_hash": "sha256:v", "candidate_hash": "sha256:v"}},
		"conversions":                []any{map[string]any{"stream_type": "audio", "stream_index": 0, "from_codec": "truehd", "to_codec": "aac"}},
	}
	seedOperation(t, st, "audio-job", "transcode_media", "completed", map[string]any{"path": "/media/movies/Movie/Movie.mkv", "mode": "size"}, nil, state)
	w := request(h, "GET", "/api/maintenance/operations?id=audio-job", "", true)
	var page struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	size := operationMap(page.Jobs[0]["size_mode"])
	if size["expected_bytes"] != float64(6500) || size["video_copy"] != "verified" || size["main10"] != nil || size["final_quality"] != nil {
		t.Fatalf("audio-only summary wrong: %v", size)
	}
	if audio, _ := size["compacted_audio"].([]any); len(audio) != 1 || operationMap(audio[0])["from_codec"] != "truehd" {
		t.Fatalf("compacted audio wrong: %v", size["compacted_audio"])
	}
}

func TestOperationsProjectSizeAudioOnlyCheckStates(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	for id, evidence := range map[string]any{
		"failed":  map[string]any{"verdict": "fail", "video_copy": map[string]any{"verdict": "fail", "reason_codes": []any{"video_copy_packets_changed"}}},
		"pending": nil,
	} {
		state := map[string]any{"mode": "size", "size_audio_only": true, "plan_source": "audio_only", "original": map[string]any{"size_bytes": 9000}}
		if evidence != nil {
			state["quality_evidence"] = evidence
		}
		seedOperation(t, st, id, "transcode_media", "completed", map[string]any{"path": "/media/movies/Movie/Movie.mkv", "mode": "size"}, nil, state)
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		var page struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		if got := operationMap(page.Jobs[0]["size_mode"])["video_copy"]; got != id {
			t.Fatalf("%s: video copy state %v", id, got)
		}
	}
}

// The encoder of the winning settings reaches the page, and a VideoToolbox
// winner is flagged as hardware so it can be told apart before replacing.
func TestOperationsProjectWinningEncoder(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	note := optimization.HardwareSelectedNote + ": 4000 bytes against 3800 for the smallest software candidate, within the 15% margin"
	for id, codec := range map[string]string{"vt-job": "hevc_videotoolbox", "vt-plain-job": "hevc_videotoolbox", "x265-job": "libx265"} {
		uncertainties := []any{"sampled estimate"}
		if id != "vt-plain-job" {
			uncertainties = append(uncertainties, note)
		}
		seedOperation(t, st, id, "transcode_media", "completed", map[string]any{"path": "/media/series/Show/" + id + ".mkv", "mode": "size"}, nil, map[string]any{
			"mode":               "size",
			"original":           map[string]any{"size_bytes": 9000},
			"benchmark_decision": map[string]any{"winner": map[string]any{"estimated_bytes": 4000, "video_codec": codec, "candidate_id": id, "uncertainties": uncertainties}},
		})
	}
	read := func(id string) map[string]any {
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		var page struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		return operationMap(page.Jobs[0]["size_mode"])
	}
	if vt := read("vt-job"); vt["encoder"] != "hevc_videotoolbox" || vt["hardware"] != true {
		t.Fatalf("media engine winner not flagged: %v", vt)
	} else if vt["hardware_reason"] != "4000 bytes against 3800 for the smallest software candidate, within the 15% margin" {
		t.Fatalf("media engine reason not projected: %v", vt)
	}
	if plain := read("vt-plain-job"); plain["hardware"] != true || plain["hardware_reason"] != nil {
		t.Fatalf("reason invented for a winner without one: %v", plain)
	}
	if soft := read("x265-job"); soft["encoder"] != "libx265" || soft["hardware"] != nil || soft["hardware_reason"] != nil {
		t.Fatalf("software winner flagged: %v", soft)
	}
}

// The job detail carries how the winner was verified and each window's search
// and final scores side by side, including the out-of-sample pool.
func TestOperationsProjectWindowReport(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "grid-job", "transcode_media", "completed", map[string]any{"path": "/media/series/Show/S01E01.mkv", "mode": "size"}, nil, map[string]any{
		"mode":     "size",
		"original": map[string]any{"size_bytes": 9000},
		"automatic_search": map[string]any{
			"verification_windows": "final_grid",
			"verification": map[string]any{"attempts": []any{
				map[string]any{"candidate_id": "a", "role": "smallest", "windows": 5, "outcome": "quality_failed"},
				map[string]any{"candidate_id": "b", "role": "safest", "windows": 5, "outcome": "passed"},
			}},
			"winner_windows": []any{map[string]any{"start_seconds": 600, "duration_seconds": 10, "role": "discovery", "vmaf_mean": 93, "vmaf_p5": 90, "vmaf_min": 88}},
		},
		"quality_evidence": map[string]any{
			"verdict":    "pass",
			"vmaf":       map[string]any{"mean": 91.0, "p5": 88.0},
			"fresh_vmaf": map[string]any{"mean": 89.5, "p5": 86.0},
			"windows": []any{
				map[string]any{"start_seconds": 600, "duration_seconds": 10, "role": "grid", "vmaf_mean": 91, "vmaf_p5": 88, "vmaf_min": 85},
				map[string]any{"start_seconds": 1200, "duration_seconds": 10, "role": "fresh", "vmaf_mean": 89, "vmaf_p5": 86, "vmaf_min": 83},
			},
		},
	})
	w := request(h, "GET", "/api/maintenance/operations?id=grid-job", "", true)
	var page struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	size := operationMap(page.Jobs[0]["size_mode"])
	verification := operationMap(size["verification"])
	if attempts, _ := verification["attempts"].([]any); verification["windows"] != "final_grid" || len(attempts) != 2 || verification["stopped_by"] != nil {
		t.Fatalf("verification wrong: %v", verification)
	}
	windows, _ := size["windows"].([]any)
	if len(windows) != 2 {
		t.Fatalf("windows wrong: %v", size["windows"])
	}
	first, second := operationMap(windows[0]), operationMap(windows[1])
	if operationMap(first["search"])["vmaf_mean"] != float64(93) || operationMap(first["final"])["vmaf_mean"] != float64(91) || first["role"] != "discovery" {
		t.Fatalf("first window wrong: %v", first)
	}
	if second["role"] != "fresh" || second["search"] != nil || operationMap(second["final"])["vmaf_mean"] != float64(89) {
		t.Fatalf("fresh window wrong: %v", second)
	}
	if final := operationMap(size["final_quality"]); final["fresh_vmaf_mean"] != 89.5 || final["fresh_vmaf_p5"] != float64(86) {
		t.Fatalf("fresh pool wrong: %v", final)
	}
}

// A size budget replaces the search's outcome: the encoder is x265, the
// expected size is the budget encode's, never an over-budget winner's, and the
// budget block carries the curve and the outcome.
func TestOperationsProjectSizeBudget(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	budget := map[string]any{
		"trigger": "passing_candidate_over_budget", "source_bytes": 9000, "budget_bytes": 3000, "budget_percent": 33.0, "target_bytes": 2910, "non_video_bytes": 400, "video_bitrate_kbps": 2400,
		"over_budget_candidate_id": "cand_crf24:medium", "over_budget_estimated_bytes": 4000,
		"quality_target": map[string]any{"vmaf_mean_minimum": 88.0}, "quality_floor": map[string]any{"vmaf_mean_minimum": 70.0},
		"curve": []any{
			map[string]any{"kind": "searched", "candidate_id": "cand_crf24:medium", "bytes": 4000, "passed": true, "within_budget": false, "vmaf_mean": 90.0},
			map[string]any{"kind": "budget_fallback", "bytes": 2910, "within_budget": true},
		},
		"message":  "Encoded once at the budget.",
		"siblings": map[string]any{"scope": "season", "median_bitrate_kbps": 1600.0, "multiplier": 1.5, "count": 5, "resolution_class": "1080p", "source_bitrate_kbps": 4800.0, "ratio": 3.0, "files": []any{map[string]any{"name": "E02.mkv", "bytes": 700000000, "duration_seconds": 2600.0, "bitrate_kbps": 1600.0, "width": 1920, "height": 1080}}},
	}
	state := map[string]any{
		"mode": "size", "plan_source": "size_budget", "original": map[string]any{"size_bytes": 9000},
		"benchmark_decision": map[string]any{"winner": map[string]any{"video_codec": "hevc_videotoolbox", "estimated_bytes": 4000}},
		"size_budget":        budget,
	}
	seedOperation(t, st, "budget-job", "transcode_media", "waiting_decision", map[string]any{"path": "/media/series/Show/S01E05.mkv", "mode": "size"}, nil, state)
	done := map[string]any{}
	for k, v := range budget {
		done[k] = v
	}
	done["candidate_bytes"], done["outcome"] = 2850, "below_quality_target_within_budget"
	done["final"] = map[string]any{"verdict": "pass", "measured": map[string]any{"vmaf_mean": 79.5}, "limits": map[string]any{"vmaf_mean_minimum": 70.0}}
	finished := map[string]any{"mode": "size", "plan_source": "size_budget", "original": map[string]any{"size_bytes": 9000}, "size_budget": done}
	seedOperation(t, st, "budget-done", "transcode_media", "completed", map[string]any{"path": "/media/series/Show/S01E06.mkv", "mode": "size"}, nil, finished)
	read := func(id string) map[string]any {
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		var page struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		return operationMap(page.Jobs[0]["size_mode"])
	}
	planned := read("budget-job")
	if planned["encoder"] != "libx265" || planned["hardware"] != nil || planned["expected_bytes"] != float64(2910) {
		t.Fatalf("a planned budget encode must not report the over-budget winner: %v", planned)
	}
	block := operationMap(planned["size_budget"])
	if block["budget_bytes"] != float64(3000) || block["target_bytes"] != float64(2910) || block["video_bitrate_kbps"] != float64(2400) || block["over_budget_candidate_id"] != "cand_crf24:medium" || block["message"] != "Encoded once at the budget." {
		t.Fatalf("budget block wrong: %v", block)
	}
	if sib := operationMap(block["siblings"]); sib["scope"] != "season" || sib["count"] != float64(5) || sib["ratio"] != 3.0 || len(sib["files"].([]any)) != 1 {
		t.Fatalf("sibling basis missing: %v", block["siblings"])
	}
	if curve, _ := block["curve"].([]any); len(curve) != 2 || operationMap(curve[1])["kind"] != "budget_fallback" {
		t.Fatalf("curve missing: %v", block["curve"])
	}
	result := read("budget-done")
	if block := operationMap(result["size_budget"]); block["outcome"] != "below_quality_target_within_budget" || operationMap(operationMap(block["final"])["measured"])["vmaf_mean"] != 79.5 || result["expected_bytes"] != float64(2850) {
		t.Fatalf("finished budget encode wrong: %v", result)
	}
	// A frozen budget the search's outcome stood without says why.
	seedOperation(t, st, "budget-declined", "transcode_media", "completed", map[string]any{"path": "/media/series/Show/S01E07.mkv", "mode": "size"}, nil, map[string]any{"mode": "size", "original": map[string]any{"size_bytes": 9000}, "size_budget_skip_message": "Not applied."})
	seedOperation(t, st, "budget-siblings", "transcode_media", "completed", map[string]any{"path": "/media/series/Show/S01E08.mkv", "mode": "size"}, nil, map[string]any{"mode": "size", "original": map[string]any{"size_bytes": 9000}, "size_budget_siblings": map[string]any{"reason": "too_few_siblings", "message": "Only 1 sibling."}})
	if got := read("budget-declined"); got["size_budget_note"] != "Not applied." || got["size_budget"] != nil {
		t.Fatalf("declined budget note: %v", got)
	}
	if got := read("budget-siblings"); got["size_budget_note"] != "Only 1 sibling." {
		t.Fatalf("sibling reason note: %v", got)
	}
	if got := read("budget-done"); got["size_budget_note"] != nil {
		t.Fatalf("note shown beside a budget encode: %v", got)
	}
}
