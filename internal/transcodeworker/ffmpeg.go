package transcodeworker

import (
	"bytes"
	"context"
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

// RunFFmpeg executes the transcode process using hevc_videotoolbox and streams preservation.
func RunFFmpeg(ctx context.Context, ffmpegPath string, quality int, job *JobRecord, progressPath, logPath string) error {
	if quality <= 0 {
		quality = 65
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("creating ffmpeg log file %s: %w", logPath, err)
	}
	defer logFile.Close()

	args := []string{
		"-y",
		"-progress", progressPath,
		"-nostats",
		"-i", job.Source,
		"-map", "0:v?",
		"-map", "0:a?",
		"-map", "0:s?",
		"-map", "0:t?",
		"-map_metadata", "0",
		"-map_chapters", "0",
		"-c", "copy",
		"-c:v", "hevc_videotoolbox",
		"-q:v", strconv.Itoa(quality),
		job.Candidate,
	}

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg execution failed: %w", err)
	}
	return nil
}
