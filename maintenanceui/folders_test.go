package maintenanceui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/fsop"
)

func TestFolderSourcesWithoutArr(t *testing.T) {
	s, h := testUI(t)
	root := t.TempDir()
	season := filepath.Join(root, "Season 1")
	if err := os.Mkdir(season, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"film.mkv", "notes.txt", "Season 1/episode.mp4"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	escaped := filepath.Join(t.TempDir(), "escaped.mp4")
	os.WriteFile(escaped, []byte("not allowed"), 0600)
	os.Symlink(escaped, filepath.Join(root, "link.mp4"))
	deps := s.engine.Deps()
	deps.Fs, _ = fsop.NewResolver([]string{root}, nil)
	s.engine = action.NewEngine(deps)
	base := "/api/maintenance/folder?path=" + url.QueryEscape(root)
	w := request(h, "GET", base, "", true)
	if w.Code != 200 || strings.Contains(w.Body.String(), "notes.txt") || strings.Contains(w.Body.String(), "escaped") {
		t.Fatal(w.Body.String())
	}
	for _, tc := range []struct {
		query string
		count int
	}{{"&files=1", 1}, {"&files=1&recursive=1", 2}, {"&files=1&q=FILM", 1}, {"&files=1&q=episode", 0}, {"&files=1&recursive=1&q=episode", 1}} {
		w := request(h, "GET", base+tc.query, "", true)
		var selection struct {
			Paths []string `json:"paths"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &selection); err != nil || w.Code != 200 || len(selection.Paths) != tc.count {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := request(h, "GET", "/api/maintenance/folder?path="+url.QueryEscape(filepath.Dir(root)), "", true); w.Code != 400 {
		t.Fatal("escaped root accepted")
	}
	for i := 0; i < 1001; i++ {
		os.WriteFile(filepath.Join(season, fmt.Sprintf("%04d.mkv", i)), nil, 0600)
	}
	if w := request(h, "GET", base+"&files=1&recursive=1", "", true); w.Code != 400 {
		t.Fatal("silently truncated selection", w.Body.String())
	}
}
