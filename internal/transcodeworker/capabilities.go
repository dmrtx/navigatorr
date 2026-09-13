package transcodeworker

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

const videoToolboxEncoder = "hevc_videotoolbox"

type VideoToolboxCapabilities struct {
	Encoder      string   `json:"encoder"`
	Available    bool     `json:"available"`
	Profiles     []string `json:"profiles,omitempty"`
	PixelFormats []string `json:"pixel_formats,omitempty"`
	Options      []string `json:"options,omitempty"`
}

func ProbeVideoToolboxCapabilities(ctx context.Context, ffmpegPath string) (VideoToolboxCapabilities, error) {
	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-h", "encoder="+videoToolboxEncoder)
	out, err := cmd.CombinedOutput()
	caps := ParseVideoToolboxCapabilities(string(out))
	if err != nil {
		if ctx.Err() != nil {
			return caps, ctx.Err()
		}
		msg := strings.TrimSpace(string(out))
		if len(msg) > 512 {
			msg = msg[:512] + "..."
		}
		return caps, fmt.Errorf("encoder_capability_unsupported: probing %s capabilities failed: %w (%s)", videoToolboxEncoder, err, msg)
	}
	if !caps.Available {
		return caps, fmt.Errorf("encoder_capability_unsupported: FFmpeg help did not report encoder %s", videoToolboxEncoder)
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
