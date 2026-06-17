> **🤖 AI-Maintained** — This repository is maintained by AI agents. Human commits (perhaps) zero. Liability (certainly) none. Fun (definitely) infinite.

1|# WLM — Goal-Oriented Workload Manager
2|
3|[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go)](https://go.dev)
4|[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
5|
6|WLM is a userspace resource controller that brings IBM z/OS **Workload Manager** semantics to Linux. Instead of fixed priorities or fair-share, you define **business goals** (response time, throughput) per workload, and WLM dynamically adjusts cgroup v2 resource allocations to meet them.
7|
8|## Architecture
9|
10|```
11|┌──────────────────────────────────────────────────┐
12|│  Service Policy (YAML)                            │
13|│  "interactive: response_time < 2s, importance=1"  │
14|└────────────────────┬─────────────────────────────┘
15|                     │
16|┌────────────────────▼─────────────────────────────┐
17|│  WLM Daemon (wlmd)                                │
18|│                                                   │
19|│  ┌──────────┐   ┌──────────┐   ┌──────────────┐  │
20|│  │ PSI Read │ → │ PI Ctrl  │ → │ cgroup Write  │  │
21|│  │ (observe)│   │ (decide) │   │ (cpu.weight)  │  │
22|│  └──────────┘   └──────────┘   └──────────────┘  │
23|│       ↑                              │            │
24|│       └──────── feedback ────────────┘            │
25|└──────────────────────────────────────────────────┘
26|                     │
27|┌────────────────────▼─────────────────────────────┐
28|│  Linux cgroup v2 + PSI                            │
29|│  /sys/fs/cgroup/.../cpu.weight                    │
30|│  /proc/pressure/cpu                               │
31|└──────────────────────────────────────────────────┘
32|```
33|
34|- **Zero kernel changes** — uses standard cgroup v2 + PSI interfaces
35|- **Goal-oriented** — define *what* you want, not *how much* to give
36|- **PID control loop** — proportional-integral controller with anti-windup
37|- **Service class model** — maps 1:1 with z/OS WLM semantics
38|
39|## Quick Start
40|
41|### Prerequisites
42|
43|- Linux kernel ≥ 4.20 (for PSI; ≥ 5.0 recommended)
44|- cgroup v2 mounted at `/sys/fs/cgroup`
45|- Root access for initial cgroup setup
46|
47|### Setup (one-time)
48|
49|```bash
50|# Enable cpu controller delegation
51|echo "+cpu" | sudo tee /sys/fs/cgroup/cgroup.subtree_control
52|
53|# Create parent cgroup
54|sudo mkdir -p /sys/fs/cgroup/wlm
55|echo "+cpu" | sudo tee /sys/fs/cgroup/wlm/cgroup.subtree_control
56|
57|# Create workload cgroups and delegate ownership
58|sudo mkdir -p /sys/fs/cgroup/wlm/interactive /sys/fs/cgroup/wlm/batch
59|echo 100 | sudo tee /sys/fs/cgroup/wlm/interactive/cpu.weight
60|echo 100 | sudo tee /sys/fs/cgroup/wlm/batch/cpu.weight
61|sudo chown -R $USER /sys/fs/cgroup/wlm/interactive /sys/fs/cgroup/wlm/batch
62|```
63|
64|### Policy
65|
66|```yaml
67|# policy.yaml
68|service_classes:
69|  - name: "interactive"
70|    cgroup: "/wlm/interactive"
71|    goal:
72|      type: "response_time"
73|      target: "2s"
74|    importance: 1
75|    min_weight: 10
76|    max_weight: 1000
77|
78|  - name: "batch"
79|    cgroup: "/wlm/batch"
80|    goal:
81|      type: "velocity"
82|      target: ""
83|    importance: 3
84|    min_weight: 1
85|    max_weight: 900
86|```
87|
88|### Run
89|
90|```bash
91|# Build
92|go build -o wlmd ./cmd/wlmd/
93|
94|# Run (non-root — cgroups must be pre-created and chown'd)
95|./wlmd -policy policy.yaml -interval 10s
96|```
97|
98|### Putting It Together
99|
100|```bash
101|# Put processes in cgroups
102|echo $PID | sudo tee /sys/fs/cgroup/wlm/interactive/cgroup.procs
103|
104|# Run WLM — it will adjust cpu.weight based on PSI pressure
105|./wlmd -policy policy.yaml
106|
107|# Watch what happens
108|watch -n 2 'cat /sys/fs/cgroup/wlm/interactive/cpu.weight'
109|```
110|
111|## Goal Types
112|
113|| Type | Semantics | PSI Mapping |
114||------|-----------|-------------|
115|| `response_time` | p99 latency target (e.g. "2s") | PSI < target-derived threshold |
116|| `throughput` | rate target (e.g. "100/min") | PSI < 8% as inverse proxy |
117|| `velocity` | best-effort, don't starve | PSI < 10% |
118|
119|When PSI exceeds the threshold for a goal, WLM increases `cpu.weight` for that cgroup. When PSI is below threshold, weight stays steady (no drift).
120|
121|## Importance
122|
123|When resources are constrained and multiple service classes are under pressure, WLM prioritizes lower importance numbers:
124|
125|1. Importance 1 → protected first (interactive, latency-sensitive)
126|2. Importance 2-3 → balanced
127|3. Importance 4-5 → sacrificed first (batch, background)
128|
129|*Importance arbitration is planned for v0.2.0 — currently each controller operates independently.*
130|
131|## Comparison
132|
133|| | Linux CFS | cgroup limits | Kubernetes QoS | **WLM** |
134||---|---|---|---|---|
135|| Model | fair-share | hard cap | priority class | **goal-oriented** |
136|| Input | nice value | cpu.max | Guaranteed/Burstable/BestEffort | **response_time < 2s** |
137|| Feedback | none | none | none | **PSI loop every 10s** |
138|| Multi-workload | proportional | independent | pod-level | **importance arbitration** |
139|
140|## z/OS WLM Mapping
141|
142|| z/OS WLM | wlmd |
143||----------|------|
144|| Service class | ServiceClass in policy.yaml |
145|| Service policy | policy.yaml |
146|| Goal mode (response time) | `goal.type: response_time` |
147|| Importance level | `importance: 1-5` |
148|| Resource group capping | `min_weight` / `max_weight` |
149|| 10-second sampling interval | `-interval 10s` |
150|| RMF/SMF reports | stdout logging (planned: Prometheus metrics) |
151|
152|## License
153|
154|MIT
155|
156|## Related
157|
158|- [IBM z/OS WLM Documentation](https://www.ibm.com/docs/en/zos/latest?topic=management-zos-workload-manager)
159|- [Linux cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)
160|- [PSI — Pressure Stall Information](https://docs.kernel.org/accounting/psi.html)
161|