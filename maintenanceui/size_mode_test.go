package maintenanceui

import (
	"encoding/json"
	"testing"
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
