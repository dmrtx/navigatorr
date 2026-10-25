package nashelper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/internal/sampledigest"
)

const testToken = "0123456789abcdef0123456789abcdef-token"

type fixture struct {
	t    *testing.T
	root string
	srv  *Server
	http *httptest.Server
}

func newFixture(t *testing.T, maxHashes int) *fixture { return newFixtureWith(t, maxHashes, false) }

// newFixtureWith can force the component-walk resolver, which is what a kernel
// without openat2 uses.
func newFixtureWith(t *testing.T, maxHashes int, walk bool) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(Config{Roots: []string{root}, Token: testToken, MaxHashes: maxHashes})
	if err != nil {
		t.Fatal(err)
	}
	if walk {
		srv.roots.forceWalk()
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); srv.Close() })
	return &fixture{t: t, root: root, srv: srv, http: ts}
}

func (f *fixture) write(name string, size int) (string, []byte) {
	f.t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*31 + i/7)
	}
	path := filepath.Join(f.root, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path, data
}

func (f *fixture) call(method, route, token string, body any) (int, []byte) {
	f.t.Helper()
	var rdr *bytes.Reader
	switch b := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			f.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.http.URL+route, rdr)
	if err != nil {
		f.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func (f *fixture) errCode(raw []byte) string {
	var e struct{ Error *Error }
	if err := json.Unmarshal(raw, &e); err != nil || e.Error == nil {
		f.t.Fatalf("not an error body: %s", raw)
	}
	return e.Error.Code
}

func (f *fixture) startHash(path string) Job {
	f.t.Helper()
	status, raw := f.call("POST", "/v1/hash", testToken, PathRequest{Path: path})
	if status != http.StatusAccepted {
		f.t.Fatalf("start hash: %d %s", status, raw)
	}
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) waitJob(id string, states ...string) Job {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, raw := f.call("GET", "/v1/jobs/"+id, testToken, nil)
		var j Job
		if status != http.StatusOK || json.Unmarshal(raw, &j) != nil {
			f.t.Fatalf("get job: %d %s", status, raw)
		}
		for _, s := range states {
			if j.State == s {
				return j
			}
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("job %s stuck in %s, wanted %v", id, j.State, states)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRequestsNeedTheToken(t *testing.T) {
	f := newFixture(t, 2)
	for _, token := range []string{"", "wrong", testToken + "x", testToken[:len(testToken)-1]} {
		status, raw := f.call("GET", "/v1/health", token, nil)
		if status != http.StatusUnauthorized || f.errCode(raw) != CodeAuth {
			t.Errorf("token %q: %d %s", token, status, raw)
		}
	}
	status, raw := f.call("GET", "/v1/health", testToken, nil)
	var h Health
	if status != http.StatusOK || json.Unmarshal(raw, &h) != nil || h.Version != Version || h.InstanceID != f.srv.InstanceID() || len(h.Roots) != 1 {
		t.Fatalf("health: %d %s", status, raw)
	}
}

func TestAShortTokenIsRefused(t *testing.T) {
	if _, err := NewServer(Config{Roots: []string{t.TempDir()}, Token: "short"}); err == nil {
		t.Fatal("a short token must be refused")
	}
}

func TestHashMatchesTheFile(t *testing.T) {
	for _, walk := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "walk"}[walk], func(t *testing.T) {
			checkHashMatches(t, newFixtureWith(t, 2, walk))
		})
	}
}

func checkHashMatches(t *testing.T, f *fixture) {
	path, data := f.write("a.bin", 3*hashChunk+17)
	j := f.waitJob(f.startHash(path).ID, JobDone, JobFailed)
	sum := sha256.Sum256(data)
	if j.State != JobDone || j.Result == nil || j.Result.SHA256 != hex.EncodeToString(sum[:]) || j.Result.Bytes != int64(len(data)) {
		t.Fatalf("job: %+v %+v", j, j.Error)
	}
	if j.BytesRead != int64(len(data)) || j.TotalBytes != int64(len(data)) || j.InstanceID != f.srv.InstanceID() {
		t.Fatalf("progress or instance: %+v", j)
	}
	info, _ := os.Stat(path)
	if j.Result.Identity.Size != int64(len(data)) || j.Result.Identity.MtimeNs != info.ModTime().UnixNano() || j.Result.Identity.Ino == 0 {
		t.Fatalf("identity: %+v", j.Result.Identity)
	}
}

func TestSampleMatchesTheSharedDigest(t *testing.T) {
	f := newFixture(t, 2)
	path, data := f.write("big.bin", sampledigest.Chunk*10+9)
	status, raw := f.call("POST", "/v1/sample", testToken, PathRequest{Path: path})
	var s SampleResponse
	if status != http.StatusOK || json.Unmarshal(raw, &s) != nil {
		t.Fatalf("sample: %d %s", status, raw)
	}
	want, err := sampledigest.Digest(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil || s.Digest != want {
		t.Fatalf("sample digest %s, want %s (%v)", s.Digest, want, err)
	}
}

func TestIdentityReportsTheFile(t *testing.T) {
	f := newFixture(t, 2)
	path, data := f.write("a.bin", 1234)
	status, raw := f.call("POST", "/v1/identity", testToken, PathRequest{Path: path})
	var r IdentityResponse
	if status != http.StatusOK || json.Unmarshal(raw, &r) != nil || r.Identity.Size != int64(len(data)) || r.InstanceID != f.srv.InstanceID() {
		t.Fatalf("identity: %d %s", status, raw)
	}
}

// The API has no way to be told what a hash should be, so it cannot echo a
// caller's expectation back as a verdict.
func TestTheAPIRefusesAnExpectedValue(t *testing.T) {
	f := newFixture(t, 2)
	path, _ := f.write("a.bin", 10)
	for _, route := range []string{"/v1/identity", "/v1/hash", "/v1/sample"} {
		for _, body := range []string{
			`{"path":"` + path + `","expected":"abc"}`,
			`{"path":"` + path + `","expect":{"size":"10"}}`,
			`{"path":"` + path + `"} {"path":"x"}`,
			`{"path":"` + path + `","PATH":"/etc/passwd"}`,
			`{"Path":"` + path + `"}`,
			`{"path":"` + path + `","path":"/etc/passwd"}`,
			`not json`,
			`{}`,
		} {
			status, raw := f.call("POST", route, testToken, body)
			if status != http.StatusBadRequest || f.errCode(raw) != CodeBadInput {
				t.Errorf("%s %s: %d %s", route, body, status, raw)
			}
		}
	}
}

func TestPathsOutsideTheRootsAreRefused(t *testing.T) {
	for _, walk := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "walk"}[walk], func(t *testing.T) {
			checkPathsOutsideTheRoots(t, newFixtureWith(t, 2, walk))
		})
	}
}

func checkPathsOutsideTheRoots(t *testing.T, f *fixture) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.bin")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside, _ := f.write("a.bin", 10)
	if err := os.Symlink(secret, filepath.Join(f.root, "link.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.root, "dir-link")); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"absolute outside":     secret,
		"dot dot out":          filepath.Join(f.root, "..", filepath.Base(outside), "secret.bin"),
		"symlink to a file":    filepath.Join(f.root, "link.bin"),
		"through a symlink":    filepath.Join(f.root, "dir-link", "secret.bin"),
		"relative":             "a.bin",
		"prefix of the root":   f.root + "-other/a.bin",
		"the root itself":      f.root,
		"inside but not there": filepath.Join(f.root, "missing.bin"),
		"through a file":       filepath.Join(inside, "x"),
		"missing parent":       filepath.Join(f.root, "no-dir", "x.bin"),
	} {
		status, raw := f.call("POST", "/v1/identity", testToken, PathRequest{Path: path})
		code := f.errCode(raw)
		switch name {
		case "inside but not there", "through a file", "missing parent":
			if status != http.StatusNotFound || code != CodeNotFound {
				t.Errorf("%s: %d %s", name, status, raw)
			}
		case "the root itself":
			if status != http.StatusUnprocessableEntity || code != CodeNotFile {
				t.Errorf("%s: %d %s", name, status, raw)
			}
		default:
			if status != http.StatusForbidden || code != CodeOutside {
				t.Errorf("%s: %d %s", name, status, raw)
			}
		}
	}
	if status, raw := f.call("POST", "/v1/identity", testToken, PathRequest{Path: inside}); status != http.StatusOK {
		t.Fatalf("a file inside the root must work: %d %s", status, raw)
	}
}

func TestADirectoryIsNotAFile(t *testing.T) {
	f := newFixture(t, 2)
	dir := filepath.Join(f.root, "sub")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	status, raw := f.call("POST", "/v1/identity", testToken, PathRequest{Path: dir})
	if status != http.StatusUnprocessableEntity || f.errCode(raw) != CodeNotFile {
		t.Fatalf("%d %s", status, raw)
	}
}

func TestAFileChangedWhileHashingFails(t *testing.T) {
	f := newFixture(t, 2)
	path, _ := f.write("a.bin", 3*hashChunk)
	var once sync.Once
	f.srv.afterChunk = func(p string) {
		once.Do(func() {
			fh, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Error(err)
				return
			}
			fh.Write([]byte("more"))
			fh.Close()
		})
	}
	j := f.waitJob(f.startHash(path).ID, JobDone, JobFailed)
	if j.State != JobFailed || j.Error == nil || j.Error.Code != CodeChanged || j.Result != nil {
		t.Fatalf("a file that grew while it was hashed must not produce a digest: %+v %+v", j, j.Error)
	}
}

func TestAFileReplacedAtItsPathWhileHashingFails(t *testing.T) {
	f := newFixture(t, 2)
	path, _ := f.write("a.bin", 3*hashChunk)
	var once sync.Once
	f.srv.afterChunk = func(p string) {
		once.Do(func() {
			other := filepath.Join(filepath.Dir(p), "other.bin")
			if err := os.WriteFile(other, bytes.Repeat([]byte("z"), 3*hashChunk), 0o600); err != nil {
				t.Error(err)
				return
			}
			if err := os.Rename(other, p); err != nil {
				t.Error(err)
			}
		})
	}
	j := f.waitJob(f.startHash(path).ID, JobDone, JobFailed)
	if j.State != JobFailed || j.Error == nil || j.Error.Code != CodeChanged {
		t.Fatalf("the path names another file now: %+v %+v", j, j.Error)
	}
}

func TestACancelledJobStopsReading(t *testing.T) {
	f := newFixture(t, 2)
	path, _ := f.write("a.bin", 5*hashChunk)
	hold, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.srv.afterChunk = func(string) {
		once.Do(func() { close(hold); <-release })
	}
	job := f.startHash(path)
	<-hold
	if status, raw := f.call("DELETE", "/v1/jobs/"+job.ID, testToken, nil); status != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", status, raw)
	}
	close(release)
	j := f.waitJob(job.ID, JobCancelled, JobDone, JobFailed)
	if j.State != JobCancelled || j.Result != nil || j.BytesRead >= 5*hashChunk {
		t.Fatalf("a cancelled job must stop early: %+v", j)
	}
}

func TestHashesBeyondTheLimitQueue(t *testing.T) {
	f := newFixture(t, 1)
	a, _ := f.write("a.bin", 3*hashChunk)
	b, _ := f.write("b.bin", 3*hashChunk)
	hold, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.srv.afterChunk = func(string) { once.Do(func() { close(hold); <-release }) }
	first := f.startHash(a)
	<-hold
	second := f.startHash(b)
	time.Sleep(50 * time.Millisecond)
	if j := f.waitJob(second.ID, JobQueued, JobRunning, JobDone); j.State != JobQueued {
		t.Fatalf("with one slot taken the second hash must wait, got %s", j.State)
	}
	close(release)
	if j := f.waitJob(first.ID, JobDone, JobFailed); j.State != JobDone {
		t.Fatalf("first: %+v", j)
	}
	if j := f.waitJob(second.ID, JobDone, JobFailed); j.State != JobDone {
		t.Fatalf("second: %+v", j)
	}
}

func TestUnknownJobsAreNotFound(t *testing.T) {
	f := newFixture(t, 2)
	for _, method := range []string{"GET", "DELETE"} {
		status, raw := f.call(method, "/v1/jobs/nope", testToken, nil)
		if status != http.StatusNotFound || f.errCode(raw) != CodeNotFound {
			t.Errorf("%s: %d %s", method, status, raw)
		}
	}
}

func TestFinishedJobsAreForgottenAfterTheRetention(t *testing.T) {
	f := newFixture(t, 2)
	// The clock is set before any request, so no handler races with the change.
	var skew atomic.Int64
	f.srv.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	path, _ := f.write("a.bin", 100)
	job := f.waitJob(f.startHash(path).ID, JobDone)
	skew.Store(int64(jobRetention + time.Minute))
	// Any new job sweeps the old ones.
	f.waitJob(f.startHash(path).ID, JobDone)
	if status, _ := f.call("GET", "/v1/jobs/"+job.ID, testToken, nil); status != http.StatusNotFound {
		t.Fatalf("an old finished job should be gone, got %d", status)
	}
}

func TestEveryOutputKeepsPathsOutOfTheLogs(t *testing.T) {
	var logged []string
	var mu sync.Mutex
	root, _ := filepath.EvalSymlinks(t.TempDir())
	srv, err := NewServer(Config{Roots: []string{root}, Token: testToken, Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, strings.TrimSpace(format))
		for _, a := range args {
			if s, ok := a.(string); ok {
				logged = append(logged, s)
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	secret := filepath.Join(root, "very-private-name.mkv")
	os.WriteFile(secret, []byte("x"), 0o600)
	req, _ := http.NewRequest("POST", ts.URL+"/v1/identity", strings.NewReader(`{"path":"`+secret+`"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logged {
		if strings.Contains(line, "very-private-name") {
			t.Fatalf("a path reached the log: %q", line)
		}
	}
}

// A directory renamed above the file changes what the path names without touching
// the open file, so only the path check can notice it.
func TestADirectoryRenamedAboveTheFileFailsTheHash(t *testing.T) {
	f := newFixture(t, 2)
	dir := filepath.Join(f.root, "show")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 3*hashChunk), 0o600); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	f.srv.afterChunk = func(string) {
		once.Do(func() {
			if err := os.Rename(dir, filepath.Join(f.root, "show-old")); err != nil {
				t.Error(err)
				return
			}
			os.Mkdir(dir, 0o700)
			os.WriteFile(path, bytes.Repeat([]byte("b"), 3*hashChunk), 0o600)
		})
	}
	j := f.waitJob(f.startHash(path).ID, JobDone, JobFailed)
	if j.State != JobFailed || j.Error == nil || j.Error.Code != CodeChanged {
		t.Fatalf("the path names another file now: %+v %+v", j, j.Error)
	}
}

func TestJobsWaitingOrRunningAreCapped(t *testing.T) {
	f := newFixture(t, 1)
	path, _ := f.write("a.bin", 2*hashChunk)
	hold, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.srv.afterChunk = func(string) { once.Do(func() { close(hold); <-release }) }
	defer close(release)
	f.startHash(path)
	<-hold
	for i := 1; i < maxActiveJobs; i++ {
		f.startHash(path)
	}
	status, raw := f.call("POST", "/v1/hash", testToken, PathRequest{Path: path})
	if status != http.StatusTooManyRequests || f.errCode(raw) != CodeBusy {
		t.Fatalf("job %d beyond the cap: %d %s", maxActiveJobs+1, status, raw)
	}
}

func TestIdentityTellsWhetherItIsComplete(t *testing.T) {
	f := newFixture(t, 2)
	path, _ := f.write("a.bin", 10)
	_, raw := f.call("POST", "/v1/identity", testToken, PathRequest{Path: path})
	if !strings.Contains(string(raw), `"complete":`) {
		t.Fatalf("the answer must say whether the identity is complete: %s", raw)
	}
}

func TestHealthNamesTheResolver(t *testing.T) {
	for _, walk := range []bool{false, true} {
		f := newFixtureWith(t, 2, walk)
		_, raw := f.call("GET", "/v1/health", testToken, nil)
		var h Health
		if err := json.Unmarshal(raw, &h); err != nil || h.Resolver == "" {
			t.Fatalf("health: %s", raw)
		}
		if walk && h.Resolver != "walk" && h.Resolver != "dev" {
			t.Fatalf("a forced walk must report it: %s", raw)
		}
	}
}

func TestRequestsAfterCloseAreRefused(t *testing.T) {
	f := newFixture(t, 2)
	path, _ := f.write("a.bin", 10)
	f.srv.Close()
	status, raw := f.call("POST", "/v1/identity", testToken, PathRequest{Path: path})
	if status == http.StatusOK {
		t.Fatalf("closed server answered: %s", raw)
	}
	status, _ = f.call("POST", "/v1/hash", testToken, PathRequest{Path: path})
	if status != http.StatusTooManyRequests {
		t.Fatalf("a closed server must not start jobs, got %d", status)
	}
}
