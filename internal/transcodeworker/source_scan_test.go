package transcodeworker

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

func scanTestTools(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err = exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	return ffmpeg, ffprobe
}

func scanTestMedia(t *testing.T, ffmpeg string, args ...string) {
	t.Helper()
	if out, err := exec.Command(ffmpeg, append([]string{"-nostdin", "-v", "error"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("creating test media: %v: %s", err, out)
	}
}

func TestScanSourceVideoHealthyFileHasNoDemuxErrors(t *testing.T) {
	ffmpeg, ffprobe := scanTestTools(t)
	src := filepath.Join(t.TempDir(), "src.mkv")
	scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=6", "-c:v", "mpeg4", "-y", src)
	scan, err := scanSourceVideo(context.Background(), ffprobe, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if scan.DemuxErrors != 0 || scan.damage(1) != nil {
		t.Fatalf("healthy file reported damage: %+v", scan)
	}
	if scan.Packets != 144 || math.Abs(scan.Seconds()-6) > 0.05 {
		t.Fatalf("scan = %+v (%.3fs), want 144 packets over 6s", scan, scan.Seconds())
	}
}

// Zeroed bytes in the middle of a Matroska file are what the damaged library
// files held: the demuxer reports them while reading, the job fails before
// the encode with a class no retry gets past, and the message says where.
func TestScanSourceVideoReportsDamageBeforeTheEncode(t *testing.T) {
	ffmpeg, ffprobe := scanTestTools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=20", "-c:v", "mpeg4", "-q:v", "5", "-y", src)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	at := len(data) * 2 / 5
	clear(data[at : at+16384])
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	scan, err := scanSourceVideo(context.Background(), ffprobe, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if scan.DemuxErrors == 0 || scan.FirstErrorByte < int64(at) || scan.FirstErrorByte > int64(at+16384) {
		t.Fatalf("scan = %+v, want demux errors from byte %d", scan, at)
	}
	derr := scan.damage(int64(len(data)))
	if derr == nil {
		t.Fatal("damaged file passed")
	}
	class := resilience.ClassifyError(derr)
	if class != resilience.FFmpegInputCorrupt || resilience.Retryable(class, []string{string(class)}) {
		t.Fatalf("class = %s, want non-retryable %s", class, resilience.FFmpegInputCorrupt)
	}
	if resilience.Classify(derr.Error()) != resilience.FFmpegInputCorrupt {
		t.Fatalf("the persisted message classifies as %s", resilience.Classify(derr.Error()))
	}
	if msg := derr.Error(); !strings.Contains(msg, "40% into the file") || strings.Contains(msg, " @ 0x") {
		t.Fatalf("message = %q, want the position and no log context address", msg)
	}
}

// A container that runs past its video (a late subtitle here, an attachment
// in the library file) has no pictures to extract for a window placed there.
func TestFinalWindowsPastTheVideoFailBeforeTheEncode(t *testing.T) {
	ffmpeg, ffprobe := scanTestTools(t)
	dir := t.TempDir()
	srt := filepath.Join(dir, "late.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:08,000 --> 00:00:10,000\nlate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src.mkv")
	scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=4", "-i", srt, "-map", "0", "-map", "1", "-c:v", "mpeg4", "-c:s", "srt", "-y", src)
	scan, err := scanSourceVideo(context.Background(), ffprobe, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(scan.Seconds()-4) > 0.05 {
		t.Fatalf("video seconds = %.3f, want 4", scan.Seconds())
	}
	plan := &transcode.Plan{QualityValidation: &transcode.QualityValidationPlan{Samples: []transcode.BenchmarkSampleWindow{
		{Index: 0, StartSeconds: 1, DurationSeconds: 2},
		{Index: 1, StartSeconds: 6, DurationSeconds: 2},
	}}}
	err = finalWindowsFitVideo(plan, scan)
	if err == nil || !strings.Contains(err.Error(), "final window 1 ends 8.0s") || resilience.ClassifyError(err) != resilience.InternalInvariant {
		t.Fatalf("err = %v (%s), want window 1 refused as an internal invariant", err, resilience.ClassifyError(err))
	}
	plan.QualityValidation.Samples = plan.QualityValidation.Samples[:1]
	if err := finalWindowsFitVideo(plan, scan); err != nil {
		t.Fatalf("a window inside the video was refused: %v", err)
	}
}

// A demuxer reports, at error level, header it does not understand in files
// that play fine; only errors naming damaged bytes count.
func TestPacketDamagePatternIgnoresHeaderNoise(t *testing.T) {
	for _, text := range []string{
		"0x00 at pos 738436 (0xb4484) invalid as first byte of an EBML number",
		"Length 9 indicated by an EBML number's first byte 0x01 at pos 1 exceeds max length 8.",
		"Packet corrupt (stream = 0, dts = 1234)",
		"stream 0, offset 0x1a8bb6f: partial file",
	} {
		if !packetDamagePattern.MatchString(text) {
			t.Errorf("damage not recognised: %q", text)
		}
	}
	for _, text := range []string{
		"Referenced QT chapter track not found",
		"Something went wrong during header parsing, tag[0] has size 0, I will ignore it and try to continue anyway.",
		"Element at 0x1f8 ending at 0x2a0 exceeds containing master element ending at 0x29a",
	} {
		if packetDamagePattern.MatchString(text) {
			t.Errorf("header noise counted as damage: %q", text)
		}
	}
}

// A recording whose clock restarts has timestamps that no longer measure its
// length; the window check stands aside instead of failing a healthy file.
func TestFinalWindowsCheckSkipsARestartedTimeline(t *testing.T) {
	ffmpeg, ffprobe := scanTestTools(t)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.ts"), filepath.Join(dir, "b.ts")
	scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=20", "-c:v", "mpeg2video", "-output_ts_offset", "1000", "-y", a)
	// The second recording restarts inside the first's range. ffprobe reads a
	// jump back of more than a minute in a transport stream as a clock wrap
	// and keeps counting up, which only lengthens the timeline.
	scanTestMedia(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=10", "-c:v", "mpeg2video", "-output_ts_offset", "1005", "-y", b)
	joined := filepath.Join(dir, "joined.ts")
	first, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(joined, append(first, second...), 0o644); err != nil {
		t.Fatal(err)
	}
	scan, err := scanSourceVideo(context.Background(), ffprobe, joined, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !scan.Discontinuous {
		t.Fatalf("scan = %+v, want the restart seen", scan)
	}
	plan := &transcode.Plan{QualityValidation: &transcode.QualityValidationPlan{Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 25, DurationSeconds: 2}}}}
	if err := finalWindowsFitVideo(plan, scan); err != nil {
		t.Fatalf("a restarted timeline failed the window check: %v", err)
	}
}

func TestDemuxerTagAndLineWriter(t *testing.T) {
	tag, text, ok := demuxerTag("[matroska,webm @ 0x7cbd030000] 0x00 at pos 738436 (0xb4484) invalid")
	if !ok || tag != "matroska,webm" || text != "0x00 at pos 738436 (0xb4484) invalid" {
		t.Fatalf("demuxerTag = %q %q %v", tag, text, ok)
	}
	if _, _, ok := demuxerTag("/path/src.mkv: Invalid data found"); ok {
		t.Fatal("a line with no log context was tagged")
	}
	var lines []string
	w := &lineWriter{line: func(s string) { lines = append(lines, s) }}
	for _, chunk := range []string{"pack", "et|a\r\nformat|b\n", "tail"} {
		_, _ = w.Write([]byte(chunk))
	}
	w.flush()
	if strings.Join(lines, ";") != "packet|a;format|b;tail" {
		t.Fatalf("lines = %q", lines)
	}
}

// The scan runs after the probe and before the encode: a damaged source never
// reaches the encoder and its job carries the corrupt-input class.
func TestDamagedSourceFailsTheJobWithoutEncoding(t *testing.T) {
	tempDir := t.TempDir()
	src := pr6b1WriteFile(t, filepath.Join(tempDir, "src.mkv"))
	cfg := pr6b2Config(tempDir, nil)
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{createOut: true}
	rec.install(w)
	var scanned []int
	w.scanVideo = func(ctx context.Context, path string, streamIndex int) (sourceVideoScan, error) {
		scanned = append(scanned, streamIndex)
		return sourceVideoScan{Packets: 10, End: 10, DemuxErrors: 3, FirstError: "[matroska,webm] 0x00 at pos 5 invalid", FirstErrorByte: 5}, nil
	}

	const id = "job-damaged-source"
	pr6b2Seed(t, cfg.StateDir, &JobRecord{
		ID: id, Status: "queued", Source: src, Candidate: filepath.Join(tempDir, "out.mkv"), Profile: "hevc-vt",
		Plan: pr6b2Plan(t), ExecutionSpecDigest: pr5Digest, CreatedAt: time.Now().UTC(),
	})
	if err := pr6b2Run(t, w, id); err == nil {
		t.Fatal("damaged source completed")
	}
	if rec.encodeCalls != 0 || len(scanned) != 1 || scanned[0] != 0 {
		t.Fatalf("encode calls = %d, scans = %v; want the scan of stream 0 and no encode", rec.encodeCalls, scanned)
	}
	got := pr6b1LoadJob(t, cfg.StateDir, id)
	if got.Status != "failed" || got.FailureClassification != string(resilience.FFmpegInputCorrupt) || !strings.Contains(got.Error, "3 errors") {
		t.Fatalf("job = %s / %s / %q, want failed as ffmpeg_input_corrupt", got.Status, got.FailureClassification, got.Error)
	}
	if _, ok := got.PhaseCosts["source_scan"]; !ok {
		t.Fatalf("phase costs = %v, want source_scan", got.PhaseCosts)
	}
}
