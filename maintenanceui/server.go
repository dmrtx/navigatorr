// Package maintenanceui is a browser adapter over Navigatorr's existing actions
// and MCP handlers. It owns no encoding or library-replacement policy.
package maintenanceui

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

//go:embed assets/*
var assets embed.FS

var allowedTools = map[string]bool{
	"action_run": true, "action_catalog": true, "action_list": true, "action_status": true, "action_detail": true, "action_resume": true, "action_retry": true, "action_cancel": true,
	"recipe_list": true, "recipe_get": true, "recipe_save": true, "recipe_delete": true, "recipe_history": true, "recipe_status": true, "recipe_reload": true, "recipe_update": true, "recipe_rollback": true,
	"inspect_media": true, "fs_list": true, "fs_stat": true, "fs_hash": true, "transcode_backups": true,
}
var allowedActions = map[string]bool{"transcode_media": true, "transcode_batch": true, "benchmark_transcode": true, "promote_transcode_candidate": true}

type Server struct {
	cfg            *config.Config
	registry       *arrservice.Registry
	engine         *action.Engine
	mcp            *server.MCPServer
	access         *cloudflareAccessVerifier
	tokenHash      [32]byte
	mu             sync.Mutex
	sessions       map[string]time.Time
	folderSizes    folderSizeCache
	folderListings folderListingCache
}

func New(cfg *config.Config, registry *arrservice.Registry, engine *action.Engine, mcpServer *server.MCPServer) (*Server, error) {
	if cfg == nil || registry == nil || engine == nil || mcpServer == nil {
		return nil, fmt.Errorf("maintenance UI requires the action engine")
	}
	if err := cfg.Web.ValidateAuth(); err != nil {
		return nil, err
	}
	if cfg.Web.AuthModeValue() == "cloudflare_access" {
		issuer, _ := cfg.Web.CloudflareIssuer()
		return &Server{cfg: cfg, registry: registry, engine: engine, mcp: mcpServer, access: newCloudflareVerifier(issuer, strings.TrimSpace(cfg.Web.CloudflareAccess.Audience))}, nil
	}
	token := strings.TrimSpace(cfg.Web.Token)
	if cfg.Web.TokenFile != "" {
		if token != "" {
			return nil, fmt.Errorf("configure only one of web.token and web.token_file")
		}
		b, err := os.ReadFile(cfg.Web.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read web token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if len(token) < 24 {
		return nil, fmt.Errorf("web token must contain at least 24 characters")
	}
	return &Server{cfg: cfg, registry: registry, engine: engine, mcp: mcpServer, tokenHash: sha256.Sum256([]byte(token)), sessions: map[string]time.Time{}}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/maintenance/auth-info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"auth_mode": s.cfg.Web.AuthModeValue()})
	})
	mux.HandleFunc("POST /api/maintenance/login", s.login)
	mux.HandleFunc("POST /api/maintenance/logout", func(w http.ResponseWriter, r *http.Request) {
		if s.access != nil {
			fail(w, 405, "sign out through Cloudflare Access")
			return
		}
		if c, err := r.Cookie("navigatorr_session"); err == nil {
			s.mu.Lock()
			delete(s.sessions, c.Value)
			s.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: "navigatorr_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/maintenance/bootstrap", s.bootstrap)
	mux.HandleFunc("GET /api/maintenance/library", s.library)
	mux.HandleFunc("GET /api/maintenance/operations", s.operations)
	mux.HandleFunc("POST /api/maintenance/archive", s.archive)
	mux.HandleFunc("GET /api/maintenance/batch-items", s.batchItems)
	mux.HandleFunc("GET /api/maintenance/batch-preview", s.batchPreview)
	mux.HandleFunc("POST /api/maintenance/batch-preview", s.batchPreview)
	mux.HandleFunc("GET /api/maintenance/batch-reconfigure", s.batchReconfigure)
	mux.HandleFunc("POST /api/maintenance/batch-reconfigure", s.batchReconfigure)
	mux.HandleFunc("GET /api/maintenance/folder", s.folder)
	mux.HandleFunc("GET /api/maintenance/workers", s.workers)
	mux.HandleFunc("GET /api/maintenance/worker-activity", s.workerActivity)
	mux.HandleFunc("POST /api/maintenance/tool", s.tool)
	mux.HandleFunc("GET /api/maintenance/logs", s.logs)
	mux.HandleFunc("GET /api/maintenance/benchmark-comparison", s.benchmarkComparison)
	mux.HandleFunc("GET /sw.js", serveWorker)
	files, _ := fs.Sub(assets, "assets")
	static := http.FileServer(http.FS(files))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", 405)
			return
		}
		if !publicShellPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/manifest.webmanifest" {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		static.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if r.Header.Get("X-Navigatorr-Request") != "1" {
					fail(w, 403, "missing same-origin request header")
					return
				}
				if origin := r.Header.Get("Origin"); origin != "" {
					u, err := url.Parse(origin)
					if err != nil || !strings.EqualFold(u.Host, r.Host) {
						fail(w, 403, "cross-origin request denied")
						return
					}
				}
			}
			if r.URL.Path != "/api/maintenance/auth-info" && r.URL.Path != "/api/maintenance/login" && !s.authorized(r) {
				fail(w, 401, "sign in to Navigatorr")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}
func (s *Server) authorized(r *http.Request) bool {
	if s.access != nil {
		values := r.Header.Values("Cf-Access-Jwt-Assertion")
		return len(values) == 1 && s.access.verify(r.Context(), values[0]) == nil
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		h := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
		return subtle.ConstantTimeCompare(h[:], s.tokenHash[:]) == 1
	}
	c, err := r.Cookie("navigatorr_session")
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.sessions[c.Value]
	if ok && time.Now().Before(expires) {
		return true
	}
	delete(s.sessions, c.Value)
	return false
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.access != nil {
		fail(w, 405, "sign in through Cloudflare Access")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decode(w, r, &body); err != nil {
		fail(w, 400, "invalid login request")
		return
	}
	h := sha256.Sum256([]byte(body.Token))
	if subtle.ConstantTimeCompare(h[:], s.tokenHash[:]) != 1 {
		fail(w, 401, "incorrect access token")
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		fail(w, 500, "unable to create session")
		return
	}
	id := hex.EncodeToString(b)
	s.mu.Lock()
	for k, expires := range s.sessions {
		if time.Now().After(expires) {
			delete(s.sessions, k)
		}
	}
	if len(s.sessions) >= 128 {
		s.mu.Unlock()
		fail(w, 429, "session limit reached; sign out another browser")
		return
	}
	s.sessions[id] = time.Now().Add(12 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "navigatorr_session", Value: id, Path: "/", HttpOnly: true, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	services := []map[string]string{}
	for _, name := range s.registry.List() {
		if name == "sonarr" || name == "radarr" {
			kind := "series"
			if name == "radarr" {
				kind = "movie"
			}
			services = append(services, map[string]string{"name": name, "kind": kind})
		}
	}
	toolNames := []string{}
	for name := range allowedTools {
		if s.mcp.GetTool(name) != nil {
			toolNames = append(toolNames, name)
		}
	}
	sort.Strings(toolNames)
	definitions := []mcp.Tool{}
	for _, name := range toolNames {
		definitions = append(definitions, s.mcp.GetTool(name).Tool)
	}
	writeJSON(w, 200, map[string]any{"services": services, "roots": s.cfg.Media.AllowedReadRoots, "tools": toolNames, "tool_definitions": definitions, "allow_destructive": s.cfg.AllowDestructive, "transcode_enabled": s.cfg.Transcode.Enabled, "asset_kinds": []string{"video"}})
}
func (s *Server) tool(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := decode(w, r, &body); err != nil {
		fail(w, 400, "invalid tool request: "+err.Error())
		return
	}
	if !allowedTools[body.Name] {
		fail(w, 403, "tool is not available on the maintenance surface")
		return
	}
	if body.Arguments == nil {
		body.Arguments = map[string]any{}
	}
	if body.Name == "action_run" {
		name, _ := body.Arguments["action"].(string)
		if !allowedActions[name] {
			fail(w, 400, "unsupported maintenance action")
			return
		}
		inputs := map[string]any{}
		if value, present := body.Arguments["inputs"]; present {
			raw, ok := value.(string)
			if !ok || raw != "" && (json.Unmarshal([]byte(raw), &inputs) != nil || inputs == nil) {
				fail(w, 400, "inputs must be a JSON object string")
				return
			}
		}
		for _, k := range []string{"service", "media_id", "hash", "url", "path", "objective"} {
			if v, ok := body.Arguments[k].(string); ok && v != "" && inputs[k] == nil {
				inputs[k] = v
			}
		}
		key, _ := body.Arguments["idempotency_key"].(string)
		if strings.TrimSpace(key) == "" {
			key, _ = inputs["idempotency_key"].(string)
		}
		if strings.TrimSpace(key) == "" {
			b := make([]byte, 16)
			if _, err := rand.Read(b); err != nil {
				fail(w, 500, "unable to create submission receipt")
				return
			}
			key = hex.EncodeToString(b)
		}
		if action.RequiresWorker(name) {
			if err := s.engine.CheckWorkerAdmission(r.Context()); err != nil {
				fail(w, http.StatusConflict, err.Error())
				return
			}
		}
		res, err := s.engine.Enqueue(action.WithOrigin(r.Context(), "web"), name, inputs, key)
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		writeJSON(w, 202, map[string]any{"id": res.ID, "status": res.Status, "action_name": res.ActionName})
		return
	}
	if body.Name == "action_resume" || body.Name == "action_retry" || body.Name == "action_cancel" {
		id, _ := body.Arguments["id"].(string)
		inst, err := s.engine.Deps().Store.GetActionInstance(id)
		if err != nil || inst == nil || !allowedActions[inst.ActionName] {
			fail(w, 403, "action is not available on the maintenance surface")
			return
		}
	}
	t := s.mcp.GetTool(body.Name)
	if t == nil {
		fail(w, 404, "tool is not configured")
		return
	}
	res, err := t.Handler(r.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: body.Name, Arguments: body.Arguments}})
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, res)
}
