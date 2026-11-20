package openapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func TestIsLoopbackURL(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:8098/transcode/v1/openapi.json": true,
		"http://127.1.2.3/spec.json":                      true,
		"http://[::1]:8098/spec.json":                     true,
		"http://localhost:8098/spec.json":                 true,
		"http://LocalHost./spec.json":                     true,
		"http://api.localhost/spec.json":                  true,
		"https://raw.githubusercontent.com/x/spec.json":   false,
		"http://192.168.1.10:8989/api/v3/openapi.json":    false,
		"http://localhost.example.com/spec.json":          false,
		"http://10.0.0.1/spec.json":                       false,
		"::not a url":                                     false,
	}
	for raw, want := range cases {
		if got := isLoopbackURL(raw); got != want {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

// A spec this server serves itself must reflect the running build: a copy
// cached on disk by the previous build is ignored, and nothing is written.
func TestFetchLoopbackSpecSkipsDiskCache(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"openapi":"3.0.0","info":{"title":"new"}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := NewCache(dir)
	url := srv.URL + "/transcode/v1/openapi.json"
	if err := c.Put(url, []byte(`{"openapi":"3.0.0","info":{"title":"old"}}`)); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		data, err := Fetch(context.Background(), url, c)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != `{"openapi":"3.0.0","info":{"title":"new"}}` {
			t.Fatalf("fetch %d returned %s, want the live spec", i, data)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("server hit %d times, want 2 (no cache hits)", n)
	}
	if got, err := os.ReadFile(c.cacheFile(url)); err != nil || string(got) != `{"openapi":"3.0.0","info":{"title":"old"}}` {
		t.Fatalf("cache file was rewritten: %q, %v", got, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A remote spec keeps the disk cache: the first fetch stores it, the second is
// served from disk without a request. A loopback spec leaves no file behind.
func TestFetchRemoteSpecUsesDiskCache(t *testing.T) {
	var calls atomic.Int32
	orig := fetchClient.Transport
	fetchClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		rec := httptest.NewRecorder()
		rec.WriteString(`{"openapi":"3.0.0"}`)
		return rec.Result(), nil
	})
	t.Cleanup(func() { fetchClient.Transport = orig })

	dir := t.TempDir()
	c := NewCache(dir)
	const remote = "https://specs.example.com/openapi.json"
	for i := 0; i < 2; i++ {
		if data, err := Fetch(context.Background(), remote, c); err != nil || string(data) != `{"openapi":"3.0.0"}` {
			t.Fatalf("fetch %d: %q, %v", i, data, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("remote spec requested %d times, want 1 (second from cache)", n)
	}

	const loopback = "http://127.0.0.1:8098/nodes/v1/openapi.json"
	if _, err := Fetch(context.Background(), loopback, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.cacheFile(loopback)); !os.IsNotExist(err) {
		t.Fatalf("loopback spec was written to the cache (stat err %v)", err)
	}
}
