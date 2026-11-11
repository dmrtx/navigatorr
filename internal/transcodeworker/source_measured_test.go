package transcodeworker

import (
	"errors"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// A kind the source's scan reads must carry a measurement: a candidate is
// never held to nothing.
func TestSourceMeasured(t *testing.T) {
	all := packetScanRequest{video: 0, audio: map[int]bool{1: true}, subtitles: map[int]bool{2: true}}
	full := SourceProbe{videoScan: &sourceVideoScan{}, audioSpans: map[int]audioPacketSpan{}, subtitlePackets: map[int]subtitlePackets{}}
	for _, tc := range []struct {
		name string
		req  packetScanRequest
		src  SourceProbe
		fail string
	}{
		{"every kind measured", all, full, ""},
		{"nothing asked", packetScanRequest{video: -1}, SourceProbe{}, ""},
		{"video not measured", all, SourceProbe{audioSpans: full.audioSpans, subtitlePackets: full.subtitlePackets}, "the source's video packets were not measured"},
		{"audio and subtitles not measured", all, SourceProbe{videoScan: full.videoScan}, "the source's audio and subtitle packets were not measured"},
		{"an empty measure is a measure", packetScanRequest{video: -1, audio: map[int]bool{}}, SourceProbe{audioSpans: map[int]audioPacketSpan{}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sourceMeasured(tc.req, tc.src)
			if tc.fail == "" && err != nil || tc.fail != "" && (err == nil || !strings.Contains(err.Error(), tc.fail)) {
				t.Fatalf("error %v, want %q", err, tc.fail)
			}
		})
	}
}

// A converted audio track is refused before the encode when the checks after
// it could not measure it: no timed packet, or a video with no start time.
// A copied track needs neither.
func TestConvertedAudioMeasurable(t *testing.T) {
	start := 0.0
	video := SourceStream{Index: 0, Kind: "video", Codec: "h264", StartTime: &start}
	pcm := SourceStream{Index: 1, Kind: "audio", TypeIndex: 0, Codec: "pcm_s24le", Channels: 6}
	eac3 := SourceStream{Index: 1, Kind: "audio", TypeIndex: 0, Codec: "eac3", Channels: 6}
	timed := map[int]audioPacketSpan{1: {packets: 100, timed: 100}}
	untimed := map[int]audioPacketSpan{1: {packets: 100, untimed: 100}}
	plan := &transcode.Plan{AudioMode: "compact"}
	for _, tc := range []struct {
		name  string
		audio SourceStream
		video SourceStream
		spans map[int]audioPacketSpan
		fail  string
	}{
		{"a converted track with timed packets", pcm, video, timed, ""},
		{"a converted track with none", pcm, video, untimed, "audio stream 0 has 100 packets and none with a time"},
		{"a video with no start time", pcm, SourceStream{Index: 0, Kind: "video", Codec: "h264"}, timed, "the source's video has no start time"},
		{"a copied track with none", eac3, SourceStream{Index: 0, Kind: "video", Codec: "h264"}, untimed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if codec, _ := transcode.AudioTarget(plan.AudioMode, tc.audio.AudioTrack()); (codec == "copy") != (tc.audio.Codec == "eac3") {
				t.Fatalf("fixture: %s is planned as %s", tc.audio.Codec, codec)
			}
			err := convertedAudioMeasurable(plan, SourceProbe{Streams: []SourceStream{tc.video, tc.audio}, audioSpans: tc.spans})
			if tc.fail == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			// Before the encode, the source is what cannot be checked: a
			// damaged input, not a converted file that lost content.
			if err == nil || !strings.Contains(err.Error(), tc.fail) || !errors.Is(err, ErrSourceInvalid) || errors.Is(err, ErrContentNotPreserved) || resilience.ClassifyError(err) != resilience.FFmpegInputCorrupt {
				t.Fatalf("error %v (%s), want %q", err, resilience.ClassifyError(err), tc.fail)
			}
		})
	}
}
