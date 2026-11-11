package transcodeworker

import (
	"slices"
	"testing"
)

func TestCanonicalParameterSetsAnnexBKeepsOnlyParameterSets(t *testing.T) {
	sc := []byte{0, 0, 0, 1}
	join := func(nals ...[]byte) []byte {
		var out []byte
		for _, n := range nals {
			out = append(out, sc...)
			out = append(out, n...)
		}
		return out
	}
	sps, pps, sei, aud := []byte{0x67, 1, 2, 3}, []byte{0x68, 4, 5}, []byte{0x06, 9, 9}, []byte{0x09, 0xf0}
	avcC := []byte{1, 1, 2, 3, 0xff, 0xe1, 0, byte(len(sps))}
	avcC = append(avcC, sps...)
	avcC = append(avcC, 1, 0, byte(len(pps)))
	avcC = append(avcC, pps...)
	want := canonicalParameterSets("h264", avcC)
	if want == "" {
		t.Fatal("avcC did not parse")
	}
	if got := canonicalParameterSets("h264", join(aud, sps, pps, sei)); got != want {
		t.Errorf("h264 Annex-B with SEI and AUD = %q, want %q", got, want)
	}
	if got := canonicalParameterSets("h264", join(sei, aud)); got != "" {
		t.Errorf("h264 Annex-B without parameter sets = %q, want empty", got)
	}

	vps, hsps, hpps, hsei := []byte{0x40, 1, 2}, []byte{0x42, 1, 3}, []byte{0x44, 1, 4}, []byte{0x4e, 1, 5}
	hvcC := make([]byte, 23)
	hvcC[0], hvcC[22] = 1, 3
	for _, set := range [][]byte{vps, hsps, hpps} {
		hvcC = append(hvcC, set[0]>>1&0x3f, 0, 1, 0, byte(len(set)))
		hvcC = append(hvcC, set...)
	}
	hwant := canonicalParameterSets("hevc", hvcC)
	if hwant == "" {
		t.Fatal("hvcC did not parse")
	}
	if got := canonicalParameterSets("hevc", join(vps, hsps, hpps, hsei)); got != hwant {
		t.Errorf("hevc Annex-B with SEI = %q, want %q", got, hwant)
	}
	// An hvcC record with an SEI array besides its parameter sets digests
	// the same: only VPS, SPS and PPS count.
	withSEI := slices.Clone(hvcC)
	withSEI[22] = 4
	withSEI = append(withSEI, hsei[0]>>1&0x3f, 0, 1, 0, byte(len(hsei)))
	withSEI = append(withSEI, hsei...)
	if got := canonicalParameterSets("hevc", withSEI); got != hwant {
		t.Errorf("hvcC with an SEI array = %q, want %q", got, hwant)
	}
}
