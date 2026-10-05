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
