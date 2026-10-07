// Package acoustic recognizes complete, previously reviewed recordings. It
// makes no semantic decisions. Spectral hashes only select candidates; every
// accepted interval is independently verified against the entire waveform.
package acoustic

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sort"
)

const Rate = 8000
const hop = 128
const frameSize = 256
const MaxSamples = Rate * 43200

type Match struct {
	StartSample    int64
	MinCorrelation float64
}

// SameRecording is deliberately stricter than partial overlap. It is used for
// library deduplication and tombstones, never to infer a semantic category.
func SameRecording(ctx context.Context, a, b []byte) (bool, error) {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(a) < Rate*8*2 || len(b)-len(a) > Rate*2*2 {
		return false, nil
	}
	samples := make([]int16, len(a)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(a[i*2:]))
	}
	p, err := Prepare(ctx, bytes.NewReader(b), int64(len(b)/2))
	if err != nil {
		return false, err
	}
	matches, err := p.Find(ctx, samples)
	return len(matches) > 0, err
}

// PCM lives on private disk. Only compact spectral hashes and bounded seed /
// candidate buffers are resident, even for a twelve-hour source.
type PCM struct {
	File    io.ReaderAt
	Samples int64
	hashes  []uint32
	index   map[uint32][]int
}

func ReadSamples(r io.ReaderAt, start int64, n int) ([]int16, error) {
	if start < 0 || n < 0 || n > Rate*180 {
		return nil, fmt.Errorf("PCM read out of bounds")
	}
	raw := make([]byte, n*2)
	_, err := r.ReadAt(raw, start*2)
	if err != nil {
		return nil, err
	}
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return out, nil
}
func Prepare(ctx context.Context, r io.ReaderAt, samples int64) (*PCM, error) {
	if samples < frameSize || samples > MaxSamples {
		return nil, fmt.Errorf("invalid PCM duration")
	}
	p := &PCM{File: r, Samples: samples, index: map[uint32][]int{}}
	raw := make([]byte, frameSize*2)
	for pos := int64(0); pos+frameSize <= samples; pos += hop {
		if pos%(hop*256) == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if _, err := r.ReadAt(raw, pos*2); err != nil {
			return nil, err
		}
		h := signature(raw)
		p.hashes = append(p.hashes, h)
		i := len(p.hashes) - 1
		{
			for part := uint32(0); part < 3; part++ {
				key := part<<12 | (h>>(part*10))&4095
				bucket := p.index[key]
				if len(bucket) < 513 {
					p.index[key] = append(bucket, i)
				}
			}
		}
	}
	return p, nil
}

var edges = [...]int{1, 2, 3, 4, 6, 8, 11, 15, 20, 27, 36, 48, 64, 85, 106, 118, 128}

func signature(raw []byte) uint32 {
	var f [frameSize]complex128
	for i := range f {
		sample := float64(int16(binary.LittleEndian.Uint16(raw[i*2:])))
		f[i] = complex(sample*(.5-.5*math.Cos(2*math.Pi*float64(i)/float64(frameSize-1))), 0)
	}
	for i, j := 1, 0; i < frameSize; i++ {
		bit := frameSize >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			f[i], f[j] = f[j], f[i]
		}
	}
	for size := 2; size <= frameSize; size *= 2 {
		angle := -2 * math.Pi / float64(size)
		w := complex(math.Cos(angle), math.Sin(angle))
		for base := 0; base < frameSize; base += size {
			z := complex(1, 0)
			for j := 0; j < size/2; j++ {
				a, b := f[base+j], z*f[base+j+size/2]
				f[base+j], f[base+j+size/2] = a+b, a-b
				z *= w
			}
		}
	}
	var bands [16]float64
	avg := 0.0
	for i := range bands {
		e := 0.0
		for k := edges[i]; k < edges[i+1]; k++ {
			e += real(f[k])*real(f[k]) + imag(f[k])*imag(f[k])
		}
		bands[i] = math.Log1p(e / float64(edges[i+1]-edges[i]))
		avg += bands[i] / 16
	}
	var h uint32
	for i, v := range bands {
		if v > avg {
			h |= 1 << i
		}
		if i < 15 && v > bands[i+1] {
			h |= 1 << (i + 16)
		}
	}
	return h
}

type vote struct {
	count    int
	quarters uint8
}

func (p *PCM) Find(ctx context.Context, ref []int16) ([]Match, error) {
	if len(ref) < Rate*8 || len(ref) > Rate*180 || !Informative(ref) {
		return nil, nil
	}
	rh := hashes(ref)
	votes := map[int]vote{}
	for i := 0; i < len(rh); i += 16 {
		for part := uint32(0); part < 3; part++ {
			key := part<<12 | (rh[i]>>(part*10))&4095
			bucket := p.index[key]
			if len(bucket) > 512 {
				continue
			}
			for _, at := range bucket {
				off := at - i
				if off < -4 || int64(off*hop+len(ref)) > p.Samples+4*hop {
					continue
				}
				off = (off + 2) / 4 * 4
				v := votes[off]
				v.count++
				v.quarters |= 1 << min(3, i*4/len(rh))
				votes[off] = v
			}
		}
	}
	type candidate struct{ off, count int }
	var candidates []candidate
	for off, v := range votes {
		if v.count >= max(10, len(rh)/16/5) && v.quarters == 15 {
			candidates = append(candidates, candidate{off, v.count})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].count > candidates[j].count })
	if len(candidates) > 32 {
		candidates = candidates[:32]
	}
	var out []Match
	checked := []int64{}
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		at := int64(c.off * hop)
		skip := false
		for _, v := range checked {
			if abs(at-v) < Rate/8 {
				skip = true
			}
		}
		if skip {
			continue
		}
		checked = append(checked, at)
		aligned, score, err := p.verify(ctx, ref, at)
		if err != nil {
			return nil, err
		}
		if score >= .94 {
			out = append(out, Match{aligned, score})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartSample < out[j].StartSample })
	return out, nil
}
func hashes(ref []int16) []uint32 {
	raw := make([]byte, len(ref)*2)
	for i, v := range ref {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(v))
	}
	var out []uint32
	for pos := 0; pos+frameSize <= len(ref); pos += hop {
		out = append(out, signature(raw[pos*2:]))
	}
	return out
}
func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func corr(a, b []int16, stride int) float64 {
	var ab, aa, bb, ma, mb float64
	n := 0
	for i := 0; i < len(a); i += stride {
		x, y := float64(a[i]), float64(b[i])
		ma += x
		mb += y
		aa += x * x
		bb += y * y
		ab += x * y
		n++
	}
	aa -= ma * ma / float64(n)
	bb -= mb * mb / float64(n)
	ab -= ma * mb / float64(n)
	if aa <= float64(n)*4 || bb <= float64(n)*4 {
		return -1
	}
	return ab / math.Sqrt(aa*bb)
}

// Reject silence/tones and highly repetitive beds before indexing. Native
// transcript seed validation separately requires multiple spoken units.
func Informative(ref []int16) bool {
	if len(ref) < Rate*8 {
		return false
	}
	hs := hashes(ref)
	seen := map[uint32]bool{}
	energetic := 0
	for i := 0; i < len(ref); i += Rate {
		end := min(i+Rate, len(ref))
		var e float64
		for _, v := range ref[i:end] {
			e += float64(v) * float64(v)
		}
		if e/float64(end-i) > 10000 {
			energetic++
		}
	}
	for _, h := range hs {
		seen[h] = true
	}
	return len(seen) > 40 && energetic >= len(ref)/Rate*3/4
}
func (p *PCM) verify(ctx context.Context, ref []int16, at int64) (int64, float64, error) {
	// Locate the sample alignment using three distributed half-second probes,
	// then verify EVERY second including both edges with the same alignment.
	const radius = hop * 4
	first := max(int64(0), at-radius)
	last := min(p.Samples-int64(len(ref)), at+radius)
	if last < first {
		return 0, 0, nil
	}
	probeOffsets := []int{Rate / 4, len(ref) / 2, len(ref) - Rate}
	probeLen := Rate / 2
	activeProbes := []int{}
	for _, off := range probeOffsets {
		if corr(ref[off:off+probeLen], ref[off:off+probeLen], 1) > .99 {
			activeProbes = append(activeProbes, off)
		}
	}
	if len(activeProbes) < 2 {
		return 0, 0, nil
	}
	probeOffsets = activeProbes
	bufs := make([][]int16, len(probeOffsets))
	for i, off := range probeOffsets {
		data, err := ReadSamples(p.File, first+int64(off), int(last-first)+probeLen)
		if err != nil {
			return 0, 0, err
		}
		bufs[i] = data
	}
	bestAt := first
	best := -1.0
	for a := first; a <= last; a += 8 {
		score := 0.0
		for i, off := range probeOffsets {
			score += corr(ref[off:off+probeLen], bufs[i][int(a-first):int(a-first)+probeLen], 8) / float64(len(probeOffsets))
		}
		if score > best {
			best, bestAt = score, a
		}
	}
	coarse := bestAt
	best = -1
	for a := max(first, coarse-8); a <= min(last, coarse+8); a++ {
		score := 0.0
		for i, off := range probeOffsets {
			score += corr(ref[off:off+probeLen], bufs[i][int(a-first):int(a-first)+probeLen], 1) / float64(len(probeOffsets))
		}
		if score > best {
			best, bestAt = score, a
		}
	}
	if best < .94 {
		return 0, 0, nil
	}
	candidate, err := ReadSamples(p.File, bestAt, len(ref))
	if err != nil {
		return 0, 0, err
	}
	minimum := 1.0
	informative := 0
	total := 0
	for i := 0; i < len(ref); i += Rate / 2 {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		end := min(i+Rate/2, len(ref))
		if end-i < Rate/8 {
			i = max(0, len(ref)-Rate/2)
			end = len(ref)
		}
		total++
		score := corr(ref[i:end], candidate[i:end], 1)
		if score == -1 { // Quiet edges also must remain quiet, never skip a changed tail.
			var delta float64
			for k := i; k < end; k++ {
				d := float64(ref[k]) - float64(candidate[k])
				delta += d * d
			}
			if delta > float64(end-i)*16 {
				return 0, 0, nil
			}
		} else {
			informative++
			minimum = math.Min(minimum, score)
			if score < .94 {
				return 0, 0, nil
			}
		}
		if end == len(ref) {
			break
		}
	}
	if informative*4 < total*3 {
		return 0, 0, nil
	}
	return bestAt, minimum, nil
}
