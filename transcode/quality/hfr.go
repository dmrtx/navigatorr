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
// 0, factor, 2*factor, ... of one input. It must follow PTS normalization on
// both inputs so source and candidate drop exactly the same frames. The comma
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

// DecimatedFrameCount is the number of frames DecimationFilter keeps from n.
func DecimatedFrameCount(n, factor int) int {
	if n <= 0 || factor <= 0 {
		return 0
	}
	return (n + factor - 1) / factor
}

func validDecimation(factor int) bool { return factor == 1 || factor == HFRDecimationFactor }
