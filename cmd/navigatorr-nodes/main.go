// Command navigatorr-nodes runs the builds registry (package nodes) on its
// own. It owns nodes.db: the one process that opens it, under the flock the
// registry takes. It never opens the main database, never imports builds from
// it (that move is made by the Transcode server before this binary takes over)
// and does not link the transcode engine: node pauses are read over HTTP from
// the engine (transcode_url). It reads the same config.yaml as the server, plus
// a "nodes" block and database.nodes_path.
//
//	navigatorr-nodes [-config config.yaml]                  run the registry
//	navigatorr-nodes check --path nodes.db [--checkpoint]   inspect a stopped database
//
// Every route but /healthz needs the service key (nodes.key_file) as
// X-Api-Key or Authorization: Bearer.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/internal"
	"github.com/jakenesler/navigatorr/internal/mcpkey"
	"github.com/jakenesler/navigatorr/internal/transcodeview"
	"github.com/jakenesler/navigatorr/nodes"
)

const (
	// drainTimeout outlasts the longest build-log long-poll (nodes.BuildLogMaxWait,
	// 25 s) with room to spare, so a reader in flight at SIGTERM is answered
	// before the database closes.
	drainTimeout = 35 * time.Second
	// healthTimeout bounds one /healthz probe.
	healthTimeout = 2 * time.Second
	// handlerGrace bounds the wait, after the server is closed, for the
	// requests it cancelled to return before the database closes.
	handlerGrace = 5 * time.Second
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "check" {
		os.Exit(runCheck(os.Args[2:], os.Stdout, os.Stderr))
	}
	configPath, ok := parseServeArgs(os.Args[1:], os.Stderr)
	if !ok {
		os.Exit(2)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	in, err := build(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", in.listen)
	if err != nil {
		in.svc.Close()
		fmt.Fprintf(os.Stderr, "error: nodes cannot bind %s: %v\n", in.listen, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	internal.Logf("navigatorr nodes listening on %s, database %s", ln.Addr(), in.dbPath)
	if err := in.serve(ctx, ln, drainTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
	internal.Logf("nodes stopped cleanly")
}

// parseServeArgs reads the flags of the serve mode. It refuses stray
// positional arguments (a mistyped subcommand must not start the server).
func parseServeArgs(args []string, stderr io.Writer) (string, bool) {
	fs := flag.NewFlagSet("navigatorr-nodes", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to config.yaml (default: ~/.config/navigatorr/config.yaml)")
	if err := fs.Parse(args); err != nil {
		return "", false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\nusage: navigatorr-nodes [-config config.yaml] | navigatorr-nodes check --path nodes.db [--checkpoint]\n", fs.Arg(0))
		return "", false
	}
	return *configPath, true
}

// instance is a configured, opened registry and the handler in front of it.
type instance struct {
	svc      *nodes.Service
	handler  http.Handler
	inflight *inflight
	listen   string
	dbPath   string
}

// build validates the configuration and opens the registry. It refuses to
// start on a missing or short service key, an empty database.nodes_path (the
// path is never derived from the main database, which is never opened) or a
// listen address outside loopback without nodes.allow_non_loopback.
func build(cfg *config.Config) (*instance, error) {
	n := cfg.Nodes
	if err := n.CheckListen(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(n.KeyFile) == "" {
		return nil, errors.New("nodes.key_file is required: every route but /healthz needs the service key")
	}
	key, err := mcpkey.Load(n.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("nodes.key_file: %w", err)
	}
	dbPath := strings.TrimSpace(cfg.Database.NodesPath)
	if dbPath == "" {
		return nil, errors.New("database.nodes_path is required: Nodes never derives its database from the main one")
	}

	var names []string
	if cfg.Transcode.Enabled {
		// The same worker list the server builds its workers from.
		if names, err = cfg.Transcode.NodeNames(); err != nil {
			return nil, fmt.Errorf("transcode workers: %w", err)
		}
	}
	view, err := transcodeViewFor(n)
	if err != nil {
		return nil, err
	}
	opts := nodes.Options{Path: dbPath, Topology: topology(names), Retention: cfg.Builds.RetentionDuration}
	if view != nil {
		opts.Transcode = view
	}
	svc, err := nodes.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("nodes database %s: %w", dbPath, err)
	}
	fl := newInflight()
	return &instance{svc: svc, handler: fl.wrap(newHandler(svc, key)), inflight: fl, listen: n.ListenAddr(), dbPath: dbPath}, nil
}

// transcodeViewFor is the engine view, or nil (no transcode block) when no
// transcode_url is set.
func transcodeViewFor(n config.NodesConfig) (nodes.TranscodeView, error) {
	if strings.TrimSpace(n.TranscodeURL) == "" {
		if strings.TrimSpace(n.TranscodeKeyFile) != "" {
			return nil, errors.New("nodes.transcode_key_file needs nodes.transcode_url")
		}
		return nil, nil
	}
	var key string
	if strings.TrimSpace(n.TranscodeKeyFile) != "" {
		k, err := mcpkey.Load(n.TranscodeKeyFile)
		if err != nil {
			return nil, fmt.Errorf("nodes.transcode_key_file: %w", err)
		}
		key = k
	}
	c, err := transcodeview.New(transcodeview.Options{BaseURL: strings.TrimSpace(n.TranscodeURL), APIKey: key})
	if err != nil {
		return nil, fmt.Errorf("nodes.transcode_url: %w", err)
	}
	return viewAdapter{c}, nil
}

// viewAdapter is nodes.TranscodeView over the engine's HTTP client. A failure
// is "no view": the nodes show without a transcode block, never an error (N3).
type viewAdapter struct{ c *transcodeview.Client }

func (a viewAdapter) TranscodeStates(ctx context.Context) map[string]nodes.TranscodeState {
	states, err := a.c.States(ctx)
	if err != nil {
		return nil
	}
	out := make(map[string]nodes.TranscodeState, len(states))
	for name, s := range states {
		out[name] = nodes.TranscodeState{Paused: s.Paused, Mode: s.Mode, Since: s.Since, HeldBy: s.HeldBy, Message: s.Message}
	}
	return out
}

// topology is nodes.Topology over the configured worker names.
type topology []string

func (t topology) Node(target string) (string, bool) {
	for _, n := range t {
		if n == target {
			return n, true
		}
	}
	return "", false
}

func (t topology) Nodes() []string {
	out := append([]string(nil), t...)
	sort.Strings(out)
	return out
}

// newHandler routes /healthz (open) and everything else behind the key: the
// /nodes/v1 API and its spec, and the legacy builds API at its own paths.
func newHandler(svc *nodes.Service, key string) http.Handler {
	api := http.NewServeMux()
	api.Handle("GET "+nodes.SpecPath, nodes.SpecHandler())
	api.Handle(nodes.BasePath+"/", svc.Handler())
	legacy := svc.LegacyHandler()
	api.Handle(nodes.LegacyBasePath, legacy)
	api.Handle(nodes.LegacyBasePath+"/", legacy)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := svc.Healthy(ctx); err != nil {
			internal.Logf("nodes: unhealthy: %v", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("unavailable\n"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	// The same 401 body as the other JSON APIs of the repo.
	deny := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "authentication required", "code": "unauthorized",
			"hint": "send the nodes service key as the X-Api-Key header (or Authorization: Bearer)",
		})
	})
	mux.Handle("/", mcpkey.RequireWith(key, api, deny))
	return mux
}

// newHTTPServer has no WriteTimeout: a build-log long-poll holds its answer for
// up to nodes.BuildLogMaxWait seconds.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       60 * time.Second,
	}
}

// serve runs the reconciler and the HTTP server until ctx is cancelled
// (SIGTERM or SIGINT), then drains requests in flight for up to drain. Whatever
// happened, it then closes the server, stops the reconciler and closes the
// registry, so the database ends without a -wal file and the lock is released.
func (in *instance) serve(ctx context.Context, ln net.Listener, drain time.Duration) error {
	srv := newHTTPServer(in.handler)
	// Every request context derives from base, cancelled once the server is
	// closed: net/http alone does not cancel a request whose body is unread.
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv.BaseContext = func(net.Listener) context.Context { return base }
	reconCtx, stopRecon := context.WithCancel(context.Background())
	reconDone := in.svc.Start(reconCtx, 0)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
		internal.Logf("shutting down: draining requests in flight for up to %s", drain)
		sctx, cancel := context.WithTimeout(context.Background(), drain)
		err = srv.Shutdown(sctx)
		cancel()
		if err != nil {
			internal.Logf("nodes: drain did not finish, closing connections: %v", err)
		}
	}
	_ = srv.Close()
	cancelBase()
	// Close does not wait for the requests it cancelled; the database must not
	// close (and the lock must not drop) under one. A request that reaches the
	// handler from now on is refused without touching the registry.
	if !in.inflight.closeAndWait(handlerGrace) {
		internal.Logf("nodes: handlers still running %s after close; closing the database anyway", handlerGrace)
	}
	stopRecon()
	<-reconDone
	if cerr := in.svc.Close(); cerr != nil {
		err = errors.Join(err, fmt.Errorf("closing the nodes database: %w", cerr))
	}
	return err
}

// inflight counts the HTTP requests being handled, so serve can wait for the
// ones srv.Close cancelled before it closes the database.
type inflight struct {
	mu     sync.Mutex
	n      int
	closed bool          // set by closeAndWait: new requests are refused
	zero   chan struct{} // closed while n == 0
}

func newInflight() *inflight {
	z := make(chan struct{})
	close(z)
	return &inflight{zero: z}
}

func (f *inflight) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		if f.n == 0 {
			f.zero = make(chan struct{})
		}
		f.n++
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.n--
			if f.n == 0 {
				close(f.zero)
			}
			f.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// closeAndWait refuses new requests and reports whether every request in
// flight finished within d.
func (f *inflight) closeAndWait(d time.Duration) bool {
	f.mu.Lock()
	f.closed = true
	z := f.zero
	f.mu.Unlock()
	select {
	case <-z:
		return true
	case <-time.After(d):
		return false
	}
}

// runCheck is `navigatorr-nodes check --path nodes.db [--checkpoint]`: it
// inspects a database nobody has open and exits 0 only when it is ready to be
// served. It fails when the lock is held, the integrity check fails, the
// legacy_import mark is missing, or a -wal file is present and --checkpoint
// was not given.
func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("path", "", "path to nodes.db")
	checkpoint := fs.Bool("checkpoint", false, "fold the -wal file into the database first (wal_checkpoint(TRUNCATE))")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: navigatorr-nodes check --path nodes.db [--checkpoint]")
		return 2
	}
	rep, err := nodes.Check(context.Background(), *path, *checkpoint)
	if err != nil {
		fmt.Fprintf(stderr, "check failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "database:      %s\n", rep.Path)
	fmt.Fprintf(stdout, "file owner:    uid %d, mode %04o\n", rep.FileUID, rep.FileMode)
	fmt.Fprintf(stdout, "dir owner:     uid %d, mode %04o\n", rep.DirUID, rep.DirMode)
	fmt.Fprintf(stdout, "-wal present:  %t\n", rep.WALPresent)
	fmt.Fprintf(stdout, "-shm present:  %t\n", rep.SHMPresent)
	if *checkpoint {
		fmt.Fprintf(stdout, "checkpointed:  %t\n", rep.Checkpointed)
	}
	if rep.Integrity != "" {
		fmt.Fprintf(stdout, "integrity:     %s\n", rep.Integrity)
	}
	switch {
	case rep.LegacyImportSet:
		fmt.Fprintf(stdout, "legacy_import: %s\n", rep.LegacyImport)
	case rep.LegacyImportMissing:
		fmt.Fprintf(stdout, "legacy_import: (missing)\n")
	}
	if !rep.Ok() {
		for _, p := range rep.Problems {
			fmt.Fprintf(stderr, "problem: %s\n", p)
		}
		return 1
	}
	fmt.Fprintln(stdout, "ok")
	return 0
}
