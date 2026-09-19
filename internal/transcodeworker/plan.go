package transcodeworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

type SourceStream struct {
	Index       int            `json:"index"`
	TypeIndex   int            `json:"type_index"`
	Kind        string         `json:"kind"`
	Codec       string         `json:"codec"`
	Language    string         `json:"language,omitempty"`
	Title       string         `json:"title,omitempty"`
	Channels    int            `json:"channels,omitempty"`
	Disposition map[string]int `json:"disposition,omitempty"`
	// Detailed video fields (zero/empty when the probe could not determine
	// them). They drive the worker-local pre-publish policy validation so an
	// invalid candidate is rejected before any NAS publish.
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	BitDepth    int    `json:"bit_depth,omitempty"`
	PixelFormat string `json:"pixel_format,omitempty"`
	Profile     string `json:"profile,omitempty"`
}

// SourceProbe is the full local structural/media probe result used for
// pre-publish validation. Chapters is the container chapter count.
type SourceProbe struct {
	Streams     []SourceStream
	DurationSec float64
	Chapters    int
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
	if err := cmd.Run(); err != nil {
		return SourceProbe{}, fmt.Errorf("ffprobe probe failed on %s: %w (stderr: %s)", filePath, err, stderr.String())
	}
	var probe struct {
		Streams []struct {
			Index            int               `json:"index"`
			CodecType        string            `json:"codec_type"`
			CodecName        string            `json:"codec_name"`
			Profile          string            `json:"profile"`
			Width            int               `json:"width"`
			Height           int               `json:"height"`
			PixFmt           string            `json:"pix_fmt"`
			BitsPerRawSample any               `json:"bits_per_raw_sample"`
			BitsPerSample    any               `json:"bits_per_sample"`
			Channels         int               `json:"channels"`
			Tags             map[string]string `json:"tags"`
			Disposition      map[string]int    `json:"disposition"`
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
		lang, title := "", ""
		for k, v := range st.Tags {
			if strings.EqualFold(k, "language") {
				lang = norm(v)
			}
			if strings.EqualFold(k, "title") {
				title = strings.TrimSpace(v)
			}
		}
		bitDepth := parseBitDepth(st.BitsPerRawSample, st.BitsPerSample, st.PixFmt)
		out = append(out, SourceStream{
			Index: st.Index, TypeIndex: typeIdx, Kind: kind, Codec: norm(st.CodecName),
			Language: lang, Title: title, Channels: st.Channels, Disposition: st.Disposition,
			Width: st.Width, Height: st.Height, BitDepth: bitDepth, PixelFormat: norm(st.PixFmt), Profile: strings.TrimSpace(st.Profile),
		})
	}
	return SourceProbe{Streams: out, DurationSec: duration, Chapters: len(probe.Chapters)}, nil
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
	Kind        string `json:"kind"`
	SourceIndex int    `json:"source_index"`
	TypeIndex   int    `json:"type_index"`
	Codec       string `json:"codec"`
	TargetCodec string `json:"target_codec"`
}
type ExecutionPlan struct {
	Plan        *transcode.Plan              `json:"plan"`
	Streams     []StreamAction               `json:"streams"`
	Conversions []transcode.ConversionRecord `json:"conversions"`
	DurationSec float64                      `json:"duration_sec"`
}

func BuildExecutionPlan(plan *transcode.Plan, streams []SourceStream, duration float64) (*ExecutionPlan, error) {
	if err := ValidatePlan(plan); err != nil {
		return nil, err
	}
	ep := &ExecutionPlan{Plan: plan, DurationSec: duration}
	subtitleActions := map[int]transcode.SubtitleAction{}
	for _, a := range plan.SubtitleActions {
		subtitleActions[a.TypeIndex] = a
	}
	seenSubs := map[int]bool{}
	for _, s := range streams {
		switch s.Kind {
		case "video":
			ep.Streams = append(ep.Streams, StreamAction{Kind: "video", SourceIndex: s.Index, TypeIndex: s.TypeIndex, Codec: s.Codec, TargetCodec: plan.VideoCodec})
		case "audio":
			ep.Streams = append(ep.Streams, StreamAction{Kind: "audio", SourceIndex: s.Index, TypeIndex: s.TypeIndex, Codec: s.Codec, TargetCodec: "copy"})
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
				return nil, fmt.Errorf("subtitle #%d source index changed: plan=%d actual=%d (fail closed)", s.TypeIndex, a.SourceStreamIndex, s.Index)
			}
			if norm(a.SourceCodec) != norm(s.Codec) {
				return nil, fmt.Errorf("subtitle #%d source codec changed: plan=%s actual=%s (fail closed)", s.TypeIndex, a.SourceCodec, s.Codec)
			}
			target := norm(a.Codec)
			if norm(a.Operation) == "copy" {
				target = "copy"
			}
			ep.Streams = append(ep.Streams, StreamAction{Kind: "subtitle", SourceIndex: s.Index, TypeIndex: s.TypeIndex, Codec: s.Codec, TargetCodec: target})
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
			return nil, fmt.Errorf("resolved plan references missing subtitle #%d (source changed; fail closed)", idx)
		}
	}
	return ep, nil
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
