package transcodeworker

import (
	"context"
	"fmt"
	"os"
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
	if norm(plan.AudioMode) == "he_aac" || transcode.IsAudioLevelMode(norm(plan.AudioMode)) {
		available, err := ProbeAvailableEncoders(ctx, ffmpegPath)
		if err != nil || !available["aac_at"] {
			return fmt.Errorf("encoder_capability_unsupported: %s audio requires AudioToolbox aac_at: %v", norm(plan.AudioMode), err)
		}
	}
	if norm(plan.AudioMode) == "compact" || norm(plan.AudioMode) == transcode.AudioModeCompactLossless || transcode.IsAudioLevelMode(norm(plan.AudioMode)) {
		available, err := ProbeAvailableEncoders(ctx, ffmpegPath)
		if err != nil || !available["aac"] {
			return fmt.Errorf("encoder_capability_unsupported: compact audio requires AAC encoder: %v", err)
		}
	}
	switch norm(plan.VideoCodec) {
	case transcode.VideoCodecCopy:
		if err := transcode.ValidateSizeAudioOnlyShape(plan); err != nil {
			return fmt.Errorf("encoder_capability_unsupported: %w", err)
		}
		return nil
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
		if plan.Compact != nil {
			const need = "encoder_capability_unsupported: compact libx265 encodes need FFmpeg's -dolbyvision option (FFmpeg 7.1 or later) to switch Dolby Vision RPU coding off"
			ok, err := libX265HasDolbyVisionOption(ctx, ffmpegPath)
			if err != nil {
				return fmt.Errorf("%s: %w", need, err)
			}
			if !ok {
				return fmt.Errorf("%s; the configured FFmpeg lacks it", need)
			}
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
	return ProbeWorkerCapabilitiesWithScratch(ctx, ffmpegPath, os.TempDir())
}

// ProbeWorkerCapabilitiesWithScratch keeps executable quality probes inside
// worker-owned scratch when called by a running worker.
func ProbeWorkerCapabilitiesWithScratch(ctx context.Context, ffmpegPath, scratchRoot string) (transcode.WorkerCapabilities, error) {
	return probeWorkerCapabilitiesWithScratch(ctx, ffmpegPath, scratchRoot, true)
}

// The encoder/filter inventory is always probed. Native 10-bit VMAF also needs
// executable proof, even without an explicit model.
func probeWorkerCapabilitiesWithScratch(ctx context.Context, ffmpegPath, scratchRoot string, probeQuality bool) (transcode.WorkerCapabilities, error) {
	ver, commit := GetBuildMetadata()
	caps := transcode.WorkerCapabilities{
		// The search-only flags here and below stay for fingerprint stability and go when the fingerprint is versioned.
		ExplicitMain10Benchmarks: true,
		AutomaticModesV2:         true,
		AutomaticQualityPruning:  true,
		SavingsPrecheck:          true,
		ProtocolVersion:          transcode.WorkerProtocolVersion,
		WorkerVersion:            ver,
		BuildGitCommit:           commit,
		FFmpegPath:               ffmpegPath,
		Encoders:                 make(map[string]bool),
		Filters:                  make(map[string]bool),
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
	caps.CompactAudio = encErr == nil && caps.Encoders["aac"]
	caps.X265TwoPass = encErr == nil && caps.Encoders["libx265"]
	caps.HEAACAudio = encErr == nil && caps.Encoders["aac_at"]
	// Cover art (attached pictures) is kept out of the video encode and
	// re-attached byte-identically; the server only admits such sources
	// against workers that advertise it.
	caps.CoverArt = true
	// Size policy v3 compacts lossless audio with the same AAC encoder.
	caps.SizeModeV3 = caps.CompactAudio
	// Audio-only size plans stream-copy the video (keeping leading non-key
	// packets) and prove it unchanged from ffprobe's per-packet hashes.
	caps.SizeAudioOnly = caps.SizeModeV3
	// Size policy v5 converts per track to AAC-LC or HE-AAC, so it needs both
	// encoders.
	caps.SizeAudioLevels = caps.CompactAudio && caps.HEAACAudio
	// Track selection maps explicit streams and validates the output against
	// them; it needs no encoder of its own.
	caps.TrackSelection = true
	// Search-only flags, kept for fingerprint stability; they go when the
	// fingerprint is versioned.
	// Mode searches may stretch their budget while work keeps finishing.
	caps.SearchBudgetExtension = true
	// Size searches may carry automatic_search rules (selection margin,
	// quality-first verification, preset-ordered quality fallback).
	caps.AutomaticSearchRules = true
	// ...verified on the whole final validation grid, until a candidate passes
	// or none is eligible, with fresh windows reported apart in final validation.
	caps.AutomaticGridVerification = true
	// ...walking the quality fallback one rung at a time per encoder, refining
	// between a passing and a failing ladder rung, within the longer candidate
	// list.
	caps.AutomaticQualityWalk = true
	// Size searches may carry hardware_search rules (VideoToolbox candidates
	// that win within a size margin of the x265 winner).
	caps.HardwareSearch = true
	// ...and a higher-quality VideoToolbox fallback when the whole ladder
	// fails quality.
	caps.HardwareFallbackSearch = true
	// ...held to the frozen larger VMAF selection margin.
	caps.HardwareSelectionMargin = true
	// A size search that follows a failed VideoToolbox result may reuse the
	// earlier search's measured x265 samples.
	caps.SearchContinuation = true
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
	// Filters missing from -filters are a confirmed absence; only a build that
	// lists them all is asked to execute the chain.
	compactFilters := caps.Filters["setparams"] && caps.Filters["scale"] && caps.Filters["format"] && caps.Filters["lut3d"] && caps.Filters["sidedata"]
	compactEncoder := caps.Encoders[videoToolboxEncoder]
	if !compactEncoder && caps.Encoders[libX265Encoder] {
		compactEncoder, _ = libX265HasDolbyVisionOption(ctx, ffmpegPath)
	}
	if filtErr == nil && encErr == nil && compactFilters && compactEncoder {
		if err := probeCompactChain(ctx, ffmpegPath); err != nil {
			caps.ProbeErrors = append(caps.ProbeErrors, transcode.ProbeError{Component: "compact_transcode", Message: boundedErrorMessage(err, nil, 256)})
		} else {
			caps.CompactTranscode = true
		}
	}
	if probeQuality && caps.Filters["libvmaf"] {
		qualityCaps := cachedQualityCapabilities(ctx, ffmpegPath, scratchRoot)
		caps.Quality = &qualityCaps
	}

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

// libX265HasDolbyVisionOption reports whether FFmpeg's libx265 wrapper has
// the -dolbyvision option. Its default ("auto") enables RPU coding whenever
// the input carries a Dolby Vision configuration record, which is stream
// side data the compact chain's sidedata filter cannot remove, so compact
// encodes always pass "-dolbyvision 0". Builds without the option (before
// FFmpeg 7.1) cannot code RPUs at all, but are refused rather than trusted.
func libX265HasDolbyVisionOption(ctx context.Context, ffmpegPath string) (bool, error) {
	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-h", "encoder="+libX265Encoder).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("probing %s options: %w", libX265Encoder, err)
	}
	return parseEncoderHasOption(string(out), "dolbyvision"), nil
}

// parseEncoderHasOption finds an AVOption line ("-name <type> ...") in
// `ffmpeg -h encoder=...` output.
func parseEncoderHasOption(raw, name string) bool {
	for _, line := range strings.Split(raw, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && strings.EqualFold(fields[0], "-"+name) {
			return true
		}
	}
	return false
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
				case "profile", "prio_speed", "spatial_aq", "realtime",
					"constant_bit_rate", "power_efficient", "max_ref_frames":
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
	// VideoToolbox-specific options must be reported by the installed FFmpeg;
	// anything requested but absent fails closed instead of being silently
	// dropped. Generic codec controls (-b:v, -maxrate, -qmin/-qmax, -g, -bf,
	// -flags cgop) are core FFmpeg options and are validated structurally by
	// BuildVideoEncoderArgs, so they need no encoder-option probe gate.
	for _, knob := range []struct {
		name string
		set  bool
	}{
		{name: "prio_speed", set: plan.PrioritizeSpeed != nil},
		{name: "spatial_aq", set: plan.SpatialAQ != nil},
		{name: "realtime", set: plan.Realtime != nil},
		{name: "constant_bit_rate", set: plan.ConstantBitrate != nil},
		{name: "power_efficient", set: plan.PowerEfficient != nil},
		{name: "max_ref_frames", set: plan.MaxRefFrames != nil},
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
