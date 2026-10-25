package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/internal/nashelper"
)

const helperTestToken = "0123456789abcdef0123456789abcdef"

type helperRig struct {
	engine *Engine
	root   string
	path   string
	sum    string
	reads  *readCounter
	hits   atomic.Int64
	ts     *httptest.Server
}

// newHelperRig serves root through a real helper; remote is the directory the
// helper is told the mount's files live in (root when empty).
func newHelperRig(t *testing.T, remote string, content []byte) *helperRig {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if remote == "" {
		remote = root
	}
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	if remote, err = filepath.EvalSymlinks(remote); err != nil {
		t.Fatal(err)
	}
	if content == nil {
		content = []byte(strings.Repeat("media-bytes-", 1000))
	}
	path := filepath.Join(root, "media.mkv")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := nashelper.NewServer(nashelper.Config{Roots: []string{remote}, Token: helperTestToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	rig := &helperRig{root: root, path: path}
	rig.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.hits.Add(1)
		srv.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(rig.ts.Close)
	sum := sha256.Sum256(content)
	rig.sum = hex.EncodeToString(sum[:])
	rig.engine = rigEngine(t, root, rig.ts.URL, remote)
	rig.reads = countReads(rig.engine)
	return rig
}

func rigEngine(t *testing.T, root, url, remote string) *Engine {
	t.Helper()
	resolver, err := fsop.NewResolver([]string{root}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	client, err := nashelper.NewClient(nashelper.ClientConfig{
		URL: url, Token: helperTestToken,
		Mappings: []nashelper.PathMapping{{Local: root, Remote: remote}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewEngine(EngineDeps{Fs: resolver, NASHelper: client})
}

func readRecord(t *testing.T, ec *ExecutionContext, path string) map[string]any {
	t.Helper()
	reads, _ := ec.State["integrity_reads"].(map[string]any)
	rec, _ := reads[path].(map[string]any)
	if rec == nil {
		t.Fatalf("no integrity read recorded for %s: %+v", path, ec.State)
	}
	return rec
}

func TestPromotionHashAsksTheHelperAndReadsNothingThroughTheMount(t *testing.T) {
	rig := newHelperRig(t, "", nil)
	ec := &ExecutionContext{State: map[string]any{}}
	ctx := withPromotionVerifiedFiles(context.Background(), ec)
	var lastRead, lastTotal int64
	ctx = context.WithValue(ctx, promotionHashProgressKey{}, promotionHashProgress(func(_ string, read, total int64) error {
		lastRead, lastTotal = read, total
		return nil
	}))
	sum, n, err := rig.engine.promotionHash(ctx, rig.path)
	if err != nil {
		t.Fatal(err)
	}
	if sum != rig.sum || n != int64(len(strings.Repeat("media-bytes-", 1000))) {
		t.Fatalf("hash %s (%d bytes), want %s", sum, n, rig.sum)
	}
	// Only the sampled ranges cross the mount; the hash loop does not run.
	if got := rig.reads.of(rig.path); got != 0 {
		t.Fatalf("%d bytes were hashed through the mount", got)
	}
	if lastRead != n || lastTotal != n {
		t.Fatalf("progress ended at %d/%d, want %d/%d", lastRead, lastTotal, n, n)
	}
	if rec := readRecord(t, ec, rig.path); rec["via"] != "nas_helper" {
		t.Fatalf("record %+v", rec)
	}
	// The verified-file cache still works on top of it.
	if err := rig.engine.verifyPromotionHash(ctx, rig.path, rig.sum); err != nil {
		t.Fatal(err)
	}
	hits := rig.hits.Load()
	if err := rig.engine.verifyPromotionHash(ctx, rig.path, rig.sum); err != nil {
		t.Fatal(err)
	}
	if rig.hits.Load() != hits {
		t.Fatal("an unchanged, already verified file went back to the helper")
	}
}

func TestPromotionHashRecordSurvivesFullReadContext(t *testing.T) {
	rig := newHelperRig(t, "", nil)
	ec := &ExecutionContext{State: map[string]any{}}
	ctx := withoutPromotionVerifiedFiles(withPromotionVerifiedFiles(context.Background(), ec))
	if _, _, err := rig.engine.promotionHash(ctx, rig.path); err != nil {
		t.Fatal(err)
	}
	if rec := readRecord(t, ec, rig.path); rec["via"] != "nas_helper" {
		t.Fatalf("record %+v", rec)
	}
}

func TestPromotionHashFallsBackToTheMountWhenTheHelperIsDown(t *testing.T) {
	rig := newHelperRig(t, "", nil)
	rig.ts.Close()
	ec := &ExecutionContext{State: map[string]any{}}
	ctx := withPromotionVerifiedFiles(context.Background(), ec)
	sum, _, err := rig.engine.promotionHash(ctx, rig.path)
	if err != nil || sum != rig.sum {
		t.Fatalf("hash %s, %v", sum, err)
	}
	if rig.reads.of(rig.path) == 0 {
		t.Fatal("the fallback did not read through the mount")
	}
	rec := readRecord(t, ec, rig.path)
	if rec["via"] != "smb_fallback" || rec["reason"] != "helper_unavailable" {
		t.Fatalf("record %+v", rec)
	}
	// The next file does not pay the timeout again.
	if !rig.engine.helperBreaker.open(time.Now()) {
		t.Fatal("an unreachable helper did not open the breaker")
	}
	ec2 := &ExecutionContext{State: map[string]any{}}
	if _, _, err := rig.engine.promotionHash(withPromotionVerifiedFiles(context.Background(), ec2), rig.path); err != nil {
		t.Fatal(err)
	}
	if rec := readRecord(t, ec2, rig.path); rec["reason"] != "helper_down" {
		t.Fatalf("record %+v", rec)
	}
}

func TestPromotionHashReadsUnmappedPathsThroughTheMount(t *testing.T) {
	rig := newHelperRig(t, "", nil)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "elsewhere.mkv")
	if err := os.WriteFile(other, []byte("not under any mapping"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The resolver must allow it for the hash to run at all.
	resolver, err := fsop.NewResolver([]string{rig.root, filepath.Dir(other)}, []string{rig.root})
	if err != nil {
		t.Fatal(err)
	}
	rig.engine.deps.Fs = resolver
	hits := rig.hits.Load()
	if _, _, err := rig.engine.promotionHash(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if rig.hits.Load() != hits {
		t.Fatal("a path outside every mapping was sent to the helper")
	}
}

func TestPromotionHashRefusesAMappingThatPointsAtOtherBytes(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote")
	rig := newHelperRig(t, remote, nil)
	// Same name, same size, different bytes under the helper's view.
	other := []byte(strings.Repeat("other-bytes-", 1000))
	if err := os.WriteFile(filepath.Join(remote, "media.mkv"), other, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := rig.engine.promotionHash(context.Background(), rig.path)
	var disagree *promotionHelperDisagreeError
	if !errors.As(err, &disagree) {
		t.Fatalf("got %v, want a disagreement", err)
	}
	if transientPromotionHashError(err) {
		t.Fatal("a disagreement must not be retried as a changing file")
	}
}

func TestPromotionHashReadsThroughTheMountWhenTheHelperDoesNotSeeTheFile(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote")
	rig := newHelperRig(t, remote, nil)
	ec := &ExecutionContext{State: map[string]any{}}
	sum, _, err := rig.engine.promotionHash(withPromotionVerifiedFiles(context.Background(), ec), rig.path)
	if err != nil || sum != rig.sum {
		t.Fatalf("hash %s, %v", sum, err)
	}
	if rec := readRecord(t, ec, rig.path); rec["reason"] != "helper_disagrees" {
		t.Fatalf("record %+v", rec)
	}
	if rig.engine.helperBreaker.open(time.Now()) {
		t.Fatal("a helper that answered must not open the breaker")
	}
}

func TestPromotionHashStopsWhenTheObserverStops(t *testing.T) {
	rig := newHelperRig(t, "", nil)
	stop := errors.New("operator cancelled")
	ctx := context.WithValue(context.Background(), promotionHashProgressKey{}, promotionHashProgress(func(_ string, read, _ int64) error {
		if read > 0 {
			return stop
		}
		return nil
	}))
	if _, _, err := rig.engine.promotionHash(ctx, rig.path); !errors.Is(err, stop) {
		t.Fatalf("got %v, want the observer's error", err)
	}
}

func TestPromotionHashFallsBackWhenTheHelperStalls(t *testing.T) {
	old := helperStallAfter
	helperStallAfter = 150 * time.Millisecond
	t.Cleanup(func() { helperStallAfter = old })

	var cancels atomic.Int64
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"j1","instance_id":"i1","state":"queued","bytes_read":"0","total_bytes":"0"}`))
		case r.Method == http.MethodDelete:
			cancels.Add(1)
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{"id":"j1","instance_id":"i1","state":"queued","bytes_read":"0","total_bytes":"0"}`))
		}
	}))
	t.Cleanup(stuck.Close)
	rig := newHelperRig(t, "", nil)
	rig.engine = rigEngine(t, rig.root, stuck.URL, rig.root)
	rig.reads = countReads(rig.engine)
	ec := &ExecutionContext{State: map[string]any{}}
	sum, _, err := rig.engine.promotionHash(withPromotionVerifiedFiles(context.Background(), ec), rig.path)
	if err != nil || sum != rig.sum {
		t.Fatalf("hash %s, %v", sum, err)
	}
	if rec := readRecord(t, ec, rig.path); rec["reason"] != "stalled" {
		t.Fatalf("record %+v", rec)
	}
	if cancels.Load() == 0 {
		t.Fatal("the stalled job was left running on the helper")
	}
}
