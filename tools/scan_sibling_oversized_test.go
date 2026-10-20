package tools

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
)

func episodeFile(id, season int, path string, size int64, runTime, resolution string) map[string]any {
	return map[string]any{"id": float64(id), "seasonNumber": float64(season), "relativePath": path, "size": float64(size),
		"mediaInfo": map[string]any{"runTime": runTime, "resolution": resolution}}
}

func TestSiblingOversizedFindings(t *testing.T) {
	files := []map[string]any{
		// Season 1: e01 runs at five times the median of e02-e04.
		episodeFile(1, 1, "Season 01/e01.mkv", 2_000_000_000, "25:00", "1920x1080"),
		episodeFile(2, 1, "Season 01/e02.mkv", 300_000_000, "25:00", "1920x1080"),
		episodeFile(3, 1, "Season 01/e03.mkv", 400_000_000, "25:00", "1920x800"),
		episodeFile(4, 1, "Season 01/e04.mkv", 500_000_000, "0:25:00", "1920x1080"),
		// Another resolution class is not a sibling, and neither is a file
		// without a runtime.
		episodeFile(5, 1, "Season 01/e05.mkv", 900_000_000, "25:00", "1280x720"),
		episodeFile(6, 1, "Season 01/e06.mkv", 900_000_000, "", "1920x1080"),
		// Season 2 has only two others: too few for a median.
		episodeFile(7, 2, "Season 02/e01.mkv", 2_000_000_000, "25:00", "1920x1080"),
		episodeFile(8, 2, "Season 02/e02.mkv", 300_000_000, "25:00", "1920x1080"),
		episodeFile(9, 2, "Season 02/e03.mkv", 300_000_000, "25:00", "1920x1080"),
	}
	findings := siblingOversizedFindings("7", "Example Show", files, 2)
	if len(findings) != 1 {
		t.Fatalf("findings %+v", findings)
	}
	f := findings[0]
	if f.MediaType != "episode" || f.MediaID != "7:1" || f.FileID != "1" || f.FileSize != 2_000_000_000 || f.Issue != "oversized" || f.Title != "Example Show" {
		t.Fatalf("finding %+v", f)
	}
	// Median of 300, 400 and 500 MB over 1500 s is 2.1 Mbps.
	for _, want := range []string{"season 1 · 1080p", "Season 01/e01.mkv", "5.0×", "2.1 Mbps median", "3 sibling episodes"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail %q lacks %q", f.Detail, want)
		}
	}
	if findings := siblingOversizedFindings("7", "Example Show", files, 6); len(findings) != 0 {
		t.Fatalf("ratio 6 flagged %+v", findings)
	}
}

func TestParseRunTimeAndResolution(t *testing.T) {
	for in, want := range map[any]float64{"1:02:31": 3751, "42:31": 2551, "95": 95, "": 0, "1:x": 0, "1:2:3:4": 0, nil: 0, 120.0: 120, "NaN": 0, math.NaN(): 0} {
		if got := parseRunTime(in); got != want {
			t.Errorf("parseRunTime(%v) = %v, want %v", in, got, want)
		}
	}
	if w, h := parseResolution("1920x800"); w != 1920 || h != 800 {
		t.Errorf("resolution %dx%d", w, h)
	}
	if w, h := parseResolution("unknown"); w != 0 || h != 0 {
		t.Errorf("unknown resolution %dx%d", w, h)
	}
}

// A dry run reports per-episode issues and writes nothing; a real run opens
// one maintenance issue per episode file and closes the series' pending ones
// it no longer finds.
func TestScanServiceSiblingOversized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ep := func(id int, size int64) string {
			return fmt.Sprintf(`{"id":%d,"seasonNumber":1,"relativePath":"Season 01/e%02d.mkv","size":%d,"mediaInfo":{"runTime":"25:00","resolution":"1920x1080","audioLanguages":["English"]}}`, id, id-10, size)
		}
		switch r.URL.Path {
		case "/api/v3/series":
			w.Write([]byte(`[{"id":1,"title":"Example Show","statistics":{"sizeOnDisk":4000000000,"episodeFileCount":6}}]`))
		case "/api/v3/episodefile":
			// The outlier is the sixth file, past the language sample.
			w.Write([]byte("[" + strings.Join([]string{ep(11, 300_000_000), ep(12, 400_000_000), ep(13, 500_000_000),
				ep(14, 400_000_000), ep(15, 300_000_000), ep(16, 2_000_000_000)}, ",") + "]"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cfg := &config.Config{Services: map[string]config.ServiceConfig{"sonarr": {URL: srv.URL, APIKey: "k", AuthMethod: "header", AuthHeader: "X-Api-Key", APIVersion: "/api/v3"}}}
	st, err := store.Open(t.TempDir() + "/scan.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := &Deps{Store: st, Config: cfg}
	count := func(f []scanFinding) (n int) {
		for _, x := range f {
			if x.Issue == "oversized" && x.MediaType == "episode" {
				n++
			}
		}
		return n
	}
	seed := func(mediaID, status string) int64 {
		it, err := st.AddItem(store.MaintenanceItem{Service: "sonarr", MediaType: "episode", MediaID: mediaID, Title: "Example Show", IssueType: "oversized"})
		if err != nil {
			t.Fatal(err)
		}
		if status != "" {
			if _, err := st.Transition(it.ID, status, ""); err != nil {
				t.Fatal(err)
			}
		}
		return it.ID
	}
	statusOf := func(id int64) string {
		it, err := st.GetItem(id)
		if err != nil {
			t.Fatal(err)
		}
		return it.Status
	}
	// Open issues for a file that is gone, one a worker is researching, and one
	// in another series whose id shares the prefix digit.
	gone := seed("1:99", "")
	busy := seed("1:98", store.MaintResearching)
	other := seed("10:5", "")

	stat, err := scanService(context.Background(), arrservice.NewRegistry(cfg), d, "sonarr", 0, 900<<20, 2, false)
	if err != nil || count(stat.Issues) != 1 {
		t.Fatalf("dry run %+v %v", stat.Issues, err)
	}
	if items, _ := st.ListItems(store.ItemFilter{Limit: 50}); len(items) != 3 || statusOf(gone) != store.MaintPending {
		t.Fatalf("dry run wrote %+v", items)
	}
	if stat, _ := scanService(context.Background(), arrservice.NewRegistry(cfg), d, "sonarr", 0, 900<<20, 0, false); count(stat.Issues) != 0 {
		t.Fatal("sibling_ratio=0 still flagged")
	}
	stat, err = scanService(context.Background(), arrservice.NewRegistry(cfg), d, "sonarr", 0, 900<<20, 2, true)
	if err != nil || count(stat.Issues) != 1 {
		t.Fatalf("real run %+v %v", stat.Issues, err)
	}
	var job *int64
	for _, x := range stat.Issues {
		if x.MediaType == "episode" {
			job = x.JobID
		}
	}
	if job == nil {
		t.Fatalf("real run opened no job: %+v", stat.Issues)
	}
	it, err := st.GetItem(*job)
	if err != nil {
		t.Fatal(err)
	}
	if it.Service != "sonarr" || it.MediaType != "episode" || it.MediaID != "1:16" || it.IssueType != "oversized" ||
		it.CurrentFileID == nil || *it.CurrentFileID != "16" || it.CurrentSize == nil || *it.CurrentSize != 2_000_000_000 {
		t.Fatalf("stored item %+v", it)
	}
	if statusOf(gone) != store.MaintDone || statusOf(busy) != store.MaintResearching || statusOf(other) != store.MaintPending {
		t.Fatalf("stale handling: gone=%s busy=%s other=%s", statusOf(gone), statusOf(busy), statusOf(other))
	}
	// A second real run keeps the flagged issue open.
	if _, err := scanService(context.Background(), arrservice.NewRegistry(cfg), d, "sonarr", 0, 900<<20, 2, true); err != nil || statusOf(*job) != store.MaintPending {
		t.Fatalf("rescan closed the flagged issue: %v %s", err, statusOf(*job))
	}
	// Resolving a series by media id leaves its episode issues alone, even
	// when an episode file id equals the series id.
	if _, err := st.AutoResolveByMedia("sonarr", "16", "series done"); err != nil || statusOf(*job) != store.MaintPending {
		t.Fatalf("series resolve closed an episode issue: %v %s", err, statusOf(*job))
	}
}
