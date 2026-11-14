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

// A lease the server cannot record is no longer silent: the failure is logged
// once for each cause, and again when it works.
func TestLeaseRegistrationFailureIsLoggedOncePerCause(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	rep := &fakeReporter{down: true}
	w.leases.reporter = rep
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	l, err := w.LeaseCreate(leaseReq("verify"))
	if err != nil {
		t.Fatal(err)
	}
	if l.ServerID != "" {
		t.Fatalf("the server is down: %+v", l)
	}
	for i := 0; i < 3; i++ {
		w.leaseNoteRegister(l.ID, "the server answered HTTP 404")
	}
	out := buf.String()
	if n := strings.Count(out, "not registered with the server (the server answered HTTP 404)"); n != 1 {
		t.Fatalf("one line for a repeated cause, got %d:\n%s", n, out)
	}
	w.leaseNoteRegister(l.ID, "the builds token is not readable")
	if !strings.Contains(buf.String(), "the builds token is not readable") {
		t.Fatalf("a different cause is logged:\n%s", buf.String())
	}
	w.leaseNoteRegister(l.ID, "")
	if !strings.Contains(buf.String(), "registered with the server") {
		t.Fatalf("recovery is logged:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "http://") {
		t.Fatalf("no URL in the log:\n%s", buf.String())
	}
}
