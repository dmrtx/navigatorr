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
