package transcodeworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

type SourceStream struct {
	Index       int            `json:"index"`
	TypeIndex   int            `json:"type_index"`
	Kind        string         `json:"kind"`
	Codec       string         `json:"codec"`
	Language    string         `json:"language,omitempty"`
	Title       string         `json:"title,omitempty"`
	Handler     string         `json:"handler,omitempty"`
	Channels    int            `json:"channels,omitempty"`
	Layout      string         `json:"channel_layout,omitempty"`
	Disposition map[string]int `json:"disposition,omitempty"`
	StartTime   *float64       `json:"start_time,omitempty"`
	// BitrateKbps is an audio track's source bitrate (zero when the probe
	// states none), which the size goal's audio levels read.
	BitrateKbps int `json:"bitrate_kbps,omitempty"`
	// Detailed video fields (zero/empty when the probe could not determine
	// them). They drive the worker-local pre-publish policy validation so an
	// invalid candidate is rejected before any NAS publish.
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	BitDepth    int    `json:"bit_depth,omitempty"`
	PixelFormat string `json:"pixel_format,omitempty"`
	Profile     string `json:"profile,omitempty"`
	// Filename and MimeType come from the stream tags. Matroska cover art
	// (an image attachment ffprobe exposes as an attached_pic video stream)
	// carries both; they are reused when the cover is re-attached.
	Filename string `json:"filename,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// AudioTrack is the stream as the audio policy reads it.
func (s SourceStream) AudioTrack() transcode.AudioTrack {
	return transcode.AudioTrack{Codec: s.Codec, Profile: s.Profile, Title: s.Title, Handler: s.Handler, Channels: s.Channels, Layout: s.Layout, BitrateKbps: s.BitrateKbps}
}

// IsAttachedPicture reports whether s is embedded cover art (a video stream
// with disposition attached_pic=1) rather than programme video.
func (s SourceStream) IsAttachedPicture() bool {
	return strings.EqualFold(strings.TrimSpace(s.Kind), "video") && s.Disposition["attached_pic"] > 0
}

// SourceProbe is the full local structural/media probe result used for
// pre-publish validation. Chapters is the container chapter count.
type SourceProbe struct {
	Path        string
	Streams     []SourceStream
	DurationSec float64
	Chapters    int
	// audioSpans is the source's measured audio packet timing, taken before
	// the encode when the plan re-encodes audio.
	audioSpans map[int]audioPacketSpan
}

func ProbeSourceStreams(ctx context.Context, ffprobePath, filePath string) ([]SourceStream, float64, error) {
	p, err := ProbeSourceDetails(ctx, ffprobePath, filePath)
	if err != nil {
		return nil, 0, err
	}
	return p.Streams, p.DurationSec, nil
}

// ProbeSourceDetails probes structure, duration, and chapter count with a
// single ffprobe invocation. It is used worker-local for pre-publish
// validation; callers pass either the staged local input or the local
// candidate, never a NAS path after publish.
func ProbeSourceDetails(ctx context.Context, ffprobePath, filePath string) (SourceProbe, error) {
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", "-show_chapters", filePath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := runCharged(ctx, cmd); err != nil {
		return SourceProbe{}, fmt.Errorf("ffprobe probe failed on %s: %w (stderr: %s)", filePath, err, stderr.String())
	}
	var probe struct {
		Streams []struct {
			Index            int               `json:"index"`
			BitRate          any               `json:"bit_rate"`
			CodecType        string            `json:"codec_type"`
			CodecName        string            `json:"codec_name"`
			Profile          string            `json:"profile"`
			Width            int               `json:"width"`
			Height           int               `json:"height"`
			PixFmt           string            `json:"pix_fmt"`
			BitsPerRawSample any               `json:"bits_per_raw_sample"`
			BitsPerSample    any               `json:"bits_per_sample"`
			Channels         int               `json:"channels"`
			ChannelLayout    string            `json:"channel_layout"`
			Tags             map[string]string `json:"tags"`
			Disposition      map[string]int    `json:"disposition"`
			StartTime        string            `json:"start_time"`
		} `json:"streams"`
		Chapters []struct {
			Index int `json:"index"`
		} `json:"chapters"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
		return SourceProbe{}, fmt.Errorf("parsing ffprobe json for %s: %w", filePath, err)
	}
	var duration float64
	if d := strings.TrimSpace(probe.Format.Duration); d != "" && d != "N/A" {
		duration, _ = strconv.ParseFloat(d, 64)
	}
	counts := map[string]int{}
	out := make([]SourceStream, 0, len(probe.Streams))
	for _, st := range probe.Streams {
		kind := norm(st.CodecType)
		typeIdx := counts[kind]
		counts[kind]++
		lang, title, filename, mimeType := "", "", "", ""
		for k, v := range st.Tags {
			if strings.EqualFold(k, "language") {
				lang = norm(v)
			}
			if strings.EqualFold(k, "title") {
				title = strings.TrimSpace(v)
			}
			if strings.EqualFold(k, "filename") {
				filename = strings.TrimSpace(v)
			}
			if strings.EqualFold(k, "mimetype") {
				mimeType = norm(v)
			}
		}
		bitDepth := parseBitDepth(st.BitsPerRawSample, st.BitsPerSample, st.PixFmt)
		var startTime *float64
		if v, err := strconv.ParseFloat(st.StartTime, 64); err == nil && isFiniteQuality(v) {
			startTime = &v
		}
		bitrateKbps := 0
		if kind == "audio" {
			bitrateKbps = transcode.SourceAudioKbps(mediainspect.ParseBitRate(st.BitRate, st.Tags), st.Tags, duration)
		}
		out = append(out, SourceStream{
			Index: st.Index, TypeIndex: typeIdx, Kind: kind, Codec: norm(st.CodecName),
			Language: lang, Title: title, Handler: transcode.AudioHandler(st.Tags), Channels: st.Channels, Layout: st.ChannelLayout, Disposition: st.Disposition,
			StartTime: startTime,
			Width:     st.Width, Height: st.Height, BitDepth: bitDepth, PixelFormat: norm(st.PixFmt), Profile: strings.TrimSpace(st.Profile),
			Filename: filename, MimeType: mimeType, BitrateKbps: bitrateKbps,
		})
	}
	return SourceProbe{Path: filePath, Streams: out, DurationSec: duration, Chapters: len(probe.Chapters)}, nil
}

// parseBitDepth resolves the video bit depth from ffprobe fields, falling back
// to the pixel format when the raw sample fields are absent.
func parseBitDepth(rawSample, sample any, pixFmt string) int {
	for _, raw := range []any{rawSample, sample} {
		if v := strings.TrimSpace(anyToString(raw)); v != "" && v != "N/A" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				return int(f)
			}
		}
	}
	pf := strings.ToLower(strings.TrimSpace(pixFmt))
	switch {
	case strings.Contains(pf, "10le"), strings.Contains(pf, "p010"), strings.Contains(pf, "10be"):
		return 10
	case strings.Contains(pf, "12le"), strings.Contains(pf, "p012"), strings.Contains(pf, "12be"):
		return 12
	case pf != "":
		return 8
	}
	return 0
}

// anyToString renders a JSON scalar (string or number) as text.
func anyToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprintf("%v", t)
	}
}

type StreamAction struct {
	Disposition map[string]int `json:"disposition,omitempty"`
	Kind        string         `json:"kind"`
	SourceIndex int            `json:"source_index"`
	TypeIndex   int            `json:"type_index"`
	// OutputIndex is the stream's position among the output's streams of its
	// kind (audio or subtitle). It equals TypeIndex unless the plan drops
	// tracks (Plan.Tracks); only then do the per-stream arguments use it, for
	// they address the output's streams.
	OutputIndex int    `json:"output_index"`
	Codec       string `json:"codec"`
	TargetCodec string `json:"target_codec"`
	BitrateKbps int    `json:"bitrate_kbps,omitempty"`
	// HEAAC is set on an audio track converted to HE-AAC rather than AAC-LC.
	HEAAC bool `json:"he_aac,omitempty"`
	// AudioToolbox is set on a converted audio track that aac_at encodes
	// (HE-AAC always; AAC-LC in the size goal's levels), else the native encoder.
	AudioToolbox bool `json:"aac_at,omitempty"`
	// Audio only: the source title, handler, channels and layout, used to
	// retitle a converted track whose title or handler names its old format.
	Title    string `json:"title,omitempty"`
	Handler  string `json:"handler,omitempty"`
	Channels int    `json:"channels,omitempty"`
	Layout   string `json:"channel_layout,omitempty"`
	// Cover art only: the attachment name and MIME type it is re-attached with.
	Filename string `json:"filename,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}
type ExecutionPlan struct {
	Plan        *transcode.Plan              `json:"plan"`
	Streams     []StreamAction               `json:"streams"`
	Conversions []transcode.ConversionRecord `json:"conversions"`
	DurationSec float64                      `json:"duration_sec"`
	// SourceAttachments counts the source's attachment streams (fonts etc.,
	// not cover art). `-map 0:t?` maps all of them ahead of any re-attached
	// cover, so cover metadata indexes start after them.
	SourceAttachments int `json:"source_attachments,omitempty"`
}

// CoverArt returns the plan's cover-art actions in source order.
func (ep *ExecutionPlan) CoverArt() []StreamAction {
	if ep == nil {
		return nil
	}
	var out []StreamAction
	for _, s := range ep.Streams {
		if s.Kind == "cover" {
			out = append(out, s)
		}
	}
	return out
}

// coverArtMimeTypes maps attached-picture codecs to the MIME type Matroska
// needs on the attachment. Other codecs fail closed.
var coverArtMimeTypes = map[string]string{"mjpeg": "image/jpeg", "png": "image/png", "bmp": "image/bmp", "gif": "image/gif", "webp": "image/webp", "tiff": "image/tiff"}

var coverArtExtensions = map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/bmp": ".bmp", "image/gif": ".gif", "image/webp": ".webp", "image/tiff": ".tiff"}

// coverArtAction keeps an attached picture as an untouched cover. The
// Matroska muxer writes a copied attached_pic stream as an ordinary one-frame
// video track, so the cover is re-attached as an image attachment instead,
// which ffprobe reads back as attached_pic=1.
func coverArtAction(s SourceStream) (StreamAction, error) {
	mimeType := coverArtMimeTypes[norm(s.Codec)]
	if mimeType == "" {
		return StreamAction{}, fmt.Errorf("cover art stream %d has unsupported codec %q (fail closed)", s.Index, s.Codec)
	}
	if s.MimeType != "" {
		if _, ok := coverArtExtensions[s.MimeType]; !ok {
			return StreamAction{}, fmt.Errorf("cover art stream %d has unsupported mimetype %q (fail closed)", s.Index, s.MimeType)
		}
		mimeType = s.MimeType
	}
	filename := s.Filename
	if !safeAttachmentFilename(filename) {
		filename = fmt.Sprintf("cover_%d%s", s.TypeIndex, coverArtExtensions[mimeType])
	}
	return StreamAction{Kind: "cover", SourceIndex: s.Index, TypeIndex: s.TypeIndex, Codec: s.Codec, TargetCodec: "copy", Filename: filename, MimeType: mimeType}, nil
}

func safeAttachmentFilename(name string) bool {
	if name == "" || len(name) > 255 || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func BuildExecutionPlan(plan *transcode.Plan, streams []SourceStream, duration float64) (*ExecutionPlan, error) {
	if err := ValidatePlan(plan); err != nil {
		return nil, err
	}
	if plan.X265 != nil {
		n := 0
		for _, s := range streams {
			if s.Kind == "video" && !s.IsAttachedPicture() {
				n++
			}
		}
		if n != 1 {
			return nil, fmt.Errorf("two-pass x265 requires exactly one video stream")
		}
	}
	ep := &ExecutionPlan{Plan: plan, DurationSec: duration}
	subtitleActions := map[int]transcode.SubtitleAction{}
	for _, a := range plan.SubtitleActions {
		subtitleActions[a.TypeIndex] = a
	}
	seenSubs := map[int]bool{}
	// With a track selection only the kept audio and subtitle streams get an
	// action. A kept track's disposition is what the output must carry (the
	// default moves to the first kept track when the source's was dropped), and
	// its position among the output's streams of its kind is counted here.
	kept := map[int]map[string]int{}
	seenKept := map[int]bool{}
	outAudio, outSubtitle := 0, 0
	if plan.Tracks != nil {
		keptAudio, keptSubs, _, _ := plan.Tracks.Partition(trackInfos(streams))
		for _, t := range append(keptAudio, keptSubs...) {
			kept[t.Index] = t.Disposition
		}
	}
	for _, s := range streams {
		if plan.Tracks != nil && (s.Kind == "audio" || s.Kind == "subtitle") && !plan.Tracks.Keeps(s.Kind, s.Index) {
			if s.Kind == "subtitle" {
				if _, planned := subtitleActions[s.TypeIndex]; planned {
					return nil, sourceChanged(fmt.Errorf("resolved plan has an action for dropped subtitle #%d (source changed; fail closed)", s.TypeIndex))
				}
			}
			continue
		}
		switch {
		case s.IsAttachedPicture():
			a, err := coverArtAction(s)
			if err != nil {
				return nil, err
			}
			ep.Streams = append(ep.Streams, a)
			continue
		case s.Kind == "attachment":
			ep.SourceAttachments++
		}
		switch s.Kind {
		case "video":
			ep.Streams = append(ep.Streams, StreamAction{Kind: "video", SourceIndex: s.Index, TypeIndex: s.TypeIndex, Codec: s.Codec, TargetCodec: plan.VideoCodec})
		case "audio":
			out := transcode.AudioOutputFor(plan.AudioMode, s.AudioTrack())
			target, bitrate := out.Codec, out.Kbps
			disposition := s.Disposition
			if plan.Tracks != nil {
				disposition, seenKept[s.Index] = kept[s.Index], true
			}
			ep.Streams = append(ep.Streams, StreamAction{Kind: "audio", SourceIndex: s.Index, TypeIndex: s.TypeIndex, OutputIndex: outAudio, Codec: s.Codec, TargetCodec: target, BitrateKbps: bitrate, HEAAC: out.HEAAC, AudioToolbox: out.AudioToolbox, Disposition: disposition, Title: s.Title, Handler: s.Handler, Channels: s.Channels, Layout: s.Layout})
			outAudio++
			if target != "copy" {
				reason := fmt.Sprintf("%s audio requested; AAC at %d kbps, preserving channels and language (lossy)", plan.AudioMode, bitrate)
				if out.HEAAC {
					reason = fmt.Sprintf("%s audio requested; HE-AAC at %d kbps, preserving channels and language (lossy)", plan.AudioMode, bitrate)
				}
				if transcode.ObjectAudio(s.AudioTrack()) {
					reason += "; Atmos/DTS:X object and height metadata dropped, channel bed kept"
				}
				ep.Conversions = append(ep.Conversions, transcode.ConversionRecord{StreamType: "audio", StreamIndex: s.TypeIndex, FromCodec: s.Codec, ToCodec: target, Reason: reason})
			}
		case "subtitle":
			a, ok := subtitleActions[s.TypeIndex]
			if !ok && plan.RecipeVersion == "legacy-worker-shim" {
				// Rolling-upgrade compatibility only. Normal Navigatorr jobs always send
				// explicit recipe-resolved actions; this shim can be removed once old
				// profile-only clients are no longer supported.
				a, ok = legacyHevcVTSubtitleAction(s)
			}
			if !ok {
				return nil, fmt.Errorf("resolved plan has no action for subtitle stream %d (subtitle #%d); fail closed", s.Index, s.TypeIndex)
			}
			if a.SourceStreamIndex != s.Index {
				return nil, sourceChanged(fmt.Errorf("subtitle #%d source index changed: plan=%d actual=%d (fail closed)", s.TypeIndex, a.SourceStreamIndex, s.Index))
			}
			if norm(a.SourceCodec) != norm(s.Codec) {
				return nil, sourceChanged(fmt.Errorf("subtitle #%d source codec changed: plan=%s actual=%s (fail closed)", s.TypeIndex, a.SourceCodec, s.Codec))
			}
			target := norm(a.Codec)
			if norm(a.Operation) == "copy" {
				target = "copy"
			}
			var disposition map[string]int
			if plan.Tracks != nil {
				seenKept[s.Index] = true
				// Subtitle dispositions are copied with the stream; only a
				// default that moved to this track is written.
				if kept[s.Index]["default"] > 0 && s.Disposition["default"] == 0 {
					disposition = kept[s.Index]
				}
			}
			ep.Streams = append(ep.Streams, StreamAction{Kind: "subtitle", SourceIndex: s.Index, TypeIndex: s.TypeIndex, OutputIndex: outSubtitle, Codec: s.Codec, TargetCodec: target, Disposition: disposition})
			outSubtitle++
			seenSubs[s.TypeIndex] = true
			if norm(a.Operation) == "transcode" {
				ep.Conversions = append(ep.Conversions, transcode.ConversionRecord{StreamType: "subtitle", StreamIndex: s.TypeIndex, FromCodec: s.Codec, ToCodec: target, Reason: a.Reason})
			}
		case "attachment":
			if plan.PreserveAttachments {
				ep.Streams = append(ep.Streams, StreamAction{Kind: "attachment", SourceIndex: s.Index, TypeIndex: s.TypeIndex, Codec: s.Codec, TargetCodec: "copy"})
			}
		}
	}
	for idx := range subtitleActions {
		if !seenSubs[idx] {
			return nil, sourceChanged(fmt.Errorf("resolved plan references missing subtitle #%d (source changed; fail closed)", idx))
		}
	}
	if plan.Tracks != nil {
		// Every stream the plan keeps must still be there, as the kind the plan
		// says: a source that changed since planning fails here, not after an
		// encode that mapped something else.
		for _, idx := range append(append([]int(nil), plan.Tracks.KeepAudio...), plan.Tracks.KeepSubtitles...) {
			if !seenKept[idx] {
				return nil, sourceChanged(fmt.Errorf("resolved plan keeps stream %d, which is not a stream of that kind in the source (source changed; fail closed)", idx))
			}
		}
	}
	return ep, nil
}

// trackInfos lists the audio and subtitle streams as the track selection reads
// them.
func trackInfos(streams []SourceStream) []transcode.TrackInfo {
	var out []transcode.TrackInfo
	for _, s := range streams {
		if s.Kind == "audio" || s.Kind == "subtitle" {
			out = append(out, transcode.TrackInfo{Index: s.Index, Kind: s.Kind, Language: s.Language, Title: s.Title, Codec: s.Codec, Disposition: s.Disposition})
		}
	}
	return out
}

// outputIndex is the stream's position among the output's streams of its kind.
// A plan that drops no track keeps every source stream, so it is the source's
// type index (the form earlier plans and their tests use).
func outputIndex(plan *transcode.Plan, s StreamAction) int {
	if plan.Tracks == nil {
		return s.TypeIndex
	}
	return s.OutputIndex
}

func legacyHevcVTSubtitleAction(s SourceStream) (transcode.SubtitleAction, bool) {
	copyCodecs := map[string]bool{"subrip": true, "srt": true, "ass": true, "ssa": true, "hdmv_pgs_subtitle": true, "pgs": true, "dvd_subtitle": true, "vobsub": true, "dvb_subtitle": true, "dvb_teletext": true, "webvtt": true, "text": true}
	codec := norm(s.Codec)
	if copyCodecs[codec] {
		return transcode.SubtitleAction{SourceStreamIndex: s.Index, TypeIndex: s.TypeIndex, SourceCodec: codec, Operation: "copy", Codec: "copy", Reason: "legacy_worker_shim"}, true
	}
	if codec == "mov_text" {
		return transcode.SubtitleAction{SourceStreamIndex: s.Index, TypeIndex: s.TypeIndex, SourceCodec: codec, Operation: "transcode", Codec: "subrip", Reason: "legacy_worker_shim_matroska_compatibility"}, true
	}
	return transcode.SubtitleAction{}, false
}
