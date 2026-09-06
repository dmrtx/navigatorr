package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/internal"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/queue"
	"github.com/jakenesler/navigatorr/sabnzbd"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/tools"
	"github.com/jakenesler/navigatorr/transmission"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	configPath := flag.String("config", "", "path to config.yaml (default: ~/.config/navigatorr/config.yaml)")
	flag.Parse()

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	internal.Logf("loaded config with %d services", len(cfg.Services))

	// Build service registry
	registry := arrservice.NewRegistry(cfg)

	// Build OpenAPI spec store and load specs
	specStore := openapi.NewStore(cfg)
	ctx := context.Background()
	specStore.LoadAll(ctx)

	// Build Transmission client if configured
	var txClient *transmission.Client
	if cfg.Transmission.URL != "" {
		txClient = transmission.NewClient(
			cfg.Transmission.URL,
			cfg.Transmission.Username,
			cfg.Transmission.Password,
		)
		internal.Logf("transmission client configured: %s", cfg.Transmission.URL)
	}

	// Build qBittorrent client if configured
	var qbClient *qbit.Client
	if cfg.QBittorrent.URL != "" {
		qbClient = qbit.NewClient(
			cfg.QBittorrent.URL,
			cfg.QBittorrent.Username,
			cfg.QBittorrent.Password,
		)
		internal.Logf("qbittorrent client configured: %s", cfg.QBittorrent.URL)
	}

	// Build SABnzbd client if configured
	var sabClient *sabnzbd.Client
	if cfg.SABnzbd.URL != "" {
		sabClient = sabnzbd.NewClient(
			cfg.SABnzbd.URL,
			cfg.SABnzbd.URLBase,
			cfg.SABnzbd.APIKey,
		)
		internal.Logf("sabnzbd client configured: %s", cfg.SABnzbd.URL)
	}

	// Open the request queue. This is always available to the MCP tools so an
	// agent can drain a backlog even when the HTTP endpoint is disabled.
	queuePath := cfg.Queue.Path
	if queuePath == "" {
		queuePath = config.DefaultQueuePath()
	}
	qStore, err := queue.Open(queuePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer qStore.Close()
	internal.Logf("request queue at %s", queuePath)

	// Open the persistent maintenance-agent database. It lives in the cache
	// directory deployments already mount as a volume, so jobs, preferences
	// and decisions survive container restarts.
	dbPath := cfg.Database.Path
	if dbPath == "" {
		dbPath = config.DefaultDatabasePath()
	}
	mStore, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer mStore.Close()
	internal.Logf("maintenance database at %s", dbPath)

	// Serve the HTTP ingest endpoint alongside stdio when configured.
	if cfg.Queue.Listen != "" {
		// Refuse to listen without a token rather than serving openly. Anything
		// posted here is later read and acted on by an agent holding write
		// credentials to every configured service.
		qSrv, err := queue.NewServer(qStore, cfg.Queue.Token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		srv := &http.Server{
			Addr:              cfg.Queue.Listen,
			Handler:           qSrv.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		// Bind failures have to be loud. Logging and carrying on leaves a
		// process that looks healthy while silently accepting nothing.
		ln, err := net.Listen("tcp", cfg.Queue.Listen)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: queue endpoint cannot bind %s: %v\n", cfg.Queue.Listen, err)
			os.Exit(1)
		}
		internal.Logf("queue HTTP endpoint listening on %s", ln.Addr())
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				internal.Errorf("queue HTTP server stopped: %v", err)
			}
		}()
	}

	// Create MCP server
	s := server.NewMCPServer(
		"navigatorr",
		"1.0.0",
		server.WithToolCapabilities(true),
		server.WithInstructions("Navigatorrr provides tools to browse *arr service API documentation, make authenticated API calls to Sonarr/Radarr/Lidarr/Seerr/etc., manage Transmission torrents, manage qBittorrent torrents, and manage SABnzbd Usenet downloads. Use list_services to see available services, search_api to find endpoints, and call_api to make requests."),
	)

	// Register all tools
	tools.RegisterAll(s, cfg, registry, specStore, txClient, qbClient, sabClient, qStore)
	tools.RegisterMaintenance(s, cfg, registry, qbClient, mStore)
	tools.RegisterDiagnostics(s, cfg, registry, specStore, txClient, qbClient, sabClient, mStore)

	internal.Logf("starting navigatorr MCP server (stdio)")

	// Serve over stdio
	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}
