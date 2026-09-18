package tools

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/sabnzbd"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transmission"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var serverStartTime = time.Now()

// DiagnosticsDeps bundles dependencies for runtime diagnostics
type DiagnosticsDeps struct {
	Config    *config.Config
	Registry  *arrservice.Registry
	SpecStore *openapi.Store
	TxClient  *transmission.Client
	QbClient  *qbit.Client
	SabClient *sabnzbd.Client
	Store     *store.Store
	Transcode transcode.Executor
}

func registerDiagnosticsTools(s *server.MCPServer, d DiagnosticsDeps) {
	// diagnostics — runtime health, connectivity, and configuration inspection without leaking secrets
	s.AddTool(
		mcp.NewTool("diagnostics",
			mcp.WithDescription("diagnostics muestra configuración efectiva no sensible y redacta todos los secretos. Check runtime health, service connectivity, effective configuration, download clients, OpenAPI spec store, and SQLite database stats."),
			mcp.WithBoolean("check_connectivity", mcp.Description("Whether to ping external services and download clients (default true)")),
			mcp.WithBoolean("check_deep", mcp.Description("Also run the transcode worker's deep Doctor checks (default false). Doctor results are separate from readiness.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			checkConn := true
			if v, ok := req.GetArguments()["check_connectivity"].(bool); ok {
				checkConn = v
			}

			overallStatus := "ok"
			checkDeep, _ := req.GetArguments()["check_deep"].(bool)

			// Start worker probes before upstream checks so a slow *arr service
			// cannot starve readiness. Deep diagnostics are explicitly requested
			// and never decide whether the executor is available.
			var workerDone chan map[string]any
			if checkConn && d.Transcode != nil {
				workerDone = make(chan map[string]any, 1)
				go func() {
					workerDone <- inspectTranscodeAvailability(ctx, d.Transcode, checkDeep)
				}()
			}

			// 1. Effective configuration (redacted)
			effConfig := map[string]any{
				"config_file_loaded":   "",
				"state_path":           "",
				"allowed_read_roots":   []string{},
				"allowed_write_roots":  []string{},
				"root_validation":      []string{},
				"allow_destructive":    false,
				"max_response_size_kb": 100,
				"concurrency_limits": map[string]int{
					"api":          3,
					"mediainspect": 2,
				},
			}
			if d.Config != nil {
				effConfig["config_file_loaded"] = d.Config.LoadedPath
				dbPath := d.Config.Database.Path
				if dbPath == "" {
					dbPath = config.DefaultDatabasePath()
				}
				effConfig["state_path"] = dbPath
				effConfig["allowed_read_roots"] = d.Config.Media.AllowedReadRoots
				effConfig["allowed_write_roots"] = d.Config.Media.AllowedWriteRoots
				effConfig["root_validation"] = d.Config.ValidateRoots()
				effConfig["allow_destructive"] = d.Config.AllowDestructive
				if d.Config.MaxResponseSizeKB > 0 {
					effConfig["max_response_size_kb"] = d.Config.MaxResponseSizeKB
				}
				if d.Config.Concurrency.MaxAPISimultaneous > 0 {
					effConfig["concurrency_limits"].(map[string]int)["api"] = d.Config.Concurrency.MaxAPISimultaneous
				}
				if d.Config.Concurrency.MaxInspectSimultaneous > 0 {
					effConfig["concurrency_limits"].(map[string]int)["mediainspect"] = d.Config.Concurrency.MaxInspectSimultaneous
				}
				if d.Config.Queue.Listen != "" {
					queueInfo := map[string]any{
						"listen": d.Config.Queue.Listen,
					}
					if d.Config.Queue.Token != "" {
						queueInfo["token"] = "***REDACTED***"
					}
					effConfig["queue"] = queueInfo
				}
			}

			// 2. Upstream services health
			servicesHealth := make(map[string]any)
			if d.Registry != nil {
				for _, svcName := range d.Registry.List() {
					svc, err := d.Registry.Get(svcName)
					if err != nil {
						continue
					}
					cleanURL := redactURL(svc.BaseURL)
					sh := map[string]any{
						"url":    cleanURL,
						"status": "configured",
					}
					if svc.Config.APIKey != "" {
						sh["api_key"] = "***REDACTED***"
					}
					if svc.Config.AuthMethod != "" {
						sh["auth_method"] = svc.Config.AuthMethod
					}
					if svc.Config.AuthHeader != "" {
						sh["auth_header"] = svc.Config.AuthHeader
					}
					if checkConn {
						pingStart := time.Now()
						status := svc.Ping(ctx)
						sh["status"] = status
						sh["latency_ms"] = time.Since(pingStart).Milliseconds()
						if status != "ok" {
							overallStatus = "degraded"
						}
					}
					servicesHealth[svcName] = sh
				}
			}

			// 3. Download clients
			dlClients := make(map[string]any)
			// qBittorrent
			qbInfo := map[string]any{"configured": d.QbClient != nil}
			if d.Config != nil && d.Config.QBittorrent.URL != "" {
				qbInfo["url"] = redactURL(d.Config.QBittorrent.URL)
				if d.Config.QBittorrent.Username != "" {
					qbInfo["username"] = d.Config.QBittorrent.Username
				}
				if d.Config.QBittorrent.Password != "" {
					qbInfo["password"] = "***REDACTED***"
				}
			}
			if d.QbClient != nil {
				qbInfo["status"] = "ok"
				if checkConn {
					tInfo, err := d.QbClient.GetTransferInfo(ctx)
					if err != nil {
						qbInfo["status"] = "error"
						qbInfo["error"] = err.Error()
						overallStatus = "degraded"
					} else {
						qbInfo["connection_status"] = tInfo.ConnectionStatus
						qbInfo["dht_nodes"] = tInfo.DHtNodes
					}
				}
			}
			dlClients["qbittorrent"] = qbInfo

			// Transmission
			txInfo := map[string]any{"configured": d.TxClient != nil}
			if d.Config != nil && d.Config.Transmission.URL != "" {
				txInfo["url"] = redactURL(d.Config.Transmission.URL)
				if d.Config.Transmission.Username != "" {
					txInfo["username"] = d.Config.Transmission.Username
				}
				if d.Config.Transmission.Password != "" {
					txInfo["password"] = "***REDACTED***"
				}
			}
			if d.TxClient != nil {
				txInfo["status"] = "ok"
			}
			dlClients["transmission"] = txInfo

			// SABnzbd
			sabInfo := map[string]any{"configured": d.SabClient != nil}
			if d.Config != nil && d.Config.SABnzbd.URL != "" {
				sabInfo["url"] = redactURL(d.Config.SABnzbd.URL)
				if d.Config.SABnzbd.APIKey != "" {
					sabInfo["api_key"] = "***REDACTED***"
				}
			}
			if d.SabClient != nil {
				sabInfo["status"] = "ok"
			}
			dlClients["sabnzbd"] = sabInfo

			// 4. OpenAPI spec store
			specInfo := map[string]any{
				"specs_loaded": 0,
				"services":     []string{},
			}
			if d.SpecStore != nil {
				specs := d.SpecStore.LoadedServices()
				specInfo["specs_loaded"] = len(specs)
				specInfo["services"] = specs
			}

			// 5. SQLite database stats
			dbStats := map[string]any{
				"configured": d.Store != nil,
			}
			if d.Store != nil {
				activeActions, _ := d.Store.ListActionInstances("running", 100)
				waitingActions, _ := d.Store.ListActionInstances("waiting_external", 100)
				decisionActions, _ := d.Store.ListActionInstances("waiting_decision", 100)
				maintItems, _ := d.Store.ListItems(store.ItemFilter{Status: "open", Limit: 100})

				dbStats["active_actions"] = len(activeActions)
				dbStats["waiting_external_actions"] = len(waitingActions)
				dbStats["waiting_decision_actions"] = len(decisionActions)
				dbStats["active_maintenance_jobs"] = len(maintItems)
			}

			// 6. Transcode executor. Automatic execution is HTTP-only; SSH is
			// retired/admin-only and is never wired here.
			var tcInfo map[string]any
			if d.Transcode != nil {
				tcInfo = map[string]any{
					"configured": true,
					"executor":   "http",
					"status":     "not_checked",
				}
				if workerDone != nil {
					for key, value := range <-workerDone {
						tcInfo[key] = value
					}
					if tcInfo["status"] != "ok" {
						overallStatus = "degraded"
					}
				}
			}

			res := map[string]any{
				"status":           overallStatus,
				"uptime_seconds":   int64(time.Since(serverStartTime).Seconds()),
				"effective_config": effConfig,
				"services":         servicesHealth,
				"download_clients": dlClients,
				"openapi_store":    specInfo,
				"database":         dbStats,
			}
			if tcInfo != nil {
				res["transcode"] = tcInfo
			}

			return toolJSON(res), nil
		},
	)

	// action_history — query enriched action audit log
	s.AddTool(
		mcp.NewTool("action_history",
			mcp.WithDescription("Query the enriched action audit log by media name, service, or action (e.g. search what happened to 'Evangelion' or inspect recent scan_library runs)."),
			mcp.WithString("media", mcp.Description("Optional filter by media title, path, or identifier")),
			mcp.WithString("service", mcp.Description("Optional filter by service name (e.g. sonarr, radarr)")),
			mcp.WithString("action", mcp.Description("Optional filter by action type (e.g. scan_library, safe_replace, action_completed)")),
			mcp.WithString("limit", mcp.Description("Max log records to return (default 20, max 100)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if d.Store == nil {
				return toolErr("maintenance store is not configured"), nil
			}

			args := req.GetArguments()
			media := strings.TrimSpace(argString(args, "media", ""))
			service := strings.TrimSpace(argString(args, "service", ""))
			action := strings.TrimSpace(argString(args, "action", ""))
			limit := int(argInt64(args, "limit", 20))

			records, err := d.Store.QueryActionLog(media, service, action, limit)
			if err != nil {
				return toolErr("querying action log: %v", err), nil
			}

			return toolJSON(map[string]any{
				"count":   len(records),
				"records": records,
			}), nil
		},
	)
}

// inspectTranscodeAvailability keeps process health, submit readiness and deep
// diagnostics independent. Each probe runs concurrently within the caller's
// deadline. Executors without an availability API are reported as unknown;
// Doctor is never silently used as their readiness gate.
func inspectTranscodeAvailability(ctx context.Context, executor transcode.Executor, deep bool) map[string]any {
	result := map[string]any{"status": "unknown", "doctor": map[string]any{"status": "not_checked"}}
	availability, supported := executor.(interface {
		Health(context.Context) error
		Ready(context.Context) error
	})
	type probe struct {
		name string
		err  error
	}
	done := make(chan probe, 3)
	pending := make(map[string]bool)
	start := func(name string, run func(context.Context) error) {
		pending[name] = true
		go func() { done <- probe{name, run(ctx)} }()
	}
	if supported {
		start("health", availability.Health)
		start("ready", availability.Ready)
	} else {
		result["health"] = map[string]any{"status": "unsupported"}
		result["ready"] = map[string]any{"status": "unsupported"}
	}
	if deep {
		start("doctor", executor.Doctor)
	}
	setResult := func(name string, err error) {
		status := map[string]any{"status": "ok"}
		if err != nil {
			status["status"] = "degraded"
			status["error"] = err.Error()
			class := "worker_unavailable"
			if name == "doctor" {
				class = "diagnostic_failed"
				if errors.Is(err, context.DeadlineExceeded) {
					class = "diagnostic_timeout"
				} else if errors.Is(err, context.Canceled) {
					class = "diagnostic_cancelled"
				}
			} else if transcode.IsTransportUncertain(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				class = "worker_reachability_unknown"
				status["status"] = "unknown"
			}
			status["error_class"] = class
		}
		result[name] = status
		if name == "ready" {
			result["status"] = status["status"]
			result["can_accept_jobs"] = err == nil
			if status["status"] == "unknown" {
				result["can_accept_jobs"] = nil
			}
			if err != nil {
				result["error"] = err.Error()
			}
		}
	}
	for len(pending) > 0 {
		select {
		case p := <-done:
			delete(pending, p.name)
			setResult(p.name, p.err)
		case <-ctx.Done():
			// Drain already finished probes before labelling the rest timed out.
			// A slow deep check must not erase a readiness result already received.
			for {
				select {
				case p := <-done:
					delete(pending, p.name)
					setResult(p.name, p.err)
				default:
					for name := range pending {
						setResult(name, ctx.Err())
					}
					return result
				}
			}
		}
	}
	return result
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	// Strip userinfo
	u.User = nil
	// Strip query params (which may carry apikey)
	u.RawQuery = ""
	return u.String()
}
