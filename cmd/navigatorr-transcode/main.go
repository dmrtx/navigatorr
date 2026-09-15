package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/internal/transcodeworker"
	"github.com/jakenesler/navigatorr/transcode"
)

var (
	Version   string
	GitCommit string
)

func init() {
	if Version != "" || GitCommit != "" {
		transcodeworker.SetBuildMetadata(Version, GitCommit)
	}
}

func main() {
	var configPath string

	// Custom parsing to allow --config anywhere
	args := os.Args[1:]
	var subcmd string
	var subcmdArgs []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--config" || arg == "-config" {
			if i+1 < len(args) {
				configPath = args[i+1]
				i++
			}
		} else if stringsHasPrefix(arg, "--config=") {
			configPath = arg[9:]
		} else if stringsHasPrefix(arg, "-config=") {
			configPath = arg[8:]
		} else if subcmd == "" {
			subcmd = arg
		} else {
			subcmdArgs = append(subcmdArgs, arg)
		}
	}

	if subcmd == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s [--config <path>] <doctor|capabilities|submit|status|cancel|serve|_internal_run|benchmark_submit|benchmark_status|benchmark_cancel|_internal_benchmark> [args...]\n", os.Args[0])
		os.Exit(1)
	}

	cfg, err := transcodeworker.LoadWorkerConfig(configPath)
	if err != nil {
		printJSON(map[string]any{"error": err.Error()})
		os.Exit(1)
	}

	worker := transcodeworker.NewWorker(cfg)
	ctx := context.Background()

	selfExe, err := os.Executable()
	if err != nil {
		selfExe = os.Args[0]
	}

	switch subcmd {
	case "doctor":
		res := worker.Doctor(ctx)
		printJSON(res)
		if !res.OK {
			os.Exit(1)
		}

	case "capabilities":
		caps, err := worker.Capabilities(ctx)
		if err != nil {
			printJSON(map[string]any{"error": err.Error(), "capabilities": caps})
			os.Exit(1)
		}
		printJSON(caps)

	case "submit":
		inputData, err := io.ReadAll(os.Stdin)
		if err != nil {
			printJSON(map[string]any{"error": fmt.Sprintf("reading stdin: %v", err)})
			os.Exit(1)
		}
		var req transcodeworker.SubmitRequest
		if err := json.Unmarshal(inputData, &req); err != nil {
			printJSON(map[string]any{"error": fmt.Sprintf("parsing submit JSON: %v", err)})
			os.Exit(1)
		}

		resp, err := worker.Submit(ctx, req, selfExe, configPath)
		printJSON(resp)
		if err != nil {
			os.Exit(1)
		}

	case "_internal_run":
		if len(subcmdArgs) < 1 {
			fmt.Fprintf(os.Stderr, "missing job id for _internal_run\n")
			os.Exit(1)
		}
		jobID := subcmdArgs[0]
		sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		if err := worker.InternalRun(sigCtx, jobID); err != nil {
			fmt.Fprintf(os.Stderr, "internal_run failed for job %s: %v\n", jobID, err)
			os.Exit(1)
		}

	case "status":
		if len(subcmdArgs) < 1 {
			printJSON(map[string]any{"error": "missing job id"})
			os.Exit(1)
		}
		jobID := subcmdArgs[0]
		st, err := worker.Status(ctx, jobID)
		printJSON(st)
		if err != nil {
			os.Exit(1)
		}

	case "cancel":
		if len(subcmdArgs) < 1 {
			printJSON(map[string]any{"error": "missing job id"})
			os.Exit(1)
		}
		jobID := subcmdArgs[0]
		res, err := worker.Cancel(ctx, jobID)
		printJSON(res)
		if err != nil {
			os.Exit(1)
		}

	case "benchmark_submit":
		inputData, err := io.ReadAll(os.Stdin)
		if err != nil {
			printJSON(transcode.BenchmarkSubmitResponse{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				Error:           fmt.Sprintf("reading stdin: %v", err),
			})
			os.Exit(1)
		}
		var req transcode.BenchmarkRequest
		if err := json.Unmarshal(inputData, &req); err != nil {
			printJSON(transcode.BenchmarkSubmitResponse{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				Error:           fmt.Sprintf("parsing benchmark submit JSON: %v", err),
			})
			os.Exit(1)
		}

		resp, err := worker.BenchmarkSubmit(ctx, req, selfExe, configPath)
		printJSON(resp)
		if err != nil {
			os.Exit(1)
		}

	case "benchmark_status":
		if len(subcmdArgs) < 1 {
			printJSON(transcode.BenchmarkStatus{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				Error:           "missing job id",
			})
			os.Exit(1)
		}
		jobID := subcmdArgs[0]
		st, err := worker.BenchmarkStatus(ctx, jobID)
		printJSON(st)
		if err != nil {
			os.Exit(1)
		}

	case "benchmark_cancel":
		if len(subcmdArgs) < 1 {
			printJSON(transcode.BenchmarkCancelResponse{
				ProtocolVersion: transcode.WorkerProtocolVersion,
				Error:           "missing job id",
			})
			os.Exit(1)
		}
		jobID := subcmdArgs[0]
		res, err := worker.BenchmarkCancel(ctx, jobID)
		printJSON(res)
		if err != nil {
			os.Exit(1)
		}

	case "serve":
		if err := runServe(cfg, configPath, selfExe, subcmdArgs); err != nil {
			fmt.Fprintf(os.Stderr, "serve failed: %v\n", err)
			os.Exit(1)
		}

	case "_internal_benchmark":
		if len(subcmdArgs) < 1 {
			fmt.Fprintf(os.Stderr, "missing job id for _internal_benchmark\n")
			os.Exit(1)
		}
		jobID := subcmdArgs[0]
		runToken := ""
		if len(subcmdArgs) >= 2 {
			runToken = subcmdArgs[1]
		}
		sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		if err := worker.InternalBenchmark(sigCtx, jobID, runToken); err != nil {
			fmt.Fprintf(os.Stderr, "internal_benchmark failed for job %s: %v\n", jobID, err)
			os.Exit(1)
		}

	default:
		printJSON(map[string]any{"error": fmt.Sprintf("unknown command %q", subcmd)})
		os.Exit(1)
	}
}

func printJSON(v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(data))
}

// runServe starts the persistent PR1 HTTP daemon. It reuses the existing
// Worker for Submit/Status/Cancel and never shells out to ffmpeg directly:
// execution still flows through Worker.InternalRun via the detached runner.
func runServe(cfg *transcodeworker.WorkerConfig, configPath, selfExe string, args []string) error {
	var listenFlag, tokenFlag, tokenFileFlag string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--listen":
			if i+1 < len(args) {
				listenFlag = args[i+1]
				i++
			}
		case "--token":
			if i+1 < len(args) {
				tokenFlag = args[i+1]
				i++
			}
		case "--token-file":
			if i+1 < len(args) {
				tokenFileFlag = args[i+1]
				i++
			}
		default:
			if stringsHasPrefix(args[i], "--listen=") {
				listenFlag = args[i][len("--listen="):]
			} else if stringsHasPrefix(args[i], "--token=") {
				tokenFlag = args[i][len("--token="):]
			} else if stringsHasPrefix(args[i], "--token-file=") {
				tokenFileFlag = args[i][len("--token-file="):]
			} else {
				return fmt.Errorf("unknown serve flag %q (expected --listen, --token, --token-file)", args[i])
			}
		}
	}

	serveCfg, err := transcodeworker.ResolveServeConfig(cfg, listenFlag, tokenFlag, tokenFileFlag, selfExe, configPath)
	if err != nil {
		return err
	}

	worker := transcodeworker.NewWorker(cfg)
	srv := transcodeworker.NewServer(worker, serveCfg.SelfExe, serveCfg.ConfigPath, serveCfg.Token)

	// Serve lifetime: SIGTERM/SIGINT drives both HTTP graceful shutdown and
	// the autonomous queue drain below.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	httpSrv := &http.Server{
		Addr:              serveCfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ln, err := net.Listen("tcp", serveCfg.Listen)
	if err != nil {
		return fmt.Errorf("serve cannot bind %s: %w", serveCfg.Listen, err)
	}

	// Autonomous durable-queue drain for the serve lifetime only (startup
	// sweep plus a bounded ticker sweep so queued jobs start when capacity
	// frees without another submit, including while Navigatorr is
	// disconnected). Safe startup ordering (fail closed): bind first so a bind
	// failure can never spawn queued work, then reconcile persisted state
	// synchronously BEFORE any scheduling, and only start the drain once
	// reconciliation succeeds. No queued job is scheduled/spawned unless
	// reconciliation completed; a reconciliation error closes the listener and
	// fails startup without ever serving as healthy.
	stopScheduler := func() {}
	if err := startServeAfterReconcileAndResume(sigCtx, worker, worker, serveCfg.SelfExe, serveCfg.ConfigPath, func() {
		stopScheduler = startServeQueueDrain(sigCtx, srv)
	}); err != nil {
		_ = ln.Close()
		return err
	}
	defer stopScheduler()

	fmt.Fprintf(os.Stderr, "navigatorr-transcode serve listening on %s\n", ln.Addr())

	go func() {
		<-sigCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve http server stopped: %w", err)
	}
	return nil
}

// startupReconciler is the fail-closed startup dependency for the serve
// lifecycle. *transcodeworker.Worker implements it; tests inject a stub so the
// ordering can be proven without real processes or network.
type startupReconciler interface {
	ReconcileStartup(ctx context.Context) error
}

// startServeAfterReconcile enforces the safe serve startup order: persisted
// state is reconciled synchronously and startDrain (the autonomous queue drain,
// which is what can schedule/spawn queued jobs) is only invoked when
// reconciliation succeeds. On failure it returns a contextual error and never
// calls startDrain, so startup fails closed. It is a small seam so the
// ordering is deterministically testable.
func startServeAfterReconcile(ctx context.Context, rec startupReconciler, startDrain func()) error {
	if rec == nil {
		return errors.New("serve startup: worker is not configured for reconciliation")
	}
	if err := rec.ReconcileStartup(ctx); err != nil {
		return fmt.Errorf("serve startup reconciliation failed (refusing to start scheduler): %w", err)
	}
	if startDrain != nil {
		startDrain()
	}
	return nil
}

// startupResumer is the fail-closed startup dependency for the Phase 6B2
// post-encode finalization resume. *transcodeworker.Worker implements it; tests
// inject a stub so the ordering and failure behavior are deterministic.
type startupResumer interface {
	ResumePostEncode(ctx context.Context, selfExe, configPath string) (int, error)
}

// startServeAfterReconcileAndResume enforces the full safe serve startup order
// after the listener is bound: ReconcileStartup must succeed, then the
// post-encode finalization resume must succeed synchronously, and only then is
// the queued scheduler started. Any failure returns a contextual error and
// never starts the queued scheduler, so the daemon fails startup closed instead
// of appearing healthy with stranded post-encode jobs. It is a small seam so
// the ordering is deterministically testable.
func startServeAfterReconcileAndResume(ctx context.Context, rec startupReconciler, res startupResumer, selfExe, configPath string, startDrain func()) error {
	if rec == nil {
		return errors.New("serve startup: worker is not configured for reconciliation")
	}
	if err := rec.ReconcileStartup(ctx); err != nil {
		return fmt.Errorf("serve startup reconciliation failed (refusing to start scheduler): %w", err)
	}
	if res == nil {
		return errors.New("serve startup: worker is not configured for post-encode resume")
	}
	if _, err := res.ResumePostEncode(ctx, selfExe, configPath); err != nil {
		return fmt.Errorf("serve startup post-encode resume failed (refusing to start scheduler): %w", err)
	}
	if startDrain != nil {
		startDrain()
	}
	return nil
}

// startServeQueueDrain starts the daemon-owned autonomous queue drain bound to
// ctx (the serve lifetime) with the default tick. Split out so the serve
// wiring itself is unit-testable without binding a port.
func startServeQueueDrain(ctx context.Context, srv *transcodeworker.Server) func() {
	if srv == nil {
		return func() {}
	}
	return srv.StartScheduler(ctx, 0)
}

func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
