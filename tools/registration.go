package tools

import (
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/queue"
	"github.com/jakenesler/navigatorr/sabnzbd"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transmission"
	"github.com/mark3labs/mcp-go/server"
)

// RegisterAll registers all tools with the MCP server. The original tools
// keep their names and behavior; the maintenance agent tools are added
// alongside and are only registered when a state store is available.
func RegisterAll(s *server.MCPServer, cfg *config.Config, registry *arrservice.Registry, specStore *openapi.Store, txClient *transmission.Client, qbClient *qbit.Client, sabClient *sabnzbd.Client, qStore *queue.Store) {
	registerDocTools(s, registry, specStore)
	registerAPICallTool(s, registry, specStore, cfg.MaxResponseSizeKB, cfg.AllowDestructive)
	if txClient != nil {
		registerTransmissionTools(s, txClient, cfg.AllowDestructive)
	}
	if qbClient != nil {
		registerQbitTools(s, qbClient, cfg.AllowDestructive)
	}
	if sabClient != nil {
		registerSabnzbdTools(s, sabClient, cfg.AllowDestructive)
	}
	if qStore != nil {
		registerQueueTools(s, qStore)
	}
}

// RegisterMaintenance wires the persistent maintenance-agent tools. It is
// separate from RegisterAll so the classic tools never depend on SQLite:
// with a nil mStore this is a no-op and the server behaves exactly as before.
func RegisterMaintenance(s *server.MCPServer, cfg *config.Config, registry *arrservice.Registry, qbClient *qbit.Client, mStore *store.Store) {
	if mStore == nil {
		return
	}
	resolver, err := fsop.NewResolver(cfg.Media.AllowedReadRoots, cfg.Media.AllowedWriteRoots)
	if err != nil {
		resolver, _ = fsop.NewResolver(nil, nil)
	}
	ffprobe := cfg.Media.FfprobePath
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	d := &Deps{Store: mStore, Config: cfg, Fs: resolver, Ffprobe: ffprobe}
	SeedDefaults(mStore, cfg)
	registerMemoryTools(s, d)
	registerMaintenanceTools(s, d)
	registerMediaTools(s, d, registry, qbClient)
	registerSafeReplaceTools(s, d, registry, qbClient, cfg.AllowDestructive)
	registerScanTools(s, d, registry)
	registerFsTools(s, d)

	actEngine := action.NewEngine(action.EngineDeps{
		Store:     mStore,
		Config:    cfg,
		Registry:  registry,
		Qbit:      qbClient,
		Fs:        resolver,
		Ffprobe:   ffprobe,
		StartTime: time.Now(),
	})
	registerActionTools(s, actEngine)
}

// RegisterDiagnostics registers the diagnostics and action audit log tools.
func RegisterDiagnostics(s *server.MCPServer, cfg *config.Config, registry *arrservice.Registry, specStore *openapi.Store, txClient *transmission.Client, qbClient *qbit.Client, sabClient *sabnzbd.Client, mStore *store.Store) {
	registerDiagnosticsTools(s, DiagnosticsDeps{
		Config:    cfg,
		Registry:  registry,
		SpecStore: specStore,
		TxClient:  txClient,
		QbClient:  qbClient,
		SabClient: sabClient,
		Store:     mStore,
	})
}
