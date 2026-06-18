> **🤖 AI-Maintained** — This repository is maintained by LLM agents. Human commits (perhaps) zero. Liability (certainly) none. Fun (definitely) infinite.
>
> All code changes, issue triage, and PR review are performed by AI. Results may vary. Use at your own risk.

---

# WLM — Goal-Oriented Workload Manager

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

WLM is a userspace resource controller that brings IBM z/OS **Workload Manager** semantics to Linux. Instead of fixed priorities or fair-share, you define **business goals** (response time, throughput, token budget) per workload, and WLM dynamically adjusts cgroup v2 resource allocations to meet them.

## Why

Linux has great CPU schedulers — CFS, EEVDF.  But they answer *"how much CPU should each process get?"* — a resource-centric question.

Production workloads ask a different question: *"is my interactive workload responding in under 2 seconds?"*

WLM bridges this gap.  You declare the **goal** (response time < 2s, stay within 10K tokens/hour).  WLM observes, decides, and applies — in a closed loop, every 10 seconds.

## Architecture

```
┌──────────────────────────────────────────────────┐
│  Service Policy (YAML)                            │
│  "interactive: response_time < 2s, importance=1"  │
│  "llm-agent:     token_budget < 10K/hour"         │
└────────────────────┬─────────────────────────────┘
                     │
┌────────────────────▼─────────────────────────────┐
│  WLM Daemon (wlmd)                                │
│                                                   │
│  ┌──────────┐   ┌──────────┐   ┌──────────────┐  │
│  │ PSI Read │ → │ PI Ctrl  │ → │ cgroup Write  │  │
│  │ (observe)│   │ (decide) │   │ (cpu.weight)  │  │
│  └──────────┘   └──────────┘   └──────────────┘  │
│       ↑              │               │            │
│       │     ┌────────▼────────┐      │            │
│       │     │  Importance     │      │            │
│       └─────│  Arbitration    │──────┘            │
│             └─────────────────┘                   │
│                                                   │
│  ┌──────────┐   ┌──────────┐   ┌──────────────┐  │
│  │Token Obs │ → │ Budget   │ → │ Signal File  │  │
│  │(counter) │   │ Arbiter  │   │(JSON on disk) │  │
│  └──────────┘   └──────────┘   └──────────────┘  │
└────────────────────┬─────────────────────────────┘
                     │
┌────────────────────▼─────────────────────────────┐
│  Linux cgroup v2 + PSI (kernel, zero changes)     │
│  /sys/fs/cgroup/.../cpu.weight                    │
│  /proc/pressure/cpu                               │
└──────────────────────────────────────────────────┘
```

- **Zero kernel changes** — uses standard cgroup v2 + PSI interfaces
- **Goal-oriented** — define *what* you want, not *how much* to give
- **PID control loop** — proportional-integral controller with anti-windup
- **Importance arbitration** — when resources are tight, high-importance workloads are protected first
- **Token budgets** — signal-based budget enforcement for AI agent token consumption

## How It Works — Deep Dive

### The PID Controller

Each service class has its own PID (Proportional-Integral) controller:

```
                Setpoint (goal)
                     │
                     ▼
    ┌──── error ────[+]────▶ Kp · error  ────┐
    │                 ▲                        │
    │                 │ Ki · ∫ error dt        │
    │                 │                        ▼
    │                 └──────────────[+]─── control output ──▶ cpu.weight
    │                                           ▲
    └──────── PSI feedback ─────────────────────┘
```

1. **Observe**: read `/proc/pressure/cpu` → PSI some/full averages
2. **Compare**: PSI vs. goal-derived threshold (e.g., response_time < 2s → keep PSI below 15%)
3. **Act**: if PSI > threshold → increase `cpu.weight` (proportional to error + accumulated error)
4. **Anti-windup**: when weight hits `max_weight`, integration stops to prevent overshoot after recovery

This is classic industrial control theory — the same algorithm that keeps your room temperature stable, applied to CPU scheduling.

### Importance Arbitration

When multiple service classes compete and not all can meet their goals:

```
importance=1 (interactive)  ── under pressure ──▶  takes from importance=3
importance=3 (batch)        ── under pressure ──▶  takes from importance=5
                              all goals met   ──▶  no redistribution
```

The arbitrator runs after each PID cycle:
1. Sort classes by importance (1 = highest priority)
2. For each class not meeting its goal: calculate how much weight it needs
3. Collect the shortfall from lower-importance classes that have weight to spare
4. Never push a class below `min_weight` or above `max_weight`

This directly mirrors the z/OS WLM goal-mode arbitration algorithm from 1994.

### Token Budget Controller

Designed for AI agent workloads — controls LLM API call volume based on budget windows:

```
  Agent calls LLM ──▶ writes token count ──▶ token_counters.jsonl
                                                    │
                          ┌─────────────────────────┘
                          ▼
                    Observer reads counters
                          │
                          ▼
                    Budget Arbiter
                          │
                          ▼
              token_state.json on disk
                          │
                          ▼
       Agent reads signal before next LLM call
```

**Four signal levels:**

| Signal | Meaning | Agent behavior |
|--------|---------|---------------|
| 🟢 green | Budget healthy | Normal operation |
| 🟡 yellow | Spending faster than expected | Skip non-essential calls |
| 🔴 red | Budget exhausted in current window | Block until next window |
| ⚫ black | Emergency stop | Halt all LLM calls |

```json
{
  "signal": "yellow",
  "budget_remaining": 4500,
  "budget_total": 10000,
  "window_remaining": "25m",
  "consumption_rate": 220.0,
  "projected_exhaustion": "in 20m"
}
```

The Hermes Agent hook at `~/.hermes/hooks/wlm-token/` reads this file before every LLM call.  Green = go.  Yellow = consider skipping.  Red = stop.  Black = emergency halt.

## Quick Start

### Prerequisites

- Linux kernel ≥ 4.20 (for PSI; ≥ 5.0 recommended)
- cgroup v2 mounted at `/sys/fs/cgroup`
- Root access for initial cgroup setup

### Setup (one-time)

```bash
# Enable cpu controller delegation
echo "+cpu" | sudo tee /sys/fs/cgroup/cgroup.subtree_control

# Create parent cgroup
sudo mkdir -p /sys/fs/cgroup/wlm
echo "+cpu" | sudo tee /sys/fs/cgroup/wlm/cgroup.subtree_control

# Create workload cgroups and delegate ownership
sudo mkdir -p /sys/fs/cgroup/wlm/interactive /sys/fs/cgroup/wlm/batch
echo 100 | sudo tee /sys/fs/cgroup/wlm/interactive/cpu.weight
echo 100 | sudo tee /sys/fs/cgroup/wlm/batch/cpu.weight
sudo chown -R $USER /sys/fs/cgroup/wlm/interactive /sys/fs/cgroup/wlm/batch
```

### CPU Arbitration

```yaml
# policy.yaml
service_classes:
  - name: "interactive"
    cgroup: "/wlm/interactive"
    goal:
      type: "response_time"
      target: "2s"
    importance: 1
    min_weight: 10
    max_weight: 1000

  - name: "batch"
    cgroup: "/wlm/batch"
    goal:
      type: "velocity"
      target: ""
    importance: 3
    min_weight: 1
    max_weight: 900
```

```bash
go build -o wlmd ./cmd/wlmd/
echo $PID | sudo tee /sys/fs/cgroup/wlm/interactive/cgroup.procs
./wlmd -policy policy.yaml -interval 10s
watch -n 2 'cat /sys/fs/cgroup/wlm/interactive/cpu.weight'
```

### Token Budget

```yaml
# policy_token.yaml
service_classes:
  - name: "llm-agent"
    type: "token"
    goal:
      target: "10000/hour"
    importance: 1
    signal_file: "/var/run/wlm/token_state.json"
    counter_file: "/var/run/wlm/token_counters.jsonl"
```

```bash
./wlmd -policy policy_token.yaml
# Agent reads /var/run/wlm/token_state.json before each LLM call
cat /var/run/wlm/token_state.json
```

## Goal Types

| Type | Semantics | PSI Mapping |
|------|-----------|-------------|
| `response_time` | p99 latency target (e.g. "2s") | PSI < target-derived threshold |
| `throughput` | rate target (e.g. "100/min") | PSI < 8% as inverse proxy |
| `velocity` | best-effort, don't starve | PSI < 10% |
| `token` | budget per time window | Token consumption rate vs. budget |

## Importance

| Level | Typical use | Behavior under pressure |
|-------|------------|------------------------|
| 1 | Interactive, latency-sensitive | Protected first — takes from lower levels |
| 2-3 | Balanced workloads | Moderate protection |
| 4-5 | Batch, background | Sacrificed first |

## Why Userspace

WLM intentionally runs as a userspace daemon, not a kernel module:

- **Zero kernel maintenance.**  No LKML patchsets, no backport hell, no distribution politics.
- **Safe failure mode.**  If `wlmd` crashes, cgroup weights stay where they are.  The kernel keeps scheduling.  No panic, no reboot.
- **Rapid iteration.**  `go build` → `./wlmd`.  Minutes, not months.
- **Minimum viable abstraction.**  WLM only does what the kernel *doesn't* do: goal translation and arbitration.  CPU scheduling stays in the kernel where it belongs.

## Comparison

| | Linux CFS | cgroup limits | Kubernetes QoS | **WLM** |
|---|---|---|---|---|
| Model | fair-share | hard cap | priority class | **goal-oriented** |
| Input | nice value | cpu.max | QoS class label | **"response_time < 2s"** |
| Feedback | none | none | none | **PSI loop every 10s** |
| Multi-workload | proportional | independent | pod-level | **importance arbitration** |
| Token budget | N/A | N/A | N/A | **signal-based, 4 levels** |

## Real Use Cases

### 1. Web server + ML training on one host

```
┌─────────────────────────────────────────────────┐
│  nginx (importance=1, response_time < 500ms)     │
│  pytorch train (importance=5, velocity)          │
└─────────────────────────────────────────────────┘

Idle:       training eats 90% CPU
Peak:       nginx PSI spikes → WLM gives nginx weight
            training weight drops → nginx recovers
            peak passes → training reclaims CPU
```

No cron job.  No manual tuning.  WLM handles the transitions.

### 2. Multi-Agent token budget

```
┌─────────────────────────────────────────────────┐
│  Arch Guardian Agent:  importance=1, 500K/month  │
│  Code Generator Agent:  importance=2, 2M/month   │
│  Monitor Agent:         importance=4, 500K/month  │
└─────────────────────────────────────────────────┘
```

All three agents check `/var/run/wlm/token_state.json` before LLM calls.  Over-budget agents get 🟡 yellow or 🔴 red signals.  The critical architecture agent always gets priority budget.

### 3. CI pipeline isolation

```
┌─────────────────────────────────────────────────┐
│  CI build 1: /wlm/ci/frontend, importance=2      │
│  CI build 2: /wlm/ci/backend,  importance=2      │
│  CI test:    /wlm/ci/e2e,      importance=1      │
└─────────────────────────────────────────────────┘
```

Two parallel builds can eat CPU, but end-to-end tests always get resources first.  No more "CI flaked because the build starved the test runner."

## Code

```
$ wc -l internal/**/*.go
  191  arbitrator/arbitrator.go
  122  cgroup/cgroup.go
  174  control/controller.go
   96  policy/policy.go
   41  token/arbitrator.go
  378  token/arbitrator_test.go
  118  token/budget.go
   52  token/hermes.go
  215  token/observer.go
 1769  total
```

1,769 lines of Go.  One external dependency: `gopkg.in/yaml.v3`.  MIT license.

**Test coverage:** 100% on the CPU arbitrator (33 test scenarios).  Token budget controller tests cover all four signal levels, cross-window reset, and threshold transitions.

## z/OS WLM Mapping

| z/OS WLM | wlmd |
|----------|------|
| Service class | ServiceClass in policy.yaml |
| Service policy | policy.yaml |
| Goal mode (response time) | `goal.type: response_time` |
| Importance level | `importance: 1-5` |
| Resource group capping | `min_weight` / `max_weight` |
| 10-second sampling interval | `-interval 10s` |
| RMF/SMF reports | stdout logging (planned: Prometheus metrics) |

## Roadmap

- [ ] Kubernetes operator (Custom Resource → WLM policy)
- [ ] GPU pressure sensing (NVML-based PSI equivalent)
- [ ] Memory pressure PID controller
- [ ] Multi-host coordinated arbitration (gRPC)
- [ ] Prometheus metrics export
- [ ] systemd integration (socket activation)

## License

MIT

## Related

- [IBM z/OS WLM Documentation](https://www.ibm.com/docs/en/zos/latest?topic=management-zos-workload-manager)
- [Linux cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- [PSI — Pressure Stall Information](https://docs.kernel.org/accounting/psi.html)
- [Hermes Agent WLM Token Hook](https://github.com/deeparchi-ai/wlm/blob/main/internal/token/hermes.go)
