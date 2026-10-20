package transcode

import (
	"fmt"
	"math"
)

// X265Settings is an explicit, bounded ABR experiment. Nil preserves the
// established CRF path and its serialized identity. Stats paths and arbitrary
// encoder arguments are deliberately absent from the public recipe.
type X265Settings struct {
	BitrateKbps int     `json:"bitrate_kbps" yaml:"bitrate_kbps"`
	TwoPass     bool    `json:"two_pass" yaml:"two_pass"`
	Lookahead   int     `json:"lookahead" yaml:"lookahead"`
	BFrames     int     `json:"b_frames" yaml:"b_frames"`
	RefFrames   int     `json:"ref_frames" yaml:"ref_frames"`
	AQMode      int     `json:"aq_mode" yaml:"aq_mode"`
	AQStrength  float64 `json:"aq_strength" yaml:"aq_strength"`
	PsyRD       float64 `json:"psy_rd" yaml:"psy_rd"`
	PsyRDOQ     float64 `json:"psy_rdoq" yaml:"psy_rdoq"`
	SSIMRD      bool    `json:"ssim_rd" yaml:"ssim_rd"`
}

func (x *X265Settings) Clone() *X265Settings {
	if x == nil {
		return nil
	}
	cp := *x
	return &cp
}

// The bounds of a two-pass x265 average bitrate.
const (
	X265MinBitrateKbps = 64
	X265MaxBitrateKbps = 50000
)

func (x *X265Settings) Validate(codec string, quality int) error {
	if x == nil {
		return nil
	}
	if NormalizeVideoCodec(codec) != VideoCodecLibX265 || quality != 0 {
		return fmt.Errorf("x265 ABR settings require libx265 with quality=0")
	}
	if !x.TwoPass || x.BitrateKbps < X265MinBitrateKbps || x.BitrateKbps > X265MaxBitrateKbps || x.Lookahead < 5 || x.Lookahead > 250 || x.BFrames < 0 || x.BFrames > 16 || x.RefFrames < 1 || x.RefFrames > 16 || x.AQMode < 0 || x.AQMode > 4 {
		return fmt.Errorf("invalid bounded two-pass x265 settings")
	}
	for name, v := range map[string]float64{"aq_strength": x.AQStrength, "psy_rd": x.PsyRD, "psy_rdoq": x.PsyRDOQ} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 3 {
			return fmt.Errorf("invalid x265 %s", name)
		}
	}
	return nil
}
