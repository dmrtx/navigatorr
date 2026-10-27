package nashelper

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func (f *fixture) tree() {
	f.t.Helper()
	for path, size := range map[string]int{
		"a.mkv":            1000,
		"show/s1/e1.mkv":   2000,
		"show/s1/e2.mkv":   3000,
		"show/.hidden/x":   10,
		"show/empty/.keep": 0,
	} {
		full := filepath.Join(f.root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
}

// sameTotals compares two results by their totals alone.
func sameTotals(a, b UsageResult) bool {
	a.Stamps, b.Stamps = nil, nil
	return a.Bytes == b.Bytes && a.Files == b.Files && a.Dirs == b.Dirs && a.Skipped == b.Skipped
}

func (f *fixture) usage(path string) (int, Job, []byte) {
	f.t.Helper()
	status, raw := f.call("POST", "/v1/usage", testToken, PathRequest{Path: path})
	var j Job
	_ = json.Unmarshal(raw, &j)
	return status, j, raw
}

func TestUsageSumsTheTree(t *testing.T) {
	for _, walk := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "walk"}[walk], func(t *testing.T) {
			f := newFixtureWith(t, 2, walk)
			f.tree()
			for path, want := range map[string]UsageResult{
				f.root:                           {Bytes: 6010, Files: 5, Dirs: 4},
				filepath.Join(f.root, "show"):    {Bytes: 5010, Files: 4, Dirs: 3},
				filepath.Join(f.root, "show/s1"): {Bytes: 5000, Files: 2, Dirs: 0},
			} {
				status, job, raw := f.usage(path)
				if status != http.StatusAccepted {
					t.Fatalf("%s: %d %s", path, status, raw)
				}
				done := f.waitJob(job.ID, JobDone, JobFailed)
				if done.State != JobDone || done.Usage == nil || !sameTotals(*done.Usage, want) {
					t.Errorf("%s: %+v, want %+v", path, done, want)
				}
				if got := done.Usage.Stamps; got == nil || got["."] == 0 || len(got) != int(want.Dirs)+1 {
					t.Errorf("%s: stamps %v, want one per directory", path, got)
				}
			}
		})
	}
}

func TestUsageDoesNotFollowLinksOrLeaveTheRoots(t *testing.T) {
	for _, walk := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "walk"}[walk], func(t *testing.T) {
			f := newFixtureWith(t, 2, walk)
			f.tree()
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "big"), make([]byte, 99999), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(f.root, "show", "out")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "big"), filepath.Join(f.root, "big-link")); err != nil {
				t.Fatal(err)
			}
			_, job, _ := f.usage(f.root)
			done := f.waitJob(job.ID, JobDone, JobFailed)
			if done.Usage == nil || done.Usage.Bytes != 6010 {
				t.Fatalf("links must not count: %+v", done)
			}
			for name, path := range map[string]string{
				"outside":      outside,
				"dot dot":      filepath.Join(f.root, "..", filepath.Base(outside)),
				"through link": filepath.Join(f.root, "show", "out"),
				"relative":     "show",
			} {
				if status, _, raw := f.usage(path); status != http.StatusForbidden {
					t.Errorf("%s: %d %s", name, status, raw)
				}
			}
			status, _, raw := f.usage(filepath.Join(f.root, "a.mkv"))
			if status != http.StatusUnprocessableEntity || f.errCode(raw) != CodeNotDir {
				t.Errorf("a file is not a directory: %d %s", status, raw)
			}
			status, _, raw = f.usage(filepath.Join(f.root, "missing"))
			if status != http.StatusNotFound {
				t.Errorf("missing: %d %s", status, raw)
			}
		})
	}
}

func TestUsageNeedsTheTokenAndAPath(t *testing.T) {
	f := newFixture(t, 2)
	if status, _ := f.call("POST", "/v1/usage", "", PathRequest{Path: f.root}); status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", status)
	}
	if status, _ := f.call("GET", "/v1/volumes", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("volumes without a token: %d", status)
	}
	if status, _ := f.call("POST", "/v1/usage", testToken, `{"path":"/x","extra":1}`); status != http.StatusBadRequest {
		t.Fatalf("extra field: %d", status)
	}
}

func TestVolumesReportTheRootsSpace(t *testing.T) {
	f := newFixture(t, 2)
	status, raw := f.call("GET", "/v1/volumes", testToken, nil)
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, raw)
	}
	var resp VolumeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.InstanceID != f.srv.InstanceID() || len(resp.Volumes) != 1 {
		t.Fatalf("%+v", resp)
	}
	v := resp.Volumes[0]
	if v.Root != f.root || v.TotalBytes <= 0 || v.FreeBytes < 0 || v.FreeBytes > v.TotalBytes || v.AvailBytes > v.FreeBytes {
		t.Fatalf("implausible volume: %+v", v)
	}
}

func TestAUsageWalkCanBeCancelled(t *testing.T) {
	f := newFixture(t, 2)
	// Hold the only walk slot so the next one stays queued, then cancel it.
	f.srv.usageSem <- struct{}{}
	defer func() { <-f.srv.usageSem }()
	_, job, _ := f.usage(f.root)
	if job.State != JobQueued {
		t.Fatalf("state %s", job.State)
	}
	if status, _ := f.call("DELETE", "/v1/jobs/"+job.ID, testToken, nil); status != http.StatusAccepted {
		t.Fatalf("cancel: %d", status)
	}
	if done := f.waitJob(job.ID, JobCancelled); done.Error == nil || done.Error.Code != CodeCancelled {
		t.Fatalf("%+v", done)
	}
}

func TestClientUsageAndVolumes(t *testing.T) {
	f := newFixture(t, 2)
	f.tree()
	c := f.client(t)
	res, err := c.Usage(context.Background(), filepath.Join(f.root, "show"), nil)
	if err != nil || !sameTotals(res, UsageResult{Bytes: 5010, Files: 4, Dirs: 3}) {
		t.Fatalf("%+v, %v", res, err)
	}
	vols, err := c.Volumes(context.Background())
	if err != nil || len(vols.Volumes) != 1 || vols.Volumes[0].TotalBytes <= 0 {
		t.Fatalf("%+v, %v", vols, err)
	}
	if _, err := c.Usage(context.Background(), "/nowhere", nil); !ShouldFallBack(err) {
		t.Fatalf("an unmapped path must fall back: %v", err)
	}
	if _, err := c.Usage(context.Background(), filepath.Join(f.root, "missing"), nil); err == nil || !IsAnswer(err) {
		t.Fatalf("a missing directory is the helper's answer: %v", err)
	}
	// The caller's deadline ends the wait and cancels the job on the helper.
	f.srv.usageSem <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Usage(ctx, f.root, nil); err == nil {
		t.Fatal("expected the deadline to end the call")
	}
	<-f.srv.usageSem
}

// A walk someone waits on has its own lane: it runs while the background lane is
// held by the statistics' walk of a whole root.
func TestInteractiveUsageDoesNotQueueBehindBackgroundWalks(t *testing.T) {
	f := newFixture(t, 2)
	f.tree()
	f.srv.usageSem <- struct{}{}
	defer func() { <-f.srv.usageSem }()

	_, background, _ := f.usage(f.root)
	if background.State != JobQueued {
		t.Fatalf("the background walk should queue behind the held lane: %s", background.State)
	}
	status, raw := f.call("POST", "/v1/usage?priority=interactive", testToken, PathRequest{Path: filepath.Join(f.root, "show")})
	var job Job
	if status != http.StatusAccepted || json.Unmarshal(raw, &job) != nil {
		t.Fatalf("%d %s", status, raw)
	}
	if done := f.waitJob(job.ID, JobDone, JobFailed); done.State != JobDone || done.Usage == nil || done.Usage.Bytes != 5010 {
		t.Fatalf("%+v", done)
	}
}

func TestUsageStampsDirectoriesOnlyWhenTheyAreFew(t *testing.T) {
	f := newFixture(t, 2)
	for i := 0; i < MaxUsageStamps+1; i++ {
		if err := os.MkdirAll(filepath.Join(f.root, "many", "d"+strconv.Itoa(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, job, _ := f.usage(filepath.Join(f.root, "many"))
	done := f.waitJob(job.ID, JobDone, JobFailed)
	if done.State != JobDone || done.Usage.Dirs != MaxUsageStamps+1 || done.Usage.Stamps != nil {
		t.Fatalf("a tree past the cap carries no stamps: %+v", done.Usage)
	}
}

// The run budget starts when the helper starts the walk, so a long wait in the
// queue does not use it up.
func TestClientUsageBudgetCountsOnlyTheRun(t *testing.T) {
	f := newFixture(t, 2)
	f.tree()
	c := f.client(t)
	f.srv.foldSem <- struct{}{}
	f.srv.foldSem <- struct{}{}
	var states []string
	var mu sync.Mutex
	go func() {
		time.Sleep(400 * time.Millisecond)
		<-f.srv.foldSem
		<-f.srv.foldSem
	}()
	res, err := c.Usage(context.Background(), filepath.Join(f.root, "show"), nil,
		UsageInteractive(), UsageRunBudget(200*time.Millisecond),
		UsageOnState(func(s string) { mu.Lock(); states = append(states, s); mu.Unlock() }))
	if err != nil || res.Bytes != 5010 {
		t.Fatalf("a queue longer than the budget must not fail the walk: %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) < 2 || states[0] != JobQueued || states[len(states)-1] == JobQueued {
		t.Fatalf("states %v, want queued first and then past it", states)
	}
}
