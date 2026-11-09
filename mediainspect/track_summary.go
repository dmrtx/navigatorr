package mediainspect

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// VideoTrack is the programme video of an inspected file: its real size,
// colour signalling and whether it is high dynamic range.
type VideoTrack struct {
	Index          int     `json:"index"`
	Codec          string  `json:"codec"`
	Profile        string  `json:"profile,omitempty"`
	Width          int     `json:"width,omitempty"`
	Height         int     `json:"height,omitempty"`
	PixelFormat    string  `json:"pixel_format,omitempty"`
	BitDepth       int     `json:"bit_depth,omitempty"`
	FPS            float64 `json:"fps,omitempty"`
	BitRate        int64   `json:"bit_rate,omitempty"`
	ColorRange     string  `json:"color_range,omitempty"`
	ColorSpace     string  `json:"color_space,omitempty"`
	ColorPrimaries string  `json:"color_primaries,omitempty"`
	ColorTransfer  string  `json:"color_transfer,omitempty"`
	// IsHDR is true for a PQ or HLG transfer or Dolby Vision signalling.
	IsHDR bool `json:"is_hdr"`
	// HDRFormats lists what the stream signals: dolby_vision, hdr10 (PQ,
	// BT.2020 and mastering metadata), pq10 (PQ without it) and hlg.
	HDRFormats         []string `json:"hdr_formats,omitempty"`
	DolbyVisionProfile int      `json:"dolby_vision_profile,omitempty"`
}

// AudioTrack is one audio stream of an inspected file.
type AudioTrack struct {
	Index    int    `json:"index"`
	Language string `json:"language,omitempty"`
	Codec    string `json:"codec"`
	Profile  string `json:"profile,omitempty"`
	Channels int    `json:"channels,omitempty"`
	// Layout is the channel layout as people say it: 1.0, 2.0, 5.1, 7.1.
	Layout        string `json:"layout,omitempty"`
	ChannelLayout string `json:"channel_layout,omitempty"`
	BitRate       int64  `json:"bit_rate,omitempty"`
	Title         string `json:"title,omitempty"`
	Default       bool   `json:"default"`
	Commentary    bool   `json:"commentary"`
	Original      bool   `json:"original,omitempty"`
}

// SubtitleTrack is one subtitle stream of an inspected file.
type SubtitleTrack struct {
	Index    int    `json:"index"`
	Language string `json:"language,omitempty"`
	Codec    string `json:"codec"`
	// Format is text (SRT, ASS, mov_text, WebVTT) or bitmap (PGS, VobSub, DVB).
	Format  string `json:"format,omitempty"`
	Title   string `json:"title,omitempty"`
	Default bool   `json:"default"`
	Forced  bool   `json:"forced"`
	SDH     bool   `json:"sdh"`
	// Events is the number of subtitle packets: from the container's own
	// statistics (EventsSource container) or from reading the whole file
	// (EventsSource counted). A bitmap track usually holds two packets per
	// line shown (one to show it, one to clear it). Absent when unknown.
	Events       *int64 `json:"events,omitempty"`
	EventsSource string `json:"events_source,omitempty"`
}

// Where a subtitle event count came from.
const (
	EventsFromContainer = "container"
	EventsCounted       = "counted"
)

// commentaryTitle matches a track title naming a commentary, in the languages
// of the library (English, Spanish, Portuguese, Italian, German).
var commentaryTitle = regexp.MustCompile(`(?i)comment|comentari|kommentar`)

// IsCommentaryTitle reports a track title that names a commentary.
func IsCommentaryTitle(title string) bool { return commentaryTitle.MatchString(title) }

var (
	forcedTitle = regexp.MustCompile(`(?i)\bforced\b|forzad|forc[eé]s?\b`)
	sdhTitle    = regexp.MustCompile(`(?i)\bsdh\b|\bcc\b|hearing[ -]?impaired|sordos`)
)

// fillTrackSummary adds the per-track details of det to the summary report,
// keeping the historical summary fields (codec, height class, languages).
func fillTrackSummary(rep *Report, det *DetailedReport) {
	video := det.MainVideo()
	if len(video) == 0 && len(det.Video) > 0 {
		video = det.Video[:1]
	}
	if len(video) > 0 {
		v := video[0]
		rep.VideoCodec = v.Codec
		rep.Resolution = resolutionName(v.Height)
		rep.BitDepth = v.BitDepth
		rep.Width, rep.Height = v.Width, v.Height
		rep.Video = videoTrack(v)
		rep.IsHDR = rep.Video.IsHDR
	}
	for _, a := range det.Audio {
		rep.AudioLanguages = appendNorm(rep.AudioLanguages, a.Language)
		rep.AudioTracks = append(rep.AudioTracks, AudioTrack{
			Index:         a.Index,
			Language:      a.Language,
			Codec:         a.Codec,
			Profile:       a.Profile,
			Channels:      a.Channels,
			Layout:        AudioLayoutName(a.ChannelLayout, a.Channels),
			ChannelLayout: a.ChannelLayout,
			BitRate:       a.BitRate,
			Title:         a.Title,
			Default:       a.Disposition["default"] > 0,
			Commentary:    a.Disposition["comment"] > 0 || IsCommentaryTitle(a.Title),
			Original:      a.Disposition["original"] > 0,
		})
	}
	for _, s := range det.Subtitles {
		rep.SubtitleLanguages = appendNorm(rep.SubtitleLanguages, s.Language)
		t := SubtitleTrack{
			Index:    s.Index,
			Language: s.Language,
			Codec:    s.Codec,
			Format:   subtitleFormat(s.Codec),
			Title:    s.Title,
			Default:  s.Disposition["default"] > 0,
			Forced:   s.Disposition["forced"] > 0 || forcedTitle.MatchString(s.Title),
			SDH:      s.Disposition["hearing_impaired"] > 0 || sdhTitle.MatchString(s.Title),
		}
		if n := containerEventCount(s, det.nbFrames[s.Index]); n > 0 {
			t.Events, t.EventsSource = &n, EventsFromContainer
		}
		rep.SubtitleTracks = append(rep.SubtitleTracks, t)
	}
}

// videoTrack summarizes one programme video stream from its stream-level
// signalling. HDR10+ is carried per frame, so it is not visible here.
func videoTrack(v DetailedStream) *VideoTrack {
	t := &VideoTrack{
		Index: v.Index, Codec: v.Codec, Profile: v.Profile,
		Width: v.Width, Height: v.Height, PixelFormat: v.PixelFormat, BitDepth: v.BitDepth,
		FPS: v.FPS, BitRate: v.BitRate,
		ColorRange: v.ColorRange, ColorSpace: v.ColorSpace, ColorPrimaries: v.ColorPrimaries, ColorTransfer: v.ColorTransfer,
	}
	if dv := dolbyVisionInfo(v, nil); dv != nil {
		t.HDRFormats = append(t.HDRFormats, DynamicRangeDolbyVision)
		if dv.Profile > 0 {
			t.DolbyVisionProfile = dv.Profile
		}
	}
	switch normalizeColorValue(v.ColorTransfer) {
	case "smpte2084":
		if normalizeColorValue(v.ColorPrimaries) == "bt2020" && v.MasteringDisplay != nil {
			t.HDRFormats = append(t.HDRFormats, DynamicRangeHDR10)
		} else {
			t.HDRFormats = append(t.HDRFormats, DynamicRangePQ10)
		}
	case "arib-std-b67":
		t.HDRFormats = append(t.HDRFormats, DynamicRangeHLG)
	}
	t.IsHDR = len(t.HDRFormats) > 0
	return t
}

// AudioLayoutName turns ffprobe's channel layout ("5.1(side)", "stereo")
// into the usual name ("5.1", "2.0"), falling back to the channel count.
func AudioLayoutName(layout string, channels int) string {
	l := strings.ToLower(strings.TrimSpace(layout))
	if i := strings.IndexByte(l, '('); i > 0 {
		l = l[:i]
	}
	switch l {
	case "mono":
		return "1.0"
	case "stereo", "downmix":
		return "2.0"
	case "quad":
		return "4.0"
	case "hexagonal":
		return "6.0"
	case "octagonal":
		return "8.0"
	}
	if len(l) == 3 && l[1] == '.' && l[0] >= '0' && l[0] <= '9' && l[2] >= '0' && l[2] <= '9' {
		return l
	}
	switch channels {
	case 0:
		return ""
	case 1:
		return "1.0"
	case 2:
		return "2.0"
	case 6:
		return "5.1"
	case 8:
		return "7.1"
	}
	return fmt.Sprintf("%dch", channels)
}

// subtitleFormat says whether a subtitle codec carries text or pictures.
func subtitleFormat(codec string) string {
	switch strings.ToLower(codec) {
	case "subrip", "srt", "ass", "ssa", "mov_text", "webvtt", "text", "microdvd", "subviewer", "jacosub", "sami", "realtext", "stl", "mpl2", "pjs", "vplayer", "ttml", "eia_608":
		return "text"
	case "hdmv_pgs_subtitle", "dvd_subtitle", "dvb_subtitle", "xsub", "dvb_teletext":
		return "bitmap"
	}
	return ""
}

// containerEventCount reads a subtitle stream's packet count from what the
// container already states: Matroska's statistics tags (NUMBER_OF_FRAMES,
// written by mkvmerge, possibly with a language suffix) or the stream's own
// frame count (MP4 sample tables).
func containerEventCount(s DetailedStream, nbFrames int64) int64 {
	var suffixed int64
	for k, v := range s.Tags {
		key := strings.ToUpper(k)
		if key != "NUMBER_OF_FRAMES" && !strings.HasPrefix(key, "NUMBER_OF_FRAMES-") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		if key == "NUMBER_OF_FRAMES" {
			return n
		}
		suffixed = n
	}
	if suffixed > 0 {
		return suffixed
	}
	return nbFrames
}

// subtitleCountTimeout bounds CountSubtitlePackets, which demuxes the whole
// file: about five minutes for a 30 GB file read over the network at 100 MB/s.
var subtitleCountTimeout = 10 * time.Minute

// CountSubtitlePackets counts every subtitle stream's packets by reading the
// whole file (no decoding), for files whose container states no count. It
// returns the counts by stream index.
func CountSubtitlePackets(ctx context.Context, ffprobePath, path string) (map[int]int64, error) {
	if ffprobePath == "" {
		ffprobePath, _ = exec.LookPath("ffprobe")
	}
	res, err := runFFprobe(ctx, ffprobePath, subtitleCountTimeout,
		"-v", "error", "-select_streams", "s", "-count_packets",
		"-show_entries", "stream=index,nb_read_packets", "-print_format", "json", path)
	if err != nil {
		return nil, err
	}
	var probe struct {
		Streams []struct {
			Index         int    `json:"index"`
			NbReadPackets string `json:"nb_read_packets"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(res.stdout, &probe); err != nil {
		return nil, res.badOutput(err)
	}
	counts := make(map[int]int64, len(probe.Streams))
	for _, st := range probe.Streams {
		n, err := strconv.ParseInt(strings.TrimSpace(st.NbReadPackets), 10, 64)
		if err != nil || n < 0 {
			return nil, res.badOutput(fmt.Errorf("stream %d: packet count %q", st.Index, st.NbReadPackets))
		}
		counts[st.Index] = n
	}
	return counts, nil
}

// ApplySubtitleCounts replaces the subtitle event counts with counted ones.
// A counted zero is kept: the track really is empty.
func (r *Report) ApplySubtitleCounts(counts map[int]int64) {
	for i := range r.SubtitleTracks {
		if n, ok := counts[r.SubtitleTracks[i].Index]; ok {
			r.SubtitleTracks[i].Events, r.SubtitleTracks[i].EventsSource = &n, EventsCounted
		}
	}
}
