package nashelper

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	simdsha256 "github.com/minio/sha256-simd"

	"github.com/jakenesler/navigatorr/internal/sampledigest"
)

const (
	hashChunk    = 1 << 20
	jobRetention = 15 * time.Minute
	// defaultOrphanAfter is how long a job may go unasked-about before the helper
	// stops reading for it. The client polls every couple of seconds, so a job
	// nobody asks about belongs to a caller that is gone (or never got the id).
	defaultOrphanAfter = 2 * time.Minute
	maxJobs            = 256
	maxActiveJobs      = 32
	maxRequestBytes    = 64 << 10
	minTokenLength     = 32
)

// Config configures a Server.
type Config struct {
	// Roots are the directories served. Every path in a request must be inside
	// one of them.
	Roots []string
	// Token is the bearer secret every request must present.
	Token string
	// MaxHashes bounds the hashes reading at once; the rest queue. Default 2.
	MaxHashes int
	// Logf receives one line per request, never a path. Default discards.
	Logf func(format string, args ...any)
}

// Server is the helper's HTTP service.
type Server struct {
	roots     *Roots
	tokenHash [sha256.Size]byte
	instance  string
	sem       chan struct{}
	usageSem  chan struct{}
	logf      func(string, ...any)

	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu     sync.Mutex
	jobs   map[string]*job
	closed bool
	now    func() time.Time

	// afterChunk runs after each chunk of a hash, so a test can change the file
	// mid-read or hold a job.
	afterChunk func(path string)

	orphanAfter time.Duration
}

// NewServer opens the roots and returns a Server.
func NewServer(cfg Config) (*Server, error) {
	if len(cfg.Token) < minTokenLength {
		return nil, fmt.Errorf("the token must be at least %d characters", minTokenLength)
	}
	roots, err := OpenRoots(cfg.Roots)
	if err != nil {
		return nil, err
	}
	if cfg.MaxHashes <= 0 {
		cfg.MaxHashes = 2
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		roots.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		roots:     roots,
		tokenHash: sha256.Sum256([]byte(cfg.Token)),
		instance:  hex.EncodeToString(idBytes),
		sem:       make(chan struct{}, cfg.MaxHashes),
		usageSem:  make(chan struct{}, 1),
		logf:      cfg.Logf,
		baseCtx:   ctx,
		cancel:    cancel,
		jobs:      map[string]*job{},
		now:       time.Now,

		orphanAfter: defaultOrphanAfter,
	}, nil
}

// InstanceID is random per start. A caller that stored it can tell that the
// helper was restarted.
func (s *Server) InstanceID() string { return s.instance }

// Close stops every job and releases the roots.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
	s.roots.Close()
}

// Handler is the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("POST /v1/identity", s.identity)
	mux.HandleFunc("POST /v1/sample", s.sample)
	mux.HandleFunc("POST /v1/hash", s.startHash)
	mux.HandleFunc("GET /v1/volumes", s.volumes)
	mux.HandleFunc("POST /v1/usage", s.startUsage)
	mux.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	mux.HandleFunc("DELETE /v1/jobs/{id}", s.cancelJob)
	return s.authenticated(mux)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := s.now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(sum[:], s.tokenHash[:]) != 1 {
			writeError(rec, &Error{Code: CodeAuth, Message: "missing or wrong bearer token"})
		} else {
			next.ServeHTTP(rec, r)
		}
		s.logf("%s %s %d %s", r.Method, r.Pattern, rec.status, s.now().Sub(started).Round(time.Millisecond))
	})
}

func statusFor(code string) int {
	switch code {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeOutside, CodeDenied:
		return http.StatusForbidden
	case CodeNotFile, CodeNotDir:
		return http.StatusUnprocessableEntity
	case CodeChanged, CodeCancelled:
		return http.StatusConflict
	case CodeBusy:
		return http.StatusTooManyRequests
	case CodeBadInput:
		return http.StatusBadRequest
	case CodeAuth:
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	e := asError(err)
	writeJSON(w, statusFor(e.Code), map[string]*Error{"error": e})
}

func readPathBody(dec *json.Decoder) (PathRequest, bool) {
	var req PathRequest
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return req, false
	}
	seen := false
	for dec.More() {
		tok, err := dec.Token()
		key, isString := tok.(string)
		if err != nil || !isString || key != "path" || seen {
			return req, false
		}
		seen = true
		if err := dec.Decode(&req.Path); err != nil {
			return req, false
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return req, false
	}
	return req, seen
}

// decodePath reads a request body that is exactly {"path": "..."}.
func decodePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	// The key is read by hand: encoding/json would match "PATH" to path and let
	// a duplicate key win. The body must be one object with the key "path" once.
	req, ok := readPathBody(dec)
	if !ok {
		writeError(w, &Error{Code: CodeBadInput, Message: "the body must be a JSON object with only a path"})
		return "", false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, &Error{Code: CodeBadInput, Message: "the body must be a single JSON object"})
		return "", false
	}
	if req.Path == "" {
		writeError(w, &Error{Code: CodeBadInput, Message: "path is required"})
		return "", false
	}
	return req.Path, true
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Health{Version: Version, InstanceID: s.instance, Roots: s.roots.Paths(), Resolver: s.roots.Resolver()})
}

func (s *Server) identity(w http.ResponseWriter, r *http.Request) {
	path, ok := decodePath(w, r)
	if !ok {
		return
	}
	f, err := s.roots.Open(path)
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()
	id, err := identityOf(f)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, IdentityResponse{InstanceID: s.instance, Identity: id})
}

// sample digests a few ranges of a file, the same value the coordinator computes
// over the network mount, so the two can be compared for the cost of 8 MiB.
func (s *Server) sample(w http.ResponseWriter, r *http.Request) {
	path, ok := decodePath(w, r)
	if !ok {
		return
	}
	// A sample reads 8 MiB: it does not take a hash slot, or it would wait minutes
	// behind two whole-file reads and its caller would give up.
	f, err := s.roots.Open(path)
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()
	before, err := identityOf(f)
	if err != nil {
		writeError(w, err)
		return
	}
	digest, err := sampledigest.Digest(r.Context(), f, before.Size)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = errChanged
		}
		writeError(w, err)
		return
	}
	after, err := identityOf(f)
	if err != nil {
		writeError(w, err)
		return
	}
	if after != before || !s.stillNames(path, before) {
		writeError(w, errChanged)
		return
	}
	writeJSON(w, http.StatusOK, SampleResponse{InstanceID: s.instance, Digest: digest, Identity: after})
}

// stillNames reports whether path opens a file with the identity seen on the
// descriptor that was read. A directory renamed above the file changes what the
// path names without touching the open file's identity.
func (s *Server) stillNames(path string, want Identity) bool {
	again, err := s.roots.Open(path)
	if err != nil {
		return false
	}
	defer again.Close()
	named, err := identityOf(again)
	return err == nil && named == want
}

type job struct {
	id   string
	path string

	cancel context.CancelFunc
	// asked is when a caller last asked about the job (unix nanoseconds, real
	// clock), so an abandoned one can be stopped.
	asked atomic.Int64
	read  atomic.Int64
	total atomic.Int64

	mu       sync.Mutex
	state    string
	result   *HashResult
	usage    *UsageResult
	err      *Error
	finished time.Time
}

func (s *Server) snapshot(j *job) Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	return Job{ID: j.id, InstanceID: s.instance, State: j.state, BytesRead: j.read.Load(), TotalBytes: j.total.Load(), Result: j.result, Usage: j.usage, Error: j.err}
}

func (s *Server) startHash(w http.ResponseWriter, r *http.Request) {
	path, ok := decodePath(w, r)
	if !ok {
		return
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithCancel(s.baseCtx)
	j := &job{id: hex.EncodeToString(idBytes), path: path, cancel: cancel, state: JobQueued}
	j.asked.Store(time.Now().UnixNano())

	s.mu.Lock()
	s.expireLocked()
	if s.closed || len(s.jobs) >= maxJobs || s.activeLocked() >= maxActiveJobs {
		s.mu.Unlock()
		cancel()
		writeError(w, &Error{Code: CodeBusy, Message: "too many hash jobs are waiting or held"})
		return
	}
	s.jobs[j.id] = j
	s.wg.Add(1)
	s.mu.Unlock()

	go s.runHash(ctx, j)
	writeJSON(w, http.StatusAccepted, s.snapshot(j))
}

// activeLocked counts the jobs still queued or running. s.mu is held.
func (s *Server) activeLocked() int {
	n := 0
	for _, j := range s.jobs {
		j.mu.Lock()
		if j.finished.IsZero() {
			n++
		}
		j.mu.Unlock()
	}
	return n
}

// expireLocked forgets finished jobs nobody collected. s.mu is held.
func (s *Server) expireLocked() {
	cutoff := s.now().Add(-jobRetention)
	for id, j := range s.jobs {
		j.mu.Lock()
		gone := !j.finished.IsZero() && j.finished.Before(cutoff)
		j.mu.Unlock()
		if gone {
			delete(s.jobs, id)
		}
	}
}

func (s *Server) lookup(id string) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(r.PathValue("id"))
	if j == nil {
		writeError(w, &Error{Code: CodeNotFound, Message: "no such job"})
		return
	}
	j.asked.Store(time.Now().UnixNano())
	writeJSON(w, http.StatusOK, s.snapshot(j))
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(r.PathValue("id"))
	if j == nil {
		writeError(w, &Error{Code: CodeNotFound, Message: "no such job"})
		return
	}
	j.cancel()
	writeJSON(w, http.StatusAccepted, s.snapshot(j))
}

func (j *job) set(state string) {
	j.mu.Lock()
	j.state = state
	j.mu.Unlock()
}

func (s *Server) finish(j *job, res *HashResult, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.finished = s.now()
	switch {
	case err == nil:
		j.state, j.result = JobDone, res
	case errors.Is(err, context.Canceled):
		j.state, j.err = JobCancelled, &Error{Code: CodeCancelled, Message: "the job was cancelled"}
	default:
		j.state, j.err = JobFailed, asError(err)
	}
}

func (s *Server) runHash(ctx context.Context, j *job) {
	defer s.wg.Done()
	defer j.cancel()
	go s.reapIfAbandoned(ctx, j)
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		s.finish(j, nil, ctx.Err())
		return
	}
	// The job reads at idle I/O priority. The thread is taken only once the job
	// has a slot, so a queue of jobs does not pin a thread each, and it is never
	// unlocked: the runtime ends the thread with the goroutine and the priority
	// does not leak to other work.
	runtime.LockOSThread()
	lowerIOPriority()
	j.set(JobRunning)
	res, err := s.hashFile(ctx, j)
	s.finish(j, res, err)
}

// reapIfAbandoned cancels the job when nobody has asked about it for orphanAfter,
// so a caller that died or never received the job id cannot leave the helper
// reading a large file (and holding a slot) for nothing.
func (s *Server) reapIfAbandoned(ctx context.Context, j *job) {
	every := max(s.orphanAfter/4, 5*time.Millisecond)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if time.Since(time.Unix(0, j.asked.Load())) > s.orphanAfter {
				j.cancel()
				return
			}
		}
	}
}

// hashFile reads the whole file from local disk. The result stands only if the
// file's identity is the same before and after the read and the path still names
// the same file.
func (s *Server) hashFile(ctx context.Context, j *job) (*HashResult, error) {
	f, err := s.roots.Open(j.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := identityOf(f)
	if err != nil {
		return nil, err
	}
	j.total.Store(before.Size)
	// The standard library uses the SHA extensions only on CPUs that also have
	// AVX2, which the NAS's Celeron lacks (98 MB/s against 555 MB/s here).
	h := simdsha256.New()
	buf := make([]byte, hashChunk)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, rerr := f.Read(buf)
		h.Write(buf[:m])
		n += int64(m)
		j.read.Store(n)
		if s.afterChunk != nil {
			s.afterChunk(j.path)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	after, err := identityOf(f)
	if err != nil {
		return nil, err
	}
	if after != before || n != before.Size {
		return nil, errChanged
	}
	if !s.stillNames(j.path, before) {
		return nil, errChanged
	}
	return &HashResult{SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: n, Identity: before}, nil
}
