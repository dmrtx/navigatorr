package transcodeworker

import (
	"context"
	"fmt"
	"os/exec"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

const (
	videoToolboxEncoder = "hevc_videotoolbox"
	libX265Encoder      = "libx265"
)

// ProbeAvailableEncoders runs ffmpeg -encoders and parses encoder availability.
func ProbeAvailableEncoders(ctx context.Context, ffmpegPath string) (map[string]bool, error) {
	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("probing available encoders failed: %w (%s)", err, boundedErrorMessage(err, out, 256))
	}
	return ParseAvailableEncoders(string(out)), nil
}

// ValidateEncoderCapabilities validates a resolved plan against the actual
// FFmpeg binary the worker will execute. It fails closed for any codec that is
// not proven available, dispatching on the plan's video codec.
func ValidateEncoderCapabilities(ctx context.Context, ffmpegPath string, plan *transcode.Plan) error {
	if plan == nil {
		return fmt.Errorf("encoder_capability_unsupported: transcode plan is nil")
	}
	switch norm(plan.VideoCodec) {
	case videoToolboxEncoder:
		caps, err := ProbeVideoToolboxCapabilities(ctx, ffmpegPath)
		if err != nil {
			return err
		}
		return ValidateVideoToolboxCapabilities(plan, caps)
	case libX265Encoder:
		available, err := ProbeAvailableEncoders(ctx, ffmpegPath)
		if err != nil {
			return fmt.Errorf("encoder_capability_unsupported: %w", err)
		}
		if !available[libX265Encoder] {
			return fmt.Errorf("encoder_capability_unsupported: encoder %s is unavailable in the configured FFmpeg (install an FFmpeg build with libx265)", libX265Encoder)
		}
		if preset := norm(plan.Preset); preset != "" && !transcode.IsValidLibX265Preset(preset) {
			return fmt.Errorf("encoder_capability_unsupported: unsupported libx265 preset %q", plan.Preset)
		}
		return nil
	default:
		return fmt.Errorf("encoder_capability_unsupported: unsupported video codec %q", plan.VideoCodec)
	}
}

type VideoToolboxCapabilities = transcode.VideoToolboxCapabilities

var (
	buildVersion   = ""
	buildGitCommit = ""
)

// SetBuildMetadata allows setting worker build version and git commit dynamically.
func SetBuildMetadata(version, gitCommit string) {
	buildVersion = strings.TrimSpace(version)
	buildGitCommit = strings.TrimSpace(gitCommit)
}

// GetBuildMetadata retrieves injected build metadata, falls back to runtime debug build info,
// or returns explicit "unknown".
func GetBuildMetadata() (version string, commit string) {
	version = buildVersion
	commit = buildGitCommit

	if version == "" || commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			if version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
				version = bi.Main.Version
			}
			if commit == "" {
				for _, setting := range bi.Settings {
					if setting.Key == "vcs.revision" {
						commit = setting.Value
						break
					}
				}
			}
		}
	}

	if version == "" {
		version = "unknown"
	}
	if commit == "" {
		commit = "unknown"
	}
	return version, commit
}

func boundedErrorMessage(err error, output []byte, maxLen int) string {
	var parts []string
	if err != nil {
		parts = append(parts, err.Error())
	}
	outStr := strings.TrimSpace(string(output))
	if outStr != "" {
		parts = append(parts, outStr)
	}
	msg := strings.Join(parts, ": ")
	if maxLen > 0 && len(msg) > maxLen {
		return msg[:maxLen] + "..."
	}
	return msg
}

// ProbeWorkerCapabilities probes full versioned capabilities of the worker node.
func ProbeWorkerCapabilities(ctx context.Context, ffmpegPath string) (transcode.WorkerCapabilities, error) {
	ver, commit := GetBuildMetadata()
	caps := transcode.WorkerCapabilities{
		ProtocolVersion: transcode.WorkerProtocolVersion,
		WorkerVersion:   ver,
		BuildGitCommit:  commit,
		FFmpegPath:      ffmpegPath,
		Encoders:        make(map[string]bool),
		Filters:         make(map[string]bool),
	}

	// 1. Probe FFmpeg version
	verCmd := exec.CommandContext(ctx, ffmpegPath, "-version")
	verOut, err := verCmd.CombinedOutput()
	if err != nil {
		errMsg := boundedErrorMessage(err, verOut, 256)
		caps.ProbeErrors = append(caps.ProbeErrors, transcode.ProbeError{
			Component: "ffmpeg_version",
			Message:   errMsg,
		})
		return caps, fmt.Errorf("probing ffmpeg version at %s failed: %w (%s)", ffmpegPath, err, errMsg)
	}
	caps.FFmpegVersion = ParseFFmpegVersion(string(verOut))

	// 2. Probe VideoToolbox details (partial absence does not fail the whole report, but records structured warning/error)
	vtCaps, vtErr := ProbeVideoToolboxCapabilities(ctx, ffmpegPath)
	if vtErr != nil {
		caps.ProbeErrors = append(caps.ProbeErrors, transcode.ProbeError{
			Component: "videotoolbox",
			Message:   boundedErrorMessage(vtErr, nil, 256),
		})
	}
	if caps.EncoderDetails == nil {
		caps.EncoderDetails = make(map[string]transcode.EncoderCapabilities)
	}
	caps.EncoderDetails[videoToolboxEncoder] = vtCaps

	// 3. Probe encoders availability (preserve partial parsed lines if command failed, but record structured warning/error)
	encCmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders")
	encOut, encErr := encCmd.CombinedOutput()
	if encErr != nil {
		caps.ProbeErrors = append(caps.ProbeErrors, transcode.ProbeError{
			Component: "encoders",
			Message:   boundedErrorMessage(encErr, encOut, 256),
		})
	}
	caps.Encoders = ParseAvailableEncoders(string(encOut))
	if vtCaps.Available {
		caps.Encoders[videoToolboxEncoder] = true
	}

	// 4. Probe filters availability (preserve partial parsed lines if command failed, but record structured warning/error)
	filtCmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-filters")
	filtOut, filtErr := filtCmd.CombinedOutput()
	if filtErr != nil {
		caps.ProbeErrors = append(caps.ProbeErrors, transcode.ProbeError{
			Component: "filters",
			Message:   boundedErrorMessage(filtErr, filtOut, 256),
		})
	}
	caps.Filters = ParseAvailableFilters(string(filtOut))

	// 5. Generate deterministic capability fingerprint
	fp, err := transcode.ComputeCapabilityFingerprint(caps)
	if err != nil {
		return caps, fmt.Errorf("generating capability fingerprint: %w", err)
	}
	caps.CapabilityFingerprint = fp

	return caps, nil
}

// ParseFFmpegVersion extracts the FFmpeg version string from ffmpeg -version output.
func ParseFFmpegVersion(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), "ffmpeg version ") {
			fields := strings.Fields(trimmed)
			if len(fields) >= 3 {
				return fields[2]
			}
			return trimmed
		}
	}
	return "unknown"
}

// ParseAvailableEncoders extracts presence of key encoders from ffmpeg -encoders output.
func ParseAvailableEncoders(raw string) map[string]bool {
	encoders := map[string]bool{
		"hevc_videotoolbox":   false,
		"h264_videotoolbox":   false,
		"prores_videotoolbox": false,
		"libx264":             false,
		"libx265":             false,
		"aac":                 false,
	}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			name := strings.ToLower(fields[1])
			if len(fields[0]) >= 6 && (strings.HasPrefix(fields[0], "V") || strings.HasPrefix(fields[0], "A") || strings.HasPrefix(fields[0], "S")) {
				encoders[name] = true
			}
		}
	}
	return encoders
}

// ParseAvailableFilters extracts presence of key filters from ffmpeg -filters output.
// Modern FFmpeg outputs 2-character flag columns (e.g. ".. libvmaf", "TS ssim"),
// while older FFmpeg versions output 3-character flag columns (e.g. "..C libvmaf", "... scale").
func ParseAvailableFilters(raw string) map[string]bool {
	filters := map[string]bool{
		"libvmaf": false,
		"ssim":    false,
		"scale":   false,
		"format":  false,
		"null":    false,
		"fps":     false,
	}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			flagLen := len(fields[0])
			if (flagLen == 2 || flagLen == 3) && fields[1] != "=" {
				name := strings.ToLower(fields[1])
				filters[name] = true
			}
		}
	}
	return filters
}

func isCleanAbsence(encoderName, output string) bool {
	lower := strings.ToLower(output)
	enc := strings.ToLower(strings.TrimSpace(encoderName))
	if enc == "" {
		enc = videoToolboxEncoder
	}

	// Must specifically mention the queried encoder
	if !strings.Contains(lower, enc) {
		return false
	}

	// Known clean absence messages output by FFmpeg when an encoder is not built into the binary.
	// Generic errors like "unrecognized option" or "not found" (e.g. missing libraries/binaries)
	// must NOT match so they remain ProbeErrors.
	return strings.Contains(lower, fmt.Sprintf("codec '%s' is not recognized by ffmpeg", enc)) ||
		strings.Contains(lower, fmt.Sprintf("encoder '%s' not found", enc)) ||
		strings.Contains(lower, fmt.Sprintf("unknown encoder '%s'", enc)) ||
		strings.Contains(lower, fmt.Sprintf("cannot find encoder '%s'", enc)) ||
		(strings.Contains(lower, "is not recognized by ffmpeg") && strings.Contains(lower, enc))
}

func ProbeVideoToolboxCapabilities(ctx context.Context, ffmpegPath string) (VideoToolboxCapabilities, error) {
	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-h", "encoder="+videoToolboxEncoder)
	out, err := cmd.CombinedOutput()
	caps := ParseVideoToolboxCapabilities(string(out))
	if err != nil {
		if ctx.Err() != nil {
			return caps, ctx.Err()
		}
		if isCleanAbsence(videoToolboxEncoder, string(out)) {
			caps.Available = false
			return caps, nil
		}
		msg := strings.TrimSpace(string(out))
		if len(msg) > 512 {
			msg = msg[:512] + "..."
		}
		return caps, fmt.Errorf("encoder_capability_unsupported: probing %s capabilities failed: %w (%s)", videoToolboxEncoder, err, msg)
	}
	if !caps.Available {
		// Clean absence when help returns exit code 0 but does not report the encoder
		return caps, nil
	}
	return caps, nil
}

func ParseVideoToolboxCapabilities(raw string) VideoToolboxCapabilities {
	caps := VideoToolboxCapabilities{Encoder: videoToolboxEncoder}
	lower := strings.ToLower(raw)
	caps.Available = strings.Contains(lower, "encoder hevc_videotoolbox") || strings.Contains(lower, "hevc_videotoolbox avoptions")

	profileSet := map[string]bool{}
	pixelSet := map[string]bool{}
	optionSet := map[string]bool{}
	inProfileValues := false

	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		ll := strings.ToLower(line)
		if strings.HasPrefix(ll, "supported pixel formats:") {
			for _, f := range strings.Fields(strings.TrimSpace(line[len("Supported pixel formats:"):])) {
				pixelSet[strings.ToLower(f)] = true
			}
			continue
		}
		if strings.HasPrefix(line, "-") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				name := strings.TrimPrefix(strings.ToLower(fields[0]), "-")
				switch name {
				case "profile", "prio_speed", "spatial_aq", "realtime":
					optionSet[name] = true
				}
				inProfileValues = name == "profile"
			}
			continue
		}
		if inProfileValues {
			fields := strings.Fields(ll)
			if len(fields) >= 2 && (fields[0] == "main" || fields[0] == "main10") {
				profileSet[fields[0]] = true
			}
		}
	}

	caps.Profiles = sortedKeys(profileSet)
	caps.PixelFormats = sortedKeys(pixelSet)
	caps.Options = sortedKeys(optionSet)
	return caps
}

func ValidateVideoToolboxCapabilities(plan *transcode.Plan, caps VideoToolboxCapabilities) error {
	if plan == nil {
		return fmt.Errorf("encoder_capability_unsupported: transcode plan is nil")
	}
	if norm(plan.VideoCodec) != videoToolboxEncoder {
		return fmt.Errorf("encoder_capability_unsupported: capability validator only supports %s, got %q", videoToolboxEncoder, plan.VideoCodec)
	}
	if !caps.Available {
		return fmt.Errorf("encoder_capability_unsupported: encoder %s is unavailable", videoToolboxEncoder)
	}
	if plan.VideoProfile != "" {
		if !contains(caps.Options, "profile") {
			return fmt.Errorf("encoder_capability_unsupported: FFmpeg does not expose the profile option for %s", videoToolboxEncoder)
		}
		if !contains(caps.Profiles, norm(plan.VideoProfile)) {
			return fmt.Errorf("encoder_capability_unsupported: HEVC profile %q is not reported by installed FFmpeg (reported: %v)", plan.VideoProfile, caps.Profiles)
		}
	}
	if plan.PixelFormat != "" && !contains(caps.PixelFormats, norm(plan.PixelFormat)) {
		return fmt.Errorf("encoder_capability_unsupported: pixel format %q is not reported by installed FFmpeg for %s (reported: %v)", plan.PixelFormat, videoToolboxEncoder, caps.PixelFormats)
	}
	for _, knob := range []struct {
		name string
		set  bool
	}{
		{name: "prio_speed", set: plan.PrioritizeSpeed != nil},
		{name: "spatial_aq", set: plan.SpatialAQ != nil},
		{name: "realtime", set: plan.Realtime != nil},
	} {
		if knob.set && !contains(caps.Options, knob.name) {
			return fmt.Errorf("encoder_capability_unsupported: FFmpeg does not expose %s for %s", knob.name, videoToolboxEncoder)
		}
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(values []string, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, v := range values {
		if strings.ToLower(strings.TrimSpace(v)) == want {
			return true
		}
	}
	return false
}
