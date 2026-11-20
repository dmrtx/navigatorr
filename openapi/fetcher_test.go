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
