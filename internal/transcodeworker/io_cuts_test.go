package transcodeworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/internal/smbdirect"
)

// All paths and bytes below are synthetic temp files.

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// countLocalHashes records whole-file local hashes per path for one test.
func countLocalHashes(t *testing.T) func(path string) int {
	t.Helper()
	var mu sync.Mutex
	counts := map[string]int{}
	onLocalHashRead = func(path string) {
		mu.Lock()
		counts[filepath.Clean(path)]++
		mu.Unlock()
	}
	t.Cleanup(func() { onLocalHashRead = nil })
	return func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[filepath.Clean(path)]
	}
}

func TestDigestMemoReusesFingerprintWithinRun(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "job", "candidate.mkv")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("validated candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := withDigestMemo(context.Background(), root)
	m := digestMemoFrom(ctx)
	first, err := hashLocalFileSHA256(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := verifiedLocalSHA256(ctx, path)
		if err != nil || again != first {
			t.Fatalf("reuse %d: %s %v", i, again, err)
		}
	}
	if m.fullReads() != 1 || m.reused != 3 {
		t.Fatalf("reads=%d reused=%d, want one read and three fingerprint hits", m.fullReads(), m.reused)
	}

	// Any rewrite changes the fingerprint and forces a full read.
	if err := os.WriteFile(path, []byte("replaced candidate!"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(path, future, future)
	changed, err := verifiedLocalSHA256(ctx, path)
	if err != nil || changed == first || m.fullReads() != 2 {
		t.Fatalf("changed file: sha=%s reads=%d err=%v", changed, m.fullReads(), err)
	}

	// A new job run (restart, retried finalization) starts empty.
	fresh := withDigestMemo(context.Background(), root)
	if _, err := verifiedLocalSHA256(fresh, path); err != nil || digestMemoFrom(fresh).fullReads() != 1 {
		t.Fatal("a fresh run must read the file again", err)
	}
}

func TestDigestMemoNeverTrustsSharedStorage(t *testing.T) {
	private := t.TempDir()
	shared := filepath.Join(t.TempDir(), "mounted-share", "source.mkv")
	if err := os.MkdirAll(filepath.Dir(shared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("shared bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := withDigestMemo(context.Background(), private)
	for i := 0; i < 2; i++ {
		if _, err := verifiedLocalSHA256(ctx, shared); err != nil {
			t.Fatal(err)
		}
	}
	if got := digestMemoFrom(ctx).fullReads(); got != 2 {
		t.Fatalf("reads=%d, want every check of a shared file to read it", got)
	}
}

// A staged job hashes the staged source zero times after the streaming copy
// and the local candidate once, instead of 3-4 and ~6 times.
func TestStagedJobReadsEachLocalFileAtMostOnce(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "nas")
	src := pr6b1WriteFile(t, filepath.Join(ext, "src.mkv"))
	content, _ := os.ReadFile(src)
	cand := filepath.Join(ext, "out.mkv")
	cfg := pr6b2Config(dir, func(c *WorkerConfig) {
		c.ExternalRoots = []string{ext}
		c.StagingPolicy = StagingPolicyAlways
	})
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	(&pr6b2Recorder{createOut: true}).install(w)
	hashes := countLocalHashes(t)
	const id = "job-hash-once"
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: id, SourcePath: src, CandidatePath: cand, SourceSHA256: sha256Hex(content)}, "exe", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.InternalRun(context.Background(), id); err != nil {
		t.Fatalf("InternalRun: %v", err)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "completed" {
		t.Fatalf("status=%q err=%s", got.Status, got.Error)
	}
	staged, _ := StagedInputPath(w.localWorkDir(), id, src)
	local, _ := LocalCandidatePath(w.localWorkDir(), id, cand)
	if n := hashes(staged); n != 0 {
		t.Fatalf("staged input hashed %d times, want 0 (digest streamed during staging)", n)
	}
	if n := hashes(local); n != 1 {
		t.Fatalf("local candidate hashed %d times, want 1", n)
	}
	if c := got.PhaseCosts["staging"]; c.NASReadBytes == nil || *c.NASReadBytes != int64(len(content)) {
		t.Fatalf("staging NAS read bytes = %v, want %d", c.NASReadBytes, len(content))
	}
	if c := got.PhaseCosts["publication"]; c.NASWrittenBytes == nil || *c.NASWrittenBytes <= 0 {
		t.Fatalf("publication NAS written bytes missing: %+v", c)
	}
}

func TestPopulateSourceCacheHardLinksStagedInput(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	local := filepath.Join(work, "job-1", "input.mkv")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("staged source bytes")
	if err := os.WriteFile(local, content, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := withDigestMemo(context.Background(), work)
	rememberLocalDigest(ctx, local, sha256Hex(content))
	hashes := countLocalHashes(t)
	fi := directInfo{size: int64(len(content))}
	cached, err := populateSourceCacheFromLocal(ctx, filepath.Join(work, "_source-cache"), "/synthetic/nas/movie.mkv", fi, local, sha256Hex(content))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(local)
	b, _ := os.Stat(cached)
	if !os.SameFile(a, b) {
		t.Fatal("cache entry is a copy, want a hard link to the staged input")
	}
	if hashes(local) != 0 {
		t.Fatal("populate re-read bytes whose digest was already proved")
	}
	// A wrong digest is still never published.
	if _, err := populateSourceCacheFromLocal(context.Background(), filepath.Join(dir, "other-cache"), "/synthetic/nas/movie.mkv", fi, local, sha256Hex([]byte("other"))); err == nil {
		t.Fatal("mismatched digest populated the cache")
	}
}

func writeCacheEntry(t *testing.T, cacheDir, name string, size int, age time.Duration) string {
	t.Helper()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cacheDir, name+".mkv")
	if err := os.WriteFile(p, bytes.Repeat([]byte("s"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	_ = os.Chtimes(p, when, when)
	return p
}

func TestSourceCacheCapCoversLargestSourceAndPins(t *testing.T) {
	dir := t.TempDir()
	cfg := pr6b2Config(dir, func(c *WorkerConfig) { c.SourceCacheMaxBytes = 10 })
	w := NewWorker(cfg)
	cacheDir := w.sourceCacheDir()

	// One source larger than the configured cap survives: the effective cap
	// is 1.5x the largest entry.
	big := writeCacheEntry(t, cacheDir, "aaaa", 100, 2*time.Hour)
	_ = w.sweepSourceCache()
	if _, err := os.Stat(big); err != nil {
		t.Fatal("a single large source was evicted by its own size", err)
	}

	// Over the effective cap, the oldest unpinned entry goes and a pinned one
	// stays even though it is older.
	pinned := writeCacheEntry(t, cacheDir, "bbbb", 100, 4*time.Hour)
	if err := os.MkdirAll(sourceCachePinDir(cacheDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceCachePinDir(cacheDir), "bbbb."+pinOwnerTag(benchmarkPinOwner)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = w.sweepSourceCache()
	if _, err := os.Stat(pinned); err != nil {
		t.Fatal("pinned entry evicted", err)
	}
	if _, err := os.Stat(big); !os.IsNotExist(err) {
		t.Fatal("unpinned entry should have been evicted to honour the cap", err)
	}

	// Completing the job that held the entry releases the benchmark pin too.
	if err := os.WriteFile(filepath.Join(sourceCachePinDir(cacheDir), "bbbb."+pinOwnerTag("job-done")), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	releaseSourceCachePins(cacheDir, "job-done", true)
	if live := liveSourceCachePins(cacheDir, time.Hour, time.Now()); live["bbbb"] {
		t.Fatal("pins survived the completed job")
	}
	if got := effectiveSourceCacheMaxBytes(1000, 100); got != 1000 {
		t.Fatalf("configured cap above the floor must stand, got %d", got)
	}
}

// verifiedCountingStore is the counting store with the streaming download.
type verifiedCountingStore struct {
	ioOptCountingStore
	attempts int
}

func (s *verifiedCountingStore) DownloadAtomicVerified(ctx context.Context, remote, local, expected string) (smbdirect.Transfer, error) {
	if err := s.ioOptCountingStore.DownloadAtomic(ctx, remote, local); err != nil {
		return smbdirect.Transfer{}, err
	}
	n := int64(len(s.content))
	return smbdirect.Transfer{ReadBytes: n + n/2, Attempts: s.attempts, Resumed: s.attempts > 1, SHA256: sha256Hex(s.content)}, nil
}

func stagedJob(t *testing.T, w *Worker, stateDir, id, src, sha string) (*JobRecord, *resolvedOperational) {
	t.Helper()
	staged, err := StagedInputPath(w.localWorkDir(), id, src)
	if err != nil {
		t.Fatal(err)
	}
	job := &JobRecord{
		ID: id, Status: "running", Source: src, Candidate: filepath.Join(filepath.Dir(src), "out.mkv"),
		Profile: "hevc-vt", Plan: ioOptPlan(t), ExecutionSpecDigest: pr5Digest,
		SourceSHA256: sha, StagingPolicy: string(StagingPolicyAlways),
		StagingState: string(StagingStatePending), EffectiveInputPath: staged, StagedInputPath: staged,
		FinalizationState: string(FinalizationStateNotRequired), CreatedAt: time.Now().UTC(),
	}
	ioOptSeed(t, stateDir, job)
	r, err := resolveOperationalForExecution(job)
	if err != nil {
		t.Fatal(err)
	}
	return job, r
}

// A download that reconnected reports its attempts and the bytes it really
// read; the staged digest comes from the stream, not a re-read.
func TestStagingRecordsDownloadRetryAndStreamedDigest(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "unmounted-nas")
	src := filepath.Join(root, "movie.mkv")
	content := []byte("synthetic source bytes")
	store := &verifiedCountingStore{ioOptCountingStore: ioOptCountingStore{root: root, content: content}, attempts: 2}
	cfg := &WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{root}, ExternalRoots: []string{root}, LocalWorkDir: filepath.Join(dir, "work"), StagingPolicy: StagingPolicyAlways, MaxParallelJobs: 1, DisableSourceCache: true}
	w := NewWorker(cfg)
	w.mediaStore = store
	job, r := stagedJob(t, w, cfg.StateDir, "job-retry", src, sha256Hex(content))
	hashes := countLocalHashes(t)
	ctx := withDigestMemo(context.Background(), w.localWorkDir())
	if cancelled, err := w.ensureStaged(ctx, filepath.Join(cfg.StateDir, job.ID), filepath.Join(cfg.StateDir, job.ID, "job.json"), job, r); err != nil || cancelled {
		t.Fatalf("ensureStaged: %v %v", cancelled, err)
	}
	if err := verifyLocalDigest(ctx, r.stagedInput, job.SourceSHA256); err != nil || hashes(r.stagedInput) != 0 {
		t.Fatalf("staged digest check: err=%v reads=%d, want streamed digest reuse", err, hashes(r.stagedInput))
	}
	cost := pr6b1LoadJob(t, cfg.StateDir, job.ID).PhaseCosts["staging"]
	if cost.Attempts != 2 || cost.NASReadBytes == nil || *cost.NASReadBytes != int64(len(content)+len(content)/2) {
		t.Fatalf("staging cost = %+v, want 2 attempts and the bytes actually read", cost)
	}
}

// A manual retry under a new job ID stages from the cache with zero NAS
// reads, after the failed job's own staged copy was removed.
func TestRetryWithNewJobIDHitsCacheAfterFailedJobCleanup(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "unmounted-nas")
	src := filepath.Join(root, "movie.mkv")
	content := []byte("synthetic source for retry")
	store := &verifiedCountingStore{ioOptCountingStore: ioOptCountingStore{root: root, content: content}, attempts: 1}
	cfg := &WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{root}, ExternalRoots: []string{root}, LocalWorkDir: filepath.Join(dir, "work"), StagingPolicy: StagingPolicyAlways, MaxParallelJobs: 1}
	w := NewWorker(cfg)
	w.mediaStore = store
	sha := sha256Hex(content)

	first, r1 := stagedJob(t, w, cfg.StateDir, "job-first", src, sha)
	if _, err := w.ensureStaged(context.Background(), filepath.Join(cfg.StateDir, first.ID), filepath.Join(cfg.StateDir, first.ID, "job.json"), first, r1); err != nil {
		t.Fatal(err)
	}
	_ = w.failJobTerminal(filepath.Join(cfg.StateDir, first.ID), filepath.Join(cfg.StateDir, first.ID, "job.json"), first, errors.New("synthetic encoder failure"))
	if _, err := os.Stat(r1.stagedInput); !os.IsNotExist(err) {
		t.Fatal("failed job kept its staged input", err)
	}

	retry, r2 := stagedJob(t, w, cfg.StateDir, "job-retry", src, sha)
	if _, err := w.ensureStaged(context.Background(), filepath.Join(cfg.StateDir, retry.ID), filepath.Join(cfg.StateDir, retry.ID, "job.json"), retry, r2); err != nil {
		t.Fatal(err)
	}
	if store.downloads != 1 {
		t.Fatalf("downloads=%d, want the retry served from the cache", store.downloads)
	}
	cost := pr6b1LoadJob(t, cfg.StateDir, retry.ID).PhaseCosts["staging"]
	if cost.CacheHits == nil || *cost.CacheHits != 1 || cost.NASReadBytes == nil || *cost.NASReadBytes != 0 {
		t.Fatalf("retry staging cost = %+v", cost)
	}
	if b, err := os.ReadFile(r2.stagedInput); err != nil || !bytes.Equal(b, content) {
		t.Fatal("retry staged bytes wrong", err)
	}
}

func TestCancelRemovesStagedInputButNotCacheEntry(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "unmounted-nas")
	src := filepath.Join(root, "movie.mkv")
	content := []byte("synthetic source for cancel")
	store := &verifiedCountingStore{ioOptCountingStore: ioOptCountingStore{root: root, content: content}, attempts: 1}
	cfg := &WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{root}, ExternalRoots: []string{root}, LocalWorkDir: filepath.Join(dir, "work"), StagingPolicy: StagingPolicyAlways, MaxParallelJobs: 1}
	w := NewWorker(cfg)
	w.mediaStore = store
	job, r := stagedJob(t, w, cfg.StateDir, "job-cancel", src, sha256Hex(content))
	if _, err := w.ensureStaged(context.Background(), filepath.Join(cfg.StateDir, job.ID), filepath.Join(cfg.StateDir, job.ID, "job.json"), job, r); err != nil {
		t.Fatal(err)
	}
	entries := ioOptCacheDataFiles(t, w.sourceCacheDir())
	if len(entries) != 1 {
		t.Fatalf("cache entries = %v", entries)
	}
	w.cleanupCancelledArtifacts(job)
	if _, err := os.Stat(r.stagedInput); !os.IsNotExist(err) {
		t.Fatal("cancelled job kept its staged input", err)
	}
	if b, err := os.ReadFile(entries[0]); err != nil || !bytes.Equal(b, content) {
		t.Fatal("cancel cleanup broke the shared cache entry", err)
	}
}

// On a share without atomic rename or links the copy fallback reads the
// local candidate (no read-back of the partial), checks the accepted digest,
// and is reported; the next publish to that share skips the partial.
func TestMountedPublishCopyFallbackIsReportedAndLearned(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	content := bytes.Repeat([]byte("encoded"), 1000)
	mustWriteFile(t, candidate, content, 0o640)
	dest := filepath.Join(base, "share", "out.mkv")
	caps := &commitCaps{noAtom: map[uint64]time.Time{}, horizon: time.Hour}
	fallback := func(ctx context.Context, src, dst string) error {
		return commitNoReplaceWith(ctx, src, dst, unsupportedRename, definiteNoLink)
	}

	rep := &publicationReport{expectSHA: sha256Hex(content)}
	if err := finalizeOutputAtomicCaps(withPublicationReport(context.Background(), rep), candidate, dest, "job-1", nil, fallback, caps); err != nil {
		t.Fatal(err)
	}
	m := int64(len(content))
	if !rep.CopyFallback || rep.Method != publishMethodCopy || rep.ReadBytes != 0 || rep.WrittenBytes != 2*m {
		t.Fatalf("report = %+v, want copy fallback writing 2M and reading 0", rep)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Fatal("destination bytes differ")
	}
	if exists, _ := pathExists(PartialPathFor(dest, "job-1")); exists {
		t.Fatal("partial left behind")
	}

	// The share is now known: the next job writes the final name directly.
	dest2 := filepath.Join(base, "share", "out2.mkv")
	partialWritten := false
	rep2 := &publicationReport{expectSHA: sha256Hex(content)}
	err := finalizeOutputAtomicCaps(withPublicationReport(context.Background(), rep2), candidate, dest2, "job-2", func(string) error { partialWritten = true; return nil }, fallback, caps)
	if err != nil {
		t.Fatal(err)
	}
	if partialWritten || rep2.Method != publishMethodDirect || !rep2.CopyFallback || rep2.WrittenBytes != m {
		t.Fatalf("report = %+v partial=%v, want one direct write of M", rep2, partialWritten)
	}

	// A candidate that no longer matches its accepted digest is withdrawn.
	dest3 := filepath.Join(base, "share", "out3.mkv")
	rep3 := &publicationReport{expectSHA: sha256Hex([]byte("accepted something else"))}
	if err := finalizeOutputAtomicCaps(withPublicationReport(context.Background(), rep3), candidate, dest3, "job-3", nil, fallback, caps); !errors.Is(err, ErrSourceInvalid) || errors.Is(err, ErrStorageIO) {
		t.Fatalf("digest mismatch: %v, want ErrSourceInvalid and not a retryable storage error", err)
	}
	if exists, _ := pathExists(dest3); exists {
		t.Fatal("withdrawn publication left its destination")
	}
}

// Test-only finalizers never touch the process-wide capability cache.
func TestFinalizeOutputAtomicWithLeavesSharedCapsAlone(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	mustWriteFile(t, candidate, []byte("bytes"), 0o640)
	dest := filepath.Join(base, "dest", "final.mkv")
	if err := finalizeOutputAtomicWith(context.Background(), candidate, dest, "job-1", nil, smbCommitFallback()); err != nil {
		t.Fatal(err)
	}
	if sharedCommitCaps.atomicUnsupported(filepath.Dir(dest)) {
		t.Fatal("injected fallback poisoned the shared capability cache")
	}
}

// verifiedDirectStore adds the verified publish to the fake SMB store.
type verifiedDirectStore struct {
	fakeDirectStore
	expect   smbdirect.PublishExpect
	verified int
}

func (s *verifiedDirectStore) PublishVerified(_ context.Context, local, destination, _ string, expect smbdirect.PublishExpect, _ func(func() error) error) (smbdirect.Transfer, error) {
	if _, err := os.Stat(local); err != nil {
		return smbdirect.Transfer{}, err
	}
	s.verified++
	s.expect = expect
	s.publishTo = destination
	return smbdirect.Transfer{WrittenBytes: expect.SizeBytes, ReadBytes: expect.SizeBytes, Attempts: 1}, nil
}

// Direct-SMB finalization hands the accepted size and digest to the verified
// publish (no local pre-hash, one read-back, no post-rename re-hash) and
// records the bytes that crossed the share.
func TestSMBFinalizationUsesVerifiedPublishWithAcceptedDigest(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "unmounted-media")
	stateDir := filepath.Join(dir, "state")
	workDir := filepath.Join(dir, "work")
	store := &verifiedDirectStore{fakeDirectStore: fakeDirectStore{root: root, source: []byte("source")}}
	w := NewWorker(&WorkerConfig{StateDir: stateDir, AllowedRoots: []string{root}, ExternalRoots: []string{root}, LocalWorkDir: workDir, StagingPolicy: StagingPolicyAuto})
	w.mediaStore = store
	candidate := []byte("encoded-candidate")
	dest := filepath.Join(root, "Show", "episode.mkv")
	job := &JobRecord{
		ID: "job-smb-verified", Status: "running", Source: filepath.Join(root, "Show", "source.mkv"),
		Candidate: dest, EncodeComplete: true, ExecutionSpecDigest: pr5Digest,
		StagingState: string(StagingStateNotRequired), FinalizationState: string(FinalizationStatePending),
		LocalCandidatePath:  filepath.Join(workDir, "job-smb-verified", "candidate.mkv"),
		IntendedDestination: dest, PartialPath: PartialPathFor(dest, "job-smb-verified"),
		CandidateSizeBytes: int64(len(candidate)), CandidateSHA256: sha256Hex(candidate),
	}
	jobDir := filepath.Join(stateDir, job.ID)
	jobFile := filepath.Join(jobDir, "job.json")
	if err := SaveJobAtomic(jobFile, job); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(job.LocalCandidatePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(job.LocalCandidatePath, candidate, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := resolveOperationalForExecution(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.finalizeOperational(context.Background(), jobDir, jobFile, job, r); err != nil {
		t.Fatal(err)
	}
	if store.verified != 1 || store.published != 0 || store.expect.SHA256 != job.CandidateSHA256 || store.expect.SizeBytes != job.CandidateSizeBytes {
		t.Fatalf("verified=%d legacy=%d expect=%+v", store.verified, store.published, store.expect)
	}
	got, err := LoadJob(jobFile)
	if err != nil {
		t.Fatal(err)
	}
	pub := got.PhaseCosts["publication"]
	if got.Status != "completed" || pub.NASWrittenBytes == nil || *pub.NASWrittenBytes != int64(len(candidate)) || pub.NASReadBytes == nil || *pub.NASReadBytes != int64(len(candidate)) {
		t.Fatalf("status=%s publication=%+v", got.Status, pub)
	}
}

// Sweeps in tests never see the developer's real disk: a nearly full laptop
// would otherwise trip the free-space floor. The floor test stubs it itself.
func init() {
	diskSpaceFn = func(string) (uint64, uint64, bool) { return 0, 0, false }
}

func TestDiskSpaceReportsTheVolume(t *testing.T) {
	avail, total, ok := diskSpace(t.TempDir())
	if !ok {
		t.Skip("disk space unavailable on this platform")
	}
	if total == 0 || avail > total {
		t.Fatalf("avail=%d total=%d", avail, total)
	}
}

func definiteNoLink(src, dst string) error {
	return &os.LinkError{Op: "link", Old: src, New: dst, Err: syscall.ENOTSUP}
}

// A transient link failure still falls back for that one publication but is
// never remembered, so the next publication tries the atomic primitives again.
func TestTransientLinkFailureIsNotLearned(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "candidate.mkv")
	content := []byte("encoded bytes")
	mustWriteFile(t, candidate, content, 0o640)
	caps := &commitCaps{noAtom: map[uint64]time.Time{}, horizon: time.Hour}
	for _, tc := range []struct {
		name  string
		err   error
		learn bool
	}{
		{"eio", syscall.EIO, false},
		{"etimedout", syscall.ETIMEDOUT, false},
		{"opaque", errors.New("operation not supported"), false},
		{"eperm", syscall.EPERM, true},
	} {
		dest := filepath.Join(base, tc.name, "out.mkv")
		linkErr := tc.err
		commit := func(ctx context.Context, src, dst string) error {
			return commitNoReplaceWith(ctx, src, dst, unsupportedRename, func(s, d string) error {
				return &os.LinkError{Op: "link", Old: s, New: d, Err: linkErr}
			})
		}
		rep := &publicationReport{}
		if err := finalizeOutputAtomicCaps(withPublicationReport(context.Background(), rep), candidate, dest, "job-1", nil, commit, caps); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !rep.CopyFallback || rep.AtomicUnsupported != tc.learn || caps.atomicUnsupported(filepath.Dir(dest)) != tc.learn {
			t.Fatalf("%s: report=%+v learned=%v, want learn=%v", tc.name, rep, caps.atomicUnsupported(filepath.Dir(dest)), tc.learn)
		}
	}
}

// Fingerprints are never trusted inside an external or direct-SMB root, even
// when the local work dir was misconfigured to live there.
func TestDigestMemoRejectsWorkDirOnSharedStorage(t *testing.T) {
	share := t.TempDir()
	work := filepath.Join(share, "work")
	path := filepath.Join(work, "job", "candidate.mkv")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []*WorkerConfig{
		{StateDir: filepath.Join(t.TempDir(), "state"), LocalWorkDir: work, ExternalRoots: []string{share}},
		{StateDir: filepath.Join(t.TempDir(), "state"), LocalWorkDir: work, SMBDirect: smbdirect.Config{LocalRoot: share}},
	} {
		w := &Worker{cfg: cfg}
		ctx := w.withJobDigestMemo(context.Background())
		for i := 0; i < 2; i++ {
			if _, err := verifiedLocalSHA256(ctx, path); err != nil {
				t.Fatal(err)
			}
		}
		if got := digestMemoFrom(ctx).fullReads(); got != 2 {
			t.Fatalf("reads=%d, want a full read every time on shared storage", got)
		}
	}
}

// Below the free-space floor the cache gives up even pinned entries.
func TestSourceCacheDiskFloorOutranksPins(t *testing.T) {
	dir := t.TempDir()
	w := NewWorker(pr6b2Config(dir, func(c *WorkerConfig) { c.SourceCacheMaxBytes = 1 << 40 }))
	cacheDir := w.sourceCacheDir()
	entry := writeCacheEntry(t, cacheDir, "cccc", 100, 2*time.Hour)
	if err := os.MkdirAll(sourceCachePinDir(cacheDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceCachePinDir(cacheDir), "cccc."+pinOwnerTag(benchmarkPinOwner)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	orig := diskSpaceFn
	t.Cleanup(func() { diskSpaceFn = orig })

	diskSpaceFn = func(string) (uint64, uint64, bool) { return 500, 1000, true } // plenty free
	_ = w.sweepSourceCache()
	if _, err := os.Stat(entry); err != nil {
		t.Fatal("pinned entry evicted with free space to spare", err)
	}

	diskSpaceFn = func(string) (uint64, uint64, bool) { return 50, 1000, true } // under the 10% floor
	_ = w.sweepSourceCache()
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatal("pinned entry kept while the work disk is below its floor", err)
	}
}

type partialRemovingStore struct {
	fakeDirectStore
	removed []string
}

func (s *partialRemovingStore) RemovePartial(_ context.Context, destination, jobID string) error {
	s.removed = append(s.removed, destination+"|"+jobID)
	return nil
}

// A cancelled post-encode job drops its partial on the share; a job that
// never reached the encode checkpoint costs no share round trip.
func TestCancelRemovesRemotePartial(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "unmounted-media")
	store := &partialRemovingStore{fakeDirectStore: fakeDirectStore{root: root}}
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, "state"), AllowedRoots: []string{root}, ExternalRoots: []string{root}, LocalWorkDir: filepath.Join(dir, "work")})
	w.mediaStore = store
	dest := filepath.Join(root, "Show", "episode.mkv")
	w.cleanupCancelledArtifacts(&JobRecord{ID: "job-early", Candidate: dest, IntendedDestination: dest})
	w.cleanupCancelledArtifacts(&JobRecord{ID: "job-late", Candidate: dest, IntendedDestination: dest, EncodeComplete: true, FinalizationState: string(FinalizationStateFinalizing)})
	if len(store.removed) != 1 || store.removed[0] != dest+"|job-late" {
		t.Fatalf("removed=%v, want only the post-encode job's partial", store.removed)
	}
}
