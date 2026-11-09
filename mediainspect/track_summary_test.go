package mediainspect

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// uhdDolbyVisionProbe is what ffprobe prints for a 2160p Dolby Vision 8.1
// Matroska file with a cover, three audio tracks and three subtitle tracks,
// two of which carry mkvmerge's statistics tags.
const uhdDolbyVisionProbe = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "hevc", "profile": "Main 10", "pix_fmt": "yuv420p10le",
     "width": 3840, "height": 1600, "sample_aspect_ratio": "1:1", "avg_frame_rate": "24000/1001", "r_frame_rate": "24000/1001",
     "color_range": "tv", "color_space": "bt2020nc", "color_primaries": "bt2020", "color_transfer": "smpte2084",
     "side_data_list": [
       {"side_data_type": "DOVI configuration record", "dv_version_major": 1, "dv_profile": 8, "dv_level": 6,
        "rpu_present_flag": 1, "el_present_flag": 0, "bl_present_flag": 1, "dv_bl_signal_compatibility_id": 1},
       {"side_data_type": "Mastering display metadata", "red_x": "34000/50000", "red_y": "16000/50000",
        "green_x": "13250/50000", "green_y": "34500/50000", "blue_x": "7500/50000", "blue_y": "3000/50000",
        "white_point_x": "15635/50000", "white_point_y": "16450/50000", "min_luminance": "50/10000", "max_luminance": "10000000/10000"}
     ],
     "tags": {"BPS": "21000000", "language": "und"}, "disposition": {"default": 1}},
    {"index": 1, "codec_type": "audio", "codec_name": "truehd", "profile": "Dolby TrueHD + Dolby Atmos", "channels": 8, "channel_layout": "7.1",
     "tags": {"language": "eng", "title": "TrueHD Atmos 7.1", "BPS-eng": "4100000"}, "disposition": {"default": 1, "original": 1}},
    {"index": 2, "codec_type": "audio", "codec_name": "eac3", "channels": 6, "channel_layout": "5.1(side)", "bit_rate": "640000",
     "tags": {"language": "spa", "title": "Castellano"}, "disposition": {"default": 0}},
    {"index": 3, "codec_type": "audio", "codec_name": "ac3", "channels": 2, "channel_layout": "stereo", "bit_rate": "192000",
     "tags": {"language": "eng", "title": "Director's Commentary"}, "disposition": {"comment": 0}},
    {"index": 4, "codec_type": "subtitle", "codec_name": "subrip",
     "tags": {"language": "spa", "title": "Forzados", "NUMBER_OF_FRAMES-spa": "41"}, "disposition": {"forced": 0}},
    {"index": 5, "codec_type": "subtitle", "codec_name": "hdmv_pgs_subtitle",
     "tags": {"language": "eng", "title": "English SDH", "NUMBER_OF_FRAMES": "2846"}, "disposition": {"hearing_impaired": 0}},
    {"index": 6, "codec_type": "subtitle", "codec_name": "ass",
     "tags": {"language": "jpn"}, "disposition": {"default": 1, "forced": 1}},
    {"index": 7, "codec_type": "video", "codec_name": "mjpeg", "width": 600, "height": 900,
     "tags": {"filename": "cover.jpg"}, "disposition": {"attached_pic": 1}}
  ],
  "format": {"format_name": "matroska,webm", "duration": "7932.480000", "size": "40000000000", "bit_rate": "40340000"}
}`

func writeProbeScript(t *testing.T, dir, name, script string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInspectFileReportsTracksColourAndSize(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "Film.2160p.DV.mkv")
	if err := os.WriteFile(media, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	ffprobe := createMockFFprobe(t, dir, uhdDolbyVisionProbe)

	rep, err := InspectFile(context.Background(), ffprobe, media)
	if err != nil || !rep.Probed {
		t.Fatalf("InspectFile = %+v, %v", rep, err)
	}

	// The historical summary fields stay as they were: resolution is a class
	// by height alone, so a 3840x1600 scope film still reads 1080p there and
	// width/height carry the real size.
	if rep.Container != "matroska" || rep.VideoCodec != "hevc" || rep.Resolution != "1080p" || rep.BitDepth != 10 {
		t.Errorf("summary = %s %s %s %d", rep.Container, rep.VideoCodec, rep.Resolution, rep.BitDepth)
	}
	if !reflect.DeepEqual(rep.AudioLanguages, []string{"eng", "spa"}) || !reflect.DeepEqual(rep.SubtitleLanguages, []string{"spa", "eng", "jpn"}) {
		t.Errorf("languages = %v / %v", rep.AudioLanguages, rep.SubtitleLanguages)
	}

	// Real size of the programme video, not the cover, and overall bitrate.
	if rep.Width != 3840 || rep.Height != 1600 || rep.BitRate != 40340000 || rep.DurationSec != 7932.48 {
		t.Errorf("size/bitrate/duration = %dx%d %d %v", rep.Width, rep.Height, rep.BitRate, rep.DurationSec)
	}
	v := rep.Video
	if v == nil || v.Index != 0 || v.ColorTransfer != "smpte2084" || v.ColorPrimaries != "bt2020" || v.ColorSpace != "bt2020nc" || v.BitDepth != 10 || v.BitRate != 21000000 {
		t.Fatalf("video = %+v", v)
	}
	if !rep.IsHDR || !v.IsHDR || !reflect.DeepEqual(v.HDRFormats, []string{"dolby_vision", "hdr10"}) || v.DolbyVisionProfile != 8 {
		t.Errorf("HDR = %v %v %v profile %d", rep.IsHDR, v.IsHDR, v.HDRFormats, v.DolbyVisionProfile)
	}

	wantAudio := []AudioTrack{
		{Index: 1, Language: "eng", Codec: "truehd", Profile: "Dolby TrueHD + Dolby Atmos", Channels: 8, Layout: "7.1", ChannelLayout: "7.1", BitRate: 4100000, Title: "TrueHD Atmos 7.1", Default: true, Original: true},
		{Index: 2, Language: "spa", Codec: "eac3", Channels: 6, Layout: "5.1", ChannelLayout: "5.1(side)", BitRate: 640000, Title: "Castellano"},
		{Index: 3, Language: "eng", Codec: "ac3", Channels: 2, Layout: "2.0", ChannelLayout: "stereo", BitRate: 192000, Title: "Director's Commentary", Commentary: true},
	}
	if !reflect.DeepEqual(rep.AudioTracks, wantAudio) {
		t.Errorf("audio tracks:\n got %+v\nwant %+v", rep.AudioTracks, wantAudio)
	}

	n41, n2846 := int64(41), int64(2846)
	wantSubs := []SubtitleTrack{
		{Index: 4, Language: "spa", Codec: "subrip", Format: "text", Title: "Forzados", Forced: true, Events: &n41, EventsSource: EventsFromContainer},
		{Index: 5, Language: "eng", Codec: "hdmv_pgs_subtitle", Format: "bitmap", Title: "English SDH", SDH: true, Events: &n2846, EventsSource: EventsFromContainer},
		{Index: 6, Language: "jpn", Codec: "ass", Format: "text", Default: true, Forced: true},
	}
	if !reflect.DeepEqual(rep.SubtitleTracks, wantSubs) {
		got, _ := json.Marshal(rep.SubtitleTracks)
		t.Errorf("subtitle tracks: %s", got)
	}
}

func TestInspectFileJSONKeepsSummaryFieldsAndAddsDetail(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "Film.mkv")
	_ = os.WriteFile(media, []byte("payload"), 0o644)
	rep, err := InspectFile(context.Background(), createMockFFprobe(t, dir, uhdDolbyVisionProbe), media)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"path", "container", "video_codec", "resolution", "bit_depth", "audio_languages", "subtitle_languages", "duration_sec", "size_bytes", "probed",
		"width", "height", "bit_rate", "is_hdr", "video", "audio_tracks", "subtitle_tracks"} {
		if _, ok := m[key]; !ok {
			t.Errorf("JSON lacks %q: %s", key, raw)
		}
	}
}

func TestInspectFileSDRAndHLG(t *testing.T) {
	for _, tc := range []struct {
		name, transfer, primaries string
		hdr                       bool
		formats                   []string
	}{
		{"sdr", "bt709", "bt709", false, nil},
		{"unspecified", "", "", false, nil},
		{"hlg", "arib-std-b67", "bt2020", true, []string{"hlg"}},
		{"pq without mastering", "smpte2084", "bt2020", true, []string{"pq10"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			media := filepath.Join(dir, "x.mkv")
			_ = os.WriteFile(media, []byte("x"), 0o644)
			probe := `{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","pix_fmt":"yuv420p10le","width":1920,"height":1080,
				"color_transfer":"` + tc.transfer + `","color_primaries":"` + tc.primaries + `"}],"format":{"format_name":"matroska"}}`
			rep, err := InspectFile(context.Background(), createMockFFprobe(t, dir, probe), media)
			if err != nil {
				t.Fatal(err)
			}
			if rep.IsHDR != tc.hdr || !reflect.DeepEqual(rep.Video.HDRFormats, tc.formats) {
				t.Errorf("is_hdr=%v formats=%v, want %v %v", rep.IsHDR, rep.Video.HDRFormats, tc.hdr, tc.formats)
			}
		})
	}
}

func TestInspectFileMP4SubtitleFrameCount(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "x.mp4")
	_ = os.WriteFile(media, []byte("x"), 0o644)
	probe := `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":1280,"height":720},
		{"index":1,"codec_type":"subtitle","codec_name":"mov_text","nb_frames":"612","tags":{"language":"spa"}}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2"}}`
	rep, err := InspectFile(context.Background(), createMockFFprobe(t, dir, probe), media)
	if err != nil {
		t.Fatal(err)
	}
	s := rep.SubtitleTracks
	if len(s) != 1 || s[0].Events == nil || *s[0].Events != 612 || s[0].EventsSource != EventsFromContainer || s[0].Format != "text" {
		t.Fatalf("subtitle = %+v", s)
	}
}

func TestInspectFileWithoutFFprobeLeavesDetailEmpty(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "x.mkv")
	_ = os.WriteFile(media, []byte("x"), 0o644)
	failing := writeProbeScript(t, dir, "ffprobe", "exit 1\n")
	rep, err := InspectFile(context.Background(), failing, media)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Probed || rep.Video != nil || rep.AudioTracks != nil || rep.SubtitleTracks != nil || rep.IsHDR || rep.Width != 0 {
		t.Errorf("unprobed report carries detail: %+v", rep)
	}
}

func TestCountSubtitlePackets(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "x.mkv")
	_ = os.WriteFile(media, []byte("x"), 0o644)
	args := filepath.Join(dir, "args")
	ffprobe := writeProbeScript(t, dir, "ffprobe", `echo "$@" > `+args+`
cat <<'EOF'
{"streams":[{"index":4,"nb_read_packets":"41"},{"index":6,"nb_read_packets":"0"}]}
EOF
`)
	counts, err := CountSubtitlePackets(context.Background(), ffprobe, media)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(counts, map[int]int64{4: 41, 6: 0}) {
		t.Errorf("counts = %v", counts)
	}
	got, _ := os.ReadFile(args)
	for _, want := range []string{"-select_streams s", "-count_packets", "stream=index,nb_read_packets"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("argv %q lacks %q", got, want)
		}
	}
	// It reads packets only: it never decodes frames.
	if strings.Contains(string(got), "-count_frames") || strings.Contains(string(got), "-show_frames") {
		t.Errorf("argv decodes frames: %q", got)
	}

	rep := Report{SubtitleTracks: []SubtitleTrack{{Index: 4}, {Index: 5}, {Index: 6}}}
	rep.ApplySubtitleCounts(counts)
	if *rep.SubtitleTracks[0].Events != 41 || rep.SubtitleTracks[0].EventsSource != EventsCounted ||
		rep.SubtitleTracks[1].Events != nil ||
		rep.SubtitleTracks[2].Events == nil || *rep.SubtitleTracks[2].Events != 0 {
		t.Errorf("applied = %+v", rep.SubtitleTracks)
	}

	bad := writeProbeScript(t, dir, "ffprobe-bad", "echo '{\"streams\":[{\"index\":4,\"nb_read_packets\":\"N/A\"}]}'\n")
	if _, err := CountSubtitlePackets(context.Background(), bad, media); err == nil {
		t.Error("a count of N/A was accepted")
	}
}

func TestAudioLayoutName(t *testing.T) {
	for _, tc := range []struct {
		layout   string
		channels int
		want     string
	}{
		{"mono", 1, "1.0"}, {"stereo", 2, "2.0"}, {"5.1(side)", 6, "5.1"}, {"5.1", 6, "5.1"}, {"7.1(wide)", 8, "7.1"},
		{"quad", 4, "4.0"}, {"2.1", 3, "2.1"}, {"", 6, "5.1"}, {"", 2, "2.0"}, {"", 8, "7.1"}, {"unknown", 5, "5ch"}, {"", 0, ""},
	} {
		if got := AudioLayoutName(tc.layout, tc.channels); got != tc.want {
			t.Errorf("AudioLayoutName(%q, %d) = %q, want %q", tc.layout, tc.channels, got, tc.want)
		}
	}
}
