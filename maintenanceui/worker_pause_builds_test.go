package maintenanceui

import (
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// The page words "pause lifted; the running build still holds transcode" from
// the answer to its own POST (setNodePause reads builds[].status there), so the
// answer must list the node's running builds with their status.
func TestWorkerPauseAnswerListsTheRunningBuildsOfTheNode(t *testing.T) {
	_, h := buildsUI(t)
	const path = "/api/maintenance/worker-pause"
	node := transcode.DefaultWorkerName
	post := func(mode string) struct {
		Paused bool `json:"paused"`
		Builds []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"builds"`
	} {
		t.Helper()
		w := request(h, "POST", path, `{"node":"`+node+`","mode":"`+mode+`"}`, true)
		if w.Code != 200 {
			t.Fatal(mode, w.Code, w.Body.String())
		}
		var out struct {
			Paused bool `json:"paused"`
			Builds []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"builds"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err, w.Body.String())
		}
		return out
	}
	if got := post("graceful"); len(got.Builds) != 0 {
		t.Fatalf("no build yet: %+v", got)
	}
	b := decodeBuild(t, withBuildsToken(h, "POST", "/api/maintenance/builds", `{"target":"`+node+`","source":"ci","title":"run"}`))
	for _, mode := range []string{"graceful", "off"} {
		got := post(mode)
		if len(got.Builds) != 1 || got.Builds[0].ID != b.ID || got.Builds[0].Status != "running" {
			t.Fatalf("%s: the answer must list the running build with its status: %+v", mode, got)
		}
	}
	withBuildsToken(h, "POST", "/api/maintenance/builds/"+b.ID+"/finish", `{"status":"succeeded"}`)
	if got := post("off"); len(got.Builds) != 0 {
		t.Fatalf("an ended build is not listed: %+v", got)
	}
}
