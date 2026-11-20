// Package transcodeapi is the HTTP contract of the Transcode side of
// Navigatorr, version 1: its base path, its routes, its OpenAPI document and
// the shape of its errors. It holds no business logic. The handlers live in
// the web adapter (maintenanceui), which reuses the functions the maintenance
// routes and the MCP tools already call, and are mounted here by NewHandler so
// that exactly the routes of Routes() exist and no others.
//
// Phase 1a served reads; phase 1b adds the writes (POST, PUT and DELETE routes
// on the same resources). Both are listed in Routes().
//
// Invariant N2: Transcode never imports or consults Nodes. This package
// imports nothing of the module (internal/importguard checks it, and that
// maintenanceui's handlers for this API do not pull Nodes into it either).
package transcodeapi

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// BasePath is where the HTTP API lives.
const BasePath = "/transcode/v1"

// SpecPath is the OpenAPI document of the API. It is public (it describes the
// API and holds no data) so a service registry can fetch it without a key.
const SpecPath = BasePath + "/openapi.json"

//go:embed openapi.json
var specJSON []byte

// Spec is the OpenAPI 3 document of /transcode/v1.
func Spec() []byte { return specJSON }

// SpecHandler serves the document at SpecPath. It is public by design.
func SpecHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(specJSON)
	})
}

// Route is one operation of the API.
type Route struct {
	Method string
	Path   string // OpenAPI form, e.g. /transcode/v1/actions/{id}
	// NoHead marks a route that answers GET only, because reading it has an
	// effect a HEAD must not have. The 405 of any other method says
	// Allow: GET for it, not GET, HEAD.
	NoHead bool
}

// Key is the "METHOD /path" form Handlers is keyed by.
func (r Route) Key() string { return r.Method + " " + r.Path }

// Routes lists every operation of the API (the spec is checked against it).
// Order is the order the spec presents them in.
func Routes() []Route {
	g := func(p string) Route { return Route{Method: "GET", Path: BasePath + p} }
	w := func(method, p string) Route { return Route{Method: method, Path: BasePath + p} }
	return []Route{
		g("/workers"),
		g("/workers/{name}"),
		g("/workers/{name}/slots"),
		w("PUT", "/workers/{name}/slots"),
		g("/activity"),
		g("/state"),
		g("/drain"),
		w("PUT", "/drain"),
		g("/pauses"),
		g("/pauses/{name}"),
		w("PUT", "/pauses/{name}"),
		w("DELETE", "/pauses/{name}"),
		g("/windows"),
		g("/workers/{name}/window"),
		w("PUT", "/workers/{name}/window"),
		w("DELETE", "/workers/{name}/window"),
		g("/actions"),
		w("POST", "/actions"),
		g("/actions/{id}"),
		w("POST", "/actions/{id}/cancel"),
		w("POST", "/actions/{id}/resume"),
		w("POST", "/actions/{id}/retry"),
		g("/actions/{id}/detail/{section}"),
		g("/actions/{id}/podcast/blocks"),
		{Method: "GET", Path: BasePath + "/actions/{id}/podcast/blocks/{block_id}", NoHead: true},
		g("/actions/{id}/podcast/artifacts/{artifact}"),
		w("POST", "/actions/{id}/podcast/classify"),
		w("POST", "/actions/{id}/podcast/review"),
		g("/queue"),
		w("POST", "/queue/actions/{id}/hold"),
		w("POST", "/queue/actions/{id}/release"),
		w("POST", "/queue/actions/{id}/move"),
		w("POST", "/queue/items/{id}/hold"),
		w("POST", "/queue/items/{id}/release"),
		w("POST", "/queue/items/{id}/move"),
		w("POST", "/queue/batches/{id}/hold"),
		w("POST", "/queue/batches/{id}/release"),
		w("POST", "/queue/batches/{id}/move"),
		g("/history"),
		g("/catalog"),
		g("/batches/{id}/items"),
		g("/batches/{id}/settings"),
		g("/batches/{id}/candidates"),
		g("/batches/{id}/promotion-plan"),
		g("/batches/{id}/preview"),
		g("/batches/{id}/goal"),
		w("POST", "/batches/{id}/goal"),
		g("/backups"),
		g("/backups/candidates"),
		w("POST", "/backups/candidates/{id}/discard"),
		w("POST", "/backups/{id}/clean"),
		w("POST", "/backups/{id}/restore"),
		w("POST", "/backups/{id}/discard"),
		w("POST", "/backups/{id}/discard-duplicate"),
		g("/recipes"),
		g("/recipes/{name}"),
		w("PUT", "/recipes/{name}"),
		w("DELETE", "/recipes/{name}"),
		g("/recipes/{name}/history"),
		g("/recipe-bundle"),
		w("POST", "/recipe-bundle/reload"),
		w("POST", "/recipe-bundle/update"),
		w("POST", "/recipe-bundle/rollback"),
		g("/podcasts"),
		g("/podcasts/{id}/local-library"),
		g("/podcasts/{id}/ad-library"),
		w("DELETE", "/podcasts/{id}/ad-library/{entry}"),
		g("/review-queue"),
		w("POST", "/review-queue/{id}/close"),
		w("POST", "/review-queue/{id}/decisions"),
	}
}

// Handlers maps "METHOD /path" (Route.Key) to the function serving it.
type Handlers map[string]http.HandlerFunc

// NewHandler mounts handlers, which must be exactly the routes of Routes().
// Anything else under BasePath answers a JSON error: 405 (with Allow listing
// the methods that path serves) for a known path used with another method, 404
// otherwise. It does no authentication: the caller wraps it.
func NewHandler(handlers Handlers) (http.Handler, error) {
	want := map[string]bool{}
	paths := http.NewServeMux()
	methods := map[string][]string{} // path pattern -> methods its routes serve
	noHead := map[string]bool{}
	mux := http.NewServeMux()
	for _, r := range Routes() {
		want[r.Key()] = true
		fn, ok := handlers[r.Key()]
		if !ok || fn == nil {
			return nil, fmt.Errorf("transcodeapi: no handler for %s", r.Key())
		}
		if _, seen := methods[r.Path]; !seen {
			paths.HandleFunc(r.Path, func(http.ResponseWriter, *http.Request) {})
		}
		methods[r.Path] = append(methods[r.Path], r.Method)
		if r.NoHead && r.Method == http.MethodGet {
			noHead[r.Path] = true
			// A GET pattern also serves HEAD; refuse it here so the route's
			// handler never runs for a HEAD.
			get := fn
			fn = func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodHead {
					w.Header().Set("Allow", "GET")
					WriteError(w, 405, "method_not_allowed", "this endpoint answers GET only",
						"GET "+SpecPath+" lists the endpoints of this API")
					return
				}
				get(w, req)
			}
		}
		mux.HandleFunc(r.Key(), fn)
	}
	var extra []string
	for k := range handlers {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, fmt.Errorf("transcodeapi: handlers for routes that are not in Routes(): %v", extra)
	}
	allow := map[string]string{}
	for p, ms := range methods {
		allow[p] = AllowHeader(ms, noHead[p])
	}
	mux.HandleFunc(BasePath+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == SpecPath && r.Method != http.MethodGet && r.Method != http.MethodHead {
			// The spec is served by the caller's own GET route; any other
			// method lands here.
			w.Header().Set("Allow", "GET, HEAD")
			WriteError(w, 405, "method_not_allowed", "this endpoint is read-only: only GET is served",
				"GET "+SpecPath+" lists the endpoints of this API")
			return
		}
		if _, pattern := paths.Handler(r); pattern != "" {
			w.Header().Set("Allow", allow[pattern])
			WriteError(w, 405, "method_not_allowed", "this endpoint does not serve "+r.Method,
				"the Allow header lists the methods it serves; GET "+SpecPath+" lists the endpoints of this API")
			return
		}
		WriteError(w, 404, "not_found", "no such endpoint", "GET "+SpecPath+" lists the endpoints of this API")
	})
	return mux, nil
}

// AllowHeader is the Allow header of a path whose routes use methods: GET also
// serves HEAD (unless the route refuses it), then the writes in a fixed order.
func AllowHeader(methods []string, noHead bool) string {
	have := map[string]bool{}
	for _, m := range methods {
		have[m] = true
	}
	var out []string
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"} {
		switch {
		case m == "HEAD":
			if have["GET"] && !noHead {
				out = append(out, m)
			}
		case have[m]:
			out = append(out, m)
		}
	}
	return strings.Join(out, ", ")
}

// WriteJSON writes v as the JSON body of status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError answers {error, code, hint}: a message for a person, a stable
// code for a program and, when there is one, the endpoint to use instead.
func WriteError(w http.ResponseWriter, status int, code, msg, hint string) {
	body := map[string]string{"error": msg, "code": code}
	if hint != "" {
		body["hint"] = hint
	}
	WriteJSON(w, status, body)
}

// CodeForStatus is the error code of a status no endpoint named more
// precisely.
func CodeForStatus(status int) string {
	switch status {
	case 400:
		return "invalid_request"
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 413:
		return "payload_too_large"
	case 415:
		return "unsupported_media_type"
	case 422:
		return "rejected"
	case 405:
		return "method_not_allowed"
	case 409:
		return "conflict"
	case 503:
		return "unavailable"
	}
	if status >= 500 {
		return "internal"
	}
	return "error"
}
