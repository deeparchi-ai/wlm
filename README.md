# WLM — Goal-Oriented Workload Manager

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

WLM is a userspace resource controller that brings IBM z/OS **Workload Manager** semantics to Linux. Instead of fixed priorities or fair-share, you define **business goals** (response time, throughput) per workload, and WLM dynamically adjusts cgroup v2 resource allocations to meet them.

## Architecture

```
┌──────────────────────────────────────────────────┐
│  Service Policy (YAML)                            │
│  "interactive: response_time < 2s, importance=1"  │
└────────────────────┬─────────────────────────────┘
                     │
┌────────────────────▼─────────────────────────────┐
│  WLM Daemon (wlmd)                                │
│                                                   │
│  ┌──────────┐   ┌──────────┐   ┌──────────────┐  │
│  │ PSI Read │ → │ PI Ctrl  │ → │ cgroup Write  │  │
│  │ (observe)│   │ (decide) │   │ (cpu.weight)  │  │
│  └──────────┘   └──────────┘   └──────────────┘  │
│       ↑                              │            │
│       └──────── feedback ────────────┘            │
└──────────────────────────────────────────────────┘
                     │
┌────────────────────▼─────────────────────────────┐
│  Linux cgroup v2 + PSI                            │
│  /sys/fs/cgroup/.../cpu.weight                    │
│  /proc/pressure/cpu                               │
└──────────────────────────────────────────────────┘
```

- **Zero kernel changes** — uses standard cgroup v2 + PSI interfaces
- **Goal-oriented** — define *what* you want, not *how much* to give
- **PID control loop** — proportional-integral controller with anti-windup
- **Service class model** — maps 1:1 with z/OS WLM semantics

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

### Policy

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

### Run

```bash
# Build
go build -o wlmd ./cmd/wlmd/

# Run (non-root — cgroups must be pre-created and chown'd)
./wlmd -policy policy.yaml -interval 10s
```

### Putting It Together

```bash
# Put processes in cgroups
echo $PID | sudo tee /sys/fs/cgroup/wlm/interactive/cgroup.procs

# Run WLM — it will adjust cpu.weight based on PSI pressure
./wlmd -policy policy.yaml

# Watch what happens
watch -n 2 'cat /sys/fs/cgroup/wlm/interactive/cpu.weight'
```

## Goal Types

| Type | Semantics | PSI Mapping |
|------|-----------|-------------|
| `response_time` | p99 latency target (e.g. "2s") | PSI < target-derived threshold |
| `throughput` | rate target (e.g. "100/min") | PSI < 8% as inverse proxy |
| `velocity` | best-effort, don't starve | PSI < 10% |

When PSI exceeds the threshold for a goal, WLM increases `cpu.weight` for that cgroup. When PSI is below threshold, weight stays steady (no drift).

## Importance

When resources are constrained and multiple service classes are under pressure, WLM prioritizes lower importance numbers:

1. Importance 1 → protected first (interactive, latency-sensitive)
2. Importance 2-3 → balanced
3. Importance 4-5 → sacrificed first (batch, background)

*Importance arbitration is planned for v0.2.0 — currently each controller operates independently.*

## Comparison

| | Linux CFS | cgroup limits | Kubernetes QoS | **WLM** |
|---|---|---|---|---|
| Model | fair-share | hard cap | priority class | **goal-oriented** |
| Input | nice value | cpu.max | Guaranteed/Burstable/BestEffort | **response_time < 2s** |
| Feedback | none | none | none | **PSI loop every 10s** |
| Multi-workload | proportional | independent | pod-level | **importance arbitration** |

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

## License

MIT

## Related

- [IBM z/OS WLM Documentation](https://www.ibm.com/docs/en/zos/latest?topic=management-zos-workload-manager)
- [Linux cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- [PSI — Pressure Stall Information](https://docs.kernel.org/accounting/psi.html)
