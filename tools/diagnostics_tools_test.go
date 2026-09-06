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
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

func TestDiagnosticsTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "diag_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{
		AllowDestructive:  false,
		LoadedPath:        "/test/path/config.yaml",
		MaxResponseSizeKB: 150,
		Concurrency: config.ConcurrencyConfig{
			MaxAPISimultaneous:     4,
			MaxInspectSimultaneous: 2,
		},
	}
	cfg.Services = map[string]config.ServiceConfig{
		"radarr": {
			URL:        srv.URL,
			APIKey:     "secret-radarr-key-never-leak",
			AuthMethod: "query",
		},
	}

	reg := arrservice.NewRegistry(cfg)
	specStore := openapi.NewStore(cfg)

	s := server.NewMCPServer("test", "0.0.0")
	RegisterDiagnostics(s, cfg, reg, specStore, nil, nil, nil, st)

	res := callTool(t, s, "diagnostics", map[string]any{
		"check_connectivity": true,
	})
	txt := resultText(t, res)

	// Must never leak secret API key
	if strings.Contains(txt, "secret-radarr-key-never-leak") {
		t.Fatalf("diagnostics tool leaked secret API key! %s", txt)
	}

	var dMap map[string]any
	if err := json.Unmarshal([]byte(txt), &dMap); err != nil {
		t.Fatalf("decoding diagnostics output: %v", err)
	}

	if dMap["status"] != "ok" {
		t.Errorf("expected overall status ok, got %v", dMap["status"])
	}

	effCfg, ok := dMap["effective_config"].(map[string]any)
	if !ok {
		t.Fatalf("missing effective_config: %v", dMap)
	}
	if effCfg["allow_destructive"] != false {
		t.Errorf("expected allow_destructive=false, got %v", effCfg["allow_destructive"])
	}
	if effCfg["config_file_loaded"] != "/test/path/config.yaml" {
		t.Errorf("expected config_file_loaded=/test/path/config.yaml, got %v", effCfg["config_file_loaded"])
	}
	if effCfg["state_path"] == "" {
		t.Errorf("expected non-empty state_path in effective_config")
	}
	if _, hasValidation := effCfg["root_validation"]; !hasValidation {
		t.Errorf("expected root_validation key in effective_config")
	}

	dbStats, ok := dMap["database"].(map[string]any)
	if !ok || dbStats["configured"] != true {
		t.Errorf("expected database configured=true, got %v", dbStats)
	}
}

func TestActionHistoryTool(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "history_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Seed action logs
	_ = st.LogActionEnriched("safe_replace", "sonarr", "Evangelion", `{"step":"verify"}`, "success", "", "Evangelion-S01", 120)
	_ = st.LogActionEnriched("scan_library", "radarr", "Akira", `{"dry_run":true}`, "found oversized", "", "Akira-1988", 45)

	s := server.NewMCPServer("test", "0.0.0")
	RegisterDiagnostics(s, &config.Config{}, nil, nil, nil, nil, nil, st)

	// 1. Query by media name "Evangelion"
	res := callTool(t, s, "action_history", map[string]any{
		"media": "Evangelion",
	})
	txt := resultText(t, res)
	if !strings.Contains(txt, "Evangelion") {
		t.Errorf("expected Evangelion in history, got: %s", txt)
	}
	if strings.Contains(txt, "Akira") {
		t.Errorf("expected Akira to be filtered out, got: %s", txt)
	}

	// 2. Query by action "scan_library"
	resScan := callTool(t, s, "action_history", map[string]any{
		"action": "scan_library",
	})
	scanTxt := resultText(t, resScan)
	if !strings.Contains(scanTxt, "Akira") {
		t.Errorf("expected Akira in scan_library query, got: %s", scanTxt)
	}
}

func TestDiagnosticsNeverExposesSecrets(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "diag_secrets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	secrets := map[string]string{
		"radarr_key":     "secret-radarr-key-999",
		"sonarr_key":     "secret-sonarr-key-888",
		"qbit_pass":      "secret-qbit-password-777",
		"tx_pass":        "secret-tx-password-666",
		"sab_key":        "secret-sabnzbd-key-555",
		"queue_token":    "secret-queue-token-444",
		"custom_abs_key": "secret-bearer-token-333",
	}

	cfg := &config.Config{
		LoadedPath: "/test/config.yaml",
		Services: map[string]config.ServiceConfig{
			"radarr": {
				URL:    "http://localhost:7878?apikey=secret-query-param",
				APIKey: secrets["radarr_key"],
			},
			"sonarr": {
				URL:        "http://localhost:8989",
				APIKey:     secrets["sonarr_key"],
				AuthMethod: "header",
				AuthHeader: "X-Api-Key",
			},
			"audiobookshelf": {
				URL:        "http://localhost:13378",
				APIKey:     secrets["custom_abs_key"],
				AuthMethod: "header",
				AuthHeader: "Authorization",
				AuthPrefix: "Bearer ",
			},
		},
		Transmission: config.TransmissionConfig{
			URL:      "http://user:secret-tx-userinfo@localhost:9091",
			Username: "admin",
			Password: secrets["tx_pass"],
		},
		QBittorrent: config.QBittorrentConfig{
			URL:      "http://localhost:8080",
			Username: "admin",
			Password: secrets["qbit_pass"],
		},
		SABnzbd: config.SABnzbdConfig{
			URL:    "http://localhost:8085",
			APIKey: secrets["sab_key"],
		},
		Queue: config.QueueConfig{
			Listen: "127.0.0.1:8099",
			Token:  secrets["queue_token"],
		},
	}

	reg := arrservice.NewRegistry(cfg)
	specStore := openapi.NewStore(cfg)

	s := server.NewMCPServer("test", "0.0.0")
	RegisterDiagnostics(s, cfg, reg, specStore, nil, nil, nil, st)

	res := callTool(t, s, "diagnostics", map[string]any{"check_connectivity": false})
	txt := resultText(t, res)

	// Verify that NO secret string appears in the diagnostics output
	for name, secretVal := range secrets {
		if strings.Contains(txt, secretVal) {
			t.Errorf("CRITICAL SECURITY FAILURE: %s (%q) leaked in diagnostics output!\n%s", name, secretVal, txt)
		}
	}
	if strings.Contains(txt, "secret-query-param") {
		t.Errorf("CRITICAL SECURITY FAILURE: URL query param leaked in diagnostics output!")
	}
	if strings.Contains(txt, "secret-tx-userinfo") {
		t.Errorf("CRITICAL SECURITY FAILURE: URL userinfo leaked in diagnostics output!")
	}

	// Verify that ***REDACTED*** appears in the output for masked values
	if !strings.Contains(txt, "***REDACTED***") {
		t.Errorf("expected ***REDACTED*** markers in diagnostics output")
	}
}
