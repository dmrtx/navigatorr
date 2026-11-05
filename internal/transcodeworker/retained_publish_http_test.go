package transcodeworker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Publishing a retained candidate verifies the whole file before the worker
// answers. The server's read and write deadlines (15 s in production, for every
// other route) must not end that request: a candidate whose hash takes longer
// was cut off every time, never verified and never published.
func TestRetainedPublishRequestOutlastsTheServerDeadlines(t *testing.T) {
	const deadline = 150 * time.Millisecond
	f := newRetainedFixture(t, nil)
	f.retain("job-slow-1")

	// A hash that takes three times as long as the server's deadlines.
	onLocalHashRead = func(string) { time.Sleep(3 * deadline) }
	t.Cleanup(func() { onLocalHashRead = nil })

	ts := httptest.NewUnstartedServer(NewServer(f.w, "test-exe", "", "").Handler())
	ts.Config.ReadTimeout = deadline
	ts.Config.WriteTimeout = deadline
	ts.Start()
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/jobs/job-slow-1/publish", "application/json", nil)
	if err != nil {
		t.Fatalf("a slow verification must not cost the request its connection: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"finalization_state":"finalizing"`) {
		t.Fatalf("status = %d body = %s, want 200 finalizing", resp.StatusCode, body)
	}
	if job := pr6b1LoadJob(t, f.cfg.StateDir, "job-slow-1"); job.FinalizationState != string(FinalizationStateFinalizing) {
		t.Fatalf("job = %+v, want the publication recorded", job)
	}
}
