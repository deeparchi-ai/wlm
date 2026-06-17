// Package token provides the token budget control model.
// It maps the WLM observe→arbitrate→apply loop onto token consumption,
// using model-weighted burn rate as the observation signal.
package token

import (
	"math"
	"time"
)

// Budget tracks token consumption and projections for one service class.
//
// The budget cycle (e.g. 24h) is the tracking period — DailyBudget is the
// maximum weighted tokens allowed within one cycle. The arbitration ticker
// fires more frequently (e.g. every 1h) but does NOT reset Consumed.
// Consumed only resets when the cycle ends.
type Budget struct {
	Name          string
	Importance    int
	DailyBudget   uint32             // max weighted tokens per cycle
	MinBudget     uint32             // floor — cannot be taken below this
	ModelWeights  map[string]float64 // model name → cost multiplier
	Consumed      uint32             // weighted tokens consumed this cycle (cumulative)
	CycleStart    time.Time
	CycleDuration time.Duration
}

const (
	// minObservation is the minimum time before we trust actual burn rate.
	minObservation = 2 * time.Minute
)

// WeightedConsume records a token consumption event, applying model weight.
func (b *Budget) WeightedConsume(model string, tokens uint32) uint32 {
	weight := 1.0
	if w, ok := b.ModelWeights[model]; ok {
		weight = w
	}
	cost := uint32(math.Round(float64(tokens) * weight))
	b.Consumed += cost
	return cost
}

// BurnRate returns current burn rate in weighted tokens/second.
//
// During the observation blind zone we use a conservative prior based on
// importance: high-importance classes are assumed to need their full budget,
// low-importance classes are assumed on-track.
func (b *Budget) BurnRate() float64 {
	elapsed := time.Since(b.CycleStart)
	if elapsed < minObservation {
		ratio := 1.0
		if b.Importance >= 3 {
			ratio = 0.5
		}
		return b.BudgetRate() * ratio
	}
	seconds := elapsed.Seconds()
	if seconds <= 0 {
		return b.BudgetRate()
	}
	return float64(b.Consumed) / seconds
}

// BudgetRate returns the target rate to stay within daily budget.
func (b *Budget) BudgetRate() float64 {
	return float64(b.DailyBudget) / b.CycleDuration.Seconds()
}

// Projected returns estimated total consumption at cycle end.
func (b *Budget) Projected() uint32 {
	remaining := b.CycleDuration - time.Since(b.CycleStart)
	if remaining <= 0 {
		return b.Consumed
	}
	projected := float64(b.Consumed) + b.BurnRate()*remaining.Seconds()
	if projected > float64(b.DailyBudget*2) {
		projected = float64(b.DailyBudget * 2)
	}
	return uint32(math.Round(projected))
}

// GoalMet returns true if current burn rate is within budget.
func (b *Budget) GoalMet() bool {
	return b.BurnRate() <= b.BudgetRate()
}

// Pressure returns >1 when overspending, <1 when underspending.
func (b *Budget) Pressure() float64 {
	br := b.BudgetRate()
	if br <= 0 {
		return 1.0
	}
	return b.BurnRate() / br
}

// Remaining returns budget left for this cycle.
func (b *Budget) Remaining() uint32 {
	if b.Consumed >= b.DailyBudget {
		return 0
	}
	return b.DailyBudget - b.Consumed
}

// SignalLevel returns the 4-level signal for Hermes integration.
func (b *Budget) SignalLevel() string {
	p := b.Pressure()
	switch {
	case p < 0.8:
		return "green"
	case p < 1.0:
		return "yellow"
	case p < 1.5:
		return "red"
	default:
		return "black"
	}
}
