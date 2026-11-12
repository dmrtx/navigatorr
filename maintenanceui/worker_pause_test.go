package maintenanceui

import (
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestWorkerPauseEndpointPausesAndResumesANode(t *testing.T) {
	s, h := testUI(t)
	const path = "/api/maintenance/worker-pause"
	node := transcode.DefaultWorkerName
	read := func(body []byte) transcode.NodePauseStatus {
		t.Helper()
		var st transcode.NodePauseStatus
		if err := json.Unmarshal(body, &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	if w := request(h, "GET", path+"?node="+node, "", true); w.Code != 200 || read(w.Body.Bytes()).Paused {
		t.Fatal("a node starts unpaused", w.Code, w.Body.String())
	}
	for _, bad := range []string{`{}`, `{"node":"` + node + `"}`, `{"node":"` + node + `","mode":"later"}`, `{"mode":"graceful"}`, `{"node":"` + node + `","mode":"graceful","extra":1}`, `nope`} {
		if w := request(h, "POST", path, bad, true); w.Code != 400 {
			t.Fatal("a malformed change must be refused", bad, w.Code)
		}
	}
	if w := request(h, "POST", path, `{"node":"nowhere","mode":"graceful"}`, true); w.Code != 404 {
		t.Fatal("an unknown node must be a 404", w.Code, w.Body.String())
	}
	if pauses, _ := s.engine.Deps().Store.ListWorkerPauses(); len(pauses) != 0 {
		t.Fatal("a refused request must pause nothing", pauses)
	}

	w := request(h, "POST", path, `{"node":"`+node+`","mode":"graceful"}`, true)
	if st := read(w.Body.Bytes()); w.Code != 200 || !st.Paused || st.Mode != "graceful" || st.Node != node || st.Since == "" {
		t.Fatal("graceful pause", w.Code, w.Body.String())
	}
	w = request(h, "POST", path, `{"node":"`+node+`","mode":"immediate"}`, true)
	if st := read(w.Body.Bytes()); w.Code != 200 || st.Mode != "immediate" {
		t.Fatal("switching to immediate", w.Code, w.Body.String())
	}

	// The same state shows in the nodes list the header and dialog read.
	var list struct {
		Nodes []struct {
			Pause *transcode.NodePauseStatus `json:"pause"`
		} `json:"nodes"`
	}
	w = request(h, "GET", "/api/maintenance/workers", "", true)
	if json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Nodes) == 0 || list.Nodes[0].Pause == nil || !list.Nodes[0].Pause.Paused {
		t.Fatal("workers must carry each node's pause", w.Body.String())
	}
	var all struct {
		Nodes map[string]transcode.NodePauseStatus `json:"nodes"`
	}
	w = request(h, "GET", path, "", true)
	if json.Unmarshal(w.Body.Bytes(), &all) != nil || !all.Nodes[node].Paused {
		t.Fatal("the pause list must name the paused node", w.Body.String())
	}

	// Suspend is a valid mode; this node's worker predates it, so the change is
	// refused as a conflict and the pause stays what it was.
	w = request(h, "POST", path, `{"node":"`+node+`","mode":"suspend"}`, true)
	if w.Code != 409 {
		t.Fatal("a node that cannot suspend must be a 409", w.Code, w.Body.String())
	}
	if pauses, _ := s.engine.Deps().Store.ListWorkerPauses(); pauses[node].Mode != "immediate" {
		t.Fatal("a refused suspend must leave the pause as it was", pauses)
	}

	w = request(h, "POST", path, `{"node":"`+node+`","mode":"off"}`, true)
	if st := read(w.Body.Bytes()); w.Code != 200 || st.Paused || st.Mode != "" {
		t.Fatal("resume", w.Code, w.Body.String())
	}
	if request(h, "POST", path, `{"node":"`+node+`","mode":"graceful"}`, false).Code != 401 {
		t.Fatal("a pause must need the session")
	}
}
