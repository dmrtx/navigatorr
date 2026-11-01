package maintenanceui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// writeJSONTagged answers like writeJSON, and names the body with a weak ETag
// (a hash of exactly what would be sent). A client that sends the tag back in
// If-None-Match gets a 304 with no body when nothing in it changed, so a list
// it already holds costs a request and a few hundred bytes. The tag covers the
// whole body, marks and sizes included, so a 304 never hides a change.
// Responses stay Cache-Control: no-store: the client keeps the tag and the body
// itself and decides when to ask.
func writeJSONTagged(w http.ResponseWriter, r *http.Request, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		fail(w, 500, "could not encode the answer")
		return
	}
	sum := sha256.Sum256(body)
	tag := `W/"` + hex.EncodeToString(sum[:12]) + `"`
	w.Header().Set("ETag", tag)
	if code == http.StatusOK && etagMatches(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
}

// etagMatches is the weak comparison of If-None-Match: any listed tag, or "*".
func etagMatches(header, tag string) bool {
	weak := func(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "W/") }
	for _, candidate := range strings.Split(header, ",") {
		if c := strings.TrimSpace(candidate); c == "*" || (c != "" && weak(c) == weak(tag)) {
			return true
		}
	}
	return false
}
