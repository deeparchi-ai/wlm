// Package control implements the PID-based resource controller.
// It translates performance metrics into cgroup weight adjustments,
// following the goal-oriented model of IBM z/OS WLM.
//
// Each Controller manages one service class. The Observe method reads
// metrics from cgroup/PSI. The PID then computes a proposed weight.
// The final weight is determined by the arbitrator, which considers
// all classes' importance levels.
package control

import (
	"fmt"
	"log"
	"math"
	"time"

	"github.com/deeparchi-ai/wlm/internal/arbitrator"
	"github.com/deeparchi-ai/wlm/internal/cgroup"
	"github.com/deeparchi-ai/wlm/internal/policy"
)

// Controller manages one service class's resource allocation loop.
type Controller struct {
	Class     policy.ServiceClass
	prevError float64
	integral  float64
	lastTime  time.Time
	lastCPU   *cgroup.CPUStat

	// Cached observation from last Observe()
	cachedPSI      *cgroup.PSIStats
	cachedError    float64
	cachedGoalMet  bool
	cachedWeight   uint32
}

// NewController creates a new PID controller for a service class.
func NewController(sc policy.ServiceClass) *Controller {
	return &Controller{
		Class:    sc,
		lastTime: time.Now(),
	}
}

// Observe reads current metrics and returns the PID-proposed weight.
// This does NOT write to cgroup — that's the arbitrator's job.
func (c *Controller) Observe() (proposedWeight uint32, err error) {
	now := time.Now()
	dt := now.Sub(c.lastTime).Seconds()
	if dt < 0.1 {
		return c.cachedWeight, fmt.Errorf("tick interval too short: %.2fs", dt)
	}

	// --- OBSERVE ---
	c.cachedPSI, err = cgroup.ReadPSI(c.Class.CgroupPath)
	if err != nil {
		return c.cachedWeight, fmt.Errorf("reading PSI: %w", err)
	}
	c.cachedWeight, err = cgroup.ReadCPUWeight(c.Class.CgroupPath)
	if err != nil {
		return c.cachedWeight, fmt.Errorf("reading cpu.weight: %w", err)
	}
	cpuStat, err := cgroup.ReadCPUStat(c.Class.CgroupPath)
	if err != nil {
		return c.cachedWeight, fmt.Errorf("reading cpu.stat: %w", err)
	}

	// --- COMPARE ---
	c.cachedError = c.computeError(c.cachedPSI, dt, cpuStat)
	c.cachedGoalMet = c.cachedError <= 0

	// --- PID CONTROL ---
	newWeight := c.computeWeight(c.cachedWeight, c.cachedError, dt)

	// Clamp to [MinWeight, MaxWeight] — local bounds only
	if newWeight < c.Class.MinWeight {
		newWeight = c.Class.MinWeight
	}
	if newWeight > c.Class.MaxWeight {
		newWeight = c.Class.MaxWeight
	}

	c.lastTime = now
	c.lastCPU = cpuStat

	return newWeight, nil
}

// State returns the arbitrator's view of this controller.
func (c *Controller) State(proposedWeight uint32) arbitrator.State {
	psi := 0.0
	if c.cachedPSI != nil {
		psi = c.cachedPSI.SomeAvg10
	}
	return arbitrator.State{
		Name:       c.Class.Name,
		Importance: c.Class.Importance,
		Proposed:   proposedWeight,
		MinWeight:  c.Class.MinWeight,
		MaxWeight:  c.Class.MaxWeight,
		GoalMet:    c.cachedGoalMet,
		Pressure:   psi,
	}
}

// Apply writes the final weight to cgroup and updates internal state.
func (c *Controller) Apply(finalWeight uint32) error {
	currentWeight := c.cachedWeight
	if finalWeight != currentWeight {
		if err := cgroup.WriteCPUWeight(c.Class.CgroupPath, finalWeight); err != nil {
			return fmt.Errorf("writing cpu.weight: %w", err)
		}
	}

	if finalWeight != currentWeight {
		log.Printf("[%s] PSI=%.2f%% weight: %d→%d (goal_met=%v, error=%.3f)",
			c.Class.Name, c.cachedPSI.SomeAvg10, currentWeight, finalWeight,
			c.cachedGoalMet, c.cachedError)
	}

	c.cachedWeight = finalWeight
	return nil
}

// computeError calculates how far from the goal we are.
func (c *Controller) computeError(psi *cgroup.PSIStats, dt float64, _ *cgroup.CPUStat) float64 {
	switch c.Class.Goal.Type {
	case policy.GoalVelocity:
		return psi.SomeAvg10 - 10.0

	case policy.GoalResponseTime:
		dur, err := c.Class.Goal.Duration()
		if err != nil {
			return psi.SomeAvg10 - 5.0
		}
		targetPSI := 10.0
		if dur < time.Second {
			targetPSI = 3.0
		} else if dur < 5*time.Second {
			targetPSI = 5.0
		}
		return psi.SomeAvg10 - targetPSI

	case policy.GoalThroughput:
		return psi.SomeAvg10 - 8.0

	default:
		return psi.SomeAvg10 - 10.0
	}
}

// computeWeight applies a PI controller to adjust cpu.weight.
// When the system is under no pressure (error <= 0), we hold steady.
func (c *Controller) computeWeight(currentWeight uint32, error, dt float64) uint32 {
	const Kp = 15.0
	const Ki = 1.0

	if error > 0 {
		c.integral += error * dt
	} else {
		c.integral *= 0.9
	}

	if c.integral > 500 {
		c.integral = 500
	}
	if c.integral < -100 {
		c.integral = -100
	}

	adjustment := Kp*error + Ki*c.integral
	newWeight := float64(currentWeight) + adjustment
	return uint32(math.Round(newWeight))
}
