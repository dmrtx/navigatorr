package maintenanceui

import (
	"encoding/json"
	"testing"
)

func TestOperationsReplacementContextIsDurableAndExcludesPromotedCandidates(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	media := map[string]any{"original": map[string]any{"size_bytes": 1000}, "result": map[string]any{"size_bytes": 300}, "original_intact": true, "original_sha256": "before"}
	seedOperation(t, st, "series-batch", "transcode_batch", "completed", map[string]any{"service": "sonarr", "series_id": "77"}, nil, nil)
	seedOperation(t, st, "folder-batch", "transcode_batch", "completed", map[string]any{"paths": []string{"/media/folder.mkv"}}, nil, nil)
	seedOperation(t, st, "series-child", "transcode_media", "completed", map[string]any{"path": "/media/series.mkv", "parent_action_id": "series-batch"}, media, nil)
	seedOperation(t, st, "folder-child", "transcode_media", "completed", map[string]any{"path": "/media/folder.mkv", "parent_action_id": "folder-batch"}, media, nil)
	seedOperation(t, st, "movie-ui", "transcode_media", "completed", map[string]any{"path": "/media/movie.mkv", "library_context": map[string]any{"service": "radarr", "id": 42}}, media, nil)
	seedOperation(t, st, "movie-agent", "transcode_media", "completed", map[string]any{"path": "/media/agent.mkv", "service": "radarr", "media_id": "43"}, media, nil)
	seedOperation(t, st, "unknown", "transcode_media", "completed", map[string]any{"path": "/media/unknown.mkv", "library_context": map[string]any{"service": "radarr", "id": -1}}, media, nil)
	readJob := func(id string) map[string]any {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/operations?id="+id, "", true)
		var page operationsPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		return page.Jobs[0]
	}
	for _, tc := range []struct {
		id, service, key string
		expected         float64
	}{{"series-child", "sonarr", "series_id", 77}, {"movie-ui", "radarr", "movie_id", 42}, {"movie-agent", "radarr", "movie_id", 43}} {
		job := readJob(tc.id)
		context := operationMap(job["replacement_context"])
		if context["service"] != tc.service || context[tc.key] != tc.expected || job["candidate_ready"] != true {
			t.Fatal(tc.id, job)
		}
	}
	if context := operationMap(readJob("folder-child")["replacement_context"]); context["service"] != "filesystem" {
		t.Fatal(context)
	}
	if context := readJob("unknown")["replacement_context"]; context != nil {
		t.Fatal("invalid routing hint accepted", context)
	}
	seedOperation(t, st, "existing-review", "promote_transcode_candidate", "waiting_decision", map[string]any{"transcode_action_id": "series-child", "service": "sonarr", "series_id": 77}, nil, nil)
	if job := readJob("series-child"); job["replacement_action_id"] != "existing-review" || job["candidate_ready"] != true {
		t.Fatal("existing review lost", job)
	}
	seedOperation(t, st, "promoted", "promote_transcode_candidate", "completed", map[string]any{"transcode_action_id": "movie-ui", "service": "radarr", "movie_id": 42}, map[string]any{"promoted": true, "recovery_retained": false, "promotion": map[string]any{"transcode_action_id": "movie-ui", "original_path": "/media/movie.mkv", "original_sha256": "before", "original_bytes": 1000, "candidate_bytes": 300, "recovery_cleanup_completed": true, "recovery_verified_before_replacement": true}}, nil)
	if job := readJob("movie-ui"); job["candidate_ready"] != false || job["replacement_action_id"] != "promoted" || job["replaced"] != true {
		t.Fatal("consumed candidate offered for replacement", job)
	}
	seedOperation(t, st, "same-original-other-action", "transcode_media", "completed", map[string]any{"path": "/media/movie.mkv"}, media, nil)
	if job := readJob("same-original-other-action"); job["candidate_ready"] != false {
		t.Fatal("obsolete source content offered for replacement", job)
	}
}
