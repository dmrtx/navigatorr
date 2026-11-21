package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/internal"
	"github.com/jakenesler/navigatorr/internal/mcpkey"
	"github.com/jakenesler/navigatorr/internal/nodesclient"
	"github.com/jakenesler/navigatorr/internal/nodesproxy"
	"github.com/jakenesler/navigatorr/internal/nodeswire"
	"github.com/jakenesler/navigatorr/nodes"
)

// nodesBackend is where this server's builds registry lives: in this process
// (nodes.url empty, the default) or in the separate Nodes process (nodes.url
// set). The two never mix: with a url the in-process registry is never opened,
// imported, started or served, and a bad key stops the server instead of
// falling back to it.
type nodesBackend struct {
	// Service is the in-process registry; nil when Nodes runs apart.
	Service *nodes.Service
	// Client reads and writes the remote registry; nil in-process.
	Client *nodesclient.Client
	// Proxy forwards the builds routes to the remote registry; nil in-process.
	Proxy *nodesproxy.Handler
}

// nodesInputs is what joining the registry to this server needs.
type nodesInputs struct {
	// DBPath is the main database; the in-process registry's file derives from it.
	DBPath    string
	Topology  nodes.Topology
	Transcode nodes.TranscodeView
}

// openNodes decides and opens the registry. open is nodes.Open (a parameter so
// a test can prove it is never called when nodes.url is set).
func openNodes(cfg *config.Config, in nodesInputs, open func(nodes.Options) (*nodes.Service, error)) (*nodesBackend, error) {
	if cfg.Nodes.URL == "" {
		path := cfg.Database.NodesDatabasePath(in.DBPath)
		svc, err := open(nodes.Options{Path: path, Topology: in.Topology, Transcode: in.Transcode, Retention: cfg.Builds.RetentionDuration})
		if err != nil {
			return nil, fmt.Errorf("nodes database %s: %w", path, err)
		}
		internal.Logf("nodes database at %s", path)
		return &nodesBackend{Service: svc}, nil
	}
	key, err := mcpkey.Load(cfg.Nodes.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("nodes.url is set but nodes.key_file cannot be used (the server does not fall back to an in-process registry): %w", err)
	}
	var known func() []string
	if in.Topology != nil {
		known = in.Topology.Nodes
	}
	client, err := nodesclient.New(nodesclient.Options{BaseURL: cfg.Nodes.URL, Key: key, KnownNodes: known})
	if err != nil {
		return nil, fmt.Errorf("nodes.url: %w", err)
	}
	proxy, err := nodesproxy.New(nodesproxy.Options{BaseURL: cfg.Nodes.URL, Key: key})
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("nodes.url: %w", err)
	}
	internal.Logf("builds registry is the separate Nodes process at %s", cfg.Nodes.URL)
	return &nodesBackend{Client: client, Proxy: proxy}, nil
}

// Wire gives the engine its view of the registry.
func (b *nodesBackend) Wire(e *action.Engine) {
	switch {
	case b.Service != nil:
		nodeswire.Wire(e, b.Service)
	case b.Client != nil:
		nodeswire.WireRemote(e, b.Client)
	}
}

// Start runs the in-process registry's background work and returns what stops
// it (a no-op when there is none: Nodes apart runs by itself).
func (b *nodesBackend) Start(dbPath string) (stop func()) {
	if b.Service == nil {
		return func() {}
	}
	if _, err := b.Service.ImportLegacy(context.Background(), dbPath); err != nil {
		// Not fatal: the import is one transaction, so nothing was imported, and
		// its flag is only written when it succeeds: the next start tries again,
		// whatever the registry holds by then (rows already there are kept).
		internal.Logf("nodes: ERROR importing builds and pauses from the main database failed (the registry starts without them; the import is retried at the next start): %v", err)
	}
	return b.startService()
}

func (b *nodesBackend) startService() func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := b.Service.Start(ctx, 0)
	return func() {
		cancel()
		<-done
	}
}

// ConnectUI attaches the registry to the web UI: the registry itself in-process,
// the proxy to Nodes otherwise.
func (b *nodesBackend) ConnectUI(ui interface {
	SetNodes(*nodes.Service)
	SetNodesProxy(http.Handler)
}) {
	if b.Service != nil {
		ui.SetNodes(b.Service)
		return
	}
	ui.SetNodesProxy(b.Proxy)
}

// Close releases what the backend holds.
func (b *nodesBackend) Close() {
	if b.Service != nil {
		b.Service.Close()
	}
	if b.Proxy != nil {
		b.Proxy.Close()
	}
	if b.Client != nil {
		b.Client.Close()
	}
}
