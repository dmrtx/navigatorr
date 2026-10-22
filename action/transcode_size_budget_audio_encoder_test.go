package action

import (
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// The record names, per converted track, what encodes it: the level-2 plan of a
// source mixing layouts uses HE-AAC through aac_at only where aac_at encodes it
// correctly, AAC-LC through aac_at where that keeps the layout, and the native
// encoder otherwise. A copied track names none.
func TestSizeBudgetAudioRecordNamesTheEncoder(t *testing.T) {
	rep := levelReport()
	rep.Audio = []mediainspect.DetailedStream{
		{Index: 1, Codec: "eac3", Channels: 6, ChannelLayout: "5.1(side)", BitRate: 640_000},
		{Index: 2, Codec: "truehd", Channels: 8, ChannelLayout: "7.1", BitRate: 4_000_000},
		{Index: 3, Codec: "ac3", Channels: 1, ChannelLayout: "mono", BitRate: 192_000},
		{Index: 4, Codec: "ac3", Channels: 3, ChannelLayout: "3.0", BitRate: 640_000},
		{Index: 5, Codec: "eac3", Channels: 3, ChannelLayout: "2.1", BitRate: 640_000},
		{Index: 6, Codec: "aac", Channels: 2, ChannelLayout: "stereo", BitRate: 128_000},
	}
	type out struct{ output, encoder string }
	for mode, want := range map[string][]out{
		transcode.AudioModeSizeAAC:   {{"aac", "aac_at"}, {"aac", "aac"}, {"aac", "aac_at"}, {"aac", "aac_at"}, {"aac", "aac"}, {"copy", ""}},
		transcode.AudioModeSizeHEAAC: {{"he_aac", "aac_at"}, {"aac", "aac"}, {"aac", "aac_at"}, {"aac", "aac_at"}, {"aac", "aac"}, {"he_aac", "aac_at"}},
	} {
		r := sizeBudgetAudioRecord(rep, mode, 1, 1990, 0)
		for i, tr := range r.Tracks {
			if tr.Output != want[i].output || tr.Encoder != want[i].encoder {
				t.Errorf("%s track %d (%s): %+v, want %+v", mode, i, tr.Codec, tr, want[i])
			}
		}
	}
	s := sizeBudgetAudioSentence(sizeBudgetAudioRecord(rep, transcode.AudioModeSizeHEAAC, 2, 1990, 800))
	if !strings.Contains(s, "2 HE-AAC") || !strings.Contains(s, "4 AAC-LC") {
		t.Errorf("sentence %q", s)
	}
}
