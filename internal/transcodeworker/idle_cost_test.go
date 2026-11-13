package transcodeworker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resetJobIndexes drops both caches, as a restart does.
func resetJobIndexes() {
	jobIndex.Lock()
	jobIndex.m = map[string]*jobCacheEntry{}
	jobIndex.Unlock()
	dirIndex.Lock()
	dirIndex.m = map[string]*dirListing{}
	dirIndex.Unlock()
}

// ageFile makes a file (or directory) look untouched for hours, so the indexes
// treat it as settled.
func ageFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func seedAged(t *testing.T, w *Worker, job *JobRecord, age time.Duration) string {
	t.Helper()
	path := filepath.Join(w.cfg.StateDir, job.ID, "job.json")
	if err := SaveJobAtomic(path, job); err != nil {
		t.Fatal(err)
	}
	ageFile(t, path, age)
	return path
}

func cachedEntry(path string) *jobCacheEntry {
	jobIndex.Lock()
	defer jobIndex.Unlock()
	return jobIndex.m[path]
}

func TestJobIndexFollowsWritesAndExternalEdits(t *testing.T) {
	resetJobIndexes()
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(t.TempDir(), "jobs")})

	// A settled finished record is served from memory.
	done := seedAged(t, w, &JobRecord{ID: "done", Status: "completed", FinishedAt: time.Now().Add(-48 * time.Hour)}, 48*time.Hour)
	got, err := peekJob(done)
	if err != nil || got.Status != "completed" {
		t.Fatalf("peek: %+v %v", got, err)
	}
	if cachedEntry(done) == nil || !cachedEntry(done).stable {
		t.Fatal("a settled finished record must be indexed as stable")
	}

	// A write through the worker shows at once, with no waiting for a recheck.
	if err := SaveJobAtomic(done, &JobRecord{ID: "done", Status: "failed", Error: "later"}); err != nil {
		t.Fatal(err)
	}
	if got, err := peekJob(done); err != nil || got.Status != "failed" || got.Error != "later" {
		t.Fatalf("a worker write must be seen immediately: %+v %v", got, err)
	}

	// A record being rewritten (younger than the racy window) is never kept.
	if cachedEntry(done) != nil {
		t.Fatal("a record modified moments ago must not be indexed")
	}

	// A running record edited behind the worker's back (another process, by
	// hand) is noticed through its file time and size on the next read.
	run := seedAged(t, w, &JobRecord{ID: "run", Status: "running", PID: 7}, time.Hour)
	if got, _ := peekJob(run); got.PID != 7 {
		t.Fatalf("running record: %+v", got)
	}
	edited, _ := json.Marshal(&JobRecord{ID: "run", Status: "running", PID: 99999})
	if err := os.WriteFile(run, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	ageFile(t, run, 30*time.Minute)
	if got, _ := peekJob(run); got.PID != 99999 {
		t.Fatalf("an external edit of a running record must be seen: %+v", got)
	}

	// A finished record edited externally is seen at the next recheck at the
	// latest, and a deleted one is an error again, as an uncached load says.
	fin := seedAged(t, w, &JobRecord{ID: "fin", Status: "completed"}, time.Hour)
	if _, err := peekJob(fin); err != nil {
		t.Fatal(err)
	}
	edited, _ = json.Marshal(&JobRecord{ID: "fin", Status: "completed", Error: "edited"})
	if err := os.WriteFile(fin, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	ageFile(t, fin, 30*time.Minute)
	cachedEntry(fin).checked = time.Now().Add(-2 * jobCacheStableRecheck)
	if got, _ := peekJob(fin); got.Error != "edited" {
		t.Fatalf("a finished record must be re-validated after the recheck window: %+v", got)
	}
	if err := os.RemoveAll(filepath.Dir(fin)); err != nil {
		t.Fatal(err)
	}
	cachedEntry(fin).checked = time.Now().Add(-2 * jobCacheStableRecheck)
	if _, err := peekJob(fin); !os.IsNotExist(unwrapPathErr(err)) {
		t.Fatalf("a deleted record must read as missing, got %v", err)
	}
	if cachedEntry(fin) != nil {
		t.Fatal("a deleted record must leave the index")
	}
}

func unwrapPathErr(err error) error {
	for err != nil {
		if pe, ok := err.(*os.PathError); ok {
			return pe
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
	return nil
}

func TestJobDirListingFollowsCreatesAndDeletes(t *testing.T) {
	resetJobIndexes()
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(t.TempDir(), "jobs")})
	a := seedAged(t, w, &JobRecord{ID: "a", Status: "completed"}, time.Hour)
	seedAged(t, w, &JobRecord{ID: "b", Status: "completed"}, time.Hour)
	ageFile(t, w.cfg.StateDir, 2*time.Hour)

	names := func() string {
		entries, err := w.readJobDirs()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return strings.Join(out, ",")
	}
	if got := names(); got != "a,b" {
		t.Fatalf("listing: %s", got)
	}
	if _, err := peekJob(a); err != nil {
		t.Fatal(err)
	}
	if dirIndex.m[w.cfg.StateDir] == nil {
		t.Fatal("a settled listing must be indexed")
	}

	// A job directory created later shows up (the directory's time moved).
	seedAged(t, w, &JobRecord{ID: "c", Status: "completed"}, time.Hour)
	ageFile(t, w.cfg.StateDir, 90*time.Minute)
	if got := names(); got != "a,b,c" {
		t.Fatalf("a new job must be listed: %s", got)
	}

	// A directory deleted behind the worker's back leaves the listing and the
	// record index.
	if err := os.RemoveAll(filepath.Dir(a)); err != nil {
		t.Fatal(err)
	}
	ageFile(t, w.cfg.StateDir, 80*time.Minute)
	if got := names(); got != "b,c" {
		t.Fatalf("a deleted job must leave the listing: %s", got)
	}
	if cachedEntry(a) != nil {
		t.Fatal("a deleted job must leave the record index")
	}
}

// The polled endpoints answer what a scan of the disk answers, byte for byte,
// whether the indexes are warm or cold.
func TestPolledEndpointsMatchAFreshScan(t *testing.T) {
	resetJobIndexes()
	worker, srv, _, _ := newHTTPTestSetup(t, "secret")
	worker.isAlive = func(*JobRecord) bool { return true }
	for i := 0; i < 40; i++ {
		seedAged(t, worker, &JobRecord{ID: fmt.Sprintf("old-%02d", i), Status: "completed", FinishedAt: time.Now().Add(-72 * time.Hour)}, 72*time.Hour)
	}
	seedAged(t, worker, &JobRecord{ID: "run-a", Status: "running", PID: 11, Source: "/m/a.mkv", CreatedAt: time.Now().Add(-time.Hour)}, time.Minute)
	seedAged(t, worker, &JobRecord{ID: "run-b", Status: "running", PID: 12, Source: "/m/b.mkv", CreatedAt: time.Now().Add(-time.Hour)}, time.Minute)
	seedAged(t, worker, &JobRecord{ID: "wait", Status: "queued", Source: "/m/c.mkv", CreatedAt: time.Now().Add(-time.Hour)}, time.Minute)
	ageFile(t, worker.cfg.StateDir, time.Hour)

	get := func(path string) string {
		resp := doRequest(t, srv, "GET", path, "", "secret")
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, resp.Code, resp.Body.String())
		}
		// The wall clock moves between two requests; nothing else may.
		var doc map[string]any
		if json.Unmarshal(resp.Body.Bytes(), &doc) == nil {
			delete(doc, "wall_duration_ms")
			if b, err := json.Marshal(doc); err == nil {
				return string(b)
			}
		}
		return resp.Body.String()
	}
	paths := []string{"/v1/hold", "/v1/suspend", "/v1/jobs/run-a", "/v1/jobs/wait", "/v1/jobs/old-03"}
	cold := map[string]string{}
	for _, p := range paths {
		resetJobIndexes()
		cold[p] = get(p)
	}
	for _, p := range paths {
		for i := 0; i < 3; i++ { // warm
			if got := get(p); got != cold[p] {
				t.Fatalf("%s differs warm vs cold:\nwarm %s\ncold %s", p, got, cold[p])
			}
		}
	}
	if want := `{"active":2,"hold":false}`; cold["/v1/hold"] != want {
		t.Fatalf("hold body %q, want %q", cold["/v1/hold"], want)
	}
	if want := `{"active":2,"frozen":0,"suspend":false}`; cold["/v1/suspend"] != want {
		t.Fatalf("suspend body %q, want %q", cold["/v1/suspend"], want)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(cold["/v1/jobs/wait"]), &st); err != nil || st["status"] != "queued" || st["queue_position"] != float64(1) {
		t.Fatalf("queued job status: %v %v", st, err)
	}
}

func TestRequestLogSkipsSuccessfulPollsOnly(t *testing.T) {
	resetJobIndexes()
	worker, srv, _, _ := newHTTPTestSetup(t, "secret")
	seedAged(t, worker, &JobRecord{ID: "known", Status: "completed"}, time.Hour)
	var lines []string
	srv.logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	h := srv.Handler()
	call := func(method, path, body, token string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, c := range []struct {
		method, path, body, token string
		wantLogged                bool
	}{
		{"GET", "/v1/hold", "", "secret", false},
		{"GET", "/v1/suspend", "", "secret", false},
		{"GET", "/v1/health", "", "secret", false},
		{"GET", "/v1/activity", "", "secret", false},
		{"GET", "/v1/jobs/known", "", "secret", false},
		{"GET", "/v1/jobs/unknown", "", "secret", true},       // 404
		{"GET", "/v1/hold", "", "wrong", true},                // 401
		{"GET", "/v1/jobs/known/logs", "", "secret", true},    // not a status poll
		{"PUT", "/v1/hold", `{"hold":false}`, "secret", true}, // mutating
		{"PUT", "/v1/suspend", `{"suspend":false}`, "secret", true},
		{"DELETE", "/v1/jobs/known", "", "secret", true},
		{"POST", "/v1/jobs", `{}`, "secret", true},
	} {
		before := len(lines)
		call(c.method, c.path, c.body, c.token)
		if logged := len(lines) > before; logged != c.wantLogged {
			t.Errorf("%s %s (token %q): logged=%v, want %v", c.method, c.path, c.token, logged, c.wantLogged)
		}
	}
}

func TestIsPolledPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/hold": true, "/v1/suspend": true, "/v1/capabilities": true, "/v1/ready": true, "/v1/slots": true,
		"/v1/jobs/abc": true, "/v1/jobs/": false, "/v1/jobs": false, "/v1/jobs/abc/logs": false,
		"/v1/doctor": false, "/v1/podcast-ad-library": false,
	} {
		if got := isPolledPath(path); got != want {
			t.Errorf("isPolledPath(%q)=%v, want %v", path, got, want)
		}
	}
}

func TestCappedLogRotatesAndKeepsOneOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.err.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := NewCappedLog(f, 100)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 10; i++ {
		if _, err := c.Write([]byte(fmt.Sprintf("%02d", i) + line[2:])); err != nil {
			t.Fatal(err)
		}
		if fi, _ := os.Stat(path); fi.Size() > 100 {
			t.Fatalf("log grew past its cap: %d", fi.Size())
		}
	}
	cur, _ := os.ReadFile(path)
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("the rotated text must be kept: %v", err)
	}
	if len(old) > 100 || len(old) == 0 {
		t.Fatalf("rotated file holds %d bytes", len(old))
	}
	if !strings.HasPrefix(string(cur), "09") && !strings.Contains(string(cur), "09") {
		t.Fatalf("the newest line must be in the live log: %q", cur)
	}
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Fatal("only one old file may be kept")
	}
	if _, err := os.Stat(path + ".1.tmp"); err == nil {
		t.Fatal("no temporary file may be left behind")
	}
}

func TestCappedLogCutsAnOversizedLogAtStartWithoutAHole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.err.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 500)+"TAIL"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644) // not O_APPEND: the offset must be reset
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Seek(0, 2); err != nil {
		t.Fatal(err)
	}
	c, err := NewCappedLog(f, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("fresh\n")); err != nil {
		t.Fatal(err)
	}
	cur, _ := os.ReadFile(path)
	if string(cur) != "fresh\n" {
		t.Fatalf("live log after the cut: %q", cur)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil || len(old) != 100 || !strings.HasSuffix(string(old), "TAIL") {
		t.Fatalf("the newest 100 bytes must be kept: len=%d err=%v", len(old), err)
	}
}

func TestCappedLogRefusesAPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := NewCappedLog(w, 100); err == nil {
		t.Fatal("a pipe has no file to cap")
	}
}

// ---- retention ----

func seedFinished(t *testing.T, w *Worker, job *JobRecord, age time.Duration) string {
	t.Helper()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().Add(-age)
	}
	if job.FinishedAt.IsZero() && isTerminalStatus(job.Status) {
		job.FinishedAt = time.Now().Add(-age)
	}
	return seedAged(t, w, job, age)
}

func TestPruneRemovesOnlyOldFinishedJobs(t *testing.T) {
	resetJobIndexes()
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(t.TempDir(), "jobs"), JobRetentionDays: 30})
	w.isAlive = func(job *JobRecord) bool { return job.ID == "live-done" }
	old := 45 * 24 * time.Hour

	gone := []string{"done-old", "cancelled-old", "failed-early-old"}
	seedFinished(t, w, &JobRecord{ID: "done-old", Status: "completed", EncodeComplete: true, FinalizationState: string(FinalizationStateCompleted)}, old)
	seedFinished(t, w, &JobRecord{ID: "cancelled-old", Status: "cancelled"}, old)
	seedFinished(t, w, &JobRecord{ID: "failed-early-old", Status: "failed", Error: "probe failed"}, old)

	kept := []string{
		"done-recent", "failed-recent",
		"queued-old", "running-old", "post-encode-old", "failed-publication-old",
		"deferred-old", "live-done", "touched-recently",
	}
	seedFinished(t, w, &JobRecord{ID: "done-recent", Status: "completed"}, 3*24*time.Hour)
	seedFinished(t, w, &JobRecord{ID: "failed-recent", Status: "failed"}, 24*time.Hour)
	seedFinished(t, w, &JobRecord{ID: "queued-old", Status: "queued"}, old)
	seedFinished(t, w, &JobRecord{ID: "running-old", Status: "running", PID: 5}, old)
	seedFinished(t, w, &JobRecord{ID: "post-encode-old", Status: "running", EncodeComplete: true, FinalizationState: string(FinalizationStatePending)}, old)
	seedFinished(t, w, &JobRecord{ID: "failed-publication-old", Status: "failed", EncodeComplete: true, FinalizationState: string(FinalizationStatePending), PartialPath: "/x"}, old)
	seedFinished(t, w, &JobRecord{ID: "deferred-old", Status: "running", PublishPolicy: "on_approval", EncodeComplete: true, FinalizationState: string(FinalizationStateRetained)}, old)
	seedFinished(t, w, &JobRecord{ID: "live-done", Status: "completed", PID: 9}, old)
	touched := seedFinished(t, w, &JobRecord{ID: "touched-recently", Status: "completed"}, old)
	ageFile(t, touched, time.Hour) // the record file itself was written an hour ago

	// A retained candidate lives in a hidden directory and is never pruned,
	// however old; a directory without a record is not a job.
	hidden := filepath.Join(w.cfg.StateDir, retainedDirPrefix+"held")
	if err := SaveJobAtomic(filepath.Join(hidden, "job.json"), &JobRecord{ID: "held", Status: "running", FinalizationState: string(FinalizationStateRetained)}); err != nil {
		t.Fatal(err)
	}
	ageFile(t, filepath.Join(hidden, "job.json"), old)
	if err := os.MkdirAll(filepath.Join(w.cfg.StateDir, "no-record"), 0o755); err != nil {
		t.Fatal(err)
	}

	n, err := w.PruneJobs(context.Background(), time.Now())
	if err != nil || n != len(gone) {
		t.Fatalf("pruned %d (err %v), want %d", n, err, len(gone))
	}
	for _, id := range gone {
		if _, err := os.Stat(filepath.Join(w.cfg.StateDir, id)); !os.IsNotExist(err) {
			t.Errorf("%s must be removed", id)
		}
		if cachedEntry(filepath.Join(w.cfg.StateDir, id, "job.json")) != nil {
			t.Errorf("%s must leave the record index", id)
		}
	}
	for _, id := range append(kept, retainedDirPrefix+"held", "no-record") {
		if _, err := os.Stat(filepath.Join(w.cfg.StateDir, id)); err != nil {
			t.Errorf("%s must be kept: %v", id, err)
		}
	}

	// A second pass finds nothing more.
	if n, err := w.PruneJobs(context.Background(), time.Now()); err != nil || n != 0 {
		t.Fatalf("second pass pruned %d (err %v)", n, err)
	}
}

func TestPruneRespectsConfiguredWindowAndCanBeOff(t *testing.T) {
	resetJobIndexes()
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(t.TempDir(), "jobs"), JobRetentionDays: 7})
	seedFinished(t, w, &JobRecord{ID: "ten-days", Status: "completed"}, 10*24*time.Hour)
	seedFinished(t, w, &JobRecord{ID: "three-days", Status: "completed"}, 3*24*time.Hour)
	if n, _ := w.PruneJobs(context.Background(), time.Now()); n != 1 {
		t.Fatalf("a 7 day window must remove only the 10 day old job, removed %d", n)
	}
	if _, err := os.Stat(filepath.Join(w.cfg.StateDir, "three-days")); err != nil {
		t.Fatal("the 3 day old job must stay")
	}

	off := NewWorker(&WorkerConfig{StateDir: w.cfg.StateDir, JobRetentionDays: -1})
	seedFinished(t, off, &JobRecord{ID: "ancient", Status: "completed"}, 400*24*time.Hour)
	if n, _ := off.PruneJobs(context.Background(), time.Now()); n != 0 {
		t.Fatalf("a negative window keeps every job, removed %d", n)
	}
	off.MaybePruneJobs(context.Background())
	if _, err := os.Stat(filepath.Join(w.cfg.StateDir, "ancient")); err != nil {
		t.Fatal("pruning must be off")
	}

	if got := NewWorker(&WorkerConfig{StateDir: w.cfg.StateDir}).jobRetention(); got != 30*24*time.Hour {
		t.Fatalf("the default window is 30 days, got %s", got)
	}
}

func TestPruneIsRateLimitedAndRunsFromTheScheduler(t *testing.T) {
	resetJobIndexes()
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(t.TempDir(), "jobs")})
	seedFinished(t, w, &JobRecord{ID: "old", Status: "completed"}, 60*24*time.Hour)
	w.runSchedulerSweeps(context.Background(), "exe", "")
	if _, err := os.Stat(filepath.Join(w.cfg.StateDir, "old")); !os.IsNotExist(err) {
		t.Fatal("a scheduler pass must prune an old finished job")
	}
	seedFinished(t, w, &JobRecord{ID: "old2", Status: "completed"}, 60*24*time.Hour)
	w.MaybePruneJobs(context.Background())
	if _, err := os.Stat(filepath.Join(w.cfg.StateDir, "old2")); err != nil {
		t.Fatal("a second pass within the hour must not scan again")
	}
}
