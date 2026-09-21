package action

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

const (
	// DefaultMinSavingsPercent is the production size-guardrail default for
	// transcode_media when the caller omits min_savings_percent. It mirrors
	// the established transcode_batch behavior (see config.yaml.example).
	DefaultMinSavingsPercent = 15.0
	// DefaultMaxSizeIncreasePercent is the production size-guardrail default
	// for transcode_media when the caller omits max_size_increase_percent.
	DefaultMaxSizeIncreasePercent = 0.0
)

// resolveTranscodeSizeGuardrails returns the effective size guardrails for
// transcode_media. Explicit caller-provided values always win; otherwise the
// configured policy applies; otherwise the production defaults (15% minimum
// savings, 0% allowed growth) apply.
func resolveTranscodeSizeGuardrails(inputs map[string]any, configMinSavingsPercent float64) (minSavingsPercent, maxSizeIncreasePercent float64) {
	minSavingsPercent = DefaultMinSavingsPercent
	if configMinSavingsPercent > 0 {
		minSavingsPercent = configMinSavingsPercent
	}
	maxSizeIncreasePercent = DefaultMaxSizeIncreasePercent
	if inputs != nil {
		if v, ok := inputs["min_savings_percent"]; ok && v != nil {
			minSavingsPercent = getFloat(inputs, "min_savings_percent")
		}
		if v, ok := inputs["max_size_increase_percent"]; ok && v != nil {
			maxSizeIncreasePercent = getFloat(inputs, "max_size_increase_percent")
		}
	}
	return minSavingsPercent, maxSizeIncreasePercent
}

// effectiveSizeGuardrails resolves the guardrails for the current execution,
// honoring explicit caller inputs first and the configured transcode policy
// second. transcode_batch semantics are untouched: batch children always
// carry explicit values, so this helper only changes the direct-call path
// that previously had no guardrails at all.
func (e *Engine) effectiveSizeGuardrails(ec *ExecutionContext) (float64, float64) {
	var configMin float64
	if e.deps.Config != nil {
		configMin = e.deps.Config.Transcode.MinSavingsPercent
	}
	return resolveTranscodeSizeGuardrails(ec.Inputs, configMin)
}

// checkBenchmarkSavingsGuardrail reports whether a benchmark winner's
// predicted savings satisfies the effective size guardrails. Positive
// savings means the output is predicted smaller; negative savings means it
// is predicted larger. A nil return means the full encode may proceed.
//
// Zero disables the minimum-savings requirement (matching the selector
// semantics: min_savings_percent=0 means no minimum), while
// max_size_increase_percent=0 allows exactly zero growth.
func checkBenchmarkSavingsGuardrail(savingsPercent, minSavingsPercent, maxSizeIncreasePercent float64) error {
	if minSavingsPercent > 0 && savingsPercent < minSavingsPercent {
		return fmt.Errorf("benchmark winner predicts only %.1f%% savings, below required minimum %.1f%% (min_savings_percent=%.1f)", savingsPercent, minSavingsPercent, minSavingsPercent)
	}
	if savingsPercent < 0 {
		if growth := -savingsPercent; growth > maxSizeIncreasePercent {
			return fmt.Errorf("benchmark winner predicts %.1f%% size growth, exceeding allowed maximum size increase %.1f%% (max_size_increase_percent=%.1f)", growth, maxSizeIncreasePercent, maxSizeIncreasePercent)
		}
	}
	return nil
}

func getPlan(v any) *transcode.Plan {
	if p, ok := v.(*transcode.Plan); ok {
		return p
	}
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var p transcode.Plan
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

func getFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

func getStreamsList(m map[string]any, key string) []mediainspect.DetailedStream {
	if m == nil {
		return nil
	}
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}
	if streams, ok := raw.([]mediainspect.DetailedStream); ok {
		return streams
	}
	var b []byte
	switch v := raw.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		var err error
		b, err = json.Marshal(v)
		if err != nil {
			return nil
		}
	}
	var streams []mediainspect.DetailedStream
	if json.Unmarshal(b, &streams) != nil {
		return nil
	}
	return streams
}

func getInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		i, err := strconv.Atoi(string(v))
		if err == nil {
			return i
		}
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil {
			return i
		}
	}
	return 0
}

func getBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	if b, ok := m[key].(bool); ok {
		return b
	}
	if s, ok := m[key].(string); ok {
		s = strings.ToLower(strings.TrimSpace(s))
		return s == "true" || s == "1" || s == "yes"
	}
	return false
}

func getSourceReport(v any) *mediainspect.DetailedReport {
	if r, ok := v.(*mediainspect.DetailedReport); ok {
		return r
	}
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var r mediainspect.DetailedReport
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

func getOptimizationPolicy(v any) *recipe.OptimizationPolicy {
	if p, ok := v.(*recipe.OptimizationPolicy); ok {
		return p
	}
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var p recipe.OptimizationPolicy
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

func isSourceHDRorDV(rep *mediainspect.DetailedReport) bool {
	return mediainspect.IsHDRorDolbyVisionReport(rep)
}
