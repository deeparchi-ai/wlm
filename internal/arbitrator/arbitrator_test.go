package arbitrator

import (
	"testing"
)

func TestArbitrateSingleClass(t *testing.T) {
	states := []State{
		{Name: "only", Importance: 1, Proposed: 500, MinWeight: 100, MaxWeight: 1000, GoalMet: true},
	}
	decisions := Arbitrate(states)
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decisions))
	}
	if decisions[0].Final != 500 {
		t.Errorf("single class should keep proposed weight, got %d", decisions[0].Final)
	}
}

func TestArbitrateAllGoalsMet(t *testing.T) {
	// When all goals are met, no redistribution should occur
	states := []State{
		{Name: "high", Importance: 1, Proposed: 500, MinWeight: 100, MaxWeight: 1000, GoalMet: true},
		{Name: "mid", Importance: 3, Proposed: 300, MinWeight: 100, MaxWeight: 1000, GoalMet: true},
		{Name: "low", Importance: 5, Proposed: 200, MinWeight: 100, MaxWeight: 1000, GoalMet: true},
	}
	decisions := Arbitrate(states)
	for _, d := range decisions {
		if d.Final != d.Proposed {
			t.Errorf("%s: all goals met, expected no change (proposed=%d, final=%d)",
				d.Name, d.Proposed, d.Final)
		}
	}
}

func TestHighImportanceTakesFromLow(t *testing.T) {
	// High-importance class is under pressure, low-importance has surplus
	states := []State{
		{Name: "critical", Importance: 1, Proposed: 300, MinWeight: 100, MaxWeight: 1000, GoalMet: false, Pressure: 25.0},
		{Name: "batch", Importance: 5, Proposed: 700, MinWeight: 100, MaxWeight: 1000, GoalMet: true, Pressure: 1.0},
	}
	decisions := Arbitrate(states)

	// Find critical and batch decisions
	var crit, bat Decision
	for _, d := range decisions {
		switch d.Name {
		case "critical":
			crit = d
		case "batch":
			bat = d
		}
	}

	if crit.Final <= crit.Proposed {
		t.Errorf("critical should gain weight: proposed=%d, final=%d", crit.Proposed, crit.Final)
	}
	if bat.Final >= bat.Proposed {
		t.Errorf("batch should lose weight: proposed=%d, final=%d", bat.Proposed, bat.Final)
	}
	if bat.Final < 100 {
		t.Errorf("batch should not go below minWeight: min=%d, final=%d", 100, bat.Final)
	}
}

func TestLowImportanceDoesNotTakeFromHigh(t *testing.T) {
	// Low-importance class is under pressure, but should NOT take from high
	states := []State{
		{Name: "critical", Importance: 1, Proposed: 300, MinWeight: 100, MaxWeight: 1000, GoalMet: true, Pressure: 1.0},
		{Name: "batch", Importance: 5, Proposed: 300, MinWeight: 100, MaxWeight: 1000, GoalMet: false, Pressure: 30.0},
	}
	decisions := Arbitrate(states)

	var crit, bat Decision
	for _, d := range decisions {
		switch d.Name {
		case "critical":
			crit = d
		case "batch":
			bat = d
		}
	}

	if crit.Final < crit.Proposed {
		t.Errorf("critical should NOT lose weight to lower-importance: proposed=%d, final=%d",
			crit.Proposed, crit.Final)
	}
	// batch can still gain via its own PID (proposed already reflects that)
	if bat.Final < bat.Proposed {
		t.Errorf("batch should not lose weight: proposed=%d, final=%d", bat.Proposed, bat.Final)
	}
}

func TestMinWeightRespected(t *testing.T) {
	// High-importance needs weight, but low-importance is already at min
	states := []State{
		{Name: "critical", Importance: 1, Proposed: 300, MinWeight: 100, MaxWeight: 1000, GoalMet: false, Pressure: 30.0},
		{Name: "batch", Importance: 5, Proposed: 100, MinWeight: 100, MaxWeight: 1000, GoalMet: true, Pressure: 1.0},
	}
	decisions := Arbitrate(states)

	var bat Decision
	for _, d := range decisions {
		if d.Name == "batch" {
			bat = d
		}
	}
	if bat.Final < 100 {
		t.Errorf("batch should stay at minWeight=100, got %d", bat.Final)
	}
}

func TestThreeClassHierarchy(t *testing.T) {
	// imp=1 needs weight, imp=3 has surplus, imp=5 has surplus
	// imp=1 should take from imp=5 first, then imp=3 if needed
	states := []State{
		{Name: "critical", Importance: 1, Proposed: 100, MinWeight: 100, MaxWeight: 1000, GoalMet: false, Pressure: 35.0},
		{Name: "normal", Importance: 3, Proposed: 400, MinWeight: 200, MaxWeight: 800, GoalMet: true, Pressure: 5.0},
		{Name: "batch", Importance: 5, Proposed: 500, MinWeight: 100, MaxWeight: 900, GoalMet: true, Pressure: 2.0},
	}
	decisions := Arbitrate(states)

	var crit Decision
	for _, d := range decisions {
		if d.Name == "critical" {
			crit = d
		}
	}

	if crit.Final < crit.Proposed {
		t.Errorf("critical should gain from lower classes: proposed=%d, final=%d",
			crit.Proposed, crit.Final)
	}
	// Verify that lower classes didn't go below their mins
	for _, d := range decisions {
		if d.Name == "normal" && d.Final < 200 {
			t.Errorf("normal went below min: final=%d", d.Final)
		}
		if d.Name == "batch" && d.Final < 100 {
			t.Errorf("batch went below min: final=%d", d.Final)
		}
	}
}

func TestPass1MinimumGuarantee(t *testing.T) {
	// A class below its min should be pulled up even if goal is met
	states := []State{
		{Name: "starving", Importance: 2, Proposed: 50, MinWeight: 200, MaxWeight: 1000, GoalMet: true, Pressure: 0.0},
		{Name: "fat", Importance: 5, Proposed: 800, MinWeight: 100, MaxWeight: 1000, GoalMet: true, Pressure: 0.0},
	}
	decisions := Arbitrate(states)

	var star, fat Decision
	for _, d := range decisions {
		switch d.Name {
		case "starving":
			star = d
		case "fat":
			fat = d
		}
	}

	if star.Final < 200 {
		t.Errorf("starving should reach minWeight=200 via Pass 1, got %d", star.Final)
	}
	if fat.Final > fat.Proposed {
		t.Errorf("fat should not gain, got %d", fat.Final)
	}
}
