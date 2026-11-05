package transcodeworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// scanWith runs scanSourceVideo over a fake ffprobe that prints the packets of
// a short video and then stderr.
func scanWith(t *testing.T, stderr string) (sourceVideoScan, error) {
	t.Helper()
	dir := t.TempDir()
	ffprobe := filepath.Join(dir, "ffprobe")
	body := "#!/bin/sh\ncat <<'OUT'\npacket|pts_time=0.000000|duration_time=0.040000\npacket|pts_time=4.000000|duration_time=0.040000\nformat|format_name=matroska,webm\nOUT\ncat >&2 <<'ERR'\n" + stderr + "ERR\n"
	if err := os.WriteFile(ffprobe, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return scanSourceVideo(context.Background(), ffprobe, filepath.Join(dir, "a.mkv"), 0)
}

// A read the I/O layer failed names a position like damaged bytes do, but it
// says nothing about the file: ffprobe ends the scan there as if the file ended.
// The job fails as a read problem a retry can get past, not as a damaged file.
func TestScanSourceVideoTreatsAFailedReadAsAnIOErrorNotDamage(t *testing.T) {
	scan, err := scanWith(t, "[matroska,webm @ 0x7f] Read error at pos. 4021 (0xfb5)\n")
	if err == nil {
		t.Fatalf("a scan cut short by a failed read judged the file: %+v", scan)
	}
	if scan.DemuxErrors != 0 {
		t.Fatalf("a failed read counted as damage: %+v", scan)
	}
	message := "scanning source video packets: " + err.Error()
	if class := resilience.Classify(message); class != resilience.StorageIOTransient {
		t.Fatalf("class = %s, want a retryable read problem: %s", class, message)
	}
	if !strings.Contains(err.Error(), "Read error at pos. 4021") || !strings.Contains(err.Error(), "judged nothing") {
		t.Fatalf("the error lost what the demuxer said: %v", err)
	}
}

// A run whose read failed stopped where the read did, and what it printed
// before may be that failure's aftermath (a packet cut short by the read, not a
// bad one). The job is retried as a read problem; if the file is damaged, the
// next scan that reads it through says so without a failed read.
func TestScanSourceVideoJudgesNothingWhenAReadFailedAfterDamage(t *testing.T) {
	scan, err := scanWith(t, "[matroska,webm @ 0x7f] Packet corrupt (stream = 0, dts = 42).\n[matroska,webm @ 0x7f] Read error at pos. 4021 (0xfb5)\n")
	if err == nil {
		t.Fatalf("a scan whose read failed judged the file: %+v", scan)
	}
	if class := resilience.Classify("scanning source video packets: " + err.Error()); class != resilience.StorageIOTransient {
		t.Fatalf("class = %s, want a retryable read problem: %v", class, err)
	}
	// The same damage with the read intact is the file's.
	scan, err = scanWith(t, "[matroska,webm @ 0x7f] Packet corrupt (stream = 0, dts = 42).\n")
	if err != nil || scan.DemuxErrors != 1 || sourceDamagedClass(scan.damage(10000)) != resilience.FFmpegInputCorrupt {
		t.Fatalf("scan = %+v err = %v", scan, err)
	}
}

// A failed read counts as one whichever context logged it, or none: the line
// carries no "[context @ address]" when the I/O layer logs it itself.
func TestScanSourceVideoJudgesNothingWhenAnUntaggedReadFailed(t *testing.T) {
	scan, err := scanWith(t, "[matroska,webm @ 0x7f] Packet corrupt (stream = 0, dts = 42).\nRead error at pos. 4021 (0xfb5)\n")
	if err == nil {
		t.Fatalf("a scan whose read failed judged the file: %+v", scan)
	}
	if class := resilience.Classify("scanning source video packets: " + err.Error()); class != resilience.StorageIOTransient {
		t.Fatalf("class = %s, want a retryable read problem: %v", class, err)
	}
	if !strings.Contains(err.Error(), "failed 1 time(s)") || !strings.Contains(err.Error(), "Read error at pos. 4021") {
		t.Fatalf("the error lost what the demuxer said: %v", err)
	}
}

// A file that ends in the middle of an element is damaged.
func TestScanSourceVideoCountsATruncatedFileAsDamage(t *testing.T) {
	scan, err := scanWith(t, "[matroska,webm @ 0x7f] File ended prematurely at pos. 4021 (0xfb5)\n")
	if err != nil {
		t.Fatal(err)
	}
	if scan.DemuxErrors != 1 || scan.FirstErrorByte != 4021 || sourceDamagedClass(scan.damage(10000)) != resilience.FFmpegInputCorrupt {
		t.Fatalf("scan = %+v", scan)
	}
}

func sourceDamagedClass(err error) resilience.FailureClass { return resilience.ClassifyError(err) }

func TestReadFaultPatternSeparatesAFailedReadFromABadByte(t *testing.T) {
	if !mediainspect.ReadFaultPattern.MatchString("Read error at pos. 4021 (0xfb5)") {
		t.Error("a failed read not recognised")
	}
	for _, text := range []string{"File ended prematurely at pos. 4021 (0xfb5)", "Packet corrupt (stream = 0, dts = 42)"} {
		if mediainspect.ReadFaultPattern.MatchString(text) {
			t.Errorf("damage read as a failed read: %q", text)
		}
	}
}
