package nashelper

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
				if done.State != JobDone || done.Usage == nil || *done.Usage != want {
					t.Errorf("%s: %+v, want %+v", path, done, want)
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
	if err != nil || res != (UsageResult{Bytes: 5010, Files: 4, Dirs: 3}) {
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
