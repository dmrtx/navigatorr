package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

// HDR policies for mode operations. Rejecting (the default) keeps every HDR
// and Dolby Vision source untouched; preserve_hdr10 converts HDR10 sources
// (and Dolby Vision with an HDR10 base layer after per-file approval) while
// keeping HDR10 signalling and static metadata.
const (
	HDRPolicyReject        = "reject"
	HDRPolicyPreserveHDR10 = "preserve_hdr10"
)

// DecisionApproveDolbyVisionRemoval is the only decision that authorizes
// dropping Dolby Vision from one file. It is never a generic "approve" and is
// never issued automatically.
const DecisionApproveDolbyVisionRemoval = "approve_dolby_vision_removal"

const waitingDolbyVisionRemoval = "dolby_vision_removal_approval"

// ModeHDR10Quality is the stricter quality bar applied to HDR10 sources. VMAF
// and CAMBI are measured on both sides after the identical deterministic tone
// map (transcode.HDRToneMapMethod); that rendition compresses highlights, so
// artifacts there are attenuated and the thresholds are raised to compensate.
type ModeHDR10Quality struct {
	ToneMapMethod          string  `json:"tone_map_method"`
	VMAFTarget             float64 `json:"vmaf_target"`
	VMAFMinimum            float64 `json:"vmaf_minimum"`
	VMAFP5Minimum          float64 `json:"vmaf_p5_minimum"`
	VMAFWorstWindowMinimum float64 `json:"vmaf_worst_window_minimum"`
	CAMBIMaxMean           float64 `json:"cambi_max_mean"`
	CAMBIMaxPeak           float64 `json:"cambi_max_peak"`
}

func hdr10ModeQuality(mode string) *ModeHDR10Quality {
	q := &ModeHDR10Quality{ToneMapMethod: transcode.HDRToneMapMethod, VMAFTarget: 97, VMAFMinimum: 96, VMAFP5Minimum: 94, VMAFWorstWindowMinimum: 93, CAMBIMaxMean: 4, CAMBIMaxPeak: 12}
	if mode == "size" {
		q.VMAFTarget, q.VMAFMinimum, q.VMAFP5Minimum, q.VMAFWorstWindowMinimum = 94, 92, 89, 89
	}
	return q
}

// modeHDRPolicy returns the normalized hdr_policy input: "" for the default
// rejection, or HDRPolicyPreserveHDR10.
func modeHDRPolicy(inputs map[string]any) (string, error) {
	raw, present := inputs["hdr_policy"]
	if !present {
		return "", nil
	}
	policy, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("hdr_policy must be a string")
	}
	switch policy {
	case "", HDRPolicyReject:
		return "", nil
	case HDRPolicyPreserveHDR10:
		return policy, nil
	}
	return "", fmt.Errorf("hdr_policy must be %s or %s", HDRPolicyReject, HDRPolicyPreserveHDR10)
}

// applyHDR10ModeQuality tightens a mode profile to the HDR10 quality bar. It
// only ever raises VMAF floors and lowers CAMBI ceilings.
func applyHDR10ModeQuality(p *recipe.Profile, q *ModeHDR10Quality) error {
	if q == nil || p.Optimization == nil || p.Optimization.Quality == nil || p.Optimization.Quality.VMAF == nil || p.Optimization.Quality.VMAF.Model == "" || p.Optimization.Quality.Banding == nil || !p.Optimization.Quality.Banding.Enabled {
		return fmt.Errorf("HDR10 mode requires a frozen VMAF model and CAMBI policy")
	}
	opt := *p.Optimization
	quality := *opt.Quality
	vmaf := *quality.VMAF
	banding := *quality.Banding
	raise := func(cur *float64, floor float64) *float64 {
		v := floor
		if cur != nil && *cur > v {
			v = *cur
		}
		return &v
	}
	lower := func(cur *float64, ceiling float64) *float64 {
		v := ceiling
		if cur != nil && *cur < v {
			v = *cur
		}
		return &v
	}
	vmaf.Target = *raise(&vmaf.Target, q.VMAFTarget)
	vmaf.Minimum = *raise(&vmaf.Minimum, q.VMAFMinimum)
	vmaf.P5Minimum = raise(vmaf.P5Minimum, q.VMAFP5Minimum)
	vmaf.WorstWindowMinimum = raise(vmaf.WorstWindowMinimum, q.VMAFWorstWindowMinimum)
	vmaf.GuardrailEnforcement = "reject"
	banding.MaxMean = lower(banding.MaxMean, q.CAMBIMaxMean)
	banding.MaxPeak = lower(banding.MaxPeak, q.CAMBIMaxPeak)
	banding.Enforcement = "reject"
	quality.VMAF, quality.Banding = &vmaf, &banding
	// SSIM has no HDR calibration; HDR10 is judged by tone-mapped VMAF/CAMBI.
	quality.SSIM = nil
	opt.Quality = &quality
	p.Optimization = &opt
	p.Video.Codec = transcode.VideoCodecLibX265
	return nil
}

// classifySourceDynamicRange classifies the source. Frame evidence is only
// decoded when the stream signals HDR; a failed frame probe leaves the source
// unconvertible (hdr_frame_evidence_missing) rather than failing the action.
func (e *Engine) classifySourceDynamicRange(ctx context.Context, path string, rep *mediainspect.DetailedReport) mediainspect.DynamicRange {
	d := mediainspect.ClassifyDynamicRange(rep, nil)
	if !d.IsHDR() || e.deps.Ffprobe == "" {
		return d
	}
	frames, err := mediainspect.ProbeFrameSignals(ctx, e.deps.Ffprobe, path, mediainspect.FrameSignalSampleFrames)
	if err != nil {
		return d
	}
	return mediainspect.ClassifyDynamicRange(rep, &frames)
}

type dolbyVisionRemovalApproval struct {
	SourceSHA256      string `json:"source_sha256"`
	Profile           int    `json:"profile"`
	BLCompatibilityID int    `json:"bl_compatibility_id"`
	ApprovedAt        string `json:"approved_at"`
}

// dolbyVisionRemovalApproved reports a persisted approval bound to this exact
// source content and Dolby Vision configuration.
func dolbyVisionRemovalApproved(state map[string]any, sourceSHA string, dv *mediainspect.DolbyVisionInfo) bool {
	raw, ok := state["dolby_vision_removal_approval"]
	if !ok || dv == nil || sourceSHA == "" {
		return false
	}
	var a dolbyVisionRemovalApproval
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, &a) != nil {
		return false
	}
	return a.SourceSHA256 == sourceSHA && a.Profile == dv.Profile && a.BLCompatibilityID == dv.BLCompatibilityID
}

func dolbyVisionRemovalPrompt(path string, d mediainspect.DynamicRange) string {
	return fmt.Sprintf("%s is %s. Converting it keeps HDR10 (static metadata) but permanently removes Dolby Vision dynamic metadata from the new file. The original is never modified.", filepath.Base(path), d.Label)
}

// resolveHDR10Plan derives the HDR10 plan for a convertible HDR source. A
// Dolby Vision source waits for the explicit per-file approval decision;
// result is non-nil whenever the step must stop here.
func (e *Engine) resolveHDR10Plan(ec *ExecutionContext, d mediainspect.DynamicRange, sourceSHA, path string, outputs map[string]any) (*transcode.HDR10Plan, *StepResult) {
	approved := false
	if d.RequiresDolbyVisionRemoval() {
		approved = dolbyVisionRemovalApproved(ec.State, sourceSHA, d.DolbyVision)
		if !approved {
			switch ec.Decision {
			case DecisionApproveDolbyVisionRemoval:
				ec.State["dolby_vision_removal_approval"] = dolbyVisionRemovalApproval{SourceSHA256: sourceSHA, Profile: d.DolbyVision.Profile, BLCompatibilityID: d.DolbyVision.BLCompatibilityID, ApprovedAt: e.now().UTC().Format(time.RFC3339Nano)}
				ec.Decision = ""
				approved = true
			case "cancel", "reject":
				ec.State["reason_code"] = "user_cancelled"
				return nil, &StepResult{Status: StepCancelled, Outputs: map[string]any{"reason_code": "user_cancelled", "dynamic_range": d}}
			default:
				ec.State["reason_code"] = string(transcode.ErrDolbyVisionRemovalApprovalRequired.Error())
				out := map[string]any{"reason_code": ec.State["reason_code"], "dynamic_range": d}
				for k, v := range outputs {
					out[k] = v
				}
				return nil, &StepResult{
					Status:           StepWaitingDecision,
					WaitingCondition: waitingDolbyVisionRemoval,
					WaitingReason:    dolbyVisionRemovalPrompt(path, d),
					WaitingOptions: []WaitingOption{
						{Decision: DecisionApproveDolbyVisionRemoval, Description: "Remove Dolby Vision from this file and keep HDR10"},
						{Decision: "cancel", Description: "Keep the original untouched"},
					},
					Outputs: out,
				}
			}
		}
	}
	plan, err := transcode.NewHDR10Plan(d, approved)
	if err != nil {
		if errors.Is(err, transcode.ErrDolbyVisionRemovalApprovalRequired) {
			return nil, &StepResult{Status: StepFailed, Error: "Dolby Vision removal approval is missing (fail closed)"}
		}
		ec.State["skip_transcode"] = true
		ec.State["reason_code"] = "unsupported_source"
		out := map[string]any{"skipped": true, "reason_code": "unsupported_source", "hdr_reason": err.Error(), "dynamic_range": d}
		for k, v := range outputs {
			out[k] = v
		}
		return nil, &StepResult{Status: StepCompleted, Outputs: out}
	}
	return plan, nil
}
