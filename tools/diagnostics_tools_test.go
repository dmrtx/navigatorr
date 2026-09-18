package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Only availability probes may run during diagnostics; invoking an encode
// operation through the embedded interface would panic.
type diagDoctorStub struct {
	transcode.Executor
	doctorCalls int32
	doctorFunc  func(ctx context.Context) error
	readyFunc   func(ctx context.Context) error
}

func (m *diagDoctorStub) Health(context.Context) error { return nil }

func (m *diagDoctorStub) Ready(ctx context.Context) error {
	if m.readyFunc != nil {
		return m.readyFunc(ctx)
	}
	return nil
}

func (m *diagDoctorStub) Doctor(ctx context.Context) error {
	atomic.AddInt32(&m.doctorCalls, 1)
	if m.doctorFunc != nil {
		return m.doctorFunc(ctx)
	}
	return nil
}

func callDiagnosticsWithContext(t *testing.T, s *server.MCPServer, ctx context.Context, args map[string]any) string {
	t.Helper()
	tool := s.GetTool("diagnostics")
	if tool == nil {
		t.Fatal("diagnostics tool was not registered")
	}
	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "diagnostics", Arguments: args}}
	res, err := tool.Handler(ctx, req)
	if err != nil {
		t.Fatalf("diagnostics returned a transport error: %v", err)
	}
	return resultText(t, res)
}

// A slow upstream probe must not starve the transcode Doctor check. Before the
// fix, Doctor only ran after the sequential probes, so under a bounded parent
// deadline it started against an already-expired context and reported degraded.
func TestDiagnosticsSlowProbeDoesNotStarveDoctor(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	cfg := &config.Config{
		Services: map[string]config.ServiceConfig{
			"radarr": {URL: slow.URL},
		},
	}
	reg := arrservice.NewRegistry(cfg)

	stub := &diagDoctorStub{
		doctorFunc: func(ctx context.Context) error {
			select {
			case <-time.After(50 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}

	s := server.NewMCPServer("test", "0.0.0")
	RegisterDiagnostics(s, cfg, reg, nil, nil, nil, nil, nil, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	txt := callDiagnosticsWithContext(t, s, ctx, map[string]any{"check_connectivity": true, "check_deep": true})

	var dMap map[string]any
	if err := json.Unmarshal([]byte(txt), &dMap); err != nil {
		t.Fatalf("decoding diagnostics output: %v", err)
	}
	tcInfo, ok := dMap["transcode"].(map[string]any)
	if !ok {
		t.Fatalf("missing transcode section: %v", dMap)
	}
	if tcInfo["status"] != "ok" {
		t.Errorf("expected transcode status ok despite slow probe, got %v (error=%v)", tcInfo["status"], tcInfo["error"])
	}
	if _, hasErr := tcInfo["error"]; hasErr {
		t.Errorf("expected no transcode error, got %v", tcInfo["error"])
	}
	if got := atomic.LoadInt32(&stub.doctorCalls); got != 1 {
		t.Errorf("expected Doctor called once, got %d", got)
	}
}

// A deep diagnostic failure must remain visible without declaring a ready
// worker unavailable.
func TestDiagnosticsDoctorErrorDoesNotMaskReadiness(t *testing.T) {
	stub := &diagDoctorStub{
		doctorFunc: func(ctx context.Context) error {
			return errors.New("doctor boom")
		},
	}

	s := server.NewMCPServer("test", "0.0.0")
	RegisterDiagnostics(s, &config.Config{}, nil, nil, nil, nil, nil, nil, stub)

	txt := callDiagnosticsWithContext(t, s, context.Background(), map[string]any{"check_connectivity": true, "check_deep": true})

	var dMap map[string]any
	if err := json.Unmarshal([]byte(txt), &dMap); err != nil {
		t.Fatalf("decoding diagnostics output: %v", err)
	}
	if dMap["status"] != "ok" {
		t.Errorf("expected overall status ok, got %v", dMap["status"])
	}
	tcInfo, ok := dMap["transcode"].(map[string]any)
	if !ok {
		t.Fatalf("missing transcode section: %v", dMap)
	}
	if tcInfo["status"] != "ok" || tcInfo["can_accept_jobs"] != true {
		t.Errorf("expected ready transcode worker, got %v", tcInfo)
	}
	doctor := tcInfo["doctor"].(map[string]any)
	if got, _ := doctor["error"].(string); !strings.Contains(got, "doctor boom") {
		t.Errorf("expected doctor error surfaced, got %q", got)
	}
}

func TestDiagnosticsReadinessFailureIsUnavailable(t *testing.T) {
	stub := &diagDoctorStub{readyFunc: func(context.Context) error {
		return errors.New("worker draining")
	}}
	result := inspectTranscodeAvailability(context.Background(), stub, false)
	if result["status"] != "degraded" || result["can_accept_jobs"] != false {
		t.Fatalf("readiness failure hidden: %v", result)
	}
	if atomic.LoadInt32(&stub.doctorCalls) != 0 {
		t.Fatal("ordinary diagnostics invoked deep Doctor")
	}
}

func TestDiagnosticsDeepTimeoutPreservesHealthyReadyResult(t *testing.T) {
	stub := &diagDoctorStub{doctorFunc: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := inspectTranscodeAvailability(ctx, stub, true)
	if result["status"] != "ok" || result["can_accept_jobs"] != true {
		t.Fatalf("deep timeout changed readiness: %v", result)
	}
	doctor := result["doctor"].(map[string]any)
	if doctor["error_class"] != "diagnostic_timeout" {
		t.Fatalf("missing specific diagnostic timeout: %v", doctor)
	}
}

func TestDiagnosticsCancelledQueryDoesNotDeclareWorkerDown(t *testing.T) {
	stub := &diagDoctorStub{readyFunc: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := inspectTranscodeAvailability(ctx, stub, false)
	ready := result["ready"].(map[string]any)
	if ready["error_class"] != "worker_reachability_unknown" || result["status"] != "unknown" || result["can_accept_jobs"] != nil {
		t.Fatalf("query cancellation was mistaken for worker outage: %v", result)
	}
}

// With connectivity checks disabled, Doctor must not be invoked at all.
func TestDiagnosticsSkipsDoctorWhenConnectivityDisabled(t *testing.T) {
	stub := &diagDoctorStub{}

	s := server.NewMCPServer("test", "0.0.0")
	RegisterDiagnostics(s, &config.Config{}, nil, nil, nil, nil, nil, nil, stub)

	txt := callDiagnosticsWithContext(t, s, context.Background(), map[string]any{"check_connectivity": false})

	var dMap map[string]any
	if err := json.Unmarshal([]byte(txt), &dMap); err != nil {
		t.Fatalf("decoding diagnostics output: %v", err)
	}
	if got := atomic.LoadInt32(&stub.doctorCalls); got != 0 {
		t.Errorf("expected Doctor not called when check_connectivity=false, got %d calls", got)
	}
	tcInfo, ok := dMap["transcode"].(map[string]any)
	if !ok {
		t.Fatalf("missing transcode section: %v", dMap)
	}
	if tcInfo["status"] != "not_checked" {
		t.Errorf("expected transcode status not_checked when connectivity skipped, got %v", tcInfo["status"])
	}
}

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
