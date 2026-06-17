package token

import (
	"os"
	"testing"

	"github.com/deeparchi-ai/wlm/internal/arbitrator"
)

func setupHermesTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	TestStateDir = dir
	TestCountersPath = dir + "/token_counters.jsonl"
	t.Cleanup(func() {
		TestStateDir = ""
		TestCountersPath = ""
	})
}

func TestBeforeCallSafetyFactor(t *testing.T) {
	setupHermesTest(t)

	arch := newTestBudget("arch", 1, 400000, 100000, 100000)
	budgets := []*Budget{arch}
	states := []arbitrator.State{arch.ToArbitratorState()}
	decisions := arbitrator.Arbitrate(states)
	WriteState(budgets, decisions)

	result, err := BeforeCall("arch", "claude-haiku", 1000)
	if err != nil {
		t.Fatalf("BeforeCall failed: %v", err)
	}
	if !result.Allowed {
		t.Error("1500 cost should be allowed with sufficient budget")
	}
	if result.CostEstimate != 1500 {
		t.Errorf("cost should be 1000×1.0×1.5=1500, got %d", result.CostEstimate)
	}

	result, _ = BeforeCall("arch", "claude-haiku", 500000)
	if result.Allowed {
		t.Error("large cost should be rejected")
	}
}

func TestBeforeCallModelWeightSafetyFactor(t *testing.T) {
	setupHermesTest(t)

	arch := newTestBudget("arch", 1, 400000, 100000, 50000)
	arch.ModelWeights = map[string]float64{"claude-opus": 15.0}
	budgets := []*Budget{arch}
	states := []arbitrator.State{arch.ToArbitratorState()}
	decisions := arbitrator.Arbitrate(states)
	WriteState(budgets, decisions)

	result, _ := BeforeCall("arch", "claude-opus", 10)
	if result.CostEstimate != 225 {
		t.Errorf("Opus safety cost: expected 225, got %d", result.CostEstimate)
	}
}

func TestBeforeCallClassNotFound(t *testing.T) {
	setupHermesTest(t)
	_, err := BeforeCall("nonexistent", "claude-haiku", 100)
	if err == nil {
		t.Error("BeforeCall should return error for unknown class")
	}
}

func TestAfterCallAppendsCounter(t *testing.T) {
	setupHermesTest(t)
	os.Remove(TestCountersPath)

	weights := map[string]float64{"claude-haiku": 1.0}
	err := AfterCall("arch", "claude-haiku", 500, weights)
	if err != nil {
		t.Fatalf("AfterCall failed: %v", err)
	}

	entries, _ := ReadAndClearCounters()
	if len(entries) != 1 {
		t.Fatalf("expected 1 counter entry, got %d", len(entries))
	}
	if entries[0].Class != "arch" || entries[0].Tokens != 500 {
		t.Errorf("counter mismatch: class=%s tokens=%d", entries[0].Class, entries[0].Tokens)
	}
}
