package maintenanceui

import (
	"encoding/json"
	"github.com/jakenesler/navigatorr/nodes"
	"reflect"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// A build on a node is the node worker's lease. A client that does not say so
// (an old nbuild, a script) is told where the build must run, and nothing is
// recorded.
func TestNodeTargetWithoutViaIs409NodeLeaseRequired(t *testing.T) {
	s, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	const path = "/api/maintenance/builds"
	w := withBuildsToken(h, "POST", path, `{"target":"`+node+`","source":"ci","title":"run"}`)
	var refusal map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err, w.Body.String())
	}
	if w.Code != 409 || refusal["code"] != "node_lease_required" {
		t.Fatal("a node target without via is a 409 node_lease_required", w.Code, w.Body.String())
	}
	if list, _ := s.nodes.ListBuilds(nodes.BuildFilter{}); len(list) != 0 {
		t.Fatal("a refused create records nothing", list)
	}
	// The marker is the only difference. (The API answers 200 for a create.)
	if w := withBuildsToken(h, "POST", path, `{"target":"`+node+`","via":"node_lease","source":"ci","title":"run"}`); w.Code != 200 || decodeBuild(t, w).ID == "" {
		t.Fatal("with via the create succeeds", w.Code, w.Body.String())
	}
	// A machine that is not a node needs no marker.
	if w := withBuildsToken(h, "POST", path, `{"target":"laptop","source":"ci","title":"run"}`); w.Code != 200 {
		t.Fatal("a non-node target needs no via", w.Code, w.Body.String())
	}
}

func TestUnknownPriorityIsA400(t *testing.T) {
	s, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	for _, target := range []string{node, "laptop"} {
		w := withBuildsToken(h, "POST", "/api/maintenance/builds", `{"target":"`+target+`","via":"node_lease","source":"ci","title":"run","priority":"urgent"}`)
		if w.Code != 400 {
			t.Fatal("an unknown priority is a 400", target, w.Code, w.Body.String())
		}
	}
	if list, _ := s.nodes.ListBuilds(nodes.BuildFilter{}); len(list) != 0 {
		t.Fatal("a refused create records nothing", list)
	}
}

// The page reads the node's pause from the POST answer and from the GET: both
// carry the records and the hold the worker's leases show, in the same shape.
func TestPauseAnswerHasTheShapeOfTheGetForEveryBranch(t *testing.T) {
	s, h := buildsUI(t)
	node := transcode.DefaultWorkerName
	withBuildsToken(h, "POST", "/api/maintenance/builds", `{"target":"`+node+`","via":"node_lease","source":"ci","title":"run"}`)
	reportLeases(t, s, runningLease("lease1"))

	asMap := func(body []byte) map[string]any {
		t.Helper()
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err, string(body))
		}
		return out
	}
	keys := func(m map[string]any) []string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		return sortStrings(ks)
	}
	for _, c := range []struct{ name, body string }{
		{"mode", `{"node":"` + node + `","mode":"graceful"}`},
		{"builds pause", `{"node":"` + node + `","builds_paused":true}`},
	} {
		post := request(h, "POST", "/api/maintenance/worker-pause", c.body, true)
		if post.Code != 200 {
			t.Fatal(c.name, post.Code, post.Body.String())
		}
		got := asMap(post.Body.Bytes())
		get := request(h, "GET", "/api/maintenance/worker-pause?node="+node, "", true)
		want := asMap(get.Body.Bytes())
		if !reflect.DeepEqual(keys(got), keys(want)) {
			t.Fatalf("%s: the POST answer must have the keys of the GET:\n post %v\n get  %v", c.name, keys(got), keys(want))
		}
		if b, _ := got["builds"].([]any); len(b) != 1 {
			t.Fatal(c.name, "the POST answer lists the node's builds", got)
		}
	}
	// Lifting the operator pause: the lease the worker reports is what holds the node, in both.
	post := request(h, "POST", "/api/maintenance/worker-pause", `{"node":"`+node+`","mode":"off"}`, true)
	got := asMap(post.Body.Bytes())
	if got["held_by"] != "builds" || got["paused"] != true {
		t.Fatal("the POST answer shows the hold the leases give, like the GET", got)
	}
}

func sortStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}
