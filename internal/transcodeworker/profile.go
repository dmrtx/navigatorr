package transcodeworker

import (
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

// BuiltinPlans returns the built-in transcode plans available on the worker.
func BuiltinPlans() map[string]transcode.Plan {
	return map[string]transcode.Plan{
		"hevc-vt": {
			Container:                    "mkv",
			VideoCodec:                   "hevc_videotoolbox",
			Quality:                      65,
			AudioMode:                    "copy",
			SubtitleMode:                 "preserve",
			ConvertIncompatibleSubtitles: true,
			PreserveMetadata:             true,
			PreserveChapters:             true,
			PreserveAttachments:          true,
		},
		"hevc-vt-balanced": {
			Container:                    "mkv",
			VideoCodec:                   "hevc_videotoolbox",
			Quality:                      65,
			AudioMode:                    "copy",
			SubtitleMode:                 "preserve",
			ConvertIncompatibleSubtitles: true,
			PreserveMetadata:             true,
			PreserveChapters:             true,
			PreserveAttachments:          true,
		},
		"hevc-vt-quality": {
			Container:                    "mkv",
			VideoCodec:                   "hevc_videotoolbox",
			Quality:                      55,
			AudioMode:                    "copy",
			SubtitleMode:                 "preserve",
			ConvertIncompatibleSubtitles: true,
			PreserveMetadata:             true,
			PreserveChapters:             true,
			PreserveAttachments:          true,
		},
		"hevc-vt-space": {
			Container:                    "mkv",
			VideoCodec:                   "hevc_videotoolbox",
			Quality:                      75,
			AudioMode:                    "copy",
			SubtitleMode:                 "preserve",
			ConvertIncompatibleSubtitles: true,
			PreserveMetadata:             true,
			PreserveChapters:             true,
			PreserveAttachments:          true,
		},
	}
}

// ValidatePlan verifies that all fields in a transcode Plan are strictly within
// allowed enumeration boundaries. Any unknown or arbitrary values are rejected (fail closed).
func ValidatePlan(p *transcode.Plan) error {
	if p == nil {
		return fmt.Errorf("transcode plan is nil (fail closed)")
	}

	container := strings.ToLower(strings.TrimSpace(p.Container))
	if container != "mkv" && container != "matroska" {
		return fmt.Errorf("unsupported container %q (only mkv is allowed; fail closed)", p.Container)
	}

	videoCodec := strings.ToLower(strings.TrimSpace(p.VideoCodec))
	if videoCodec != "hevc_videotoolbox" {
		return fmt.Errorf("unsupported video codec %q (only hevc_videotoolbox is allowed; fail closed)", p.VideoCodec)
	}

	if p.Quality < 1 || p.Quality > 100 {
		return fmt.Errorf("invalid quality level %d (must be between 1 and 100; fail closed)", p.Quality)
	}

	audioMode := strings.ToLower(strings.TrimSpace(p.AudioMode))
	if audioMode != "copy" {
		return fmt.Errorf("unsupported audio mode %q (only copy is allowed; fail closed)", p.AudioMode)
	}

	subtitleMode := strings.ToLower(strings.TrimSpace(p.SubtitleMode))
	if subtitleMode != "preserve" {
		return fmt.Errorf("unsupported subtitle mode %q (only preserve is allowed; fail closed)", p.SubtitleMode)
	}

	return nil
}

// ResolveWorkerPlan resolves a profile name and/or supplied Plan into a fully validated Plan.
// Worker enforces defense-in-depth: even if a plan is supplied across the SSH boundary,
// it is strictly validated against allowed enums.
func ResolveWorkerPlan(profile string, supplied *transcode.Plan) (*transcode.Plan, error) {
	if supplied != nil {
		if err := ValidatePlan(supplied); err != nil {
			return nil, fmt.Errorf("validating transcode plan: %w", err)
		}
		// Return a copy with normalized values
		planCopy := *supplied
		planCopy.Container = strings.ToLower(strings.TrimSpace(supplied.Container))
		planCopy.VideoCodec = strings.ToLower(strings.TrimSpace(supplied.VideoCodec))
		planCopy.AudioMode = strings.ToLower(strings.TrimSpace(supplied.AudioMode))
		planCopy.SubtitleMode = strings.ToLower(strings.TrimSpace(supplied.SubtitleMode))
		return &planCopy, nil
	}

	profName := strings.TrimSpace(profile)
	if profName == "" {
		profName = "hevc-vt"
	}

	builtins := BuiltinPlans()
	plan, ok := builtins[profName]
	if !ok {
		return nil, fmt.Errorf("unknown transcode profile %q on worker (fail closed)", profName)
	}

	return &plan, nil
}
