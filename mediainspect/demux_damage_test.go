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

// Only the container's own demuxer counts, and only errors that name damaged
// bytes: a decoder's complaint, or a header element the demuxer does not
// understand, is not a damaged file.
func TestScanDemuxDamageCountsTheContainersDamageReports(t *testing.T) {
	packets := "packet|pts_time=0.000000\npacket|pts_time=0.041000\npacket|pts_time=0.083000\nformat|format_name=matroska,webm\n"
	for name, tc := range map[string]struct {
		stderr string
		errors int
		first  string
	}{
		"healthy":                    {"", 0, ""},
		"damaged":                    {"[matroska,webm @ 0x1] EBML header parsing failed\n[matroska,webm @ 0x1] Packet corrupt (stream = 0, dts = 42).\n", 2, "[matroska,webm] EBML header parsing failed"},
		"another context":            {"[h264 @ 0x2] corrupt decoded frame\n", 0, ""},
		"metadata the demuxer skips": {"[matroska,webm @ 0x1] Unknown-sized element in the chapters\n", 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ffprobe := fakePacketProbe(t, "cat <<'OUT'\n"+packets+"OUT\ncat >&2 <<'ERR'\n"+tc.stderr+"ERR\n")
			got, err := ScanDemuxDamage(context.Background(), ffprobe, "Deleted Scenes.mkv", 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.Packets != 3 || got.Errors != tc.errors || got.First != tc.first {
				t.Fatalf("got %+v", got)
			}
		})
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
