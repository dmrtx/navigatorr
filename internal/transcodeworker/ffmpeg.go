package transcodeworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

	// Focus on the tail (last 8KB max)
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
		// Skip progress-like lines
		if strings.HasPrefix(lower, "frame=") || strings.HasPrefix(lower, "size=") || strings.HasPrefix(lower, "video:") {
			continue
		}
		// Skip generic FFmpeg trailers to prioritize specific root causes
		if strings.EqualFold(lower, "conversion failed!") || strings.HasPrefix(lower, "error initializing output stream") {
			continue
		}
		if strings.Contains(lower, "error") ||
			strings.Contains(lower, "could not") ||
			strings.Contains(lower, "not supported") ||
			strings.Contains(lower, "failed") ||
			strings.Contains(lower, "function not implemented") ||
			strings.Contains(lower, "invalid") {
			// Strip leading prefix like [matroska @ 0x12345] if present
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

	// Fallback to last non-empty line
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

// RunFFmpeg executes the transcode process using an ExecutionPlan and stream-by-stream codec mapping.
func RunFFmpeg(ctx context.Context, ffmpegPath string, execPlan *ExecutionPlan, job *JobRecord, progressPath, logPath string) error {
	if execPlan == nil {
		return fmt.Errorf("execution plan cannot be nil")
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

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Run(); err != nil {
		return errors.New(SummarizeFFmpegError(logPath, err))
	}
	return nil
}
