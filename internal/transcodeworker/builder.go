package transcodeworker

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

func BuildVideoEncoderArgs(plan *transcode.Plan) ([]string, error) {
	if plan == nil {
		return nil, fmt.Errorf("video plan cannot be nil (fail closed)")
	}
	codec := norm(plan.VideoCodec)
	if codec != "hevc_videotoolbox" {
		return nil, fmt.Errorf("unsupported video codec %q (fail closed)", plan.VideoCodec)
	}
	quality := plan.Quality
	if quality <= 0 {
		quality = 65
	}
	if quality < 1 || quality > 100 {
		return nil, fmt.Errorf("invalid quality %d (must be 1-100)", quality)
	}
	profile := norm(plan.VideoProfile)
	pixelFormat := norm(plan.PixelFormat)
	if profile != "" && profile != "main" && profile != "main10" {
		return nil, fmt.Errorf("unsupported HEVC profile %q", plan.VideoProfile)
	}
	if pixelFormat != "" && pixelFormat != "yuv420p" && pixelFormat != "p010le" {
		return nil, fmt.Errorf("unsupported pixel format %q", plan.PixelFormat)
	}
	if profile == "main10" && pixelFormat != "p010le" {
		return nil, fmt.Errorf("HEVC main10 requires pixel format p010le")
	}
	if pixelFormat == "p010le" && profile != "main10" {
		return nil, fmt.Errorf("pixel format p010le requires HEVC main10")
	}
	if plan.ExpectedBitDepth != 0 && plan.ExpectedBitDepth != 8 && plan.ExpectedBitDepth != 10 {
		return nil, fmt.Errorf("unsupported expected bit depth %d", plan.ExpectedBitDepth)
	}
	if plan.ExpectedBitDepth == 10 && (profile != "main10" || pixelFormat != "p010le") {
		return nil, fmt.Errorf("expected 10-bit output requires main10 + p010le")
	}
	if profile == "main10" && plan.ExpectedBitDepth != 10 {
		return nil, fmt.Errorf("main10 plan must require expected bit depth 10")
	}

	args := []string{"-c:v", codec, "-q:v", strconv.Itoa(quality)}
	if profile != "" {
		args = append(args, "-profile:v", profile)
	}
	if pixelFormat != "" {
		args = append(args, "-pix_fmt", pixelFormat)
	}
	if plan.PrioritizeSpeed != nil {
		args = append(args, "-prio_speed", boolFFmpeg(*plan.PrioritizeSpeed))
	}
	if plan.SpatialAQ != nil {
		args = append(args, "-spatial_aq", boolFFmpeg(*plan.SpatialAQ))
	}
	if plan.Realtime != nil {
		args = append(args, "-realtime", boolFFmpeg(*plan.Realtime))
	}
	return args, nil
}

func boolFFmpeg(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// BuildFFmpegArgs generates the exact, safe FFmpeg argument slice from an ExecutionPlan.
// It explicitly specifies codec mapping per stream type and subtitle stream index,
// avoiding dangerous global defaults that could corrupt stylized subtitles or mux incompatible codecs.
func BuildFFmpegArgs(execPlan *ExecutionPlan, sourcePath, candidatePath, progressPath string) ([]string, error) {
	if execPlan == nil || execPlan.Plan == nil {
		return nil, fmt.Errorf("execution plan cannot be nil (fail closed)")
	}
	if strings.TrimSpace(sourcePath) == "" || strings.TrimSpace(candidatePath) == "" {
		return nil, fmt.Errorf("source and candidate paths are required (fail closed)")
	}

	args := []string{
		"-y",
		"-progress", progressPath,
		"-nostats",
		"-i", sourcePath,
		"-map", "0:v?",
		"-map", "0:a?",
		"-map", "0:s?",
		"-map", "0:t?",
	}

	if execPlan.Plan.PreserveMetadata {
		args = append(args, "-map_metadata", "0")
	}
	if execPlan.Plan.PreserveChapters {
		args = append(args, "-map_chapters", "0")
	}

	videoArgs, err := BuildVideoEncoderArgs(execPlan.Plan)
	if err != nil {
		return nil, err
	}
	args = append(args, videoArgs...)

	// Audio mode: copy
	args = append(args, "-c:a", "copy")

	// Subtitle streams: specify explicitly per subtitle stream index
	for _, stream := range execPlan.Streams {
		if stream.Kind == "subtitle" {
			args = append(args, fmt.Sprintf("-c:s:%d", stream.TypeIndex), stream.TargetCodec)
		}
	}

	// Attachments: copy
	if execPlan.Plan.PreserveAttachments {
		args = append(args, "-c:t", "copy")
	}

	args = append(args, candidatePath)

	return args, nil
}
