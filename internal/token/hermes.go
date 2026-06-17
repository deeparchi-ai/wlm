package token

import (
	"fmt"
	"math"
)

// CheckResult is returned by BeforeCall.
type CheckResult struct {
	Allowed      bool
	SignalLevel  string
	Remaining    uint32
	CostEstimate uint32
	ModelWeights map[string]float64 // cached for AfterCall
}

// BeforeCall checks if a class has enough budget for an LLM call.
// Uses 1.5x safety factor for completion uncertainty.
//
// NOTE: Token control is advisory — Agent voluntarily calls BeforeCall/AfterCall.
// Not kernel-enforced. A gateway-level hard enforcement is planned for v0.4.0.
func BeforeCall(className, model string, estimatedTokens uint32) (CheckResult, error) {
	state, err := ReadState()
	if err != nil {
		return CheckResult{}, fmt.Errorf("reading token state: %w", err)
	}

	for _, c := range state.Classes {
		if c.Name == className {
			weight := 1.0
			if w, ok := c.ModelWeights[model]; ok {
				weight = w
			}
			cost := uint32(math.Round(float64(estimatedTokens) * weight * 1.5))

			return CheckResult{
				Allowed:      cost <= c.AvailableBudget,
				SignalLevel:  c.SignalLevel,
				Remaining:    c.AvailableBudget,
				CostEstimate: cost,
				ModelWeights: c.ModelWeights,
			}, nil
		}
	}

	return CheckResult{}, fmt.Errorf("class %q not found in token state", className)
}

// AfterCall records token consumption after an LLM call completes.
func AfterCall(className, model string, actualTokens uint32, modelWeights map[string]float64) error {
	return AppendCounter(className, model, actualTokens, modelWeights)
}
