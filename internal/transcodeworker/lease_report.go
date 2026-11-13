package transcodeworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Reporting leases to the server.
//
// The server keeps a record of every build (Queue rows, build_list,
// build_status, build_cancel, Pause builds, /builds/nodes), so the worker
// tells it about each lease through the same builds API nbuild uses without a
// worker: it registers the lease, forwards the heartbeats with the progress
// and the output lines, and finishes it. The server then also freezes the node
// the usual way (a suspend request to this worker), which is harmless: the
// worker's own lease and the server's suspend are two records, and the
// encoders thaw when neither is on.
//
// None of it is on the lease's path. The worker grants and expires leases
// alone; the reports are retried in the background (leaseReportEvery) and an
// unreachable or refusing server only means the server's record lags. Two
// answers matter: builds_paused on registration refuses a new lease, and a
// build the operator cancelled in the server (the heartbeat's answer says so)
// ends the lease as cancelled, which the build learns from its next heartbeat.

const (
	leaseReportEvery   = 2 * time.Second
	leaseKeepaliveGap  = 20 * time.Second
	leaseRegisterTries = 5 * time.Second // between background registration attempts
	leaseReportTimeout = 3 * time.Second
)

// leaseReporter is the worker's side of the builds API.
type leaseReporter interface {
	// Register creates the build; errLeasesPaused when builds are paused.
	Register(l *leaseRecord) (serverID string, err error)
	// Heartbeat forwards progress and output. ended is the status the server
	// ended the build with, empty while it runs.
	Heartbeat(serverID string, progress *string, ttlSeconds int, lines []string) (ended string, err error)
	Finish(serverID, status string, exit *int) error
}

// httpLeaseReporter calls the builds API with a builds-only token.
type httpLeaseReporter struct {
	url       string
	tokenFile string
	node      string
	client    *http.Client
}

func newHTTPLeaseReporter(url, tokenFile, node string) *httpLeaseReporter {
	return &httpLeaseReporter{
		url: strings.TrimRight(url, "/"), tokenFile: tokenFile, node: node,
		client: &http.Client{Timeout: leaseReportTimeout},
	}
}

func (r *httpLeaseReporter) do(method, path string, body any) (int, []byte, error) {
	data, err := os.ReadFile(r.tokenFile)
	if err != nil {
		return 0, nil, errors.New("the builds token is not readable")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return 0, nil, errors.New("the builds token is empty")
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), leaseReportTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, r.url+"/api/maintenance/builds"+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		// The error text can carry the URL: say only that it failed.
		return 0, nil, errors.New("the server is unreachable")
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

func (r *httpLeaseReporter) Register(l *leaseRecord) (string, error) {
	code, out, err := r.do(http.MethodPost, "", map[string]any{
		"target": r.node, "source": l.Source, "holder": l.Holder, "kind": l.Kind,
		"title": l.Title, "preempt_mode": l.PreemptMode, "ttl_seconds": l.TTLSeconds,
		"progress": l.Progress,
	})
	if err != nil {
		return "", err
	}
	var b struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(out, &b)
	switch {
	case code == http.StatusConflict && b.Code == "builds_paused":
		return "", errLeasesPaused
	case code == http.StatusOK && b.ID != "":
		return b.ID, nil
	}
	return "", fmt.Errorf("the server answered HTTP %d", code)
}

func (r *httpLeaseReporter) Heartbeat(serverID string, progress *string, ttlSeconds int, lines []string) (string, error) {
	body := map[string]any{}
	if progress != nil {
		body["progress"] = *progress
	}
	if ttlSeconds > 0 {
		body["ttl_seconds"] = ttlSeconds
	}
	if len(lines) > 0 {
		body["log"] = lines
	}
	code, out, err := r.do(http.MethodPost, "/"+serverID+"/heartbeat", body)
	if err != nil {
		return "", err
	}
	var b struct {
		Status string `json:"status"`
		Build  struct {
			Status string `json:"status"`
		} `json:"build"`
	}
	_ = json.Unmarshal(out, &b)
	switch code {
	case http.StatusOK:
		return "", nil
	case http.StatusConflict:
		if b.Build.Status != "" {
			return b.Build.Status, nil
		}
		return "ended", nil
	case http.StatusNotFound:
		return "ended", nil
	case http.StatusBadRequest:
		// A line the server refuses must not cost the heartbeat.
		if len(lines) > 0 || progress != nil {
			return r.Heartbeat(serverID, nil, ttlSeconds, nil)
		}
	}
	return "", fmt.Errorf("the server answered HTTP %d", code)
}

func (r *httpLeaseReporter) Finish(serverID, status string, exit *int) error {
	body := map[string]any{"status": status}
	if exit != nil {
		body["exit_code"] = *exit
	}
	code, _, err := r.do(http.MethodPost, "/"+serverID+"/finish", body)
	if err != nil {
		return err
	}
	if code == http.StatusOK || code == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("the server answered HTTP %d", code)
}

func (w *Worker) leaseKickReporter() {
	if k := w.leases.kick; k != nil {
		select {
		case k <- struct{}{}:
		default:
		}
	}
}

// reportStatus is the status the server is told when a lease ends: the server
// has no "lost".
func reportStatus(status string) string {
	if status == leaseStatusLost {
		return leaseStatusFailed
	}
	return status
}

// leaseReportPass tells the server what changed, once. It never holds the
// lease lock across a network call.
func (w *Worker) leaseReportPass() {
	s := &w.leases
	rep := s.reporter
	if rep == nil {
		return
	}
	now := s.clock()
	type job struct {
		l   leaseRecord
		p   leasePending
		hbs bool
	}
	var work []job
	s.mu.Lock()
	f := w.loadLeasesLocked()
	for _, l := range f.Leases {
		switch {
		case l.active() && l.Status == leaseStatusRunning:
			p := s.pendingFor(l.ID)
			if l.ServerID == "" {
				if now.Sub(p.lastSent) < leaseRegisterTries {
					continue
				}
				p.lastSent = now
				work = append(work, job{l: *l})
				continue
			}
			if p.dirty || now.Sub(p.lastSent) >= leaseKeepaliveGap {
				work = append(work, job{l: *l, p: *p, hbs: true})
				p.dirty, p.progress, p.log, p.lastSent = false, nil, nil, now
			}
		case !l.active() && !l.Reported:
			work = append(work, job{l: *l}) // without a server id it is only marked reported
		}
	}
	s.mu.Unlock()

	for _, j := range work {
		l := j.l
		switch {
		case l.active() && l.ServerID == "":
			id, err := rep.Register(&l)
			if err == nil {
				w.leaseSetServerID(l.ID, id)
			} else if errors.Is(err, errLeasesPaused) {
				// The operator paused builds after the lease was granted: the
				// build already runs; it is not stopped for that.
				log.Printf("build lease %s: server reports builds paused; not recorded", l.ID)
			}
		case l.active():
			ended, err := rep.Heartbeat(l.ServerID, j.p.progress, 0, j.p.log)
			switch {
			case err != nil:
				// Keep what was not delivered for the next pass.
				s.mu.Lock()
				p := s.pendingFor(l.ID)
				p.dirty = true
				if j.p.progress != nil && p.progress == nil {
					p.progress = j.p.progress
				}
				p.log = append(append([]string{}, j.p.log...), p.log...)
				if len(p.log) > leaseLogMax {
					p.log = p.log[len(p.log)-leaseLogMax:]
				}
				s.mu.Unlock()
			case ended == leaseStatusCancelled:
				// The operator cancelled it in the server: the build learns it
				// from its next heartbeat, and the encoders thaw now.
				_, _ = w.leaseEnd(l.ID, leaseStatusCancelled, nil)
				w.leaseMarkReported(l.ID)
			case ended != "":
				// The server lost or finished it behind our back (it was
				// unreachable past the ttl): register it again.
				w.leaseSetServerID(l.ID, "")
			}
		default:
			if l.ServerID == "" {
				w.leaseMarkReported(l.ID)
				continue
			}
			if err := rep.Finish(l.ServerID, reportStatus(l.Status), l.ExitCode); err == nil {
				w.leaseMarkReported(l.ID)
			}
		}
	}
}

func (w *Worker) leaseUpdate(id string, fn func(l *leaseRecord)) {
	s := &w.leases
	s.mu.Lock()
	defer s.mu.Unlock()
	f := w.loadLeasesLocked()
	for _, l := range f.Leases {
		if l.ID == id {
			fn(l)
			if err := w.saveLeasesLocked(&f, s.clock()); err != nil {
				log.Printf("build leases: not saved: %v", err)
			}
			return
		}
	}
}

func (w *Worker) leaseSetServerID(id, serverID string) {
	w.leaseUpdate(id, func(l *leaseRecord) { l.ServerID = serverID })
}

func (w *Worker) leaseMarkReported(id string) {
	w.leaseUpdate(id, func(l *leaseRecord) { l.Reported = true })
}
