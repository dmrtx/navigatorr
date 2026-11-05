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
		// ffprobe takes a read error for the end of the file and exits 0. It
		// names a position, but it is the I/O layer failing, not bytes found wrong.
		"a read that failed":           {"[matroska,webm @ 0x1] Read error at pos. 4021 (0xfb5)\n", 0, "", 1, "[matroska,webm] Read error at pos. 4021 (0xfb5)"},
		"a file that ends mid-element": {"[matroska,webm @ 0x1] File ended prematurely at pos. 4021 (0xfb5)\n", 1, "[matroska,webm] File ended prematurely at pos. 4021 (0xfb5)", 0, ""},
		"a line of no context":         {"Input/output error\n", 0, "", 1, "Input/output error"},
		"last line without a newline":  {"[matroska,webm @ 0x1] Seek failed", 0, "", 1, "[matroska,webm] Seek failed"},
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

// A failed read is counted apart from the damage a run printed: a run that had
// one stopped where the read did, so what came before it is not a judgement of
// the file.
func TestScanDemuxDamageCountsFailedReadsApart(t *testing.T) {
	for name, tc := range map[string]struct {
		stderr string
		errors int
		faults int
	}{
		"none":                        {"", 0, 0},
		"damage alone":                {"[matroska,webm @ 0x1] Packet corrupt (stream = 0, dts = 42).\n", 1, 0},
		"a failed read alone":         {"[matroska,webm @ 0x1] Read error at pos. 4021 (0xfb5)\n", 0, 1},
		"damage, then a failed read":  {"[matroska,webm @ 0x1] Packet corrupt (stream = 0, dts = 42).\n[matroska,webm @ 0x1] Read error at pos. 4021 (0xfb5)\n", 1, 1},
		"a failed read of no context": {"Read error\n", 0, 1},
		"another context's read":      {"[h264 @ 0x2] Read error at pos. 9\n", 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			ffprobe := fakePacketProbe(t, "cat <<'OUT'\n"+scanPackets+"OUT\nprintf '%s' '"+tc.stderr+"' >&2\n")
			got, err := ScanDemuxDamage(context.Background(), ffprobe, "a.mkv", 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.Errors != tc.errors || got.ReadFaults != tc.faults {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

// A stream with no presentation times (AVI, raw streams) is placed by its
// decode times, and one with neither measures nothing; a clock that restarts
// is reported, a few frames of reordering is not.
func TestScanDemuxDamagePlacesPacketsByTheTimesTheyCarry(t *testing.T) {
	for name, tc := range map[string]struct {
		packets       string
		seconds       float64
		discontinuous bool
	}{
		"decode times only":         {"packet|dts_time=0.000000|duration_time=0.040000\npacket|dts_time=59.960000|duration_time=0.040000\n", 60, false},
		"presentation not set":      {"packet|pts_time=N/A|dts_time=1.000000|duration_time=0.500000\npacket|pts_time=N/A|dts_time=3.000000|duration_time=0.500000\n", 2.5, false},
		"no times at all":           {"packet|size=100\npacket|size=100\n", 0, false},
		"reordered by a few frames": {"packet|pts_time=0.083000|duration_time=0.041000\npacket|pts_time=0.000000|duration_time=0.041000\npacket|pts_time=0.166000|duration_time=0.041000\n", 0.207, false},
		"a clock that restarts":     {"packet|pts_time=0.000000|duration_time=1.000000\npacket|pts_time=100.000000|duration_time=1.000000\npacket|pts_time=5.000000|duration_time=1.000000\n", 101, true},
	} {
		t.Run(name, func(t *testing.T) {
			ffprobe := fakePacketProbe(t, "cat <<'OUT'\n"+tc.packets+"format|format_name=avi\nOUT\n")
			got, err := ScanDemuxDamage(context.Background(), ffprobe, "a.avi", 0)
			if err != nil {
				t.Fatal(err)
			}
			if d := got.Seconds - tc.seconds; d > 0.001 || d < -0.001 || got.Discontinuous != tc.discontinuous {
				t.Fatalf("got %+v, want %v seconds, discontinuous %v", got, tc.seconds, tc.discontinuous)
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
