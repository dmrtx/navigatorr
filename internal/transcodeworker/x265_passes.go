package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

type encoderPhaseContextKey struct{}

func reportEncoderPhase(ctx context.Context, phase string) error {
	if report, ok := ctx.Value(encoderPhaseContextKey{}).(func(string) error); ok {
		return report(phase)
	}
	return nil
}

// runEncoderCommand preserves the existing invocation for legacy plans. The
// two-pass path owns a private stats directory per sample/job, never accepts
// stats paths from callers, and never starts pass two after failure/cancellation.
func runEncoderCommand(ctx context.Context, cmd *exec.Cmd, args []string, plan *transcode.Plan, output string) error {
	if plan.X265 == nil {
		return runCharged(ctx, cmd)
	}
	if err := plan.X265.Validate(plan.VideoCodec, plan.Quality); err != nil {
		return err
	}
	if !filepath.IsAbs(output) {
		return fmt.Errorf("two-pass output must be absolute")
	}
	input := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-i" {
			input = args[i+1]
			break
		}
	}
	if !filepath.IsAbs(input) {
		return fmt.Errorf("two-pass input must be absolute")
	}
	statsDir, err := os.MkdirTemp(filepath.Dir(output), ".x265-stats-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(statsDir)
	video, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		return err
	}
	if err := reportEncoderPhase(ctx, "analysing_video"); err != nil {
		return err
	}
	// 0:V skips attached pictures, so cover art never becomes the analysed stream.
	first := []string{"-y", "-nostats", "-i", input, "-map", "0:V:0"}
	first = append(first, video...)
	first = append(first, "-an", "-sn", "-dn", "-f", "null", os.DevNull)
	first = withX265Pass(first, 1)
	passOne := exec.CommandContext(ctx, cmd.Path, first...)
	passOne.Dir = statsDir
	passOne.Env = cmd.Env
	passOne.Stdout = cmd.Stdout
	passOne.Stderr = cmd.Stderr
	passOne.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	passOne.Cancel = func() error {
		if passOne.Process != nil {
			return syscall.Kill(-passOne.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	passOne.WaitDelay = 2 * time.Second
	if err := runCharged(ctx, passOne); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("x265 first pass: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := reportEncoderPhase(ctx, "encoding"); err != nil {
		return err
	}
	cmd.Dir = statsDir
	cmd.Args = append([]string{cmd.Path}, withX265Pass(args, 2)...)
	return runCharged(ctx, cmd)
}

func withX265Pass(args []string, pass int) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-x265-params" {
			out[i+1] += fmt.Sprintf(":pass=%d:stats=x265.log", pass)
			return out
		}
	}
	return out
}
