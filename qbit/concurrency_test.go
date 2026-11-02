package qbit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Requests must not queue behind each other: the client used to hold its
// mutex across the whole round trip.
func TestRequestsRunSideBySide(t *testing.T) {
	var inFlight, peak, logins atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/login") {
			logins.Add(1)
			w.Write([]byte("Ok."))
			return
		}
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		w.Write([]byte("[]"))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "u", "p")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.ListFiles(context.Background(), "h"); err != nil {
				t.Error(err)
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for peak.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if peak.Load() < 4 {
		t.Fatalf("only %d requests were in flight together", peak.Load())
	}
	if logins.Load() != 1 {
		t.Fatalf("logins = %d, want one", logins.Load())
	}
}
