package action

import (
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

// CompressionGoal says how one goal compresses a file today, in words built
// from the policy a new job of that goal freezes now: changing the policy
// changes the page, and the digest names the exact policy described.
type CompressionGoal struct {
	Goal    string `json:"goal"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Picture string `json:"picture"`
	Video   string `json:"video"`
	Quality string `json:"quality,omitempty"`
	Audio   string `json:"audio"`
	Tracks  string `json:"tracks,omitempty"`
	Keeps   string `json:"keeps"`
}

// CompressionChange is one dated change to how files are compressed.
type CompressionChange struct {
	Date    string `json:"date"`
	Version string `json:"version,omitempty"`
	Text    string `json:"text"`
}

// CompressionToday is the "how we compress today" page: each goal as a new job
// freezes it now, and what changed and when.
type CompressionToday struct {
	Goals   []CompressionGoal   `json:"goals"`
	Changes []CompressionChange `json:"changes"`
}

// compressionGoals are the goals the page describes, in the order the goal
// form lists them.
var compressionGoals = []struct {
	goal, mode string
	audioOnly  bool
}{{"size", "size", false}, {"compact", "compact", false}, {AudioOnlyGoal, "size", true}, {"quality", "quality", false}, {"x265_preserve", "x265_preserve", false}}

// CompressionPolicyToday freezes each goal's policy with the defaults a new
// job takes (media engine on for Save space, 1080p for Shrink, no size budget,
// not anime) and describes it.
func CompressionPolicyToday() (CompressionToday, error) {
	out := CompressionToday{Changes: compressionChanges}
	for _, g := range compressionGoals {
		var hardware *transcode.HardwareSearchRules
		if g.mode == "size" && !g.audioOnly {
			rules, err := modeHardwareInput("size", map[string]any{})
			if err != nil {
				return out, err
			}
			hardware = rules
		}
		snapshot, err := modePolicyGoal(g.mode, false, 0, hardware, false, nil, nil, g.audioOnly)
		if err != nil {
			return out, fmt.Errorf("%s: %w", g.goal, err)
		}
		out.Goals = append(out.Goals, describeCompression(g.goal, &snapshot))
	}
	return out, nil
}

func describeCompression(goal string, p *ModePolicySnapshot) CompressionGoal {
	d := CompressionGoal{Goal: goal, Version: strings.TrimPrefix(p.Version, "transcode-modes-"), Digest: strings.TrimPrefix(p.Digest, "sha256:")}
	if len(d.Digest) > 12 {
		d.Digest = d.Digest[:12]
	}
	switch {
	case p.Compact != nil:
		height := fmt.Sprintf("%dp", p.Compact.MaxHeight)
		d.Picture = fmt.Sprintf("HDR10, HLG and Dolby Vision 7/8 are tone-mapped to SDR (BT.709) and the picture is scaled to at most %s; Keep 4K converts to SDR at 4K. An SDR file that already fits is left as it is; Dolby Vision 5 is refused.", height)
	case p.Size != nil && p.Size.AudioOnly:
		d.Picture = "The video is copied and proven unchanged: 4K, HDR and Dolby Vision included."
	default:
		d.Picture = "Same resolution. HDR and Dolby Vision files are skipped: Shrink or Audio only handles them."
	}
	d.Video = describeRecipe(p.Recipe)
	if p.Size != nil && p.Size.EightBitMain10 {
		d.Video += " An 8-bit source may become 10-bit HEVC on a worker proven to keep its quality."
	}
	if vmaf := p.Profile.Optimization.Quality.VMAF; vmaf != nil && !(p.Size != nil && p.Size.AudioOnly) {
		d.Quality = fmt.Sprintf("Each encode is measured against the original on the whole file: VMAF at least %g on average", vmaf.Minimum)
		if vmaf.P5Minimum != nil {
			d.Quality += fmt.Sprintf(", %g at the 5th percentile", *vmaf.P5Minimum)
		}
		if vmaf.WorstWindowMinimum != nil && vmaf.WorstWindowSeconds > 0 {
			d.Quality += fmt.Sprintf(" and %g in the worst %g-second window", *vmaf.WorstWindowMinimum, vmaf.WorstWindowSeconds)
		}
		d.Quality += "."
	}
	d.Audio = describeAudio(p.Profile.Audio.Mode)
	if goal == "size" || goal == "compact" || goal == AudioOnlyGoal {
		d.Tracks = "Keeps the audio and subtitles in the languages to keep (Settings), plus the original language and untagged tracks; video, attachments and chapters are never removed."
	}
	switch {
	case p.Size != nil && p.Size.AudioOnly:
		d.Keeps = "Kept if it comes out smaller at all, then waits for your approval before replacing the original."
	case p.MinSavingsPercent > 0:
		d.Keeps = fmt.Sprintf("Kept only if it is at least %g%% smaller, then waits for your approval before replacing the original.", p.MinSavingsPercent)
	default:
		d.Keeps = "Kept if it does not grow, then waits for your approval before replacing the original."
	}
	return d
}

func describeRecipe(r *ModeRecipe) string {
	if r == nil {
		return ""
	}
	ladder := func(r *ModeRecipe) string {
		values := make([]string, len(r.QualityValues))
		for i, v := range r.QualityValues {
			values[i] = fmt.Sprint(v)
		}
		if r.VideoCodec == transcode.VideoCodecHEVCVideoToolbox {
			return "HEVC on the Apple media engine at quality " + strings.Join(values, ", then ")
		}
		return fmt.Sprintf("HEVC with x265 (%s preset) at CRF %s", r.Preset, strings.Join(values, ", then "))
	}
	switch r.Kind {
	case RecipeAudioOnly:
		return "The video is not re-encoded."
	case RecipeSizeBudget:
		return "The whole file is encoded to fit the size limit."
	}
	text := ladder(r) + ", until an encode passes the quality check; one that fails is encoded again at the next, better setting."
	if r.Fallback != nil {
		text += " A worker without the media engine uses " + ladder(r.Fallback) + "."
	}
	return text
}

func describeAudio(mode string) string {
	switch {
	case transcode.IsAudioLevelMode(mode):
		mono, stereo, surround, wide, num, den := transcode.AudioLevelTargetsKbps(transcode.IsHEAACAudioLevelMode(mode))
		codec := "AAC-LC"
		if transcode.IsHEAACAudioLevelMode(mode) {
			codec = "HE-AAC"
		}
		text := fmt.Sprintf("Lossless audio, and lossy audio at least %g times its target, becomes %s (mono %d, stereo %d, 5.1 %d, 7.1 %d kbps), keeping its channel layout.", float64(num)/float64(den), codec, mono, stereo, surround, wide)
		if transcode.IsObjectBedAudioLevelMode(mode) {
			text += " Atmos and DTS:X keep their channel bed and lose their objects."
		} else {
			text += " Lossy Atmos is copied."
		}
		return text + " Other audio is copied."
	case mode == transcode.AudioModeCompactLossless:
		return "Lossless audio becomes AAC; lossy audio is copied."
	case mode == "compact":
		mono, stereo, surround, wide, _, _ := transcode.AudioLevelTargetsKbps(false)
		return fmt.Sprintf("Lossless, DTS and PCM audio becomes AAC-LC (mono %d, stereo %d, 5.1 %d, 7.1 %d kbps), keeping its channel layout; TrueHD Atmos and DTS:X keep their channel bed and lose their objects. Other audio, lossy Atmos included, is copied.", mono, stereo, surround, wide)
	}
	return "Audio is copied as it is."
}
