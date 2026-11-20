package nodeswire

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/nodes"
)

type oneNode struct{}

func (oneNode) Node(t string) (string, bool) { return t, t == "nodeA" }
func (oneNode) Nodes() []string              { return []string{"nodeA"} }

// A running build in a node's view carries the same information as the
// transcode.NodeBuild the engine gets from the registry, field by field.
func TestRunningBuildMatchesNodeBuild(t *testing.T) {
	s, err := nodes.Open(nodes.Options{Path: filepath.Join(t.TempDir(), "nodes.db"), Topology: oneNode{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateBuild(context.Background(), nodes.BuildInput{Target: "nodeA", Source: "ci-tool", Holder: "runner1", Kind: "gradle", Title: "run", PreemptMode: nodes.PreemptGraceful, Via: nodes.BuildViaNodeLease}); err != nil {
		t.Fatal(err)
	}
	view, err := s.Node(context.Background(), "nodeA")
	if err != nil || len(view.RunningBuilds) != 1 {
		t.Fatalf("%+v %v", view, err)
	}
	recs := Records(s).NodeBuildRecords("nodeA")
	if len(recs) != 1 {
		t.Fatalf("%+v", recs)
	}
	rb, nb := reflect.ValueOf(view.RunningBuilds[0]), reflect.ValueOf(recs[0])
	rt, nt := rb.Type(), nb.Type()
	if rt.NumField() != nt.NumField() {
		t.Fatalf("RunningBuild has %d fields, NodeBuild %d", rt.NumField(), nt.NumField())
	}
	for i := 0; i < nt.NumField(); i++ {
		f := nt.Field(i)
		rf, ok := rt.FieldByName(f.Name)
		if !ok {
			t.Errorf("RunningBuild lacks %s", f.Name)
			continue
		}
		if strings.Split(rf.Tag.Get("json"), ",")[0] != strings.Split(f.Tag.Get("json"), ",")[0] {
			t.Errorf("%s: json name %q vs %q", f.Name, rf.Tag.Get("json"), f.Tag.Get("json"))
		}
		if got, want := rb.FieldByName(f.Name).Interface(), nb.Field(i).Interface(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v vs %v", f.Name, got, want)
		}
	}
}
