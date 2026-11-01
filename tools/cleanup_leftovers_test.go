package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

// leftoverQbit serves one torrent whose folder is owned, the categories and
// the preferences. fail makes the torrent list answer 500.
func leftoverQbit(t *testing.T, root string, fail bool) *qbit.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/login"):
			w.Write([]byte("Ok."))
		case strings.HasSuffix(r.URL.Path, "/torrents/info"):
			if fail {
				w.WriteHeader(500)
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{{
				"hash": "abc", "name": "Active", "save_path": filepath.Join(root, "Sonarr"),
				"content_path": filepath.Join(root, "Sonarr", "Active"),
			}})
		case strings.HasSuffix(r.URL.Path, "/torrents/categories"):
			json.NewEncoder(w).Encode(map[string]any{"radarr": map[string]any{"savePath": filepath.Join(root, "Radarr")}})
		case strings.HasSuffix(r.URL.Path, "/app/preferences"):
			json.NewEncoder(w).Encode(map[string]any{"save_path": root, "temp_path": filepath.Join(root, "Incomplete"), "temp_path_enabled": true})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return qbit.NewClient(srv.URL, "u", "p")
}

func leftoverServer(t *testing.T, allowDestructive, qbFails bool) (*server.MCPServer, string, *store.Store) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{AllowDestructive: allowDestructive}
	cfg.Media.AllowedReadRoots = []string{root}
	cfg.Media.AllowedWriteRoots = []string{root}
	cfg.Services = map[string]config.ServiceConfig{}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := server.NewMCPServer("test", "0.0.0")
	RegisterMaintenance(s, cfg, arrservice.NewRegistry(cfg), leftoverQbit(t, root, qbFails), st)
	old := time.Now().Add(-48 * time.Hour)
	for _, f := range []string{"Old Show/a.nfo", "Sonarr/Active/b.nfo", "Sonarr/Done/c.nfo", "Radarr/.keep-empty/x", "Incomplete/gone/d.nfo", "Movie/m.mkv"} {
		p := filepath.Join(root, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, old, old)
	}
	os.MkdirAll(filepath.Join(root, "Radarr", "empty-cat-dir"), 0o755)
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chtimes(p, old, old)
		}
		return nil
	})
	return s, root, st
}

func TestLeftoverListKeepsWhatQbitOwns(t *testing.T) {
	s, root, _ := leftoverServer(t, true, false)
	res := callTool(t, s, "cleanup_download_leftovers", map[string]any{"path": root})
	if res.IsError {
		t.Fatalf("list failed: %s", resultText(t, res))
	}
	var out struct {
		Scan struct {
			Leftovers []struct{ Path, Class string } `json:"leftovers"`
		} `json:"scan"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range out.Scan.Leftovers {
		rel, _ := filepath.Rel(root, l.Path)
		got[rel] = l.Class
	}
	for _, want := range []string{"Old Show", "Sonarr/Done", "Incomplete/gone", "Radarr/empty-cat-dir"} {
		if got[want] == "" {
			t.Errorf("%s should be listed: %v", want, got)
		}
	}
	for _, kept := range []string{"Sonarr", "Sonarr/Active", "Radarr", "Incomplete", "Movie"} {
		if got[kept] != "" {
			t.Errorf("%s must be kept: %v", kept, got)
		}
	}
}

func TestLeftoverRefusesWhenQbitCannotBeRead(t *testing.T) {
	s, root, _ := leftoverServer(t, true, true)
	if res := callTool(t, s, "cleanup_download_leftovers", map[string]any{"path": root}); !res.IsError {
		t.Error("list went on without knowing what qBittorrent owns")
	}
	res := callTool(t, s, "cleanup_download_leftovers", map[string]any{
		"mode": "remove_junk_dirs", "paths": []any{filepath.Join(root, "Old Show")}, "confirm": true})
	if !res.IsError {
		t.Error("removal went on without knowing what qBittorrent owns")
	}
	if _, err := os.Stat(filepath.Join(root, "Old Show")); err != nil {
		t.Errorf("folder removed: %v", err)
	}
}

func TestLeftoverRemovalGates(t *testing.T) {
	s, root, _ := leftoverServer(t, true, false)
	one := []any{filepath.Join(root, "Old Show")}
	cases := map[string]map[string]any{
		"no confirm": {"mode": "remove_junk_dirs", "paths": one},
		"no paths":   {"mode": "remove_junk_dirs", "confirm": true},
		"bad mode":   {"mode": "rm_rf", "paths": one, "confirm": true},
	}
	many := make([]any, 51)
	for i := range many {
		many[i] = filepath.Join(root, "x", string(rune('a'+i%26)))
	}
	cases["too many"] = map[string]any{"mode": "remove_junk_dirs", "paths": many, "confirm": true}
	for name, args := range cases {
		if res := callTool(t, s, "cleanup_download_leftovers", args); !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "Old Show")); err != nil {
		t.Errorf("folder removed by a refused call: %v", err)
	}

	off, offRoot, _ := leftoverServer(t, false, false)
	res := callTool(t, off, "cleanup_download_leftovers", map[string]any{
		"mode": "remove_junk_dirs", "paths": []any{filepath.Join(offRoot, "Old Show")}, "confirm": true})
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_destructive") {
		t.Errorf("allow_destructive off: %s", resultText(t, res))
	}
	if _, err := os.Stat(filepath.Join(offRoot, "Old Show")); err != nil {
		t.Errorf("removed with allow_destructive off: %v", err)
	}
}

func TestLeftoverRemoveRechecksAndAudits(t *testing.T) {
	s, root, st := leftoverServer(t, true, false)
	res := callTool(t, s, "cleanup_download_leftovers", map[string]any{
		"mode": "remove_junk_dirs", "confirm": true,
		"paths": `["` + filepath.Join(root, "Old Show") + `","` + filepath.Join(root, "Sonarr", "Active") +
			`","` + filepath.Join(root, "Movie") + `","` + filepath.Join(root, "Gone") + `"]`,
	})
	if res.IsError {
		t.Fatalf("%s", resultText(t, res))
	}
	var out struct {
		Removed, Skipped int
		Results          []struct{ Path, Status, Reason string }
	}
	json.Unmarshal([]byte(resultText(t, res)), &out)
	status := map[string]string{}
	for _, r := range out.Results {
		status[filepath.Base(r.Path)] = r.Status
	}
	if status["Old Show"] != "removed" || status["Active"] != "skipped" || status["Movie"] != "skipped" || status["Gone"] != "already_absent" {
		t.Errorf("statuses: %v", status)
	}
	for _, kept := range []string{"Sonarr/Active/b.nfo", "Movie/m.mkv"} {
		if _, err := os.Stat(filepath.Join(root, kept)); err != nil {
			t.Errorf("%s was removed", kept)
		}
	}
	acts, _ := st.RecentActions(10)
	found := false
	for _, a := range acts {
		if a["action"] == "cleanup_leftovers_remove" {
			found = true
		}
	}
	if !found {
		t.Error("no audit entry for the removal")
	}
}

func TestQbitKeepPathsCoverCategoriesWithoutAPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/login"):
			w.Write([]byte("Ok."))
		case strings.HasSuffix(r.URL.Path, "/app/preferences"):
			w.Write([]byte(`{"save_path":"/dl/","temp_path":"/dl/Incomplete","temp_path_enabled":true}`))
		case strings.HasSuffix(r.URL.Path, "/torrents/categories"):
			w.Write([]byte(`{"tv":{"name":"tv","savePath":""},"movies":{"name":"movies","savePath":"films"},"abs":{"name":"abs","savePath":"/elsewhere/abs","download_path":"/dl/tmp-abs"},"nodl":{"name":"nodl","savePath":"","download_path":false}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	got, err := qbit.NewClient(srv.URL, "u", "p").KeepPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	for _, want := range []string{"/dl/", "/dl/Incomplete", "/dl/tv", "/dl/films", "/elsewhere/abs", "/dl/tmp-abs", "/dl/Incomplete/tv", "/dl/nodl"} {
		if !have[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}
