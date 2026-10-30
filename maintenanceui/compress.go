package maintenanceui

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/http"
	"strings"
	"sync"
)

// gzipResponses compresses text responses for clients that accept gzip. The
// console script alone is close to half a megabyte, and the queue answer grows
// with every page the person loads; both shrink to about a quarter.
func gzipResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A range reply names byte offsets of the identity body, and a HEAD reply has none.
		if r.Method == http.MethodHead || r.Header.Get("Range") != "" || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.finish()
		next.ServeHTTP(gw, r)
	})
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		// "gzip;q=0" means the client refuses it.
		params = strings.ReplaceAll(strings.ToLower(params), " ", "")
		return params != "q=0" && params != "q=0.0" && params != "q=0.00" && params != "q=0.000"
	}
	return false
}

func compressibleType(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(media, "text/"), strings.HasSuffix(media, "+json"), strings.HasSuffix(media, "+xml"):
		return true
	}
	switch media {
	case "application/json", "application/javascript", "application/xml", "image/svg+xml":
		return true
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	zw      *gzip.Writer
	decided bool
}

func (g *gzipWriter) decide(code int) {
	if g.decided {
		return
	}
	g.decided = true
	h := g.Header()
	if code < 200 || code == http.StatusNoContent || code == http.StatusNotModified || h.Get("Content-Encoding") != "" || !compressibleType(h.Get("Content-Type")) {
		return
	}
	h.Add("Vary", "Accept-Encoding")
	h.Set("Content-Encoding", "gzip")
	h.Del("Content-Length")
	// Speed over the last few percent: the answer is built per request.
	g.zw, _ = gzip.NewWriterLevel(g.ResponseWriter, gzip.BestSpeed)
}

func (g *gzipWriter) WriteHeader(code int) {
	g.decide(code)
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	g.decide(http.StatusOK)
	if g.zw != nil {
		return g.zw.Write(p)
	}
	return g.ResponseWriter.Write(p)
}

func (g *gzipWriter) Flush() {
	if g.zw != nil {
		_ = g.zw.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) finish() {
	if g.zw != nil {
		_ = g.zw.Close()
	}
}

var (
	shellTags     = map[string]string{}
	shellTagsOnce sync.Once
)

// shellTag names the bytes of one shell file. The files are embedded, so the
// tag only changes with a new build.
func shellTag(path string) string {
	shellTagsOnce.Do(func() {
		for _, p := range shellPaths {
			name := strings.TrimPrefix(p, "/")
			if name == "" {
				name = "index.html"
			}
			data, err := assets.ReadFile("assets/" + name)
			if err != nil {
				continue
			}
			sum := sha256.Sum256(data)
			shellTags[p] = `W/"` + hex.EncodeToString(sum[:8]) + `"`
		}
	})
	return shellTags[path]
}
