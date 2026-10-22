package action

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// A size budget's audio is a level, chosen once per source from the frozen
// levels (transcode.AudioLevelModes, least compressed first) by one rule:
//
//   - the first level that leaves the video at least a "good" bitrate wins;
//   - when none does, the most compressed level that leaves a usable video
//     bitrate wins (the picture is then as good as the budget allows);
//   - when none leaves a usable video bitrate, the budget is unreachable, and
//     the "at least this much" figure is the most compressed level's.
//
// Level 0 (compact_lossless: only lossless audio converted) is what budgets
// frozen before the levels carry. They keep it, and never get a level.
//
// The level is a function of the frozen policy, the source and the budget, so
// validation chooses it again and compares it with the plan and the record.

// sizeBudgetGoodBitsPerPixel is the video a size budget asks for before it
// compresses audio further: bits per pixel per frame, so 1080p at 23.976 fps
// asks for about 2.0 Mbps. It is an engineering figure, not a measured quality
// gate: x265 two-pass at that rate on 1080p television content lands well
// above the budget quality floor (VMAF mean 70, p5 60), and below it the floor
// is what decides. It is one number so the rule has one knob; the final
// validation windows still judge every encode.
const sizeBudgetGoodBitsPerPixel = 0.04

// SizeBudgetAudio is the audio policy a size budget freezes with policy v5.
type SizeBudgetAudio struct {
	// Modes are the audio levels the plan may use, least compressed first.
	Modes []string `json:"modes"`
	// GoodBitsPerPixel turns a source's size and frame rate into the video
	// bitrate that makes a level good (see sizeBudgetGoodBitsPerPixel).
	GoodBitsPerPixel float64 `json:"good_bits_per_pixel"`
}

func newSizeBudgetAudio() *SizeBudgetAudio {
	return &SizeBudgetAudio{Modes: slices.Clone(transcode.AudioLevelModes), GoodBitsPerPixel: sizeBudgetGoodBitsPerPixel}
}

func (a *SizeBudgetAudio) validate() error {
	if a == nil {
		return nil
	}
	if !slices.Equal(a.Modes, transcode.AudioLevelModes) || !(a.GoodBitsPerPixel > 0 && a.GoodBitsPerPixel <= 1) {
		return fmt.Errorf("invalid size budget audio levels")
	}
	return nil
}

// audioModes are the levels a budget may plan with: its frozen levels, or the
// one audio policy snapshots frozen before the levels carry.
func (b *SizeBudgetPolicy) audioModes() []string {
	if b == nil || b.Audio == nil {
		return []string{transcode.AudioModeCompactLossless}
	}
	return b.Audio.Modes
}

// goodVideoKbps is the video bitrate that makes a level good for this source,
// never more than the source's own video bitrate (a source that was never
// better than that gains nothing from more). Zero when the source's size or
// frame rate is unknown, which accepts the first level.
func (a *SizeBudgetAudio) goodVideoKbps(rep *mediainspect.DetailedReport, sourceBytes int64) int {
	if a == nil || rep == nil {
		return 0
	}
	video := rep.MainVideo()
	if len(video) != 1 || video[0].Width <= 0 || video[0].Height <= 0 || video[0].FPS <= 0 {
		return 0
	}
	good := int(math.Round(a.GoodBitsPerPixel * float64(video[0].Width) * float64(video[0].Height) * video[0].FPS / 1000))
	if n := audioStreamBytes(video[0], rep.DurationSec, sourceBytes); n > 0 && rep.DurationSec > 0 {
		good = min(good, int(float64(n)*8/rep.DurationSec/1000))
	}
	return good
}

// SizeBudgetAudioTrack is one audio track's plan under the chosen level.
type SizeBudgetAudioTrack struct {
	Index      int    `json:"index"`
	Codec      string `json:"codec"`
	SourceKbps int    `json:"source_kbps,omitempty"`
	// Output is copy, aac (AAC-LC) or he_aac.
	Output string `json:"output"`
	// Kbps is a converted track's bitrate.
	Kbps int `json:"kbps,omitempty"`
}

// SizeBudgetAudioRecord is the audio level a budget planned and what it does
// to each track, frozen with the plan so validation can check both.
type SizeBudgetAudioRecord struct {
	Mode  string `json:"mode"`
	Level int    `json:"level"`
	// GoodVideoKbps is the video bitrate this source's level had to leave.
	GoodVideoKbps int `json:"good_video_kbps"`
	// PreviousVideoKbps is what the level before this one left, when it was
	// tried and fell short.
	PreviousVideoKbps int                    `json:"previous_video_kbps,omitempty"`
	Tracks            []SizeBudgetAudioTrack `json:"tracks"`
}

// converted counts the tracks the level re-encodes.
func (r *SizeBudgetAudioRecord) converted() int {
	n := 0
	for _, t := range r.Tracks {
		if t.Output != "copy" {
			n++
		}
	}
	return n
}

// sizeBudgetAudioRecord is what a level does to a source's audio tracks.
func sizeBudgetAudioRecord(rep *mediainspect.DetailedReport, mode string, level, good, previous int) *SizeBudgetAudioRecord {
	r := &SizeBudgetAudioRecord{Mode: mode, Level: level, GoodVideoKbps: good, PreviousVideoKbps: previous, Tracks: []SizeBudgetAudioTrack{}}
	for i, a := range rep.Audio {
		track := audioTrackOf(a, rep.DurationSec)
		out := transcode.AudioOutputFor(mode, track)
		t := SizeBudgetAudioTrack{Index: i, Codec: a.Codec, SourceKbps: track.BitrateKbps, Output: "copy"}
		if out.Codec != "copy" {
			t.Output, t.Kbps = "aac", out.Kbps
			if out.HEAAC {
				t.Output = "he_aac"
			}
		}
		r.Tracks = append(r.Tracks, t)
	}
	return r
}

// audioTrackOf is a source audio stream as the audio policy reads it, the same
// way the worker reads it from its own probe.
func audioTrackOf(a mediainspect.DetailedStream, durationSec float64) transcode.AudioTrack {
	return transcode.DetailedAudioTrackAt(a.Codec, a.Profile, a.Title, a.Tags, a.Channels, a.ChannelLayout, a.BitRate, durationSec)
}

// sizeBudgetAudioSentence says in words what the budget does to the audio.
func sizeBudgetAudioSentence(r *SizeBudgetAudioRecord) string {
	if r == nil {
		return ""
	}
	if r.converted() == 0 {
		return fmt.Sprintf(" Audio level %d of %d: all %d tracks are already within 25%% of their targets and are copied.", r.Level, len(transcode.AudioLevelModes), len(r.Tracks))
	}
	what := "AAC-LC (stereo 192 kbps, 5.1 384 kbps)"
	if r.Mode == transcode.AudioModeSizeHEAAC {
		what = "HE-AAC (stereo 64 kbps, 5.1 160 kbps)"
	}
	why := ""
	if r.PreviousVideoKbps > 0 {
		why = fmt.Sprintf(" Level %d left only %d kbps of video, below the %d kbps this picture needs.", r.Level-1, r.PreviousVideoKbps, r.GoodVideoKbps)
	}
	return fmt.Sprintf(" Audio level %d of %d: %d of %d tracks are re-encoded as %s; the rest, subtitles and attachments are untouched.%s", r.Level, len(transcode.AudioLevelModes), r.converted(), len(r.Tracks), what, why)
}

// sizeBudgetAudioOnlyNote says why a budget copies the video.
func sizeBudgetAudioOnlyNote(plan *transcode.Plan) string {
	switch plan.AudioMode {
	case transcode.AudioModeSizeAAC:
		return "Copying the video and re-encoding the audio as AAC-LC alone fits the budget, so the video is copied unchanged."
	case transcode.AudioModeSizeHEAAC:
		return "Copying the video and re-encoding the audio as HE-AAC alone fits the budget, so the video is copied unchanged."
	}
	return "Copying the video and compacting the lossless audio alone fits the budget, so the video is copied unchanged and only the lossless audio is compacted."
}

func sizeBudgetAudioRecordJSON(r *SizeBudgetAudioRecord) string {
	b, _ := json.Marshal(r)
	return string(b)
}

// sizeAudioOnlyLevelMatches reports whether an audio-only plan carries the level
// the frozen policy would choose for this source and budget. A budget without
// levels (frozen before them) has the one legacy level, which the worker's plan
// check already pins.
func sizeAudioOnlyLevelMatches(snapshot *ModePolicySnapshot, state map[string]any, plan *transcode.Plan) bool {
	if snapshot.Size == nil || snapshot.Size.SizeBudget == nil || snapshot.Size.SizeBudget.Audio == nil {
		return true
	}
	policy := snapshot.Size.SizeBudget
	original := getInt64(state, "original_size")
	mode, _, ok := sizeBudgetAudioOnlyLevel(policy, getSourceReport(state["source_report"]), original, policy.BudgetBytes(original, siblingEvidenceOf(state)))
	return ok && mode == plan.AudioMode
}

// planWithAudioMode is plan with another audio mode and its digest.
func planWithAudioMode(plan *transcode.Plan, mode string) (*transcode.Plan, error) {
	if plan == nil {
		return nil, fmt.Errorf("the frozen size plan is missing")
	}
	p := *plan
	p.AudioMode = mode
	p.PlanDigest = ""
	d, err := transcode.DigestPlan(&p)
	if err != nil {
		return nil, err
	}
	p.PlanDigest = d
	return &p, nil
}

// sizeBudgetAudioOnlyLevel is the least compressed level at which copying the
// video and converting audio alone fits the budget, with its estimate.
func sizeBudgetAudioOnlyLevel(policy *SizeBudgetPolicy, rep *mediainspect.DetailedReport, sourceBytes, budget int64) (string, audioOnlyEstimate, bool) {
	for _, mode := range policy.audioModes() {
		if est := estimateAudioOnly(rep, sourceBytes, mode); est.Compacted > 0 && est.CandidateBytes <= budget {
			return mode, est, true
		}
	}
	return "", audioOnlyEstimate{}, false
}
