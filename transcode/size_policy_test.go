package transcode

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestFinalSizeFrozenModeBoundary(t *testing.T) {
	for _, tc := range []struct {
		source, candidate int64
		minimum           float64
		pass              bool
	}{{100, 85, 15, true}, {100, 86, 15, false}, {100, 95, 15, false}, {100, 95, 0, true}, {100, 110, 0, false}, {101, 86, 15, false}, {1000000000000000000, 850000000000000000, 15, true}, {1000000000000000000, 850000000000000001, 15, false}, {0, 85, 15, false}, {100, 85, math.NaN(), false}} {
		err := ValidateFinalSize(tc.source, tc.candidate, &SizePolicy{MinSavingsPercent: tc.minimum, MaxSizeIncreasePercent: 0})
		if (err == nil) != tc.pass {
			t.Fatalf("source=%d candidate=%d minimum=%v pass=%v: %v", tc.source, tc.candidate, tc.minimum, tc.pass, err)
		}
	}
	if err := ValidateFinalSize(0, 0, nil); err != nil {
		t.Fatal("legacy behavior changed", err)
	}
}

func TestFinalSizeMaxCandidateBytes(t *testing.T) {
	policy := func(max int64) *SizePolicy {
		return &SizePolicy{MinSavingsPercent: 15, MaxCandidateBytes: max}
	}
	if err := ValidateFinalSize(1000, 500, policy(500)); err != nil {
		t.Fatalf("a candidate equal to the cap must pass: %v", err)
	}
	err := ValidateFinalSize(1000, 501, policy(500))
	var rejected *PolicyRejectionError
	if !errors.As(err, &rejected) || rejected.ReasonCode != "size_budget_exceeded" {
		t.Fatalf("one byte over the cap must be size_budget_exceeded, got %v", err)
	}
	// Over the cap and over the 85% line is still reported as over the budget.
	if err := ValidateFinalSize(1000, 900, policy(500)); !errors.As(err, &rejected) || rejected.ReasonCode != "size_budget_exceeded" {
		t.Fatalf("over the cap and the minimum saving must report the cap, got %v", err)
	}
	err = ValidateFinalSize(1000, 500, policy(-1))
	if err == nil || !strings.HasPrefix(err.Error(), "size_policy_invalid") {
		t.Fatalf("a negative cap must be size_policy_invalid, got %v", err)
	}
	if err := ValidateFinalSize(1000, 840, policy(0)); err != nil {
		t.Fatalf("a zero cap means no ceiling: %v", err)
	}
}

// A plan without a ceiling serializes exactly as before, so plan digests of
// every other mode are unchanged.
func TestSizePolicyCapIsOmittedWhenZero(t *testing.T) {
	b, err := json.Marshal(SizePolicy{MinSavingsPercent: 15})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "max_candidate_bytes") {
		t.Fatalf("zero cap must be omitted: %s", b)
	}
}
