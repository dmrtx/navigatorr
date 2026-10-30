package maintenanceui

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fetch(h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("X-Navigatorr-Request", "1")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func gunzip(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestShellIsCompressedAndRevalidated(t *testing.T) {
	_, h := testUI(t)
	plain := fetch(h, "/app.js", nil)
	if plain.Code != 200 || plain.Header().Get("Content-Encoding") != "" || plain.Body.Len() < 100000 {
		t.Fatal("a client that does not ask for gzip gets the plain script", plain.Code, plain.Header())
	}
	zipped := fetch(h, "/app.js", map[string]string{"Accept-Encoding": "gzip, br"})
	if zipped.Code != 200 || zipped.Header().Get("Content-Encoding") != "gzip" || zipped.Header().Get("Vary") != "Accept-Encoding" || zipped.Header().Get("Content-Length") != "" {
		t.Fatal("script is not compressed", zipped.Code, zipped.Header())
	}
	if zipped.Body.Len()*2 > plain.Body.Len() {
		t.Fatalf("compressed script is %d bytes of %d", zipped.Body.Len(), plain.Body.Len())
	}
	if gunzip(t, zipped) != plain.Body.String() {
		t.Fatal("compressed script differs from the plain one")
	}
	if got := zipped.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Fatal(got)
	}
	// The browser may keep the shell only if it asks again every time.
	etag := plain.Header().Get("ETag")
	if etag == "" || plain.Header().Get("Cache-Control") != "no-cache" || zipped.Header().Get("ETag") != etag {
		t.Fatal("shell needs a tag and must be revalidated", plain.Header())
	}
	for _, accept := range []string{"", "gzip"} {
		again := fetch(h, "/app.js", map[string]string{"If-None-Match": etag, "Accept-Encoding": accept})
		if again.Code != 304 || again.Body.Len() != 0 || again.Header().Get("Content-Encoding") != "" {
			t.Fatal("unchanged shell is sent again", accept, again.Code, again.Body.Len(), again.Header())
		}
	}
	if stale := fetch(h, "/app.js", map[string]string{"If-None-Match": `W/"0000"`}); stale.Code != 200 {
		t.Fatal(stale.Code)
	}
	// Every shell file has its own tag, so a changed script does not hide a changed page.
	if fetch(h, "/app.css", nil).Header().Get("ETag") == etag || fetch(h, "/", nil).Header().Get("ETag") == etag {
		t.Fatal("tags are shared between files")
	}
	// The worker script is checked on every start and is never kept.
	if w := fetch(h, "/sw.js", map[string]string{"Accept-Encoding": "gzip"}); w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("ETag") != "" {
		t.Fatal(w.Header())
	}
}

func TestOnlyTextIsCompressed(t *testing.T) {
	_, h := testUI(t)
	accept := map[string]string{"Accept-Encoding": "gzip"}
	if w := fetch(h, "/icon-512.png", accept); w.Header().Get("Content-Encoding") != "" {
		t.Fatal("an image was compressed again", w.Header())
	}
	if w := fetch(h, "/app.js", map[string]string{"Accept-Encoding": "gzip;q=0"}); w.Header().Get("Content-Encoding") != "" {
		t.Fatal("gzip;q=0 refuses gzip")
	}
	if w := fetch(h, "/app.js", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-99"}); w.Header().Get("Content-Encoding") != "" || w.Code != 206 || w.Body.Len() != 100 {
		t.Fatal("a range is answered from the plain bytes", w.Code, w.Body.Len(), w.Header())
	}
	// Failures and answers behind sign-in are compressed like any other, and stay uncached.
	w := fetch(h, "/api/maintenance/bootstrap", accept)
	if w.Code != 401 || w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(gunzip(t, w), "sign in") {
		t.Fatal(w.Code, w.Header())
	}
	r := httptest.NewRequest("GET", "/api/maintenance/bootstrap", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Accept-Encoding", "gzip")
	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, r)
	if ok.Code != 200 || ok.Header().Get("Content-Encoding") != "gzip" || !strings.HasPrefix(ok.Header().Get("Content-Type"), "application/json") || !strings.HasPrefix(gunzip(t, ok), "{") {
		t.Fatal(ok.Code, ok.Header())
	}
	if ok.Header().Get("ETag") != "" || ok.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("answers about jobs must not be tagged or kept", ok.Header())
	}
}
