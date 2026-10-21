package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/transcode/quality"
)

// BuildReferenceExtractionArgs constructs the exact, safe FFmpeg argument slice
// for extracting a frame-aligned, timestamp-normalized, lossless reference sample.
//
// Frame alignment & windowing rationale:
//  1. Fast & frame-accurate seeking: Placing `-accurate_seek -ss <startSec>` before `-i`
//     enables demuxer keyframe seeking immediately before the target timestamp, followed
//     by accurate frame-by-frame decoding and discarding up to the requested point. This avoids
//     decoding the entire media file from time 0 while guaranteeing deterministic frame boundaries.
//  2. Deterministic window duration: `-t <durationSec>` extracts the requested window,
//     which is deterministically frame-aligned and frame-quantized for VFR/timebase sources
//     rather than a mathematically continuous floating-point cut.
//  3. PTS normalization: `-avoid_negative_ts make_zero` resets stream and container timestamps
//     so that the extracted sample starts cleanly at PTS 0.
//  4. Lossless master: `-c:v ffv1` encodes losslessly, preserving raw decoded pixel format,
//     bit depth, and frame cadence with zero generational loss.
//     Source timestamps pass through unchanged (sourceTimestampArgs), so a
//     window cut from the source and one cut from its encode hold the same frames.
//  5. Clean elementary stream: `-an -sn -dn` strips audio, subtitles, and data streams.
//  6. Deterministic candidate alignment: Candidate samples are subsequently encoded from this
//     FFV1 reference master from frame 0 to end without seeking or trimming. While the source
//     window itself is frame-quantized, the candidate-to-reference frame correspondence is
//     strictly 1:1 and exact for downstream VMAF/SSIM metric evaluation.
func BuildReferenceExtractionArgs(sourcePath, refPath string, videoIndex int, startSec, durationSec float64) []string {
	return []string{
		"-y",
		"-nostats",
		"-accurate_seek",
		"-ss", fmt.Sprintf("%.6f", startSec),
		"-i", sourcePath,
		"-t", fmt.Sprintf("%.6f", durationSec),
		"-avoid_negative_ts", "make_zero",
		"-map", fmt.Sprintf("0:%d", videoIndex),
		"-c:v", "ffv1",
		sourceTimestampArgs[0], sourceTimestampArgs[1],
		sourceTimestampArgs[2], sourceTimestampArgs[3],
		"-an",
		"-sn",
		"-dn",
		refPath,
	}
}

// isSupportedSourceChroma returns true if the source pixel format has safe 4:2:0 chroma subsampling.
// Full-range yuvj420p is deferred until Phase 5 range-normalized metric pipeline support to avoid
// implicit full-to-limited range conversion discrepancies during VideoToolbox candidate comparison.
func isSupportedSourceChroma(pixFmt string, bitDepth int) bool {
	p := strings.ToLower(strings.TrimSpace(pixFmt))
	if bitDepth == 8 {
		switch p {
		case "yuv420p", "nv12":
			return true
		}
		return false
	}
	if bitDepth == 10 {
		switch p {
		case "yuv420p10le", "p010le":
			return true
		}
		return false
	}
	return false
}

// tailBuffer retains up to maxBytes of trailing output.
// It is thread-safe and preserves the end of process stdout/stderr diagnostics
// where summary lines and final aggregate statistics are printed.
type tailBuffer struct {
	mu       sync.Mutex
	buf      []byte
	maxBytes int
	total    int64
}

func newTailBuffer(maxBytes int) *tailBuffer {
	if maxBytes <= 0 {
		maxBytes = 16 * 1024
	}
	return &tailBuffer{
		buf:      make([]byte, 0, maxBytes),
		maxBytes: maxBytes,
	}
}

func (t *tailBuffer) Write(p []byte) (n int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n = len(p)
	t.total += int64(n)
	if len(p) >= t.maxBytes {
		t.buf = append(t.buf[:0], p[len(p)-t.maxBytes:]...)
		return n, nil
	}
	overflow := (len(t.buf) + len(p)) - t.maxBytes
	if overflow > 0 {
		copy(t.buf, t.buf[overflow:])
		t.buf = t.buf[:len(t.buf)-overflow]
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// MaxMetricLogSizeBytes defines the maximum allowed size of a metric log or stats file (5 MB)
// to prevent denial-of-service / memory exhaustion attacks.
const MaxMetricLogSizeBytes = 5 * 1024 * 1024

// escapeFFmpegFilterPath escapes an absolute filesystem path for safe use as a filter option
// value in an FFmpeg -filter_complex argument.
// FFmpeg filtergraph evaluation involves two distinct unescaping stages:
//  1. Filtergraph syntax parsing (avfilter_graph_parse2), which unescapes backslashes preceding
//     special syntax characters such as ':', '\”, '\\', '[', ']', ';', ','.
//  2. Filter option key=value parsing (av_opt_set / av_set_options_string).
//
// To safely survive both stages without unintended option splitting or quote stripping:
// - Backslash '\' becomes 4 backslashes "\\\\" (resolves to "\\" after stage 1, then "\" after stage 2)
// - Single quote '\” becomes 3 backslashes + quote "\\\'" (resolves to "\'" after stage 1, then "'" after stage 2)
// - Colon ':' becomes 2 backslashes + colon "\\:" (resolves to "\:" after stage 1, preventing option splitting, then ":" after stage 2)
// - Brackets '[', ']', commas ',', and semicolons ';' are prefixed with "\\".
func escapeFFmpegFilterPath(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\\\`)
		case '\'':
			b.WriteString(`\\\'`)
		case ':':
			b.WriteString(`\\:`)
		case '[', ']', ';', ',':
			b.WriteString(`\\`)
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// BuildVMAFModelArgs makes the model and measurement precision explicit. The
// encoded files retain their own bit depths; conversion exists only in the
// metric graph. CAMBI must still know the encoded depth: otherwise an 8-bit
// encode upconverted for measurement is incorrectly evaluated as 10-bit.
// framesync must end at the shorter input without repeats.
// threads > 0 sets libvmaf n_threads; zero keeps the libvmaf default.
// decimation (quality.TemporalDecimation) drops the same frame indices from
// both inputs; both are then restamped by index (quality.MetricPairInputs).
func BuildVMAFModelArgs(candPath, refPath, logPath string, model quality.VMAFModel, encodedBitDepth, decimation, threads int) ([]string, error) {
	if model.ID == "" || model.LibvmafModel == "" || model.MeasurementBitDepth != 10 || (encodedBitDepth != 8 && encodedBitDepth != 10) {
		return nil, fmt.Errorf("unsupported VMAF model measurement contract")
	}
	inputs, err := quality.MetricPairInputs(decimation)
	if err != nil {
		return nil, err
	}
	filter := inputs + vmafModelFilter(model, encodedBitDepth, logPath) + libvmafThreadsOption(threads)
	return []string{"-nostdin", "-nostats", "-i", candPath, "-i", refPath, "-filter_complex", filter, "-f", "null", "-"}, nil
}

// BuildCAMBIArgs runs a separate, bounded full-reference pass. VMAF v1 has an
// internal CAMBI feature, so adding another to that invocation is unsafe on
// several libvmaf builds. CAMBI is spatial and measures every frame.
func BuildCAMBIArgs(candPath, refPath, logPath string, encodedBitDepth, threads int) []string {
	inputs, _ := quality.MetricPairInputs(1)
	filter := inputs + fmt.Sprintf("libvmaf=model=version=vmaf_v0.6.1:feature='name=cambi\\:full_ref=true\\:enc_bitdepth=%d':log_fmt=json:log_path=%s:shortest=1:repeatlast=0%s", encodedBitDepth, escapeFFmpegFilterPath(logPath), libvmafThreadsOption(threads))
	return []string{"-nostdin", "-nostats", "-i", candPath, "-i", refPath, "-filter_complex", filter, "-f", "null", "-"}
}

// vmafModelFilter is the libvmaf invocation shared by measurement and the
// capability probes, so a probe executes the production model options.
func vmafModelFilter(model quality.VMAFModel, encodedBitDepth int, logPath string) string {
	return fmt.Sprintf("libvmaf=model='version=%s\\:cambi.enc_bitdepth=%d':log_fmt=json:log_path=%s:shortest=1:repeatlast=0", model.LibvmafModel, encodedBitDepth, escapeFFmpegFilterPath(logPath))
}

func libvmafThreadsOption(threads int) string {
	if threads <= 0 {
		return ""
	}
	return fmt.Sprintf(":n_threads=%d", threads)
}

// metricThreads resolves libvmaf n_threads for one of concurrent simultaneous
// metric invocations. An explicit worker setting wins; otherwise the host CPUs
// are shared across the invocations of every video job running now, so one
// job alone uses them all. Thread counts never change metric values.
func (w *Worker) metricThreads(concurrent int) int {
	if w != nil && w.cfg != nil && w.cfg.MetricThreads > 0 {
		return w.cfg.MetricThreads
	}
	if concurrent < 1 {
		concurrent = 1
	}
	concurrent *= w.videoJobsSharingCPU()
	threads := hostCPUCount() / concurrent
	if threads < 1 {
		threads = 1
	}
	return threads
}

// BuildSSIMArgs constructs the exact, safe FFmpeg argument slice
// for calculating SSIM between a candidate sample (distorted, 0:v)
// and an FFV1 reference sample (reference, 1:v).
func BuildSSIMArgs(candPath, refPath, statsPath string) []string {
	// Paired by frame index like VMAF/CAMBI, at the inputs' own precision.
	filter := quality.IndexPairInputs() + "ssim"
	if statsPath != "" {
		filter += "=stats_file=" + escapeFFmpegFilterPath(statsPath)
	}
	return []string{
		"-nostats",
		"-i", candPath,
		"-i", refPath,
		"-filter_complex", filter,
		"-f", "null",
		"-",
	}
}

// ParseSSIMStatsFile parses an FFmpeg ssim stats_file output, averaging the per-frame All scores.
func ParseSSIMStatsFile(data []byte) (float64, error) {
	if len(data) == 0 {
		return 0, errors.New("empty ssim stats data (fail closed)")
	}
	if len(data) > MaxMetricLogSizeBytes {
		return 0, fmt.Errorf("oversized ssim stats data (%d bytes exceeds %d max) (fail closed)", len(data), MaxMetricLogSizeBytes)
	}

	lines := strings.Split(string(data), "\n")
	var sum float64
	var count int

	re := regexp.MustCompile(`\bAll:([0-9.]+|nan|inf|-inf)\b`)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		matches := re.FindStringSubmatch(line)
		if len(matches) < 2 {
			continue
		}
		valStr := matches[1]
		if strings.EqualFold(valStr, "nan") || strings.EqualFold(valStr, "inf") || strings.EqualFold(valStr, "-inf") {
			return 0, errors.New("ssim frame score is NaN or Inf (fail closed)")
		}
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing ssim frame score %q: %w (fail closed)", valStr, err)
		}
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return 0, errors.New("ssim frame score is NaN or Inf (fail closed)")
		}
		if val < 0.0 || val > 1.0 {
			return 0, fmt.Errorf("ssim frame score %v out of range [0, 1] (fail closed)", val)
		}
		sum += val
		count++
	}

	if count == 0 {
		return 0, errors.New("no valid ssim frames found in stats file (fail closed)")
	}

	avg := sum / float64(count)
	if math.IsNaN(avg) || math.IsInf(avg, 0) {
		return 0, errors.New("ssim average score is NaN or Inf (fail closed)")
	}
	if avg < 0.0 || avg > 1.0 {
		return 0, fmt.Errorf("ssim average score %v out of range [0, 1] (fail closed)", avg)
	}

	return avg, nil
}

// ParseSSIMStderr extracts aggregate SSIM score from FFmpeg stderr output, choosing the last match (final aggregate).
func ParseSSIMStderr(stderr string) (float64, error) {
	if strings.TrimSpace(stderr) == "" {
		return 0, errors.New("empty stderr output (fail closed)")
	}
	re := regexp.MustCompile(`SSIM\s+.*?\bAll:([0-9.]+|nan|inf|-inf)\b`)
	allMatches := re.FindAllStringSubmatch(stderr, -1)
	if len(allMatches) == 0 {
		return 0, errors.New("ssim aggregate score not found in stderr (fail closed)")
	}
	// Select the last match which represents the final summary
	lastMatch := allMatches[len(allMatches)-1]
	valStr := lastMatch[1]
	if strings.EqualFold(valStr, "nan") || strings.EqualFold(valStr, "inf") || strings.EqualFold(valStr, "-inf") {
		return 0, errors.New("ssim aggregate score is NaN or Inf (fail closed)")
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing ssim score %q: %w (fail closed)", valStr, err)
	}
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, errors.New("ssim aggregate score is NaN or Inf (fail closed)")
	}
	if val < 0.0 || val > 1.0 {
		return 0, fmt.Errorf("ssim aggregate score %v out of range [0, 1] (fail closed)", val)
	}
	return val, nil
}

func readMetricLogFile(logPath string) ([]byte, error) {
	f, err := openNoFollow(logPath)
	if err != nil {
		return nil, fmt.Errorf("open metric log %s: %w (symlinks or unreadable files rejected, fail closed)", logPath, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat open metric log %s: %w (fail closed)", logPath, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("metric log %s is not a regular file (symlink or special file rejected, fail closed)", logPath)
	}
	if fi.Size() == 0 {
		return nil, fmt.Errorf("metric log %s is empty (fail closed)", logPath)
	}
	if fi.Size() > MaxMetricLogSizeBytes {
		return nil, fmt.Errorf("metric log %s size %d exceeds maximum allowed %d (fail closed)", logPath, fi.Size(), MaxMetricLogSizeBytes)
	}

	lr := io.LimitReader(f, int64(MaxMetricLogSizeBytes)+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("read metric log %s: %w (fail closed)", logPath, err)
	}
	if int64(len(data)) > int64(MaxMetricLogSizeBytes) {
		return nil, fmt.Errorf("metric log %s exceeded limit of %d bytes while reading (fail closed)", logPath, MaxMetricLogSizeBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("metric log %s contained 0 bytes after read (fail closed)", logPath)
	}
	return data, nil
}

// withReferenceChain inserts the compact picture chain into reference
// extraction args, ahead of the lossless FFV1 encode.
func withReferenceChain(args []string, chain string) []string {
	out := make([]string, 0, len(args)+2)
	inserted := false
	for _, a := range args {
		if a == "-c:v" && !inserted {
			out = append(out, "-filter:v:0", chain)
			inserted = true
		}
		out = append(out, a)
	}
	return out
}

func runCAMBIMetricSample(ctx context.Context, ffmpegPath, samplesDir, candPath, refPath string, candIdx int, candidateID string, sampleIdx int, sourceStartSec, fps float64, encodedBitDepth int, outputMetric string, threads int) (quality.CAMBIStats, error) {
	logPath, err := derivedMetricLogPath(samplesDir, "cambi", candIdx, candidateID, sampleIdx)
	if err != nil {
		return quality.CAMBIStats{}, err
	}
	if err := prepareOutputFile(logPath); err != nil {
		return quality.CAMBIStats{}, err
	}
	if encodedBitDepth != 8 && encodedBitDepth != 10 {
		return quality.CAMBIStats{}, fmt.Errorf("unsupported CAMBI encoded bit depth %d", encodedBitDepth)
	}
	args := BuildCAMBIArgs(candPath, refPath, logPath, encodedBitDepth, threads)
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	stderrBuf := newTailBuffer(64 * 1024)
	cmd.Stderr = stderrBuf
	if err := runCharged(ctx, cmd); err != nil {
		return quality.CAMBIStats{}, fmt.Errorf("ffmpeg CAMBI full-reference failed: %v: %s", err, stderrBuf.String())
	}
	data, err := readMetricLogFile(logPath)
	if err != nil {
		return quality.CAMBIStats{}, err
	}
	return quality.ParseCAMBIFullReference(data, MaxMetricLogSizeBytes, outputMetric, sampleIdx, sourceStartSec, fps)
}

// prepareOutputFile verifies that the target path does not pre-exist as a symlink.
// If a regular file pre-exists at the target path, it is removed explicitly before FFmpeg runs
// to prevent FFmpeg's -y flag from following pre-existing symlinks or writing into existing inodes.
func prepareOutputFile(targetPath string) error {
	fi, err := os.Lstat(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking target output %s: %w", targetPath, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("target output %s is a symlink (fail closed)", targetPath)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("target output %s is not a regular file (fail closed)", targetPath)
	}
	if err := os.Remove(targetPath); err != nil {
		return fmt.Errorf("removing existing target file %s: %w", targetPath, err)
	}
	return nil
}

func verifyChildPath(parentDir, targetPath string) error {
	cleanParent := filepath.Clean(parentDir)
	cleanTarget := filepath.Clean(targetPath)

	rel, err := filepath.Rel(cleanParent, cleanTarget)
	if err != nil {
		return fmt.Errorf("path %s cannot be related to %s: %w", targetPath, parentDir, err)
	}
	if rel == "." || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") || strings.Contains(rel, "\\") {
		return fmt.Errorf("target %s escapes parent %s (rel: %s)", targetPath, parentDir, rel)
	}

	if resolvedParent, err := filepath.EvalSymlinks(cleanParent); err == nil {
		targetDir := filepath.Dir(cleanTarget)
		if resolvedDir, err := filepath.EvalSymlinks(targetDir); err == nil {
			relResolved, err := filepath.Rel(resolvedParent, resolvedDir)
			if err != nil || (relResolved == "." && filepath.Clean(targetDir) != cleanParent) || strings.HasPrefix(relResolved, "..") {
				return fmt.Errorf("resolved target dir %s escapes resolved parent %s", resolvedDir, resolvedParent)
			}
		}
	}
	return nil
}

// derivedMetricLogPath constructs a collision-free, verified scratch path for metric logs
// derived from candidate index, sample index, safe hash, and metric type.
func derivedMetricLogPath(samplesDir string, metric string, candIdx int, candidateID string, sampleIdx int) (string, error) {
	h := sha256.Sum256([]byte(candidateID))
	shortHash := hex.EncodeToString(h[:4]) // 8 hex chars
	var ext string
	if metric == "vmaf" {
		ext = "json"
	} else {
		ext = "log"
	}
	filename := fmt.Sprintf("metric_%s_cand_%d_%s_sample_%d.%s", metric, candIdx, shortHash, sampleIdx, ext)
	logPath := filepath.Join(samplesDir, filename)
	if err := verifyChildPath(samplesDir, logPath); err != nil {
		return "", fmt.Errorf("resolving metric log path %s: %w", filename, err)
	}
	return logPath, nil
}
