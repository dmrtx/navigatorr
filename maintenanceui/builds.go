package maintenanceui

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/jakenesler/navigatorr/nodes"
)

// Legacy builds API. The routes and their bodies live in package nodes
// (nodes.LegacyHandler, documented at nodes.LegacyBasePath); this file only
// decides who may call them: the builds token (web.builds_token_file, a second
// bearer that may call only these routes), or a signed-in session.

const buildsPath = nodes.LegacyBasePath

// isBuildsPath is whether a request path is one of the build routes, and only
// those: the builds token is accepted nowhere else.
func isBuildsPath(p string) bool {
	if p == buildsPath {
		return true
	}
	rest, ok := strings.CutPrefix(p, buildsPath+"/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || len(parts) > 2 || !validBuildID(parts[0]) {
		return false
	}
	return len(parts) == 1 || parts[1] == "heartbeat" || parts[1] == "finish" || parts[1] == "log"
}

func validBuildID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// buildsTokenCall is whether the request carries the builds token on a build
// route. The token is a bearer, not an ambient cookie, so such a call needs no
// same-origin header.
func (s *Server) buildsTokenCall(r *http.Request) bool {
	if !s.hasBuildsToken || !isBuildsPath(r.URL.Path) {
		return false
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	h := sha256.Sum256([]byte(strings.TrimPrefix(auth, "Bearer ")))
	return subtle.ConstantTimeCompare(h[:], s.buildsTokenHash[:]) == 1
}

// legacyBuilds serves every legacy builds route (auth was checked by Handler's
// wrapper). The routes themselves live in package nodes (LegacyHandler).
func (s *Server) legacyBuilds(w http.ResponseWriter, r *http.Request) {
	if s.nodes == nil || s.legacy == nil {
		fail(w, 503, "builds are not available")
		return
	}
	s.legacy.ServeHTTP(w, r)
}

// SetNodes gives the server the build registry (package nodes). Without it the
// builds routes answer 503.
func (s *Server) SetNodes(n *nodes.Service) {
	s.nodes, s.legacy = n, nil
	if n != nil {
		s.legacy = n.LegacyHandler()
	}
}

// nodesAPI serves /nodes/v1 (auth was checked by Handler's wrapper).
func (s *Server) nodesAPI(w http.ResponseWriter, r *http.Request) {
	if s.nodes == nil {
		writeJSON(w, 503, map[string]string{"error": "nodes is not available", "code": "unavailable"})
		return
	}
	s.nodes.Handler().ServeHTTP(w, r)
}

// nodesAuthorized is whether a /nodes/v1 call may proceed: the builds token as
// X-Api-Key (what call_api sends) or as a bearer, or a signed-in session (a
// change through a session also needs the same-origin header, as everywhere).
func (s *Server) nodesAuthorized(r *http.Request) bool {
	if s.hasBuildsToken {
		tok := r.Header.Get("X-Api-Key")
		if auth := r.Header.Get("Authorization"); tok == "" && strings.HasPrefix(auth, "Bearer ") {
			tok = strings.TrimPrefix(auth, "Bearer ")
		}
		if tok != "" {
			h := sha256.Sum256([]byte(tok))
			return subtle.ConstantTimeCompare(h[:], s.buildsTokenHash[:]) == 1
		}
	}
	if !s.authorized(r) {
		return false
	}
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Header.Get("X-Navigatorr-Request") == "1"
}
