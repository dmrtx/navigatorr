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

// The largest accepted candidate is exactly the boundary ValidateFinalSize
// draws: it passes and one byte more is refused, whichever limit is lowest.
func TestMaxAcceptedCandidateBytesIsTheValidationBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source int64
		policy SizePolicy
		want   int64
	}{
		{"minimum saving", 10_000, SizePolicy{MinSavingsPercent: 15}, 8_500},
		{"fractional saving floors", 1_001, SizePolicy{MinSavingsPercent: 15}, 850},
		{"decimal percent is exact", 1_000, SizePolicy{MinSavingsPercent: 0.1}, 999},
		{"budget under the saving", 10_000, SizePolicy{MinSavingsPercent: 15, MaxCandidateBytes: 6_000}, 6_000},
		{"no saving required", 10_000, SizePolicy{MaxSizeIncreasePercent: 50}, 10_000},
		{"whole file saving", 10_000, SizePolicy{MinSavingsPercent: 100}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MaxAcceptedCandidateBytes(tc.source, &tc.policy)
			if !ok || got != tc.want {
				t.Fatalf("MaxAcceptedCandidateBytes = %d, %v; want %d", got, ok, tc.want)
			}
			if got > 0 {
				if err := ValidateFinalSize(tc.source, got, &tc.policy); err != nil {
					t.Fatalf("the limit itself must pass: %v", err)
				}
			}
			var rejected *PolicyRejectionError
			if err := ValidateFinalSize(tc.source, got+1, &tc.policy); !errors.As(err, &rejected) {
				t.Fatalf("one byte over the limit must be a policy refusal, got %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		source int64
		policy *SizePolicy
	}{{"no policy", 1_000, nil}, {"no source size", 0, &SizePolicy{MinSavingsPercent: 15}}, {"invalid policy", 1_000, &SizePolicy{MinSavingsPercent: 150}}} {
		if _, ok := MaxAcceptedCandidateBytes(tc.source, tc.policy); ok {
			t.Fatalf("%s: no limit can be stated", tc.name)
		}
	}
}
