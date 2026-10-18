package quality

import "fmt"

// The VMAF models in use were trained on content at or below 30 fps; their
// temporal (motion) features are out of distribution for 50/59.94/60 fps.
// High-frame-rate sources are therefore measured on a frame-exact 2:1
// decimation of both reference and candidate, which keeps the pairs aligned
// by frame index and presents the model with 25/29.97/30 fps motion.
const (
	// HFRMinFPS is the first nominal rate that needs decimation.
	HFRMinFPS = 45.0
	// HFRMaxFPS bounds the supported HFR class: 60 fps plus container rounding.
	// Anything faster has no calibrated decimation and stays unsupported.
	HFRMaxFPS = 61.0
	// HFRDecimationFactor keeps every second frame.
	HFRDecimationFactor = 2
)

// TemporalDecimation returns the VMAF frame decimation factor for a nominal
// source rate: 1 below HFRMinFPS, HFRDecimationFactor up to HFRMaxFPS, and an
// error for missing, non-finite or faster rates (fail closed).
func TemporalDecimation(fps float64) (int, error) {
	switch {
	case !finite(fps) || fps <= 0:
		return 0, fmt.Errorf("source FPS %.3f unavailable", fps)
	case fps < HFRMinFPS:
		return 1, nil
	case fps <= HFRMaxFPS:
		return HFRDecimationFactor, nil
	default:
		return 0, fmt.Errorf("source FPS %.3f exceeds the supported %.0f FPS class", fps, HFRMaxFPS)
	}
}

// IsHFR reports whether a rate needs decimation to be measured.
func IsHFR(fps float64) bool {
	factor, err := TemporalDecimation(fps)
	return err == nil && factor > 1
}

// DecimationFilter returns the filtergraph suffix that keeps frame indices
// 0, factor, 2*factor, ... of one input. select counts input frames, so both
// inputs drop exactly the same indices whatever their timestamps. The comma
// is escaped for -filter_complex, where an unescaped one separates filters.
func DecimationFilter(factor int) (string, error) {
	switch factor {
	case 1:
		return "", nil
	case HFRDecimationFactor:
		return fmt.Sprintf(",select=not(mod(n\\,%d))", factor), nil
	default:
		return "", fmt.Errorf("unsupported temporal decimation factor %d", factor)
	}
}

// MetricPairInputs returns the filtergraph prefix that prepares the candidate
// ([0:v]) and reference ([1:v]) of a two-input metric and ends with "[d][r]".
// Each input is converted to 10-bit measurement precision, decimated by frame
// index and then restamped by frame index. libvmaf's framesync pairs frames by
// timestamp; lossless extraction rounds both samples to a 1 ms timebase, so
// at 59.94 fps a source and candidate frame can land 1 ms apart and pair with
// a neighbour. Restamping both inputs identically pairs frame k with frame k.
func MetricPairInputs(decimation int) (string, error) {
	decimate, err := DecimationFilter(decimation)
	if err != nil {
		return "", err
	}
	chain := "format=yuv420p10le" + decimate + ",settb=AVTB,setpts=N"
	return "[0:v]" + chain + "[d];[1:v]" + chain + "[r];[d][r]", nil
}

// DecimatedFrameCount is the number of frames DecimationFilter keeps from n.
func DecimatedFrameCount(n, factor int) int {
	if n <= 0 || factor <= 0 {
		return 0
	}
	return (n + factor - 1) / factor
}

func validDecimation(factor int) bool { return factor == 1 || factor == HFRDecimationFactor }
