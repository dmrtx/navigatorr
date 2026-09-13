package transcodeworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ResolveToolPath finds the tool at specified path or falls back to PATH.
func ResolveToolPath(configured, fallbackName string) string {
	if configured != "" {
		if fi, err := os.Stat(configured); err == nil && !fi.IsDir() {
			return configured
		}
	}
	if p, err := exec.LookPath(fallbackName); err == nil {
		return p
	}
	return configured
}

// CheckVideoToolboxEncoder verifies that hevc_videotoolbox is available in FFmpeg.
func CheckVideoToolboxEncoder(ctx context.Context, ffmpegPath string) (bool, error) {
	cmd := exec.CommandContext(ctx, ffmpegPath, "-encoders")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("running %s -encoders: %w", ffmpegPath, err)
	}
	return strings.Contains(stdout.String(), "hevc_videotoolbox"), nil
}

// ProbeDuration uses ffprobe to determine the duration in seconds of a media file.
func ProbeDuration(ctx context.Context, ffprobePath, filePath string) (float64, error) {
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		filePath,
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("probing duration with %s: %w", ffprobePath, err)
	}
	durStr := strings.TrimSpace(stdout.String())
	if durStr == "" || durStr == "N/A" {
		return 0, nil
	}
	dur, err := strconv.ParseFloat(durStr, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing duration %q: %w", durStr, err)
	}
	return dur, nil
}

// SummarizeFFmpegError extracts a bounded, sanitized error summary from the tail of ffmpeg.log.
func SummarizeFFmpegError(logPath string, runErr error) string {
	data, err := os.ReadFile(logPath)
	if err != nil || len(data) == 0 {
		return fmt.Sprintf("ffmpeg execution failed: %v", runErr)
	}

	if len(data) > 8192 {
		data = data[len(data)-8192:]
	}

	lines := strings.Split(string(data), "\n")
	var errorLines []string

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "frame=") || strings.HasPrefix(lower, "size=") || strings.HasPrefix(lower, "video:") {
			continue
		}
		if strings.EqualFold(lower, "conversion failed!") || strings.HasPrefix(lower, "error initializing output stream") {
			continue
		}
		if strings.Contains(lower, "error") ||
			strings.Contains(lower, "could not") ||
			strings.Contains(lower, "not supported") ||
			strings.Contains(lower, "failed") ||
			strings.Contains(lower, "function not implemented") ||
			strings.Contains(lower, "invalid") {
			cleaned := line
			if idx := strings.Index(cleaned, "] "); idx != -1 {
				cleaned = cleaned[idx+2:]
			}
			errorLines = append([]string{cleaned}, errorLines...)
			if len(errorLines) >= 3 {
				break
			}
		}
	}

	if len(errorLines) > 0 {
		summary := strings.Join(errorLines, "; ")
		if len(summary) > 300 {
			summary = summary[:300] + "..."
		}
		return fmt.Sprintf("ffmpeg failed (%v): %s", runErr, summary)
	}

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && !strings.HasPrefix(line, "frame=") {
			if len(line) > 200 {
				line = line[:200] + "..."
			}
			return fmt.Sprintf("ffmpeg failed (%v): %s", runErr, line)
		}
	}

	return fmt.Sprintf("ffmpeg execution failed: %v", runErr)
}

var (
	spatialAQRegex            = regexp.MustCompile(`(?i)\bspatial[\s_.-]*aq\b`)
	spatialAQUnsupportedRegex = regexp.MustCompile(`(?i)(not\s+support|n't\s+support|no\s+support|unsupport|non-support|nonsupport|ignor|not\s+available|unavailable|not\s+accept)`)
)

// DetectSpatialAQWarning inspects stderr/log output line-by-line for VideoToolbox warnings
// specifically indicating that spatial AQ is unsupported or its value was ignored.
// It returns true and the cleaned warning message if detected, or false and empty string otherwise.
func DetectSpatialAQWarning(stderr string) (bool, string) {
	for _, rawLine := range strings.Split(stderr, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if spatialAQRegex.MatchString(line) && spatialAQUnsupportedRegex.MatchString(line) {
			cleaned := line
			if idx := strings.LastIndex(cleaned, "] "); idx != -1 {
				cleaned = strings.TrimSpace(cleaned[idx+2:])
			}
			return true, cleaned
		}
	}
	return false, ""
}

// HasSpatialAQUnsupportedWarning returns true if stderr contains a VideoToolbox warning
// specifically indicating that spatial AQ is unsupported or its value was ignored.
func HasSpatialAQUnsupportedWarning(stderr string) bool {
	matched, _ := DetectSpatialAQWarning(stderr)
	return matched
}

// RunFFmpeg executes the transcode process using an ExecutionPlan and stream-by-stream codec mapping.
func RunFFmpeg(ctx context.Context, ffmpegPath string, execPlan *ExecutionPlan, job *JobRecord, progressPath, logPath string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if execPlan == nil || execPlan.Plan == nil {
		return fmt.Errorf("execution plan cannot be nil")
	}

	caps, err := ProbeVideoToolboxCapabilities(ctx, ffmpegPath)
	if err != nil {
		return err
	}
	if err := ValidateVideoToolboxCapabilities(execPlan.Plan, caps); err != nil {
		return err
	}

	args, err := BuildFFmpegArgs(execPlan, job.Source, job.Candidate, progressPath)
	if err != nil {
		return fmt.Errorf("building ffmpeg arguments: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("creating ffmpeg log file %s: %w", logPath, err)
	}
	defer logFile.Close()

	var stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stdout = logFile
	if execPlan.Plan.SpatialAQ != nil {
		cmd.Stderr = io.MultiWriter(logFile, &stderrBuf)
	} else {
		cmd.Stderr = logFile
	}

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New(SummarizeFFmpegError(logPath, err))
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if execPlan.Plan.SpatialAQ != nil {
		stderrOutput := stderrBuf.String()
		if stderrOutput == "" {
			if data, err := os.ReadFile(logPath); err == nil {
				stderrOutput = string(data)
			}
		}
		if matched, warning := DetectSpatialAQWarning(stderrOutput); matched {
			return fmt.Errorf("encoder_capability_unsupported: VideoToolbox spatial_aq unsupported by device: %s", warning)
		}
	}
	return nil
}
