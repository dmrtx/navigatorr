package maintenanceui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
)

type queuePage struct {
	Items  []maintenanceQueueItem  `json:"items"`
	Counts store.MaintenanceCounts `json:"counts"`
	Total  int                     `json:"total"`
}

func TestMaintenanceQueueShowsFoldersAndClosesByHand(t *testing.T) {
	s, h := testUI(t)
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/api/v3/movie" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 7, "title": "Synthetic Film", "year": 2020, "path": "/media/movies/Synthetic Film (2020)", "movieFile": map[string]any{"size": 5000}},
			{"id": 8, "title": "Elsewhere", "year": 2021, "path": "/other/Elsewhere (2021)"},
		})
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"radarr": {URL: upstream.URL}}})
	st := s.engine.Deps().Store
	add := func(id, title string, size int64) store.MaintenanceItem {
		sz := size
		it, err := st.AddItem(store.MaintenanceItem{MediaType: "movie", Service: "radarr", MediaID: id, Title: title, IssueType: "oversized", Priority: 10, Notes: "Candidato", CurrentSize: &sz})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	a, b := add("7", "Synthetic Film", 0), add("8", "Elsewhere", 900)
	add("99", "Unknown to Radarr", 1)

	read := func(query string) queuePage {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/maintenance-queue"+query, "", true)
		var page queuePage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return page
	}
	page := read("?issue_type=oversized")
	if page.Total != 3 || page.Counts.Open != 3 {
		t.Fatal(page)
	}
	byID := map[int64]maintenanceQueueItem{}
	for _, row := range page.Items {
		byID[row.ID] = row
	}
	if got := byID[a.ID]; got.Folder != "/media/movies/Synthetic Film (2020)" || !got.InFiles || got.Year == nil || *got.Year != 2020 {
		t.Fatalf("browsable title: %+v", got)
	}
	if got := byID[b.ID]; got.Folder != "/other/Elsewhere (2021)" || got.InFiles {
		t.Fatalf("a folder outside the roots must not offer Files: %+v", got)
	}
	for _, row := range page.Items {
		if row.Title == "Unknown to Radarr" && (row.Folder != "" || row.InFiles) {
			t.Fatalf("unknown title has a folder: %+v", row)
		}
	}
	if hits != 1 {
		t.Fatalf("the library list should be read once, not per entry: %d", hits)
	}

	post := func(body string) (int, map[string]json.RawMessage) {
		w := request(h, "POST", "/api/maintenance/maintenance-queue", body, true)
		var out map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, _ := post(`{"id":` + itoa(a.ID) + `,"action":"processed"}`); code != 200 {
		t.Fatal(code)
	}
	if code, _ := post(`{"id":` + itoa(b.ID) + `,"action":"dismiss","note":"no me interesa"}`); code != 200 {
		t.Fatal(code)
	}
	if page := read("?issue_type=oversized"); page.Total != 1 || page.Counts != (store.MaintenanceCounts{Open: 1, Processed: 1, Dismissed: 1}) {
		t.Fatalf("after closing: %+v", page)
	}
	if page := read("?state=dismissed"); page.Total != 1 || page.Items[0].ID != b.ID || page.Items[0].ClosedBy != "console" {
		t.Fatalf("dismissed list: %+v", page)
	}
	if code, _ := post(`{"id":` + itoa(b.ID) + `,"action":"restore"}`); code != 200 {
		t.Fatal(code)
	}
	if page := read("?issue_type=oversized"); page.Total != 2 {
		t.Fatalf("after restore: %+v", page)
	}

	for _, bad := range []string{`{"id":0,"action":"dismiss"}`, `{"id":1,"action":"explode"}`, `{"id":1}`, `{"id":1,"action":"dismiss","extra":1}`} {
		if code, _ := post(bad); code != 400 {
			t.Fatal(bad, code)
		}
	}
	if code, _ := post(`{"id":4242,"action":"dismiss"}`); code != 404 {
		t.Fatal("unknown id", code)
	}
	if code, _ := post(`{"id":` + itoa(a.ID) + `,"action":"dismiss"}`); code != 409 {
		t.Fatal("a processed entry cannot be dismissed", code)
	}
	if w := request(h, "GET", "/api/maintenance/maintenance-queue?state=bogus", "", true); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := request(h, "GET", "/api/maintenance/maintenance-queue", "", false); w.Code != 401 {
		t.Fatal("unauthenticated read", w.Code)
	}
	if w := request(h, "POST", "/api/maintenance/maintenance-queue", `{"id":1,"action":"dismiss"}`, false); w.Code != 401 {
		t.Fatal("unauthenticated write", w.Code)
	}
}

func TestMaintenanceQueueSurvivesAnUnreachableLibrary(t *testing.T) {
	s, h := testUI(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 500) }))
	defer down.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"radarr": {URL: down.URL}}})
	if _, err := s.engine.Deps().Store.AddItem(store.MaintenanceItem{MediaType: "movie", Service: "radarr", MediaID: "1", Title: "Film", IssueType: "oversized"}); err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/api/maintenance/maintenance-queue", "", true)
	var page queuePage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 1 || page.Items[0].Folder != "" {
		t.Fatal(w.Code, w.Body.String())
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
