// Package arbitrator implements importance-based resource redistribution.
//
// When multiple service classes compete for resources, the arbitrator ensures
// that higher-importance classes (lower importance numbers) meet their goals
// first, by transferring resource allocation from lower-importance classes.
//
// This is the core of IBM z/OS WLM's goal-mode arbitration: when importance=1
// is under pressure, it takes from importance=3; importance=3 takes from
// importance=5. When all goals are met, no redistribution occurs.
package arbitrator

import (
	"log"
	"sort"
)

// State describes one service class at arbitration time.
type State struct {
	Name       string
	Importance int     // 1=highest, 5=lowest
	Proposed   uint32  // weight proposed by local PID controller
	MinWeight  uint32  // absolute floor
	MaxWeight  uint32  // absolute ceiling
	GoalMet    bool    // true if local PID says goal is satisfied
	Pressure   float64 // current PSI or token depletion rate
}

// Decision is the arbitration output for one service class.
type Decision struct {
	Name       string
	Importance int
	Proposed   uint32 // original PID proposal
	Final      uint32 // after arbitration
	WeightDelta int32 // positive = gained, negative = lost
	Reason     string // human-readable explanation
}

// budgetPool tracks the redistribution budget during arbitration.
type budgetPool struct {
	classes []classEntry
}

type classEntry struct {
	idx        int
	importance int
	name       string
	proposed   uint32
	minWeight  uint32
	maxWeight  uint32
	goalMet    bool
	pressure   float64
	final      uint32
}

// Arbitrate redistributes weights to prioritize higher-importance classes.
//
// Algorithm (two-pass):
//
//	Pass 1: Ensure minimum guarantees.
//	  For each class sorted by importance (1 first):
//	    If class is below its minWeight → take from lower-importance classes
//	    that are above their minWeight, proportionally to surplus.
//
//	Pass 2: Goal-driven redistribution.
//	  For each class sorted by importance (1 first):
//	    If goal is NOT met (under pressure):
//	      Take weight from lower-importance classes, proportional to their
//	      surplus above min. The transfer amount is capped at:
//	         min(shortfall_to_max, sum_of_surplus_from_lower)
func Arbitrate(states []State) []Decision {
	if len(states) <= 1 {
		// Nothing to arbitrate — return as-is
		result := make([]Decision, len(states))
		for i, s := range states {
			result[i] = Decision{
				Name: s.Name, Importance: s.Importance,
				Proposed: s.Proposed, Final: s.Proposed,
				WeightDelta: 0, Reason: "only class",
			}
		}
		return result
	}

	pool := &budgetPool{
		classes: make([]classEntry, len(states)),
	}
	for i, s := range states {
		pool.classes[i] = classEntry{
			idx: i, importance: s.Importance,
			name: s.Name, proposed: s.Proposed,
			minWeight: s.MinWeight, maxWeight: s.MaxWeight,
			goalMet: s.GoalMet, pressure: s.Pressure,
			final: s.Proposed, // start with proposed
		}
	}

	// Sort by importance (ascending: 1=highest first)
	sort.Slice(pool.classes, func(i, j int) bool {
		return pool.classes[i].importance < pool.classes[j].importance
	})

	// --- Pass 1: Minimum guarantees ---
	for i := range pool.classes {
		c := &pool.classes[i]
		if c.final < c.minWeight {
			deficit := c.minWeight - c.final
			pool.transferFromLower(i, deficit)
		}
	}

	// --- Pass 2: Goal-driven redistribution ---
	for i := range pool.classes {
		c := &pool.classes[i]
		if !c.goalMet && c.final < c.maxWeight {
			// How much more would we like?
			headroom := c.maxWeight - c.final
			// Ask lower-importance classes for up to headroom
			pool.transferFromLower(i, headroom)
		}
	}

	// Build decisions (restore original order)
	decisions := make([]Decision, len(states))
	for _, c := range pool.classes {
		delta := int32(c.final) - int32(c.proposed)
		reason := "goal met, no change"
		if delta > 0 {
			reason = "gained from lower-importance classes"
		} else if delta < 0 {
			reason = "yielded to higher-importance classes"
		}
		decisions[c.idx] = Decision{
			Name: c.name, Importance: c.importance,
			Proposed: c.proposed, Final: c.final,
			WeightDelta: delta, Reason: reason,
		}
	}

	// Log changes
	for _, d := range decisions {
		if d.WeightDelta != 0 {
			log.Printf("[arb] %s (imp=%d): %d→%d (%+d) — %s",
				d.Name, d.Importance, d.Proposed, d.Final, d.WeightDelta, d.Reason)
		}
	}

	return decisions
}

// transferFromLower takes 'amount' weight from classes with lower importance
// (higher importance number) that are above their minWeight.
func (b *budgetPool) transferFromLower(donorIdx int, amount uint32) {
	donor := &b.classes[donorIdx]
	remaining := amount

	// Sum surplus of all lower-importance classes
	var totalSurplus uint32
	for j := donorIdx + 1; j < len(b.classes); j++ {
		c := &b.classes[j]
		if c.final > c.minWeight {
			totalSurplus += c.final - c.minWeight
		}
	}

	if totalSurplus == 0 {
		return // nothing to take from
	}

	// Distribute the transfer proportionally
	for j := donorIdx + 1; j < len(b.classes) && remaining > 0; j++ {
		c := &b.classes[j]
		if c.final <= c.minWeight {
			continue
		}
		surplus := c.final - c.minWeight
		// Proportional share of the transfer
		share := uint32(float64(remaining) * float64(surplus) / float64(totalSurplus))
		if share > surplus {
			share = surplus
		}
		if share > remaining {
			share = remaining
		}

		c.final -= share
		remaining -= share
		totalSurplus -= share
	}

	donor.final += amount - remaining
}
