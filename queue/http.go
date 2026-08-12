package queue

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
)

// maxBodyBytes caps a request body. Comfortably above MaxTextLen so an
// oversized text gets a clear error rather than a truncated-JSON one.
const maxBodyBytes = 64 << 10

// Server exposes the queue over HTTP so non-MCP clients (phone shortcuts, webhooks,
// curl, the iMessage bridge) can submit requests for an agent to work later.
type Server struct {
	store *Store
	token string
}

// NewServer returns a server that requires the given bearer token.
//
// An empty token is rejected rather than treated as "no auth". What lands in
// this queue is free-form text that an agent later reads and acts on while
// holding write credentials to every configured *arr service and torrent
// client, so an open endpoint is a way to drive that agent, not just a way to
// add spam.
func NewServer(store *Store, token string) (*Server, error) {
	if token == "" {
		return nil, errors.New("queue.token is required to serve the HTTP endpoint")
	}
	return &Server{store: store, token: token}, nil
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/request", s.handleRequest)
	mux.HandleFunc("/queue", s.handleQueue)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return s.authenticated(mux)
}

// bearerToken pulls the credentials out of an Authorization header. RFC 7235
// makes the scheme case-insensitive and allows extra whitespace, which a shell
// script or shortcut app will produce sooner or later.
func bearerToken(h string) (string, bool) {
	parts := strings.Fields(h)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return parts[1], true
}

// authenticated enforces the bearer token on everything except /healthz.
func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			got, ok := bearerToken(r.Header.Get("Authorization"))
			// Constant-time, and only after the scheme parsed, so a malformed
			// header cannot authenticate by accident.
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleRequest enqueues a new request.
//
//	POST /request  {"text": "boston legal", "source": "imessage"}
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	// Require a JSON content type. Without it this is a CORS "simple request",
	// which any page the user happens to visit can send to localhost without a
	// preflight.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeErr(w, http.StatusUnsupportedMediaType, "content-type must be application/json")
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var body struct {
		Text   string `json:"text"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body exceeds %d bytes", maxBodyBytes))
			return
		}
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return
	}

	it, err := s.store.Add(body.Text, body.Source)
	if err != nil {
		// Add rejects empty and oversized text; both are the caller's fault.
		if strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "limit is") {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, it)
}

// handleQueue lists requests, optionally filtered by ?status=.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && !ValidStatus(status) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("status must be one of %s", strings.Join(Statuses, ", ")))
		return
	}
	writeJSON(w, http.StatusOK, s.store.List(status))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
