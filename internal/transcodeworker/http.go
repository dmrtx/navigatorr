package transcodeworker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// maxHTTPBodyBytes caps versioned API request bodies. Submit payloads are small
// structured JSON (paths + resolved plan); 1 MiB is comfortably above any
// legitimate plan while bounding memory use.
const maxHTTPBodyBytes = 1 << 20

// validTranscodeJobIDRegex mirrors transcode.validJobIDRegex (SSH path):
// safe filesystem identifier without separators. Traversal, bench- namespace,
// and reserved names are enforced separately in ValidateTranscodeJobID.
var validTranscodeJobIDRegex = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// MaxTranscodeJobIDLength bounds job IDs accepted over HTTP.
const MaxTranscodeJobIDLength = 128

// ValidateTranscodeJobID fail-closes on empty, oversized, traversal, encoded
// traversal, illegal characters, and reserved filesystem identifiers. It also
// rejects the reserved benchmark namespace so transcode and benchmark IDs can
// never collide.
func ValidateTranscodeJobID(id string) error {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return errors.New("job id is required")
	}
	if len(trimmed) > MaxTranscodeJobIDLength {
		return fmt.Errorf("invalid job id %q: exceeds max length %d", trimmed, MaxTranscodeJobIDLength)
	}
	if trimmed == "." || trimmed == ".." {
		return fmt.Errorf("invalid job id %q: reserved identifier", trimmed)
	}
	if strings.ContainsAny(trimmed, "/\\:\x00") {
		return fmt.Errorf("invalid job id %q: contains illegal characters or path separators", trimmed)
	}
	if strings.Contains(trimmed, "..") {
		return fmt.Errorf("invalid job id %q: path traversal attempt detected", trimmed)
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return fmt.Errorf("invalid job id %q: encoded path traversal detected", trimmed)
	}
	if strings.HasPrefix(lower, "bench-") {
		return fmt.Errorf("invalid job id %q: reserved benchmark prefix 'bench-'", trimmed)
	}
	if !validTranscodeJobIDRegex.MatchString(trimmed) {
		return fmt.Errorf("invalid job id %q (allowed: alphanumeric, underscore, dash, dot)", trimmed)
	}
	reserved := map[string]bool{
		"con": true, "prn": true, "aux": true, "nul": true,
		"com1": true, "com2": true, "com3": true, "com4": true,
		"lpt1": true, "lpt2": true, "lpt3": true,
	}
	if reserved[lower] {
		return fmt.Errorf("invalid job id %q: reserved filesystem identifier", trimmed)
	}
	return nil
}

// ResolveHTTPToken returns the effective bearer token: explicit token wins,
// otherwise the token file is read and trimmed. Empty means "no auth", which
// callers must only permit on loopback binds.
func ResolveHTTPToken(token, tokenFile string) (string, error) {
	if strings.TrimSpace(token) != "" {
		return strings.TrimSpace(token), nil
	}
	if strings.TrimSpace(tokenFile) == "" {
		return "", nil
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", fmt.Errorf("reading http token file %s: %w", tokenFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// IsLoopbackBind reports whether addr binds only to loopback. Empty host,
// wildcard (0.0.0.0 / ::), or any non-loopback IP/hostname is NOT loopback
// (fail closed: callers must require auth).
func IsLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Bare host without port (e.g. "localhost"): evaluate directly.
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	// Strip brackets for IPv6 literals like "[::1]".
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ValidateServeAddr fail-closes when a non-loopback bind has no bearer token.
func ValidateServeAddr(addr, token string) error {
	if strings.TrimSpace(addr) == "" {
		return errors.New("http listen address is required")
	}
	if !IsLoopbackBind(addr) && strings.TrimSpace(token) == "" {
		return fmt.Errorf("refusing to bind %q without bearer auth (non-loopback requires http_token or http_token_file)", addr)
	}
	return nil
}

// Server exposes the existing Worker over a versioned JSON HTTP API.
// It reuses Worker.Submit/Status/Cancel semantics verbatim. PR3 implements
// the authoritative persistent queue: transcode submits persist as queued
// (201) even at capacity; same key + same digest reuses the existing job
// (200); same key + different digest is a definitive 409 idempotency
// conflict (never UncertainError client-side).
type Server struct {
	worker     *Worker
	token      string
	selfExe    string
	configPath string
}

// NewServer builds a versioned API server around an existing Worker.
// token may be empty only for loopback binds (enforced by ValidateServeAddr
// at startup; Handler itself simply skips auth when token is empty).
func NewServer(worker *Worker, selfExe, configPath, token string) *Server {
	return &Server{worker: worker, selfExe: selfExe, configPath: configPath, token: token}
}

// StartScheduler starts the daemon-owned autonomous queue drain bound to ctx
// (the serve lifetime): an initial sweep plus a bounded ticker sweep, so
// persisted queued jobs start when capacity frees without another submit or
// status request, including while Navigatorr is disconnected. It performs no
// running-job reconciliation (phase 5). The returned func stops the ticker.
func (s *Server) StartScheduler(ctx context.Context, interval time.Duration) func() {
	if s.worker == nil {
		return func() {}
	}
	return s.worker.RunScheduler(ctx, s.selfExe, s.configPath, interval)
}

// Handler builds the /v1 routes with auth enforcement.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/ready", s.handleReady)
	mux.HandleFunc("/v1/doctor", s.handleDoctor)
	mux.HandleFunc("/v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("/v1/jobs", s.handleJobs)
	mux.HandleFunc("/v1/jobs/", s.handleJobByID)
	mux.HandleFunc("/v1/benchmarks", s.handleBenchmarks)
	mux.HandleFunc("/v1/benchmarks/", s.handleBenchmarkByID)
	// Auth applies first so the documented invariant holds: when a token is
	// configured it protects ALL /v1/* including malformed/traversal paths
	// (unauthenticated traversal -> 401). rejectTraversal still runs before
	// the mux itself, so authenticated traversal fails closed with 400 and
	// http.ServeMux path cleaning can never 301/307-redirect
	// (e.g. /v1/jobs/../evil -> /v1/evil) before validation.
	return s.withAuth(rejectTraversal(mux))
}

// rejectTraversal fails closed on raw "." / ".." / empty segments and on
// encoded separators/dots anywhere under /v1/jobs/ or /v1/benchmarks/
// before ServeMux path cleaning can redirect. Legitimate IDs match
// ^[a-zA-Z0-9_.-]+$ (transcode) or ^bench-... (benchmark) and never
// contain "/" or "%", so rejecting "%2f/%5c/%2e" here is safe; IDs that
// merely contain dots (e.g. "a.b") pass through to job-ID validation.
func rejectTraversal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped := r.URL.EscapedPath()
		for _, prefix := range []string{"/v1/jobs/", "/v1/benchmarks/"} {
			if strings.HasPrefix(escaped, prefix) {
				rest := strings.TrimPrefix(escaped, prefix)
				lower := strings.ToLower(rest)
				if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "%2e") {
					writeHTTPError(w, http.StatusBadRequest, "invalid job id: encoded path traversal detected")
					return
				}
				if trimmed := strings.Trim(rest, "/"); trimmed != "" {
					for _, seg := range strings.Split(trimmed, "/") {
						if seg == "." || seg == ".." || seg == "" {
							writeHTTPError(w, http.StatusBadRequest, fmt.Sprintf("invalid job id %q", seg))
							return
						}
					}
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(h string) (string, bool) {
	parts := strings.Fields(h)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return parts[1], true
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeHTTPError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeHTTPJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// maxHTTPErrorLen caps user-visible error strings so an oversized invalid
// field (e.g. a megabyte-long job ID echoed via %q) cannot generate an
// unbounded response body. Semantics are otherwise unchanged.
const maxHTTPErrorLen = 2048

func boundHTTPErrorMessage(msg string) string {
	if len(msg) > maxHTTPErrorLen {
		return msg[:maxHTTPErrorLen-3] + "..."
	}
	return msg
}

func writeHTTPError(w http.ResponseWriter, code int, msg string) {
	writeHTTPJSON(w, code, map[string]string{"error": boundHTTPErrorMessage(msg)})
}

// handleHealth is a liveness probe. It never touches ffmpeg, disks, or jobs.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	ver, commit := GetBuildMetadata()
	writeHTTPJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "navigatorr-transcode",
		"version": ver,
		"commit":  commit,
	})
}

// handleReady is a readiness probe. PR1 keeps it minimal and deterministic:
// StateDir must be creatable AND writable, verified by a bounded
// create/write/close/remove probe (MkdirAll alone could pass on an existing
// read-only directory). It deliberately does NOT gate on
// darwin/arm64, ffmpeg presence, encoder probes, or allowed-root contents so
// health checking stays usable in CI and pre-provisioning.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	stateDir := ""
	maxParallel := 1
	if s.worker != nil && s.worker.cfg != nil {
		stateDir = s.worker.cfg.StateDir
		maxParallel = s.worker.cfg.MaxParallelJobs
	}
	if strings.TrimSpace(stateDir) == "" {
		writeHTTPJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "error": "state_dir is not configured"})
		return
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "error": fmt.Sprintf("state_dir not writable: %v", err)})
		return
	}
	// Bounded writability probe: create, write a few bytes, close, remove.
	// Any failure means the directory is not genuinely writable.
	probe, err := os.CreateTemp(stateDir, ".ready-*.tmp")
	if err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "error": fmt.Sprintf("state_dir not writable: %v", err)})
		return
	}
	probeName := probe.Name()
	_, writeErr := probe.Write([]byte("ready"))
	closeErr := probe.Close()
	removeErr := os.Remove(probeName)
	if writeErr != nil || closeErr != nil || removeErr != nil {
		_ = os.Remove(probeName)
		detail := writeErr
		if detail == nil {
			detail = closeErr
		}
		if detail == nil {
			detail = removeErr
		}
		writeHTTPJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "error": fmt.Sprintf("state_dir not writable: %v", detail)})
		return
	}
	writeHTTPJSON(w, http.StatusOK, map[string]any{
		"ready":             true,
		"state_dir":         stateDir,
		"max_parallel_jobs": maxParallel,
	})
}

// handleJobs serves POST /v1/jobs. Structured JSON only; no shell/raw ffmpeg args.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/jobs" {
		writeHTTPError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeHTTPError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPBodyBytes)
	var req SubmitRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	if err := ValidateTranscodeJobID(req.ID); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.worker == nil {
		writeHTTPError(w, http.StatusInternalServerError, "worker is not configured")
		return
	}
	resp, err := s.worker.Submit(r.Context(), req, s.selfExe, s.configPath)
	if err != nil {
		writeHTTPJSON(w, classifySubmitError(err), resp)
		return
	}
	code := http.StatusCreated
	if resp.Reused {
		// Idempotent re-submit of the same execution spec (any status).
		code = http.StatusOK
	}
	writeHTTPJSON(w, code, resp)
}

// handleJobByID serves GET /v1/jobs/{id} and POST /v1/jobs/{id}/cancel.
func (s *Server) handleJobByID(w http.ResponseWriter, r *http.Request) {
	escaped := r.URL.EscapedPath()
	rest := strings.TrimPrefix(escaped, "/v1/jobs/")
	// Fail closed on encoded separators/dots before any decoding.
	// (The outer rejectTraversal guard already blocks these pre-mux; this
	// is defense in depth if the handler is ever wired without it.)
	lowerRest := strings.ToLower(rest)
	if strings.Contains(lowerRest, "%2f") || strings.Contains(lowerRest, "%5c") || strings.Contains(lowerRest, "%2e") {
		writeHTTPError(w, http.StatusBadRequest, "invalid job id: encoded path traversal detected")
		return
	}
	trimmed := strings.Trim(rest, "/")
	parts := strings.Split(trimmed, "/")
	var id string
	var isCancel, isLogs bool
	switch {
	case len(parts) == 1:
		id = parts[0]
	case len(parts) == 2 && parts[1] == "cancel":
		id = parts[0]
		isCancel = true
	case len(parts) == 2 && parts[1] == "logs":
		id = parts[0]
		isLogs = true
	default:
		writeHTTPError(w, http.StatusNotFound, "not found")
		return
	}
	// Decode percent-encoding (e.g. %20) before validation; encoded
	// separators were already rejected above.
	if decoded, err := url.PathUnescape(id); err != nil {
		writeHTTPError(w, http.StatusBadRequest, fmt.Sprintf("invalid job id %q", id))
		return
	} else {
		id = decoded
	}
	// Reject empty, nested, or backslash IDs before validation.
	if strings.TrimSpace(id) == "" || strings.Contains(id, "/") || strings.Contains(id, "\\") {
		writeHTTPError(w, http.StatusBadRequest, fmt.Sprintf("invalid job id %q", id))
		return
	}
	if err := ValidateTranscodeJobID(id); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.worker == nil {
		writeHTTPError(w, http.StatusInternalServerError, "worker is not configured")
		return
	}
	switch {
	case r.Method == http.MethodGet && isLogs:
		s.handleJobLogs(w, r, id)
	case r.Method == http.MethodGet && !isCancel:
		st, err := s.worker.Status(r.Context(), id)
		if err != nil {
			if isNotFoundError(err) {
				writeHTTPJSON(w, http.StatusNotFound, st)
				return
			}
			writeHTTPJSON(w, http.StatusBadRequest, st)
			return
		}
		writeHTTPJSON(w, http.StatusOK, st)
	case r.Method == http.MethodPost && isCancel:
		res, err := s.worker.Cancel(r.Context(), id)
		if err != nil {
			if isNotFoundError(err) {
				writeHTTPJSON(w, http.StatusNotFound, res)
				return
			}
			writeHTTPJSON(w, http.StatusBadRequest, res)
			return
		}
		writeHTTPJSON(w, http.StatusOK, res)
	default:
		if isCancel {
			writeHTTPError(w, http.StatusMethodNotAllowed, "use POST")
			return
		}
		writeHTTPError(w, http.StatusMethodNotAllowed, "use GET")
	}
}

// handleJobLogs serves GET /v1/jobs/{id}/logs. It exposes the existing
// per-job runner log (<StateDir>/<id>/ffmpeg.log) as a bounded JSON tail:
// content plus truncated/size_bytes so a dropped prefix is unambiguous. The
// body is hard-capped by Worker.ReadJobLog regardless of the optional
// tail_bytes query parameter; malformed or non-positive tail_bytes is a 400.
func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request, jobID string) {
	if s.worker == nil {
		writeHTTPError(w, http.StatusInternalServerError, "worker is not configured")
		return
	}
	var tailBytes int64
	if raw := strings.TrimSpace(r.URL.Query().Get("tail_bytes")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			writeHTTPError(w, http.StatusBadRequest, "tail_bytes must be a positive integer")
			return
		}
		tailBytes = n
	}
	log, err := s.worker.ReadJobLog(jobID, tailBytes)
	if err != nil {
		if IsJobLogNotFound(err) {
			writeHTTPError(w, http.StatusNotFound, "job log not found")
			return
		}
		writeHTTPError(w, http.StatusInternalServerError, fmt.Sprintf("reading job log: %v", err))
		return
	}
	writeHTTPJSON(w, http.StatusOK, log)
}

// handleDoctor serves GET /v1/doctor. It reuses Worker.Doctor verbatim so
// HTTPExecutor.Doctor has the same fail-closed semantics as the SSH
// `doctor` subcommand (ffmpeg/VideoToolbox/allowed-roots checks). It is
// deliberately separate from /v1/health (liveness) and /v1/ready
// (state-dir writability): capabilities/doctor must never be faked from
// health.
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if s.worker == nil {
		writeHTTPError(w, http.StatusInternalServerError, "worker is not configured")
		return
	}
	res := s.worker.Doctor(r.Context())
	if !res.OK {
		writeHTTPJSON(w, http.StatusServiceUnavailable, res)
		return
	}
	writeHTTPJSON(w, http.StatusOK, res)
}

// handleCapabilities serves GET /v1/capabilities. It reuses
// Worker.Capabilities (ProbeWorkerCapabilities) verbatim, including the
// versioned protocol_version and capability_fingerprint fields that the
// Navigatorr side must verify (same checks as SSHExecutor.Capabilities).
// A probe failure is a 500 with an error envelope so callers fail closed
// instead of caching a fake/partial capability set.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if s.worker == nil {
		writeHTTPError(w, http.StatusInternalServerError, "worker is not configured")
		return
	}
	caps, err := s.worker.Capabilities(r.Context())
	if err != nil {
		writeHTTPError(w, http.StatusInternalServerError, fmt.Sprintf("probing worker capabilities: %v", err))
		return
	}
	writeHTTPJSON(w, http.StatusOK, caps)
}

// handleBenchmarks serves POST /v1/benchmarks. Structured JSON only; the
// body is transcode.BenchmarkRequest with DisallowUnknownFields and a 1 MiB
// cap, mirroring POST /v1/jobs. Delegates to Worker.BenchmarkSubmit so
// validation, digest idempotency, capacity accounting, and detached spawn
// semantics are reused verbatim.
func (s *Server) handleBenchmarks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/benchmarks" {
		writeHTTPError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeHTTPError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPBodyBytes)
	var req transcode.BenchmarkRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}
	if s.worker == nil {
		writeHTTPError(w, http.StatusInternalServerError, "worker is not configured")
		return
	}
	resp, err := s.worker.BenchmarkSubmit(r.Context(), req, s.selfExe, s.configPath)
	if err != nil {
		writeHTTPJSON(w, classifyBenchmarkSubmitError(err), resp)
		return
	}
	code := http.StatusCreated
	if resp.Status == "running" || resp.Status == "completed" || resp.Status == "failed" || resp.Status == "cancelled" {
		// Idempotent re-submit of an existing benchmark (all terminal and
		// live states are strictly idempotent by id+digest in the worker).
		code = http.StatusOK
	}
	writeHTTPJSON(w, code, resp)
}

// handleBenchmarkByID serves GET /v1/benchmarks/{id} and
// POST /v1/benchmarks/{id}/cancel.
func (s *Server) handleBenchmarkByID(w http.ResponseWriter, r *http.Request) {
	escaped := r.URL.EscapedPath()
	rest := strings.TrimPrefix(escaped, "/v1/benchmarks/")
	lowerRest := strings.ToLower(rest)
	if strings.Contains(lowerRest, "%2f") || strings.Contains(lowerRest, "%5c") || strings.Contains(lowerRest, "%2e") {
		writeHTTPError(w, http.StatusBadRequest, "invalid job id: encoded path traversal detected")
		return
	}
	trimmed := strings.Trim(rest, "/")
	parts := strings.Split(trimmed, "/")
	var id string
	var isCancel bool
	switch {
	case len(parts) == 1:
		id = parts[0]
	case len(parts) == 2 && parts[1] == "cancel":
		id = parts[0]
		isCancel = true
	default:
		writeHTTPError(w, http.StatusNotFound, "not found")
		return
	}
	if decoded, err := url.PathUnescape(id); err != nil {
		writeHTTPError(w, http.StatusBadRequest, fmt.Sprintf("invalid job id %q", id))
		return
	} else {
		id = decoded
	}
	if strings.TrimSpace(id) == "" || strings.Contains(id, "/") || strings.Contains(id, "\\") {
		writeHTTPError(w, http.StatusBadRequest, fmt.Sprintf("invalid job id %q", id))
		return
	}
	if err := validateBenchmarkJobIDHTTP(id); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.worker == nil {
		writeHTTPError(w, http.StatusInternalServerError, "worker is not configured")
		return
	}
	switch {
	case r.Method == http.MethodGet && !isCancel:
		st, err := s.worker.BenchmarkStatus(r.Context(), id)
		if err != nil {
			if isBenchmarkNotFoundError(err) {
				writeHTTPJSON(w, http.StatusNotFound, st)
				return
			}
			writeHTTPJSON(w, http.StatusBadRequest, st)
			return
		}
		writeHTTPJSON(w, http.StatusOK, st)
	case r.Method == http.MethodPost && isCancel:
		res, err := s.worker.BenchmarkCancel(r.Context(), id)
		if err != nil {
			if isBenchmarkNotFoundError(err) {
				writeHTTPJSON(w, http.StatusNotFound, res)
				return
			}
			writeHTTPJSON(w, http.StatusBadRequest, res)
			return
		}
		writeHTTPJSON(w, http.StatusOK, res)
	default:
		if isCancel {
			writeHTTPError(w, http.StatusMethodNotAllowed, "use POST")
			return
		}
		writeHTTPJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET"})
	}
}

// classifySubmitError maps Worker.Submit failures to HTTP codes while reusing
// Submit/Status/Cancel semantics. PR3: durable queue acceptance never returns
// "worker busy" for transcodes (busy mapping retained only for benchmark
// compat/legacy workers); strong idempotency conflicts are a definitive 409.
func classifySubmitError(err error) int {
	if IsIdempotencyConflict(err) {
		return http.StatusConflict
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "idempotency_conflict"):
		return http.StatusConflict
	case strings.Contains(msg, "worker busy") || strings.Contains(msg, "max parallel jobs"):
		return http.StatusConflict
	case strings.Contains(msg, "acquiring capacity lock"),
		strings.Contains(msg, "spawning worker process"),
		strings.Contains(msg, "persisting transcode process identity"),
		strings.Contains(msg, "saving initial job state"),
		strings.Contains(msg, "creating job directory"),
		strings.Contains(msg, "creating candidate directory"),
		strings.Contains(msg, "checking active jobs"):
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}

func isNotFoundError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "job not found") ||
		strings.Contains(msg, "reading job file") ||
		strings.Contains(msg, "no such file")
}

// validateBenchmarkJobIDHTTP reuses the canonical benchmark namespace
// validation so HTTP and SSH transports share one fail-closed definition.
func validateBenchmarkJobIDHTTP(id string) error {
	return transcode.ValidateBenchmarkJobID(id)
}

// classifyBenchmarkSubmitError mirrors classifySubmitError for benchmarks:
// busy stays a temporary 409 (retryable); spawn/persist/lock failures are
// 500; validation/plan/path/digest collisions are 400.
func classifyBenchmarkSubmitError(err error) int {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "worker busy") || strings.Contains(msg, "max parallel jobs"):
		return http.StatusConflict
	case strings.Contains(msg, "acquiring capacity lock"),
		strings.Contains(msg, "acquiring job lock"),
		strings.Contains(msg, "spawning benchmark process"),
		strings.Contains(msg, "persisting benchmark process identity"),
		strings.Contains(msg, "saving initial benchmark state"),
		strings.Contains(msg, "creating samples workspace"),
		strings.Contains(msg, "generating run token"),
		strings.Contains(msg, "checking active jobs"):
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}

func isBenchmarkNotFoundError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "benchmark job not found") ||
		strings.Contains(msg, "reading benchmark file") ||
		strings.Contains(msg, "no such file")
}

// ServeConfig carries the resolved daemon startup settings.
type ServeConfig struct {
	Listen     string
	Token      string
	SelfExe    string
	ConfigPath string
}

// ResolveServeConfig merges CLI overrides over WorkerConfig file values and
// enforces the safe-bind invariant (non-loopback requires bearer auth).
func ResolveServeConfig(cfg *WorkerConfig, listenFlag, tokenFlag, tokenFileFlag, selfExe, configPath string) (ServeConfig, error) {
	listen := strings.TrimSpace(listenFlag)
	if listen == "" && cfg != nil {
		listen = strings.TrimSpace(cfg.HTTPListen)
	}
	if listen == "" {
		listen = "127.0.0.1:8097"
	}
	token := strings.TrimSpace(tokenFlag)
	tokenFile := strings.TrimSpace(tokenFileFlag)
	if token == "" && cfg != nil && strings.TrimSpace(cfg.HTTPToken) != "" {
		token = strings.TrimSpace(cfg.HTTPToken)
	}
	if token == "" && tokenFile == "" && cfg != nil {
		tokenFile = strings.TrimSpace(cfg.HTTPTokenFile)
	}
	resolved, err := ResolveHTTPToken(token, tokenFile)
	if err != nil {
		return ServeConfig{}, err
	}
	if err := ValidateServeAddr(listen, resolved); err != nil {
		return ServeConfig{}, err
	}
	return ServeConfig{Listen: listen, Token: resolved, SelfExe: selfExe, ConfigPath: configPath}, nil
}
