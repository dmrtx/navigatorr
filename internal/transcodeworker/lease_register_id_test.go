package transcodeworker

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// The server's shape of a client-chosen build id (action.buildClientID).
var serverClientID = regexp.MustCompile(`^bld_[A-Za-z0-9_-]{8,59}$`)

// The m1 case of 2026-11-17: the server could not reach this worker, so each
// create waited minutes on its hold call, the 3 s registration timed out and
// every retry became one more "running" build in the server. A registration
// whose answer is lost and then retried must leave one server build.
func TestRetriedRegistrationMakesOneServerBuild(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	rep := &fakeReporter{loseAnswers: 3}
	w.leases.reporter = rep
	var offset time.Duration
	w.leases.now = func() time.Time { return time.Now().Add(offset) }

	l, err := w.LeaseCreate(leaseReq("up android server"))
	if err != nil {
		t.Fatal(err)
	}
	if l.ServerID != "" {
		t.Fatalf("the first answer was lost: %+v", l)
	}
	for i := 0; i < 4; i++ {
		offset += leaseRegisterTries + time.Second
		w.leaseReportPass()
	}
	got, err := w.LeaseGet(l.ID)
	if err != nil {
		t.Fatal(err)
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	if len(rep.keys) != 4 {
		t.Fatalf("one create and three retries, got %v", rep.keys)
	}
	for _, k := range rep.keys {
		if k != l.ID {
			t.Fatalf("every retry asks for the lease's own id %q, got %v", l.ID, rep.keys)
		}
	}
	if len(rep.byKey) != 1 || got.ServerID != rep.byKey[l.ID] {
		t.Fatalf("one server build, the lease's: %v, lease %+v", rep.byKey, got)
	}
}

// A build the server ended or forgot is registered again under a new id (the
// old one would answer the ended build), valid for the server, and each later
// re-registration gets yet another.
func TestReRegistrationUsesAFreshValidID(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	rep := &fakeReporter{}
	w.leases.reporter = rep
	l, err := w.LeaseCreate(leaseReq("compile"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for round := 0; round < 3; round++ {
		if _, err := w.LeaseHeartbeat(l.ID, ptr("x"), 0, nil); err != nil {
			t.Fatal(err)
		}
		rep.mu.Lock()
		rep.gone = true
		rep.mu.Unlock()
		w.leaseReportPass()
		w.leases.pending = nil
		w.leaseReportPass()
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	for _, k := range rep.keys {
		if !serverClientID.MatchString(k) {
			t.Fatalf("id %q is not one the server accepts", k)
		}
		if seen[k] {
			t.Fatalf("id %q asked twice: %v", k, rep.keys)
		}
		seen[k] = true
	}
	if len(rep.keys) != 4 || len(rep.registers) != 4 {
		t.Fatalf("the first registration and three after each end: %v", rep.keys)
	}
	got, _ := w.LeaseGet(l.ID)
	if got.RegGen != 3 || got.ServerID != rep.byKey[rep.keys[3]] {
		t.Fatalf("lease follows the newest server build: %+v %v", got, rep.byKey)
	}
}

// The longest client lease id still makes a registration id the server takes.
func TestRegistrationIDOfALongLeaseID(t *testing.T) {
	long := "bld_" + strings.Repeat("a", 56)
	for gen := 0; gen < 3; gen++ {
		l := leaseRecord{ID: long, RegGen: gen}
		if id := l.registrationID(); !serverClientID.MatchString(id) {
			t.Fatalf("gen %d: %q", gen, id)
		}
	}
}

// The HTTP reporter sends the id, and a server from before client ids (it
// refuses unknown fields with a 400) still gets the build, without it.
func TestHTTPLeaseReporterSendsTheIDAndFallsBackForAnOldServer(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, old := range []bool{false, true} {
		var mu sync.Mutex
		var ids []any
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			ids = append(ids, body["id"])
			mu.Unlock()
			if _, has := body["id"]; has && old {
				rw.WriteHeader(http.StatusBadRequest)
				_, _ = rw.Write([]byte(`{"error":"send {...}"}`))
				return
			}
			_, _ = rw.Write([]byte(`{"id":"bld_ok"}`))
		}))
		rep := newHTTPLeaseReporter(srv.URL, tokenFile, "m1")
		id, err := rep.Register(&leaseRecord{ID: "bld_lease0001", Title: "t", PreemptMode: "suspend", TTLSeconds: 300})
		srv.Close()
		if err != nil || id != "bld_ok" {
			t.Fatalf("old=%v: %q %v", old, id, err)
		}
		mu.Lock()
		want := []any{"bld_lease0001"}
		if old {
			want = append(want, nil)
		}
		if len(ids) != len(want) || ids[0] != want[0] || (old && ids[1] != nil) {
			t.Fatalf("old=%v: ids sent %v, want %v", old, ids, want)
		}
		mu.Unlock()
	}
}

// An id the server says names another build is never asked for again: the
// lease moves to a fresh id and gets registered (else it retried forever).
func TestTakenRegistrationIDMovesToAFreshOne(t *testing.T) {
	w, _, _ := newLeaseWorker(t, 2, newStubSpawn())
	req := leaseReq("compile")
	req.ID = "bld_clientid01"
	rep := &fakeReporter{taken: map[string]bool{"bld_clientid01": true}}
	w.leases.reporter = rep
	var offset time.Duration
	w.leases.now = func() time.Time { return time.Now().Add(offset) }
	l, err := w.LeaseCreate(req)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		offset += leaseRegisterTries + time.Second
		w.leaseReportPass()
	}
	got, _ := w.LeaseGet(l.ID)
	rep.mu.Lock()
	defer rep.mu.Unlock()
	if len(rep.keys) != 2 || rep.keys[1] == "bld_clientid01" || got.ServerID == "" || got.ServerID != rep.byKey[rep.keys[1]] {
		t.Fatalf("keys %v, lease %+v", rep.keys, got)
	}
}

// The HTTP reporter reads the server's build_id_taken as such, not as paused.
func TestHTTPLeaseReporterReadsATakenID(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusConflict)
		_, _ = rw.Write([]byte(`{"error":"taken","code":"build_id_taken"}`))
	}))
	defer srv.Close()
	_, err := newHTTPLeaseReporter(srv.URL, tokenFile, "m1").Register(&leaseRecord{ID: "bld_lease0001", Title: "t"})
	if !errors.Is(err, errRegistrationIDTaken) {
		t.Fatalf("got %v", err)
	}
}
