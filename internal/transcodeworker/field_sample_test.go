package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProbe is an ffprobe that prints listing whatever it is asked, and
// counts its runs in the returned file.
func fakeProbe(t *testing.T, listing string) (path, runs string) {
	t.Helper()
	dir := t.TempDir()
	out, runs := filepath.Join(dir, "listing"), filepath.Join(dir, "runs")
	if err := os.WriteFile(out, []byte(listing), 0o600); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "ffprobe")
	script := fmt.Sprintf("#!/bin/sh\necho x >> %q\ncat %q\n", runs, out)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, runs
}

// sampleListing is what ffprobe prints decoding a sample: the keyframe at
// keyAt, then packets at the given times and pictures at the given times.
func sampleListing(keyAt float64, packets, pictures []float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "packet|pts_time=%.6f|flags=K__\nframe|best_effort_timestamp_time=%.6f\n", keyAt, keyAt)
	for _, p := range packets {
		fmt.Fprintf(&b, "packet|pts_time=%.6f|flags=___\n", p)
	}
	for _, p := range pictures {
		fmt.Fprintf(&b, "frame|best_effort_timestamp_time=%.6f\n", p)
	}
	return b.String()
}

func times(n int, first, step float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = first + float64(i)*step
	}
	return out
}

// A source of a field codec whose head is frame-coded and whose later
// packets lie half a frame apart may carry fields there, or pictures at twice
// the head's rate: only decoding them tells. Fields keep the range the
// fields allow; pictures at the higher rate are counted exactly, so a
// conversion that loses some of them is refused.
func TestFieldSampleDecidesClosePackets(t *testing.T) {
	// 400 frames at 25 FPS, then 100 packets 0.02 apart, each stated a
	// whole frame long; the head decodes to a picture per packet.
	src := func() sourceVideoScan {
		late := run(100, 16, 0.02, func(int) string { return "" })
		for i := range late {
			late[i].dur = 0.04
		}
		return decoded(videoScanOf(cat(run(400, 0, 0.04, nil), late)...), 301)
	}
	late := times(100, 16, 0.02)
	// Every packet kept, with the late ones at twice the rate.
	whole := videoScanOf(cat(run(400, 0, 0.04, nil), run(100, 16, 0.02, func(int) string { return "" }))...)
	for _, tc := range []struct {
		name    string
		listing string
		want    fieldSampleResult
		cand    sourceVideoScan
		fail    string
	}{
		{"fields: every picture", sampleListing(0, late, times(50, 16, 0.04)), fieldsSampledFields, frames(450, 0, 0.04), ""},
		{"fields: one picture missing", sampleListing(0, late, times(50, 16, 0.04)), fieldsSampledFields, frames(449, 0, 0.04), "449 frames, the source shows at least 450"},
		{"a higher frame rate: every picture", sampleListing(0, late, late), fieldsSampledFrames, whole, ""},
		{"a higher frame rate: half its pictures lost", sampleListing(0, late, late), fieldsSampledFrames, frames(450, 0, 0.04), "450 frames, the source shows 500"},
		// A read that did not start at the keyframe says nothing: the
		// range the fields allow stays.
		{"the decode never reached the keyframe", strings.TrimPrefix(sampleListing(0, late, late), "packet|pts_time=0.000000|flags=K__\n"), fieldsNotSampled, frames(450, 0, 0.04), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe, _ := fakeProbe(t, tc.listing)
			s := src()
			if err := sampleFieldPictures(context.Background(), probe, "source.ts", 0, &s, "h264", 25); err != nil {
				t.Fatal(err)
			}
			if s.fieldSample != tc.want {
				t.Fatalf("sample found %d, want %d", s.fieldSample, tc.want)
			}
			checkVideo(t, s, tc.cand, "h264", 25, tc.fail)
		})
	}
}

// Nothing is decoded when no packet lies close to another, when the head
// already shows fields, or for a codec that never codes fields apart.
func TestFieldSampleSkipped(t *testing.T) {
	late := run(100, 16, 0.02, func(int) string { return "" })
	for _, tc := range []struct {
		name  string
		src   sourceVideoScan
		codec string
	}{
		{"frames only", decoded(frames(500, 0, 0.04), 301), "h264"},
		{"the head shows fields", decoded(videoScanOf(run(200, 0, 0.02, nil)...), 100), "h264"},
		{"a frame codec", decoded(videoScanOf(cat(run(400, 0, 0.04, nil), late)...), 301), "mpeg4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe, runs := fakeProbe(t, "")
			s := tc.src
			if err := sampleFieldPictures(context.Background(), probe, "source.ts", 0, &s, tc.codec, 25); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(runs); err == nil || s.fieldSample != fieldsNotSampled {
				t.Fatalf("decoded a sample (result %d)", s.fieldSample)
			}
		})
	}
}

// A video whose packet times cannot all be kept (here one 80 hours past
// the others, beyond what the scan holds) keeps no times at all, and any of
// its packets may then be a field: never an exact count for a field codec.
func TestVideoTimesOverflowStaysLoose(t *testing.T) {
	s := videoScanOf(cat(run(10, 0, 0.04, nil), []vpkt{{pts: 80 * 3600, dur: 0.04}})...)
	if !s.timesOverflow || s.times != nil {
		t.Fatalf("times kept past the bound: overflow %v, %d times", s.timesOverflow, len(s.times))
	}
	s = decoded(s, 11)
	if p := sourcePictures(s, "h264", 25); !p.fields || p.least != 6 || p.most != 11 {
		t.Fatalf("field codec counted %+v, want fields between 6 and 11", p)
	}
	if p := sourcePictures(s, "mpeg4", 25); p.fields || p.least != 11 || p.most != 11 {
		t.Fatalf("frame codec counted %+v, want exactly 11", p)
	}
}

// A window's source extract seeks before the keyframe its pictures are
// decoded from when the container has no keyframe index, and to the
// window's start when it has one; with no keyframe times to aim at, the seek
// stays at the start and is said not to reach a keyframe.
func TestWindowSeek(t *testing.T) {
	ts := func() *sourceVideoScan {
		s := &sourceVideoScan{format: "mpegts"}
		for i, at := range times(300, 1.5, 0.1) { // a keyframe every 50 packets: 1.5, 6.5, 11.5, ...
			s.addTime(at, 0.1, i%50 == 0)
		}
		return s
	}
	cases := []struct {
		name  string
		scan  func() *sourceVideoScan
		at    float64
		seek  float64
		keyed bool
	}{
		{"indexed container", func() *sourceVideoScan { s := ts(); s.format = "matroska,webm"; return s }, 9.95, 9.95, true},
		{"transport stream", ts, 9.95, 5.5, true},
		{"just after a keyframe", ts, 11.55, 10.5, true},
		{"before the first keyframe", ts, 1.45, 1.45, false},
		{"times overflowed", func() *sourceVideoScan { s := ts(); s.times, s.timesOverflow = nil, true; return s }, 9.95, 9.95, false},
		{"clock restarted", func() *sourceVideoScan { s := ts(); s.Discontinuous = true; return s }, 9.95, 9.95, false},
	}
	for _, tc := range cases {
		seek, keyed := tc.scan().windowSeek(tc.at)
		if keyed != tc.keyed || seek < tc.seek-1e-6 || seek > tc.seek+1e-6 {
			t.Errorf("%s: seek %.4f keyed %v, want %.4f %v", tc.name, seek, keyed, tc.seek, tc.keyed)
		}
	}
	args := buildExtractionArgs("src.ts", "out.mkv", 0, 1.5, 8.45, 12.45, 5.5)
	if got := strings.Join(args, " "); !strings.Contains(got, "-ss 5.500000 -i src.ts") || !strings.Contains(got, "trim=start=9.950000:end=13.950000") {
		t.Fatalf("extract args %s", got)
	}
}
