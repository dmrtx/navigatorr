package transcodeworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The lease socket.
//
// A Unix socket in the worker's state directory (default
// <state_dir>/.builds/lease.sock, in a 0700 directory, the socket itself 0600:
// only the user the worker runs as can reach it; there is no token; with
// build_socket_group, also that group: 0660). It
// speaks HTTP and, on purpose, the shape of the server's builds API, so a
// caller that already speaks that one (nbuild) only changes where it sends:
//
//	GET  /v1/node                                   {"node": NAME, "leases": true}
//	POST /api/maintenance/builds                    create (an optional "id" makes asking again
//	                                                safe); 200 with the build; 409
//	                                                {"error", "code": "builds_paused"} while
//	                                                the operator paused builds on the node
//	GET  /api/maintenance/builds/{id}               one build
//	POST /api/maintenance/builds/{id}/heartbeat     extend; 409 {"build": {...}} if it ended
//	POST /api/maintenance/builds/{id}/finish        end it; finishing an ended build is 200
//
// A normal build is "running" in the answer to its create (every running encoder
// has frozen by then; 503 if one did not in time, and nothing is held); a low one is "waiting" until the node is idle, and its
// heartbeats answer "running" once it is.

const (
	maxLeaseBodyBytes   = 1 << 20
	leaseTickEvery      = time.Second
	leaseSocketDirName  = ".builds"
	leaseSocketFileName = "lease.sock"
)

// LeaseSocketPath is where the lease socket is, or "" when leases are off.
func (w *Worker) LeaseSocketPath() string {
	if w == nil || w.cfg == nil || strings.TrimSpace(w.cfg.BuildNode) == "" {
		return ""
	}
	if p := strings.TrimSpace(w.cfg.BuildSocket); p != "" {
		return p
	}
	if w.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(w.cfg.StateDir, leaseSocketDirName, leaseSocketFileName)
}

// StartBuildLeases runs the tick (which expires leases, promotes low ones and
// heals an unusable record) and the server reports until ctx ends, and serves
// the lease socket. Leases are on only for a worker with a build_node. The tick
// runs even when the socket cannot be opened (the error is returned; the worker
// works without leases), so a record never stays unusable.
func (w *Worker) StartBuildLeases(ctx context.Context) error {
	path := w.LeaseSocketPath()
	if path == "" {
		return nil
	}
	if leaseSocketLive(path) {
		return errLeaseSocketTaken
	}
	s := &w.leases
	s.mu.Lock()
	if s.reporter == nil && strings.TrimSpace(w.cfg.BuildsURL) != "" && strings.TrimSpace(w.cfg.BuildsTokenFile) != "" {
		s.reporter = newHTTPLeaseReporter(w.cfg.BuildsURL, w.cfg.BuildsTokenFile, w.cfg.BuildNode)
	}
	s.kick = make(chan struct{}, 1)
	s.mu.Unlock()
	w.leaseRestart()
	w.leaseLoops(ctx)
	return w.serveLeaseSocket(ctx, path)
}

// leaseLoops starts the tick and the report pass, apart so that a slow server
// never delays an expiry.
func (w *Worker) leaseLoops(ctx context.Context) {
	s := &w.leases
	go func() {
		tick := time.NewTicker(leaseTickEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				w.leaseTick()
			}
		}
	}()
	go func() {
		report := time.NewTicker(leaseReportEvery)
		defer report.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-report.C:
				w.leaseReportPass()
			case <-s.kick:
				w.leaseReportPass()
			}
		}
	}()
}

var errLeaseSocketTaken = errors.New("another worker already serves the build socket")

// leaseSocketLive is whether a live worker answers on the socket.
func leaseSocketLive(path string) bool {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (w *Worker) serveLeaseSocket(ctx context.Context, path string) error {
	if len(path) >= 100 {
		return fmt.Errorf("build socket path is too long for a Unix socket (%d bytes): set build_socket", len(path))
	}
	// The socket is bound first in a temporary directory beside it (see
	// listenLeaseSocket), whose path is longer than the directory's by at most
	// leaseBindExtra(path) bytes.
	if n := len(filepath.Dir(path)) + leaseBindExtra(path); n >= 104 {
		return fmt.Errorf("build socket directory is too long for a Unix socket (%d bytes while binding): set a shorter build_socket", n)
	}
	// The group first: a bad one changes nothing on disk (not even a stale
	// socket of an earlier worker).
	gid, err := leaseSocketGroupID(w.cfg.BuildSocketGroup, strings.TrimSpace(w.cfg.BuildSocket) != "")
	if err != nil {
		return err
	}
	if err := prepareLeaseSocketDir(filepath.Dir(path), strings.TrimSpace(w.cfg.BuildSocket) == ""); err != nil {
		return err
	}
	// One worker per socket: the lock is held for as long as the socket is
	// served, so a second worker never touches the first one's socket.
	lock, err := lockLeaseSocket(path)
	if err != nil {
		return err
	}
	// A socket that answers belongs to a live worker (one that predates the
	// lock); one that does not is replaced once the new one is ready.
	if leaseSocketLive(path) {
		unlockLeaseSocket(lock)
		return errLeaseSocketTaken
	}
	removeLeaseBindLeftovers(path)
	ln, err := listenLeaseSocket(path, gid)
	if err != nil {
		unlockLeaseSocket(lock)
		return err
	}
	served, _ := os.Lstat(path)
	srv := &http.Server{
		Handler:           w.leaseHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      40 * time.Second, // a create may wait for another, then for the freeze
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("build socket stopped: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if srv.Shutdown(shutdown) != nil {
			_ = srv.Close() // no connection outlives the lock
		}
		// Only the socket this worker linked: never a file put there since.
		if now, err := os.Lstat(path); err == nil && served != nil && os.SameFile(now, served) {
			_ = os.Remove(path)
		}
		unlockLeaseSocket(lock)
	}()
	return nil
}

// leaseBindPrefix names the bind directories of one socket: "." plus the
// socket's name plus ".b", then MkdirTemp's random digits.
// A "*" would be MkdirTemp's placeholder, so it becomes "_".
func leaseBindPrefix(path string) string {
	return "." + strings.ReplaceAll(filepath.Base(path), "*", "_") + ".b"
}

// leaseBindExtra is how much longer than the socket's directory the bind path
// can be: "/" plus the prefix plus MkdirTemp's digits (at most 10) plus "/s".
func leaseBindExtra(path string) int { return 1 + len(leaseBindPrefix(path)) + 10 + 2 }

// listenLeaseSocket binds the socket in a new 0700 directory next to path and
// gives it its final mode and group there, so no umask ever opens it early.
// Only then does a stale socket at path go, and the new one is hard-linked
// into place, which never replaces anything (a file that is not a socket stays
// and the start fails). A failure leaves nothing behind.
func listenLeaseSocket(path string, gid int) (net.Listener, error) {
	tmp, err := os.MkdirTemp(filepath.Dir(path), leaseBindPrefix(path))
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	bind := filepath.Join(tmp, "s")
	ln, err := net.Listen("unix", bind)
	if err != nil {
		return nil, err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false) // the socket moves; shutdown removes path
	}
	if err := setLeaseSocketAccess(bind, gid); err != nil {
		_ = ln.Close()
		return nil, err
	}
	// A socket left by a worker that died (the caller saw it does not answer).
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	if err := os.Link(bind, path); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("build socket: %w", err)
	}
	return ln, nil
}

// lockLeaseSocket takes path+".lock" for as long as the socket is served.
// Held by another worker: errLeaseSocketTaken.
func lockLeaseSocket(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("build socket lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLeaseSocketTaken
		}
		return nil, fmt.Errorf("build socket lock: %w", err)
	}
	return f, nil
}

func unlockLeaseSocket(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// removeLeaseBindLeftovers clears the bind directories this socket's worker
// left when it was killed mid-start: named by leaseBindPrefix and digits only,
// empty or holding nothing but the "s" socket. Called with this socket's lock
// held, so no start of this socket is using one; other sockets' are never
// matched.
func removeLeaseBindLeftovers(path string) {
	dir, prefix := filepath.Dir(path), leaseBindPrefix(path)
	names, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, n := range names {
		rest, ok := strings.CutPrefix(n.Name(), prefix)
		if !ok || rest == "" || strings.Trim(rest, "0123456789") != "" || !n.IsDir() {
			continue
		}
		m := filepath.Join(dir, n.Name())
		entries, err := os.ReadDir(m)
		if err != nil || len(entries) > 1 {
			continue
		}
		if len(entries) == 1 && (entries[0].Name() != "s" || entries[0].Type()&os.ModeSocket == 0 ||
			leaseSocketLive(filepath.Join(m, "s"))) {
			continue // not a dead bind of ours
		}
		_ = os.RemoveAll(m)
	}
}

// leaseSocketGroupID is the group the socket opens to, or -1 for owner-only.
// A group needs a socket path of the operator's own: the worker's default
// directory is private, so a group could not reach a socket there anyway.
func leaseSocketGroupID(name string, ownPath bool) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return -1, nil
	}
	if !ownPath {
		return -1, errors.New("build_socket_group needs build_socket (a directory that group can enter)")
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return -1, fmt.Errorf("build_socket_group: %w", err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return -1, fmt.Errorf("build_socket_group %q: bad gid %q", name, g.Gid)
	}
	return gid, nil
}

// setLeaseSocketAccess makes the socket owner-only (gid -1) or opens it to
// one group: the group is set before the mode, so the socket is never open to
// the group it had when it was made.
func setLeaseSocketAccess(path string, gid int) error {
	if gid < 0 {
		return os.Chmod(path, 0o600)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if err := os.Lchown(path, -1, gid); err != nil {
		return fmt.Errorf("build_socket_group: %w (the worker's user must be in the group)", err)
	}
	return os.Chmod(path, 0o660)
}

// prepareLeaseSocketDir makes the directory the socket goes in. The worker's
// own (<state_dir>/.builds) is created 0700. A directory the operator chose is
// theirs: it must already exist, belong to this user and not be writable by
// anyone else; it is never changed.
func prepareLeaseSocketDir(dir string, own bool) error {
	if own {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.Chmod(dir, 0o700)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("build_socket directory: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o022 != 0 {
		return errors.New("build_socket directory must be a directory of this user that others cannot write to")
	}
	return nil
}

func (w *Worker) leaseHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/node", func(rw http.ResponseWriter, r *http.Request) {
		writeHTTPJSON(rw, http.StatusOK, map[string]any{"node": w.cfg.BuildNode, "leases": true})
	})
	mux.HandleFunc("POST /api/maintenance/builds", w.handleLeaseCreate)
	mux.HandleFunc("GET /api/maintenance/builds/{id}", w.handleLeaseOne)
	mux.HandleFunc("POST /api/maintenance/builds/{id}/heartbeat", w.handleLeaseHeartbeat)
	mux.HandleFunc("POST /api/maintenance/builds/{id}/finish", w.handleLeaseFinish)
	return mux
}

// leaseJSON is a lease in the shape of the server's build record.
func (w *Worker) leaseJSON(l leaseRecord) map[string]any {
	out := map[string]any{
		"id": l.ID, "target": w.cfg.BuildNode, "source": l.Source, "holder": l.Holder, "kind": l.Kind,
		"title": l.Title, "preempt_mode": l.PreemptMode, "priority": l.Priority, "status": l.Status,
		"created_at":           l.CreatedAt.Format(time.RFC3339),
		"last_heartbeat_at":    l.LastHeartbeatAt.Format(time.RFC3339),
		"expires_at":           l.ExpiresAt.Format(time.RFC3339),
		"holds_transcode_node": true, "local": true,
	}
	if l.StartedAt != nil {
		out["started_at"] = l.StartedAt.Format(time.RFC3339)
	}
	if l.EndedAt != nil {
		out["ended_at"] = l.EndedAt.Format(time.RFC3339)
	}
	if l.Progress != "" {
		out["progress"] = l.Progress
	}
	if l.ExitCode != nil {
		out["exit_code"] = *l.ExitCode
	}
	return out
}

func (w *Worker) leaseError(rw http.ResponseWriter, l leaseRecord, err error) {
	var invalid *leaseInvalidError
	switch {
	case errors.As(err, &invalid):
		writeHTTPError(rw, http.StatusBadRequest, invalid.Reason)
	case errors.Is(err, errLeasesPaused):
		writeHTTPJSON(rw, http.StatusConflict, map[string]string{"error": "builds paused on " + w.cfg.BuildNode, "code": "builds_paused"})
	case errors.Is(err, errLeaseFreeze):
		writeHTTPError(rw, http.StatusServiceUnavailable, "the encoders did not freeze in time; try again")
	case errors.Is(err, errLeaseNotFound):
		writeHTTPError(rw, http.StatusNotFound, "build not found")
	case errors.Is(err, errLeaseEnded):
		writeHTTPJSON(rw, http.StatusConflict, map[string]any{"error": "build already ended", "build": w.leaseJSON(l)})
	default:
		log.Printf("build lease: %v", err)
		writeHTTPError(rw, http.StatusInternalServerError, "build not saved")
	}
}

func decodeLeaseBody(rw http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(rw, r.Body, maxLeaseBodyBytes)
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

func (w *Worker) handleLeaseCreate(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          string `json:"id"`
		Target      string `json:"target"`
		Source      string `json:"source"`
		Holder      string `json:"holder"`
		Kind        string `json:"kind"`
		Title       string `json:"title"`
		PreemptMode string `json:"preempt_mode"`
		Priority    string `json:"priority"`
		TTLSeconds  int    `json:"ttl_seconds"`
		Progress    string `json:"progress"`
	}
	if err := decodeLeaseBody(rw, r, &body); err != nil {
		writeHTTPError(rw, http.StatusBadRequest, `send {"target", "source", "title", "holder"?, "kind"?, "preempt_mode"?, "priority"?, "ttl_seconds"?, "progress"?}`)
		return
	}
	if strings.TrimSpace(body.Target) != w.cfg.BuildNode {
		// Another node's build is the server's business: the caller falls back.
		writeHTTPJSON(rw, http.StatusConflict, map[string]string{"error": "this socket serves another node", "code": "wrong_node"})
		return
	}
	l, err := w.LeaseCreateContext(r.Context(), LeaseRequest{ID: body.ID, Source: body.Source, Holder: body.Holder, Kind: body.Kind, Title: body.Title,
		PreemptMode: body.PreemptMode, Priority: body.Priority, Progress: body.Progress, TTLSeconds: body.TTLSeconds})
	if err != nil {
		w.leaseError(rw, l, err)
		return
	}
	writeHTTPJSON(rw, http.StatusOK, w.leaseJSON(l))
}

func (w *Worker) handleLeaseOne(rw http.ResponseWriter, r *http.Request) {
	l, err := w.LeaseGet(r.PathValue("id"))
	if err != nil && !errors.Is(err, errLeaseEnded) {
		w.leaseError(rw, l, err)
		return
	}
	writeHTTPJSON(rw, http.StatusOK, w.leaseJSON(l))
}

func (w *Worker) handleLeaseHeartbeat(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// The lease first, from the path alone: unknown is 404, over is 409,
	// whatever the body is.
	if l, err := w.LeaseGet(id); err != nil {
		w.leaseError(rw, l, err)
		return
	}
	var body struct {
		Progress   json.RawMessage `json:"progress"`
		TTLSeconds json.RawMessage `json:"ttl_seconds"`
		Log        json.RawMessage `json:"log"`
	}
	if err := decodeLeaseBody(rw, r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeHTTPError(rw, http.StatusBadRequest, `send {"progress"?, "ttl_seconds"?, "log"?: [lines]} or nothing`)
		return
	}
	var progress *string
	var ttl int
	var lines []string
	if len(body.Progress) > 0 && string(body.Progress) != "null" {
		var p string
		if json.Unmarshal(body.Progress, &p) != nil {
			writeHTTPError(rw, http.StatusBadRequest, "progress must be a string")
			return
		}
		progress = &p
	}
	if len(body.TTLSeconds) > 0 && string(body.TTLSeconds) != "null" && json.Unmarshal(body.TTLSeconds, &ttl) != nil {
		writeHTTPError(rw, http.StatusBadRequest, "ttl_seconds must be a whole number")
		return
	}
	if len(body.Log) > 0 && string(body.Log) != "null" {
		var items []*string
		if json.Unmarshal(body.Log, &items) != nil {
			items = []*string{nil}
		}
		for _, line := range items {
			if line == nil {
				writeHTTPError(rw, http.StatusBadRequest, "log must be an array of strings")
				return
			}
			lines = append(lines, *line)
		}
	}
	l, err := w.LeaseHeartbeat(id, progress, ttl, lines)
	if err != nil {
		w.leaseError(rw, l, err)
		return
	}
	writeHTTPJSON(rw, http.StatusOK, w.leaseJSON(l))
}

func (w *Worker) handleLeaseFinish(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Status   string `json:"status"`
		ExitCode *int   `json:"exit_code"`
	}
	if err := decodeLeaseBody(rw, r, &body); err != nil {
		writeHTTPError(rw, http.StatusBadRequest, `send {"status": "succeeded"|"failed"|"cancelled", "exit_code"?}`)
		return
	}
	l, err := w.LeaseFinish(r.PathValue("id"), body.Status, body.ExitCode)
	if err != nil {
		w.leaseError(rw, l, err)
		return
	}
	writeHTTPJSON(rw, http.StatusOK, w.leaseJSON(l))
}
