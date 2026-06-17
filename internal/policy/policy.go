// Package policy defines the WLM service policy model.
// A policy maps business goals (response time, throughput) to resource
// allocation rules, following the IBM z/OS WLM service class model.
package policy

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// GoalType defines the kind of performance objective.
type GoalType string

const (
	GoalResponseTime GoalType = "response_time" // p99 latency target
	GoalThroughput   GoalType = "throughput"    // rate target
	GoalVelocity     GoalType = "velocity"      // best-effort, don't starve
	GoalTokenBudget  GoalType = "token_budget"  // token budget per cycle
)

// ServiceClass defines a workload with a performance goal and importance.
// This maps directly to a z/OS WLM service class + cgroup.
type ServiceClass struct {
	Name       string        `yaml:"name"`
	CgroupPath string        `yaml:"cgroup"`
	Goal       GoalSpec      `yaml:"goal"`
	Importance int           `yaml:"importance"` // 1=highest, 5=lowest
	MinWeight  uint32        `yaml:"min_weight"` // floor for cpu.weight
	MaxWeight  uint32        `yaml:"max_weight"` // ceiling for cpu.weight

	// Token-specific fields (only used when goal.type == "token_budget")
	DailyBudget  uint32             `yaml:"daily_budget,omitempty"`  // max weighted tokens per budget cycle
	MinBudget    uint32             `yaml:"min_budget,omitempty"`    // floor — cannot be taken below this
	ModelWeights map[string]float64 `yaml:"model_weights,omitempty"` // model → cost multiplier
}

// GoalSpec defines one performance objective.
type GoalSpec struct {
	Type   GoalType `yaml:"type"`
	Target string   `yaml:"target"` // e.g. "2s" or "100/min"
}

// Duration parses the target as a time.Duration for response_time goals.
func (g GoalSpec) Duration() (time.Duration, error) {
	if g.Type != GoalResponseTime {
		return 0, fmt.Errorf("Duration() only valid for response_time goals, got %s", g.Type)
	}
	return time.ParseDuration(g.Target)
}

// Policy is the top-level WLM configuration.
type Policy struct {
	ServiceClasses []ServiceClass `yaml:"service_classes"`
	BudgetCycle    string         `yaml:"budget_cycle,omitempty"` // "24h" (default), budget tracking period
}

// Load reads a policy from a YAML file.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading policy: %w", err)
	}
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing policy: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks the policy for consistency.
func (p *Policy) Validate() error {
	if len(p.ServiceClasses) == 0 {
		return fmt.Errorf("at least one service class is required")
	}
	for _, sc := range p.ServiceClasses {
		if sc.Name == "" {
			return fmt.Errorf("service class name is required")
		}
		if sc.CgroupPath == "" {
			return fmt.Errorf("cgroup path is required for %q", sc.Name)
		}
		if sc.Importance < 1 || sc.Importance > 5 {
			return fmt.Errorf("importance must be 1-5 for %q", sc.Name)
		}
		if sc.Goal.Type != GoalResponseTime && sc.Goal.Type != GoalThroughput && sc.Goal.Type != GoalVelocity && sc.Goal.Type != GoalTokenBudget {
			return fmt.Errorf("unsupported goal type %q for %q", sc.Goal.Type, sc.Name)
		}
	}
	return nil
}
