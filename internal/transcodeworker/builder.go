package transcodeworker

import (
	"fmt"
	"strconv"
)

// BuildFFmpegArgs generates the exact, safe FFmpeg argument slice from an ExecutionPlan.
// It explicitly specifies codec mapping per stream type and subtitle stream index,
// avoiding dangerous global defaults that could corrupt stylized subtitles or mux incompatible codecs.
func BuildFFmpegArgs(execPlan *ExecutionPlan, sourcePath, candidatePath, progressPath string) ([]string, error) {
	if execPlan == nil || execPlan.Plan == nil {
		return nil, fmt.Errorf("execution plan cannot be nil (fail closed)")
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

	// Video codec & quality
	args = append(args, "-c:v", execPlan.Plan.VideoCodec)
	quality := execPlan.Plan.Quality
	if quality <= 0 {
		quality = 65
	}
	args = append(args, "-q:v", strconv.Itoa(quality))

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
