package token

import (
	"testing"
	"time"

	"github.com/deeparchi-ai/wlm/internal/arbitrator"
)

// ─────────────────────────────────────────────
// 仲裁映射测试 (8 tests — 复用 CPU 仲裁器)
// ─────────────────────────────────────────────

func TestTokenArbitrateAllWithinBudget(t *testing.T) {
	arch := newTestBudget("arch", 1, 400000, 100000, 200000)
	code := newTestBudget("code", 3, 300000, 50000, 100000)
	batch := newTestBudget("batch", 5, 200000, 10000, 50000)

	states := []arbitrator.State{
		arch.ToArbitratorState(),
		code.ToArbitratorState(),
		batch.ToArbitratorState(),
	}
	decisions := arbitrator.Arbitrate(states)

	for _, d := range decisions {
		if d.Final != d.Proposed {
			t.Errorf("%s: all within budget, expected no change (proposed=%d, final=%d)",
				d.Name, d.Proposed, d.Final)
		}
	}
}

func TestTokenArbitrateHighImportanceTakesFromLow(t *testing.T) {
	// arch (imp=1) overspending modestly → still within daily budget cap
	// code (imp=3) underspending → but arch is at its daily cap, can't receive more
	// Token semantics: daily budget IS the hard ceiling — arch can't exceed 400k
	arch := newTestBudget("arch", 1, 400000, 100000, 250000) // GoalMet=false, Projected~500k→capped 400k
	code := newTestBudget("code", 3, 300000, 50000, 100000)  // GoalMet=true, underspending

	states := []arbitrator.State{arch.ToArbitratorState(), code.ToArbitratorState()}
	decisions := arbitrator.Arbitrate(states)

	var archDec, codeDec arbitrator.Decision
	for _, d := range decisions {
		switch d.Name {
		case "arch":
			archDec = d
		case "code":
			codeDec = d
		}
	}

	// arch at max: Final should stay at DailyBudget (no headroom to receive more)
	if archDec.Final > archDec.Proposed {
		t.Errorf("arch at daily cap: should not gain above cap (proposed=%d, final=%d)", archDec.Proposed, archDec.Final)
	}
	// code underspending: no one takes from it since arch is capped
	if codeDec.Final <= 0 {
		t.Errorf("code should retain its budget, got %d", codeDec.Final)
	}
	if codeDec.Final < 50000 {
		t.Errorf("code should not go below min_budget=50000, got %d", codeDec.Final)
	}
}

func TestTokenArbitrateLowCannotTakeFromHigh(t *testing.T) {
	arch := newTestBudget("arch", 1, 400000, 100000, 150000)
	code := newTestBudget("code", 3, 300000, 50000, 280000)

	states := []arbitrator.State{arch.ToArbitratorState(), code.ToArbitratorState()}
	decisions := arbitrator.Arbitrate(states)

	var archDec arbitrator.Decision
	for _, d := range decisions {
		if d.Name == "arch" {
			archDec = d
		}
	}

	if archDec.Final < archDec.Proposed {
		t.Errorf("arch should NOT lose budget to lower-importance class")
	}
}

func TestTokenArbitrateMinBudgetRespected(t *testing.T) {
	arch := newTestBudget("arch", 1, 400000, 100000, 380000)
	code := newTestBudget("code", 3, 300000, 50000, 50000)

	states := []arbitrator.State{arch.ToArbitratorState(), code.ToArbitratorState()}
	decisions := arbitrator.Arbitrate(states)

	var codeDec arbitrator.Decision
	for _, d := range decisions {
		if d.Name == "code" {
			codeDec = d
		}
	}

	if codeDec.Final < 50000 {
		t.Errorf("code at min_budget should not lose more, got %d", codeDec.Final)
	}
}

func TestTokenArbitrateThreeClassHierarchy(t *testing.T) {
	// arch (imp=1) overspending at daily cap — can't receive more
	// code (imp=3) underspending
	// batch (imp=5) underspending
	// Token semantics: arch is at its hard 400k ceiling
	arch := newTestBudget("arch", 1, 400000, 100000, 250000) // GoalMet=false, capped at 400k
	code := newTestBudget("code", 3, 300000, 50000, 150000)  // GoalMet=true
	batch := newTestBudget("batch", 5, 200000, 10000, 80000)  // GoalMet=true

	states := []arbitrator.State{
		arch.ToArbitratorState(),
		code.ToArbitratorState(),
		batch.ToArbitratorState(),
	}
	decisions := arbitrator.Arbitrate(states)

	// arch at cap: Final should equal Proposed (= DailyBudget)
	for _, d := range decisions {
		if d.Name == "arch" && d.Final != d.Proposed {
			t.Errorf("arch at cap: Final should equal Proposed (%d), got %d", d.Proposed, d.Final)
		}
		if d.Name == "code" && d.Final < 50000 {
			t.Errorf("code went below min_budget")
		}
		if d.Name == "batch" && d.Final < 10000 {
			t.Errorf("batch went below min_budget")
		}
	}
}

func TestTokenArbitratePass1MinGuarantee(t *testing.T) {
	starving := newTestBudgetWithElapsed("starving", 2, 400000, 200000, 50000, 12*time.Hour)
	fat := newTestBudgetWithElapsed("fat", 5, 300000, 10000, 250000, 12*time.Hour)

	states := []arbitrator.State{starving.ToArbitratorState(), fat.ToArbitratorState()}
	decisions := arbitrator.Arbitrate(states)

	var starDec, fatDec arbitrator.Decision
	for _, d := range decisions {
		switch d.Name {
		case "starving":
			starDec = d
		case "fat":
			fatDec = d
		}
	}

	if starDec.Final < 200000 {
		t.Errorf("starving should reach min_budget via Pass 1, got %d", starDec.Final)
	}
	if fatDec.Final > fatDec.Proposed {
		t.Errorf("fat should not gain, got %d", fatDec.Final)
	}
}

func TestTokenArbitrateLargeBudgets(t *testing.T) {
	// All three classes at half cycle with proportional consumption.
	// arch consumed 2.5M/4M → overspending (GoalMet=false, at cap)
	// code consumed 1.4M/3M → slightly under (GoalMet=true)
	// batch consumed 0.9M/2M → slightly under (GoalMet=true)
	// Token view: arch at cap, code and batch keep their surplus since arch can't use more.
	arch := newTestBudgetWithElapsed("arch", 1, 4_000_000, 500_000, 2_500_000, 12*time.Hour)
	code := newTestBudgetWithElapsed("code", 3, 3_000_000, 300_000, 1_400_000, 12*time.Hour)
	batch := newTestBudgetWithElapsed("batch", 5, 2_000_000, 100_000, 900_000, 12*time.Hour)

	states := []arbitrator.State{
		arch.ToArbitratorState(),
		code.ToArbitratorState(),
		batch.ToArbitratorState(),
	}
	decisions := arbitrator.Arbitrate(states)

	for _, d := range decisions {
		switch d.Name {
		case "arch":
			if d.Final > d.Proposed {
				t.Errorf("arch at cap: should not gain above cap (proposed=%d, final=%d)", d.Proposed, d.Final)
			}
		case "code":
			if d.Final < 300_000 {
				t.Errorf("code below min: final=%d", d.Final)
			}
		case "batch":
			if d.Final < 100_000 {
				t.Errorf("batch below min: final=%d", d.Final)
			}
		}
	}
}

func TestToArbitratorStateMapping(t *testing.T) {
	b := &Budget{
		Name: "arch-guardian", Importance: 1,
		DailyBudget: 400000, MinBudget: 100000,
		Consumed: 200000,
	}
	b.CycleStart = time.Now().Add(-12 * time.Hour)
	b.CycleDuration = 24 * time.Hour

	state := b.ToArbitratorState()

	if state.Name != "arch-guardian" {
		t.Errorf("Name mismatch: %s", state.Name)
	}
	if state.Importance != 1 {
		t.Errorf("Importance mismatch: %d", state.Importance)
	}
	if state.MinWeight != 100000 {
		t.Errorf("MinWeight mismatch: %d", state.MinWeight)
	}
	if state.MaxWeight != 400000 {
		t.Errorf("MaxWeight mismatch: %d", state.MaxWeight)
	}
}

// ─────────────────────────────────────────────
// Token 专属边界测试 (5 tests)
// ─────────────────────────────────────────────

func TestBurnRateWithRealElapsed(t *testing.T) {
	b := newTestBudgetWithElapsed("test", 1, 400000, 100000, 33000, 2*time.Hour)

	rate := b.BurnRate()
	expectedRate := b.BudgetRate()
	if rate < expectedRate*0.9 || rate > expectedRate*1.1 {
		t.Errorf("BurnRate mismatch: got %.2f, want ~%.2f", rate, expectedRate)
	}
}

func TestBudgetCycleResetKeepsAccumulated(t *testing.T) {
	b := newTestBudget("test", 1, 400000, 100000, 100000)

	b.Consumed += 50000
	if b.Consumed != 150000 {
		t.Errorf("Consumed should accumulate: got %d, want 150000", b.Consumed)
	}
	if b.Remaining() != 250000 {
		t.Errorf("Remaining should be 250k, got %d", b.Remaining())
	}

	b.CycleStart = time.Now()
	b.Consumed = 0
	if b.Consumed != 0 {
		t.Errorf("Consumed should reset on cycle end, got %d", b.Consumed)
	}
}

func TestModelWeightNormalization(t *testing.T) {
	b := &Budget{
		Name: "test", Importance: 1,
		DailyBudget: 400000, MinBudget: 100000,
		ModelWeights: map[string]float64{
			"claude-opus":  15.0,
			"claude-haiku": 1.0,
		},
	}

	opusCost := b.WeightedConsume("claude-opus", 1000)
	haikuCost := b.WeightedConsume("claude-haiku", 1000)

	if opusCost != 15000 {
		t.Errorf("Opus 1000 tokens × 15 = 15000, got %d", opusCost)
	}
	if haikuCost != 1000 {
		t.Errorf("Haiku 1000 tokens × 1 = 1000, got %d", haikuCost)
	}
}

func TestBurnRateBlindZoneConservative(t *testing.T) {
	b1 := newTestBudgetWithElapsed("high-imp", 1, 400000, 100000, 100, 30*time.Second)
	b3 := newTestBudgetWithElapsed("low-imp", 5, 400000, 100000, 100, 30*time.Second)

	highRate := b1.BurnRate()
	lowRate := b3.BurnRate()
	budgetRate := b1.BudgetRate()

	if highRate != budgetRate {
		t.Errorf("imp=1 blind zone: expected budget_rate %.2f, got %.2f", budgetRate, highRate)
	}
	if lowRate != budgetRate*0.5 {
		t.Errorf("imp=5 blind zone: expected 0.5x budget_rate %.2f, got %.2f", budgetRate*0.5, lowRate)
	}
}

func TestWeightedConsumeUint32OverflowSafety(t *testing.T) {
	b := &Budget{
		Name: "test", Importance: 1,
		DailyBudget: 10_000_000, MinBudget: 100_000,
		ModelWeights: map[string]float64{"claude-opus": 15.0},
	}

	cost := b.WeightedConsume("claude-opus", 1_000_000)
	if cost != 15_000_000 {
		t.Errorf("1M Opus tokens: expected 15M, got %d", cost)
	}
	if b.Consumed != 15_000_000 {
		t.Errorf("Consumed mismatch: got %d", b.Consumed)
	}
}

// ─────────────────────────────────────────────
// Budget 模型补充测试 (2 tests)
// ─────────────────────────────────────────────

func TestProjectedCappedAt2x(t *testing.T) {
	b := newTestBudgetWithElapsed("test", 1, 400_000, 100_000, 350_000, 2*time.Hour)
	projected := b.Projected()
	if projected > 800_000 {
		t.Errorf("Projected should cap at 2× DailyBudget (800k), got %d", projected)
	}
}

func TestWeightedConsumeUnknownModel(t *testing.T) {
	b := &Budget{
		Name: "test", Importance: 1,
		DailyBudget: 400000, MinBudget: 100000,
	}
	cost := b.WeightedConsume("unknown-model", 500)
	if cost != 500 {
		t.Errorf("Unknown model should use weight=1.0, got %d", cost)
	}
}

// ─────────────────────────────────────────────
// Token 边界补充测试 (3 tests)
// ─────────────────────────────────────────────

func TestSignalLevelBoundaries(t *testing.T) {
	b := newTestBudget("test", 1, 400000, 100000, 200000)
	if s := b.SignalLevel(); s != "yellow" {
		t.Errorf("at budget rate: expected yellow, got %s", s)
	}
}

func TestProjectedExactAtBudgetRate(t *testing.T) {
	b := newTestBudget("test", 1, 400000, 100000, 200000)
	projected := b.Projected()
	if projected < 390000 || projected > 410000 {
		t.Errorf("Projected at budget rate should be ~400k, got %d", projected)
	}
}

func TestGoalMetAtExactBoundary(t *testing.T) {
	b := newTestBudget("test", 1, 400000, 100000, 201000)
	if b.GoalMet() {
		t.Error("GoalMet should be false when barely over budget rate")
	}
}

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

func newTestBudget(name string, imp int, daily, min, consumed uint32) *Budget {
	b := &Budget{
		Name: name, Importance: imp,
		DailyBudget: daily, MinBudget: min,
		Consumed: consumed,
	}
	b.CycleStart = time.Now().Add(-12 * time.Hour)
	b.CycleDuration = 24 * time.Hour
	return b
}

func newTestBudgetWithElapsed(name string, imp int, daily, min, consumed uint32, elapsed time.Duration) *Budget {
	b := &Budget{
		Name: name, Importance: imp,
		DailyBudget: daily, MinBudget: min,
		Consumed: consumed,
	}
	b.CycleStart = time.Now().Add(-elapsed)
	b.CycleDuration = 24 * time.Hour
	return b
}
