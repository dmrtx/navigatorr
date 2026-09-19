package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// Transcode I/O optimization tests: content-addressed shared source cache,
// local digest verification, job-local hard-link leases that survive eviction,
// worker-local full validation before publish, lightweight post-publish
// verification, sidecar sweep correctness, and crash/resume/cancel safety.

func ioOptSeed(t *testing.T, stateDir string, job *JobRecord) {
	t.Helper()
	if err := SaveJobAtomic(filepath.Join(stateDir, job.ID, "job.json"), job); err != nil {
		t.Fatal(err)
	}
}

func ioOptPlan(t *testing.T) *transcode.Plan {
	t.Helper()
	p, err := ResolveWorkerPlan("hevc-vt", nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ioOptSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ioOptWriteFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func ioOptCacheDataFiles(t *testing.T, cacheDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".meta.json") || strings.HasSuffix(name, ".lock") {
			continue
		}
		out = append(out, filepath.Join(cacheDir, name))
	}
	return out
}

// ioOptCountingStore is an unmounted external media store that counts downloads.
type ioOptCountingStore struct {
	root      string
	content   []byte
	downloads int
}

func (s *ioOptCountingStore) Maps(name string) bool {
	clean := filepath.Clean(name)
	return clean != filepath.Clean(s.root) && strings.HasPrefix(clean, filepath.Clean(s.root)+string(filepath.Separator))
}

func (s *ioOptCountingStore) Stat(context.Context, string) (os.FileInfo, error) {
	return directInfo{size: int64(len(s.content))}, nil
}

func (s *ioOptCountingStore) DownloadAtomic(_ context.Context, _ string, local string) error {
	s.downloads++
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	return os.WriteFile(local, s.content, 0o600)
}

func (s *ioOptCountingStore) Publish(context.Context, string, string, string) error { return nil }
func (s *ioOptCountingStore) CheckRoot(context.Context) error                       { return nil }

// TestIOOpt_CacheDigestMismatchNeverPublished proves a local copy whose bytes do
// not match the coordinator-supplied SHA-256 is never published into the cache,
// and that the correct digest does publish.
func TestIOOpt_CacheDigestMismatchNeverPublished(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	src := ioOptWriteFile(t, filepath.Join(ext, "movie.mkv"), "content-A")
	real := ioOptSHA(t, src)
	wrong := strings.Repeat("a", 64)
	cfg := pr6b2Config(dir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{ext}
		c.StagingPolicy = StagingPolicyAlways
	})
	w := NewWorker(cfg)
	ctx := context.Background()

	if _, err := w.ensureSourceCached(ctx, filepath.Clean(src), wrong); err == nil {
		t.Fatal("expected digest mismatch to fail, cache entry published anyway")
	}
	if files := ioOptCacheDataFiles(t, w.sourceCacheDir()); len(files) != 0 {
		t.Fatalf("mismatched digest published cache data: %v", files)
	}

	local := ioOptWriteFile(t, filepath.Join(dir, "local.mkv"), "content-A")
	fi, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := populateSourceCacheFromLocal(ctx, w.sourceCacheDir(), filepath.Clean(src), fi, local, wrong); err == nil {
		t.Fatal("expected populateSourceCacheFromLocal digest mismatch to fail")
	}
	if files := ioOptCacheDataFiles(t, w.sourceCacheDir()); len(files) != 0 {
		t.Fatalf("mismatched populate published cache data: %v", files)
	}

	cached, err := w.ensureSourceCached(ctx, filepath.Clean(src), real)
	if err != nil {
		t.Fatalf("correct digest must publish: %v", err)
	}
	if files := ioOptCacheDataFiles(t, w.sourceCacheDir()); len(files) != 1 {
		t.Fatalf("expected exactly one cache entry, got %v", files)
	}
	if fi, err := os.Stat(cached); err != nil || fi.Size() != int64(len("content-A")) {
		t.Fatalf("cached entry invalid: %v", err)
	}
}

// TestIOOpt_ChangedContentSameMetadataDifferentSHAMisses proves that changing a
// source's content while preserving size and mtime is a cache miss for the new
// content identity, so stale bytes are never reused.
func TestIOOpt_ChangedContentSameMetadataDifferentSHAMisses(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	src := ioOptWriteFile(t, filepath.Join(ext, "movie.mkv"), "content-AAAA")
	shaA := ioOptSHA(t, src)
	cfg := pr6b2Config(dir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{ext}
		c.StagingPolicy = StagingPolicyAlways
	})
	w := NewWorker(cfg)
	ctx := context.Background()
	cachedA, err := w.ensureSourceCached(ctx, filepath.Clean(src), shaA)
	if err != nil {
		t.Fatal(err)
	}
	if hit, ok := lookupSourceCache(w.sourceCacheDir(), filepath.Clean(src), mustStat(t, src), shaA); !ok || hit != cachedA {
		t.Fatalf("expected initial hit %q, got %q ok=%v", cachedA, hit, ok)
	}

	orig, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	// Same length, different content, restored mtime => metadata identical.
	ioOptWriteFile(t, src, "content-BBBB")
	if err := os.Chtimes(src, orig.ModTime(), orig.ModTime()); err != nil {
		t.Fatal(err)
	}
	shaB := ioOptSHA(t, src)
	if shaA == shaB {
		t.Fatal("test setup: digests must differ")
	}
	if hit, ok := lookupSourceCache(w.sourceCacheDir(), filepath.Clean(src), mustStat(t, src), shaB); ok {
		t.Fatalf("changed content reused stale cache entry %q", hit)
	}
	cachedB, err := w.ensureSourceCached(ctx, filepath.Clean(src), shaB)
	if err != nil {
		t.Fatalf("re-populate for changed content: %v", err)
	}
	if cachedB == cachedA {
		t.Fatal("changed content must map to a distinct cache entry")
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// TestIOOpt_HardLinkLeaseCausesZeroSecondTransfer proves that once the shared
// cache is populated, giving a benchmark/transcode a job-local path performs no
// second NAS download and yields a hard link to the cached inode.
func TestIOOpt_HardLinkLeaseCausesZeroSecondTransfer(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "unmounted-nas")
	src := filepath.Join(root, "movie.mkv")
	content := []byte("nas-source-bytes")
	sha := func() string { s := sha256.Sum256(content); return hex.EncodeToString(s[:]) }()
	store := &ioOptCountingStore{root: root, content: content}
	w := NewWorker(&WorkerConfig{
		StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{root},
		ExternalRoots: []string{root}, LocalWorkDir: filepath.Join(dir, "work"),
		StagingPolicy: StagingPolicyAlways, MaxParallelJobs: 1,
	})
	w.mediaStore = store
	ctx := context.Background()

	cached, err := w.ensureSourceCached(ctx, filepath.Clean(src), sha)
	if err != nil {
		t.Fatal(err)
	}
	if store.downloads != 1 {
		t.Fatalf("downloads=%d want 1 after cache population", store.downloads)
	}
	cachedInfo, err := os.Stat(cached)
	if err != nil {
		t.Fatal(err)
	}

	for i, id := range []string{"job-link-1", "job-link-2"} {
		staged := filepath.Join(w.localWorkDir(), id, "input.mkv")
		lease, err := w.acquireCachedSourceLease(ctx, cached, staged)
		if err != nil {
			t.Fatalf("job %d acquireCachedSourceLease: %v", i, err)
		}
		li, err := os.Stat(lease)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(cachedInfo, li) {
			t.Fatalf("job %d lease is not a hard link to the cache entry", i)
		}
	}
	if store.downloads != 1 {
		t.Fatalf("downloads=%d want 1: hard-link reuse must cause zero second NAS transfer", store.downloads)
	}
}

// TestIOOpt_ActiveTranscodeSurvivesCacheEviction proves an in-flight transcode's
// job-local link remains readable after the shared cache names are evicted.
func TestIOOpt_ActiveTranscodeSurvivesCacheEviction(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	content := "immutable-source-bytes"
	src := ioOptWriteFile(t, filepath.Join(ext, "movie.mkv"), content)
	sha := ioOptSHA(t, src)
	work := filepath.Join(dir, "work")
	cfg := pr6b2Config(dir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{ext}
		c.StagingPolicy = StagingPolicyAlways
		c.LocalWorkDir = work
	})
	w := NewWorker(cfg)
	ctx := context.Background()
	cached, err := w.ensureSourceCached(ctx, filepath.Clean(src), sha)
	if err != nil {
		t.Fatal(err)
	}

	const id = "job-evict"
	staged := filepath.Join(work, id, "input.mkv")
	job := &JobRecord{
		ID: id, Status: "running", Source: src, Candidate: filepath.Join(ext, "out.mkv"),
		Profile: "hevc-vt", Plan: ioOptPlan(t), ExecutionSpecDigest: pr5Digest,
		SourceSHA256: sha, StagingPolicy: string(StagingPolicyAlways),
		StagingState: string(StagingStatePending), EffectiveInputPath: staged, StagedInputPath: staged,
		FinalizationState: string(FinalizationStateNotRequired), CreatedAt: time.Now().UTC(),
	}
	ioOptSeed(t, cfg.StateDir, job)
	r, err := resolveOperationalForExecution(job)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled, err := w.ensureStaged(ctx, filepath.Join(cfg.StateDir, id), filepath.Join(cfg.StateDir, id, "job.json"), job, r); err != nil || cancelled {
		t.Fatalf("ensureStaged: cancelled=%v err=%v", cancelled, err)
	}
	if w.isCachePath(r.stagedInput) {
		t.Fatal("staged input must be job-local, not the shared cache path")
	}

	// Evict the shared cache entry; the active job's link must be unaffected.
	if err := os.Remove(cached); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(cached + ".meta.json")
	b, err := os.ReadFile(r.stagedInput)
	if err != nil {
		t.Fatalf("active job's staged link broke after eviction: %v", err)
	}
	if string(b) != content {
		t.Fatalf("staged bytes %q != %q", b, content)
	}
}

// ioOptEvictRunner deletes shared cache entries mid-run and then reads the
// benchmark's effective source, proving the benchmark ran against its own lease.
type ioOptEvictRunner struct {
	cacheDir  string
	want      string
	found     bool
	usedLease bool
}

func (r *ioOptEvictRunner) RunBenchmark(ctx context.Context, w *Worker, record *BenchmarkRecord) error {
	entries, err := os.ReadDir(r.cacheDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".meta.json") || strings.HasSuffix(name, ".lock") {
			continue
		}
		r.found = true
		if err := os.Remove(filepath.Join(r.cacheDir, name)); err != nil {
			return err
		}
	}
	if !r.found {
		return fmt.Errorf("no shared cache entry to evict")
	}
	if w.isCachePath(record.Source) {
		return fmt.Errorf("benchmark ran directly against the shared cache path %q", record.Source)
	}
	b, err := os.ReadFile(record.Source)
	if err != nil {
		return fmt.Errorf("benchmark lease unreadable after eviction: %w", err)
	}
	if string(b) != r.want {
		return fmt.Errorf("benchmark lease content %q != %q", b, r.want)
	}
	r.usedLease = true
	return nil
}

// TestIOOpt_ActiveBenchmarkSurvivesCacheEviction proves the benchmark acquires a
// job-local lease and keeps running after the shared cache names are unlinked.
func TestIOOpt_ActiveBenchmarkSurvivesCacheEviction(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	content := "benchmark-source-bytes"
	src := ioOptWriteFile(t, filepath.Join(ext, "movie.mkv"), content)
	sha := ioOptSHA(t, src)
	cfg := pr6b2Config(dir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{ext}
		c.StagingPolicy = StagingPolicyAlways
		c.LocalWorkDir = filepath.Join(dir, "work")
	})
	w := NewWorker(cfg)
	const id = "bench-evict-1"
	jobDir := filepath.Join(cfg.StateDir, id)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &BenchmarkRecord{
		ProtocolVersion: transcode.WorkerProtocolVersion, ID: id, Status: "queued",
		Source: src, SourceSHA256: sha, EffectiveSource: filepath.Join(jobDir, "source.mkv"),
		Metric: "vmaf", RunToken: "run-token-evict",
		Samples:    []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0, DurationSeconds: 1}},
		Candidates: []transcode.BenchmarkCandidate{{ID: "cand-1", VideoProfile: "main", PixelFormat: "yuv420p"}},
		CreatedAt:  time.Now().UTC(),
	}
	if err := SaveBenchmarkAtomic(filepath.Join(jobDir, "benchmark.json"), rec); err != nil {
		t.Fatal(err)
	}
	runner := &ioOptEvictRunner{cacheDir: w.sourceCacheDir(), want: content}
	w.SetBenchmarkRunner(runner)
	if err := w.InternalBenchmark(context.Background(), id, "run-token-evict"); err != nil {
		t.Fatalf("InternalBenchmark: %v", err)
	}
	if !runner.usedLease {
		t.Fatal("benchmark did not run against its job-local lease")
	}
}

// TestIOOpt_SweepRemovesCorrectSidecar proves the sweep removes
// <key>.meta.json (not <key><ext>.meta.json).
func TestIOOpt_SweepRemovesCorrectSidecar(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	src := ioOptWriteFile(t, filepath.Join(ext, "movie.mkv"), "sweep-me")
	sha := ioOptSHA(t, src)
	cfg := pr6b2Config(dir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{ext}
		c.StagingPolicy = StagingPolicyAlways
		c.SourceCacheTTLHours = 1
	})
	w := NewWorker(cfg)
	ctx := context.Background()
	cached, err := w.ensureSourceCached(ctx, filepath.Clean(src), sha)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSuffix(filepath.Base(cached), filepath.Ext(cached))
	metaPath := filepath.Join(w.sourceCacheDir(), key+".meta.json")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("expected sidecar %s: %v", metaPath, err)
	}
	wrongMeta := cached + ".meta.json"
	if err := os.Chtimes(cached, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.sweepSourceCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cached); !os.IsNotExist(err) {
		t.Fatalf("expired data not swept: %v", err)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("correct sidecar %s not swept: %v", metaPath, err)
	}
	if _, err := os.Stat(wrongMeta); !os.IsNotExist(err) {
		t.Fatalf("mis-derived sidecar %s must never exist: %v", wrongMeta, err)
	}
}

// TestIOOpt_BadLocalCandidateNeverPublished covers wrong-codec, bit-depth, and
// subtitle-loss candidates: full validation runs before publish, the job fails
// closed, the destination stays absent, and the local candidate is cleaned.
func TestIOOpt_BadLocalCandidateNeverPublished(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(plan *transcode.Plan, cand string) func(context.Context, string) (SourceProbe, error)
		wantSubst string
	}{
		{
			name: "wrong codec",
			mutate: func(plan *transcode.Plan, cand string) func(context.Context, string) (SourceProbe, error) {
				return func(_ context.Context, path string) (SourceProbe, error) {
					if path == cand {
						return SourceProbe{Streams: []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264", BitDepth: 8}}, DurationSec: 60}, nil
					}
					return SourceProbe{Streams: []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8}}, DurationSec: 60}, nil
				}
			},
			wantSubst: "codec",
		},
		{
			name: "bit depth",
			mutate: func(plan *transcode.Plan, cand string) func(context.Context, string) (SourceProbe, error) {
				plan.ExpectedBitDepth = 10
				plan.VideoProfile = "main10"
				plan.PixelFormat = "p010le"
				return func(_ context.Context, path string) (SourceProbe, error) {
					if path == cand {
						return SourceProbe{Streams: []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8, PixelFormat: "yuv420p"}}, DurationSec: 60}, nil
					}
					return SourceProbe{Streams: []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 10, PixelFormat: "p010le"}}, DurationSec: 60}, nil
				}
			},
			wantSubst: "bit depth",
		},
		{
			name: "subtitle loss",
			mutate: func(plan *transcode.Plan, cand string) func(context.Context, string) (SourceProbe, error) {
				plan.SubtitleActions = append(plan.SubtitleActions, transcode.SubtitleAction{SourceStreamIndex: 1, TypeIndex: 0, SourceCodec: "subrip", Operation: "copy", Codec: "copy"})
				return func(_ context.Context, path string) (SourceProbe, error) {
					if path == cand {
						return SourceProbe{Streams: []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8}}, DurationSec: 60}, nil
					}
					return SourceProbe{Streams: []SourceStream{
						{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8},
						{Index: 1, TypeIndex: 0, Kind: "subtitle", Codec: "subrip", Language: "eng", Disposition: map[string]int{"default": 1}},
					}, DurationSec: 60}, nil
				}
			},
			wantSubst: "subtitle",
		},
		{
			name: "resolution mismatch",
			mutate: func(plan *transcode.Plan, cand string) func(context.Context, string) (SourceProbe, error) {
				return func(_ context.Context, path string) (SourceProbe, error) {
					if path == cand {
						return SourceProbe{Streams: []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8, Width: 1280, Height: 720}}, DurationSec: 60}, nil
					}
					return SourceProbe{Streams: []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8, Width: 1920, Height: 1080}}, DurationSec: 60}, nil
				}
			},
			wantSubst: "resolution",
		},
		{
			name: "duration mismatch",
			mutate: func(plan *transcode.Plan, cand string) func(context.Context, string) (SourceProbe, error) {
				return func(_ context.Context, path string) (SourceProbe, error) {
					if path == cand {
						return SourceProbe{Streams: []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8}}, DurationSec: 200}, nil
					}
					return SourceProbe{Streams: []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8}}, DurationSec: 1420}, nil
				}
			},
			wantSubst: "duration",
		},
		{
			name: "audio loss",
			mutate: func(plan *transcode.Plan, cand string) func(context.Context, string) (SourceProbe, error) {
				return func(_ context.Context, path string) (SourceProbe, error) {
					if path == cand {
						return SourceProbe{Streams: []SourceStream{
							{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8},
							{Index: 1, TypeIndex: 0, Kind: "audio", Codec: "aac", Language: "eng", Channels: 2},
						}, DurationSec: 60}, nil
					}
					return SourceProbe{Streams: []SourceStream{
						{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8},
						{Index: 1, TypeIndex: 0, Kind: "audio", Codec: "flac", Language: "jpn", Channels: 2},
						{Index: 2, TypeIndex: 1, Kind: "audio", Codec: "aac", Language: "eng", Channels: 2},
					}, DurationSec: 60}, nil
				}
			},
			wantSubst: "audio",
		},
		{
			name: "forced disposition loss",
			mutate: func(plan *transcode.Plan, cand string) func(context.Context, string) (SourceProbe, error) {
				plan.SubtitleActions = append(plan.SubtitleActions, transcode.SubtitleAction{SourceStreamIndex: 1, TypeIndex: 0, SourceCodec: "ass", Operation: "copy", Codec: "copy"})
				return func(_ context.Context, path string) (SourceProbe, error) {
					if path == cand {
						return SourceProbe{Streams: []SourceStream{
							{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8},
							{Index: 1, TypeIndex: 0, Kind: "subtitle", Codec: "ass", Language: "eng", Disposition: map[string]int{"forced": 0}},
						}, DurationSec: 60}, nil
					}
					return SourceProbe{Streams: []SourceStream{
						{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc", BitDepth: 8},
						{Index: 1, TypeIndex: 0, Kind: "subtitle", Codec: "ass", Language: "eng", Disposition: map[string]int{"forced": 1}},
					}, DurationSec: 60}, nil
				}
			},
			wantSubst: "disposition",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ext := filepath.Join(dir, "nas")
			src := pr6b1WriteFile(t, filepath.Join(ext, "src.mkv"))
			cand := filepath.Join(ext, "out.mkv")
			cfg := pr6b2Config(dir, func(c *WorkerConfig) { c.ExternalRoots = []string{ext} })
			w := NewWorker(cfg)
			newStubSpawn().install(w)
			plan := ioOptPlan(t)
			probeFn := tc.mutate(plan, cand)
			if d, derr := transcode.DigestPlan(plan); derr == nil {
				plan.PlanDigest = d
			}
			w.SetProbeDetails(probeFn)
			published := 0
			w.SetFinalizeOutput(func(ctx context.Context, local, dest, id string) error {
				published++
				return FinalizeOutputAtomic(ctx, local, dest, id)
			})
			w.SetRunFFmpeg(func(ctx context.Context, _ *ExecutionPlan, _ *JobRecord, _, out, _, _ string) error {
				if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
					return err
				}
				return os.WriteFile(out, []byte("bad-candidate"), 0o644)
			})
			const id = "job-io-bad"
			ioOptSeed(t, cfg.StateDir, &JobRecord{
				ID: id, Status: "queued", Source: src, Candidate: cand, Profile: "hevc-vt",
				Plan: plan, ExecutionSpecDigest: pr5Digest, CreatedAt: time.Now().UTC(),
			})
			err := w.InternalRun(context.Background(), id)
			if err == nil {
				t.Fatal("expected pre-publish validation failure")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.wantSubst) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSubst)
			}
			if published != 0 {
				t.Fatalf("bad candidate was published %d times, want 0", published)
			}
			if _, err := os.Stat(cand); !os.IsNotExist(err) {
				t.Fatalf("destination must stay absent, stat=%v", err)
			}
			got, err := LoadJob(filepath.Join(cfg.StateDir, id, "job.json"))
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "failed" {
				t.Fatalf("status=%q want failed", got.Status)
			}
		})
	}
}

// Successful local validation publishes exactly once and cleans worker-owned
// local artifacts.
func TestIOOpt_SuccessPublishesExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	src := pr6b1WriteFile(t, filepath.Join(ext, "src.mkv"))
	cand := filepath.Join(ext, "out.mkv")
	cfg := pr6b2Config(dir, func(c *WorkerConfig) { c.ExternalRoots = []string{ext} })
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)
	publishes := 0
	inner := w.finalizeOutput
	w.SetFinalizeOutput(func(ctx context.Context, local, dest, id string) error {
		publishes++
		if inner != nil {
			return inner(ctx, local, dest, id)
		}
		return FinalizeOutputAtomic(ctx, local, dest, id)
	})
	const id = "job-io-once"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "exe", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(context.Background(), id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	if publishes != 1 {
		t.Fatalf("publishes=%d want 1", publishes)
	}
	if _, err := os.Stat(cand); err != nil {
		t.Fatalf("destination missing: %v", err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" {
		t.Fatalf("status=%q want completed", got.Status)
	}
	if got.CandidateSizeBytes <= 0 || got.CandidateSHA256 == "" {
		t.Fatalf("accepted candidate identity not attested: %+v", got)
	}
}

// Post-publish verification failure fails closed: a publish that leaves a
// size-mismatched destination stays resumable and never completes.
func TestIOOpt_PostPublishVerificationFailsClosed(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	src := pr6b1WriteFile(t, filepath.Join(dir, "src.mkv"))
	cand := filepath.Join(ext, "out.mkv")
	cfg := pr6b2Config(dir, func(c *WorkerConfig) { c.ExternalRoots = []string{ext} })
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)
	w.SetFinalizeOutput(func(ctx context.Context, local, dest, id string) error {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, []byte("x"), 0o644)
	})
	const id = "job-io-verify-fail"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "exe", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(context.Background(), id); err == nil {
		t.Fatal("expected post-publish verification failure")
	}
	got, err := LoadJob(filepath.Join(cfg.StateDir, id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "completed" {
		t.Fatal("size-mismatched publish must never complete")
	}
	if !got.EncodeComplete {
		t.Fatal("encode checkpoint must be preserved for safe resume")
	}
}

// Crash/resume/cancel never double-publishes: after EncodeComplete, InternalRun
// resumes finalization only and never re-encodes; cancellation before publish
// wins and leaves the destination absent.
func TestIOOpt_CrashResumeDoesNotDoubleEncodeOrPublish(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	src := pr6b1WriteFile(t, filepath.Join(ext, "src.mkv"))
	cand := filepath.Join(ext, "out.mkv")
	cfg := pr6b2Config(dir, func(c *WorkerConfig) { c.ExternalRoots = []string{ext} })
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	encodes := 0
	w.SetProbeSource(func(context.Context, string) ([]SourceStream, float64, error) {
		return []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "hevc"}}, 60, nil
	})
	w.SetRunFFmpeg(func(_ context.Context, _ *ExecutionPlan, _ *JobRecord, _, out, _, _ string) error {
		encodes++
		return os.WriteFile(out, []byte("encoded"), 0o644)
	})
	publishes := 0
	w.SetFinalizeOutput(func(ctx context.Context, local, dest, id string) error {
		publishes++
		return FinalizeOutputAtomic(ctx, local, dest, id)
	})
	const id = "job-io-resume"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand}, "exe", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(context.Background(), id); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if encodes != 1 || publishes != 1 {
		t.Fatalf("encodes=%d publishes=%d want 1/1", encodes, publishes)
	}
	encodes, publishes = 0, 0
	if err := w.InternalRun(context.Background(), id); err != nil {
		t.Fatalf("resume of completed job: %v", err)
	}
	if encodes != 0 || publishes != 0 {
		t.Fatalf("completed resume must not encode/publish: encodes=%d publishes=%d", encodes, publishes)
	}
}
