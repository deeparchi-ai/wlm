package token

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/deeparchi-ai/wlm/internal/arbitrator"
)

// ─────────────────────────────────────
// token_state.json — wlmd writes, Hermes reads
// ─────────────────────────────────────

const (
	stateDir  = "/var/run/wlm"
	statePath = "/var/run/wlm/token_state.json"
)

// Test overrides — exported for observer_test.go
var (
	TestStateDir  string // if set, overrides stateDir for tests
	TestCountersPath string
)

func getStateDir() string {
	if TestStateDir != "" {
		return TestStateDir
	}
	return stateDir
}

func getStatePath() string {
	if TestStateDir != "" {
		return TestStateDir + "/token_state.json"
	}
	return statePath
}

func getCountersPath() string {
	if TestCountersPath != "" {
		return TestCountersPath
	}
	return countersPath
}

// SharedState is the arbitration output written by wlmd.
type SharedState struct {
	UpdatedAt time.Time    `json:"updated_at"`
	Classes   []ClassState `json:"classes"`
}

// ClassState is one entry in the shared state.
type ClassState struct {
	Name              string             `json:"name"`
	Importance        int                `json:"importance"`
	AvailableBudget   uint32             `json:"available_budget"`
	ConsumedThisCycle uint32             `json:"consumed_this_cycle"`
	SignalLevel       string             `json:"signal_level"`
	ModelWeights      map[string]float64 `json:"model_weights"`
	CycleEndsAt       time.Time          `json:"cycle_ends_at"`
}

// WriteState atomically writes arbitration results. Only called by wlmd.
func WriteState(budgets []*Budget, decisions []arbitrator.Decision) error {
	decMap := make(map[string]arbitrator.Decision)
	for _, d := range decisions {
		decMap[d.Name] = d
	}

	state := SharedState{UpdatedAt: time.Now()}
	for _, b := range budgets {
		dec, ok := decMap[b.Name]
		available := b.Remaining()
		if ok {
			available = b.AvailableAfter(dec)
		}

		state.Classes = append(state.Classes, ClassState{
			Name:              b.Name,
			Importance:        b.Importance,
			AvailableBudget:   available,
			ConsumedThisCycle: b.Consumed,
			SignalLevel:       b.SignalLevel(),
			ModelWeights:      b.ModelWeights,
			CycleEndsAt:       b.CycleStart.Add(b.CycleDuration),
		})
	}

	if err := os.MkdirAll(getStateDir(), 0755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	tmpPath := getStatePath() + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("writing temp state: %w", err)
	}
	return os.Rename(tmpPath, getStatePath())
}

// ReadState reads the shared state. Used by Hermes BeforeCall.
func ReadState() (*SharedState, error) {
	data, err := os.ReadFile(getStatePath())
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}
	var state SharedState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing state: %w", err)
	}
	return &state, nil
}

// ─────────────────────────────────────
// token_counters.jsonl — Hermes appends, wlmd aggregates
// ─────────────────────────────────────

const countersPath = "/var/run/wlm/token_counters.jsonl"

// CounterEntry is one line in the counters file.
type CounterEntry struct {
	Class  string  `json:"class"`
	Model  string  `json:"model"`
	Tokens uint32  `json:"tokens"`
	Weight float64 `json:"weight"`
	TS     int64   `json:"ts"`
}

// AppendCounter appends one consumption event. Uses O_APPEND for atomic writes.
func AppendCounter(className, model string, tokens uint32, modelWeights map[string]float64) error {
	weight := 1.0
	if w, ok := modelWeights[model]; ok {
		weight = w
	}

	entry := CounterEntry{
		Class:  className,
		Model:  model,
		Tokens: tokens,
		Weight: float64(tokens) * weight,
		TS:     time.Now().UnixNano(),
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling counter: %w", err)
	}
	line = append(line, '\n')

	if err := os.MkdirAll(getStateDir(), 0755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}

	f, err := os.OpenFile(getCountersPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening counters: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("writing counter: %w", err)
	}
	return nil
}

// ReadAndClearCounters atomically reads all counter entries using rename.
//  1. Rename counters.jsonl → counters.jsonl.processing
//  2. Read and parse the processing file
//  3. New writes from Hermes (O_APPEND|O_CREAT) go to a fresh counters.jsonl
func ReadAndClearCounters() ([]CounterEntry, error) {
	processingPath := getCountersPath() + ".processing"

	if err := os.Rename(getCountersPath(), processingPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("renaming counters: %w", err)
	}

	data, err := os.ReadFile(processingPath)
	os.Remove(processingPath)
	if err != nil {
		return nil, fmt.Errorf("reading counters: %w", err)
	}

	var entries []CounterEntry
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var e CounterEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// AggregateCounters sums weighted tokens per class.
func AggregateCounters(entries []CounterEntry) map[string]uint64 {
	agg := make(map[string]uint64)
	for _, e := range entries {
		agg[e.Class] += uint64(e.Weight)
	}
	return agg
}
