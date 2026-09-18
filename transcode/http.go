package transcode

// HTTPExecutor implements Executor over the versioned worker daemon HTTP API
// (`navigatorr-transcode serve`, /v1 routes).
//
// The production pipeline uses HTTP. There is deliberately NO SSH fallback after
// HTTP uncertainty, NO automatic resubmit, and NO consumption of encode
// retry budget at the transport layer. A submit/cancel transport
// timeout/disconnect is UNKNOWN (possibly accepted by the worker), not an
// encode failure: callers must reconcile via idempotent Status before
// deciding anything. Navigatorr's background reconciler owns subsequent polls.
//
// Worker idempotency in PR3 is strong: idempotency_key +
// execution_spec_digest (canonical digest over source, candidate, normalized
// profile, and resolved plan/plan digest via DigestTranscodeExecutionSpec).
// A complete 409 idempotency conflict is definitive (never UncertainError).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// maxHTTPResponseBytes bounds every daemon response body the executor
	// will buffer. Submit/status payloads are small structured JSON; 4 MiB
	// comfortably covers capability reports (encoder details) while
	// bounding memory on malformed/oversized responses.
	maxHTTPResponseBytes = 4 << 20
	// maxHTTPErrorLen caps user-visible server error strings surfaced in
	// typed errors so a malicious/oversized envelope cannot flood logs.
	maxHTTPErrorLen = 2048
)

// DefaultHTTPRequestTimeout is the default deadline for idempotent reads
// (Doctor/Capabilities/Status/benchmark status) and for Cancel POSTs.
const DefaultHTTPRequestTimeout = 15 * time.Second

// DefaultHTTPSubmitTimeout is the default deadline for Submit POSTs
// (transcode + benchmark). Submit spawns a detached runner server-side and
// normally returns fast; the deadline only bounds transport, never the
// encode itself.
const DefaultHTTPSubmitTimeout = 60 * time.Second

// HTTPError is a typed non-2xx daemon response. It preserves method, URL
// (without secrets), status code, and a bounded server message so callers
// (and phase-4 reconciliation) can classify without string-sniffing raw
// bodies. In particular, 409 busy responses preserve the "worker busy"
// substring the action layer already classifies.
type HTTPError struct {
	Method     string
	URL        string
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "http error (<nil>)"
	}
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("transcode http %s %s: status %d: %s", e.Method, e.URL, e.StatusCode, msg)
}

// UncertainError reports that the daemon's decision is UNKNOWN and the
// caller must reconcile (phase 4) instead of retrying blindly. Two shapes:
//
//   - Mutating operations (submit/cancel/benchmark_submit/benchmark_cancel):
//     once the request may have reached the worker, ANY failure to obtain
//     and validate a definitive acknowledgement is UNKNOWN: client.Do
//     transport errors, response-body read/truncation failures, and
//     malformed/truncated or ambiguous 2xx acknowledgements. The operation
//     may already have executed. It MUST NOT be treated as an encode
//     failure, MUST NOT trigger an automatic resubmit/SSH fallback, and
//     MUST NOT burn encode retry budget. Reconcile via idempotent Status
//     (GET /v1/jobs/{id}) first.
//   - Read-only operations (doctor/capabilities/status/benchmark_status):
//     a transport failure means reachability is unknown; no worker state
//     change is implied, so a later retry of the read itself is safe.
//     These still surface as *UncertainError so callers can distinguish
//     "unknown" from definitive daemon rejections (typed *HTTPError).
type UncertainError struct {
	Op    string // e.g. "submit", "cancel", "status", "doctor", "capabilities", "benchmark_submit"
	JobID string // job/benchmark id when applicable, else ""
	Err   error  // underlying transport/parse/validation cause
}

func (e *UncertainError) Error() string {
	if e == nil {
		return "transcode transport uncertain (<nil>)"
	}
	where := e.Op
	if e.JobID != "" {
		where += " job " + e.JobID
	}
	switch e.Op {
	case "health", "ready", "doctor", "capabilities", "status", "benchmark_status":
		return fmt.Sprintf("transcode http %s: transport uncertain (read unavailable; no mutation requested; retry this read): %v", where, e.Err)
	}
	return fmt.Sprintf("transcode http %s: transport uncertain (may or may not have been accepted; reconcile via Status before retry; do not burn encode retry budget): %v", where, e.Err)
}

func (e *UncertainError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsTransportUncertain reports whether err (or any wrapped cause) is an
// *UncertainError. Phase 4 reconciliation should branch on this, not on
// message substrings.
func IsTransportUncertain(err error) bool {
	var ue *UncertainError
	return errors.As(err, &ue)
}

// HTTPConfig configures HTTPExecutor. It mirrors the SSH executor's knobs
// where they apply (path mappings, timeouts) plus daemon HTTP specifics.
type HTTPConfig struct {
	// BaseURL is the daemon origin, e.g. "http://127.0.0.1:8097".
	// The "/v1" prefix is appended by the executor; do not include it.
	BaseURL string
	// Token is the bearer credential. Empty means no Authorization header
	// (only appropriate for loopback daemons).
	Token string
	// TokenFile, when Token is empty, is read once at construction and
	// trimmed (same precedence as the worker's ResolveHTTPToken).
	TokenFile string
	// RequestTimeout bounds Doctor/Capabilities/Status/Cancel/benchmark
	// status/cancel. Zero selects DefaultHTTPRequestTimeout.
	RequestTimeout time.Duration
	// SubmitTimeout bounds Submit/benchmark-submit POSTs. Zero selects
	// DefaultHTTPSubmitTimeout.
	SubmitTimeout time.Duration
	// PathMappings translates local<->remote paths with longest-prefix
	// matching, identical semantics to SSHExecutor (fail closed).
	PathMappings []PathMapping
	// HTTPClient, when non-nil, overrides the default client construction
	// (tests). When set, RequestTimeout/SubmitTimeout still bound each
	// request via context, but redirect policy/bearer handling comes from
	// this executor's request setup, not the injected client's policy:
	// construction always disables redirects.
	HTTPClient *http.Client
}

// HTTPExecutorOption customizes an HTTPExecutor (tests).
type HTTPExecutorOption func(*HTTPExecutor)

// WithHTTPClient injects a custom *http.Client transport (tests). Redirect
// policy is still forced to disabled.
func WithHTTPClient(c *http.Client) HTTPExecutorOption {
	return func(e *HTTPExecutor) {
		if c == nil {
			return
		}
		cp := *c
		e.client = &cp
	}
}

// HTTPExecutor implements Executor over /v1 HTTP.
type HTTPExecutor struct {
	cfg    HTTPConfig
	base   string
	token  string
	client *http.Client
	reqTO  time.Duration
	subTO  time.Duration
}

// NewHTTPExecutor builds an HTTPExecutor. BaseURL must be an http/https URL;
// path mappings may be empty (translation then fails closed per call, same
// as SSHExecutor).
func NewHTTPExecutor(cfg HTTPConfig, opts ...HTTPExecutorOption) (*HTTPExecutor, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, errors.New("http worker base_url is required")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("http worker base_url %q must be an absolute http(s) URL (fail closed)", cfg.BaseURL)
	}
	base = strings.TrimRight(base, "/")

	token := strings.TrimSpace(cfg.Token)
	if token == "" && strings.TrimSpace(cfg.TokenFile) != "" {
		data, err := os.ReadFile(strings.TrimSpace(cfg.TokenFile))
		if err != nil {
			return nil, fmt.Errorf("reading http token file %s: %w", cfg.TokenFile, err)
		}
		token = strings.TrimSpace(string(data))
	}

	reqTO := cfg.RequestTimeout
	if reqTO <= 0 {
		reqTO = DefaultHTTPRequestTimeout
	}
	subTO := cfg.SubmitTimeout
	if subTO <= 0 {
		subTO = DefaultHTTPSubmitTimeout
	}

	e := &HTTPExecutor{cfg: cfg, base: base, token: token, reqTO: reqTO, subTO: subTO}
	if cfg.HTTPClient != nil {
		// Shallow-copy so forcing the no-redirect policy below never
		// mutates a caller-shared client.
		cp := *cfg.HTTPClient
		e.client = &cp
	} else {
		e.client = &http.Client{Timeout: subTO}
	}
	// Redirects must never silently normalize malformed/traversal paths.
	e.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	for _, opt := range opts {
		opt(e)
		if e.client != nil {
			e.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			}
		}
	}
	return e, nil
}

// BaseURL returns the normalized daemon origin (no trailing slash).
func (e *HTTPExecutor) BaseURL() string { return e.base }

// TranslateLocalToRemote mirrors SSHExecutor longest-prefix semantics.
func (e *HTTPExecutor) TranslateLocalToRemote(localPath string) (string, error) {
	if len(e.cfg.PathMappings) == 0 {
		return "", fmt.Errorf("no path mappings configured for transcode http executor (fail closed)")
	}
	cleanLocal := filepath.Clean(localPath)
	mappings := make([]PathMapping, len(e.cfg.PathMappings))
	copy(mappings, e.cfg.PathMappings)
	sort.Slice(mappings, func(i, j int) bool {
		return len(filepath.Clean(mappings[i].Local)) > len(filepath.Clean(mappings[j].Local))
	})
	for _, m := range mappings {
		mappedLocal := filepath.Clean(m.Local)
		if mappedLocal == "" || m.Remote == "" {
			continue
		}
		if cleanLocal == mappedLocal {
			return filepath.Clean(m.Remote), nil
		}
		prefixWithSep := mappedLocal + string(filepath.Separator)
		if strings.HasPrefix(cleanLocal, prefixWithSep) {
			rel := strings.TrimPrefix(cleanLocal, mappedLocal)
			rel = strings.TrimPrefix(rel, string(filepath.Separator))
			return filepath.ToSlash(filepath.Join(m.Remote, filepath.ToSlash(rel))), nil
		}
	}
	return "", fmt.Errorf("path %q does not match any configured transcode path mappings (fail closed)", localPath)
}

// TranslateRemoteToLocal mirrors SSHExecutor longest-prefix semantics.
func (e *HTTPExecutor) TranslateRemoteToLocal(remotePath string) (string, error) {
	if len(e.cfg.PathMappings) == 0 {
		return "", fmt.Errorf("no path mappings configured for transcode http executor (fail closed)")
	}
	cleanRemote := filepath.ToSlash(filepath.Clean(remotePath))
	mappings := make([]PathMapping, len(e.cfg.PathMappings))
	copy(mappings, e.cfg.PathMappings)
	sort.Slice(mappings, func(i, j int) bool {
		return len(filepath.ToSlash(filepath.Clean(mappings[i].Remote))) > len(filepath.ToSlash(filepath.Clean(mappings[j].Remote)))
	})
	for _, m := range mappings {
		mappedRemote := filepath.ToSlash(filepath.Clean(m.Remote))
		if mappedRemote == "" || m.Local == "" {
			continue
		}
		if cleanRemote == mappedRemote {
			return filepath.Clean(m.Local), nil
		}
		prefixWithSlash := mappedRemote + "/"
		if strings.HasPrefix(cleanRemote, prefixWithSlash) {
			rel := strings.TrimPrefix(cleanRemote, mappedRemote)
			rel = strings.TrimPrefix(rel, "/")
			return filepath.Clean(filepath.Join(m.Local, rel)), nil
		}
	}
	return "", fmt.Errorf("remote path %q does not match any configured transcode path mappings (fail closed)", remotePath)
}

func boundHTTPErrorMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > maxHTTPErrorLen {
		return msg[:maxHTTPErrorLen-3] + "..."
	}
	return msg
}

// validMutationAckStatus reports whether s is one of the lifecycle statuses
// the worker emits on job/benchmark records. A mutating acknowledgement
// carrying any other (or empty) status is ambiguous and must be treated as
// UNKNOWN, not as confirmed success.
func validMutationAckStatus(s string) bool {
	switch s {
	case StatusQueued, StatusRunning, StatusCompleted, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

func (e *HTTPExecutor) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	return req, nil
}

// do performs exactly ONE HTTP attempt. There are no automatic retries at
// the transport layer, in particular no retry for POST submit/cancel: any
// failure the caller cannot classify as definitive must surface as
// *UncertainError for phase-4 reconciliation.
//
// op/jobID identify the operation for uncertainty context; mutating must
// be true for Submit/Cancel/BenchmarkSubmit/BenchmarkCancel (operations
// the worker may already have executed once the request was sent). For
// mutating operations a response-body read/truncation failure is UNKNOWN
// (the server may have accepted the operation but the acknowledgement was
// lost), so it returns *UncertainError; for read-only operations it stays
// an ordinary error. A complete non-2xx response is definitive either way
// and is returned as a typed *HTTPError (never UncertainError).
func (e *HTTPExecutor) do(req *http.Request, op, jobID string, mutating bool) ([]byte, int, http.Header, error) {
	reqOp := req.Method + " " + req.URL.Path
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, 0, nil, errNoRedirect(reqOp, err)
	}
	defer resp.Body.Close()
	// Never follow redirects: surface 3xx fail-closed instead of silently
	// normalizing a malformed path.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, resp.StatusCode, resp.Header, &HTTPError{
			Method:     req.Method,
			URL:        redactURL(req.URL.String()),
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("redirect (%d) is never followed by the transcode http transport (fail closed); location=%q", resp.StatusCode, resp.Header.Get("Location")),
		}
	}
	lr := io.LimitReader(resp.Body, maxHTTPResponseBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		if mutating {
			return nil, resp.StatusCode, resp.Header, &UncertainError{Op: op, JobID: jobID, Err: fmt.Errorf("reading daemon response body for %s: %w", reqOp, err)}
		}
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("reading daemon response body for %s: %w", reqOp, err)
	}
	if int64(len(data)) > maxHTTPResponseBytes {
		if mutating && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, resp.StatusCode, resp.Header, &UncertainError{Op: op, JobID: jobID, Err: fmt.Errorf("daemon 2xx response body exceeds %d bytes (acknowledgement truncated; fail unknown)", maxHTTPResponseBytes)}
		}
		return nil, resp.StatusCode, resp.Header, &HTTPError{
			Method:     req.Method,
			URL:        redactURL(req.URL.String()),
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("daemon response body exceeds %d bytes (bounded; fail closed)", maxHTTPResponseBytes),
		}
	}
	return data, resp.StatusCode, resp.Header, nil
}

func errNoRedirect(op string, err error) error {
	// Preserve url.Error / net.Error chains for errors.As callers; the
	// UncertainError wrapper is applied by each operation with job context.
	_ = op
	return err
}

func redactURL(s string) string {
	// URLs never carry secrets here (bearer goes in headers), but keep the
	// helper so future query auth cannot leak via errors.
	return s
}

type httpErrorEnvelope struct {
	Error string `json:"error"`
}

func parseErrorMessage(data []byte) string {
	var env httpErrorEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return ""
	}
	return boundHTTPErrorMessage(env.Error)
}

func isTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return true
		}
		// Connection refused/reset, DNS, TLS handshake, closed body, etc.
		var opErr *net.OpError
		var dnsErr *net.DNSError
		if errors.As(err, &opErr) || errors.As(err, &dnsErr) {
			return true
		}
		msg := strings.ToLower(ue.Err.Error())
		for _, sub := range []string{"connection refused", "connection reset", "broken pipe", "no such host", "eof", "closed", "handshake", "timeout", "deadline exceeded", "canceled"} {
			if strings.Contains(msg, sub) {
				return true
			}
		}
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || os.IsTimeout(err) {
		return true
	}
	return false
}

func (e *HTTPExecutor) failUncertain(op, jobID string, err error) error {
	// do() already returns *UncertainError for mutating body-read failures;
	// never double-wrap (errors.As unwraps either way, but one layer keeps
	// messages readable).
	var ue *UncertainError
	if errors.As(err, &ue) {
		return err
	}
	return &UncertainError{Op: op, JobID: jobID, Err: err}
}

// mutatingDoErr maps a do() failure for a mutating POST (Submit, Cancel,
// BenchmarkSubmit, BenchmarkCancel) to the caller-visible contract, with no
// retry or fallback:
//   - *UncertainError (e.g. body-read/truncation failure once the request
//     may have reached the worker) passes through unchanged;
//   - *HTTPError (a complete 3xx/4xx/5xx: refused redirect or oversized
//     non-2xx) passes through unchanged as definitive, so
//     IsTransportUncertain stays false;
//   - anything else (a client.Do transport failure where no definitive HTTP
//     status response was obtained) becomes *UncertainError with op/job
//     context for phase-4 reconciliation.
func (e *HTTPExecutor) mutatingDoErr(op, jobID string, err error) error {
	var ue *UncertainError
	if errors.As(err, &ue) {
		return err
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return err
	}
	return &UncertainError{Op: op, JobID: jobID, Err: err}
}

// Health checks process liveness without inspecting storage or encoders.
func (e *HTTPExecutor) Health(ctx context.Context) error {
	return e.availability(ctx, "health", "ok")
}

// Ready checks whether the worker can accept a durable job. Deep diagnostics
// are intentionally independent and must not gate a healthy submit.
func (e *HTTPExecutor) Ready(ctx context.Context) error {
	return e.availability(ctx, "ready", "ready")
}

func (e *HTTPExecutor) availability(ctx context.Context, op, field string) error {
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	path := "/v1/" + op
	req, err := e.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("building %s request: %w", op, err)
	}
	data, code, _, err := e.do(req, op, "", false)
	if err != nil {
		if isTransportFailure(err) {
			return e.failUncertain(op, "", err)
		}
		return fmt.Errorf("transcode http %s: %w", op, err)
	}
	if code != http.StatusOK {
		return &HTTPError{Method: http.MethodGet, URL: redactURL(e.base + path), StatusCode: code, Message: parseErrorMessage(data)}
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("parsing %s response: %w", op, err)
	}
	var available bool
	if err := json.Unmarshal(result[field], &available); err != nil || !available {
		return fmt.Errorf("transcode http %s: response did not confirm %s=true", op, field)
	}
	return nil
}

// Doctor reuses the daemon's environmental checks verbatim. It GETs
// /v1/doctor (NOT /health or /ready) so capability-relevant diagnostics are
// never faked from liveness.
func (e *HTTPExecutor) Doctor(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	req, err := e.newRequest(ctx, http.MethodGet, "/v1/doctor", nil)
	if err != nil {
		return fmt.Errorf("building doctor request: %w", err)
	}
	data, code, _, err := e.do(req, "doctor", "", false)
	if err != nil {
		if isTransportFailure(err) {
			return e.failUncertain("doctor", "", err)
		}
		return fmt.Errorf("transcode http doctor: %w", err)
	}
	if code == http.StatusOK {
		var res struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &res); err != nil {
			return fmt.Errorf("parsing doctor response: %w (bounded body %d bytes)", err, len(data))
		}
		if !res.OK {
			return fmt.Errorf("remote doctor check failed: %s", boundHTTPErrorMessage(res.Error))
		}
		return nil
	}
	if code == http.StatusServiceUnavailable {
		var res struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &res); err == nil && res.Error != "" {
			return fmt.Errorf("remote doctor check failed: %s", boundHTTPErrorMessage(res.Error))
		}
	}
	msg := parseErrorMessage(data)
	if msg == "" {
		msg = http.StatusText(code)
	}
	return &HTTPError{Method: http.MethodGet, URL: redactURL(e.base + "/v1/doctor"), StatusCode: code, Message: msg}
}

// Capabilities fetches the versioned capability report and applies the same
// fail-closed checks as SSHExecutor: protocol version match + fingerprint
// verification. Capabilities are NEVER derived from /health.
func (e *HTTPExecutor) Capabilities(ctx context.Context) (WorkerCapabilities, error) {
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	req, err := e.newRequest(ctx, http.MethodGet, "/v1/capabilities", nil)
	if err != nil {
		return WorkerCapabilities{}, fmt.Errorf("building capabilities request: %w", err)
	}
	data, code, _, err := e.do(req, "capabilities", "", false)
	if err != nil {
		if isTransportFailure(err) {
			return WorkerCapabilities{}, e.failUncertain("capabilities", "", err)
		}
		return WorkerCapabilities{}, fmt.Errorf("transcode http capabilities: %w", err)
	}
	if code != http.StatusOK {
		msg := parseErrorMessage(data)
		if msg == "" {
			msg = http.StatusText(code)
		}
		return WorkerCapabilities{}, &HTTPError{Method: http.MethodGet, URL: redactURL(e.base + "/v1/capabilities"), StatusCode: code, Message: msg}
	}
	var caps WorkerCapabilities
	if err := json.Unmarshal(data, &caps); err != nil {
		return WorkerCapabilities{}, fmt.Errorf("parsing capabilities response: %w (bounded body %d bytes)", err, len(data))
	}
	if caps.ProtocolVersion != WorkerProtocolVersion {
		return WorkerCapabilities{}, fmt.Errorf("worker returned unsupported protocol version %d (expected %d) (fail closed)", caps.ProtocolVersion, WorkerProtocolVersion)
	}
	if err := VerifyCapabilityFingerprint(caps); err != nil {
		return WorkerCapabilities{}, err
	}
	return caps, nil
}

type httpSubmitPayload struct {
	ID                  string `json:"id"`
	SourcePath          string `json:"source_path"`
	CandidatePath       string `json:"candidate_path"`
	Profile             string `json:"profile"`
	Plan                *Plan  `json:"plan,omitempty"`
	IdempotencyKey      string `json:"idempotency_key,omitempty"`
	ExecutionSpecDigest string `json:"execution_spec_digest,omitempty"`
}

type httpSubmitResult struct {
	ID                  string `json:"id"`
	Status              string `json:"status"`
	CandidatePath       string `json:"candidate_path,omitempty"`
	Plan                *Plan  `json:"plan,omitempty"`
	IdempotencyKey      string `json:"idempotency_key,omitempty"`
	ExecutionSpecDigest string `json:"execution_spec_digest,omitempty"`
	Error               string `json:"error,omitempty"`
}

// Submit translates local paths to remote, POSTs exactly once, and maps the
// daemon response. Once the request may have reached the worker, ANY failure
// to obtain and validate a definitive 2xx acknowledgement (transport error,
// truncated body, malformed JSON, or an id/status mismatch) yields
// *UncertainError (UNKNOWN, not encode failure); no resubmit, no SSH
// fallback, no retry budget consumed here. A complete non-2xx rejection
// stays a definitive typed *HTTPError — including 409 idempotency conflicts
// (same key + different digest), which are definitive and never uncertain.
func (e *HTTPExecutor) Submit(ctx context.Context, req Request) (Job, error) {
	if strings.TrimSpace(req.ID) == "" {
		return Job{}, errors.New("request id is required")
	}
	if !validJobIDRegex.MatchString(req.ID) {
		return Job{}, fmt.Errorf("invalid request id %q (allowed: alphanumeric, underscore, dash, dot)", req.ID)
	}
	remoteSource, err := e.TranslateLocalToRemote(req.SourcePath)
	if err != nil {
		return Job{}, fmt.Errorf("translating source path: %w", err)
	}
	remoteCandidate, err := e.TranslateLocalToRemote(req.CandidatePath)
	if err != nil {
		return Job{}, fmt.Errorf("translating candidate path: %w", err)
	}
	profile := req.Profile
	if strings.TrimSpace(profile) == "" {
		profile = "hevc-vt"
	}
	// Canonical idempotency data (PR3): explicit key wins, otherwise the job
	// ID; digest over the immutable execution request when a resolved plan is
	// present (profile-only callers omit the digest and let the worker compute
	// it over the resolved plan).
	effKey := DefaultTranscodeIdempotencyKey(req.ID, req.IdempotencyKey)
	var specDigest string
	if req.ExecutionSpecDigest != "" {
		specDigest = strings.TrimSpace(req.ExecutionSpecDigest)
	} else if req.Plan != nil {
		if d, derr := DigestTranscodeExecutionSpec(remoteSource, remoteCandidate, profile, req.Plan); derr == nil {
			specDigest = d
		}
	}
	payload := httpSubmitPayload{ID: req.ID, SourcePath: remoteSource, CandidatePath: remoteCandidate, Profile: profile, Plan: req.Plan, IdempotencyKey: effKey, ExecutionSpecDigest: specDigest}
	body, err := json.Marshal(payload)
	if err != nil {
		return Job{}, fmt.Errorf("serializing submit payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, e.subTO)
	defer cancel()
	hreq, err := e.newRequest(ctx, http.MethodPost, "/v1/jobs", body)
	if err != nil {
		return Job{}, fmt.Errorf("building submit request: %w", err)
	}
	data, code, _, err := e.do(hreq, "submit", req.ID, true)
	if err != nil {
		// The request may already have reached the worker: a transport
		// failure with no definitive response is UNKNOWN. A definitive
		// *HTTPError from do() passes through unchanged (see
		// mutatingDoErr); never retry here.
		return Job{}, e.mutatingDoErr("submit", req.ID, err)
	}
	switch code {
	case http.StatusCreated, http.StatusOK:
		var res httpSubmitResult
		if err := json.Unmarshal(data, &res); err != nil {
			return Job{}, e.failUncertain("submit", req.ID, fmt.Errorf("parsing submit acknowledgement (bounded body %d bytes): %w", len(data), err))
		}
		// A 2xx carrying res.Error is a contradictory acknowledgement: the
		// worker accepted the mutation at the HTTP layer yet reports a
		// failure inside the ack. The job may already exist server-side, so
		// this is UNKNOWN, not a definitive rejection (definitive
		// rejections arrive as non-2xx and stay typed *HTTPError below).
		if res.Error != "" {
			return Job{}, e.failUncertain("submit", req.ID, fmt.Errorf("contradictory submit acknowledgement (2xx with error): %s (bounded body %d bytes)", boundHTTPErrorMessage(res.Error), len(data)))
		}
		if res.ID != req.ID || !validMutationAckStatus(res.Status) {
			return Job{}, e.failUncertain("submit", req.ID, fmt.Errorf("ambiguous submit acknowledgement: id=%q status=%q (bounded body %d bytes)", res.ID, res.Status, len(data)))
		}
		return Job{ID: res.ID}, nil
	default:
		// Try the structured submit envelope first so "worker busy" (409)
		// keeps the exact substring the action classifier expects.
		var res httpSubmitResult
		if err := json.Unmarshal(data, &res); err == nil && res.Error != "" {
			return Job{}, &HTTPError{Method: http.MethodPost, URL: redactURL(e.base + "/v1/jobs"), StatusCode: code, Message: boundHTTPErrorMessage(res.Error)}
		}
		msg := parseErrorMessage(data)
		if msg == "" {
			msg = http.StatusText(code)
		}
		return Job{}, &HTTPError{Method: http.MethodPost, URL: redactURL(e.base + "/v1/jobs"), StatusCode: code, Message: msg}
	}
}

// Status GETs /v1/jobs/{id} and translates the remote candidate path back
// to local (best-effort, same as SSHExecutor).
func (e *HTTPExecutor) Status(ctx context.Context, jobID string) (JobStatus, error) {
	if strings.TrimSpace(jobID) == "" {
		return JobStatus{}, errors.New("jobID is required")
	}
	if !validJobIDRegex.MatchString(jobID) {
		return JobStatus{}, fmt.Errorf("invalid jobID %q", jobID)
	}
	path := "/v1/jobs/" + url.PathEscape(jobID)
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	hreq, err := e.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return JobStatus{}, fmt.Errorf("building status request: %w", err)
	}
	data, code, _, err := e.do(hreq, "status", jobID, false)
	if err != nil {
		if isTransportFailure(err) {
			return JobStatus{}, e.failUncertain("status", jobID, err)
		}
		return JobStatus{}, fmt.Errorf("transcode http status: %w", err)
	}
	if code != http.StatusOK {
		msg := parseErrorMessage(data)
		if msg == "" {
			// Status error bodies are JobStatusResponse-shaped; surface
			// their error field when present.
			var st JobStatus
			if jerr := json.Unmarshal(data, &st); jerr == nil && st.Error != "" {
				msg = boundHTTPErrorMessage(st.Error)
			} else {
				msg = http.StatusText(code)
			}
		}
		return JobStatus{}, &HTTPError{Method: http.MethodGet, URL: redactURL(e.base + path), StatusCode: code, Message: msg}
	}
	var st JobStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return JobStatus{}, fmt.Errorf("parsing status response: %w (bounded body %d bytes)", err, len(data))
	}
	if st.CandidatePath != "" {
		if localCandidate, terr := e.TranslateRemoteToLocal(st.CandidatePath); terr == nil {
			st.CandidatePath = localCandidate
		}
	}
	if st.NavigatorrPath == "" && st.WorkerResolvedPath != "" {
		st.NavigatorrPath, _ = e.TranslateRemoteToLocal(st.WorkerResolvedPath)
	}
	return st, nil
}

// Cancel POSTs /v1/jobs/{id}/cancel exactly once. Once the request may have
// reached the worker, ANY failure to obtain and validate a definitive 2xx
// cancelled acknowledgement (transport error, truncated body, malformed
// JSON, or an id/status mismatch) yields *UncertainError (cancel may or may
// not have applied; reconcile via Status). No retries at this layer. A
// complete non-2xx response stays a definitive typed *HTTPError.
func (e *HTTPExecutor) Cancel(ctx context.Context, jobID string) error {
	if strings.TrimSpace(jobID) == "" {
		return errors.New("jobID is required")
	}
	if !validJobIDRegex.MatchString(jobID) {
		return fmt.Errorf("invalid jobID %q", jobID)
	}
	path := "/v1/jobs/" + url.PathEscape(jobID) + "/cancel"
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	hreq, err := e.newRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return fmt.Errorf("building cancel request: %w", err)
	}
	data, code, _, err := e.do(hreq, "cancel", jobID, true)
	if err != nil {
		// The cancel may already have applied server-side: a transport
		// failure with no definitive response is UNKNOWN (reconcile via
		// Status). A definitive *HTTPError passes through unchanged; never
		// retry here.
		return e.mutatingDoErr("cancel", jobID, err)
	}
	if code != http.StatusOK {
		msg := parseErrorMessage(data)
		if msg == "" {
			msg = http.StatusText(code)
		}
		return &HTTPError{Method: http.MethodPost, URL: redactURL(e.base + path), StatusCode: code, Message: msg}
	}
	var res httpSubmitResult
	if err := json.Unmarshal(data, &res); err != nil {
		return e.failUncertain("cancel", jobID, fmt.Errorf("parsing cancel acknowledgement (bounded body %d bytes): %w", len(data), err))
	}
	// A 2xx carrying res.Error is contradictory: the cancel POST was
	// accepted at the HTTP layer yet the ack reports failure. The cancel
	// may already have applied, so this is UNKNOWN (reconcile via Status),
	// not a definitive outcome.
	if res.Error != "" {
		return e.failUncertain("cancel", jobID, fmt.Errorf("contradictory cancel acknowledgement (2xx with error): %s (bounded body %d bytes)", boundHTTPErrorMessage(res.Error), len(data)))
	}
	if res.ID != jobID || res.Status != StatusCancelled {
		return e.failUncertain("cancel", jobID, fmt.Errorf("ambiguous cancel acknowledgement: id=%q status=%q (bounded body %d bytes)", res.ID, res.Status, len(data)))
	}
	return nil
}

// BenchmarkSubmit translates the source path, POSTs exactly once to
// /v1/benchmarks, and verifies the versioned response. Once the request may
// have reached the worker, ANY failure to obtain and validate a definitive
// 2xx acknowledgement yields *UncertainError; no resubmit at this layer. A
// complete non-2xx rejection stays a definitive typed *HTTPError.
func (e *HTTPExecutor) BenchmarkSubmit(ctx context.Context, req BenchmarkRequest) (BenchmarkJob, error) {
	cp := req
	if cp.ProtocolVersion == 0 {
		cp.ProtocolVersion = WorkerProtocolVersion
	}
	if err := ValidateBenchmarkRequest(&cp); err != nil {
		return BenchmarkJob{}, fmt.Errorf("validating benchmark request: %w", err)
	}
	remoteSource, err := e.TranslateLocalToRemote(cp.SourcePath)
	if err != nil {
		return BenchmarkJob{}, fmt.Errorf("translating source path: %w", err)
	}
	cp.SourcePath = remoteSource
	body, err := json.Marshal(cp)
	if err != nil {
		return BenchmarkJob{}, fmt.Errorf("serializing benchmark submit payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, e.subTO)
	defer cancel()
	hreq, err := e.newRequest(ctx, http.MethodPost, "/v1/benchmarks", body)
	if err != nil {
		return BenchmarkJob{}, fmt.Errorf("building benchmark submit request: %w", err)
	}
	data, code, _, err := e.do(hreq, "benchmark_submit", req.ID, true)
	if err != nil {
		// The benchmark may already have been accepted: a transport failure
		// with no definitive response is UNKNOWN. A definitive *HTTPError
		// passes through unchanged; never retry here.
		return BenchmarkJob{}, e.mutatingDoErr("benchmark_submit", req.ID, err)
	}
	switch code {
	case http.StatusCreated, http.StatusOK:
		var res BenchmarkSubmitResponse
		if err := json.Unmarshal(data, &res); err != nil {
			return BenchmarkJob{}, e.failUncertain("benchmark_submit", req.ID, fmt.Errorf("parsing benchmark submit acknowledgement (bounded body %d bytes): %w", len(data), err))
		}
		// Any 2xx that does not validate as a definitive success ack is
		// UNKNOWN: the benchmark may already exist server-side. That
		// includes protocol mismatches and error-bearing 2xx bodies, which
		// are contradictory acknowledgements rather than definitive
		// rejections (definitive rejections arrive as non-2xx below).
		if res.ProtocolVersion != WorkerProtocolVersion {
			return BenchmarkJob{}, e.failUncertain("benchmark_submit", req.ID, fmt.Errorf("benchmark submit acknowledgement with unsupported protocol version %d (expected %d) (fail unknown)", res.ProtocolVersion, WorkerProtocolVersion))
		}
		if res.Error != "" {
			return BenchmarkJob{}, e.failUncertain("benchmark_submit", req.ID, fmt.Errorf("contradictory benchmark submit acknowledgement (2xx with error): %s (bounded body %d bytes)", boundHTTPErrorMessage(res.Error), len(data)))
		}
		if res.ID != req.ID || !validMutationAckStatus(res.Status) {
			return BenchmarkJob{}, e.failUncertain("benchmark_submit", req.ID, fmt.Errorf("ambiguous benchmark submit acknowledgement: id=%q status=%q (bounded body %d bytes)", res.ID, res.Status, len(data)))
		}
		return BenchmarkJob{ID: res.ID}, nil
	default:
		var res BenchmarkSubmitResponse
		if err := json.Unmarshal(data, &res); err == nil && res.Error != "" {
			if res.ProtocolVersion != 0 && res.ProtocolVersion != WorkerProtocolVersion {
				return BenchmarkJob{}, fmt.Errorf("worker returned unsupported protocol version %d (expected %d) (fail closed)", res.ProtocolVersion, WorkerProtocolVersion)
			}
			return BenchmarkJob{}, &HTTPError{Method: http.MethodPost, URL: redactURL(e.base + "/v1/benchmarks"), StatusCode: code, Message: boundHTTPErrorMessage(res.Error)}
		}
		msg := parseErrorMessage(data)
		if msg == "" {
			msg = http.StatusText(code)
		}
		return BenchmarkJob{}, &HTTPError{Method: http.MethodPost, URL: redactURL(e.base + "/v1/benchmarks"), StatusCode: code, Message: msg}
	}
}

// BenchmarkStatus GETs /v1/benchmarks/{id}, verifies the protocol version,
// and translates the remote source path back to local (best-effort).
func (e *HTTPExecutor) BenchmarkStatus(ctx context.Context, jobID string) (BenchmarkStatus, error) {
	trimmedID := strings.TrimSpace(jobID)
	if trimmedID == "" {
		return BenchmarkStatus{}, errors.New("jobID is required")
	}
	if !validBenchmarkJobIDRegex.MatchString(trimmedID) {
		return BenchmarkStatus{}, fmt.Errorf("invalid benchmark jobID %q", jobID)
	}
	path := "/v1/benchmarks/" + url.PathEscape(trimmedID)
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	hreq, err := e.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return BenchmarkStatus{}, fmt.Errorf("building benchmark status request: %w", err)
	}
	data, code, _, err := e.do(hreq, "benchmark_status", trimmedID, false)
	if err != nil {
		if isTransportFailure(err) {
			return BenchmarkStatus{}, e.failUncertain("benchmark_status", trimmedID, err)
		}
		return BenchmarkStatus{}, fmt.Errorf("transcode http benchmark status: %w", err)
	}
	if code != http.StatusOK {
		msg := parseErrorMessage(data)
		if msg == "" {
			var st BenchmarkStatus
			if jerr := json.Unmarshal(data, &st); jerr == nil && st.Error != "" {
				msg = boundHTTPErrorMessage(st.Error)
			} else {
				msg = http.StatusText(code)
			}
		}
		return BenchmarkStatus{}, &HTTPError{Method: http.MethodGet, URL: redactURL(e.base + path), StatusCode: code, Message: msg}
	}
	var st BenchmarkStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return BenchmarkStatus{}, fmt.Errorf("parsing benchmark status response: %w (bounded body %d bytes)", err, len(data))
	}
	if st.ProtocolVersion != WorkerProtocolVersion {
		return BenchmarkStatus{}, fmt.Errorf("worker returned unsupported protocol version %d (expected %d) (fail closed)", st.ProtocolVersion, WorkerProtocolVersion)
	}
	if st.SourcePath != "" {
		if localSource, terr := e.TranslateRemoteToLocal(st.SourcePath); terr == nil {
			st.SourcePath = localSource
		}
	}
	return st, nil
}

// BenchmarkCancel POSTs /v1/benchmarks/{id}/cancel exactly once and verifies
// the versioned response. ANY failure to obtain and validate a definitive
// 2xx cancelled acknowledgement yields *UncertainError (reconcile via
// Status); a complete non-2xx response stays a definitive typed *HTTPError.
func (e *HTTPExecutor) BenchmarkCancel(ctx context.Context, jobID string) error {
	trimmedID := strings.TrimSpace(jobID)
	if trimmedID == "" {
		return errors.New("jobID is required")
	}
	if !validBenchmarkJobIDRegex.MatchString(trimmedID) {
		return fmt.Errorf("invalid benchmark jobID %q", jobID)
	}
	path := "/v1/benchmarks/" + url.PathEscape(trimmedID) + "/cancel"
	ctx, cancel := context.WithTimeout(ctx, e.reqTO)
	defer cancel()
	hreq, err := e.newRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return fmt.Errorf("building benchmark cancel request: %w", err)
	}
	data, code, _, err := e.do(hreq, "benchmark_cancel", trimmedID, true)
	if err != nil {
		// The cancel may already have applied server-side: a transport
		// failure with no definitive response is UNKNOWN (reconcile via
		// Status). A definitive *HTTPError passes through unchanged; never
		// retry here.
		return e.mutatingDoErr("benchmark_cancel", trimmedID, err)
	}
	if code != http.StatusOK {
		msg := parseErrorMessage(data)
		if msg == "" {
			msg = http.StatusText(code)
		}
		return &HTTPError{Method: http.MethodPost, URL: redactURL(e.base + path), StatusCode: code, Message: msg}
	}
	var res BenchmarkCancelResponse
	if err := json.Unmarshal(data, &res); err != nil {
		return e.failUncertain("benchmark_cancel", trimmedID, fmt.Errorf("parsing benchmark cancel acknowledgement (bounded body %d bytes): %w", len(data), err))
	}
	// Any 2xx that does not validate as a definitive cancelled ack is
	// UNKNOWN: the cancel may already have applied (reconcile via Status).
	if res.ProtocolVersion != WorkerProtocolVersion {
		return e.failUncertain("benchmark_cancel", trimmedID, fmt.Errorf("benchmark cancel acknowledgement with unsupported protocol version %d (expected %d) (fail unknown)", res.ProtocolVersion, WorkerProtocolVersion))
	}
	if res.Error != "" {
		return e.failUncertain("benchmark_cancel", trimmedID, fmt.Errorf("contradictory benchmark cancel acknowledgement (2xx with error): %s (bounded body %d bytes)", boundHTTPErrorMessage(res.Error), len(data)))
	}
	if res.ID != trimmedID || res.Status != StatusCancelled {
		return e.failUncertain("benchmark_cancel", trimmedID, fmt.Errorf("ambiguous benchmark cancel acknowledgement: id=%q status=%q (bounded body %d bytes)", res.ID, res.Status, len(data)))
	}
	return nil
}

var _ Executor = (*HTTPExecutor)(nil)
