package transcodeworker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeHVCC(t *testing.T) {
	// A 22-byte header, three arrays (VPS, SPS, PPS) of one NAL each, all
	// marked complete as mkvmerge writes them.
	rec := append(make([]byte, 22), 3,
		0xa0, 0, 1, 0, 2, 0x40, 0x01,
		0xa1, 0, 1, 0, 3, 0x42, 0x01, 0x01,
		0xa2, 0, 1, 0, 2, 0x44, 0x01)
	rec[0] = 1
	got := normalizeHVCC(rec)
	if got[23] != 0x20 || got[30] != 0x21 || got[38] != 0x22 || rec[23] != 0xa0 {
		t.Fatalf("completeness bits %x %x %x (input %x)", got[23], got[30], got[38], rec[23])
	}
	clear := append([]byte(nil), got...)
	if !bytes.Equal(normalizeHVCC(clear), got) {
		t.Fatal("normalizing twice changed the record")
	}
	// The parameter sets themselves still count.
	sps := append([]byte(nil), rec...)
	sps[35] = 0x02
	if bytes.Equal(normalizeHVCC(sps), got) {
		t.Fatal("a changed SPS normalized away")
	}
	// A record that does not parse is compared as is.
	for name, bad := range map[string][]byte{"truncated": rec[:30], "trailing": append(append([]byte(nil), rec...), 0), "version": append([]byte{2}, rec[1:]...)} {
		if !bytes.Equal(normalizeHVCC(bad), bad) {
			t.Errorf("%s record rewritten", name)
		}
	}
}

func TestParseFFprobeHexDump(t *testing.T) {
	dump := "\n00000000: 0102 2000 0000 9000 0000 0000 78f0 00fc  .. .........x...\n00000010: " + fmt.Sprintf("%-40s", "fdfa fa") + " ...\n"
	got, err := parseFFprobeHexDump(dump)
	want := []byte{1, 2, 0x20, 0, 0, 0, 0x90, 0, 0, 0, 0, 0, 0x78, 0xf0, 0, 0xfc, 0xfd, 0xfa, 0xfa}
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%x %v", got, err)
	}
	if got, err := parseFFprobeHexDump(""); err != nil || len(got) != 0 {
		t.Fatalf("empty dump %x %v", got, err)
	}
	for _, bad := range []string{"00000010: 0102  ..", "0000000: 01", "00000000: zz02  .."} {
		if _, err := parseFFprobeHexDump(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestVideoCopyFactsIgnoreHVCCCompleteness remuxes an HEVC Matroska file
// whose decoder configuration marks its parameter-set arrays complete, as
// mkvmerge writes them: FFmpeg's muxer clears the bits, and the stream facts
// must still match. A different encode must not.
func TestVideoCopyFactsIgnoreHVCCCompleteness(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	encode := func(name, crf string) string {
		out := filepath.Join(dir, name)
		runFFmpeg(t, ctx, ffmpeg, "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=1",
			"-c:v", "libx265", "-preset", "ultrafast", "-crf", crf, "-pix_fmt", "yuv420p10le", "-x265-params", "log-level=error", out)
		return out
	}
	source := encode("ffmpeg.mkv", "20")
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	// Set the completeness bits in place, in the CodecPrivate element.
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "V:0", "-show_data", "-show_entries", "stream=extradata", "-of", "default=nw=1:nk=1", source)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := parseFFprobeHexDump(string(out))
	if err != nil || len(rec) < 23 || bytes.Count(raw, rec) != 1 {
		t.Fatalf("hvcC %x %v", rec, err)
	}
	marked := append([]byte(nil), rec...)
	for pos, i := 23, 0; i < int(rec[22]); i++ {
		if marked[pos]&0x80 != 0 {
			t.Fatal("FFmpeg wrote a complete array; the test premise changed")
		}
		marked[pos] |= 0x80
		n := int(rec[pos+1])<<8 | int(rec[pos+2])
		pos += 3
		for range n {
			pos += 2 + (int(rec[pos])<<8 | int(rec[pos+1]))
		}
	}
	mkvmerge := filepath.Join(dir, "mkvmerge.mkv")
	if err := os.WriteFile(mkvmerge, bytes.Replace(raw, rec, marked, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	remux := filepath.Join(dir, "remux.mkv")
	runFFmpeg(t, ctx, ffmpeg, "-nostdin", "-v", "error", "-y", "-i", mkvmerge, "-map", "0:v:0", "-c", "copy", remux)

	facts := func(path string) videoStreamFacts {
		f, err := probeVideoStreamFacts(ctx, ffprobe, path)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	src, cand := facts(mkvmerge), facts(remux)
	if src != cand || src.extradata == "" {
		t.Fatalf("remux changed the stream facts:\n%+v\n%+v", src, cand)
	}
	if other := facts(encode("other.mkv", "30")); other.extradata == src.extradata && other == src {
		t.Fatal("a different encode matched")
	}
}
