package tools

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/sabnzbd"
	"github.com/jakenesler/navigatorr/store"
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
}

func registerDiagnosticsTools(s *server.MCPServer, d DiagnosticsDeps) {
	// diagnostics — runtime health, connectivity, and configuration inspection without leaking secrets
	s.AddTool(
		mcp.NewTool("diagnostics",
			mcp.WithDescription("Check runtime health, service connectivity, effective configuration, download clients, OpenAPI spec store, and SQLite database stats. Redacts all API keys and credentials."),
			mcp.WithBoolean("check_connectivity", mcp.Description("Whether to ping external services and download clients (default true)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			checkConn := true
			if v, ok := req.GetArguments()["check_connectivity"].(bool); ok {
				checkConn = v
			}

			overallStatus := "ok"

			// 1. Effective configuration (redacted)
			effConfig := map[string]any{
				"allowed_read_roots":   []string{},
				"allowed_write_roots":  []string{},
				"allow_destructive":    false,
				"max_response_size_kb": 100,
				"concurrency_limits": map[string]int{
					"api":          3,
					"mediainspect": 2,
				},
			}
			if d.Config != nil {
				effConfig["allowed_read_roots"] = d.Config.Media.AllowedReadRoots
				effConfig["allowed_write_roots"] = d.Config.Media.AllowedWriteRoots
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
			if d.TxClient != nil {
				txInfo["status"] = "ok"
			}
			dlClients["transmission"] = txInfo

			// SABnzbd
			sabInfo := map[string]any{"configured": d.SabClient != nil}
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

			res := map[string]any{
				"status":           overallStatus,
				"uptime_seconds":   int64(time.Since(serverStartTime).Seconds()),
				"effective_config": effConfig,
				"services":         servicesHealth,
				"download_clients": dlClients,
				"openapi_store":    specInfo,
				"database":         dbStats,
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
