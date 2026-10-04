package maintenanceui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

func (s *Server) library(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("service")
	if name != "sonarr" && name != "radarr" {
		fail(w, 400, "select sonarr or radarr")
		return
	}
	svc, err := s.registry.Get(name)
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	id := r.URL.Query().Get("id")
	endpoint := "/api/v3/series"
	if name == "radarr" {
		endpoint = "/api/v3/movie"
	}
	query := map[string]string{}
	if id != "" {
		n, err := strconv.Atoi(id)
		if err != nil || n <= 0 {
			fail(w, 400, "invalid media ID")
			return
		}
		endpoint = "/api/v3/episodefile"
		query["seriesId"] = id
		if name == "radarr" {
			endpoint = "/api/v3/moviefile"
			query = map[string]string{"movieId": id}
		}
	}
	b, err := svc.Get(r.Context(), endpoint, query)
	if err != nil {
		fail(w, 502, "library service unavailable: "+err.Error())
		return
	}
	var records []map[string]any
	if json.Unmarshal(b, &records) != nil {
		fail(w, 502, "invalid library response")
		return
	}
	items := []map[string]any{}
	search := strings.ToLower(r.URL.Query().Get("q"))
	for _, rec := range records {
		item := map[string]any{}
		for _, key := range []string{"id", "title", "year", "path", "relativePath", "size", "seasonNumber", "seasons", "seriesType", "genres", "episodeCount", "hasFile", "movieId", "seriesId", "mediaInfo", "quality", "languages"} {
			if val, ok := rec[key]; ok {
				item[key] = val
			}
		}
		if f, ok := rec["movieFile"].(map[string]any); ok {
			item["size"] = f["size"]
		}
		if stat, ok := rec["statistics"].(map[string]any); ok {
			item["size"] = stat["sizeOnDisk"]
			item["episodeCount"] = stat["episodeFileCount"]
		}
		if search != "" && !strings.Contains(strings.ToLower(fmt.Sprint(item["title"], " ", item["path"], " ", item["relativePath"])), search) {
			continue
		}
		// Local navigation does not claim that cached *arr mediaInfo was probed.
		item["metadata_source"] = "arr"
		items = append(items, item)
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := min(offset+limit, len(items))
	writeJSON(w, 200, map[string]any{"items": items[offset:end], "total": len(items), "offset": offset, "has_more": end < len(items), "service": name})
}

// Logs are always looked up through a known action, never through an arbitrary
// browser-supplied worker job ID or filesystem path.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	inst, err := s.engine.Deps().Store.GetActionInstance(r.URL.Query().Get("id"))
	if err != nil {
		fail(w, 404, "action not found")
		return
	}
	var state map[string]any
	if json.Unmarshal([]byte(inst.StateJSON), &state) != nil {
		fail(w, 500, "invalid action state")
		return
	}
	id, _ := state["transcode_job_id"].(string)
	if id == "" {
		id, _ = state["job_id"].(string)
	}
	if id == "" || filepath.Base(id) != id {
		fail(w, 404, "no transcode runner log for this action")
		return
	}
	// HTTPExecutor keeps the worker token server-side and bounds the log tail.
	httpExec, ok := s.engine.Deps().Transcode.(*transcode.HTTPExecutor)
	if !ok {
		fail(w, 404, "worker logs require the HTTP executor")
		return
	}
	data, err := httpExec.Logs(r.Context(), id)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, data)
}
