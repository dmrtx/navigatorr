package importguard

import (
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
	"transcode/optimization":   true,
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
// module, keyed and valued by path relative to the module root.
func moduleImports(t *testing.T) map[string][]string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", module+"/...").Output()
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

// TestWorkerLinksOnlySharedPackages fails when the worker binary gains a
// dependency on a server package, directly or transitively.
func TestWorkerLinksOnlySharedPackages(t *testing.T) {
	graph := moduleImports(t)
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
	graph := moduleImports(t)
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
