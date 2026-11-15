package transcodeworker

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// builds_url names the server. A URL that already ends with the builds path
// (the playbook wrote it that way) is the same server: the reports must not go
// to .../builds/api/maintenance/builds, where the server has no route.
func TestLeaseBaseURLAcceptsTheServerWithOrWithoutTheBuildsPath(t *testing.T) {
	for in, want := range map[string]string{
		"http://oz:8097":                          "http://oz:8097",
		"http://oz:8097/":                         "http://oz:8097",
		"http://oz:8097/api/maintenance/builds":   "http://oz:8097",
		"http://oz:8097/api/maintenance/builds/":  "http://oz:8097",
		" http://oz:8097/api/maintenance/builds ": "http://oz:8097",
		"http://oz/prefix":                        "http://oz/prefix",
	} {
		if got := leaseBaseURL(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestHTTPLeaseReporterRegistersAtTheBuildsRouteWhateverTheConfiguredURL(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/api/maintenance/builds" {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"id":"bld_ok"}`))
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, configured := range []string{srv.URL, srv.URL + "/api/maintenance/builds"} {
		rep := newHTTPLeaseReporter(configured, tokenFile, "m1")
		id, err := rep.Register(&leaseRecord{Title: "t", PreemptMode: "suspend", TTLSeconds: 300})
		if err != nil || id != "bld_ok" {
			t.Fatalf("%q: %q %v", configured, id, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if p != "POST /api/maintenance/builds" {
			t.Fatalf("a request went to %q", p)
		}
	}
}

// A lease the server cannot record is no longer silent: the real report pass
// logs the failure once for each cause however often it retries, says when it
// works, and a lease that ends without ever being recorded says so.
func TestLeaseReportPassLogsRegistrationFailureOncePerCause(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	rep := &fakeReporter{down: true}
	w.leases.reporter = rep
	var offset time.Duration
	w.leases.now = func() time.Time { return time.Now().Add(offset) }
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	l, err := w.LeaseCreate(leaseReq("verify"))
	if err != nil {
		t.Fatal(err)
	}
	pass := func() {
		offset += leaseRegisterTries + time.Second // the next attempt is due
		w.leaseReportPass()
	}
	for i := 0; i < 3; i++ {
		pass()
	}
	const cause = "not registered with the server (the server is unreachable)"
	if n := strings.Count(buf.String(), cause); n != 1 {
		t.Fatalf("one line for a repeated cause, got %d:\n%s", n, buf.String())
	}
	rep.mu.Lock()
	rep.down = false
	rep.mu.Unlock()
	pass()
	if !strings.Contains(buf.String(), "registered with the server") || strings.Contains(buf.String(), "http://") {
		t.Fatalf("recovery is logged, without a URL:\n%s", buf.String())
	}

	// A lease that ends before the server ever recorded it says so.
	rep.mu.Lock()
	rep.down = true
	rep.mu.Unlock()
	l2, err := w.LeaseCreate(leaseReq("second"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.LeaseFinish(l2.ID, leaseStatusSucceeded, ptr(0))
	pass()
	if !strings.Contains(buf.String(), "build lease "+l2.ID+" ended (succeeded) without the server having a record of it") {
		t.Fatalf("an unrecorded end is logged:\n%s", buf.String())
	}
	_ = l
}
