# 反方挑战日志 — Token 预算控制器 v0.3.0 实施规划

> 挑战对象：`docs/plans/2026-06-17-token-controller.md`
> 日期：2026-06-17 ｜ 挑战者：邝谧 / 深度架构
> 体例：每条 finding 标 级别 / 位置(file:line) / 论点 / 影响 / 建议修法 / 状态。状态默认「待裁决」，采纳后改「已修」并回填 commit。

## 摘要

骨架（observe→arbitrate→apply + 双文件单写者）成立，问题集中在**把 CPU 仲裁器套到 token 上时低估了两者的语义差**：CPU 是可再生比例、内核强制；token 是消耗性存量、协作式约束。由此引出 3 条致命（会让"预算控制"名不副实）+ 4 条设计存疑 + 3 条潜伏。最该先动：C1（窗口/预算单位）、C2（丢消耗）、D4（新代码无测试）。

---

## 致命（会导致预算控制实际失效）

### C1 — `DailyBudget` 单位与 1h 窗口冲突，日上限不存在
- **级别**：致命
- **位置**：`internal/token/budget.go:134`（BudgetRate）、`cmd/wlmd/main.go:851-853`（窗口重置）、`internal/policy/policy.go:44` vs `internal/token/budget.go:95`（单位注释自相矛盾）、`policy_token.yaml`（`daily_budget: 400000` + `budget_window: 1h`）
- **论点**：`BudgetRate = DailyBudget / WindowSize`。示例配置下 = 400000 / 3600s ≈ 111 tok/s，等于允许"日预算"在每个 1 小时窗口内全部花掉；窗口到期又 `Consumed = 0` 满血复活。于是 400k/天 实际 = 400k×24 = 9.6M/天。policy.go 注释写"tokens/day"，budget.go 写"per window"，两处矛盾。
- **影响**：文章头条场景（"一天 100 万额度，下午 2 点耗尽"）被此设计直接证伪——它一小时刷新一次，永远耗不尽。日级预算约束形同虚设。
- **建议修法**：明确语义二选一并统一命名——(a) 保留日预算语义：`BudgetRate = DailyBudget / 86400`，窗口仅作仲裁周期、重置不清零累计；或 (b) 改名 `WindowBudget` 且窗口设为真正的预算周期（如 24h），但需与"1h 仲裁一次"解耦（仲裁周期 ≠ 预算周期）。
- **状态**：待裁决

### C2 — `read→truncate` 永久丢失并发写入的消耗，"下次补回"说法错误
- **级别**：致命
- **位置**：`internal/token/observer.go:699-711`（ReadAndClearCounters）、注释 `observer.go:741`
- **论点**：先 `ReadFile` 再 `os.Truncate(path,0)`。两步之间 Hermes 用 O_APPEND 写入的新行被 truncate 直接删除，不是延迟到下一周期。注释"可接受：下次仲裁周期补回"是错的——truncate 是删除，不是结转。
- **影响**：预算控制器系统性少计消耗 = 系统性放行超支。并发越高、仲裁周期越短，丢失越多。
- **建议修法**：read 后改用 `rename(countersPath, countersPath+".processing")` 再读 `.processing`，新写入落到原路径的新文件（写者 O_APPEND|O_CREAT 自动新建）；或记录已读字节 offset，只 `truncate` 到该 offset（需配合写者侧文件锁或 inode 重建语义）。rename 方案最简且无锁。
- **状态**：待裁决

### C3 — 仲裁对象是"投影"而非"已用额度"，早窗口低消耗被误判为 surplus
- **级别**：致命（语义）
- **位置**：`internal/token/arbitrator.go:225-235`（ToArbitratorState，Proposed = Projected）、`internal/token/budget.go:138-145`（Projected 由 BurnRate 外推）
- **论点**：仲裁器吃的是窗口末**预测**消耗。窗口前期消耗少的 Agent → burn rate 低 → Projected 低 → 被判为"有 surplus"，额度被预先转给高 importance 的超支类。但"安静 20 分钟后突然爆发"是长任务 Agent 的常态。CPU 能这么抢是因为可再生（这一刻让出下一刻要得回）；token 是消耗性存量，基于早期低速率把别人额度判没＝惩罚"还没开始干活"。
- **影响**：与"CPU 仲裁器零改动复用"这一核心卖点直接冲突——复用的代价是引入了一个对消耗性资源不成立的再分配语义。
- **建议修法**：仲裁的不应是"投影总量"，而是"剩余可分配额度（Remaining）的再分配"，且对未达到最低观测窗口的类不计入 surplus（视为预留满额）；或对 surplus 认定加"已稳定消费"前置条件，避免把"尚未开始"误读为"用不完"。
- **状态**：待裁决

---

## 设计存疑

### D1 — 执行是协作式（advisory），非强制；与"侵入式拦截"宣称不符
- **级别**：高
- **位置**：`internal/token/hermes.go:939`（BeforeCall）、对照 CPU 版 apply 写 `cpu.weight`
- **论点**：CPU 版 apply 写内核 cgroup，强制、不可绕过。Token 版 apply 只写 `token_state.json`，靠 Hermes 自觉调 BeforeCall 读取判断。不调 BeforeCall、或第三方厂商 Agent，完全不受约束。
- **影响**：文章"Token 控制器的 apply 是侵入式的——请求被拦截"与机制不符（拦截发生在 Agent 自己进程内，是君子协定）。规划/文章不应宣称与 CPU 版对称。
- **建议修法**：要么承认是"协作式预算信号"（诚实降级表述）；要么把强制点上移到统一出口（如所有 LLM 调用必经的代理/网关层做硬拦截，对应 MACS 的 Agent Mesh 凭证代理思路）。
- **状态**：待裁决

### D2 — `BeforeCall` 用 estimatedTokens 门控，completion 大小事前不可知，无 reserve/对账
- **级别**：高
- **位置**：`internal/token/hermes.go:951-957`
- **论点**：`Allowed = estimatedTokens×weight <= AvailableBudget`，但输出 token 数事前不可知（文章自己承认"100 还是 10000 不知道"）。剩 50 额度的类可放行实际烧 1 万的调用。
- **影响**：门是软的，超支可在单次调用内发生。
- **建议修法**：引入 reserve→reconcile：BeforeCall 先按估算冻结额度，AfterCall 用实际值回填差额；估算缺失时按该 class 历史 p95 输出长度兜底。
- **状态**：待裁决

### D3 — 5 分钟盲区 × 每小时重置 = 每小时 8% 时间控制器实际关闭
- **级别**：中高
- **位置**：`internal/token/budget.go:119-124`（minObservation 回退）+ 窗口重置（main.go:851-853）
- **论点**：窗口前 5 分钟 BurnRate 回退到 BudgetRate（恒在轨）→ GoalMet 恒真 → 不触发仲裁。1h 窗口每小时重置，意味着每小时开头 8% 时间控制器对超支无反应。coding agent 并发恰能在这 5 分钟烧光。
- **影响**：与 C1 叠加，泄漏窗口结构化存在。
- **建议修法**：盲区内不要假设"在轨"，改用更保守的先验（如按 importance 设一个保守 burn 上限），或缩短 minObservation 并用 EWMA 平滑而非硬阈值切换。
- **状态**：待裁决

### D4 — 8 个测试测的是借来的仲裁器，未覆盖新增风险面（虚假信心）
- **级别**：高
- **位置**：`internal/token/arbitrator_test.go`（所有用例把 WindowStart 钉死 -12h/24h，:498、:509）
- **论点**：全部用例靠固定窗口凑 goalMet，burn-rate / Projected / 模型权重 / 窗口重置——即 C1/C3/D3 出问题的地方——一行未测，测的全是 CPU 版已覆盖的两阶段分配。规划把"8 个测试"当作与 CPU 版对等的严谨度，实为重测旧代码。文章"⚠️ 诚实标注"已承认 token 边界"尚未建模"，本测试清单坐实。
- **影响**：C1/C3/D3 没有回归网，修复后无法验证不复发。
- **建议修法**：补 token 专属边界测试——(1) 真实时间推进下 BurnRate/Projected 数值正确性；(2) 窗口重置前后 Consumed 与日上限关系；(3) 模型权重归一化（Opus 15x）；(4) 加权求和 uint32 溢出/截断；(5) 盲区期行为。
- **状态**：待裁决

---

## 潜伏 / 次要

### M1 — uint32 加权溢出 + 截断偏向超支
- **位置**：`internal/token/budget.go:108`、`internal/token/observer.go:730`（AggregateCounters）
- **论点**：`uint32(float64(tokens)*weight)` 截断而非四舍五入，系统性低估成本；Opus 15x 权重下大预算累加有 uint32 溢出风险，且 AggregateCounters 无溢出防护。
- **建议**：成本计算四舍五入；累计量升 uint64；加溢出断言。

### M2 — `ApplyDecision` 是死代码
- **位置**：`internal/token/arbitrator.go:240-250`
- **论点**：算出的 `available` 被 `_ = available` 丢弃，真正写状态在 `WriteState`（observer.go:599-606）另算一遍。逻辑重复、易分叉。
- **建议**：删除 ApplyDecision 或让 WriteState 复用它，单一真相源。

### M3 — 多机场景 punt 到 v0.3.1，而文件式共享状态在多机直接失效
- **位置**：规划"待定/v0.3.1"段
- **论点**：文件式 state/counters 仅单机有效。可"多 Agent"恰是文章与产品主战场。
- **建议**：至少在文档中明确 v0.3.0 适用边界＝单机单 wlmd；多机需中心化状态（Redis/SQLite），不要让"多 Agent"宣称覆盖到尚未支持的场景。

---

## 公允地说（成立的部分）

- **双文件单写者**（wlmd 写 state、Hermes 追加 counters，observer.go:743）是干净的去竞态思路，O_APPEND 行级原子用对了。
- **复用仲裁器** 在语义成立前提下确实经济。
- 规划对 5 分钟回退、文件轮询延迟、失败调用回滚等限制有诚实标注，态度可取。

问题不在工程实现的整洁度，在 token 与 CPU 的语义差被系统性低估。

---

## 优先级建议

1. **C1**（窗口/预算单位）— 不修则日上限是假的。
2. **C2**（丢消耗）— 不修则超支必然。
3. **D4**（新代码无测试）— 不补则 C1/C3/D3 修复无回归保障。
4. 其余按 C3 → D1 → D2 → D3 → M* 顺序。
