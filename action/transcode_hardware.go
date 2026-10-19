package action

import (
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// hardwareCandidatePrefix marks the VideoToolbox candidates of a hardware
// search. They are measured with the x265 initial candidates, not as a
// fallback family. The higher-quality rungs of a ladder that failed quality
// are a fallback family of their own (transcode.HardwareFallbackCandidatePrefix).
const hardwareCandidatePrefix = "vt-"

// addHardwareSearch adds a size search's VideoToolbox candidates when the
// frozen policy allows the media engine. The allowance is a permission, not a
// requirement: a search that cannot use it (an older worker, no
// VideoToolbox encoder or the proven metrics for it, or a source too short for
// the automatic search) stays x265 only, exactly as without the option.
//
// Every candidate carries the frozen keyframe interval: left to itself the
// encoder uses 12 frames, which makes the files about three times larger and
// is why VideoToolbox candidates used to lose every size contest.
func addHardwareSearch(req *transcode.BenchmarkRequest, policy *ModePolicySnapshot, plan *transcode.Plan, caps transcode.WorkerCapabilities, metric string, bitDepth, qualityBitDepth int) error {
	if policy == nil || policy.Size == nil || policy.Size.Hardware == nil || policy.Size.Hardware.Validate() != nil {
		return nil
	}
	rules := policy.Size.Hardware
	if !req.Automatic || req.AutomaticSearch == nil || !caps.HardwareSearch || plan.VideoCodec != transcode.VideoCodecLibX265 {
		return nil
	}
	if validateWorkerCapabilitiesForBenchmark(caps, metric, qualityBitDepth, transcode.VideoCodecHEVCVideoToolbox) != nil {
		return nil
	}
	vt := *plan
	vt.VideoCodec = transcode.VideoCodecHEVCVideoToolbox
	vt.Preset, vt.Tune, vt.X265 = "", "", nil
	vt.Quality = rules.QualityValues[len(rules.QualityValues)-1]
	vt.PixelFormat = "yuv420p"
	if outputBitDepth := max(bitDepth, plan.ExpectedBitDepth); outputBitDepth == 10 {
		vt.PixelFormat = "p010le"
	}
	gop, bframes := rules.GOPSize, rules.BFrames
	vt.GOPSize, vt.BFrames = &gop, &bframes
	hardware, err := buildBenchmarkCandidates(&vt, bitDepth, &recipe.SearchPolicy{MaxCandidates: len(rules.QualityValues), QualityValues: rules.QualityValues})
	if err != nil {
		return err
	}
	for i := range hardware {
		hardware[i].ID = hardwareCandidatePrefix + hardware[i].ID
	}
	req.HardwareSearch = rules.Clone()
	// The higher-quality rungs are a fallback family the worker measures only
	// when the whole ladder fails quality. A worker that does not know them
	// would reject the field and the longer candidate list, so it gets the
	// ladder alone, exactly as before the fallback existed.
	if len(rules.FallbackQualityValues) > 0 && caps.HardwareFallbackSearch {
		vt.Quality = rules.FallbackQualityValues[len(rules.FallbackQualityValues)-1]
		fallback, err := buildBenchmarkCandidates(&vt, bitDepth, &recipe.SearchPolicy{MaxCandidates: len(rules.FallbackQualityValues), QualityValues: rules.FallbackQualityValues})
		if err != nil {
			return err
		}
		for i := range fallback {
			fallback[i].ID = transcode.HardwareFallbackCandidatePrefix + fallback[i].ID
		}
		hardware = append(hardware, fallback...)
	} else {
		req.HardwareSearch.FallbackQualityValues = nil
	}
	req.Candidates = append(req.Candidates, hardware...)
	return nil
}
