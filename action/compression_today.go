package action

import (
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
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
	d := CompressionGoal{Goal: goal, Version: p.Version, Digest: strings.TrimPrefix(p.Digest, "sha256:")}
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
	if p.Size != nil && p.Size.AudioOnlyFallback && !p.Size.AudioOnly {
		d.Video += fmt.Sprintf(" When no video encode passes, the video is copied and only the audio is converted, if that still saves %g%%.", p.MinSavingsPercent)
	}
	if o := p.Profile.Optimization; o != nil && o.Quality != nil && o.Quality.VMAF != nil && !(p.Size != nil && p.Size.AudioOnly) {
		d.Quality = describeQuality(p, o.Quality)
	}
	d.Audio = describeAudio(p.Profile.Audio.Mode)
	if goal == "size" || goal == "compact" || goal == AudioOnlyGoal {
		d.Tracks = "With languages to keep (Settings), keeps the audio and subtitles in them, plus the original language and untagged tracks; when no audio track matches, every audio track is kept. With an empty list every track is kept. Video, attachments and chapters are never removed."
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

// describeQuality is the final check each encode must pass: VMAF and banding
// on windows of the file, never the whole of it.
func describeQuality(p *ModePolicySnapshot, q *recipe.QualityPolicy) string {
	where := "five windows of up to 10 seconds spread across the file"
	if s := p.Size; s != nil && s.FinalWindowSeconds > 0 && s.FinalWindowEverySeconds > 0 {
		where = fmt.Sprintf("%d-second windows, one per %g minutes of the file (%d to %d)", s.FinalWindowSeconds, float64(s.FinalWindowEverySeconds)/60, s.FinalWindowsMinimum, s.FinalWindowsMaximum)
	}
	v := q.VMAF
	text := fmt.Sprintf("Each encode is compared with the original on %s: VMAF at least %g on average and in every window", where, v.Minimum)
	if v.P5Minimum != nil {
		text += fmt.Sprintf(", %g at the 5th percentile", *v.P5Minimum)
	}
	if v.WorstWindowMinimum != nil && v.WorstWindowSeconds > 0 {
		text += fmt.Sprintf(", %g in the worst %g seconds", *v.WorstWindowMinimum, v.WorstWindowSeconds)
	}
	if b := q.Banding; b != nil && b.Enabled && b.Enforcement == "reject" && b.MaxMean != nil && b.MaxPeak != nil {
		text += fmt.Sprintf("; banding (CAMBI) at most %g on average and %g at its peak", *b.MaxMean, *b.MaxPeak)
	}
	return text + "."
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
	text := ladder(r) + ", until an encode passes the quality check; one that fails is encoded again at the next, better setting. The ladder stops early when an encode misses the saving, since a better setting is only larger, or when the scores show the next setting would still miss."
	if r.Fallback != nil {
		text += " A worker without the media engine uses " + ladder(r.Fallback) + "; when every media engine setting misses, x265 is offered as a next step."
	}
	return text
}

func describeAudio(mode string) string {
	switch {
	case transcode.IsAudioLevelMode(mode):
		mono, stereo, surround, wide, num, den := transcode.AudioLevelTargetsKbps(false)
		targets := fmt.Sprintf("AAC-LC (mono %d, stereo %d, 5.1 %d, 7.1 %d kbps)", mono, stereo, surround, wide)
		if transcode.IsHEAACAudioLevelMode(mode) {
			_, heStereo, heSurround, _, _, _ := transcode.AudioLevelTargetsKbps(true)
			targets = fmt.Sprintf("HE-AAC for stereo, quad and 5.1 (stereo %d, 5.1 %d kbps), else %s", heStereo, heSurround, targets)
		}
		text := fmt.Sprintf("A track up to 7.1, in a layout AAC keeps, becomes %s when it is at least %g times that target; a lossless track that states no bitrate is converted too.", targets, float64(num)/float64(den))
		if transcode.IsObjectBedAudioLevelMode(mode) {
			text += " A converted Atmos or DTS:X track keeps its channel bed and loses its objects."
		} else {
			text += " Lossy Atmos is copied."
		}
		return text + " Every other track, a lossy one of unknown bitrate included, is copied with its objects."
	case mode == transcode.AudioModeCompactLossless:
		return "Lossless audio becomes AAC; lossy audio is copied."
	case mode == "compact":
		mono, stereo, surround, wide, _, _ := transcode.AudioLevelTargetsKbps(false)
		return fmt.Sprintf("Lossless, DTS and PCM audio up to 7.1, in a layout AAC keeps, becomes AAC-LC (mono %d, stereo %d, 5.1 %d, 7.1 %d kbps); TrueHD Atmos and DTS:X keep their channel bed and lose their objects. Other audio, lossy Atmos included, is copied.", mono, stereo, surround, wide)
	}
	return "Audio is copied as it is."
}
