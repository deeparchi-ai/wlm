package token

import (
	"time"

	"github.com/deeparchi-ai/wlm/internal/arbitrator"
)

// ToArbitratorState maps a Token Budget into the CPU arbitrator's State model.
//
// When a class hasn't been observed long enough, reserve its full budget —
// don't project from zero/low consumption. This prevents "quiet early phase"
// classes from being marked as surplus and having their budget taken preemptively.
func (b *Budget) ToArbitratorState() arbitrator.State {
	projected := b.Projected()
	goalMet := b.GoalMet()

	if time.Since(b.CycleStart) < minObservation {
		projected = b.DailyBudget
		goalMet = true
	}

	return arbitrator.State{
		Name:       b.Name,
		Importance: b.Importance,
		Proposed:   projected,
		MinWeight:  b.MinBudget,
		MaxWeight:  b.DailyBudget,
		GoalMet:    goalMet,
		Pressure:   b.Pressure(),
	}
}

// AvailableAfter returns the available budget after arbitration.
func (b *Budget) AvailableAfter(d arbitrator.Decision) uint32 {
	adj := int32(d.Final) - int32(b.Consumed)
	if adj < 0 {
		adj = 0
	}
	return uint32(adj)
}
