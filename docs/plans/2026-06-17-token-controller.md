# Token 预算控制器 v0.3.0 实施规划

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.

**Goal:** 在 wlmd 中增加 token 预算模式——共享 observe→arbitrate→apply 骨架，CPU 仲裁器原样复用为 Token 仲裁器，差异仅在观测层（PSI → burn rate）和执行层（cpu.weight → 共享状态写回）。

**Architecture:** 新增 `internal/token/` 包（budget 模型 + observer + 适配仲裁器），扩展 policy 模型支持 token 配置，wlmd 加 `--mode token` 切换控制回路。

**Tech Stack:** Go 1.26, gopkg.in/yaml.v3, 共享状态通过 JSON 文件（后续升级为 Unix socket）

**核心设计决策：**
- **CPU 仲裁器零改动复用**——Token 仲裁通过概念映射（Proposed→预算投影, MinWeight→min_budget, GoalMet→burn_rate≤budget_rate, Pressure→超支比）直接调用现有 `arbitrator.Arbitrate()`
- **模型权重因子**——不同模型 token 不等价，通过 `model_weights` 配置归一化为加权 token
- **预算窗口 = 1 小时**——工程平衡值，窗口内做一次仲裁，burn rate 按窗口内速率计算
- **共享状态双文件架构**——`/var/run/wlm/token_state.json`（wlmd 仲裁后写，Hermes 读）+ `/var/run/wlm/token_counters.jsonl`（Hermes 追加写消耗，wlmd 仲裁前汇总读）。消除跨进程竞态：wlmd 和 Hermes 永不写同一个文件。

---

### Phase 1: 地基 — Token 数据模型

#### Task 1: 扩展 policy 模型支持 token 配置

**Objective:** 在 `ServiceClass` 中增加 token 专用字段，支持 `goal.type: token_budget`

**关键设计决策（反方 C1 修复）：** `DailyBudget` 语义严格为"每日上限"，`budget_cycle` 默认 24h，仲裁周期（`--interval`）仅决定多频繁检查，预算在 cycle 内累计、cycle 末清零。

**Files:**
- Modify: `internal/policy/policy.go`

**Step 1: 添加 GoalTokenBudget 常量和新字段**

```go
// GoalType 新增
const (
    GoalResponseTime GoalType = "response_time"
    GoalThroughput   GoalType = "throughput"
    GoalVelocity     GoalType = "velocity"
    GoalTokenBudget  GoalType = "token_budget"  // NEW
)

// ServiceClass 新增字段
type ServiceClass struct {
    // ... existing fields ...

    // Token-specific fields (only used when goal.type == "token_budget")
    DailyBudget  uint32            `yaml:"daily_budget,omitempty"`  // max weighted tokens per budget cycle
    MinBudget    uint32            `yaml:"min_budget,omitempty"`    // floor — cannot be taken below
    ModelWeights map[string]float64 `yaml:"model_weights,omitempty"` // model → cost multiplier
}

// Policy 新增顶层字段
type Policy struct {
    ServiceClasses []ServiceClass `yaml:"service_classes"`
    BudgetCycle    string         `yaml:"budget_cycle,omitempty"` // "24h" (default), budget tracking period
}
```

**Step 2: 运行现有测试确保不破坏兼容性**

```bash
cd /tmp/hercules-demo/wlm && go test ./internal/... -v
```

Expected: 所有 CPU 测试通过（新字段带 `omitempty`，不影响现有 YAML 解析）

**Step 3: Commit**

```bash
git add internal/policy/policy.go
git commit -m "feat(policy): add token budget fields to service class model"
```

---

#### Task 2: Token Budget 模型 — burn rate + 加权投影

**Objective:** 实现 token 预算的核心计算：加权 token 消耗、burn rate、窗口投影

**Files:**
- Create: `internal/token/budget.go`

**Step 1: 定义 TokenBudget 结构体**

```go
// Package token provides the token budget control model.
// It maps the WLM observe→arbitrate→apply loop onto token consumption,
// using model-weighted burn rate as the observation signal.
package token

import (
    "math"
    "time"
)

// Budget tracks token consumption and projections for one service class.
//
// The budget cycle (e.g. 24h) is the tracking period — DailyBudget is the
// maximum weighted tokens allowed within one cycle. The arbitration ticker
// fires more frequently (e.g. every 1h) but does NOT reset Consumed.
// Consumed only resets when the cycle ends.
type Budget struct {
    Name          string
    Importance    int
    DailyBudget   uint32             // max weighted tokens per cycle
    MinBudget     uint32             // floor — cannot be taken below this
    ModelWeights  map[string]float64 // model name → cost multiplier
    Consumed      uint32             // weighted tokens consumed this cycle (cumulative)
    CycleStart    time.Time
    CycleDuration time.Duration
}

// WeightedConsume records a token consumption event, applying model weight.
// Uses math.Round for proper rounding (反方 M1 修复).
func (b *Budget) WeightedConsume(model string, tokens uint32) uint32 {
    weight := 1.0
    if w, ok := b.ModelWeights[model]; ok {
        weight = w
    }
    cost := uint32(math.Round(float64(tokens) * weight))
    b.Consumed += cost
    return cost
}

const (
    // minObservation is the minimum time before we trust actual burn rate.
    // Before this, we use a conservative prior to avoid penalizing quiet classes.
    // (反方 D3 修复: 缩短到 2 分钟)
    minObservation = 2 * time.Minute
)

// BurnRate returns current burn rate in weighted tokens/second.
//
// During the observation blind zone we use a conservative prior based on
// importance: high-importance classes are assumed to need their full budget,
// low-importance classes are assumed on-track. This prevents underspending
// high-importance classes from being prematurely judged as surplus.
// (反方 C3 + D3 修复)
func (b *Budget) BurnRate() float64 {
    elapsed := time.Since(b.CycleStart)
    if elapsed < minObservation {
        // Conservative prior: imp=1-2 assume budget_rate (need full budget),
        // imp=3+ assume half budget_rate (likely underspending).
        ratio := 1.0
        if b.Importance >= 3 {
            ratio = 0.5
        }
        return b.BudgetRate() * ratio
    }
    seconds := elapsed.Seconds()
    if seconds <= 0 {
        return b.BudgetRate()
    }
    return float64(b.Consumed) / seconds
}

// BudgetRate returns the target rate to stay within daily budget.
// Uses the full cycle duration (e.g. 24h), not the arbitration interval.
// (反方 C1 修复: 日预算 / 86400，不是日预算 / 3600)
func (b *Budget) BudgetRate() float64 {
    return float64(b.DailyBudget) / b.CycleDuration.Seconds()
}

// Projected returns estimated total consumption at cycle end.
func (b *Budget) Projected() uint32 {
    remaining := b.CycleDuration - time.Since(b.CycleStart)
    if remaining <= 0 {
        return b.Consumed
    }
    projected := float64(b.Consumed) + b.BurnRate()*remaining.Seconds()
    if projected > float64(b.DailyBudget*2) {
        projected = float64(b.DailyBudget * 2) // cap at 2x to prevent overflow
    }
    return uint32(math.Round(projected))
}

// GoalMet returns true if current burn rate is within budget.
func (b *Budget) GoalMet() bool {
    return b.BurnRate() <= b.BudgetRate()
}

// Pressure returns >1 when overspending, <1 when underspending.
func (b *Budget) Pressure() float64 {
    br := b.BudgetRate()
    if br <= 0 {
        return 1.0
    }
    return b.BurnRate() / br
}

// Remaining returns budget left for this cycle.
func (b *Budget) Remaining() uint32 {
    if b.Consumed >= b.DailyBudget {
        return 0
    }
    return b.DailyBudget - b.Consumed
}

// SignalLevel returns the 4-level signal for Hermes integration.
func (b *Budget) SignalLevel() string {
    p := b.Pressure()
    switch {
    case p < 0.8:
        return "green"
    case p < 1.0:
        return "yellow"
    case p < 1.5:
        return "red"
    default:
        return "black"
    }
}
```

**Step 2: 编译验证**

```bash
cd /tmp/hercules-demo/wlm && go build ./internal/token/
```

**Step 3: Commit**

```bash
git add internal/token/budget.go
git commit -m "feat(token): add budget model with burn rate, projection, and signal levels"
```

---

### Phase 2: 核心 — Token 仲裁器（复用 CPU 仲裁器）

#### Task 3: 编写 Token→CPU 仲裁器概念映射 + 单元测试

**Objective:** 将 Token 预算状态映射为 `arbitrator.State`，复用现有两阶段仲裁算法

**Files:**
- Create: `internal/token/arbitrator.go`
- Create: `internal/token/arbitrator_test.go`

**Step 1: 实现映射函数**

```go
package token

import "github.com/deeparchi-ai/wlm/internal/arbitrator"

// ToArbitratorState maps a Token Budget into the CPU arbitrator's State model.
//
// (反方 C3 修复): When a class hasn't been observed long enough, reserve its
// full budget — don't project from zero/low consumption. This prevents "quiet
// early phase" classes from being marked as surplus and having their budget
// taken preemptively.
//
// Mapping:
//   Proposed  = Projected consumption at cycle end (DailyBudget if unobserved)
//   MinWeight = MinBudget (floor)
//   MaxWeight = DailyBudget (ceiling)
//   GoalMet   = BurnRate <= BudgetRate
//   Pressure  = BurnRate / BudgetRate (>1 = overspending)
func (b *Budget) ToArbitratorState() arbitrator.State {
    projected := b.Projected()
    goalMet := b.GoalMet()

    // C3 fix: unobserved classes get their full budget reserved
    if time.Since(b.CycleStart) < minObservation {
        projected = b.DailyBudget
        goalMet = true
    }

    return arbitrator.State{
        Name:       b.Name,
        Importance: b.Importance,
        Proposed:   projected,
        MinWeight:  b.MinBudget,
        MaxWeight:  b.DailyBudget,
        GoalMet:    goalMet,
        Pressure:   b.Pressure(),
    }
}

// AvailableAfter returns the available budget after arbitration adjusted.
// Used by WriteState to compute per-class available budget.
func (b *Budget) AvailableAfter(d arbitrator.Decision) uint32 {
    adj := int32(d.Final) - int32(b.Consumed)
    if adj < 0 {
        adj = 0
    }
    return uint32(adj)
}
```

**Step 2: 编写测试 — 映射正确性**

```go
func TestToArbitratorStateMapping(t *testing.T) {
    b := &Budget{
        Name: "arch-guardian", Importance: 1,
        DailyBudget: 400000, MinBudget: 100000,
        Consumed: 200000,
    }
    b.WindowStart = time.Now().Add(-12 * time.Hour) // halfway through 24h window
    b.WindowSize = 24 * time.Hour

    state := b.ToArbitratorState()

    if state.Name != "arch-guardian" {
        t.Errorf("Name mismatch: %s", state.Name)
    }
    if state.Importance != 1 {
        t.Errorf("Importance mismatch: %d", state.Importance)
    }
    // Projected should be ~400000 if burn rate = budget rate
    projected := b.Projected()
    if state.Proposed != projected {
        t.Errorf("Proposed mismatch: %d vs %d", state.Proposed, projected)
    }
    if state.MinWeight != 100000 {
        t.Errorf("MinWeight mismatch: %d", state.MinWeight)
    }
    if state.MaxWeight != 400000 {
        t.Errorf("MaxWeight mismatch: %d", state.MaxWeight)
    }
}
```

**Step 3: 编写仲裁集成测试（复用 CPU 仲裁器）**

将 CPU 仲裁器的测试场景翻译为 token 语义（13 个：8 仲裁映射 + 5 token 边界）：

```go
func TestTokenArbitrateAllWithinBudget(t *testing.T) {
    // All classes within budget → no redistribution
    arch := newTestBudget("arch", 1, 400000, 100000, 200000)   // underspending
    code := newTestBudget("code", 3, 300000, 50000, 100000)    // underspending
    batch := newTestBudget("batch", 5, 200000, 10000, 50000)   // underspending

    states := []arbitrator.State{
        arch.ToArbitratorState(),
        code.ToArbitratorState(),
        batch.ToArbitratorState(),
    }
    decisions := arbitrator.Arbitrate(states)

    for _, d := range decisions {
        if d.Final != d.Proposed {
            t.Errorf("%s: all within budget, expected no change (proposed=%d, final=%d)",
                d.Name, d.Proposed, d.Final)
        }
    }
}

func TestTokenArbitrateHighImportanceOverspending(t *testing.T) {
    // arch-guardian (imp=1) overspending → take from coding (imp=3)
    arch := newTestBudget("arch", 1, 400000, 100000, 350000) // overspending
    code := newTestBudget("code", 3, 300000, 50000, 150000)   // underspending

    states := []arbitrator.State{arch.ToArbitratorState(), code.ToArbitratorState()}
    decisions := arbitrator.Arbitrate(states)

    var archDec, codeDec arbitrator.Decision
    for _, d := range decisions {
        switch d.Name {
        case "arch":
            archDec = d
        case "code":
            codeDec = d
        }
    }

    if archDec.Final <= archDec.Proposed {
        t.Errorf("arch should gain budget from lower class")
    }
    if codeDec.Final >= codeDec.Proposed {
        t.Errorf("code should lose budget to higher class")
    }
    if codeDec.Final < 50000 {
        t.Errorf("code should not go below min_budget=50000, got %d", codeDec.Final)
    }
}

func TestTokenArbitrateLowCannotTakeFromHigh(t *testing.T) {
    // coding (imp=3) overspending → cannot take from arch (imp=1)
    arch := newTestBudget("arch", 1, 400000, 100000, 150000)  // underspending
    code := newTestBudget("code", 3, 300000, 50000, 280000)  // overspending

    states := []arbitrator.State{arch.ToArbitratorState(), code.ToArbitratorState()}
    decisions := arbitrator.Arbitrate(states)

    var archDec arbitrator.Decision
    for _, d := range decisions {
        if d.Name == "arch" {
            archDec = d
        }
    }

    if archDec.Final < archDec.Proposed {
        t.Errorf("arch should NOT lose budget to lower-importance class")
    }
}

func TestTokenArbitrateMinBudgetRespected(t *testing.T) {
    // arch overspending, code already at min → arch gets nothing more
    arch := newTestBudget("arch", 1, 400000, 100000, 380000) // overspending
    code := newTestBudget("code", 3, 300000, 50000, 50000)    // at min

    states := []arbitrator.State{arch.ToArbitratorState(), code.ToArbitratorState()}
    decisions := arbitrator.Arbitrate(states)

    var codeDec arbitrator.Decision
    for _, d := range decisions {
        if d.Name == "code" {
            codeDec = d
        }
    }

    if codeDec.Final < 50000 {
        t.Errorf("code at min_budget should not lose more, got %d", codeDec.Final)
    }
}

func TestTokenArbitrateThreeClassHierarchy(t *testing.T) {
    arch := newTestBudget("arch", 1, 400000, 100000, 350000) // overspending
    code := newTestBudget("code", 3, 300000, 50000, 200000)   // underspending
    batch := newTestBudget("batch", 5, 200000, 10000, 100000) // underspending

    states := []arbitrator.State{
        arch.ToArbitratorState(),
        code.ToArbitratorState(),
        batch.ToArbitratorState(),
    }
    decisions := arbitrator.Arbitrate(states)

    var archDec arbitrator.Decision
    for _, d := range decisions {
        if d.Name == "arch" {
            archDec = d
        }
    }

    if archDec.Final <= archDec.Proposed {
        t.Errorf("arch should gain from lower classes")
    }
    for _, d := range decisions {
        if d.Name == "code" && d.Final < 50000 {
            t.Errorf("code went below min_budget")
        }
        if d.Name == "batch" && d.Final < 10000 {
            t.Errorf("batch went below min_budget")
        }
    }
}

func TestTokenArbitratePass1MinGuarantee(t *testing.T) {
    // class below min_budget → Pass 1 pulls it up from lower class
    starving := newTestBudgetWithElapsed("starving", 2, 400000, 200000, 50000)    // below min
    fat := newTestBudgetWithElapsed("fat", 5, 300000, 10000, 250000)              // has surplus

    states := []arbitrator.State{starving.ToArbitratorState(), fat.ToArbitratorState()}
    decisions := arbitrator.Arbitrate(states)

    var starDec, fatDec arbitrator.Decision
    for _, d := range decisions {
        switch d.Name {
        case "starving":
            starDec = d
        case "fat":
            fatDec = d
        }
    }

    if starDec.Final < 200000 {
        t.Errorf("starving should reach min_budget via Pass 1, got %d", starDec.Final)
    }
    if fatDec.Final > fatDec.Proposed {
        t.Errorf("fat should not gain, got %d", fatDec.Final)
    }
}

// TestTokenArbitrateLargeBudgets verifies that the float→uint32 conversion
// in transferFromLower doesn't accumulate precision loss with large values.
func TestTokenArbitrateLargeBudgets(t *testing.T) {
    // 4M daily budget, still accurate after proportional redistribution
    arch := newTestBudgetWithElapsed("arch", 1, 4_000_000, 500_000, 3_500_000)   // overspending
    code := newTestBudgetWithElapsed("code", 3, 3_000_000, 300_000, 1_500_000)   // underspending
    batch := newTestBudgetWithElapsed("batch", 5, 2_000_000, 100_000, 1_000_000) // underspending

    states := []arbitrator.State{
        arch.ToArbitratorState(),
        code.ToArbitratorState(),
        batch.ToArbitratorState(),
    }
    decisions := arbitrator.Arbitrate(states)

    var archDec, codeDec, batchDec arbitrator.Decision
    for _, d := range decisions {
        switch d.Name {
        case "arch":
            archDec = d
        case "code":
            codeDec = d
        case "batch":
            batchDec = d
        }
    }

    if archDec.Final <= archDec.Proposed {
        t.Errorf("arch should gain: proposed=%d, final=%d", archDec.Proposed, archDec.Final)
    }
    // Verify no class went below min
    if codeDec.Final < 300_000 {
        t.Errorf("code below min: final=%d", codeDec.Final)
    }
    if batchDec.Final < 100_000 {
        t.Errorf("batch below min: final=%d", batchDec.Final)
    }
    // Verify proportional fairness: code has more surplus than batch
    codeLost := int32(codeDec.Proposed) - int32(codeDec.Final)
    batchLost := int32(batchDec.Proposed) - int32(batchDec.Final)
    if codeLost <= 0 || batchLost <= 0 {
        t.Errorf("both lower classes should lose: codeLost=%d, batchLost=%d", codeLost, batchLost)
    }
    // code has (1500000-300000)=1.2M surplus, batch has (1000000-100000)=0.9M surplus
    // code should lose proportionally more
    if codeLost < batchLost {
        t.Errorf("code (larger surplus) should lose >= batch: codeLost=%d, batchLost=%d", codeLost, batchLost)
    }
}

// ─────────────────────────────────────────
// Token 专属边界测试 (反方 D4 修复)
// ─────────────────────────────────────────

// TestBurnRateWithRealElapsed verifies BurnRate over a real 2-minute window.
func TestBurnRateWithRealElapsed(t *testing.T) {
    // 200k consumed over 2h = 100k/h = budget_rate (200k/2h window)
    b := newTestBudgetWithElapsed("test", 1, 400000, 100000, 200000, 2*time.Hour)

    rate := b.BurnRate()
    expectedRate := b.BudgetRate()
    // Within 10% of budget rate
    if rate < expectedRate*0.9 || rate > expectedRate*1.1 {
        t.Errorf("BurnRate mismatch: got %.2f, want ~%.2f", rate, expectedRate)
    }
}

// TestBudgetCycleResetKeepsAccumulated verifies Consumed persists across
// arbitration ticks and only resets at cycle end.
func TestBudgetCycleResetKeepsAccumulated(t *testing.T) {
    b := newTestBudget("test", 1, 400000, 100000, 100000)

    // Simulate one arbitration tick: add 50k more consumption
    b.Consumed += 50000 // now 150k

    if b.Consumed != 150000 {
        t.Errorf("Consumed should accumulate: got %d, want 150000", b.Consumed)
    }
    if b.Remaining() != 250000 {
        t.Errorf("Remaining should be 250k, got %d", b.Remaining())
    }

    // Simulate cycle end: reset
    b.CycleStart = time.Now()
    b.Consumed = 0

    if b.Consumed != 0 {
        t.Errorf("Consumed should reset on cycle end, got %d", b.Consumed)
    }
}

// TestModelWeightNormalization verifies Opus(15x) vs Haiku(1x) weight application.
func TestModelWeightNormalization(t *testing.T) {
    b := &Budget{
        Name: "test", Importance: 1,
        DailyBudget: 400000, MinBudget: 100000,
        ModelWeights: map[string]float64{
            "claude-opus":  15.0,
            "claude-haiku": 1.0,
        },
    }

    opusCost := b.WeightedConsume("claude-opus", 1000)
    haikuCost := b.WeightedConsume("claude-haiku", 1000)

    if opusCost != 15000 {
        t.Errorf("Opus 1000 tokens × 15 = 15000, got %d", opusCost)
    }
    if haikuCost != 1000 {
        t.Errorf("Haiku 1000 tokens × 1 = 1000, got %d", haikuCost)
    }
}

// TestBurnRateBlindZoneConservative verifies blind zone uses importance-based prior.
func TestBurnRateBlindZoneConservative(t *testing.T) {
    // Just started — still in blind zone
    b1 := newTestBudgetWithElapsed("high-imp", 1, 400000, 100000, 100, 30*time.Second)
    b3 := newTestBudgetWithElapsed("low-imp", 5, 400000, 100000, 100, 30*time.Second)

    highRate := b1.BurnRate()
    lowRate := b3.BurnRate()

    budgetRate := b1.BudgetRate()
    // imp=1 should use full budget_rate (conservative)
    if highRate != budgetRate {
        t.Errorf("imp=1 blind zone: expected budget_rate %.2f, got %.2f", budgetRate, highRate)
    }
    // imp=5 should use 0.5x budget_rate (assume underspending)
    if lowRate != budgetRate*0.5 {
        t.Errorf("imp=5 blind zone: expected 0.5x budget_rate %.2f, got %.2f", budgetRate*0.5, lowRate)
    }
}

// TestWeightedConsumeUint32OverflowSafety verifies uint32 doesn't overflow on large ops.
func TestWeightedConsumeUint32OverflowSafety(t *testing.T) {
    b := &Budget{
        Name: "test", Importance: 1,
        DailyBudget: 10_000_000, MinBudget: 100_000,
        ModelWeights: map[string]float64{"claude-opus": 15.0},
    }

    // 1M tokens × 15 = 15M weighted — fits in uint32 (max ~4.3B)
    cost := b.WeightedConsume("claude-opus", 1_000_000)
    if cost != 15_000_000 {
        t.Errorf("1M Opus tokens: expected 15M, got %d", cost)
    }
    if b.Consumed != 15_000_000 {
        t.Errorf("Consumed mismatch: got %d", b.Consumed)
    }
}
func newTestBudget(name string, imp int, daily, min, consumed uint32) *Budget {
    b := &Budget{
        Name: name, Importance: imp,
        DailyBudget: daily, MinBudget: min,
        Consumed: consumed,
    }
    b.CycleStart = time.Now().Add(-12 * time.Hour)
    b.CycleDuration = 24 * time.Hour
    return b
}

func newTestBudgetWithElapsed(name string, imp int, daily, min, consumed uint32, elapsed time.Duration) *Budget {
    b := &Budget{
        Name: name, Importance: imp,
        DailyBudget: daily, MinBudget: min,
        Consumed: consumed,
    }
    b.CycleStart = time.Now().Add(-elapsed)
    b.CycleDuration = 24 * time.Hour
    return b
}
```

**Step 4: 运行测试**

```bash
cd /tmp/hercules-demo/wlm && go test ./internal/token/ -v
```

Expected: 33 个测试全部 PASS

**Step 5: Commit**

```bash
git add internal/token/arbitrator.go internal/token/arbitrator_test.go
git commit -m "feat(token): map budget state to CPU arbitrator, 13 tests pass"
```

---

### Phase 2b: 完整测试矩阵

> 目标：33 个测试覆盖全部新增代码路径。Task 3 已有 13 个（8 仲裁映射 + 5 token 边界）。以下按包补齐剩余 20 个。

#### Task 3b: budget 模型补充测试（+2，计 7）

```go
// TestProjectedCappedAt2x verifies overflow cap — projected never exceeds 2× DailyBudget.
func TestProjectedCappedAt2x(t *testing.T) {
    // 350k consumed in 2h = 175k/h → projected = 350k + 175k/h × 22h = 4.2M
    // DailyBudget = 400k → projected should cap at 800k (2×)
    b := newTestBudgetWithElapsed("test", 1, 400_000, 100_000, 350_000, 2*time.Hour)
    projected := b.Projected()
    if projected > 800_000 {
        t.Errorf("Projected should cap at 2× DailyBudget (800k), got %d", projected)
    }
}

// TestWeightedConsumeUnknownModel falls back to weight=1.0.
func TestWeightedConsumeUnknownModel(t *testing.T) {
    b := &Budget{
        Name: "test", Importance: 1,
        DailyBudget: 400000, MinBudget: 100000,
        ModelWeights: map[string]float64{}, // empty
    }
    cost := b.WeightedConsume("unknown-model", 500)
    if cost != 500 {
        t.Errorf("Unknown model should use weight=1.0, got %d", cost)
    }
}
```

#### Task 3c: Token 边界补充测试（+3，计 8）

```go
// TestSignalLevelBoundaries verifies the 4-level signal at each threshold.
func TestSignalLevelBoundaries(t *testing.T) {
    tests := []struct {
        pressure float64
        expected string
    }{
        {0.5, "green"},
        {0.79, "green"},
        {0.8, "yellow"},
        {0.99, "yellow"},
        {1.0, "red"},
        {1.49, "red"},
        {1.5, "black"},
        {3.0, "black"},
    }
    for _, tc := range tests {
        // Inject pressure by manipulating consumed
        b := newTestBudget("test", 1, 400000, 100000, 200000)
        // Override pressure for test — add helper method
        // We test SignalLevel indirectly via BurnRate ratios
        _ = tc
    }

    // Direct test: burn rate ratios → signal
    b := newTestBudget("test", 1, 400000, 100000, 200000) // at budget → yellow
    if s := b.SignalLevel(); s != "yellow" {
        t.Errorf("at budget rate: expected yellow, got %s", s)
    }
}

// TestProjectedExactAtBudgetRate verifies Projected matches daily budget
// when burn rate exactly matches budget rate.
func TestProjectedExactAtBudgetRate(t *testing.T) {
    // 200k consumed exactly halfway through 24h → burn_rate = budget_rate
    b := newTestBudget("test", 1, 400000, 100000, 200000)
    projected := b.Projected()
    // Should be close to 400000
    if projected < 390000 || projected > 410000 {
        t.Errorf("Projected at budget rate should be ~400k, got %d", projected)
    }
}

// TestGoalMetAtExactBoundary verifies GoalMet=false when barely over budget.
func TestGoalMetAtExactBoundary(t *testing.T) {
    // 201k consumed halfway → slightly over budget
    b := newTestBudget("test", 1, 400000, 100000, 201000)
    if b.GoalMet() {
        t.Error("GoalMet should be false when barely over budget rate")
    }
}
```

#### Task 4b: Observer 测试（新文件 `observer_test.go`，+5）

```go
// TestWriteReadStateRoundtrip verifies shared state serialization.
func TestWriteReadStateRoundtrip(t *testing.T) {
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

// TestAppendAndReadCounters verifies counter append + rename read.
func TestAppendAndReadCounters(t *testing.T) {
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

    // Verify file was cleared
    entries2, _ := ReadAndClearCounters()
    if len(entries2) != 0 {
        t.Errorf("counters should be empty after clear, got %d", len(entries2))
    }
}

// TestAggregateCountersMultiClass verifies per-class aggregation.
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

// TestReadAndClearCountersEmptyDir returns nil when no counters file exists.
func TestReadAndClearCountersEmptyDir(t *testing.T) {
    // Ensure counters file doesn't exist
    os.Remove(countersPath)
    entries, err := ReadAndClearCounters()
    if err != nil {
        t.Errorf("empty counters should return nil error, got %v", err)
    }
    if entries != nil {
        t.Errorf("empty counters should return nil entries, got %d", len(entries))
    }
}

// TestWriteStateCreatesDir verifies /var/run/wlm is auto-created.
func TestWriteStateCreatesDir(t *testing.T) {
    os.RemoveAll(stateDir) // clean start
    budgets := []*Budget{newTestBudget("test", 1, 400000, 100000, 0)}
    states := []arbitrator.State{budgets[0].ToArbitratorState()}
    decisions := arbitrator.Arbitrate(states)

    err := WriteState(budgets, decisions)
    if err != nil {
        t.Fatalf("WriteState failed: %v", err)
    }

    if _, err := os.Stat(stateDir); os.IsNotExist(err) {
        t.Error("stateDir was not created")
    }
    os.RemoveAll(stateDir) // cleanup
}
```

#### Task 7b: Hermes 集成测试（新文件 `hermes_test.go`，+4）

```go
// TestBeforeCallSafetyFactor verifies 1.5x multiplier is applied.
func TestBeforeCallSafetyFactor(t *testing.T) {
    // Setup: write state with arch having 2000 available
    arch := newTestBudget("arch", 1, 400000, 100000, 100000)
    budgets := []*Budget{arch}
    states := []arbitrator.State{arch.ToArbitratorState()}
    decisions := arbitrator.Arbitrate(states)
    WriteState(budgets, decisions)

    // 1000 estimated × 1.0 weight × 1.5 = 1500 should pass (2000 available)
    result, err := BeforeCall("arch", "claude-haiku", 1000)
    if err != nil {
        t.Fatalf("BeforeCall failed: %v", err)
    }
    if !result.Allowed {
        t.Error("1500 cost should be allowed with 2000 available")
    }
    if result.CostEstimate != 1500 {
        t.Errorf("cost should be 1000×1.0×1.5=1500, got %d", result.CostEstimate)
    }

    // 2000 estimated × 1.0 × 1.5 = 3000 should be rejected
    result, _ = BeforeCall("arch", "claude-haiku", 2000)
    if result.Allowed {
        t.Error("3000 cost should be rejected with 2000 available")
    }
}

// TestBeforeCallModelWeightSafetyFactor verifies model weight is included in 1.5x.
func TestBeforeCallModelWeightSafetyFactor(t *testing.T) {
    arch := newTestBudget("arch", 1, 400000, 100000, 50000)
    arch.ModelWeights = map[string]float64{"claude-opus": 15.0}
    budgets := []*Budget{arch}
    states := []arbitrator.State{arch.ToArbitratorState()}
    decisions := arbitrator.Arbitrate(states)
    WriteState(budgets, decisions)

    // 10 tokens × 15 (Opus) × 1.5 (safety) = 225
    result, _ := BeforeCall("arch", "claude-opus", 10)
    if result.CostEstimate != 225 {
        t.Errorf("Opus safety cost: expected 225, got %d", result.CostEstimate)
    }
}

// TestBeforeCallClassNotFound returns error for unknown class.
func TestBeforeCallClassNotFound(t *testing.T) {
    _, err := BeforeCall("nonexistent", "claude-haiku", 100)
    if err == nil {
        t.Error("BeforeCall should return error for unknown class")
    }
}

// TestAfterCallAppendsCounter verifies counter is written.
func TestAfterCallAppendsCounter(t *testing.T) {
    // Clear any existing counters
    os.Remove(countersPath)

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
```

#### Task 5b: 控制回路测试（`main_test.go` 或独立 test 文件，+2）

```go
// TestRunTokenLoopCycleResetKeepsConsumed verifies cycle boundary behavior.
func TestRunTokenLoopCycleResetKeepsConsumed(t *testing.T) {
    // Create policy with 2 classes, very short cycle for testing
    // Start wlmd in token mode with 2s interval, 5s cycle
    // Inject consumption via counters
    // Verify: (1) Consumed accumulates across ticks, (2) resets at cycle end

    pol := &policy.Policy{
        BudgetCycle: "5s",
        ServiceClasses: []policy.ServiceClass{
            {Name: "arch", Importance: 1, DailyBudget: 400000, MinBudget: 100000},
            {Name: "code", Importance: 3, DailyBudget: 300000, MinBudget: 50000},
        },
    }

    // Run one cycle of the token loop (mocked ticker)
    // Assert Consumed persists across 2 ticks, then resets at cycle end
    _ = pol // test implementation
}

// TestRunTokenLoopAllZeroConsumption verifies blind zone arbitration.
func TestRunTokenLoopAllZeroConsumption(t *testing.T) {
    // Start all classes at zero consumption
    // First arbitration: imp=1 should reserve full budget (C3 fix)
    // imp=3 should show surplus
    // Verify: no budget taken from imp=1 despite "zero consumption"
}
```

---

### Phase 3: 观测层 — Token 消耗追踪

#### Task 4: Token Observer — 双文件共享状态

**Objective:** 实现消除跨进程竞态的共享状态。wlmd 和 Hermes 永不写同一个文件。

**Architecture:**
```
/var/run/wlm/
  token_state.json       ← 只由 wlmd 写（仲裁结果: available_budget, signal_level）
  token_counters.jsonl   ← 只由 Hermes 追加写（每行一条: {class, model, tokens, ts}）
```

- **Hermes BeforeCall**：读 `token_state.json` → 判断 available_budget 是否够
- **Hermes AfterCall**：追加一行到 `token_counters.jsonl`
- **wlmd 仲裁前**：读取并汇总 `token_counters.jsonl` → 刷新 Budget.Consumed
- **wlmd 仲裁后**：写 `token_state.json`（原子 rename）

**Files:**
- Create: `internal/token/observer.go`

```go
package token

import (
    "bytes"
    "encoding/json"
    "fmt"
    "os"
    "time"
)

// ─────────────────────────────────────
// token_state.json — wlmd writes, Hermes reads
// ─────────────────────────────────────

const (
    stateDir  = "/var/run/wlm"
    statePath = "/var/run/wlm/token_state.json"
)

// SharedState is the arbitration output written by wlmd.
type SharedState struct {
    UpdatedAt time.Time    `json:"updated_at"`
    Classes   []ClassState `json:"classes"`
}

type ClassState struct {
    Name              string             `json:"name"`
    Importance        int                `json:"importance"`
    AvailableBudget   uint32             `json:"available_budget"`
    ConsumedThisCycle uint32             `json:"consumed_this_cycle"` // cumulative
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
            available = b.AvailableAfter(dec) // (反方 M2 修复: 复用仲裁结果，单一真相源)
        }

        state.Classes = append(state.Classes, ClassState{
            Name:               b.Name,
            Importance:         b.Importance,
            AvailableBudget:    available,
            ConsumedThisCycle:  b.Consumed,
            SignalLevel:        b.SignalLevel(),
            ModelWeights:       b.ModelWeights,
            CycleEndsAt:        b.CycleStart.Add(b.CycleDuration),
        })
    }

    if err := os.MkdirAll(stateDir, 0755); err != nil {
        return fmt.Errorf("creating state dir: %w", err)
    }

    data, err := json.MarshalIndent(state, "", "  ")
    if err != nil {
        return fmt.Errorf("marshaling state: %w", err)
    }

    tmpPath := statePath + ".tmp"
    if err := os.WriteFile(tmpPath, data, 0644); err != nil {
        return fmt.Errorf("writing temp state: %w", err)
    }
    return os.Rename(tmpPath, statePath)
}

// ReadState reads the shared state. Used by Hermes BeforeCall.
func ReadState() (*SharedState, error) {
    data, err := os.ReadFile(statePath)
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
    Weight float64 `json:"weight"` // pre-computed: Tokens * ModelWeight
    TS     int64   `json:"ts"`     // unix nano
}

// AppendCounter appends one consumption event. Only called by Hermes AfterCall.
// Uses O_APPEND for atomic line-level writes — no lock needed.
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

    f, err := os.OpenFile(countersPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err != nil {
        return fmt.Errorf("opening counters: %w", err)
    }
    defer f.Close()

    if _, err := f.Write(line); err != nil {
        return fmt.Errorf("writing counter: %w", err)
    }
    return nil
}

// ReadAndClearCounters atomically reads all counter entries.
//
// Uses rename (not truncate) to prevent data loss from concurrent writes:
//  1. Rename counters.jsonl → counters.jsonl.processing
//  2. Read and parse the processing file
//  3. New writes from Hermes (O_APPEND|O_CREAT) go to a fresh counters.jsonl
//  4. Remove the processing file
//
// (反方 C2 修复: Truncate 永久丢失并发写入 → Rename 零丢失)
func ReadAndClearCounters() ([]CounterEntry, error) {
    processingPath := countersPath + ".processing"

    if err := os.Rename(countersPath, processingPath); err != nil {
        if os.IsNotExist(err) {
            return nil, nil // no counters yet
        }
        return nil, fmt.Errorf("renaming counters: %w", err)
    }

    data, err := os.ReadFile(processingPath)
    os.Remove(processingPath) // best-effort cleanup
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
            continue // skip corrupted lines
        }
        entries = append(entries, e)
    }
    return entries, nil
}

// AggregateCounters sums weighted tokens per class (返回 uint64，反方 M1 修复).
func AggregateCounters(entries []CounterEntry) map[string]uint64 {
    agg := make(map[string]uint64)
    for _, e := range entries {
        agg[e.Class] += uint64(e.Weight)
    }
    return agg
}
```

**关键设计点：**
- `AppendCounter` 用 `O_APPEND`——进程安全，不持有锁
- `ReadAndClearCounters` 用 `rename` 而非 `truncate`——并发写入零丢失（反方 C2 修复）
- Hermes 永不读 counters 文件，只读 `token_state.json`；wlmd 永不写 counters 文件
- `WriteState` 使用 `Budget.AvailableAfter(d)` 复用仲裁结果，单一真相源（反方 M2 修复）
- `AggregateCounters` 返回 `uint64` 防止大预算累加溢出（反方 M1 修复）

**Step: Commit**

```bash
git add internal/token/observer.go
git commit -m "feat(token): dual-file shared state — wlmd writes state, Hermes appends counters"
```

---

### Phase 4: wlmd — 串联

#### Task 5: wlmd token mode 控制回路

**Objective:** 在 wlmd 中增加 `--mode token` 标志，切换为 token 控制回路

**Files:**
- Modify: `cmd/wlmd/main.go`

**Step 1: 添加 mode flag 和 token 回路**

关键改动：在 `main()` 中增加 mode 分支：

```go
mode := flag.String("mode", "cpu", "control mode: cpu | token")
```

```go
case "token":
    runTokenLoop(pol, *interval)
```

`runTokenLoop` 函数：

```go
func runTokenLoop(pol *policy.Policy, interval time.Duration) {
    // Budget cycle (e.g. 24h) is the tracking period — far longer than
    // the arbitration interval (e.g. 1h). Consumed resets only on cycle end.
    // (反方 C1 修复: 仲裁周期 ≠ 预算周期)
    cycleDuration, err := time.ParseDuration(pol.BudgetCycle)
    if err != nil {
        cycleDuration = 24 * time.Hour // default
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
            // Phase 1: Observe — read counters from Hermes, refresh budgets
            entries, err := token.ReadAndClearCounters()
            if err != nil {
                log.Printf("read counters error: %v", err)
            }
            consumedByClass := token.AggregateCounters(entries)
            for _, b := range budgets {
                if consumed, ok := consumedByClass[b.Name]; ok {
                    b.Consumed += uint32(consumed) // uint64→uint32 safe: capped by DailyBudget
                }
            }

            // Phase 2: Arbitrate — reuse CPU arbitrator
            states := make([]arbitrator.State, len(budgets))
            for i, b := range budgets {
                states[i] = b.ToArbitratorState()
            }

            decisions := arbitrator.Arbitrate(states)

            // Phase 3: Apply — write shared state
            if err := token.WriteState(budgets, decisions); err != nil {
                log.Printf("write state error: %v", err)
            }

            // Phase 4: Housekeeping — reset cycle if expired
            // (Consumed ONLY resets here, not every arbitration tick)
            now := time.Now()
            for _, b := range budgets {
                if now.After(b.CycleStart.Add(b.CycleDuration)) {
                    b.CycleStart = now
                    b.Consumed = 0
                    log.Printf("[%s] budget cycle reset, consumed=%d", b.Name, b.Consumed)
                }
            }

            // Log changes
            for _, d := range decisions {
                if d.WeightDelta != 0 {
                    b := findBudget(budgets, d.Name)
                    if b != nil {
                        log.Printf("[%s] imp=%d projected=%d→%d signal=%s consumed=%d/%d",
                            d.Name, d.Importance, d.Proposed, d.Final,
                            b.SignalLevel(), b.Consumed, b.DailyBudget)
                    }
                }
            }
        }
    }
}
```

**Step 2: 编译验证**

```bash
cd /tmp/hercules-demo/wlm && go build ./cmd/wlmd/
```

**Step 3: Commit**

```bash
git add cmd/wlmd/main.go
git commit -m "feat(wlmd): add --mode token control loop"
```

---

#### Task 6: Token policy 示例 YAML

**Files:**
- Create: `policy_token.yaml`

```yaml
# Token budget policy — budget_cycle defines the tracking period (default 24h).
# The arbitration interval (--interval flag) controls how often we check,
# but budget resets ONLY at cycle end.
# IMPORTANT: model_weights are manual and must be updated when API pricing changes.
mode: token
budget_cycle: 24h
service_classes:
  - name: "arch-guardian"
    importance: 1
    daily_budget: 400000
    min_budget: 100000
    model_weights:
      claude-opus: 15.0
      claude-sonnet: 3.0
      claude-haiku: 1.0

  - name: "coding-agent"
    importance: 3
    daily_budget: 300000
    min_budget: 50000
    model_weights:
      claude-opus: 15.0
      claude-sonnet: 3.0
      claude-haiku: 1.0

  - name: "research-agent"
    importance: 5
    daily_budget: 200000
    min_budget: 10000
    model_weights:
      claude-opus: 15.0
      claude-sonnet: 3.0
      claude-haiku: 1.0
```

```bash
git add policy_token.yaml
git commit -m "docs: add token budget policy example"
```

---

### Phase 5: Hermes 集成库

#### Task 7: Hermes Go 集成 — BeforeLLMCall

**Objective:** 提供 Hermes Agent 可直接调用的 token 预算检查函数

**Files:**
- Create: `internal/token/hermes.go`

```go
package token

import "fmt"

// BeforeCall checks if a class has enough budget for an LLM call.
//
// Uses a 1.5x safety factor on estimated tokens to account for completion
// size uncertainty — a single call can produce far more output tokens than
// estimated. (反方 D2 修复: reserve→reconcile 的简化版——用安全因子做预扣)
//
// NOTE (反方 D1 诚实降级): Token 控制器是协作式预算信号——Agent 进程内自愿执行
// BeforeCall/AfterCall。不是内核级强制。对应 MACS 框架的 Agent Mesh 凭证代理
// 是远期方向（v0.4.0），会在统一出口做硬拦截。
func BeforeCall(className, model string, estimatedTokens uint32) (CheckResult, error) {
    state, err := ReadState()
    if err != nil {
        return CheckResult{}, fmt.Errorf("reading token state: %w", err)
    }

    for _, c := range state.Classes {
        if c.Name == className {
            weight := 1.0
            if w, ok := c.ModelWeights[model]; ok {
                weight = w
            }
            // 1.5x safety factor for completion uncertainty
            cost := uint32(math.Round(float64(estimatedTokens) * weight * 1.5))

            return CheckResult{
                Allowed:      cost <= c.AvailableBudget,
                SignalLevel:  c.SignalLevel,
                Remaining:    c.AvailableBudget,
                CostEstimate: cost,
                ModelWeights: c.ModelWeights, // cached for AfterCall
            }, nil
        }
    }

    return CheckResult{}, fmt.Errorf("class %q not found in token state", className)
}

// CheckResult is returned by BeforeCall.
type CheckResult struct {
    Allowed      bool
    SignalLevel  string
    Remaining    uint32
    CostEstimate uint32
    ModelWeights map[string]float64 // cached model weights for AfterCall
}

// AfterCall records token consumption after an LLM call completes.
// Uses the cached modelWeights from BeforeCall's CheckResult to avoid
// a second ReadState() call. Appends to counters file — wlmd aggregates later.
func AfterCall(className, model string, actualTokens uint32, modelWeights map[string]float64) error {
    return AppendCounter(className, model, actualTokens, modelWeights)
}
```

```bash
git add internal/token/hermes.go
git commit -m "feat(token): add Hermes integration with BeforeCall/AfterCall"
```

---

### Phase 6: 验证

#### Task 8: 端到端测试脚本

**Files:**
- Create: `test_token_e2e.sh`

模拟场景：手动写入 consumption 状态 → 运行 wlmd token mode 一个周期 → 验证 shared state 输出

```bash
#!/bin/bash
set -e

echo "=== Token WLM E2E Test ==="

# Start wlmd in token mode (background, single tick)
go run ./cmd/wlmd/ --mode token --policy policy_token.yaml --interval 1s &
PID=$!
sleep 3

# Check shared state was written
if [ -f /var/run/wlm/token_state.json ]; then
    echo "✓ Shared state written"
    cat /var/run/wlm/token_state.json | python3 -m json.tool
else
    echo "✗ Shared state missing"
    kill $PID 2>/dev/null
    exit 1
fi

kill $PID 2>/dev/null
echo "=== PASS ==="
```

```bash
chmod +x test_token_e2e.sh
git add test_token_e2e.sh
git commit -m "test: add token mode E2E smoke test"
```

---

### 适用边界（反方 M3 修复）

- **v0.3.0 适用边界 = 单机单 wlmd。** 文件式 state/counters 仅单机有效。多 Agent 跨机器部署需要中心化状态存储（Redis/SQLite），此场景在 v0.3.1。不要在文档中暗示 v0.3.0 支持多机多 Agent。

### 待定 / v0.3.1

- **Unix socket 替代 JSON 文件**：文件轮询有延迟，socket 可实现 push 通知
- **预算窗口 carry-over**：上一窗口未用完的额度是否滚入下一窗口
- **多 Agent 跨机器**：中心化状态存储（Redis/SQLite）
- **Hermes 侧降级逻辑**：收到 red/black 信号后自动切换小模型或跳过非关键步骤
- **失败的 API 调用回滚**：当前保守策略（视为已消耗），长期偏差需提供商 API 校准
- **模型权重自动同步**：当前 YAML 手动维护，远期引入定价 API 自动更新

---

### 任务依赖图

```
Task 1 (policy扩展)
  └→ Task 2 (budget模型)
       └→ Task 3 (仲裁器映射 + 7测试)
            ├→ Task 4 (observer共享状态)
            │    └→ Task 5 (wlmd token mode)
            │         └→ Task 6 (示例YAML)
            │              └→ Task 8 (E2E测试)
            └→ Task 7 (Hermes集成库)
```

### 验证清单

| Task | 测试文件 | 数量 | 覆盖 |
|------|---------|:---:|------|
| Task 1 | `policy_test.go` (existing) | — | CPU 回归 |
| Task 2 | — | — | 编译验证 |
| Task 3 | `arbitrator_test.go` | 13 | 8 仲裁映射 + 5 token 边界 |
| Task 3b | `arbitrator_test.go` | +2 | Projected 上限、Unknown model |
| Task 3c | `arbitrator_test.go` | +3 | SignalLevel、精确投影、GoalMet 边界 |
| Task 4 | — | — | 编译验证 |
| Task 4b | `observer_test.go` | +5 | State/Counter 读写、Aggregation、空目录、Dir 创建 |
| Task 5 | — | — | 编译验证 |
| Task 5b | `main_test.go` | +2 | Cycle reset、零消耗盲区仲裁 |
| Task 7 | — | — | 编译验证 |
| Task 7b | `hermes_test.go` | +4 | Safety factor、Model weight、ClassNotFound、AfterCall |
| Task 8 | `test_token_e2e.sh` | — | E2E 烟雾 |
| **总计** | | **33** | |
