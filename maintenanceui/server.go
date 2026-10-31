// Package maintenanceui is a browser adapter over Navigatorr's existing actions
// and MCP handlers. It owns no encoding or library-replacement policy.
package maintenanceui

import (
	"context"
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
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

//go:embed assets/*
var assets embed.FS

var allowedTools = map[string]bool{
	"action_run": true, "action_catalog": true, "action_list": true, "action_status": true, "action_detail": true, "action_resume": true, "action_retry": true, "action_cancel": true,
	"recipe_list": true, "recipe_get": true, "recipe_save": true, "recipe_delete": true, "recipe_history": true, "recipe_status": true, "recipe_reload": true, "recipe_update": true, "recipe_rollback": true,
	"podcast_ad_library": true, "podcast_artifact": true, "podcast_blocks": true, "podcast_block": true, "podcast_classify": true, "podcast_review": true,
	"inspect_media": true, "fs_list": true, "fs_stat": true, "fs_hash": true, "transcode_backups": true,
}

// benchmark_transcode stays listed so an old sample test can still be
// cancelled or opened; a new one is refused by the engine.
var allowedActions = map[string]bool{"clean_podcast_ads": true, "transcode_media": true, "transcode_batch": true, "benchmark_transcode": true, "promote_transcode_candidate": true}

type Server struct {
	cfg         *config.Config
	registry    *arrservice.Registry
	engine      *action.Engine
	mcp         *server.MCPServer
	access      *cloudflareAccessVerifier
	accessOnly  bool
	tokenHash   [32]byte
	mu          sync.Mutex
	sessions    map[string]time.Time
	folderSizes folderSizeCache
	storage     storageState
	// nas replaces the configured NAS helper in tests.
	nas             nasMeter
	folderListings  folderListingCache
	fileStatusCache fileStatusCache
	// probe and classify replace ffprobe in tests; nil uses the real ones.
	probe    func(ctx context.Context, ffprobe, path string) (mediainspect.DetailedReport, error)
	classify action.SourceClassifier
	// slotSync is the last slot push to each worker, by worker name.
	slotSync struct {
		mu    sync.Mutex
		nodes map[string]*slotSyncState
	}
}

type slotSyncState struct {
	last time.Time
	err  string
}

func New(cfg *config.Config, registry *arrservice.Registry, engine *action.Engine, mcpServer *server.MCPServer) (*Server, error) {
	if cfg == nil || registry == nil || engine == nil || mcpServer == nil {
		return nil, fmt.Errorf("maintenance UI requires the action engine")
	}
	if err := cfg.Web.ValidateAuth(); err != nil {
		return nil, err
	}
	var access *cloudflareAccessVerifier
	if cfg.Web.AuthModeValue() == "cloudflare_access" || cfg.Web.CloudflareAccessConfigured() {
		issuer, _ := cfg.Web.CloudflareIssuer()
		access = newCloudflareVerifier(issuer, strings.TrimSpace(cfg.Web.CloudflareAccess.Audience))
	}
	if cfg.Web.AuthModeValue() == "cloudflare_access" {
		return &Server{cfg: cfg, registry: registry, engine: engine, mcp: mcpServer, access: access, accessOnly: true}, nil
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
	return &Server{cfg: cfg, registry: registry, engine: engine, mcp: mcpServer, access: access, tokenHash: sha256.Sum256([]byte(token)), sessions: map[string]time.Time{}}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/maintenance/auth-info", func(w http.ResponseWriter, r *http.Request) {
		info := map[string]string{"auth_mode": s.cfg.Web.AuthModeValue()}
		if !s.accessOnly && s.accessAuthorized(r) {
			// The browser arrived through Access; it has no local session to sign out of.
			info["session"] = "cloudflare_access"
		}
		writeJSON(w, 200, info)
	})
	mux.HandleFunc("POST /api/maintenance/login", s.login)
	mux.HandleFunc("POST /api/maintenance/logout", func(w http.ResponseWriter, r *http.Request) {
		if s.accessOnly {
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
	mux.HandleFunc("GET /api/maintenance/commands", s.commandStatus)
	mux.HandleFunc("GET /api/maintenance/batch-candidates", s.batchCandidates)
	mux.HandleFunc("POST /api/maintenance/batch-candidates", s.batchCandidates)
	mux.HandleFunc("GET /api/maintenance/batch-settings", s.batchSettings)
	mux.HandleFunc("POST /api/maintenance/batch-settings", s.batchSettings)
	mux.HandleFunc("GET /api/maintenance/bootstrap", s.bootstrap)
	mux.HandleFunc("GET /api/maintenance/library", s.library)
	mux.HandleFunc("GET /api/maintenance/operations", s.operations)
	mux.HandleFunc("POST /api/maintenance/archive", s.archive)
	mux.HandleFunc("GET /api/maintenance/job-leftovers", s.jobLeftovers)
	mux.HandleFunc("POST /api/maintenance/job-leftovers", s.jobLeftovers)
	mux.HandleFunc("GET /api/maintenance/batch-items", s.batchItems)
	mux.HandleFunc("GET /api/maintenance/batch-preview", s.batchPreview)
	mux.HandleFunc("POST /api/maintenance/batch-preview", s.batchPreview)
	mux.HandleFunc("GET /api/maintenance/batch-reconfigure", s.batchReconfigure)
	mux.HandleFunc("POST /api/maintenance/batch-reconfigure", s.batchReconfigure)
	mux.HandleFunc("POST /api/maintenance/candidate-decision", s.candidateDecision)
	mux.HandleFunc("POST /api/maintenance/rerun", s.rerun)
	mux.HandleFunc("GET /api/maintenance/folder", s.folder)
	mux.HandleFunc("GET /api/maintenance/storage", s.storageStats)
	mux.HandleFunc("POST /api/maintenance/storage", s.storageStats)
	mux.HandleFunc("GET /api/maintenance/source-profile", s.sourceProfile)
	mux.HandleFunc("GET /api/maintenance/source-tracks", s.sourceTracks)
	mux.HandleFunc("GET /api/maintenance/podcasts", s.podcasts)
	mux.HandleFunc("GET /api/maintenance/podcast-local-library", s.podcastLocalLibrary)
	mux.HandleFunc("GET /api/maintenance/workers", s.workers)
	mux.HandleFunc("GET /api/maintenance/worker-activity", s.workerActivity)
	mux.HandleFunc("GET /api/maintenance/worker-drain", s.workerDrain)
	mux.HandleFunc("POST /api/maintenance/worker-drain", s.workerDrain)
	mux.HandleFunc("GET /api/maintenance/worker-slots", s.workerSlots)
	mux.HandleFunc("POST /api/maintenance/worker-slots", s.workerSlots)
	mux.HandleFunc("GET /api/maintenance/compression", s.compression)
	mux.HandleFunc("GET /api/maintenance/keep-languages", s.keepLanguages)
	mux.HandleFunc("PUT /api/maintenance/keep-languages", s.keepLanguages)
	mux.HandleFunc("POST /api/maintenance/tool", s.tool)
	mux.HandleFunc("GET /api/maintenance/logs", s.logs)
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
		// The shell is public and embedded. Ask the server on every launch, but
		// let it answer "unchanged" instead of sending half a megabyte again;
		// the file server turns a matching If-None-Match into a 304.
		if tag := shellTag(r.URL.Path); tag != "" {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
		}
		static.ServeHTTP(w, r)
	})
	return gzipResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// failAdmission answers a refused submission. A request the caller can fix by
// changing one input is a 400 that names the input; nothing was written, so
// the same submission key can be used again. Anything else keeps the given
// status.
func failAdmission(w http.ResponseWriter, code int, err error) {
	if invalid, ok := action.AsInvalidParams(err); ok {
		body := map[string]string{"error": invalid.Reason, "code": "invalid_params"}
		if invalid.Field != "" {
			body["field"] = invalid.Field
		}
		writeJSON(w, http.StatusBadRequest, body)
		return
	}
	fail(w, code, err.Error())
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

// accessAuthorized accepts only an assertion whose signature, issuer, audience
// and lifetime verify; the header's presence alone proves nothing.
func (s *Server) accessAuthorized(r *http.Request) bool {
	if s.access == nil {
		return false
	}
	values := r.Header.Values("Cf-Access-Jwt-Assertion")
	return len(values) == 1 && s.access.verify(r.Context(), values[0]) == nil
}

func (s *Server) authorized(r *http.Request) bool {
	if s.accessAuthorized(r) {
		return true
	}
	if s.accessOnly {
		return false
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
	if s.accessOnly {
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
	minimumSavings := s.cfg.Transcode.MinSavingsPercent
	if minimumSavings <= 0 {
		minimumSavings = action.DefaultMinSavingsPercent
	}
	writeJSON(w, 200, map[string]any{"services": services, "roots": s.cfg.Media.AllowedReadRoots, "tools": toolNames, "tool_definitions": definitions, "allow_destructive": s.cfg.AllowDestructive, "transcode_enabled": s.cfg.Transcode.Enabled, "asset_kinds": []string{"video"}, "min_savings_percent": minimumSavings})
}
func (s *Server) tool(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string         `json:"name"`
		Background bool           `json:"background"`
		Key        string         `json:"key"`
		Arguments  map[string]any `json:"arguments"`
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
			key = body.Key
		}
		if strings.TrimSpace(key) == "" {
			b := make([]byte, 16)
			if _, err := rand.Read(b); err != nil {
				fail(w, 500, "unable to create submission receipt")
				return
			}
			key = hex.EncodeToString(b)
		}
		// A request the caller got wrong is reported before anything is asked
		// of the worker.
		if err := s.engine.ValidateInputs(r.Context(), name, inputs, key); err != nil {
			failAdmission(w, 409, err)
			return
		}
		if action.RequiresWorker(name) && !body.Background {
			if err := s.engine.CheckWorkerAdmissionFor(r.Context(), name); err != nil {
				fail(w, http.StatusConflict, err.Error())
				return
			}
		}
		res, err := s.engine.Enqueue(action.WithOrigin(r.Context(), "web"), name, inputs, key)
		if err != nil {
			failAdmission(w, 409, err)
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
	if body.Background {
		kind := map[string]string{"action_resume": "resume", "action_retry": "retry", "action_cancel": "cancel"}[body.Name]
		if body.Name == "transcode_backups" {
			mode, _ := body.Arguments["mode"].(string)
			if mode == "clean" || mode == "discard_duplicate" || mode == "discard" || mode == "restore" {
				kind = mode
				body.Arguments["id"] = body.Arguments["action_id"]
			}
		}
		if kind != "" {
			if body.Arguments["inputs"] != nil {
				fail(w, 400, "command inputs cannot be changed")
				return
			}
			inputs := map[string]any{"kind": kind}
			for _, key := range []string{"id", "decision", "reason", "plan_digest"} {
				if value, ok := body.Arguments[key]; ok {
					inputs[key] = value
				}
			}
			s.admitCommand(w, r, inputs, body.Key)
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
