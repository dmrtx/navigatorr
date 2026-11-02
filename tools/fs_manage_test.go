package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

// fsManageServer serves a Radarr that tracks one movie and a qBittorrent that
// owns one file, over a library of a tracked movie with a stray folder.
func fsManageServer(t *testing.T, allowDestructive, radarrFails bool) (*server.MCPServer, string) {
	t.Helper()
	root := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	for _, f := range []string{
		"Movies/Film (1997)/Film (1997).mkv",
		"Movies/Film (1997)/Stray/big.mkv",
		"Movies/Film (1997)/Stray/big.en.srt",
		"Downloads/Seeding/seed.mkv",
	} {
		p := filepath.Join(root, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("data"), 0o644)
		os.Chtimes(p, old, old)
	}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chtimes(p, old, old)
		}
		return nil
	})

	radarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if radarrFails {
			w.WriteHeader(500)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/movie") {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{{
			"id": 1, "path": filepath.Join(root, "Movies", "Film (1997)"),
			"movieFile": map[string]any{"path": filepath.Join(root, "Movies", "Film (1997)", "Film (1997).mkv")},
		}})
	}))
	t.Cleanup(radarr.Close)

	qb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/login"):
			w.Write([]byte("Ok."))
		case strings.HasSuffix(r.URL.Path, "/torrents/info"):
			json.NewEncoder(w).Encode([]map[string]any{{
				"hash": "abc", "name": "Seeding", "save_path": filepath.Join(root, "Downloads"),
				"content_path": filepath.Join(root, "Downloads", "Seeding"),
			}})
		case strings.HasSuffix(r.URL.Path, "/torrents/categories"):
			json.NewEncoder(w).Encode(map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/app/preferences"):
			json.NewEncoder(w).Encode(map[string]any{"save_path": filepath.Join(root, "Downloads")})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(qb.Close)

	cfg := &config.Config{AllowDestructive: allowDestructive}
	cfg.Media.AllowedReadRoots = []string{root}
	cfg.Media.AllowedWriteRoots = []string{root}
	cfg.Services = map[string]config.ServiceConfig{
		"radarr": {URL: radarr.URL, APIVersion: "/api/v3", APIKey: "k", AuthMethod: "header"},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := server.NewMCPServer("test", "0.0.0")
	RegisterMaintenance(s, cfg, arrservice.NewRegistry(cfg), qbit.NewClient(qb.URL, "u", "p"), st)
	return s, root
}

func decode(t *testing.T, text string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), into); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, text)
	}
}

type planOut struct {
	Plan struct {
		Kind    string   `json:"kind"`
		Files   int      `json:"files"`
		Bytes   int64    `json:"bytes"`
		Digest  string   `json:"plan_digest"`
		Blocked []string `json:"blocked"`
	} `json:"plan"`
}

func TestFsMkdirTool(t *testing.T) {
	s, root := fsManageServer(t, false, false)
	dir := filepath.Join(root, "Movies", "Another (2001)")
	res := callTool(t, s, "fs_mkdir", map[string]any{"path": dir})
	if res.IsError {
		t.Fatalf("mkdir failed: %s", resultText(t, res))
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatal("folder not created")
	}
	if res := callTool(t, s, "fs_mkdir", map[string]any{"path": dir}); res.IsError {
		t.Fatalf("an existing folder must not fail: %s", resultText(t, res))
	}
	if res := callTool(t, s, "fs_mkdir", map[string]any{"path": filepath.Join(t.TempDir(), "x")}); !res.IsError {
		t.Fatal("outside the roots must fail")
	}
}

func TestFsRemoveStrayFolderEndToEnd(t *testing.T) {
	s, root := fsManageServer(t, true, false)
	stray := filepath.Join(root, "Movies", "Film (1997)", "Stray")

	res := callTool(t, s, "fs_remove", map[string]any{"path": stray, "recursive": "true"})
	if res.IsError {
		t.Fatalf("preview failed: %s", resultText(t, res))
	}
	var out planOut
	decode(t, resultText(t, res), &out)
	if out.Plan.Kind != "folder" || out.Plan.Files != 2 || out.Plan.Bytes != 8 || len(out.Plan.Blocked) != 0 || out.Plan.Digest == "" {
		t.Fatalf("plan = %+v", out.Plan)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("preview removed something")
	}

	for name, args := range map[string]map[string]any{
		"no digest":    {"path": stray, "recursive": "true", "mode": "delete", "confirm": "true"},
		"no confirm":   {"path": stray, "recursive": "true", "mode": "delete", "plan_digest": out.Plan.Digest},
		"wrong digest": {"path": stray, "recursive": "true", "mode": "delete", "confirm": "true", "plan_digest": "00"},
		"no recursive": {"path": stray, "mode": "delete", "confirm": "true", "plan_digest": out.Plan.Digest},
	} {
		if res := callTool(t, s, "fs_remove", args); !res.IsError {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("a refused call removed something")
	}

	res = callTool(t, s, "fs_remove", map[string]any{"path": stray, "recursive": "true", "mode": "delete",
		"confirm": "true", "plan_digest": out.Plan.Digest})
	if res.IsError {
		t.Fatalf("delete failed: %s", resultText(t, res))
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("folder still there")
	}
	if _, err := os.Stat(filepath.Join(root, "Movies", "Film (1997)", "Film (1997).mkv")); err != nil {
		t.Fatal("the tracked movie went too")
	}
}

func TestFsRemoveRefusesWithoutAllowDestructive(t *testing.T) {
	s, root := fsManageServer(t, false, false)
	stray := filepath.Join(root, "Movies", "Film (1997)", "Stray")
	res := callTool(t, s, "fs_remove", map[string]any{"path": stray, "recursive": "true"})
	var out planOut
	decode(t, resultText(t, res), &out)
	res = callTool(t, s, "fs_remove", map[string]any{"path": stray, "recursive": "true", "mode": "delete",
		"confirm": "true", "plan_digest": out.Plan.Digest})
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_destructive") {
		t.Fatalf("result = %s", resultText(t, res))
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("removed although destructive actions are off")
	}
}

func TestFsRemoveBlocksWhatRadarrAndQbitUse(t *testing.T) {
	s, root := fsManageServer(t, true, false)
	for name, tc := range map[string]struct{ path, want string }{
		"tracked movie file": {filepath.Join(root, "Movies", "Film (1997)", "Film (1997).mkv"), "radarr tracks the file"},
		"movie folder":       {filepath.Join(root, "Movies", "Film (1997)"), "library folder"},
		"seeding download":   {filepath.Join(root, "Downloads", "Seeding"), "still owns"},
		"download folder":    {filepath.Join(root, "Downloads"), "saves into"},
	} {
		res := callTool(t, s, "fs_remove", map[string]any{"path": tc.path, "recursive": "true"})
		if res.IsError {
			t.Errorf("%s: preview failed: %s", name, resultText(t, res))
			continue
		}
		var out planOut
		decode(t, resultText(t, res), &out)
		if !strings.Contains(strings.Join(out.Plan.Blocked, "|"), tc.want) {
			t.Errorf("%s: blocked = %v", name, out.Plan.Blocked)
		}
		res = callTool(t, s, "fs_remove", map[string]any{"path": tc.path, "recursive": "true", "mode": "delete",
			"confirm": "true", "plan_digest": out.Plan.Digest})
		if !res.IsError {
			t.Errorf("%s: delete went ahead", name)
		}
		if _, err := os.Stat(tc.path); err != nil {
			t.Errorf("%s: was removed", name)
		}
	}
}

func TestFsRemoveRefusesWhenRadarrCannotBeRead(t *testing.T) {
	s, root := fsManageServer(t, true, true)
	res := callTool(t, s, "fs_remove", map[string]any{"path": filepath.Join(root, "Movies", "Film (1997)", "Stray"), "recursive": "true"})
	if !res.IsError || !strings.Contains(resultText(t, res), "nothing was checked or removed") {
		t.Fatalf("result = %s", resultText(t, res))
	}
}

// A torrent that keeps its files straight in its save path needs one request
// for its file list; torrents saving elsewhere need none, and the lists that
// are needed are asked side by side.
func TestLeftoverPolicyOnlyAsksTorrentsInScope(t *testing.T) {
	var fileCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/login"):
			w.Write([]byte("Ok."))
		case strings.HasSuffix(r.URL.Path, "/torrents/info"):
			var ts []map[string]any
			for _, n := range []string{"a", "b", "c"} {
				ts = append(ts, map[string]any{"hash": "in" + n, "save_path": "/dl/in", "content_path": "/dl/in"})
				ts = append(ts, map[string]any{"hash": "out" + n, "save_path": "/dl/out", "content_path": "/dl/out"})
			}
			json.NewEncoder(w).Encode(ts)
		case strings.HasSuffix(r.URL.Path, "/torrents/files"):
			fileCalls.Add(1)
			json.NewEncoder(w).Encode([]map[string]any{{"name": r.URL.Query().Get("hash") + ".mkv"}})
		case strings.HasSuffix(r.URL.Path, "/torrents/categories"):
			json.NewEncoder(w).Encode(map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/app/preferences"):
			json.NewEncoder(w).Encode(map[string]any{})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	qb := qbit.NewClient(srv.URL, "u", "p")

	p, err := leftoverPolicy(t.Context(), qb, time.Minute, []string{"/dl/in"})
	if err != nil {
		t.Fatal(err)
	}
	if fileCalls.Load() != 3 || len(p.Owned) != 3 {
		t.Fatalf("file lists asked = %d, owned = %v", fileCalls.Load(), p.Owned)
	}
	fileCalls.Store(0)
	if _, err := leftoverPolicy(t.Context(), qb, time.Minute, nil); err != nil || fileCalls.Load() != 6 {
		t.Fatalf("without a scope every torrent is asked: %d, %v", fileCalls.Load(), err)
	}
}
