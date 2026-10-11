package maintenanceui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
)

func TestLibraryTitleDeepLinkReadsOnlySelectedPublicFields(t *testing.T) {
	s, h := testUI(t)
	paths := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		json.NewEncoder(w).Encode(map[string]any{"id": 7, "title": "Synthetic title", "path": "/media/title", "seasons": []any{}, "apiKey": "must-never-project"})
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"sonarr": {URL: upstream.URL}, "radarr": {URL: upstream.URL}}})
	for _, service := range []string{"sonarr", "radarr"} {
		w := request(h, "GET", "/api/maintenance/library?service="+service+"&id=7&title=1", "", true)
		var page struct {
			Media map[string]any `json:"media"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Media["id"] != float64(7) || page.Media["title"] != "Synthetic title" || page.Media["apiKey"] != nil {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if len(paths) != 2 || paths[0] != "/api/v3/series/7" || paths[1] != "/api/v3/movie/7" {
		t.Fatal(paths)
	}
	for _, id := range []string{"0", "-7", "7/other", "x"} {
		if w := request(h, "GET", "/api/maintenance/library?service=sonarr&title=1&id="+id, "", true); w.Code != 400 {
			t.Fatal(id, w.Code)
		}
	}
	if w := request(h, "GET", "/api/maintenance/library?service=sonarr&title=1&id=7", "", false); w.Code != 401 {
		t.Fatal("unauthenticated title read", w.Code)
	}
	if len(paths) != 2 {
		t.Fatal("invalid reads reached upstream", paths)
	}
}

func TestLibrarySizeSortBeforePaginationAndAvailableSeasons(t *testing.T) {
	s, h := testUI(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records := []map[string]any{{"id": 999, "relativePath": "unknown.mkv", "path": "/media/unknown.mkv", "seasonNumber": 1}}
		for i := 0; i < 105; i++ {
			records = append(records, map[string]any{"id": i + 1, "relativePath": fmt.Sprintf("%03d.mkv", i), "path": fmt.Sprintf("/media/%03d.mkv", i), "size": i + 1, "seasonNumber": 1 + i/100})
		}
		_ = json.NewEncoder(w).Encode(records)
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"sonarr": {URL: upstream.URL, APIVersion: "/api/v3"}}})
	for _, tc := range []struct {
		order        string
		offset, want int
	}{{"size_desc", 0, 105}, {"size_desc", 100, 5}, {"size_asc", 0, 1}, {"size_asc", 105, 999}} {
		w := request(h, "GET", fmt.Sprintf("/api/maintenance/library?service=sonarr&id=1&sort=%s&offset=%d&limit=1", tc.order, tc.offset), "", true)
		var page struct {
			Items []struct {
				ID int `json:"id"`
			}
			Seasons []struct {
				Number int `json:"seasonNumber"`
				Count  int `json:"fileCount"`
			}
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if len(page.Items) != 1 || page.Items[0].ID != tc.want {
			t.Fatal(tc, page.Items)
		}
		if len(page.Seasons) != 2 || page.Seasons[0].Number != 1 || page.Seasons[0].Count != 101 || page.Seasons[1].Count != 5 {
			t.Fatal("seasons must include files beyond the page, without empty specials", page.Seasons)
		}
	}
	w := request(h, "GET", "/api/maintenance/library?service=sonarr&id=1&all=1", "", true)
	var selection struct {
		Items   []map[string]any `json:"items"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &selection); err != nil || w.Code != 200 || len(selection.Items) != 106 || selection.HasMore {
		t.Fatal("select all must include records beyond the first page", w.Code, w.Body.String())
	}
}

func TestLibraryAllSelectionIsBoundedAndRequiresCollection(t *testing.T) {
	s, h := testUI(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records := make([]map[string]any, 1001)
		for i := range records {
			records[i] = map[string]any{"id": i + 1, "relativePath": fmt.Sprintf("episode%d.mkv", i)}
		}
		_ = json.NewEncoder(w).Encode(records)
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"sonarr": {URL: upstream.URL, APIVersion: "/api/v3"}}})
	for _, query := range []string{"service=sonarr&all=1", "service=sonarr&id=1&all=1"} {
		w := request(h, "GET", "/api/maintenance/library?"+query, "", true)
		if w.Code != http.StatusBadRequest {
			t.Fatal("unbounded selection accepted", w.Code, w.Body.String())
		}
	}
}

func TestLibrarySeasonNavigationFiltersBeforePaginationAndSelection(t *testing.T) {
	s, h := testUI(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records := []map[string]any{
			{"id": 200, "path": "/media/Season 2/two.mkv", "seasonNumber": 2, "size": 20},
			{"id": 1000, "path": "/media/Season 10/ten.mkv", "seasonNumber": 10, "size": 100},
			{"id": 500, "path": "/media/Specials/special.mkv", "seasonNumber": 0, "size": 5},
			{"id": 900, "path": "/media/unsorted.mkv"},
		}
		for i := 0; i < 105; i++ {
			records = append(records, map[string]any{"id": i + 1, "path": fmt.Sprintf("/media/Season 1/%03d.mkv", i), "seasonNumber": 1, "size": 10})
		}
		json.NewEncoder(w).Encode(records)
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"sonarr": {URL: upstream.URL}, "radarr": {URL: upstream.URL}}})
	read := func(query string) map[string]any {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/library?service=sonarr&id=1&"+query, "", true)
		var page map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return page
	}
	page := read("limit=1")
	groups := page["season_groups"].([]any)
	if len(groups) != 5 || groups[0].(map[string]any)["seasonNumber"] != float64(0) || groups[1].(map[string]any)["seasonNumber"] != float64(1) || groups[2].(map[string]any)["seasonNumber"] != float64(2) || groups[3].(map[string]any)["seasonNumber"] != float64(10) || groups[4].(map[string]any)["seasonNumber"] != nil {
		t.Fatal("season list must be complete and numerically ordered", groups)
	}
	if groups[1].(map[string]any)["fileCount"] != float64(105) || groups[1].(map[string]any)["size"] != float64(1050) || groups[4].(map[string]any)["size"] != nil {
		t.Fatal("season totals must use all files and retain unknown sizes", groups)
	}
	for _, tc := range []struct {
		query string
		id    float64
		total float64
	}{{"season=2&limit=1", 200, 1}, {"season=0", 500, 1}, {"season=unknown", 900, 1}, {"season=1&offset=100&limit=1", 101, 105}, {"season=10&all=1", 1000, 1}} {
		page := read(tc.query)
		items := page["items"].([]any)
		if page["total"] != tc.total || len(items) != 1 || items[0].(map[string]any)["id"] != tc.id {
			t.Fatal("filter must precede pagination and select-all", tc, page)
		}
	}
	page = read("season=1&all=1")
	if len(page["items"].([]any)) != 105 || page["has_more"] != false {
		t.Fatal("select season must include files beyond the first page", page)
	}
	page = read("q=ten")
	if groups := page["season_groups"].([]any); len(groups) != 1 || groups[0].(map[string]any)["seasonNumber"] != float64(10) {
		t.Fatal("search must group only matching files", page)
	}
	page = read("sort=size_desc")
	if page["season_groups"].([]any)[0].(map[string]any)["seasonNumber"] != float64(1) {
		t.Fatal("season sizes must remain sortable", page)
	}
	for _, query := range []string{"service=sonarr&id=1&season=-1", "service=sonarr&id=1&season=x", "service=sonarr&id=1&season=1.5", "service=sonarr&season=1", "service=radarr&id=1&season=1"} {
		if w := request(h, "GET", "/api/maintenance/library?"+query, "", true); w.Code != 400 {
			t.Fatal("invalid season accepted", query, w.Code)
		}
	}
}

func TestFolderSizeOrderFrozenWhileMeasurementsChange(t *testing.T) {
	a, b := int64(10), int64(20)
	items := []folderItem{{FolderSize: &folderSize{Status: "ready", Bytes: &a}}, {FolderSize: &folderSize{Status: "ready", Bytes: &b}}}
	items[0].Path = "/media/a"
	items[0].IsDir = true
	items[1].Path = "/media/b"
	items[1].IsDir = true
	sortFolderItems(items, "size_desc")
	var cache folderListingCache
	key, err := cache.save(folderListing{Path: "/media", Order: "size_desc", Items: items})
	if err != nil {
		t.Fatal(err)
	}
	a = 100
	saved, err := cache.load(key, "/media", "", "size_desc")
	if err != nil || saved.Items[0].Path != "/media/b" {
		t.Fatal("measurement reordered listing", saved, err)
	}
	if _, err = cache.load(key, "/other", "", "size_desc"); err == nil {
		t.Fatal("listing reused across roots")
	}
	if _, err = cache.load(key, "/media", "", "size_asc"); err == nil {
		t.Fatal("listing reused across order")
	}
	sortFolderItems(items, "size_desc")
	if items[0].Path != "/media/a" {
		t.Fatal("explicit reload did not use new measurements")
	}
}
