package transcodeworker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// While the hold is on, the worker starts nothing from its queue; the next
// pass after it is lifted starts the jobs in order.
func TestHoldKeepsQueuedJobsQueued(t *testing.T) {
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 2, stub)
	seedEncoderJob(t, w, stub, "a", "queued", "libx265", false, 3*time.Minute)
	seedEncoderJob(t, w, stub, "b", "queued", "libx265", false, 2*time.Minute)
	if err := w.SetHold(true); err != nil {
		t.Fatal(err)
	}
	if !w.Held() {
		t.Fatal("the hold must be reported")
	}
	if started, err := w.ScheduleQueued(context.Background(), "exe", ""); err != nil || started != 0 {
		t.Fatalf("a held worker started %d (err %v)", started, err)
	}
	if jobStarted(t, w, "a") || jobStarted(t, w, "b") {
		t.Fatal("nothing may leave the queue while held")
	}
	if err := w.SetHold(false); err != nil {
		t.Fatal(err)
	}
	if w.Held() {
		t.Fatal("the hold must be lifted")
	}
	if started, err := w.ScheduleQueued(context.Background(), "exe", ""); err != nil || started != 2 {
		t.Fatalf("released worker started %d (err %v)", started, err)
	}
}

// The hold lives in the state directory, so a restart does not release it.
func TestHoldSurvivesARestartAndUnreadableRecordHolds(t *testing.T) {
	stub := newStubSpawn()
	w, _, _ := newPR3Worker(t, 2, stub)
	if w.Held() {
		t.Fatal("a new worker is not held")
	}
	if err := w.SetHold(true); err != nil {
		t.Fatal(err)
	}
	again := &Worker{cfg: w.cfg}
	if !again.Held() {
		t.Fatal("a restarted worker must still hold")
	}
	if err := os.WriteFile(filepath.Join(w.cfg.StateDir, holdFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !w.Held() {
		t.Fatal("an unreadable record must hold")
	}
	if err := w.SetHold(false); err != nil || w.Held() {
		t.Fatalf("release: %v", err)
	}
	if err := w.SetHold(false); err != nil {
		t.Fatalf("releasing twice is fine: %v", err)
	}
}

func TestHoldEndpoint(t *testing.T) {
	worker, srv, _, _ := newHTTPTestSetup(t, "secret")
	if resp := doRequest(t, srv, "GET", "/v1/hold", "", "secret"); resp.Code != 200 || resp.Body.String() == "" {
		t.Fatalf("get: %d", resp.Code)
	}
	if resp := doRequest(t, srv, "PUT", "/v1/hold", `{"hold":true}`, "secret"); resp.Code != 200 || !worker.Held() {
		t.Fatalf("put true: %d held=%v", resp.Code, worker.Held())
	}
	if resp := doRequest(t, srv, "PUT", "/v1/hold", `{"hold":true,"extra":1}`, "secret"); resp.Code != 400 {
		t.Fatalf("unknown fields must be refused: %d", resp.Code)
	}
	if resp := doRequest(t, srv, "PUT", "/v1/hold", `{"hold":false}`, "secret"); resp.Code != 200 || worker.Held() {
		t.Fatalf("put false: %d", resp.Code)
	}
	if resp := doRequest(t, srv, "PUT", "/v1/hold", `{"hold":true}`, "wrong"); resp.Code != 401 && resp.Code != 403 {
		t.Fatalf("auth: %d", resp.Code)
	}
}
