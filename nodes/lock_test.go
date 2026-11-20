package nodes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOpenTakesTheDatabaseLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.db")
	first, err := Open(Options{Path: path, Topology: fakeTopology{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Path: path, Topology: fakeTopology{}}); err == nil || !strings.Contains(err.Error(), "held by another process") {
		t.Fatalf("a second Open on the same path must fail clearly: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("the lock file is never deleted: %v", err)
	}
	if fi, _ := os.Stat(path + ".lock"); fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode %v", fi.Mode().Perm())
	}
	again, err := Open(Options{Path: path, Topology: fakeTopology{}})
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	again.Close()
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("the lock file stays after the second Close: %v", err)
	}
}

type countingView struct{ calls atomic.Int32 }

func (c *countingView) TranscodeStates(context.Context) map[string]TranscodeState {
	c.calls.Add(1)
	return map[string]TranscodeState{"nodeA": {Mode: "graceful"}}
}

func TestNodesListSkipsTheTranscodeViewOnRequest(t *testing.T) {
	e, _ := buildSvc(t)
	tv := &countingView{}
	e.tv = tv
	h := e.Handler()
	nodesOf := func(path string) []any {
		_, body := call(t, h, "GET", path, "")
		list, _ := body["nodes"].([]any)
		return list
	}
	list := nodesOf("/nodes/v1/nodes")
	if tv.calls.Load() != 1 || list[0].(map[string]any)["transcode"] == nil {
		t.Fatalf("the default asks the view once and shows the block: calls=%d %v", tv.calls.Load(), list)
	}
	list = nodesOf("/nodes/v1/nodes?transcode=false")
	if tv.calls.Load() != 1 {
		t.Fatalf("transcode=false must not call the view: calls=%d", tv.calls.Load())
	}
	if len(list) != 2 {
		t.Fatalf("still lists every node: %v", list)
	}
	for _, n := range list {
		if _, has := n.(map[string]any)["transcode"]; has {
			t.Fatalf("no transcode block with transcode=false: %v", n)
		}
	}
}

// The spec lists every field RunningBuild marshals, and the other way round.
func TestSpecRunningBuildMatchesTheType(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas struct {
				Node struct {
					Properties struct {
						RunningBuilds struct {
							Items struct {
								Properties map[string]json.RawMessage `json:"properties"`
							} `json:"items"`
						} `json:"running_builds"`
					} `json:"properties"`
				} `json:"Node"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(Spec(), &doc); err != nil {
		t.Fatal(err)
	}
	spec := doc.Components.Schemas.Node.Properties.RunningBuilds.Items.Properties
	rt := reflect.TypeOf(RunningBuild{})
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if _, ok := spec[tag]; !ok {
			t.Errorf("spec lacks running_builds field %q", tag)
		}
		delete(spec, tag)
	}
	for extra := range spec {
		t.Errorf("spec has running_builds field %q the type does not", extra)
	}
}

func TestNodeViewRunningBuildCarriesTheRecord(t *testing.T) {
	e, _ := buildSvc(t)
	b := startBuild(t, e, "nodeA", PreemptGraceful, 0)
	v, err := e.Node(context.Background(), "nodeA")
	if err != nil || len(v.RunningBuilds) != 1 {
		t.Fatalf("%+v %v", v, err)
	}
	rb := v.RunningBuilds[0]
	if rb.Status != BuildRunning || rb.Priority != b.Priority || rb.PreemptMode != b.PreemptMode || rb.CreatedAt != b.CreatedAt || rb.LastHeartbeatAt != b.LastHeartbeatAt || rb.ExpiresAt != b.ExpiresAt || rb.CreatedAt == "" {
		t.Fatalf("%+v vs %+v", rb, b)
	}
}
