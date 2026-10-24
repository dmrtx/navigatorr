package maintenanceui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcode/httpclient"
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
	season := r.URL.Query().Get("season")
	if season != "" {
		if name != "sonarr" || id == "" {
			fail(w, 400, "select a series before selecting a season")
			return
		}
		if season != "unknown" {
			n, err := strconv.Atoi(season)
			if err != nil || n < 0 {
				fail(w, 400, "invalid season")
				return
			}
			season = strconv.Itoa(n)
		}
	}
	if id != "" && r.URL.Query().Get("title") == "1" {
		n, err := strconv.Atoi(id)
		if err != nil || n <= 0 {
			fail(w, 400, "invalid media ID")
			return
		}
		endpoint := "/api/v3/series/" + strconv.Itoa(n)
		if name == "radarr" {
			endpoint = "/api/v3/movie/" + strconv.Itoa(n)
		}
		b, err := svc.Get(r.Context(), endpoint, nil)
		var raw map[string]any
		if err != nil || json.Unmarshal(b, &raw) != nil {
			fail(w, 502, "library title unavailable")
			return
		}
		media := map[string]any{}
		for _, key := range []string{"id", "title", "year", "path", "seasons", "seriesType", "genres"} {
			if value, ok := raw[key]; ok {
				media[key] = value
			}
		}
		writeJSON(w, 200, map[string]any{"media": media})
		return
	}
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
	seasonCounts := map[int]int{}
	seasonFiles := map[int][]map[string]any{}
	search := strings.ToLower(r.URL.Query().Get("q"))
	for _, rec := range records {
		// Declared seasons include empty specials and unaired episodes. Only
		// episode files returned for this series can be selected for a batch.
		if name == "sonarr" && id != "" {
			path, _ := rec["path"].(string)
			relative, _ := rec["relativePath"].(string)
			if n, ok := rec["seasonNumber"].(float64); ok && n >= 0 && n == float64(int(n)) && (path != "" || relative != "") {
				seasonCounts[int(n)]++
			}
		}
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
		if name == "sonarr" && id != "" {
			n := libraryFileSeason(item)
			seasonFiles[n] = append(seasonFiles[n], item)
			if season != "" && (season == "unknown" && n != -1 || season != "unknown" && strconv.Itoa(n) != season) {
				continue
			}
		}
		items = append(items, item)
	}
	sortLibraryItems(items, r.URL.Query().Get("sort"))
	seasons := []map[string]int{}
	for n, count := range seasonCounts {
		seasons = append(seasons, map[string]int{"seasonNumber": n, "fileCount": count})
	}
	sort.Slice(seasons, func(i, j int) bool { return seasons[i]["seasonNumber"] < seasons[j]["seasonNumber"] })
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
	marks := s.fileStatuses()
	if r.URL.Query().Get("all") == "1" {
		if id == "" {
			fail(w, 400, "select a title before selecting files")
			return
		}
		if len(items) > 1000 {
			fail(w, 400, "selection exceeds 1000 files; filter or choose a smaller collection")
			return
		}
		offset, end = 0, len(items)
	}
	// A title's folder summarizes its files; a file shows its own state. The
	// paths are the *arr's, the same ones a batch is created from.
	for _, item := range items[offset:end] {
		path, _ := item["path"].(string)
		var mark *fileStatusView
		if id == "" {
			mark = marks.under(path)
		} else {
			mark = marks.file(path)
		}
		if mark != nil {
			item["nav_status"] = mark
		}
	}
	groups := librarySeasonGroups(seasonFiles, r.URL.Query().Get("sort"))
	for _, group := range groups {
		n := -1
		if value, ok := group["seasonNumber"].(int); ok {
			n = value
		}
		paths := make([]string, 0, len(seasonFiles[n]))
		for _, item := range seasonFiles[n] {
			path, _ := item["path"].(string)
			paths = append(paths, path)
		}
		if mark := marks.among(paths); mark != nil {
			group["nav_status"] = mark
		}
	}
	writeJSON(w, 200, map[string]any{"items": items[offset:end], "total": len(items), "offset": offset, "has_more": end < len(items), "service": name, "seasons": seasons, "season_groups": groups})
}

func libraryFileSeason(item map[string]any) int {
	if n, ok := item["seasonNumber"].(float64); ok && n >= 0 && n == float64(int(n)) {
		return int(n)
	}
	return -1
}

func librarySeasonGroups(files map[int][]map[string]any, order string) []map[string]any {
	groups := []map[string]any{}
	for n, items := range files {
		group := map[string]any{"seasonNumber": nil, "fileCount": len(items), "size": nil}
		if n >= 0 {
			group["seasonNumber"] = n
		}
		size, known := float64(0), true
		for _, item := range items {
			value, ok := item["size"].(float64)
			known = known && ok && value >= 0
			size += value
		}
		if known {
			group["size"] = size
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if order == "size_desc" || order == "size_asc" {
			av, aok := a["size"].(float64)
			bv, bok := b["size"].(float64)
			if aok != bok {
				return aok
			}
			if av != bv {
				if order == "size_desc" {
					return av > bv
				}
				return av < bv
			}
		}
		an, aok := a["seasonNumber"].(int)
		bn, bok := b["seasonNumber"].(int)
		if aok != bok {
			return aok
		}
		if order == "name_desc" {
			return an > bn
		}
		return an < bn
	})
	return groups
}

func sortLibraryItems(items []map[string]any, order string) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if order == "size_desc" || order == "size_asc" {
			av, aok := a["size"].(float64)
			bv, bok := b["size"].(float64)
			if aok != bok {
				return aok
			} // Unknown sizes always follow measured sizes.
			if av != bv {
				if order == "size_desc" {
					return av > bv
				}
				return av < bv
			}
		}
		label := func(item map[string]any) string {
			for _, key := range []string{"title", "relativePath", "path"} {
				if value, ok := item[key].(string); ok && value != "" {
					return strings.ToLower(value)
				}
			}
			return ""
		}
		if al, bl := label(a), label(b); al != bl {
			if order == "name_desc" {
				return al > bl
			}
			return al < bl
		}
		return fmt.Sprint(a["id"]) < fmt.Sprint(b["id"])
	})
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
	// The log is on the job's own worker.
	httpExec, ok := s.engine.ActionWorkerExecutor(state).(*httpclient.HTTPExecutor)
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
