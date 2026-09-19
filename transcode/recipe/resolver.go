package recipe

import (
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

func Resolve(s *Snapshot, profileName string, overrides map[string]Profile, subtitles []SourceSubtitle) (*transcode.Plan, error) {
	if s == nil {
		return nil, fmt.Errorf("no active recipe bundle")
	}
	name := strings.TrimSpace(profileName)
	if name == "" {
		name = "hevc-vt"
	}
	p, ok := s.Bundle.Profiles[name]
	if op, exists := overrides[name]; exists {
		p = op
		ok = true
	}
	if !ok {
		return nil, fmt.Errorf("unknown transcode profile %q", name)
	}
	if err := ValidateProfile(name, p); err != nil {
		return nil, err
	}
	container := normalizeContainer(p.Container)
	c, ok := s.Bundle.Containers[container]
	if !ok {
		return nil, fmt.Errorf("profile %q references missing container policy %q", name, container)
	}
	fallbackAllowed := false
	for _, f := range p.Resilience.Fallbacks {
		if f.When == "container_subtitle_incompatible" && f.Action == "apply_container_conversion" {
			fallbackAllowed = true
		}
	}
	copySet := map[string]bool{}
	for _, v := range c.SubtitleCopy {
		copySet[normalizeCodec(v)] = true
	}
	actions := make([]transcode.SubtitleAction, 0, len(subtitles))
	applied := []string{}
	usedFallback := false
	for _, sub := range subtitles {
		codec := normalizeCodec(sub.Codec)
		if codec == "" {
			return nil, fmt.Errorf("subtitle stream %d has empty codec (fail closed)", sub.SourceStreamIndex)
		}
		if copySet[codec] {
			actions = append(actions, transcode.SubtitleAction{SourceStreamIndex: sub.SourceStreamIndex, TypeIndex: sub.TypeIndex, SourceCodec: codec, Operation: "copy", Codec: "copy"})
			continue
		}
		conv, exists := c.SubtitleConversions[codec]
		if !exists || !p.Subtitles.ConvertIncompatible || !fallbackAllowed {
			return nil, fmt.Errorf("unsupported subtitle codec %q in stream %d for container %s: no recipe-authorized safe conversion (fail closed)", codec, sub.SourceStreamIndex, container)
		}
		if !usedFallback {
			if p.Resilience.MaxFallbacks < 1 {
				return nil, fmt.Errorf("profile %q disallows fallbacks required to preserve subtitle codec %q", name, codec)
			}
			applied = append(applied, "container_subtitle_incompatible:apply_container_conversion")
			usedFallback = true
		}
		actions = append(actions, transcode.SubtitleAction{SourceStreamIndex: sub.SourceStreamIndex, TypeIndex: sub.TypeIndex, SourceCodec: codec, Operation: "transcode", Codec: normalizeCodec(conv.TargetCodec), Reason: conv.Reason})
	}
	retryOn := []string{}
	for _, f := range p.Resilience.Fallbacks {
		if f.Action == "retry" {
			retryOn = append(retryOn, f.When)
		}
	}
	videoProfile := normalizeCodec(p.Video.Profile)
	pixelFormat := normalizeCodec(p.Video.PixelFormat)
	expectedBitDepth := 0
	if videoProfile == "main10" {
		expectedBitDepth = 10
	} else if videoProfile == "main" || pixelFormat == "yuv420p" {
		expectedBitDepth = 8
	}
	plan := &transcode.Plan{
		Container: container, VideoCodec: normalizeCodec(p.Video.Codec), Quality: p.Video.Quality,
		Preset:       normalizeCodec(p.Video.Preset),
		VideoProfile: videoProfile, PixelFormat: pixelFormat, PrioritizeSpeed: cloneBool(p.Video.PrioritizeSpeed), SpatialAQ: cloneBool(p.Video.SpatialAQ), Realtime: cloneBool(p.Video.Realtime),
		AverageBitrateKbps: p.Video.AverageBitrateKbps, MaxBitrateKbps: p.Video.MaxBitrateKbps, ConstantBitrate: cloneBool(p.Video.ConstantBitrate),
		QMin: cloneInt(p.Video.QMin), QMax: cloneInt(p.Video.QMax), GOPSize: cloneInt(p.Video.GOPSize), BFrames: cloneInt(p.Video.BFrames),
		ClosedGOP: cloneBool(p.Video.ClosedGOP), PowerEfficient: cloneBool(p.Video.PowerEfficient), MaxRefFrames: cloneInt(p.Video.MaxRefFrames),
		ExpectedBitDepth: expectedBitDepth,
		AudioMode:        strings.ToLower(strings.TrimSpace(p.Audio.Mode)), SubtitleMode: strings.ToLower(strings.TrimSpace(p.Subtitles.Mode)),
		ConvertIncompatibleSubtitles: p.Subtitles.ConvertIncompatible, PreserveMetadata: p.Preserve.Metadata, PreserveChapters: p.Preserve.Chapters, PreserveAttachments: p.Preserve.Attachments,
		SubtitleActions: actions, RecipeVersion: s.Identity.Version, RecipeDigest: s.Identity.Digest,
		Resilience:       transcode.ResiliencePlan{MaxAttempts: p.Resilience.MaxAttempts, TransientRetries: p.Resilience.TransientRetries, RetryBackoffSeconds: append([]int(nil), p.Resilience.RetryBackoffSeconds...), MaxFallbacks: p.Resilience.MaxFallbacks, RetryOn: retryOn},
		AppliedFallbacks: applied,
	}
	digest, err := transcode.DigestPlan(plan)
	if err != nil {
		return nil, err
	}
	plan.PlanDigest = digest
	return plan, nil
}

func cloneBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func cloneInt(v *int) *int {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
