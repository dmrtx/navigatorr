// Command navigatorr-gateway is the stateless MCP gateway. It serves the
// tools that need no state of their own (API documentation, call_api and the
// download clients) and forwards the stateful ones to the Transcode server
// over MCP. It reads the same config.yaml as the server, plus a "gateway"
// block (see config.gateway.yaml.example).
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/arrtools"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/internal"
	"github.com/jakenesler/navigatorr/internal/mcpkey"
	"github.com/jakenesler/navigatorr/internal/mcpproxy"
	"github.com/jakenesler/navigatorr/openapi"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/sabnzbd"
	"github.com/jakenesler/navigatorr/transmission"
	"github.com/mark3labs/mcp-go/server"
)

const (
	// drainTimeout is longer than the proxy's 55 s call budget, so a call in
	// flight at SIGTERM can finish.
	drainTimeout = 60 * time.Second
	// closeTimeout bounds the DELETE that ends the upstream session.
	closeTimeout = 2 * time.Second
)

func main() {
	configPath := flag.String("config", "", "path to config.yaml (default: ~/.config/navigatorr/config.yaml)")
	listenFlag := flag.String("listen", "", "listen address (default: gateway.listen, or 127.0.0.1:8099)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Gateway.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	listen := cfg.Gateway.ListenAddr()
	if *listenFlag != "" {
		listen = *listenFlag
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h, fr, err := build(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: gateway cannot bind %s: %v\n", listen, err)
		os.Exit(1)
	}
	internal.Logf("navigatorr gateway listening on %s, upstream %s", ln.Addr(), cfg.Gateway.UpstreamURL)
	go func() {
		if err := fr.Warm(ctx); err != nil {
			internal.Logf("Transcode catalog not loaded at start-up, will retry on the next tools/list: %v", err)
		}
	}()

	if err := serve(ctx, ln, h, fr, drainTimeout, closeTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
	internal.Logf("gateway stopped cleanly")
}

// build wires the own tools, the proxy frontier and /healthz. It fails when
// the key file is configured but unusable, or when an own tool collides with
// the proxy list.
func build(ctx context.Context, cfg *config.Config) (http.Handler, *mcpproxy.Frontier, error) {
	var apiKey string
	if cfg.Gateway.KeyFile != "" {
		k, err := mcpkey.Load(cfg.Gateway.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("gateway.key_file: %w", err)
		}
		apiKey = k
	}

	registry := arrservice.NewRegistry(cfg)
	var specStore *openapi.Store
	if cfg.Gateway.CacheDir != "" {
		specStore = openapi.NewStoreAt(cfg, cfg.Gateway.CacheDir)
	} else {
		specStore = openapi.NewStore(cfg)
	}
	// Loading specs reaches the services over the network; do not hold the
	// listener back for it. Tools also load lazily.
	go func() {
		specStore.LoadAll(ctx)
		specStore.RetryMissing(ctx, 5*time.Second, 5*time.Minute)
	}()

	s := server.NewMCPServer(
		"navigatorr",
		"1.0.0",
		server.WithToolCapabilities(true),
		server.WithInstructions("Navigatorrr provides tools to browse *arr service API documentation, make authenticated API calls to Sonarr/Radarr/Lidarr/Seerr/etc., manage Transmission torrents, manage qBittorrent torrents, and manage SABnzbd Usenet downloads. Use list_services to see available services, search_api to find endpoints, and call_api to make requests."),
	)
	registerOwnTools(s, cfg, registry, specStore)

	// The default session id manager is the same stateless one the server
	// uses, so a session id issued by either is valid at both.
	mcpHTTP := server.NewStreamableHTTPServer(s, server.WithEndpointPath("/mcp"))
	fr, err := mcpproxy.New(mcpproxy.Options{
		Server:      s,
		Next:        mcpHTTP,
		UpstreamURL: cfg.Gateway.UpstreamURL,
		APIKey:      apiKey,
	})
	if err != nil {
		return nil, nil, err
	}

	mux := http.NewServeMux()
	// Liveness only: it must not depend on Transcode.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/mcp", fr)
	mux.Handle("/mcp/", fr)
	return mux, fr, nil
}

// registerOwnTools registers exactly the stateless tools the single process
// registers through arrtools: the same calls as tools.RegisterAll.
func registerOwnTools(s *server.MCPServer, cfg *config.Config, registry *arrservice.Registry, specStore *openapi.Store) {
	arrtools.RegisterDocTools(s, registry, specStore)
	arrtools.RegisterAPICallTool(s, registry, specStore, cfg.MaxResponseSizeKB, cfg.AllowDestructive)
	if cfg.Transmission.URL != "" {
		arrtools.RegisterTransmissionTools(s, transmission.NewClient(cfg.Transmission.URL, cfg.Transmission.Username, cfg.Transmission.Password), cfg.AllowDestructive)
	}
	if cfg.QBittorrent.URL != "" {
		arrtools.RegisterQbitTools(s, qbit.NewClient(cfg.QBittorrent.URL, cfg.QBittorrent.Username, cfg.QBittorrent.Password), cfg.AllowDestructive)
	}
	if cfg.SABnzbd.URL != "" {
		arrtools.RegisterSabnzbdTools(s, sabnzbd.NewClient(cfg.SABnzbd.URL, cfg.SABnzbd.URLBase, cfg.SABnzbd.APIKey), cfg.AllowDestructive)
	}
}

// newHTTPServer has no WriteTimeout: a proxied call may legitimately take up
// to its 55 s budget before the first byte is written.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// serve runs until ctx is cancelled (SIGTERM), then stops accepting requests,
// drains calls in flight for up to drain, and only then ends the upstream
// session, so no call loses its session mid-way.
func serve(ctx context.Context, ln net.Listener, h http.Handler, fr *mcpproxy.Frontier, drain, closeAfter time.Duration) error {
	srv := newHTTPServer(h)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	internal.Logf("shutting down: draining calls in flight for up to %s", drain)
	sctx, cancel := context.WithTimeout(context.Background(), drain)
	err := srv.Shutdown(sctx)
	cancel()
	cctx, cancel := context.WithTimeout(context.Background(), closeAfter)
	fr.Close(cctx)
	cancel()
	return err
}
