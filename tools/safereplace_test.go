package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

func maintTestServer(t *testing.T, allowDestructive bool, sonarrURL string) (*server.MCPServer, *store.Store) {
	t.Helper()
	roots := t.TempDir()
	cfg := &config.Config{AllowDestructive: allowDestructive}
	cfg.Media.AllowedReadRoots = []string{roots}
	cfg.Media.AllowedWriteRoots = []string{roots}
	cfg.Maintenance.PreferredGroups = []string{"Judas", "EMBER", "ASW"}
	cfg.Maintenance.PreferredResolution = "1080p"
	cfg.Services = map[string]config.ServiceConfig{}
	if sonarrURL != "" {
		cfg.Services["sonarr"] = config.ServiceConfig{
			URL: sonarrURL, APIKey: "k", AuthMethod: "header",
			AuthHeader: "X-Api-Key", APIVersion: "/api/v3",
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := server.NewMCPServer("test", "0.0.0")
	RegisterMaintenance(s, cfg, arrservice.NewRegistry(cfg), nil, st)
	return s, st
}

func toolJSONMap(t *testing.T, text string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, text)
	}
	return m
}

func addJob(t *testing.T, s *server.MCPServer, title, issue string, extra map[string]any) int64 {
	t.Helper()
	args := map[string]any{
		"media_type": "series", "service": "sonarr", "media_id": "248",
		"title": title, "issue_type": issue,
	}
	for k, v := range extra {
		args[k] = v
	}
	text := resultText(t, callTool(t, s, "maintenance_add", args))
	m := toolJSONMap(t, text)
	id, ok := m["id"].(float64)
	if !ok {
		t.Fatalf("no id in: %s", text)
	}
	return int64(id)
}

// maintenance_add must not duplicate an active job.
func TestMaintenanceAddIdempotentViaTool(t *testing.T) {
	s, _ := maintTestServer(t, false, "")
	a := addJob(t, s, "Vanitas", "oversized", map[string]any{"media_id": "248"})
	b := addJob(t, s, "Vanitas", "oversized", map[string]any{"media_id": "248"})
	if a != b {
		t.Errorf("duplicated job: %d vs %d", a, b)
	}
}

// done straight from pending must be refused: only verified replacements finish.
func TestResolveDoneNeedsReplacing(t *testing.T) {
	s, _ := maintTestServer(t, false, "")
	id := addJob(t, s, "Vanitas", "oversized", nil)
	res := callTool(t, s, "maintenance_resolve", map[string]any{"id": float64(id), "status": "done", "note": "skip"})
	if !strings.Contains(resultText(t, res), "verified") {
		t.Errorf("pending->done was not gated on verification: %s", resultText(t, res))
	}
}

// Acceptance: failed verification keeps the original and blocks the job.
func TestSafeReplaceVerifyFailKeepsOriginal(t *testing.T) {
	s, st := maintTestServer(t, true, "")
	id := addJob(t, s, "Fate/strange Fake", "oversized",
		map[string]any{"media_id": "100", "requires_subtitles": "true"})
	fid := float64(id)
	callTool(t, s, "safe_replace", map[string]any{"id": fid, "step": "plan"})
	callTool(t, s, "safe_replace", map[string]any{"id": fid, "step": "select",
		"release_guid": "g1", "title": "[Judas] F/sF 1080p HEVC", "size": "6000000000"})
	// Hash-only add: no download client call, idempotent on retry.
	callTool(t, s, "safe_replace", map[string]any{"id": fid, "step": "add_torrent", "torrent_hash": "ABCDEF"})
	if _, err := st.Transition(id, store.MaintDownloaded, "complete"); err != nil {
		t.Fatalf("simulating completion: %v", err)
	}
	res := callTool(t, s, "safe_replace", map[string]any{"id": fid, "step": "verify",
		"complete": "true", "audio_langs": "Japanese", "sub_langs": ""})
	text := resultText(t, res)
	if !strings.Contains(text, "original intact") {
		t.Fatalf("verify failure did not protect the original: %s", text)
	}
	it, _ := st.GetItem(id)
	if it.Status != store.MaintBlocked {
		t.Errorf("job is %s, want blocked", it.Status)
	}
	// Nothing may delete now: the fs gate requires replacing.
	del := callTool(t, s, "fs_safe_delete", map[string]any{
		"path": "whatever.mkv", "maintenance_item_id": fid, "confirm": "true"})
	if !strings.Contains(resultText(t, del), "not replacing") {
		t.Errorf("delete gate did not hold: %s", resultText(t, del))
	}
}

// Acceptance: a torrent hiding an executable is rejected and blocklisted.
func TestMaliciousTorrentRejected(t *testing.T) {
	s, st := maintTestServer(t, true, "")
	id := addJob(t, s, "Dark Matter", "oversized", map[string]any{"media_id": "55"})
	fid := float64(id)
	callTool(t, s, "safe_replace", map[string]any{"id": fid, "step": "plan"})
	callTool(t, s, "safe_replace", map[string]any{"id": fid, "step": "select",
		"release_guid": "evil-guid", "title": "Dark.Matter.S02E03.1080p"})
	res := callTool(t, s, "safe_replace", map[string]any{"id": fid, "step": "add_torrent",
		"url":   "magnet:?xt=urn:btih:EVILHASH123",
		"files": "Dark.Matter.S02E03.1080p.mkv.exe,Dark.Matter.S02E03.1080p.nfo"})
	text := resultText(t, res)
	if !strings.Contains(text, "REJECTED") && !strings.Contains(text, "dangerous") {
		t.Fatalf("malicious torrent was not rejected: %s", text)
	}
	it, _ := st.GetItem(id)
	if it.Status != store.MaintBlocked {
		t.Errorf("job is %s, want blocked", it.Status)
	}
	if !st.IsBlocked("evil-guid") {
		t.Error("evil release was not blocklisted")
	}
}

// Acceptance: ranking through the MCP tool prefers the small healthy Judas.
func TestRankReleasesToolPrefersJudas(t *testing.T) {
	s, _ := maintTestServer(t, false, "")
	cands, _ := json.Marshal([]map[string]any{
		{"guid": "sp", "title": "[SubsPlease] F/sF 1080p HEVC 10bit", "release_group": "SubsPlease",
			"size": float64(35 << 30), "seeders": float64(200), "video_codec": "hevc",
			"resolution": "1080p", "bit_depth": float64(10),
			"audio_langs": []string{"jpn", "eng"}, "sub_langs": []string{"eng"}},
		{"guid": "ju", "title": "[Judas] F/sF 1080p HEVC x265 10bit Dual Audio Multi-Subs", "release_group": "Judas",
			"size": float64(6 << 30), "seeders": float64(100), "video_codec": "hevc",
			"resolution": "1080p", "bit_depth": float64(10), "dual_audio": true, "multi_subs": true,
			"audio_langs": []string{"jpn", "eng"}, "sub_langs": []string{"eng", "spa", "ara"}},
	})
	res := callTool(t, s, "rank_releases", map[string]any{
		"media_type": "anime", "current_size": "21000000000", "candidates": string(cands)})
	var ranked []map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &ranked); err != nil || len(ranked) != 2 {
		t.Fatalf("ranking failed: %s", resultText(t, res))
	}
	if !strings.Contains(ranked[0]["title"].(string), "Judas") {
		t.Errorf("winner is %v, want Judas", ranked[0]["title"])
	}
}

// A blocked job can be reopened along a legal edge; active or finished
// jobs cannot, and reopening without a note is refused.
func TestMaintenanceReopen(t *testing.T) {
	s, st := maintTestServer(t, false, "")
	id := addJob(t, s, "Vanitas", "oversized", nil)
	fid := float64(id)
	if _, err := st.Transition(id, store.MaintBlocked, "bad candidate"); err != nil {
		t.Fatal(err)
	}
	// No note, no reopen.
	if text := resultText(t, callTool(t, s, "maintenance_reopen", map[string]any{"id": fid})); !strings.Contains(text, "note is required") {
		t.Errorf("reopen without note was not refused: %s", text)
	}
	// Illegal edge blocked -> done is refused by the state machine.
	if text := resultText(t, callTool(t, s, "maintenance_reopen", map[string]any{"id": fid, "to": "done", "note": "x"})); !strings.Contains(text, "invalid transition") {
		t.Errorf("illegal reopen edge was not refused: %s", text)
	}
	callTool(t, s, "maintenance_reopen", map[string]any{"id": fid, "to": "researching", "note": "better subs found"})
	if it, _ := st.GetItem(id); it.Status != store.MaintResearching {
		t.Errorf("job is %s, want researching", it.Status)
	}
	// Active jobs cannot be reopened.
	if text := resultText(t, callTool(t, s, "maintenance_reopen", map[string]any{"id": fid, "note": "x"})); !strings.Contains(text, "only blocked or failed") {
		t.Errorf("reopen of active job was not refused: %s", text)
	}
}

// fs_safe_delete obeys the global allow_destructive kill-switch even for a
// fully validated (replacing) job.
func TestFsSafeDeleteNeedsAllowDestructive(t *testing.T) {
	s, st := maintTestServer(t, false, "")
	id := addJob(t, s, "Vanitas", "oversized", nil)
	for _, next := range []string{store.MaintResearching, store.MaintCandidate, store.MaintDownloading,
		store.MaintDownloaded, store.MaintVerifying, store.MaintImporting, store.MaintReplacing} {
		if _, err := st.Transition(id, next, ""); err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
	}
	text := resultText(t, callTool(t, s, "fs_safe_delete", map[string]any{
		"path": "old.mkv", "maintenance_item_id": float64(id), "confirm": "true"}))
	if !strings.Contains(text, "allow_destructive") {
		t.Errorf("delete without allow_destructive was not refused: %s", text)
	}
}

// Full happy path with a fake Sonarr: plan to done, with space-saved notes.
func TestSafeReplaceHappyPath(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/episodefile/99":
			w.Write([]byte(`{"id":99,"path":"/media/new.mkv"}`))
		case r.Method == "DELETE" && r.URL.Path == "/api/v3/episodefile/55":
			deleted = append(deleted, r.URL.Path)
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	s, st := maintTestServer(t, true, srv.URL)
	id := addJob(t, s, "Vanitas", "oversized",
		map[string]any{"media_id": "248", "current_size": "40200000000", "requires_subtitles": "true"})
	fid := float64(id)
	steps := []map[string]any{
		{"step": "plan"},
		{"step": "select", "release_guid": "ember-g", "title": "[EMBER] Vanitas 1080p HEVC",
			"size": "12500000000", "seeders": "80", "reasons": "hevc_10bit,healthy_seed_count"},
		{"step": "add_torrent", "torrent_hash": "ABCDEF1234567890"},
	}
	for _, stp := range steps {
		stp["id"] = fid
		if text := resultText(t, callTool(t, s, "safe_replace", stp)); strings.Contains(text, "error") && strings.Contains(text, "Error") {
			t.Fatalf("step %v failed: %s", stp["step"], text)
		}
	}
	// Simulate a finished download, then verify with good audio/subs.
	if _, err := st.Transition(id, store.MaintDownloaded, "complete"); err != nil {
		t.Fatal(err)
	}
	callTool(t, s, "maintenance_update", map[string]any{"id": fid, "current_file_id": "55"})
	verify := resultText(t, callTool(t, s, "safe_replace", map[string]any{"id": fid,
		"step": "verify", "complete": "true", "audio_langs": "Japanese", "sub_langs": "eng,spa"}))
	if it, _ := st.GetItem(id); it.Status != store.MaintVerifying {
		t.Fatalf("not verifying after good check: %s / %s", it.Status, verify)
	}
	imp := resultText(t, callTool(t, s, "safe_replace", map[string]any{"id": fid,
		"step": "import_confirm", "new_file_id": "99"}))
	if it, _ := st.GetItem(id); it.Status != store.MaintReplacing {
		t.Fatalf("not replacing after import: %s / %s", it.Status, imp)
	}
	// Deletion without confirm must refuse.
	noConfirm := resultText(t, callTool(t, s, "safe_replace", map[string]any{"id": fid,
		"step": "delete_original", "via": "arr"}))
	if !strings.Contains(noConfirm, "confirm=true") {
		t.Fatalf("delete without confirm was not refused: %s", noConfirm)
	}
	del := resultText(t, callTool(t, s, "safe_replace", map[string]any{"id": fid,
		"step": "delete_original", "via": "arr", "confirm": "true"}))
	_ = del
	if len(deleted) != 1 {
		t.Fatalf("original was not deleted via sonarr: %s", del)
	}
	fin := resultText(t, callTool(t, s, "safe_replace", map[string]any{"id": fid,
		"step": "finish", "notes": "ember replacement live"}))
	it, _ := st.GetItem(id)
	if it.Status != store.MaintDone {
		t.Fatalf("not done: %s / %s", it.Status, fin)
	}
	if !strings.Contains(fin, "saved=") {
		t.Errorf("finish did not report space saved: %s", fin)
	}
	// Decisions survived: why did we pick EMBER?
	decs, _ := st.ListDecisions(id, 5)
	if len(decs) == 0 || decs[0].Decision != store.DecisionSelected {
		t.Errorf("decision history missing: %+v", decs)
	}
}
