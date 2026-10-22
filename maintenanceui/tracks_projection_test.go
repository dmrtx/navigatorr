package maintenanceui

import (
	"encoding/json"
	"testing"
)

func TestOperationsProjectTheTrackReport(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	report := map[string]any{
		"audio":       map[string]any{"total": 2, "kept": []any{map[string]any{"index": 1, "language": "und", "codec": "aac"}}, "dropped": []any{map[string]any{"index": 2, "language": "fre", "codec": "ac3"}}},
		"subtitles":   map[string]any{"total": 0, "kept": []any{}, "dropped": []any{}},
		"bytes_saved": 1200,
		"languages":   []any{"eng", "spa"},
		"message":     "Kept 1 of 2 audio tracks (und); dropping the rest saves about 1.2 KB",
	}
	// The final validation's report is what a finished job shows; while the
	// job runs, the one the preflight stored is.
	seedOperation(t, st, "final-report", "transcode_media", "completed", map[string]any{"path": "/media/series/Sample/Sample Episode 1.mkv", "mode": "size"}, map[string]any{"tracks": report, "tracks_message": report["message"]}, map[string]any{"mode": "size"})
	seedOperation(t, st, "preflight-report", "transcode_media", "running", map[string]any{"path": "/media/series/Sample/Sample Episode 2.mkv", "mode": "size"}, nil, map[string]any{"mode": "size", "track_report": report})
	seedOperation(t, st, "no-report", "transcode_media", "completed", map[string]any{"path": "/media/series/Sample/Sample Episode 3.mkv", "mode": "size"}, nil, map[string]any{"mode": "size"})

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
	for _, id := range []string{"final-report", "preflight-report"} {
		job := read(id)
		if job["tracks_message"] != report["message"] || operationNumber(operationMap(job["tracks"])["bytes_saved"]) != 1200 {
			t.Fatalf("%s: %v", id, job)
		}
		audio := operationMap(operationMap(job["tracks"])["audio"])
		if kept, _ := audio["kept"].([]any); len(kept) != 1 || operationMap(kept[0])["language"] != "und" {
			t.Fatalf("%s: kept audio %v", id, audio)
		}
	}
	// The queue carries the sentence only; the lists come with the one job.
	w := request(h, "GET", "/api/maintenance/operations", "", true)
	var queue struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &queue) != nil || len(queue.Jobs) != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, job := range queue.Jobs {
		if _, has := job["tracks"]; has {
			t.Fatalf("the queue row carries the lists: %v", job["id"])
		}
		if (job["id"] != "no-report") != (job["tracks_message"] == report["message"]) {
			t.Fatalf("%v: %v", job["id"], job["tracks_message"])
		}
	}
	if job := read("no-report"); job["tracks"] != nil || job["tracks_message"] != nil {
		t.Fatalf("a job that kept every track reports none: %v", job)
	}
}

// Choose tracks starts a new job with the goal the finished one ran, so the
// job's detail carries the settings that make up that goal and nothing else
// of its inputs.
func TestJobDetailCarriesTheGoalSettings(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	report := map[string]any{
		"audio":     map[string]any{"total": 1, "kept": []any{map[string]any{"index": 1, "language": "und", "codec": "aac"}}, "dropped": []any{}},
		"subtitles": map[string]any{"total": 0, "kept": []any{}, "dropped": []any{}},
		"message":   "Kept 1 of 1 audio tracks (und)",
	}
	seedOperation(t, st, "size-goal", "transcode_media", "completed", map[string]any{
		"path": "/media/series/Sample/Sample Episode 1.mkv", "mode": "size", "allow_hardware": false, "allow_slow_preset": true,
		"size_budget_source": "siblings", "size_budget_sibling_multiplier": 1.8, "media_type": "tv", "is_anime": true,
		"library_context": map[string]any{"service": "sonarr", "id": 7}, "keep_languages": []any{"eng"}, "idempotency": "receipt",
	}, map[string]any{"tracks": report}, map[string]any{"mode": "size"})
	seedOperation(t, st, "compact-goal", "transcode_media", "completed", map[string]any{"path": "/media/movies/Sample.mkv", "mode": "compact", "max_height": 720}, map[string]any{"tracks": report}, map[string]any{"mode": "compact"})
	seedOperation(t, st, "quality-goal", "transcode_media", "completed", map[string]any{"path": "/media/movies/Other.mkv", "mode": "quality"}, map[string]any{"tracks": report}, map[string]any{"mode": "quality"})
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
	goal := operationMap(read("size-goal")["goal"])
	want := map[string]any{"mode": "size", "allow_hardware": false, "allow_slow_preset": true, "size_budget_source": "siblings", "size_budget_sibling_multiplier": 1.8, "media_type": "tv", "is_anime": true}
	if len(goal) != len(want) {
		t.Fatalf("goal %v", goal)
	}
	for key, value := range want {
		if goal[key] != value {
			t.Fatalf("%s: %v in %v", key, goal[key], goal)
		}
	}
	if goal := operationMap(read("compact-goal")["goal"]); operationNumber(goal["max_height"]) != 720 || goal["mode"] != "compact" {
		t.Fatalf("goal %v", goal)
	}
	if job := read("quality-goal"); job["goal"] != nil {
		t.Fatalf("a goal that keeps every track has no track choice to repeat: %v", job["goal"])
	}
	w := request(h, "GET", "/api/maintenance/operations", "", true)
	var queue struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &queue) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, job := range queue.Jobs {
		if _, has := job["goal"]; has {
			t.Fatalf("the queue row carries the goal: %v", job["id"])
		}
	}
}
