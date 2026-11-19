package nodes

import (
	"context"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// N2 (source level; internal/importguard checks the same through go list):
// nothing in this package imports action, the transcode engine or maintenanceui.
func TestN2NodesImportsNoEngine(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			for _, bad := range []string{"/action", "/transcode", "/maintenanceui", "/internal/nodeswire"} {
				if strings.HasSuffix(p, bad) || strings.Contains(p, bad+"/") {
					t.Errorf("%s imports %s", f, p)
				}
			}
		}
	}
}

// N3: Nodes works with no view of the transcode engine at all (and a worker
// that cannot reach Nodes simply carries on; here, the registry needs nothing from it).
func TestN3RegistryNeedsNoTranscodeView(t *testing.T) {
	s, clock := buildSvc(t)
	b := startBuild(t, s, "nodeA", "graceful", 0)
	clock.advance(time.Minute)
	if _, err := s.HeartbeatBuild(context.Background(), b.ID, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Node(context.Background(), "nodeA"); err != nil {
		t.Fatal(err)
	}
}

// N1: a running lease is decided only by the worker. Whatever the transcode
// side says (paused, held), the registry never changes a build because of it,
// and it never reports holds itself.
func TestN1RegistryNeverDecidesALease(t *testing.T) {
	s, clock := buildSvc(t)
	s.tv = fakeTranscode{"nodeA": {Paused: true, Mode: "graceful", HeldBy: "build"}}
	b := startBuild(t, s, "nodeA", "graceful", 0)
	clock.advance(time.Second)
	tick(s)
	got, err := s.GetBuild(b.ID)
	if err != nil || got.Status != "running" {
		t.Fatal("a transcode pause or hold does not end a build", got, err)
	}
	v, err := s.Node(context.Background(), "nodeA")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.RunningBuilds) != 1 {
		t.Fatal("the record is listed", v)
	}
}
