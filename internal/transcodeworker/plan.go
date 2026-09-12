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
}

func ProbeSourceStreams(ctx context.Context, ffprobePath, filePath string) ([]SourceStream, float64, error) {
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", filePath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, 0, fmt.Errorf("ffprobe probe failed on %s: %w (stderr: %s)", filePath, err, stderr.String())
	}
	var probe struct {
		Streams []struct {
			Index       int               `json:"index"`
			CodecType   string            `json:"codec_type"`
			CodecName   string            `json:"codec_name"`
			Channels    int               `json:"channels"`
			Tags        map[string]string `json:"tags"`
			Disposition map[string]int    `json:"disposition"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
		return nil, 0, fmt.Errorf("parsing ffprobe json for %s: %w", filePath, err)
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
		out = append(out, SourceStream{Index: st.Index, TypeIndex: typeIdx, Kind: kind, Codec: norm(st.CodecName), Language: lang, Title: title, Channels: st.Channels, Disposition: st.Disposition})
	}
	return out, duration, nil
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
