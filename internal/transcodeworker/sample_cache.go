package transcodeworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

// This file implements the worker-local sample-evaluation cache.
//
// A size-mode search encodes short sample windows of a source with each
// candidate and measures VMAF/CAMBI against a lossless reference of the same
// window. That measurement is a pure function of (source bytes, window,
// candidate encode parameters, encoder/FFmpeg/libvmaf build, metric settings);
// production runs repeated it bit-identically while spending minutes of CPU on
// every repeat. The cache stores one finished (candidate, window) evaluation
// per key so a later run, or a later stage of the same run, replays it instead
// of re-encoding and re-measuring.
//
// Design (correctness-first; every doubt is a miss):
//   - The cache lives on the WORKER, under <LocalWorkDir>/_sample-cache. The
//     worker is the only party that knows the exact encoder, FFmpeg and libvmaf
//     build, and VideoToolbox output is machine-specific. Keeping the data
//     local adds no request field and no capability, so the capability
//     fingerprint, frozen policy snapshots and digests are unchanged, and a
//     server can never inject measurements into selection.
//   - The key is the SHA-256 of every input that can change the result (see
//     sampleCacheKeyMaterial). Fields that are proven not to change the result
//     (encoder pool/frame threads, libvmaf n_threads, candidate labels, job
//     paths) are deliberately excluded.
//   - An entry is a checksummed JSON document written atomically. Reading
//     verifies the checksum, the schema, the key echo and every value against
//     the current request before use; anything else is ignored (and removed).
//   - Only fully successful units are stored: the encode and every enabled
//     metric succeeded. Failures, poisoned windows and CAMBI errors are never
//     cached, so a hit always replays a complete measurement.
//   - The cache is bounded by bytes and entry count, entries expire after a
//     TTL since last use, and the oldest-used entries are evicted first.

const (
	sampleCacheSchema  = 1
	sampleCacheDirName = "_sample-cache"
	// DefaultSampleCacheMaxBytes bounds the cache (64 MiB). An entry is a few
	// KiB: per-frame VMAF scores dominate, and an entry covers one short window.
	DefaultSampleCacheMaxBytes = 64 * 1024 * 1024
	// sampleCacheMaxEntries bounds the number of entries regardless of size.
	sampleCacheMaxEntries = 20000
	// DefaultSampleCacheTTL is the time an entry survives without being used.
	DefaultSampleCacheTTL = 30 * 24 * time.Hour
	// sampleCacheMaxEntryBytes refuses to read an implausibly large entry.
	sampleCacheMaxEntryBytes = 4 << 20
	// sampleCacheSweepEvery runs the eviction sweep on every Nth store (and
	// on the first store of a process).
	sampleCacheSweepEvery = 64
	// sampleCacheTempMaxAge is how long an orphaned temp file survives.
	sampleCacheTempMaxAge = time.Hour
	// sampleCacheKeyThreads is the libvmaf thread count used when building
	// the metric argument lists that enter the key. Thread counts never change
	// per-frame scores, so a constant keeps keys independent of host load.
	sampleCacheKeyThreads = 1
)

// sampleCacheStores counts stores in this process to pace eviction sweeps.
var sampleCacheStores atomic.Int64

// sampleCacheDir returns the cache directory, or "" when no work dir exists.
func (w *Worker) sampleCacheDir() string {
	base := w.localWorkDir()
	if strings.TrimSpace(base) == "" {
		return ""
	}
	return filepath.Join(filepath.Clean(base), sampleCacheDirName)
}

// sampleCacheEnabled reports whether the sample cache may be used. Disabling
// it restores the pre-cache behavior exactly.
func (w *Worker) sampleCacheEnabled() bool {
	if w == nil || w.cfg == nil || w.cfg.DisableSampleCache {
		return false
	}
	return w.sampleCacheDir() != ""
}

func (w *Worker) sampleCacheMaxBytes() int64 {
	if w == nil || w.cfg == nil || w.cfg.SampleCacheMaxBytes <= 0 {
		return DefaultSampleCacheMaxBytes
	}
	return w.cfg.SampleCacheMaxBytes
}

func (w *Worker) sampleCacheTTL() time.Duration {
	if w == nil || w.cfg == nil || w.cfg.SampleCacheTTLHours <= 0 {
		return DefaultSampleCacheTTL
	}
	return time.Duration(w.cfg.SampleCacheTTLHours) * time.Hour
}

// sampleCache is a handle on the on-disk cache. It holds no state besides its
// bounds, so any number of processes may use the same directory.
type sampleCache struct {
	dir        string
	maxBytes   int64
	maxEntries int
	ttl        time.Duration
	now        func() time.Time
}

func (w *Worker) newSampleCache() *sampleCache {
	if !w.sampleCacheEnabled() {
		return nil
	}
	return &sampleCache{
		dir:        w.sampleCacheDir(),
		maxBytes:   w.sampleCacheMaxBytes(),
		maxEntries: sampleCacheMaxEntries,
		ttl:        w.sampleCacheTTL(),
		now:        time.Now,
	}
}

// sampleCacheVMAF is the stored VMAF measurement. Frame scores are kept
// exactly (base64 of little-endian IEEE-754 doubles) because pooled p5/mean
// across samples are computed from frames, never from per-sample summaries.
type sampleCacheVMAF struct {
	Mean        float64           `json:"mean"`
	Stats       quality.VMAFStats `json:"stats"`
	FrameScores string            `json:"frame_scores"`
}

// sampleCacheBody is the stored evaluation of one (candidate, window).
type sampleCacheBody struct {
	Schema            int                 `json:"schema"`
	Key               string              `json:"key"`
	SizeBytes         int64               `json:"size_bytes"`
	EncodeDurationSec float64             `json:"encode_duration_sec"`
	VMAF              *sampleCacheVMAF    `json:"vmaf,omitempty"`
	VMAFDurationSec   float64             `json:"vmaf_duration_sec,omitempty"`
	CAMBI             *quality.CAMBIStats `json:"cambi,omitempty"`
	CAMBIDurationSec  float64             `json:"cambi_duration_sec,omitempty"`
}

// sampleCacheFile wraps the body with a checksum over its exact bytes.
type sampleCacheFile struct {
	Body   json.RawMessage `json:"body"`
	SHA256 string          `json:"sha256"`
}

// sampleCacheExpect is what the current request requires of a stored entry.
type sampleCacheExpect struct {
	runVMAF     bool
	model       *quality.VMAFModel
	decimation  int
	windowIndex int
	runCAMBI    bool
	cambiOutput string
}

func encodeFrameScores(scores []float64) string {
	buf := make([]byte, 8*len(scores))
	for i, s := range scores {
		binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(s))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func decodeFrameScores(s string) ([]float64, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) == 0 || len(raw)%8 != 0 {
		return nil, errors.New("malformed frame scores")
	}
	out := make([]float64, len(raw)/8)
	for i := range out {
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(raw[i*8:]))
	}
	return out, nil
}

func finiteNonNegative(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

// validate checks a decoded body against the current request. It restores the
// raw frame scores onto the VMAF stats, which only succeeds when they agree
// with the stored summary.
func (b *sampleCacheBody) validate(key string, want sampleCacheExpect) (*quality.VMAFStats, error) {
	if b.Schema != sampleCacheSchema {
		return nil, errors.New("schema mismatch")
	}
	if b.Key != key {
		return nil, errors.New("key mismatch")
	}
	if b.SizeBytes <= 0 || !finiteNonNegative(b.EncodeDurationSec) || !finiteNonNegative(b.VMAFDurationSec) || !finiteNonNegative(b.CAMBIDurationSec) {
		return nil, errors.New("invalid encode values")
	}
	var stats *quality.VMAFStats
	if want.runVMAF {
		if b.VMAF == nil || want.model == nil {
			return nil, errors.New("vmaf missing")
		}
		s := b.VMAF.Stats
		if s.ModelID != want.model.ID || s.LibvmafModel != want.model.LibvmafModel || s.TemporalDecimation != want.decimation ||
			s.WorstWindowSampleIndex != want.windowIndex || s.MeasurementBitDepth != want.model.MeasurementBitDepth ||
			s.ScoreMin != want.model.MinScore || s.ScoreMax != want.model.MaxScore {
			return nil, errors.New("vmaf stats do not match request")
		}
		if b.VMAF.Mean != s.Mean {
			return nil, errors.New("vmaf mean mismatch")
		}
		scores, err := decodeFrameScores(b.VMAF.FrameScores)
		if err != nil {
			return nil, err
		}
		restored, err := s.WithFrameScores(scores)
		if err != nil {
			return nil, err
		}
		stats = &restored
	} else if b.VMAF != nil {
		return nil, errors.New("unexpected vmaf")
	}
	if want.runCAMBI {
		c := b.CAMBI
		if c == nil || c.OutputMetric != want.cambiOutput || c.FramesTotal <= 0 || c.WorstSampleIndex != want.windowIndex ||
			!finiteNonNegative(c.Mean) || !finiteNonNegative(c.Max) || c.Mean > c.Max+1e-9 {
			return nil, errors.New("cambi does not match request")
		}
	} else if b.CAMBI != nil {
		return nil, errors.New("unexpected cambi")
	}
	return stats, nil
}

func (c *sampleCache) path(key string) string {
	return filepath.Join(c.dir, key[:2], key+".json")
}

func validSampleCacheKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	_, err := hex.DecodeString(key)
	return err == nil
}

// lookup returns the stored evaluation for key, or ok=false for a miss. A
// corrupt, partial, expired, mismatched or implausible entry is a miss and is
// removed; lookup never returns an error that could fail a search.
func (c *sampleCache) lookup(key string, want sampleCacheExpect) (body *sampleCacheBody, stats *quality.VMAFStats, ok bool) {
	if c == nil || !validSampleCacheKey(key) {
		return nil, nil, false
	}
	path := c.path(key)
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, nil, false
	}
	drop := func() { _ = os.Remove(path) }
	if fi.Size() <= 0 || fi.Size() > sampleCacheMaxEntryBytes || c.now().Sub(fi.ModTime()) > c.ttl {
		drop()
		return nil, nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, false
	}
	var file sampleCacheFile
	if err := json.Unmarshal(data, &file); err != nil || len(file.Body) == 0 {
		drop()
		return nil, nil, false
	}
	sum := sha256.Sum256(file.Body)
	if hex.EncodeToString(sum[:]) != file.SHA256 {
		drop()
		return nil, nil, false
	}
	var b sampleCacheBody
	dec := json.NewDecoder(bytes.NewReader(file.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		drop()
		return nil, nil, false
	}
	stats, err = b.validate(key, want)
	if err != nil {
		drop()
		return nil, nil, false
	}
	// A use refreshes the entry, so eviction and expiry follow last use.
	now := c.now()
	_ = os.Chtimes(path, now, now)
	return &b, stats, true
}

// store writes an entry atomically. Failure is silent: the cache is an
// optimisation and must never fail a search.
func (c *sampleCache) store(key string, body sampleCacheBody) {
	if c == nil || !validSampleCacheKey(key) {
		return
	}
	body.Schema = sampleCacheSchema
	body.Key = key
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	sum := sha256.Sum256(raw)
	data, err := json.Marshal(sampleCacheFile{Body: raw, SHA256: hex.EncodeToString(sum[:])})
	if err != nil || len(data) > sampleCacheMaxEntryBytes {
		return
	}
	path := c.path(key)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if n := sampleCacheStores.Add(1); n%sampleCacheSweepEvery == 1 {
		c.sweep()
	}
}

type sampleCacheFileInfo struct {
	path    string
	size    int64
	modTime time.Time
}

// sweep enforces the bounds: it removes orphaned temp files, expired entries
// and non-entry files, then evicts the least recently used entries until the
// cache is under 90% of its byte and entry bounds. It is best-effort and safe
// to run concurrently from several processes.
func (c *sampleCache) sweep() {
	if c == nil {
		return
	}
	now := c.now()
	var entries []sampleCacheFileInfo
	var total int64
	_ = filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		name := d.Name()
		switch {
		case strings.HasPrefix(name, ".tmp-"):
			if now.Sub(info.ModTime()) > sampleCacheTempMaxAge {
				_ = os.Remove(path)
			}
		case !info.Mode().IsRegular() || !strings.HasSuffix(name, ".json") || !validSampleCacheKey(strings.TrimSuffix(name, ".json")):
			_ = os.Remove(path)
		case now.Sub(info.ModTime()) > c.ttl:
			_ = os.Remove(path)
		default:
			entries = append(entries, sampleCacheFileInfo{path: path, size: info.Size(), modTime: info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	maxBytes, maxEntries := c.maxBytes*9/10, c.maxEntries*9/10
	if total <= c.maxBytes && len(entries) <= c.maxEntries {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].modTime.Equal(entries[j].modTime) {
			return entries[i].modTime.Before(entries[j].modTime)
		}
		return entries[i].path < entries[j].path
	})
	count := len(entries)
	for _, e := range entries {
		if total <= maxBytes && count <= maxEntries {
			break
		}
		if os.Remove(e.path) == nil {
			total -= e.size
			count--
		}
	}
}

// sampleCacheBuildIdentity identifies the encoder and FFmpeg build beyond the
// capability fingerprint: the fingerprint carries versions and flags, this adds
// the binary itself and the libx265 build the binary is linked against.
type sampleCacheBuildIdentity struct {
	FFmpeg   string `json:"ffmpeg"`
	X265     string `json:"x265"`
	Hardware string `json:"hardware,omitempty"` // VideoToolbox only
}

// buildIdentityMemo memoizes the probes for the life of a worker process. The
// binary is re-probed when its size or modification time changes.
type buildIdentityMemo struct {
	mu       sync.Mutex
	path     string
	size     int64
	modTime  time.Time
	identity sampleCacheBuildIdentity
	ok       bool
}

func runProbe(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if out.Len() > 1<<20 {
		out.Truncate(1 << 20)
	}
	return out.Bytes(), err
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// probeSampleCacheBuild identifies the ffmpeg binary and its libx265.
func probeSampleCacheBuild(ctx context.Context, ffmpegPath string) (sampleCacheBuildIdentity, error) {
	var id sampleCacheBuildIdentity
	binary, err := hashFile(ffmpegPath)
	if err != nil {
		return id, err
	}
	version, err := runProbe(ctx, ffmpegPath, "-hide_banner", "-version")
	if err != nil {
		return id, fmt.Errorf("ffmpeg -version: %w", err)
	}
	conf, err := runProbe(ctx, ffmpegPath, "-hide_banner", "-buildconf")
	if err != nil {
		return id, fmt.Errorf("ffmpeg -buildconf: %w", err)
	}
	sum := sha256.New()
	for _, part := range [][]byte{[]byte(binary), version, conf} {
		sum.Write(part)
		sum.Write([]byte{0})
	}
	id.FFmpeg = hex.EncodeToString(sum.Sum(nil))
	// libx265 may be linked dynamically, so neither the binary hash nor the
	// build configuration pins it. Its version banner does. A failure to
	// probe is not an error here: a worker without libx265 simply cannot
	// produce an x265 entry, and the probe result is part of the key.
	out, _ := runProbe(ctx, ffmpegPath, "-hide_banner", "-loglevel", "info", "-f", "lavfi", "-i", "color=c=black:s=64x64:r=25:d=0.04",
		"-frames:v", "1", "-c:v", "libx265", "-x265-params", "log-level=info", "-f", "null", "-")
	var x265 []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "x265 [info]: HEVC encoder version") || strings.Contains(line, "x265 [info]: build info") {
			x265 = append(x265, strings.TrimSpace(line))
		}
	}
	id.X265 = strings.Join(x265, "\n")
	if id.X265 == "" {
		id.X265 = "unavailable"
	}
	return id, nil
}

// sampleCacheHardwareIdentity identifies the machine whose hardware encoder
// produced a VideoToolbox sample: its output depends on the SoC and OS build.
// A var so tests can replace the sysctl probe.
var sampleCacheHardwareIdentity = func(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("hardware encoder identity is only known on macOS")
	}
	var parts []string
	for _, name := range []string{"hw.model", "machdep.cpu.brand_string", "kern.osversion"} {
		out, err := runProbe(ctx, "sysctl", "-n", name)
		if err != nil {
			return "", fmt.Errorf("sysctl %s: %w", name, err)
		}
		parts = append(parts, strings.TrimSpace(string(out)))
	}
	return strings.Join(parts, "|"), nil
}

// sampleCacheBuild returns the memoized build identity of the worker's ffmpeg.
func (w *Worker) sampleCacheBuild(ctx context.Context) (sampleCacheBuildIdentity, error) {
	if w.sampleCacheBuildFn != nil {
		return w.sampleCacheBuildFn(ctx, w.ffmpegPath)
	}
	fi, err := os.Stat(w.ffmpegPath)
	if err != nil {
		return sampleCacheBuildIdentity{}, err
	}
	m := &w.buildIdentity
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ok && m.path == w.ffmpegPath && m.size == fi.Size() && m.modTime.Equal(fi.ModTime()) {
		return m.identity, nil
	}
	id, err := probeSampleCacheBuild(ctx, w.ffmpegPath)
	if err != nil {
		return sampleCacheBuildIdentity{}, err
	}
	m.path, m.size, m.modTime, m.identity, m.ok = w.ffmpegPath, fi.Size(), fi.ModTime(), id, true
	return id, nil
}

var errSampleCacheOff = errors.New("sample cache not applicable")

// sampleCacheRun carries what is constant across the units of one pipeline
// call. A nil *sampleCacheRun disables the cache for the call.
type sampleCacheRun struct {
	cache    *sampleCache
	build    sampleCacheBuildIdentity
	hardware string
	source   sampleCacheSource
}

type sampleCacheSource struct {
	SHA256     string  `json:"sha256"`
	Size       int64   `json:"size"`
	VideoIndex int     `json:"video_index"`
	BitDepth   int     `json:"bit_depth"`
	PixFmt     string  `json:"pix_fmt"`
	Resolution string  `json:"resolution"`
	FPS        float64 `json:"fps"`
	FieldOrder string  `json:"field_order"`
}

// sampleCacheKeyMaterial is everything that can change a stored evaluation.
// Its JSON encoding is hashed to form the key. Adding a field changes every
// key (a clean invalidation); bump sampleCacheSchema for changes to the stored
// value instead.
type sampleCacheKeyMaterial struct {
	Schema int                `json:"schema"`
	Source sampleCacheSource  `json:"source"`
	Window sampleCacheWindow  `json:"window"`
	Cand   sampleCacheCand    `json:"candidate"`
	Metric sampleCacheMetrics `json:"metrics"`
	// Capability is the worker capability fingerprint: FFmpeg and worker
	// versions, encoders, filters and the quality (libvmaf/model/CAMBI/HFR)
	// capabilities. Any worker upgrade therefore invalidates the cache.
	Capability string                   `json:"capability"`
	Build      sampleCacheBuildIdentity `json:"build"`
}

type sampleCacheWindow struct {
	Index    int      `json:"index"`
	Start    float64  `json:"start_seconds"`
	Duration float64  `json:"duration_seconds"`
	RefArgs  []string `json:"reference_args"`
	// Compact is the compact-chain evidence (chain and LUT digest) every
	// reference sample went through, or nil.
	Compact *transcode.CompactChainEvidence `json:"compact,omitempty"`
}

type sampleCacheCand struct {
	// Candidate is the request candidate with its label cleared: a label
	// names a result, it does not change the encode.
	Candidate   transcode.BenchmarkCandidate `json:"candidate"`
	Profile     string                       `json:"profile"`
	PixelFormat string                       `json:"pixel_format"`
	BitDepth    int                          `json:"bit_depth"`
	// EncodeArgs is the exact encoder argument list with placeholder paths and
	// before encoder thread parameters, which are bitstream-neutral by design.
	EncodeArgs []string `json:"encode_args"`
}

type sampleCacheMetrics struct {
	VMAF               bool     `json:"vmaf"`
	CAMBI              bool     `json:"cambi"`
	Model              string   `json:"model,omitempty"`
	LibvmafModel       string   `json:"libvmaf_model,omitempty"`
	Decimation         int      `json:"decimation"`
	WorstWindowSeconds float64  `json:"worst_window_seconds"`
	FrameThreshold     *float64 `json:"frame_threshold,omitempty"`
	VMAFArgs           []string `json:"vmaf_args,omitempty"`
	CAMBIArgs          []string `json:"cambi_args,omitempty"`
	CAMBIOutput        string   `json:"cambi_output,omitempty"`
	NoAutoConversion   bool     `json:"no_auto_conversion"`
}

// newSampleCacheRun decides whether this pipeline call may use the cache and
// gathers its call-wide key inputs. It returns nil (cache off, behavior
// identical to before the cache) when any precondition is unmet.
func (sh *pipelineShared) newSampleCacheRun(ctx context.Context, sourceSize int64) *sampleCacheRun {
	// The savings gate prunes by comparing measured sizes between families in
	// an order-dependent way; replaying cached units would change which
	// families it prunes, so it keeps its exact original semantics.
	if sh.savings != nil || sh.w == nil {
		return nil
	}
	cache := sh.w.newSampleCache()
	if cache == nil {
		return nil
	}
	// Only the modeled-VMAF (+ optional CAMBI) measurement is cached. Legacy
	// SSIM paths and unmodeled VMAF are not.
	if !sh.runVMAF || sh.vmafModel == nil || sh.runSSIM || sh.record.Quality == nil || sh.record.Quality.VMAF == nil {
		return nil
	}
	if sh.runCAMBI && (sh.qualityCaps == nil || sh.qualityCaps.CAMBIOutput == "") {
		return nil
	}
	sha, err := transcode.NormalizeSourceSHA256(sh.record.SourceSHA256)
	if err != nil || sha == "" || sh.capFingerprint == "" {
		return nil
	}
	build, err := sh.w.sampleCacheBuild(ctx)
	if err != nil {
		return nil
	}
	run := &sampleCacheRun{
		cache: cache,
		build: build,
		source: sampleCacheSource{
			SHA256:     sha,
			Size:       sourceSize,
			VideoIndex: sh.evidence.SourceVideoIndex,
			BitDepth:   sh.evidence.SourceBitDepth,
			PixFmt:     sh.evidence.SourcePixelFormat,
			Resolution: sh.evidence.SourceResolution,
			FPS:        sh.evidence.SourceFPS,
			FieldOrder: sh.evidence.SourceFieldOrder,
		},
	}
	for _, vc := range sh.vcs {
		if BenchmarkCandidateVideoCodecIsHardware(vc.candidate) {
			hw, err := sampleCacheHardwareIdentity(ctx)
			if err != nil || strings.TrimSpace(hw) == "" {
				return nil
			}
			run.hardware = hw
			break
		}
	}
	return run
}

// BenchmarkCandidateVideoCodecIsHardware reports whether the candidate's
// encode depends on the machine's hardware encoder rather than only on
// software and build.
func BenchmarkCandidateVideoCodecIsHardware(c transcode.BenchmarkCandidate) bool {
	return transcode.BenchmarkCandidateVideoCodec(c) == transcode.VideoCodecHEVCVideoToolbox
}

// key computes the cache key of one unit and the values a stored entry must
// match. ok is false when the unit cannot be keyed, which is a miss.
func (r *sampleCacheRun) key(sh *pipelineShared, u pipelineUnit) (key string, want sampleCacheExpect, ok bool) {
	vc := sh.vcs[u.vcOrd]
	window := sh.record.Samples[u.sampOrd]

	refArgs := BuildReferenceExtractionArgs("{source}", "{reference}", sh.evidence.SourceVideoIndex, window.StartSeconds, window.DurationSeconds)
	if sh.record.Compact != nil {
		chain, err := compactFilterForRun(sh.record.Compact)
		if err != nil {
			return "", want, false
		}
		if chain != "" {
			refArgs = withReferenceChain(refArgs, chain)
		}
	}
	encArgs, err := BuildCandidateEncodeArgs("{reference}", "{candidate}", vc.plan)
	if err != nil {
		return "", want, false
	}
	cand := vc.candidate
	cand.ID = ""
	vmafArgs, err := BuildVMAFModelArgs("{candidate}", "{reference}", "{log}", *sh.vmafModel, vc.bitDepth, sh.vmafDecimation, sampleCacheKeyThreads)
	if err != nil {
		return "", want, false
	}
	metrics := sampleCacheMetrics{
		VMAF:               true,
		CAMBI:              sh.runCAMBI,
		Model:              sh.vmafModel.ID,
		LibvmafModel:       sh.vmafModel.LibvmafModel,
		Decimation:         sh.vmafDecimation,
		WorstWindowSeconds: sh.record.Quality.VMAF.WorstWindowSeconds,
		FrameThreshold:     sh.record.Quality.VMAF.FrameThreshold,
		VMAFArgs:           vmafArgs,
		NoAutoConversion:   sh.evidence.SourceBitDepth > 8,
	}
	if sh.runCAMBI {
		metrics.CAMBIArgs = BuildCAMBIArgs("{candidate}", "{reference}", "{log}", vc.bitDepth, sampleCacheKeyThreads)
		metrics.CAMBIOutput = sh.qualityCaps.CAMBIOutput
	}
	build := r.build
	if BenchmarkCandidateVideoCodecIsHardware(vc.candidate) {
		build.Hardware = r.hardware
	}
	material := sampleCacheKeyMaterial{
		Schema: sampleCacheSchema,
		Source: r.source,
		Window: sampleCacheWindow{
			Index:    window.Index,
			Start:    window.StartSeconds,
			Duration: window.DurationSeconds,
			RefArgs:  refArgs,
			Compact:  sh.evidence.Compact,
		},
		Cand: sampleCacheCand{
			Candidate:   cand,
			Profile:     vc.profile,
			PixelFormat: vc.pixelFormat,
			BitDepth:    vc.bitDepth,
			EncodeArgs:  encArgs,
		},
		Metric:     metrics,
		Capability: sh.capFingerprint,
		Build:      build,
	}
	raw, err := json.Marshal(material)
	if err != nil {
		return "", want, false
	}
	sum := sha256.Sum256(raw)
	want = sampleCacheExpect{
		runVMAF:     true,
		model:       sh.vmafModel,
		decimation:  sh.vmafDecimation,
		windowIndex: window.Index,
		runCAMBI:    sh.runCAMBI,
		cambiOutput: metrics.CAMBIOutput,
	}
	return hex.EncodeToString(sum[:]), want, true
}

// replayCachedUnits fills the outcomes of every unit the sample cache already
// holds and returns the indexes that still have to be encoded and measured, in
// their original order. With the cache off it returns every unit.
//
// A replayed unit is complete: its encode and metric outcomes are those of the
// measurement that populated the cache, marked Reused, so selection sees
// exactly what a fresh measurement gives. The units are resolved in the
// progress reporter without feeding the search budget (ReuseUnits): replay
// takes milliseconds and is not search work.
func (sh *pipelineShared) replayCachedUnits(ctx context.Context, units []pipelineUnit) []int {
	pending := make([]int, 0, len(units))
	if sh.cache = sh.newSampleCacheRun(ctx, sh.sourceInitialSize); sh.cache == nil {
		for i := range units {
			pending = append(pending, i)
		}
		return pending
	}
	sh.cacheKeys = make([]string, len(units))
	for idx, u := range units {
		key, want, ok := sh.cache.key(sh, u)
		if !ok {
			pending = append(pending, idx)
			continue
		}
		sh.cacheKeys[idx] = key
		body, stats, hit := sh.cache.cache.lookup(key, want)
		if !hit {
			pending = append(pending, idx)
			continue
		}
		sh.fillReplayedUnit(idx, u, body, stats)
		sh.progressReporter.ReuseUnits(1 + sh.passesPerSample)
	}
	return pending
}

// fillReplayedUnit turns a validated cache entry into unit outcomes.
func (sh *pipelineShared) fillReplayedUnit(idx int, u pipelineUnit, body *sampleCacheBody, stats *quality.VMAFStats) {
	vc := sh.vcs[u.vcOrd]
	window := sh.record.Samples[u.sampOrd]
	sh.encodeOut[idx] = pipelineEncodeOutcome{entry: BenchmarkCandidateSampleResult{
		CandidateID:        vc.candidate.ID,
		SampleIndex:        window.Index,
		SizeBytes:          body.SizeBytes,
		EncodeDurationSec:  body.EncodeDurationSec,
		Quality:            vc.candidate.Quality,
		AverageBitrateKbps: vc.candidate.AverageBitrateKbps,
		VideoProfile:       vc.profile,
		PixelFormat:        vc.pixelFormat,
		Reused:             true,
	}}
	mean := body.VMAF.Mean
	out := pipelineMetricOutcome{
		executed:   true,
		vmaf:       &mean,
		vmafStats:  stats,
		vmafDurSec: body.VMAFDurationSec,
		vmafScores: []optimization.SampleScore{{SampleIndex: window.Index, Score: mean, Valid: true}},
	}
	if body.CAMBI != nil {
		cambi := *body.CAMBI
		out.cambi = &cambi
		out.cambiDurSec = body.CAMBIDurationSec
	}
	out.entry = &BenchmarkMetricSampleResult{
		CandidateID:       vc.candidate.ID,
		CandidateIndex:    vc.index,
		SampleIndex:       window.Index,
		VMAF:              out.vmaf,
		VMAFStats:         out.vmafStats,
		CAMBI:             out.cambi,
		CAMBIDurationSec:  out.cambiDurSec,
		VMAFDurationSec:   out.vmafDurSec,
		MetricDurationSec: out.vmafDurSec + out.cambiDurSec,
		Reused:            true,
	}
	sh.metricOut[idx] = out
}

// storeCachedUnit records a freshly measured unit. Only a unit whose encode
// and every enabled metric fully succeeded is stored.
func (sh *pipelineShared) storeCachedUnit(idx int) {
	if sh.cache == nil || idx >= len(sh.cacheKeys) || sh.cacheKeys[idx] == "" {
		return
	}
	enc, met := sh.encodeOut[idx].entry, sh.metricOut[idx]
	if sh.encodeOut[idx].err != nil || enc.Error != "" || enc.Reused || enc.SizeBytes <= 0 ||
		!met.executed || met.vmaf == nil || met.vmafStats == nil || met.entry == nil || met.entry.Error != "" || met.entry.Reused {
		return
	}
	scores := met.vmafStats.FrameScores()
	if len(scores) == 0 || met.vmafStats.Mean != *met.vmaf {
		return
	}
	body := sampleCacheBody{
		SizeBytes:         enc.SizeBytes,
		EncodeDurationSec: enc.EncodeDurationSec,
		VMAF:              &sampleCacheVMAF{Mean: *met.vmaf, Stats: *met.vmafStats, FrameScores: encodeFrameScores(scores)},
		VMAFDurationSec:   met.vmafDurSec,
	}
	if sh.runCAMBI {
		if met.cambi == nil || met.cambiErr != "" {
			return
		}
		cambi := *met.cambi
		body.CAMBI, body.CAMBIDurationSec = &cambi, met.cambiDurSec
	}
	sh.cache.cache.store(sh.cacheKeys[idx], body)
}
