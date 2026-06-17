// wlmd is the WLM daemon — a goal-oriented workload manager.
//
// Architecture:
//
//	Observe → Arbitrate → Apply
//
// Modes:
//
//	cpu   (default): CPU controller via cgroup v2 + PSI
//	token: Token budget controller via counters + shared state
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/deeparchi-ai/wlm/internal/arbitrator"
	"github.com/deeparchi-ai/wlm/internal/cgroup"
	"github.com/deeparchi-ai/wlm/internal/control"
	"github.com/deeparchi-ai/wlm/internal/policy"
	"github.com/deeparchi-ai/wlm/internal/token"
)

var sigCh = make(chan os.Signal, 1)

func main() {
	policyPath := flag.String("policy", "policy.yaml", "path to service policy YAML")
	interval := flag.Duration("interval", 10*time.Second, "control loop interval")
	mode := flag.String("mode", "cpu", "control mode: cpu | token")
	debug := flag.Bool("debug", false, "print proposed weights before arbitration")
	flag.Parse()

	pol, err := policy.Load(*policyPath)
	if err != nil {
		log.Fatalf("failed to load policy: %v", err)
	}
	log.Printf("loaded policy with %d service classes, mode=%s", len(pol.ServiceClasses), *mode)

	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	switch *mode {
	case "token":
		runTokenLoop(pol, *interval)
	default:
		runCPULoop(pol, *interval, *debug)
	}
}

func runCPULoop(pol *policy.Policy, interval time.Duration, debug bool) {
	for _, sc := range pol.ServiceClasses {
		if err := cgroup.EnsurePath(sc.CgroupPath); err != nil {
			log.Printf("WARNING: cannot create cgroup %s: %v", sc.CgroupPath, err)
		}
	}

	controllers := make([]*control.Controller, len(pol.ServiceClasses))
	for i, sc := range pol.ServiceClasses {
		controllers[i] = control.NewController(sc)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("CPU mode started, interval=%v", interval)

	for {
		select {
		case <-sigCh:
			log.Println("shutting down")
			return

		case <-ticker.C:
			states := make([]arbitrator.State, len(controllers))
			for i, ctrl := range controllers {
				prop, err := ctrl.Observe()
				if err != nil {
					log.Printf("[%s] observe error: %v", ctrl.Class.Name, err)
					continue
				}
				states[i] = ctrl.State(prop)
				if debug {
					log.Printf("[debug] %s: proposed=%d goal_met=%v psi=%.2f min=%d max=%d",
						ctrl.Class.Name, prop, states[i].GoalMet, states[i].Pressure,
						states[i].MinWeight, states[i].MaxWeight)
				}
			}

			decisions := arbitrator.Arbitrate(states)

			decisionMap := make(map[string]arbitrator.Decision)
			for _, d := range decisions {
				decisionMap[d.Name] = d
			}

			for _, ctrl := range controllers {
				dec, ok := decisionMap[ctrl.Class.Name]
				if !ok {
					log.Printf("[%s] no arbitration decision, skipping", ctrl.Class.Name)
					continue
				}
				if err := ctrl.Apply(dec.Final); err != nil {
					log.Printf("[%s] apply error: %v", ctrl.Class.Name, err)
				}
			}
		}
	}
}

func runTokenLoop(pol *policy.Policy, interval time.Duration) {
	cycleDuration, err := time.ParseDuration(pol.BudgetCycle)
	if err != nil {
		cycleDuration = 24 * time.Hour
	}

	budgets := make([]*token.Budget, len(pol.ServiceClasses))
	for i, sc := range pol.ServiceClasses {
		budgets[i] = &token.Budget{
			Name:          sc.Name,
			Importance:    sc.Importance,
			DailyBudget:   sc.DailyBudget,
			MinBudget:     sc.MinBudget,
			ModelWeights:  sc.ModelWeights,
			CycleStart:    time.Now(),
			CycleDuration: cycleDuration,
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("token mode started, cycle=%v, interval=%v, classes=%d",
		cycleDuration, interval, len(budgets))

	for {
		select {
		case <-sigCh:
			log.Println("shutting down token mode")
			return
		case <-ticker.C:
			// Phase 1: Observe
			entries, err := token.ReadAndClearCounters()
			if err != nil {
				log.Printf("read counters error: %v", err)
			}
			consumedByClass := token.AggregateCounters(entries)
			for _, b := range budgets {
				if consumed, ok := consumedByClass[b.Name]; ok {
					b.Consumed += uint32(consumed)
				}
			}

			// Phase 2: Arbitrate
			states := make([]arbitrator.State, len(budgets))
			for i, b := range budgets {
				states[i] = b.ToArbitratorState()
			}

			decisions := arbitrator.Arbitrate(states)

			// Phase 3: Apply
			if err := token.WriteState(budgets, decisions); err != nil {
				log.Printf("write state error: %v", err)
			}

			// Phase 4: Housekeeping — cycle reset
			now := time.Now()
			for _, b := range budgets {
				if now.After(b.CycleStart.Add(b.CycleDuration)) {
					b.CycleStart = now
					b.Consumed = 0
					log.Printf("[%s] budget cycle reset", b.Name)
				}
			}

			for _, d := range decisions {
				if d.WeightDelta != 0 {
					for _, b := range budgets {
						if b.Name == d.Name {
							log.Printf("[%s] imp=%d projected=%d→%d signal=%s consumed=%d/%d",
								d.Name, d.Importance, d.Proposed, d.Final,
								b.SignalLevel(), b.Consumed, b.DailyBudget)
							break
						}
					}
				}
			}
		}
	}
}
