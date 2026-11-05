package mediainspect

import (
	"context"
	"strings"
	"testing"
)

func TestDemuxerTagDropsTheContextAddress(t *testing.T) {
	tag, text, ok := DemuxerTag("[matroska,webm @ 0x7f8a1c] EBML header parsing failed")
	if !ok || tag != "matroska,webm" || text != "EBML header parsing failed" {
		t.Fatalf("%q %q %v", tag, text, ok)
	}
	for _, line := range []string{"", "Invalid data found when processing input", "[] no tag", "[unclosed"} {
		if _, _, ok := DemuxerTag(line); ok {
			t.Errorf("%q read as a tagged line", line)
		}
	}
}

const scanPackets = "packet|pts_time=0.083000|duration_time=0.041000\npacket|pts_time=0.000000|duration_time=0.041000\npacket|pts_time=10.000000|duration_time=0.500000\nformat|format_name=matroska,webm\n"

// Only the container's own demuxer counts. Errors that name damaged bytes are
// damage; any other line of the container's or of no context is not proof of a
// clean read (ffprobe ends a read that fails mid-file as if the file ended);
// another context's complaint is neither.
func TestScanDemuxDamageSortsWhatTheDemuxerSaid(t *testing.T) {
	for name, tc := range map[string]struct {
		stderr       string
		errors       int
		first        string
		unrecognised int
		firstOther   string
	}{
		"healthy":                    {"", 0, "", 0, ""},
		"damaged":                    {"[matroska,webm @ 0x1] EBML header parsing failed\n[matroska,webm @ 0x1] Packet corrupt (stream = 0, dts = 42).\n", 2, "[matroska,webm] EBML header parsing failed", 0, ""},
		"another context":            {"[h264 @ 0x2] corrupt decoded frame\n", 0, "", 0, ""},
		"metadata the demuxer skips": {"[matroska,webm @ 0x1] Unknown-sized element in the chapters\n", 0, "", 1, "[matroska,webm] Unknown-sized element in the chapters"},
		// ffprobe takes a read error for the end of the file and exits 0.
		"a read that failed":          {"[matroska,webm @ 0x1] Read error at pos. 4021 (0xfb5)\n", 0, "", 1, "[matroska,webm] Read error at pos. 4021 (0xfb5)"},
		"a line of no context":        {"Input/output error\n", 0, "", 1, "Input/output error"},
		"last line without a newline": {"[matroska,webm @ 0x1] Read error at pos. 99", 0, "", 1, "[matroska,webm] Read error at pos. 99"},
	} {
		t.Run(name, func(t *testing.T) {
			ffprobe := fakePacketProbe(t, "cat <<'OUT'\n"+scanPackets+"OUT\nprintf '%s' '"+tc.stderr+"' >&2\n")
			got, err := ScanDemuxDamage(context.Background(), ffprobe, "Deleted Scenes.mkv", 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.Packets != 3 || got.Errors != tc.errors || got.First != tc.first || got.Unrecognised != tc.unrecognised || got.FirstUnrecognised != tc.firstOther {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

// The seconds read run from the earliest packet to the end of the latest, in
// whatever order the packets are stored.
func TestScanDemuxDamageMeasuresWhatItRead(t *testing.T) {
	ffprobe := fakePacketProbe(t, "cat <<'OUT'\n"+scanPackets+"OUT\n")
	got, err := ScanDemuxDamage(context.Background(), ffprobe, "a.mkv", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seconds != 10.5 {
		t.Fatalf("seconds = %v, want 10.5", got.Seconds)
	}
}

// A scan that could not read the file judged nothing: it is an error, never a
// clean result.
func TestScanDemuxDamageReportsAScanThatDidNotFinish(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"ffprobe fails":     {"echo 'Invalid data found when processing input' >&2\nexit 1\n", "Invalid data found"},
		"no format printed": {"echo 'packet|pts_time=0.0'\n", "no format name"},
		"nothing at all":    {"exit 0\n", "no format name"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ScanDemuxDamage(context.Background(), fakePacketProbe(t, tc.body), "a.mkv", 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := ScanDemuxDamage(context.Background(), "/nonexistent/ffprobe", "a.mkv", 0); err == nil {
		t.Fatal("a missing ffprobe judged the file")
	}
}

func TestScanDemuxDamageStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ScanDemuxDamage(ctx, fakePacketProbe(t, "exec sleep 30\n"), "a.mkv", 0); err == nil {
		t.Fatal("a cancelled scan returned a result")
	}
}
