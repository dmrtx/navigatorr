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
	state["outcome_library"] = map[string]any{"note": "This exact source already passed final validation.", "reused_from": "earlier", "matches": []any{map[string]any{"action_id": "earlier"}}}
	seedOperation(t, st, "reused-job", "transcode_media", "completed", map[string]any{"path": "/media/series/Show/S01E03.mkv", "mode": "size"}, nil, state)
	library := operationMap(operationMap(read("reused-job")["size_mode"])["library"])
	if library["note"] == nil || library["reused_from"] != "earlier" || library["matches"] != nil {
		t.Fatalf("library summary: %v", library)
	}
	state["warm_start"] = map[string]any{"quality_value": 26, "basis": "series_season", "siblings": 3, "from_actions": []any{"a1"}, "note": "Search starts near CRF 26."}
	state["automatic_search"] = map[string]any{"warm_start": map[string]any{"quality_value": 26, "start": "cand_crf26", "skipped": []any{"cand_crf24", "cand_crf30"}}}
	seedOperation(t, st, "warm-job", "transcode_media", "completed", map[string]any{"path": "/media/series/Show/S01E04.mkv", "mode": "size"}, nil, state)
	warm := operationMap(operationMap(read("warm-job")["size_mode"])["warm_start"])
	if warm["quality_value"] != float64(26) || warm["start"] != "cand_crf26" || len(warm["skipped"].([]any)) != 2 || warm["from_actions"] != nil {
		t.Fatalf("warm start summary: %v", warm)
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
