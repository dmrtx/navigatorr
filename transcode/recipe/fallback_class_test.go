package recipe

import (
	"strings"
	"testing"
)

// ffmpeg_unknown now means a real ffmpeg failure nobody recognizes, which is
// still worth a bounded retry, so a recipe may name it. The classes that are
// never retried automatically cannot be fallback conditions: the fallback would
// never fire, and the recipe says otherwise.
func TestFallbackConditionsMatchWhatTheEngineRetries(t *testing.T) {
	with := func(when string) Profile {
		p := managedTestProfile()
		p.Optimization = nil
		p.Resilience.Fallbacks = []FallbackRule{{When: when, Action: "retry"}}
		return p
	}
	for _, when := range []string{"ffmpeg_unknown", "worker_busy", "ssh_transient"} {
		if err := ValidateProfile("p", with(when)); err != nil {
			t.Errorf("fallback on %q rejected: %v", when, err)
		}
	}
	for _, when := range []string{"unclassified", "benchmark_evidence_incomplete", "internal_invariant", "media_probe_failed"} {
		err := ValidateProfile("p", with(when))
		if err == nil || !strings.Contains(err.Error(), "never retried") {
			t.Errorf("fallback on %q: want a never-retried rejection, got %v", when, err)
		}
	}
	if err := ValidateProfile("p", with("no_such_class")); err == nil || !strings.Contains(err.Error(), "unknown fallback condition") {
		t.Errorf("unknown class: %v", err)
	}
}
