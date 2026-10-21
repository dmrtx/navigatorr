package transcode

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
)

// ValidateFinalSize compares original byte counts without rounded percentages.
// Legacy plans without a policy retain their existing behavior.
type PolicyRejectionError struct {
	ReasonCode string
	Message    string
}

func (e *PolicyRejectionError) Error() string { return e.ReasonCode + ": " + e.Message }

func ValidateFinalSize(source, candidate int64, policy *SizePolicy) error {
	if policy == nil {
		return nil
	}
	if source <= 0 || candidate <= 0 {
		return fmt.Errorf("size_evidence_missing: positive source and candidate byte measurements required")
	}
	if math.IsNaN(policy.MinSavingsPercent) || math.IsInf(policy.MinSavingsPercent, 0) || policy.MinSavingsPercent < 0 || policy.MinSavingsPercent > 100 || math.IsNaN(policy.MaxSizeIncreasePercent) || math.IsInf(policy.MaxSizeIncreasePercent, 0) || policy.MaxSizeIncreasePercent < 0 {
		return fmt.Errorf("size_policy_invalid: invalid frozen size policy")
	}
	if policy.MaxCandidateBytes < 0 {
		return fmt.Errorf("size_policy_invalid: invalid frozen size policy")
	}
	// The absolute ceiling is checked first: a size budget sits under the
	// minimum saving, so a file over it is reported as over the budget, which is
	// what the coordinator acts on, not as insufficient savings.
	if policy.MaxCandidateBytes > 0 && candidate > policy.MaxCandidateBytes {
		return &PolicyRejectionError{ReasonCode: "size_budget_exceeded", Message: fmt.Sprintf("candidate is %d bytes, over the %d byte size budget", candidate, policy.MaxCandidateBytes)}
	}
	percent := func(v float64) *big.Rat {
		r, _ := new(big.Rat).SetString(strconv.FormatFloat(v, 'f', -1, 64))
		return r
	}
	src := new(big.Rat).SetInt64(source)
	cand := new(big.Rat).SetInt64(candidate)
	saving := new(big.Rat).Mul(new(big.Rat).Sub(src, cand), big.NewRat(100, 1))
	if saving.Cmp(new(big.Rat).Mul(src, percent(policy.MinSavingsPercent))) < 0 {
		return &PolicyRejectionError{ReasonCode: "insufficient_savings", Message: "candidate violates frozen minimum savings policy"}
	}
	growth := new(big.Rat).Mul(new(big.Rat).Sub(cand, src), big.NewRat(100, 1))
	if growth.Cmp(new(big.Rat).Mul(src, percent(policy.MaxSizeIncreasePercent))) > 0 {
		return &PolicyRejectionError{ReasonCode: "size_growth_exceeded", Message: "candidate violates frozen maximum growth policy"}
	}
	return nil
}
