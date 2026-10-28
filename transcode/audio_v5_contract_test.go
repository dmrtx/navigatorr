package transcode

import (
	"fmt"
	"slices"
	"testing"
)

// TestSizeAudioV5Contract is the audio contract of plan version
// transcode-modes-v5, written out in full. The size goal's audio levels are
// decided by code, not by a field of the frozen policy: the plan carries only
// "size_aac" or "size_he_aac" (or their bed pair, "size_aac_bed" and
// "size_he_aac_bed", which differ only for lossy object audio), and the
// coordinator's estimate, the worker's
// builder and both validations recompute each track from the source. So a job
// frozen under v5 stays correct only while this table stays true.
//
// ANY CHANGE TO THIS TABLE (a target, the 1.25 minimum gain, a layout, the
// encoder or the profile chosen for a layout) REQUIRES A NEW POLICY AND PLAN
// VERSION, not an edit here. The layouts were checked with a real encode (see
// TestSizeAudioLevelsRealEncodeLayouts in internal/transcodeworker).
//
// Each row is one layout. For each level it lists the action of a lossy track
// (E-AC-3) and of a lossless one (FLAC) at a source bitrate just below the
// 1.25 times boundary, at it and well above it, and of a track whose bitrate is
// unknown: a lossy one is copied, a lossless one is converted.
func TestSizeAudioV5Contract(t *testing.T) {
	type enc struct {
		kbps    int
		encoder string // aac_at or aac (FFmpeg's native encoder)
	}
	type row struct {
		layout   string // as ffprobe reports it, empty when it reports none
		channels int
		lc       enc // AAC-LC, level 1 and the fallback of level 2
		lcBelow  int // largest source kbps below 1.25 x lc
		lcAt     int // 1.25 x lc, the smallest source converted
		he       int // HE-AAC kbps through aac_at at level 2, 0 when the layout has none
		heBelow  int
		heAt     int
	}
	rows := []row{
		{"mono", 1, enc{128, "aac_at"}, 159, 160, 0, 0, 0},
		{"stereo", 2, enc{192, "aac_at"}, 239, 240, 64, 79, 80},
		{"3.0", 3, enc{384, "aac_at"}, 479, 480, 0, 0, 0},
		{"4.0", 4, enc{384, "aac_at"}, 479, 480, 0, 0, 0},
		{"quad", 4, enc{384, "aac_at"}, 479, 480, 160, 199, 200},
		{"quad(side)", 4, enc{384, "aac_at"}, 479, 480, 160, 199, 200},
		{"5.0", 5, enc{384, "aac_at"}, 479, 480, 0, 0, 0},
		{"5.0(side)", 5, enc{384, "aac_at"}, 479, 480, 0, 0, 0},
		{"5.1", 6, enc{384, "aac_at"}, 479, 480, 160, 199, 200},
		{"5.1(side)", 6, enc{384, "aac_at"}, 479, 480, 160, 199, 200},
		{"6.1(back)", 7, enc{512, "aac_at"}, 639, 640, 0, 0, 0},
		{"7.0", 7, enc{512, "aac_at"}, 639, 640, 0, 0, 0},
		{"octagonal", 8, enc{512, "aac_at"}, 639, 640, 0, 0, 0},
		// Layouts aac_at changes or cannot open: the native encoder, no HE-AAC.
		{"7.1", 8, enc{512, "aac"}, 639, 640, 0, 0, 0},
		{"7.1(wide)", 8, enc{512, "aac"}, 639, 640, 0, 0, 0},
		{"5.1.2(back)", 8, enc{512, "aac"}, 639, 640, 0, 0, 0},
		{"2.1", 3, enc{384, "aac"}, 479, 480, 0, 0, 0},
		{"3.0(back)", 3, enc{384, "aac"}, 479, 480, 0, 0, 0},
		{"3.1", 4, enc{384, "aac"}, 479, 480, 0, 0, 0},
		{"4.1", 5, enc{384, "aac"}, 479, 480, 0, 0, 0},
		{"hexagonal", 6, enc{384, "aac"}, 479, 480, 0, 0, 0},
		// An unspecified layout is the channel count's default one.
		{"", 1, enc{128, "aac_at"}, 159, 160, 0, 0, 0},
		{"", 2, enc{192, "aac_at"}, 239, 240, 64, 79, 80},
		{"", 3, enc{384, "aac"}, 479, 480, 0, 0, 0},
		{"", 4, enc{384, "aac_at"}, 479, 480, 0, 0, 0},
		{"", 5, enc{384, "aac_at"}, 479, 480, 0, 0, 0},
		{"", 6, enc{384, "aac_at"}, 479, 480, 160, 199, 200},
		{"", 7, enc{512, "aac_at"}, 639, 640, 0, 0, 0},
		{"", 8, enc{512, "aac"}, 639, 640, 0, 0, 0},
	}
	// got reads an output the way the contract names it.
	got := func(out AudioOutput) string {
		switch {
		case out.Codec == "copy":
			return "copy"
		case out.HEAAC && out.AudioToolbox:
			return sprintOutput("aac_at", "HE", out.Kbps)
		case out.HEAAC:
			return sprintOutput("aac", "HE", out.Kbps)
		case out.AudioToolbox:
			return sprintOutput("aac_at", "LC", out.Kbps)
		}
		return sprintOutput("aac", "LC", out.Kbps)
	}
	for _, r := range rows {
		name := r.layout
		if name == "" {
			name = "unspecified"
		}
		name += "/" + sprintOutput("", "", r.channels)
		lc := sprintOutput(r.lc.encoder, "LC", r.lc.kbps)
		he := ""
		if r.he > 0 {
			he = sprintOutput("aac_at", "HE", r.he)
		}
		for _, pair := range [][]string{ObjectCopyAudioLevelModes, AudioLevelModes} {
			l1, l2 := pair[0], pair[1]
			check := func(level, what string, track AudioTrack, want string) {
				t.Helper()
				if g := got(AudioOutputFor(level, track)); g != want {
					t.Errorf("%s %s %s at %d kbps: %s, want %s", level, name, what, track.BitrateKbps, g, want)
				}
			}
			for _, kind := range []struct {
				name, codec, profile string
				lossless             bool
			}{{"lossy", "eac3", "", false}, {"lossless", "flac", "", true}} {
				track := func(kbps int) AudioTrack {
					return AudioTrack{Codec: kind.codec, Profile: kind.profile, Channels: r.channels, Layout: r.layout, BitrateKbps: kbps}
				}
				// Level 1: copy below 1.25 x the target, the AAC-LC target at and above.
				check(l1, kind.name, track(r.lcBelow), "copy")
				check(l1, kind.name, track(r.lcAt), lc)
				check(l1, kind.name, track(r.lcAt*4), lc)
				// Level 2: the HE-AAC target where the layout has one, else level 1.
				if he != "" {
					check(l2, kind.name, track(r.heBelow), "copy")
					check(l2, kind.name, track(r.heAt), he)
					check(l2, kind.name, track(r.heAt*10), he)
				} else {
					check(l2, kind.name, track(r.lcBelow), "copy")
					check(l2, kind.name, track(r.lcAt), lc)
					check(l2, kind.name, track(r.lcAt*4), lc)
				}
				// A bitrate the probe did not state: lossy is copied, lossless converted
				// (to HE-AAC at level 2 where the layout has it).
				unknown := track(0)
				wantL1, wantL2 := "copy", "copy"
				if kind.lossless {
					wantL1, wantL2 = lc, lc
					if he != "" {
						wantL2 = he
					}
				}
				check(l1, kind.name, unknown, wantL1)
				check(l2, kind.name, unknown, wantL2)
			}
		}
	}

	// Lossy object audio: the first pair copies it, the bed pair converts its
	// channel bed like any other lossy track.
	joc := AudioTrack{Codec: "eac3", Profile: "Dolby Digital Plus + Dolby Atmos", Channels: 6, Layout: "5.1", BitrateKbps: 768}
	for _, level := range ObjectCopyAudioLevelModes {
		if g := got(AudioOutputFor(level, joc)); g != "copy" {
			t.Errorf("E-AC-3 JOC at %s: %s, want copy", level, g)
		}
	}
	if g := got(AudioOutputFor(AudioModeSizeAACBed, joc)); g != sprintOutput("aac_at", "LC", 384) {
		t.Errorf("E-AC-3 JOC at %s: %s", AudioModeSizeAACBed, g)
	}
	if g := got(AudioOutputFor(AudioModeSizeHEAACBed, joc)); g != sprintOutput("aac_at", "HE", 160) {
		t.Errorf("E-AC-3 JOC at %s: %s", AudioModeSizeHEAACBed, g)
	}
	titled := AudioTrack{Codec: "eac3", Title: "English Atmos", Channels: 6, Layout: "5.1(side)", BitrateKbps: 448}
	if g := got(AudioOutputFor(AudioModeSizeAACBed, titled)); g != "copy" {
		t.Errorf("E-AC-3 Atmos below 1.25x at %s: %s, want copy", AudioModeSizeAACBed, g)
	}

	// Tracks no level converts, whatever their bitrate.
	for _, tc := range []struct {
		name  string
		track AudioTrack
	}{
		{"layout AAC cannot keep", AudioTrack{Codec: "ac3", Channels: 7, Layout: "6.1", BitrateKbps: 1000}},
		{"layout of another channel count", AudioTrack{Codec: "ac3", Channels: 2, Layout: "5.1", BitrateKbps: 1000}},
		{"unknown layout", AudioTrack{Codec: "flac", Channels: 2, Layout: "unknown", BitrateKbps: 1000}},
		{"more than 8 channels", AudioTrack{Codec: "truehd", Channels: 16, BitrateKbps: 5000}},
		{"no channels", AudioTrack{Codec: "flac", BitrateKbps: 1000}},
	} {
		for _, level := range append(slices.Clone(ObjectCopyAudioLevelModes), AudioLevelModes...) {
			if out := AudioOutputFor(level, tc.track); out.Codec != "copy" {
				t.Errorf("%s at %s: %+v, want copy", tc.name, level, out)
			}
		}
	}
	// Lossless object audio is converted to its channel bed.
	if g := got(AudioOutputFor(AudioModeSizeAAC, AudioTrack{Codec: "truehd", Profile: "TrueHD + Dolby Atmos", Channels: 8, Layout: "7.1", BitrateKbps: 5000})); g != sprintOutput("aac", "LC", 512) {
		t.Errorf("TrueHD Atmos: %s", g)
	}
	// Lossless DTS needs its HD MA profile; plain DTS is lossy.
	if g := got(AudioOutputFor(AudioModeSizeAAC, AudioTrack{Codec: "dts", Profile: "DTS-HD MA", Channels: 6, Layout: "5.1(side)"})); g != sprintOutput("aac_at", "LC", 384) {
		t.Errorf("DTS-HD MA of unknown bitrate: %s", g)
	}
	if g := got(AudioOutputFor(AudioModeSizeAAC, AudioTrack{Codec: "dts", Profile: "DTS", Channels: 6, Layout: "5.1(side)"})); g != "copy" {
		t.Errorf("DTS core of unknown bitrate: %s", g)
	}
}

func sprintOutput(encoder, profile string, n int) string {
	return fmt.Sprintf("%s/%s/%d", encoder, profile, n)
}
