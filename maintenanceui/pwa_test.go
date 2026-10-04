package maintenanceui

import (
	"bytes"
	"encoding/json"
	"image/png"
	"strings"
	"testing"
)

func TestPWAShellAndWorkerHeaders(t *testing.T) {
	_, h := testUI(t)
	var manifest struct {
		Display  string                                 `json:"display"`
		StartURL string                                 `json:"start_url"`
		Scope    string                                 `json:"scope"`
		Icons    []struct{ Src, Sizes, Purpose string } `json:"icons"`
	}
	w := request(h, "GET", "/manifest.webmanifest", "", false)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/manifest+json" || json.Unmarshal(w.Body.Bytes(), &manifest) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if manifest.Display != "standalone" || manifest.StartURL != "/" || manifest.Scope != "/" || len(manifest.Icons) != 2 {
		t.Fatal(manifest)
	}
	for _, icon := range manifest.Icons {
		w := request(h, "GET", icon.Src, "", false)
		im, err := png.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
		if w.Code != 200 || err != nil || im.Width != im.Height || (im.Width != 192 && im.Width != 512) {
			t.Fatal(icon, im, err)
		}
	}
	w = request(h, "GET", "/sw.js", "", false)
	if w.Code != 200 || w.Header().Get("Service-Worker-Allowed") != "/" || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "__ASSET_VERSION__") {
		t.Fatal("worker version/headers", w.Code, w.Body.String())
	}
	for _, path := range []string{"/pwa.js", "/apple-touch-icon.png", "/app.js", "/app.css", "/"} {
		if w := request(h, "GET", path, "", false); w.Code != 200 {
			t.Fatal(path, w.Code)
		}
	}
	for _, path := range []string{"/api/maintenance/operations", "/api/maintenance/bootstrap", "/assets/index.html", "/state.db"} {
		if w := request(h, "GET", path, "", false); w.Code == 200 {
			t.Fatal("public surface widened", path)
		}
	}
}
