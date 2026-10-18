package transcodeworker

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// dolbyVision81Config is a dvcC payload: version 1.0, profile 8, level 6,
// RPU and base layer present, no enhancement layer, base-layer signal
// compatibility 1 (HDR10).
func dolbyVision81Config() []byte {
	b := make([]byte, 24)
	b[0], b[1] = 1, 0
	binary.BigEndian.PutUint16(b[2:], 8<<9|6<<3|1<<2|0<<1|1)
	b[4] = 1 << 4
	return b
}

// withDolbyVisionConfig turns an HEVC MP4 (moov after mdat, as FFmpeg writes
// it without faststart) into a Dolby Vision profile 8.1 file: the hvc1
// sample entry becomes dvh1 and gains a dvcC configuration record. Only box
// sizes on the path to the sample entry change, so mdat offsets stay valid.
func withDolbyVisionConfig(t *testing.T, mp4 []byte) []byte {
	t.Helper()
	dvcc := append([]byte{0, 0, 0, 32, 'd', 'v', 'c', 'C'}, dolbyVision81Config()...)
	var path []int // offsets of the boxes enclosing the sample entry
	var walk func(start, end int) bool
	walk = func(start, end int) bool {
		for off := start; off+8 <= end; {
			size := int(binary.BigEndian.Uint32(mp4[off:]))
			typ := string(mp4[off+4 : off+8])
			if size < 8 || off+size > end {
				t.Fatalf("malformed box %q at %d", typ, off)
			}
			switch typ {
			case "moov", "trak", "mdia", "minf", "stbl":
				path = append(path, off)
				if walk(off+8, off+size) {
					return true
				}
				path = path[:len(path)-1]
			case "stsd":
				// Full box: version/flags and entry count precede the entries.
				entry := off + 16
				if et := string(mp4[entry+4 : entry+8]); et == "hvc1" || et == "hev1" {
					path = append(path, off, entry)
					return true
				}
			}
			off += size
		}
		return false
	}
	if !walk(0, len(mp4)) {
		t.Fatal("no HEVC sample entry found")
	}
	entry := path[len(path)-1]
	moov := path[0]
	mdat := bytes.Index(mp4, []byte("mdat"))
	if mdat < 0 || mdat > moov {
		t.Fatal("moov must follow mdat")
	}
	entryEnd := entry + int(binary.BigEndian.Uint32(mp4[entry:]))
	out := append(append(append([]byte(nil), mp4[:entryEnd]...), dvcc...), mp4[entryEnd:]...)
	for _, off := range path {
		binary.BigEndian.PutUint32(out[off:], binary.BigEndian.Uint32(out[off:])+uint32(len(dvcc)))
	}
	copy(out[entry+4:], "dvh1")
	return out
}

// TestRealCompactEncodeDolbyVision81 encodes a Dolby Vision profile 8.1
// stream (a genuine configuration record over an HDR10 base layer) with the
// production compact arguments. libx265's -dolbyvision defaults to auto,
// which keys off that stream-level record; compact turns it off, and the
// output must be clean SDR with no Dolby Vision signalling.
func TestRealCompactEncodeDolbyVision81(t *testing.T) {
	ctx, ffmpeg, ffprobe := compactRealEnv(t)
	if ok, err := libX265HasDolbyVisionOption(ctx, ffmpeg); err != nil || !ok {
		t.Skipf("libx265 without -dolbyvision: %v", err)
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.mp4")
	runFFmpeg(t, ctx, ffmpeg, "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=24:duration=1",
		"-vf", "format=yuv420p10le,setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv",
		"-c:v", "libx265", "-preset", "ultrafast", "-crf", "18", "-pix_fmt", "yuv420p10le", "-tag:v", "hvc1",
		"-x265-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:range=limited:hdr10=1:repeat-headers=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50):max-cll=1000,400:log-level=error",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv",
		"-metadata:s:v:0", "title=HEVC Dolby Vision", base)
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source.mp4")
	if err := os.WriteFile(source, withDolbyVisionConfig(t, raw), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := mediainspect.InspectDetailed(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	if v := rep.MainVideo(); len(v) != 1 || v[0].CodecTag != "dvh1" || !mediainspect.HasDolbyVision(v[0]) {
		t.Fatalf("synthesized source lacks Dolby Vision signalling: %+v", rep.Video)
	}
	d, err := classifySourceDynamicRange(ctx, ffprobe, source, &rep)
	if err != nil {
		t.Fatal(err)
	}
	if d.Class != mediainspect.DynamicRangeDolbyVision || d.Reason != "" || d.BaseLayer != mediainspect.DynamicRangeHDR10 || d.DolbyVision == nil || d.DolbyVision.Profile != 8 || d.DolbyVision.BLCompatibilityID != 1 {
		t.Fatalf("classified %+v %+v", d, d.DolbyVision)
	}
	c, err := transcode.NewCompactPlan(d, rep.MainVideo()[0], transcode.CompactDefaultMaxHeight)
	if err != nil || c.DolbyVisionProfile != 8 || c.SourceSignal != mediainspect.SignalPQ || c.ToneMap == nil {
		t.Fatalf("plan %+v %v", c, err)
	}
	plan := compactModePlanFor(t, c, rep.MainVideo()[0].BitDepth)
	probe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureCompactLUT(c); err != nil {
		t.Fatal(err)
	}
	encode := func(name string, drop func([]string) []string) (string, mediainspect.DetailedReport) {
		out := filepath.Join(dir, name)
		args, err := BuildFFmpegArgs(ep, source, out, filepath.Join(dir, name+".progress"))
		if err != nil {
			t.Fatal(err)
		}
		if drop != nil {
			args = drop(args)
		}
		runFFmpeg(t, ctx, ffmpeg, append([]string{"-nostdin", "-v", "error"}, args...)...)
		r, err := mediainspect.InspectDetailed(ctx, ffprobe, out)
		if err != nil {
			t.Fatal(err)
		}
		return out, r
	}

	candidate, out := encode("candidate.mkv", nil)
	if e := validateCompactOutput(ctx, ffprobe, candidate, &out, c); e.Verdict != "pass" {
		t.Fatalf("Dolby Vision source did not become clean SDR: %+v", e)
	}
	v := out.MainVideo()[0]
	if mediainspect.HasDolbyVision(v) || v.Tags["title"] != "" || v.BitDepth != 10 {
		t.Fatalf("output video %+v", v)
	}
	if od, err := classifySourceDynamicRange(ctx, ffprobe, candidate, &out); err != nil || od.Class != mediainspect.DynamicRangeSDR {
		t.Fatalf("output classified %+v %v", od, err)
	}

	// The same encode with the encoder default shows what the switch guards
	// against. Whether this FFmpeg re-emits the record is build dependent;
	// when it does, the output check must catch it.
	bypass, bout := encode("auto.mkv", func(args []string) []string {
		kept := make([]string, 0, len(args))
		for i := 0; i < len(args); i++ {
			if args[i] == "-dolbyvision:v:0" {
				i++
				continue
			}
			kept = append(kept, args[i])
		}
		return kept
	})
	if bv := bout.MainVideo()[0]; mediainspect.HasDolbyVision(bv) {
		if e := validateCompactOutput(ctx, ffprobe, bypass, &bout, c); e.Verdict != "fail" || !strings.Contains(strings.Join(e.HDRSideData, ","), "dolby_vision") {
			t.Fatalf("re-emitted Dolby Vision passed the output check: %+v", e)
		}
		t.Log("with -dolbyvision auto this FFmpeg re-emits the configuration record; compact disables it")
	} else {
		t.Log("with -dolbyvision auto this FFmpeg did not re-emit the configuration record")
	}
}
