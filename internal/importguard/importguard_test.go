package importguard

import (
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

const module = "github.com/jakenesler/navigatorr"

// workerAllowed lists every module package the worker binary may link.
// The worker runs on a separate machine with its own release cadence, so
// adding a package here is a deliberate decision: shared packages must stay
// free of server code (config, action, store, the HTTP client, ...).
var workerAllowed = map[string]bool{
	"cmd/navigatorr-transcode": true,
	"internal/transcodeworker": true,
	"internal/smbdirect":       true,
	"transcode":                true,
	"transcode/quality":        true,
	"transcode/resilience":     true,
	"podcast":                  true,
	"podcast/acoustic":         true,
	"mediainspect":             true,
	"mediafile":                true,
}

// workerOnly may be imported only by the worker binary itself.
var workerOnly = map[string]bool{
	"internal/transcodeworker": true,
}

// moduleImports returns each module package's non-test imports within the
// module, keyed and valued by path relative to the module root, as resolved
// for goos/goarch (build-tagged files change the import set).
func moduleImports(t *testing.T, goos, goarch string) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", module+"/...")
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	graph := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pkg := rel(fields[0])
		graph[pkg] = nil
		for _, imp := range fields[1:] {
			if imp == module || strings.HasPrefix(imp, module+"/") {
				graph[pkg] = append(graph[pkg], rel(imp))
			}
		}
	}
	return graph
}

func rel(path string) string {
	if path == module {
		return "."
	}
	return strings.TrimPrefix(path, module+"/")
}

// platforms are the targets both checks resolve imports for: the worker
// ships on Apple Silicon, the server in a Linux container.
var platforms = [][2]string{{"darwin", "arm64"}, {"linux", "amd64"}}

// TestWorkerLinksOnlySharedPackages fails when the worker binary gains a
// dependency on a server package, directly or transitively.
func TestWorkerLinksOnlySharedPackages(t *testing.T) {
	for _, p := range platforms {
		t.Run(p[0]+"/"+p[1], func(t *testing.T) { checkWorkerLinks(t, moduleImports(t, p[0], p[1])) })
	}
}

func checkWorkerLinks(t *testing.T, graph map[string][]string) {
	root := "cmd/navigatorr-transcode"
	if _, ok := graph[root]; !ok {
		t.Fatalf("%s not found in go list output", root)
	}
	// Breadth-first walk that remembers how each package was reached, so a
	// failure names the import chain to cut.
	via := map[string]string{root: ""}
	queue := []string{root}
	var bad []string
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		if !workerAllowed[pkg] {
			bad = append(bad, chain(via, pkg))
		}
		for _, imp := range graph[pkg] {
			if _, seen := via[imp]; !seen {
				via[imp] = pkg
				queue = append(queue, imp)
			}
		}
	}
	sort.Strings(bad)
	for _, c := range bad {
		t.Errorf("worker binary links a non-shared package: %s", c)
	}
}

// TestServerDoesNotImportWorkerInternals fails when any package other than
// the worker binary imports the worker's internal packages.
func TestServerDoesNotImportWorkerInternals(t *testing.T) {
	for _, p := range platforms {
		t.Run(p[0]+"/"+p[1], func(t *testing.T) { checkWorkerInternals(t, moduleImports(t, p[0], p[1])) })
	}
}

func checkWorkerInternals(t *testing.T, graph map[string][]string) {
	for pkg, imports := range graph {
		if pkg == "cmd/navigatorr-transcode" || workerOnly[pkg] {
			continue
		}
		for _, imp := range imports {
			if workerOnly[imp] {
				t.Errorf("%s imports worker-only package %s", pkg, imp)
			}
		}
	}
}

func chain(via map[string]string, pkg string) string {
	parts := []string{pkg}
	for p := via[pkg]; p != ""; p = via[p] {
		parts = append([]string{p}, parts...)
	}
	return strings.Join(parts, " -> ")
}

// TestNodesAndTheEngineStayApart guards invariant N2: the build registry
// (nodes) and the transcode engine (action, transcode, and the transcodeapi contract) never import each other,
// directly or transitively. Only main and internal/nodeswire join them.
func TestNodesAndTheEngineStayApart(t *testing.T) {
	for _, p := range platforms {
		t.Run(p[0]+"/"+p[1], func(t *testing.T) {
			graph := moduleImports(t, p[0], p[1])
			reach := func(root string, banned func(string) bool) {
				// A package missing from the graph (renamed, moved, or the go
				// list output changed) must fail the test, not pass it empty.
				if _, ok := graph[root]; !ok {
					t.Fatalf("root package %q is not in the import graph (%d packages): the guard would check nothing", root, len(graph))
				}
				via := map[string]string{root: ""}
				queue := []string{root}
				for len(queue) > 0 {
					pkg := queue[0]
					queue = queue[1:]
					if pkg != root && banned(pkg) {
						t.Errorf("%s must not reach %s: %s", root, pkg, chain(via, pkg))
					}
					for _, imp := range graph[pkg] {
						if _, seen := via[imp]; !seen {
							via[imp] = pkg
							queue = append(queue, imp)
						}
					}
				}
			}
			reach("nodes", func(p string) bool {
				return p == "action" || p == "maintenanceui" || p == "internal/nodeswire" || p == "transcode" || p == "transcodeapi" || strings.HasPrefix(p, "transcode/")
			})
			// The contract package is self-contained: it imports nothing of the
			// module, so it can never carry Nodes (or anything else) into the engine.
			if imps, ok := graph["transcodeapi"]; !ok {
				t.Fatal("transcodeapi is not in the import graph: the guard would check nothing")
			} else if len(imps) > 0 {
				t.Errorf("transcodeapi must import nothing of the module, it imports %v", imps)
			}
			for _, engine := range []string{"action", "transcode", "transcodeapi"} {
				reach(engine, func(p string) bool { return p == "nodes" || p == "internal/nodeswire" })
			}
		})
	}
}

// bannedPrefixes reports whether pkg is one of the named packages or lives
// below one of them.
func bannedPrefixes(pkg string, names ...string) bool {
	for _, b := range names {
		if pkg == b || strings.HasPrefix(pkg, b+"/") {
			return true
		}
	}
	return false
}

// checkStatelessRoot fails when root reaches a banned package, directly or
// transitively, as resolved for every platform.
//
// config is a known shared dependency of arrservice and openapi, and it
// still imports the transcode and podcast packages for its settings types.
// Only those edges (config -> transcode*, config -> podcast*) are exempt
// until config is split; every other import of config, and every other
// package root reaches, is still checked. A second walk starts below the
// exempt edges, so whatever transcode* and podcast* pull in is checked too:
// only transcode* and podcast* themselves are tolerated there.
func checkStatelessRoot(t *testing.T, root string, banned func(string) bool) {
	t.Helper()
	configExempt := func(p string) bool {
		return p == "transcode" || strings.HasPrefix(p, "transcode/") || p == "podcast" || strings.HasPrefix(p, "podcast/")
	}
	for _, p := range platforms {
		t.Run(p[0]+"/"+p[1], func(t *testing.T) {
			graph := moduleImports(t, p[0], p[1])
			// A package missing from the graph must fail the test, not pass it empty.
			if _, ok := graph[root]; !ok {
				t.Fatalf("root package %q is not in the import graph (%d packages): the guard would check nothing", root, len(graph))
			}
			// First walk: everything root reaches without the exempt edges.
			via := map[string]string{root: ""}
			queue := []string{root}
			var exempt []string
			for len(queue) > 0 {
				pkg := queue[0]
				queue = queue[1:]
				if pkg != root && banned(pkg) {
					t.Errorf("%s must not reach %s: %s", root, pkg, chain(via, pkg))
				}
				for _, imp := range graph[pkg] {
					if pkg == "config" && configExempt(imp) {
						exempt = append(exempt, imp)
						continue
					}
					if _, seen := via[imp]; !seen {
						via[imp] = pkg
						queue = append(queue, imp)
					}
				}
			}
			// Second walk: below the exempt edges, only transcode* and
			// podcast* are tolerated; any other banned package still fails.
			seen := map[string]string{}
			for _, e := range exempt {
				if _, ok := seen[e]; !ok {
					seen[e] = "config"
					queue = append(queue, e)
				}
			}
			for len(queue) > 0 {
				pkg := queue[0]
				queue = queue[1:]
				if banned(pkg) && !configExempt(pkg) {
					t.Errorf("%s must not reach %s through config's exempt edges: %s", root, pkg, chain(seen, pkg))
				}
				for _, imp := range graph[pkg] {
					if _, ok := seen[imp]; !ok {
						seen[imp] = pkg
						queue = append(queue, imp)
					}
				}
			}
		})
	}
}

// TestArrtoolsStaysStateless guards the stateless MCP tools: arrtools must be
// importable by a separate gateway binary without pulling in the action
// engine, the maintenance store, the UI, the nodes registry, the transcode
// stack, the podcast pipeline, the queue or the stateful tools package.
func TestArrtoolsStaysStateless(t *testing.T) {
	checkStatelessRoot(t, "arrtools", func(p string) bool {
		return bannedPrefixes(p, "action", "store", "maintenanceui", "nodes", "internal/nodeswire", "podcast", "queue", "tools", "transcode", "transcodeapi")
	})
}

// TestGatewayStaysStateless guards the gateway binary: it must never link
// the action engine, the store, the web UI, nodes, anything transcode*, the
// podcast pipeline, the queue or the stateful tools package, or it would no
// longer be a stateless front for them. The same config exemption as for
// arrtools applies (see checkStatelessRoot).
func TestGatewayStaysStateless(t *testing.T) {
	checkStatelessRoot(t, "cmd/navigatorr-gateway", func(p string) bool {
		return bannedPrefixes(p, "action", "store", "maintenanceui", "nodes", "internal/nodeswire", "internal/transcodeworker", "podcast", "queue", "tools") ||
			strings.HasPrefix(p, "transcode")
	})
}
