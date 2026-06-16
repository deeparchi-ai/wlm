// Package control implements the PID-based resource controller.
// It translates performance metrics into cgroup weight adjustments,
// following the goal-oriented model of IBM z/OS WLM.
package control

import (
	"fmt"
	"log"
	"math"
	"time"

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
}

// NewController creates a new PID controller for a service class.
func NewController(sc policy.ServiceClass) *Controller {
	return &Controller{
		Class:   sc,
		lastTime: time.Now(),
	}
}

// Tick runs one control loop iteration: observe → compare → actuate.
// It returns the new cpu.weight that was written.
func (c *Controller) Tick() (uint32, error) {
	now := time.Now()
	dt := now.Sub(c.lastTime).Seconds()
	if dt < 0.1 {
		return 0, fmt.Errorf("tick interval too short: %.2fs", dt)
	}

	// --- OBSERVE ---
	psi, err := cgroup.ReadPSI(c.Class.CgroupPath)
	if err != nil {
		return 0, fmt.Errorf("reading PSI: %w", err)
	}
	currentWeight, err := cgroup.ReadCPUWeight(c.Class.CgroupPath)
	if err != nil {
		return 0, fmt.Errorf("reading cpu.weight: %w", err)
	}
	cpuStat, err := cgroup.ReadCPUStat(c.Class.CgroupPath)
	if err != nil {
		return 0, fmt.Errorf("reading cpu.stat: %w", err)
	}

	// --- COMPARE ---
	error := c.computeError(psi, dt, cpuStat)

	// --- PID CONTROL ---
	newWeight := c.computeWeight(currentWeight, error, dt)

	// Clamp to [MinWeight, MaxWeight]
	if newWeight < c.Class.MinWeight {
		newWeight = c.Class.MinWeight
	}
	if newWeight > c.Class.MaxWeight {
		newWeight = c.Class.MaxWeight
	}

	// --- ACTUATE ---
	if newWeight != currentWeight {
		if err := cgroup.WriteCPUWeight(c.Class.CgroupPath, newWeight); err != nil {
			return currentWeight, fmt.Errorf("writing cpu.weight: %w", err)
		}
	}

	// Update state
	c.prevError = error
	c.lastTime = now
	c.lastCPU = cpuStat

	if newWeight != currentWeight {
		log.Printf("[%s] PSI=%.2f%% weight: %d→%d (error=%.3f)",
			c.Class.Name, psi.SomeAvg10, currentWeight, newWeight, error)
	}

	return newWeight, nil
}

// computeError calculates how far from the goal we are.
// For velocity goals: error is based on CPU pressure (PSI).
// For response_time goals: error would use external latency metrics.
func (c *Controller) computeError(psi *cgroup.PSIStats, dt float64, _ *cgroup.CPUStat) float64 {
	switch c.Class.Goal.Type {
	case policy.GoalVelocity:
		// Velocity: keep PSI below 10% avg10
		// Positive error = under pressure (need more resources)
		return psi.SomeAvg10 - 10.0

	case policy.GoalResponseTime:
		// Response time: use PSI as a proxy until external metrics arrive
		// Lower PSI = better response times
		dur, err := c.Class.Goal.Duration()
		if err != nil {
			return psi.SomeAvg10 - 5.0
		}
		// Rough heuristic: target 5% PSI for sub-second, 10% for multi-second
		targetPSI := 10.0
		if dur < time.Second {
			targetPSI = 3.0
		} else if dur < 5*time.Second {
			targetPSI = 5.0
		}
		return psi.SomeAvg10 - targetPSI

	case policy.GoalThroughput:
		// Throughput: PSI as inverse proxy — lower PSI = better throughput
		return psi.SomeAvg10 - 8.0

	default:
		return psi.SomeAvg10 - 10.0
	}
}

// computeWeight applies a PI (proportional-integral) controller to
// adjust cpu.weight based on the error signal.
// When the system is under no pressure (error <= 0), we hold steady.
func (c *Controller) computeWeight(currentWeight uint32, error, dt float64) uint32 {
	// PI constants
	const Kp = 15.0 // proportional gain: 1% PSI error → 15 weight units
	const Ki = 1.0  // integral gain: accumulates slowly

	// Only accumulate integral when under pressure (positive error)
	// When error <= 0, the goal is met — don't wind down
	if error > 0 {
		c.integral += error * dt
	} else {
		// Slowly decay integral when pressure is gone
		c.integral *= 0.9
	}

	// Anti-windup: clamp integral
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
