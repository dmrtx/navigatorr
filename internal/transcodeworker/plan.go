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

// SourceStream represents one media stream identified by ffprobe on the worker.
type SourceStream struct {
	Index       int            `json:"index"`
	TypeIndex   int            `json:"type_index"`
	Kind        string         `json:"kind"` // video, audio, subtitle, attachment
	Codec       string         `json:"codec"`
	Language    string         `json:"language,omitempty"`
	Title       string         `json:"title,omitempty"`
	Channels    int            `json:"channels,omitempty"`
	Disposition map[string]int `json:"disposition,omitempty"`
}

// ProbeSourceStreams probes the source media file using ffprobe and extracts stream metadata and duration.
func ProbeSourceStreams(ctx context.Context, ffprobePath, filePath string) ([]SourceStream, float64, error) {
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		filePath,
	)
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
	if durStr := strings.TrimSpace(probe.Format.Duration); durStr != "" && durStr != "N/A" {
		if d, err := strconv.ParseFloat(durStr, 64); err == nil {
			duration = d
		}
	}

	typeCounts := make(map[string]int)
	var streams []SourceStream

	for _, st := range probe.Streams {
		kind := strings.ToLower(strings.TrimSpace(st.CodecType))
		codec := strings.ToLower(strings.TrimSpace(st.CodecName))

		typeIdx := typeCounts[kind]
		typeCounts[kind]++

		lang := ""
		title := ""
		for k, v := range st.Tags {
			if strings.EqualFold(k, "language") {
				lang = strings.ToLower(strings.TrimSpace(v))
			}
			if strings.EqualFold(k, "title") {
				title = strings.TrimSpace(v)
			}
		}

		streams = append(streams, SourceStream{
			Index:       st.Index,
			TypeIndex:   typeIdx,
			Kind:        kind,
			Codec:       codec,
			Language:    lang,
			Title:       title,
			Channels:    st.Channels,
			Disposition: st.Disposition,
		})
	}

	return streams, duration, nil
}

// StreamAction represents the transcoding or stream copying decision for a single stream.
type StreamAction struct {
	Kind        string `json:"kind"`
	SourceIndex int    `json:"source_index"`
	TypeIndex   int    `json:"type_index"`
	Codec       string `json:"codec"`
	TargetCodec string `json:"target_codec"`
}

// ExecutionPlan is the resolved stream-by-stream plan for building FFmpeg arguments.
type ExecutionPlan struct {
	Plan        *transcode.Plan              `json:"plan"`
	Streams     []StreamAction               `json:"streams"`
	Conversions []transcode.ConversionRecord `json:"conversions"`
	DurationSec float64                      `json:"duration_sec"`
}

// BuildExecutionPlan evaluates source streams against container compatibility policy and plan settings,
// producing a deterministic, stream-by-stream execution plan.
func BuildExecutionPlan(plan *transcode.Plan, streams []SourceStream, duration float64) (*ExecutionPlan, error) {
	if plan == nil {
		return nil, fmt.Errorf("transcode plan cannot be nil (fail closed)")
	}

	execPlan := &ExecutionPlan{
		Plan:        plan,
		DurationSec: duration,
	}

	for _, s := range streams {
		switch s.Kind {
		case "video":
			execPlan.Streams = append(execPlan.Streams, StreamAction{
				Kind:        "video",
				SourceIndex: s.Index,
				TypeIndex:   s.TypeIndex,
				Codec:       s.Codec,
				TargetCodec: plan.VideoCodec,
			})

		case "audio":
			execPlan.Streams = append(execPlan.Streams, StreamAction{
				Kind:        "audio",
				SourceIndex: s.Index,
				TypeIndex:   s.TypeIndex,
				Codec:       s.Codec,
				TargetCodec: "copy",
			})

		case "subtitle":
			if IsSubtitleCompatible(plan.Container, s.Codec) {
				execPlan.Streams = append(execPlan.Streams, StreamAction{
					Kind:        "subtitle",
					SourceIndex: s.Index,
					TypeIndex:   s.TypeIndex,
					Codec:       s.Codec,
					TargetCodec: "copy",
				})
			} else if plan.ConvertIncompatibleSubtitles {
				targetCodec, reason, ok := ConvertibleSubtitle(plan.Container, s.Codec)
				if !ok {
					return nil, fmt.Errorf("unsupported subtitle codec %q in stream %d (subtitle #%d) for container %s: cannot safely preserve or convert (fail closed)", s.Codec, s.Index, s.TypeIndex, plan.Container)
				}
				execPlan.Streams = append(execPlan.Streams, StreamAction{
					Kind:        "subtitle",
					SourceIndex: s.Index,
					TypeIndex:   s.TypeIndex,
					Codec:       s.Codec,
					TargetCodec: targetCodec,
				})
				execPlan.Conversions = append(execPlan.Conversions, transcode.ConversionRecord{
					StreamType:  "subtitle",
					StreamIndex: s.TypeIndex,
					FromCodec:   s.Codec,
					ToCodec:     targetCodec,
					Reason:      reason,
				})
			} else {
				return nil, fmt.Errorf("incompatible subtitle codec %q in stream %d (subtitle #%d) for container %s and convert_incompatible_subtitles is disabled (fail closed)", s.Codec, s.Index, s.TypeIndex, plan.Container)
			}

		case "attachment":
			if plan.PreserveAttachments {
				execPlan.Streams = append(execPlan.Streams, StreamAction{
					Kind:        "attachment",
					SourceIndex: s.Index,
					TypeIndex:   s.TypeIndex,
					Codec:       s.Codec,
					TargetCodec: "copy",
				})
			}
		}
	}

	return execPlan, nil
}
