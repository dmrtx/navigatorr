package acceptance

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

const (
	envGate         = "NAVIGATORR_ACCEPTANCE"
	envFFmpeg       = "NAVIGATORR_ACCEPTANCE_FFMPEG"
	envFFprobe      = "NAVIGATORR_ACCEPTANCE_FFPROBE"
	envAllowMissing = "NAVIGATORR_ACCEPTANCE_ALLOW_MISSING"
	envReport       = "NAVIGATORR_ACCEPTANCE_REPORT"
	envKeep         = "NAVIGATORR_ACCEPTANCE_KEEP"
)

// lockedBuffer collects the worker's output while the tests read it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// suite is everything the cases share: one worker process, one engine and one
// media root. The worker and the engine keep process-global caches, so the
// cases run one at a time against the same instances, as production does.
type suite struct {
	root      string
	ffmpeg    string
	ffprobe   string
	engine    *action.Engine
	executor  transcode.Executor
	caps      transcode.WorkerCapabilities
	store     *store.Store
	workerLog *lockedBuffer
	stop      []func()
	mu        sync.Mutex
	sources   map[string]string
}

// close stops the reconciler and the worker, newest first.
func (s *suite) close() {
	for i := len(s.stop) - 1; i >= 0; i-- {
		s.stop[i]()
	}
}

var (
	suiteOnce sync.Once
	suiteVal  *suite
	suiteErr  error
	// suiteDir is the one temp tree of the run; it outlives any single subtest.
	suiteDir string
)

// teardown stops the worker, its runners and the temp tree once, whichever
// way the run ends.
var teardownOnce sync.Once

func teardown() {
	teardownOnce.Do(func() {
		if suiteVal != nil {
			suiteVal.close()
		}
		if suiteDir != "" && os.Getenv(envKeep) != "1" {
			_ = os.RemoveAll(suiteDir)
		} else if suiteDir != "" {
			fmt.Fprintf(os.Stderr, "acceptance: kept %s\n", suiteDir)
		}
	})
}

func TestMain(m *testing.M) {
	flag.Parse()
	// A go test timeout panics and an operator's Ctrl-C kills the process;
	// neither returns from m.Run, and the job runners outlive their parent.
	if timeout := flag.Lookup("test.timeout"); timeout != nil {
		if d, ok := timeout.Value.(flag.Getter).Get().(time.Duration); ok && d > time.Minute {
			time.AfterFunc(d-30*time.Second, teardown)
		}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		teardown()
		os.Exit(1)
	}()
	code := m.Run()
	teardown()
	os.Exit(code)
}

// gate skips unless the operator asked for the run.
func gate(t *testing.T) {
	t.Helper()
	if os.Getenv(envGate) != "1" {
		t.Skipf("set %s=1 to run the transcode acceptance suite", envGate)
	}
}

// missing reports a capability a case needs but the host lacks. On a worker
// host that is a defect, so it fails; developer machines opt into a skip.
func missing(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv(envAllowMissing) == "1" {
		t.Skipf("missing capability (allowed): %s", msg)
	}
	t.Fatalf("missing capability: %s (set %s=1 to skip on a development machine)", msg, envAllowMissing)
}

func lookTool(envName, name string) string {
	if p := strings.TrimSpace(os.Getenv(envName)); p != "" {
		return p
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// getSuite starts the shared worker and engine once. The first caller's
// testing.T is only used to fail; nothing later depends on its lifetime.
func getSuite(t *testing.T) *suite {
	t.Helper()
	suiteOnce.Do(func() { suiteVal, suiteErr = startSuite() })
	if suiteErr != nil {
		t.Fatalf("starting the acceptance suite: %v", suiteErr)
	}
	return suiteVal
}

func startSuite() (_ *suite, err error) {
	ffmpeg, ffprobe := lookTool(envFFmpeg, "ffmpeg"), lookTool(envFFprobe, "ffprobe")
	if ffmpeg == "" || ffprobe == "" {
		return nil, fmt.Errorf("ffmpeg and ffprobe are required (set %s and %s)", envFFmpeg, envFFprobe)
	}
	dir, err := os.MkdirTemp("", "navigatorr-acceptance-")
	if err != nil {
		return nil, err
	}
	suiteDir = dir
	// Promotion requires symlink-free paths; the macOS temp dir is a symlink.
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, err
	}
	s := &suite{root: filepath.Join(dir, "media"), ffmpeg: ffmpeg, ffprobe: ffprobe, workerLog: &lockedBuffer{}, sources: map[string]string{}}
	// suiteVal is only set on success, so a failed start stops what it began.
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	for _, d := range []string{s.root, filepath.Join(dir, "worker"), filepath.Join(dir, "engine")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}

	bin := filepath.Join(dir, "navigatorr-transcode")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/jakenesler/navigatorr/cmd/navigatorr-transcode").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("building the worker: %v\n%s", err, out)
	}
	// Job and benchmark runners start in their own sessions, so stopping
	// serve leaves them and their ffmpeg children running. Every one of them
	// names a path under this run's own temp dir.
	s.stop = append(s.stop, func() { _ = exec.Command("pkill", "-KILL", "-f", dir+string(filepath.Separator)).Run() })
	base, err := startWorker(s, bin, dir)
	if err != nil {
		return nil, err
	}

	exe, err := httpclient.NewHTTPExecutor(httpclient.HTTPConfig{
		BaseURL:      base,
		PathMappings: []transcode.PathMapping{{Local: s.root, Remote: s.root}},
	})
	if err != nil {
		return nil, err
	}
	s.executor = exe
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if s.caps, err = exe.Capabilities(ctx); err != nil {
		return nil, fmt.Errorf("reading worker capabilities: %w", err)
	}

	st, err := store.Open(filepath.Join(dir, "engine", "actions.db"))
	if err != nil {
		return nil, err
	}
	s.store = st
	resolver, err := fsop.NewResolver([]string{s.root}, []string{s.root})
	if err != nil {
		return nil, err
	}
	cfg := &config.Config{
		AllowDestructive: true,
		Media:            config.MediaConfig{AllowedReadRoots: []string{s.root}, AllowedWriteRoots: []string{s.root}, FfprobePath: ffprobe},
		Transcode:        config.TranscodeConfig{Enabled: true, Executor: "http"},
	}
	s.engine = action.NewEngine(action.EngineDeps{
		Store: st, Config: cfg, Fs: resolver, Ffprobe: ffprobe, Transcode: exe,
		StartTime: time.Now(), ReconcileInterval: 100 * time.Millisecond,
	})
	rctx, stopReconciler := context.WithCancel(context.Background())
	done := s.engine.StartReconciler(rctx)
	s.stop = append(s.stop, func() { stopReconciler(); <-done; _ = st.Close() })
	return s, nil
}

// startWorker runs `navigatorr-transcode serve` the production way: a config
// file, a loopback listener, and job runners spawned as child processes of the
// same binary. It returns the base URL once /v1/ready answers.
func startWorker(s *suite, bin, dir string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfgPath := filepath.Join(dir, "worker.yaml")
	cfg := fmt.Sprintf(`ffmpeg: %q
ffprobe: %q
state_dir: %q
allowed_roots:
  - %q
max_parallel_jobs: 1
http_listen: %q
`, s.ffmpeg, s.ffprobe, filepath.Join(dir, "worker"), s.root, addr)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return "", err
	}
	cmd := exec.Command(bin, "--config", cfgPath, "serve")
	cmd.Stdout, cmd.Stderr = s.workerLog, s.workerLog
	if err := cmd.Start(); err != nil {
		return "", err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	s.stop = append(s.stop, func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	base := "http://" + addr
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return "", fmt.Errorf("worker exited during startup:\n%s", s.workerLog.String())
		default:
		}
		resp, err := client.Get(base + "/v1/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", fmt.Errorf("worker never became ready:\n%s", s.workerLog.String())
}
