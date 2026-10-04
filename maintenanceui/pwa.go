package maintenanceui

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
)

var shellPaths = []string{"/", "/app.js", "/app.css", "/pwa.js", "/manifest.webmanifest", "/icon-192.png", "/icon-512.png", "/apple-touch-icon.png"}

func publicShellPath(path string) bool {
	for _, p := range shellPaths {
		if p == path {
			return true
		}
	}
	return false
}

func serveWorker(w http.ResponseWriter, r *http.Request) {
	// A shell edit changes the worker bytes automatically, triggering the browser
	// update lifecycle and retiring only our own old shell cache on activation.
	h := sha256.New()
	for _, path := range append(append([]string{}, shellPaths...), "/sw.js") {
		name := strings.TrimPrefix(path, "/")
		if name == "" {
			name = "index.html"
		}
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			fail(w, 500, "application shell unavailable")
			return
		}
		h.Write([]byte(name))
		h.Write(data)
	}
	script, _ := assets.ReadFile("assets/sw.js")
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, strings.ReplaceAll(string(script), "__ASSET_VERSION__", fmt.Sprintf("%x", h.Sum(nil))[:16]))
}
