package optimization

import (
	"sort"
)

// Adaptive evaluation modes for benchmark candidate search.
const (
	// AdaptiveModeExhaustive preserves the existing exhaustive path unchanged.
	AdaptiveModeExhaustive = "exhaustive"
	// AdaptiveModeAdaptive enables ordered adaptive probing with exhaustive fallback.
	AdaptiveModeAdaptive = "adaptive"
)

// DefaultAdaptiveInitialQuality is the default starting quality for adaptive probing.
// It is intentionally generic: callers may override it per request/policy.
// It must not encode source or content names.
const DefaultAdaptiveInitialQuality = 65

// AdaptiveProbeObservation records one evaluated probe for planning.
// Evaluation carries the policy outcome (eligible, target reached, candidate score)
// so average-minimum, per-sample-minimum, and target semantics are preserved.
type AdaptiveProbeObservation struct {
	// SortedPos is the position in ascending-quality order (0 = lowest quality).
	SortedPos int
	// Quality is the encoder quality value probed.
	Quality int
	// Evaluation is the policy evaluation for the probed candidate.
	Evaluation PolicyEvaluation
	// Valid is true when the probe produced a conclusive evaluation
	// (encode succeeded and metric aggregate is valid).
	// A !Valid probe is a failure/inconclusive result and forces fallback.
	Valid bool
}

// AdaptivePlanner drives deterministic ordered probing over quality candidates.
// It starts near a configurable initial quality, establishes a safe quality floor
// from the high endpoint, probes to find the lowest safe candidate, and falls back
// to exhaustive evaluation whenever correctness cannot be proven.
type AdaptivePlanner struct {
	qualities   []int
	target      float64
	tolerance   float64
	initialPos  int
	highPos     int
	evaluated   map[int]AdaptiveProbeObservation
	fallback    bool
	fallbackWhy string
	floor       float64
	floorSet    bool
}

// NewAdaptivePlanner creates a planner over ordered quality candidates.
// qualities may be unsorted; the planner sorts ascending deterministically.
// initialQuality selects the starting probe (closest quality, tie-break lower).
// target and tolerance define the quality floor: floor = max(target, highScore-tolerance).
func NewAdaptivePlanner(qualities []int, initialQuality int, target, tolerance float64) *AdaptivePlanner {
	sorted := append([]int(nil), qualities...)
	sort.Ints(sorted)
	p := &AdaptivePlanner{
		qualities: sorted,
		target:    target,
		tolerance: tolerance,
		evaluated: make(map[int]AdaptiveProbeObservation),
	}
	n := len(sorted)
	if n == 0 {
		p.initialPos = -1
		p.highPos = -1
		return p
	}
	if initialQuality < 1 || initialQuality > 100 {
		initialQuality = DefaultAdaptiveInitialQuality
	}
	best := 0
	bestDist := absInt(sorted[0] - initialQuality)
	for i := 1; i < n; i++ {
		d := absInt(sorted[i] - initialQuality)
		if d < bestDist || (d == bestDist && sorted[i] < sorted[best]) {
			best = i
			bestDist = d
		}
	}
	p.initialPos = best
	p.highPos = n - 1
	return p
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Qualities returns the sorted ascending qualities.
func (p *AdaptivePlanner) Qualities() []int {
	return append([]int(nil), p.qualities...)
}

// InitialPos returns the starting probe position.
func (p *AdaptivePlanner) InitialPos() int { return p.initialPos }

// HighPos returns the high-endpoint position.
func (p *AdaptivePlanner) HighPos() int { return p.highPos }

// Floor returns the established quality floor and whether it is set.
func (p *AdaptivePlanner) Floor() (float64, bool) { return p.floor, p.floorSet }

// NeedsFallback reports whether exhaustive fallback is required.
func (p *AdaptivePlanner) NeedsFallback() bool { return p.fallback }

// FallbackReason reports why fallback was triggered.
func (p *AdaptivePlanner) FallbackReason() string { return p.fallbackWhy }

func (p *AdaptivePlanner) setFallback(why string) {
	if !p.fallback {
		p.fallback = true
		p.fallbackWhy = why
	}
}

// Observe records one probe result and updates floor/fallback state.
// It must be called in deterministic probe order via NextProbe.
func (p *AdaptivePlanner) Observe(obs AdaptiveProbeObservation) {
	if p == nil || len(p.qualities) == 0 {
		return
	}
	if obs.SortedPos < 0 || obs.SortedPos >= len(p.qualities) {
		p.setFallback("observation position out of range")
		return
	}
	p.evaluated[obs.SortedPos] = obs
	if !obs.Valid {
		p.setFallback("probe failed or inconclusive")
		return
	}

	// Once both endpoints are evaluated, establish the floor and check monotonicity.
	initObs, hasInit := p.evaluated[p.initialPos]
	highObs, hasHigh := p.evaluated[p.highPos]
	if !hasInit || !hasHigh {
		return
	}
	if !initObs.Valid || !highObs.Valid {
		// Already marked fallback via !Valid above, but guard explicitly.
		p.setFallback("endpoint probe failed or inconclusive")
		return
	}
	// High endpoint must establish a safe floor: it must be eligible and reach target.
	// Otherwise the minimum-meeting branch may apply and adaptive floor logic is unsafe.
	if !highObs.Evaluation.Eligible || !highObs.Evaluation.TargetReached {
		p.setFallback("high endpoint cannot establish safe floor")
		return
	}
	floor := highObs.Evaluation.CandidateScore - p.tolerance
	if floor < p.target {
		floor = p.target
	}
	p.floor = floor
	p.floorSet = true

	// Monotonicity between initial and high: high must not score materially below initial.
	// Material violation uses marginal tolerance as the noise budget; small jitter is tolerated.
	if highObs.Evaluation.CandidateScore+toleranceLess(p.tolerance) < initObs.Evaluation.CandidateScore {
		p.setFallback("monotonicity violated between initial and high endpoint")
		return
	}

	// Pairwise monotonicity over all evaluated: scores must be non-decreasing with
	// quality within tolerance. Any material decrease forces fallback.
	positions := make([]int, 0, len(p.evaluated))
	for pos := range p.evaluated {
		positions = append(positions, pos)
	}
	sort.Ints(positions)
	for i := 1; i < len(positions); i++ {
		lo := p.evaluated[positions[i-1]]
		hi := p.evaluated[positions[i]]
		if !lo.Valid || !hi.Valid {
			continue
		}
		if hi.Evaluation.CandidateScore+toleranceLess(p.tolerance) < lo.Evaluation.CandidateScore {
			p.setFallback("monotonicity violated between probed qualities")
			return
		}
	}
}

func toleranceLess(tol float64) float64 {
	// Use tolerance directly as the materiality budget. Guard negative tolerance
	// (should not happen after policy validation) by treating it as zero.
	if tol < 0 {
		return 0
	}
	return tol
}

// NextProbe returns the next sorted position to probe and whether probing continues.
// When fallback is required, it returns remaining unevaluated positions in deterministic
// ascending order one at a time until all are evaluated (exhaustive completion).
// When no fallback is needed, it returns the next adaptive probe, or ok=false when
// the evaluated set is sufficient and remaining candidates are provably ineligible.
func (p *AdaptivePlanner) NextProbe() (pos int, ok bool) {
	if p == nil || len(p.qualities) == 0 {
		return 0, false
	}
	n := len(p.qualities)

	// Phase 1: initial probe.
	if _, done := p.evaluated[p.initialPos]; !done {
		return p.initialPos, true
	}
	// Phase 2: high endpoint probe (unless it is the initial).
	if p.highPos != p.initialPos {
		if _, done := p.evaluated[p.highPos]; !done {
			return p.highPos, true
		}
	}

	// If fallback was triggered by observations, drain remaining ascending.
	if p.fallback {
		for i := 0; i < n; i++ {
			if _, done := p.evaluated[i]; !done {
				return i, true
			}
		}
		return 0, false
	}

	// Both endpoints must be evaluated before adaptive descent/ascent.
	if _, done := p.evaluated[p.initialPos]; !done {
		return p.initialPos, true
	}
	if p.highPos != p.initialPos {
		if _, done := p.evaluated[p.highPos]; !done {
			return p.highPos, true
		}
	}
	if !p.floorSet {
		// Floor not established (e.g. high not eligible): Observe already set fallback,
		// but defensively drain remaining.
		for i := 0; i < n; i++ {
			if _, done := p.evaluated[i]; !done {
				return i, true
			}
		}
		return 0, false
	}

	initObs := p.evaluated[p.initialPos]
	initMeets := initObs.Valid && initObs.Evaluation.Eligible &&
		initObs.Evaluation.CandidateScore >= p.floor

	if !initMeets {
		// Initial failed floor/safety: search upward for the lowest passing candidate.
		// Probe unevaluated intermediates ascending; lower qualities are skipped only
		// when the initial failure is material (more than tolerance below floor) or
		// the initial is validly ineligible. Borderline failures fall back to
		// evaluating lower candidates rather than guessing.
		for i := p.initialPos + 1; i < p.highPos; i++ {
			if _, done := p.evaluated[i]; !done {
				return i, true
			}
		}
		// All intermediates evaluated: decide about lower qualities.
		if p.initialPos > 0 {
			gap := p.floor - initObs.Evaluation.CandidateScore
			// Ineligible (e.g. per-sample below minimum) or material gap: lower cannot pass.
			if !initObs.Evaluation.Eligible || gap > toleranceLess(p.tolerance) {
				return 0, false
			}
			// Borderline: must verify lower rather than skip.
			for i := p.initialPos - 1; i >= 0; i-- {
				if _, done := p.evaluated[i]; !done {
					return i, true
				}
			}
			return 0, false
		}
		return 0, false
	}

	// Initial meets floor/safety: all intermediates between initial and high must be
	// evaluated to preserve selector size semantics (any qualified candidate could win
	// on estimated size, which adaptive must not second-guess).
	for i := p.initialPos + 1; i < p.highPos; i++ {
		if _, done := p.evaluated[i]; !done {
			return i, true
		}
	}

	// Descend below initial to find the lowest passing candidate.
	// Probe immediate predecessor first; stop skipping lower once one fails.
	for i := p.initialPos - 1; i >= 0; i-- {
		if _, done := p.evaluated[i]; !done {
			return i, true
		}
		// This position is evaluated: if it fails floor/safety, all lower are
		// provably ineligible under monotonicity, so stop. Otherwise continue down.
		obs := p.evaluated[i]
		if !obs.Valid {
			// Should have triggered fallback in Observe; drain defensively.
			for j := 0; j < n; j++ {
				if _, done := p.evaluated[j]; !done {
					return j, true
				}
			}
			return 0, false
		}
		if !obs.Evaluation.Eligible || obs.Evaluation.CandidateScore < p.floor {
			// Check materiality for borderline failures: if the failure is within
			// tolerance of the floor, lower could still pass on noise, so keep
			// probing down rather than skipping. The loop naturally continues to
			// the next lower position in that case only if we do NOT stop here.
			// To stay safe without extra complexity, stop only on material failure
			// or clear ineligibility; borderline continues downward.
			if obs.Evaluation.Eligible {
				gap := p.floor - obs.Evaluation.CandidateScore
				if gap <= toleranceLess(p.tolerance) {
					continue
				}
			}
			return 0, false
		}
	}
	return 0, false
}

// EvaluatedQualities returns probed qualities in deterministic ascending order.
func (p *AdaptivePlanner) EvaluatedQualities() []int {
	qs := make([]int, 0, len(p.evaluated))
	for pos := range p.evaluated {
		qs = append(qs, p.qualities[pos])
	}
	sort.Ints(qs)
	return qs
}

// ProbeOrder returns the deterministic initial probe prefix: initial then high.
// It is used for order-determinism checks and documentation; dynamic descent/ascent
// is driven by NextProbe/Observe.
func (p *AdaptivePlanner) ProbeOrder() []int {
	if len(p.qualities) == 0 {
		return nil
	}
	order := []int{p.qualities[p.initialPos]}
	if p.highPos != p.initialPos {
		order = append(order, p.qualities[p.highPos])
	}
	return order
}
