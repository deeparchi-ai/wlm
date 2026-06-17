package token

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/deeparchi-ai/wlm/internal/arbitrator"
)

func setupTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	TestStateDir = dir
	TestCountersPath = filepath.Join(dir, "token_counters.jsonl")
	t.Cleanup(func() {
		TestStateDir = ""
		TestCountersPath = ""
	})
	return dir
}

func TestWriteReadStateRoundtrip(t *testing.T) {
	setupTestDir(t)

	budgets := []*Budget{
		newTestBudget("arch", 1, 400000, 100000, 50000),
		newTestBudget("code", 3, 300000, 50000, 100000),
	}

	states := []arbitrator.State{
		budgets[0].ToArbitratorState(),
		budgets[1].ToArbitratorState(),
	}
	decisions := arbitrator.Arbitrate(states)

	err := WriteState(budgets, decisions)
	if err != nil {
		t.Fatalf("WriteState failed: %v", err)
	}

	state, err := ReadState()
	if err != nil {
		t.Fatalf("ReadState failed: %v", err)
	}
	if len(state.Classes) != 2 {
		t.Errorf("expected 2 classes, got %d", len(state.Classes))
	}
	if state.Classes[0].Name != "arch" {
		t.Errorf("first class should be arch, got %s", state.Classes[0].Name)
	}
}

func TestAppendAndReadCounters(t *testing.T) {
	setupTestDir(t)

	weights := map[string]float64{"claude-opus": 15.0}
	AppendCounter("arch", "claude-opus", 1000, weights)
	AppendCounter("arch", "claude-opus", 500, weights)

	entries, err := ReadAndClearCounters()
	if err != nil {
		t.Fatalf("ReadAndClearCounters failed: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}

	entries2, _ := ReadAndClearCounters()
	if len(entries2) != 0 {
		t.Errorf("counters should be empty after clear, got %d", len(entries2))
	}
}

func TestAggregateCountersMultiClass(t *testing.T) {
	entries := []CounterEntry{
		{Class: "arch", Weight: 15000},
		{Class: "code", Weight: 3000},
		{Class: "arch", Weight: 7500},
	}
	agg := AggregateCounters(entries)
	if agg["arch"] != 22500 {
		t.Errorf("arch total: expected 22500, got %d", agg["arch"])
	}
	if agg["code"] != 3000 {
		t.Errorf("code total: expected 3000, got %d", agg["code"])
	}
}

func TestReadAndClearCountersEmptyDir(t *testing.T) {
	setupTestDir(t)
	// Counters path is set but file doesn't exist yet
	os.Remove(TestCountersPath)
	entries, err := ReadAndClearCounters()
	if err != nil {
		t.Errorf("empty counters should return nil error, got %v", err)
	}
	if entries != nil {
		t.Errorf("empty counters should return nil entries, got %d", len(entries))
	}
}

func TestWriteStateCreatesDir(t *testing.T) {
	dir := t.TempDir()
	// Use a subdirectory that doesn't exist yet
	TestStateDir = filepath.Join(dir, "newsubdir")
	TestCountersPath = filepath.Join(dir, "counters.jsonl")
	t.Cleanup(func() {
		TestStateDir = ""
		TestCountersPath = ""
	})

	budgets := []*Budget{newTestBudget("test", 1, 400000, 100000, 0)}
	states := []arbitrator.State{budgets[0].ToArbitratorState()}
	decisions := arbitrator.Arbitrate(states)

	err := WriteState(budgets, decisions)
	if err != nil {
		t.Fatalf("WriteState failed: %v", err)
	}

	if _, err := os.Stat(TestStateDir); os.IsNotExist(err) {
		t.Error("stateDir was not created")
	}
}
