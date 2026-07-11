# Codex Quota Scheduler 重构决策方案（Decision Spec v1）

> **文档用途**：本文件是对《目标设计文档》（三控制器 + 统一协调层）的**决策补充**。原设计文档回答"架构长什么样"，本文件把其中留白的策略点全部定死为可实现、可测试的规则。若两份文档冲突，以本文件为准。
>
> **给审核者（Codex）的说明**：请将本文件与原设计文档、以及仓库 v0.1.6 源码（重点：`refresh.go`、`probe.go`、`scheduler.go`、`quota.go`、`state.go`、`auth.go`、`dispatch.go`）一并阅读。文末第 10 节列出了需要你重点审核的问题清单。

---

## 1. 身份与版本模型

### 1.1 账号身份键（AccountIdentity）

- **主键**：ChatGPT 账号 ID（从凭据 ID token claims 中解析的 account/user ID）。
- **次键（主键不可得时）**：账号 email，做大小写不敏感比较。
- **明确不作为身份**：CPA auth 文件名、文件路径、CPA 内部索引。这些在重新登录后可能变化。
- 插件内部配置（别名、插件优先级、分组、标签、备注、Probe 历史）全部以 AccountIdentity 为键持久化。CPA 中重新登录后只要身份匹配即保留。

### 1.2 两个正交的版本号

| 版本号 | 作用域 | 递增时机 | 保护什么 |
|---|---|---|---|
| **TierGeneration（G）** | 全局 | 最高 CPA 层的**账号身份集合**发生任何变化（增、删、层整体替换） | 防止旧层账号的请求结果写回新层状态 |
| **CredentialEpoch（E）** | 单账号 | CPA 同步发现该账号**凭据内容**变化（对凭据做内容哈希比较） | 防止用旧 refresh token 刷出的 access token 覆盖用户重新登录后的新凭据 |

**写回校验矩阵**（协调层在请求发出前 snapshot `(G, E)`，写回前复核）：

| 写回内容 | 需 G 有效 | 需 E 有效 |
|---|---|---|
| 额度数据 / 重置时间 | ✅ | ❌（旧凭据取回的额度仍是该账号的真实额度，可写回） |
| 刷新后的 access token / 凭据保存 | ✅ | ✅（E 过期 → **静默丢弃**凭据写回，记录日志） |
| Probe 窗口状态推进 | ✅ | ❌ |
| Exhausted / 临时不可用标记 | ✅ | ❌ |

**关键规则**：凭据变化（E++）**不**递增 G。层组成没变，调度不需要重置；只有凭据写回需要 E 防护。

---

## 2. 缓存状态机与调度可选性（决策 D1）

### 2.1 缓存年龄状态（每账号每次成功额度获取后重算）

```text
age = now - last_success_fetch_time

Unknown : 从未成功获取
Fresh   : age ≤ quota_refresh_interval
Aging   : quota_refresh_interval < age ≤ stale_after
Stale   : age > stale_after
```

**Aging 是纯标签**：只影响 UI 显示和排序内的轻微降权，**不触发任何刷新**。刷新的触发来源全系统只有五个：`scheduler_initial`（首次）、`scheduler_interval`（活跃期到间隔）、`scheduler_stale_recovery`（真实请求遇到 Stale）、`probe_*`（Probe 业务线）、`manual_refresh`。这条规则彻底消灭 `stale_after` 作为第二条刷新时间线的可能性。

### 2.2 Exhausted 标记的时效

- Exhausted 标记必须携带来源 reset 时间；无 reset 时间时按现行为标记 2 分钟。
- **reset 时间已过的 Exhausted 标记视同 Unknown**（不是"仍耗尽"，也不是"可用"）。调度器读取该标记时现场判断，不依赖后台任务来清除。

### 2.3 调度可选性：三档制（乐观放行 + 反馈兜底）

调度器对当前最高 CPA 层账号，按插件优先级 → 业务策略排序后，将每个账号归入三档：

```text
第一档 Preferred     : Fresh/Aging 且未耗尽、无熔断、无认证失败
第二档 Opportunistic : Unknown；Stale 且上次已知可用；Exhausted 但 reset 已过
第三档 Excluded      : 确认耗尽且 reset 未到；认证失败待重新登录；熔断开启；临时不可用
```

选取规则：

1. 在同一插件优先级层内，先取第一档，第一档为空则**允许选取第二档**（乐观放行），同时立即提交对应刷新意图（`scheduler_initial` 或 `scheduler_stale_recovery`）。
2. 第二档账号被选中后若返回 `usage_limit_reached`，沿用现有反馈机制立即标记耗尽，**不计熔断失败**——这是乐观放行的兜底。
3. 高插件优先级层全部落入第三档时，继续尝试同 CPA 层内较低插件优先级；全部账号均为第三档时才返回无法选取，进入 CPA fallback。

**这一策略保证冷启动不死锁**：插件重启、Probe 未启用、缓存全 Stale/Unknown 时，第一条真实请求仍能被服务，同时触发刷新使系统回到正常状态。

---

## 3. 统一协调层执行模型（决策 D3、D6）

### 3.1 两级资源与固定获取顺序

```text
资源一：per-account mutex（同一账号所有会修改凭据或状态的操作串行）
资源二：global semaphore，容量 = max_refresh_concurrency
        计费单位 = 单次对外 HTTP 请求（不是账号，不是序列）
```

**固定顺序（防死锁的唯一合法路径）**：

```text
acquire(account_lock)
  → 每次 HTTP 前 acquire(global_slot)
  → 发送 HTTP
  → HTTP 返回后立即 release(global_slot)
  → （序列内下一步重复上两行）
release(account_lock)
```

禁止：持有 account lock 时等待另一个 account lock；持有 global slot 时等待任何 account lock；在持有 global slot 期间做任何非 HTTP 的等待（如激活传播延迟）。

### 3.2 Probe 关键序列的原子性

`precheck 额度查询 → Probe 发送 → verify 额度查询` 三步在**一次 account lock 持有期内**完成，保证不被同账号其他操作插入。但：

- **任一步失败 → 立即释放 account lock**，将重试注册为一个全新的意图（带 Probe 自己的退避时间），绝不持锁退避。
- 若 Probe 发送与 verify 之间需要等待激活传播（建议固定小延迟，如 2–5 秒，可配置为内部策略），该等待**不持有 global slot**；若等待需要超过锁租约阈值，释放 account lock，verify 作为独立意图重新排队（verify 意图执行前重新校验 G）。
- `max_refresh_concurrency: 1`（当前默认值）下，因为槽按 HTTP 计费且请求间释放，一个 Probe 序列不会长期独占全局吞吐。

### 3.3 去重与合并规则

去重键：`(AccountIdentity, op-class, G)`。

op-class 枚举与合并规则：

| op-class | 可合并对象 | 说明 |
|---|---|---|
| `quota_read` | scheduler_initial / scheduler_interval / scheduler_stale_recovery / probe_precheck / probe_verify / manual_refresh 之间**全部可合并** | 同账号 in-flight 的额度读取只发一次，所有等待方共享结果，各自回到自己的状态机 |
| `probe_send` | **不与任何请求合并** | 它是不同类型的对外请求 |
| `token_refresh` | 同账号 in-flight 可合并 | 结果保存前校验 E |

**手动刷新的特殊性**：`manual_refresh` 无视缓存新鲜度（一定产生一次读取意图），但**可以** join 同账号 in-flight 的 `quota_read`——用户连点两次按钮不产生两个请求。

### 3.4 推荐实现模型（供 Codex 评估）

建议采用**意图队列 + 单一协调 goroutine 持有全部可变状态 + worker goroutine 只做 HTTP** 的 actor 式模型：三个控制器和 management 只向队列投递意图；协调 goroutine 做去重、锁、槽、G/E 校验和状态写回；worker 拿到已授权的请求描述后执行 HTTP 并把结果作为事件投回队列。这样绝大多数共享状态不需要显式 mutex，G/E 校验天然在单线程内完成。account lock / global slot 在该模型下退化为协调 goroutine 内的簿记，而非真正的 sync 原语。**这是推荐而非定死，请 Codex 结合 CPA 插件宿主的调用约定（`scheduler.pick` 由宿主线程同步调用？）评估可行性。**

---

## 4. Probe 与熔断的双向隔离（决策 D4）

四条硬规则：

1. **熔断不阻断 Probe**：熔断开启、半开、额度耗尽、临时不可用，均不能阻止已到期的 Probe 序列执行（原设计第七.4 节的"独立受控执行许可"）。
2. **Probe 不修改熔断**：Probe 成功不推进半开成功计数、不关闭熔断、不清零失败计数。
3. **Probe 失败不计熔断**：Probe 的任何失败（含 429、5xx、超时）只进入 Probe 自己的失败计数与退避，不进入熔断失败计数。
4. **Probe 允许修改的业务状态仅限事实纠正**：写回窗口额度数据、重置时间；当 precheck/verify 读到窗口已激活时，清除**对应窗口**的 Exhausted 标记。除此之外不触碰任何业务可用性状态。

熔断半开的成功计数**只**由真实 `scheduler.pick` 选中账号后的业务请求成功推进（沿用原设计第九节"不能被无关的静态读取推进"，此处进一步把 Probe 也排除）。

认证失败（401 且 token 恢复失败）是唯一的跨业务线状态：一旦确认凭据不可恢复，普通刷新、Probe、半开验证**全部**停止对该账号的自动请求，账号进入第三档 Excluded，UI 提示重新登录。解除条件：CPA 同步检测到 E++（用户重新登录）。

---

## 5. 时间、持久化与崩溃恢复（决策 D5）

1. **全部使用绝对时间**：每个待执行任务持久化的是"截止时刻"（deadline），定时器 tick 只是检查器。禁止任何依赖 tick 计数或相对累计的调度。
2. **启动/唤醒 = 重算，不是重放**：进程启动或检测到大幅时间跳跃（如 `now - last_tick > 2 × tick 间隔`，对应宿主机睡眠唤醒）后，对每个账号每个窗口，用 `(持久化的上次 reset 时间, 最近额度快照, now)` **推导当前应处状态**，丢弃积压的到期事件。同一窗口无论积压多少个到期点，最多产生一次 Probe 序列。
3. **只持久化意图与截止，不持久化 in-flight**：状态文件中不允许出现"正在检查""正在发送"这类执行中状态。启动恢复时，任何遗留的执行中状态一律回退为对应的 pending 态。
4. **执行租约**：任务进入执行态时记录 `startedAt`；协调层发现执行超过租约（建议 2 分钟，内部策略）即视为死任务，回收并按退避重新调度。防止 worker 挂死导致某账号永久卡锁。
5. reset 时间来自上游报告，保留现有 `refresh_after_reset_delay` 作为 reset 后的确认宽限；已消费的 reset 触发保持 one-shot（沿用 v0.1.6 已修复的行为）。

---

## 6. 边界决策（D7–D10）

**D7 · 低 CPA 层账号与 Management**：低层账号在 Management 页只展示 CPA 静态信息（存在于 CPA、层级、身份），**禁止**对其手动刷新，API 对此返回明确错误。理由：保持"活动集合是插件访问 OpenAI 的唯一面"这条不变量绝对简单，不为它开任何口子。

**D8 · `refresh_on_startup` 语义收窄**：该配置只约束普通刷新控制器。Probe 启用时，Probe 控制器的启动额度读取**不受**它控制（这是 Probe 业务线的固有行为）。配置说明与 UI 需明确写出这一点。

**D9 · CPA 同步失败降级**：沿用 last-known-good 集合，G 不变，Probe 与普通刷新照常。所有基于降级清单发出的请求在结构化日志中标记 `degraded_roster: true`，便于事后排查"为什么对已删除账号发了请求"。降级持续超过阈值（如 30 分钟，内部策略）时在 Management 页显著提示。

**D10 · 身份不可解析的账号**：凭据中既无 account ID 也无 email 的账号，不纳入活动集合，Management 页标记"身份不可识别"。不允许以文件名为键降级管理（避免重登录后配置错配）。

---

## 7. 状态机汇总（实现基准）

### 7.1 单窗口 Probe 状态机（五小时窗口、长周期窗口各一份，互不共享任何字段）

```text
Idle（未启用/账号不在最高层）
WaitingReset（持久化：预计 reset 时刻 + refresh_after_reset_delay）
  --deadline 到--> PendingCheck
PendingCheck --协调层授权--> [precheck 额度读取]
  --窗口已激活--> Confirmed（记录激活方式=natural）
  --reset 时间已更新--> Confirmed（激活方式=observed）
  --仍为 lazy--> [发送 Probe] --> [verify 额度读取]
      --已激活--> Confirmed（激活方式=probed）
      --仍未激活--> RetryWait（Probe 退避，持久化下次截止）
  --任一步失败--> RetryWait（Probe 退避；401 → AuthBlocked）
Confirmed --下一个 reset 时刻可推导--> WaitingReset
AuthBlocked --E++（重新登录）--> PendingCheck
任意状态 --账号退出最高层/G 失效--> Idle（取消所有截止）
```

持久化字段（每窗口）：上次 reset 时间、当前状态（仅限 Idle/WaitingReset/PendingCheck/RetryWait/Confirmed/AuthBlocked，无执行中态）、失败计数、下次截止时刻、最近激活方式。

### 7.2 普通刷新活跃窗口

```text
Dormant --真实 scheduler.pick--> Active（记录 last_activity；窗口截止 = last_activity + refresh_active_window）
Active 内每次 pick：延长窗口；若账号 Unknown/Stale → 提交对应刷新意图；若距上次刷新 ≥ quota_refresh_interval → 提交 scheduler_interval 意图
Active --窗口截止且无新 pick--> Dormant（保留全部缓存与账号，UI 标记"普通后台刷新已休眠"）
```

---

## 8. 不变量总表（Mock 测试的验收基准）

每条均应转化为至少一个自动化测试场景。

| 编号 | 不变量 |
|---|---|
| INV-01 | Resource 端点响应中不出现任何账号 ID、别名、额度、优先级、熔断或日志数据 |
| INV-02 | 任意时刻，插件对 OpenAI 的请求只针对当前最高 CPA 层账号（手动刷新亦然） |
| INV-03 | G 失效后返回的结果不写回活动状态；不产生成功日志误导 |
| INV-04 | E 失效后返回的凭据不覆盖新凭据（构造竞争：in-flight token 刷新期间用户重登录） |
| INV-05 | 同账号同 op-class 的 in-flight 请求最多一个；等待方共享同一结果 |
| INV-06 | `probe_send` 永不与其他请求合并 |
| INV-07 | Probe 三步序列之间不被同账号其他写操作插入 |
| INV-08 | Probe 序列任一步失败后，account lock 在有界时间内释放，普通刷新可继续 |
| INV-09 | 全局并发按 HTTP 请求计：并发=1 时，Probe 等待期内其他账号仍能发出请求 |
| INV-10 | `stale_after` 在任何时间线上不独立触发请求（仅真实请求遇 Stale 时触发一次 stale_recovery） |
| INV-11 | Aging 状态不触发请求 |
| INV-12 | 全部账号 Unknown/Stale 时，首条真实请求能选出账号（乐观放行），且触发且仅触发一轮刷新 |
| INV-13 | Exhausted 且 reset 已过的账号按 Unknown 处理，可被乐观选取 |
| INV-14 | 熔断开启不阻断到期 Probe；Probe 成功/失败均不改变熔断计数与状态 |
| INV-15 | 半开成功计数只被真实业务请求推进 |
| INV-16 | `usage_limit_reached` 不递增熔断失败计数（回归保护） |
| INV-17 | 五小时窗口与长周期窗口的状态、退避、截止互不影响；一次 Probe 请求的结果可同时更新两窗口数据但不合并任务状态 |
| INV-18 | 模拟时间跳跃（睡眠唤醒）后，同一窗口至多产生一次 Probe 序列，无积压重放 |
| INV-19 | 状态文件中不存在执行中状态；从任意持久化状态启动均能收敛到合法状态 |
| INV-20 | 账号从 CPA 删除或降层后，其所有待执行截止被取消，后续无任何对该账号的请求 |
| INV-21 | CPA 同步失败时活动集合不清空、G 不变，降级请求带 `degraded_roster` 标记 |
| INV-22 | 普通刷新休眠（Dormant）期间账号卡片与缓存保留，且已启用的 Probe 照常到期执行 |
| INV-23 | 认证不可恢复后，该账号所有业务线自动请求停止；E++ 后自动恢复 |
| INV-24 | 结构化日志中每个对外请求都有八种来源标签之一，且与实际触发方一致 |

---

## 9. 实施顺序（映射到现有仓库结构）

每个阶段可独立合入、独立验证，前一阶段的测试在后续阶段持续通过。

**Phase 0 · 地基（无行为变化）**
`models.go` 引入 AccountIdentity、TierGeneration、CredentialEpoch 类型与 `(G,E)` snapshot 结构；`state.go`/`disk_state.go` 迁移持久化键到 AccountIdentity（含旧格式迁移）；新增 `coordinator.go` 骨架（意图队列、op-class、来源标签），此阶段仅透传。验收：全部现有测试通过，状态文件迁移测试。

**Phase 1 · 协调层接管所有对外请求**
`quota.go`、`auth.go`、`probe.go`、`management.go` 的全部 OpenAI/CPA 访问改为提交意图；实现去重、account lock、按 HTTP 计费的 global slot、G/E 写回校验、日志来源标签。验收：INV-02～09、24。

**Phase 2 · 缓存状态机与调度可选性**
`quota.go` 实现四态缓存与 Exhausted 时效判断；`scheduler.go` 实现三档可选性与乐观放行；`refresh.go` 删除 stale 独立时间线，只保留活跃窗口 + interval + stale_recovery。验收：INV-10～13、22 及第 7.2 节时间线场景（10:00/10:20/10:30/11:00/11:20 用例）。

**Phase 3 · Probe 状态机重写**
`probe.go` 按第 7.1 节双窗口状态机重写；绝对时间截止、启动重算、租约回收；与熔断双向隔离。验收：INV-14～19。

**Phase 4 · CPA 同步控制器与 Management 收尾**
同步 TTL、失败降级、D7 手动刷新边界、D8 配置语义、UI 状态展示（休眠标记、过期标记、Probe 双窗口状态、最后请求来源）。验收：INV-01、20、21、23 与第十一节展示要求。

---

## 10. 提交给 Codex 的审核任务清单

请 Codex 结合本文件 + 原设计文档 + v0.1.6 源码，重点回答：

1. **宿主约定冲突**：`scheduler.pick` 由 CPA 宿主以何种线程模型调用？第 3.4 节的 actor 模型在 c-shared 插件（CGO 回调）约束下是否可行？pick 路径上"提交意图"是否会引入不可接受的延迟或跨 goroutine 阻塞？
2. **锁模型证伪**：按第 3.1 节的固定获取顺序，构造反例证明是否仍存在死锁或活锁（特别是 Probe verify 重新排队与 token_refresh 合并交织的路径）。
3. **G/E 模型漏洞**：找出 G 与 E 覆盖不到的写回竞争。例如：同一账号在两次 CPA 同步之间被"删除又加回"（身份相同、G 递增两次），in-flight 请求应如何判定？E 的哈希比较对凭据字段顺序变化是否稳健？
4. **乐观放行的风险面**：第二档放行是否可能在多账号同时 Unknown 时造成对多个耗尽账号的连续真实请求打击？是否需要在 pick 层面对第二档增加单位时间放行上限？
5. **Probe 序列的锁租约与传播延迟**：verify 延迟阈值、锁租约、Probe 退避三者的取值关系是否存在导致 verify 永远排不上的组合？
6. **崩溃恢复完备性**：对第 7.1 节状态机做穷举——每个持久化状态 × 启动时的额度/时间条件，验证第 5.2 节的重算规则都能收敛且不重放副作用。
7. **不变量表缺口**：INV-01～24 是否漏掉了设计中承诺的规则？请补充可测试的新不变量。
8. **实施顺序风险**：Phase 1 一次性接管全部对外请求是否过大？是否应先只接管 quota_read？
9. **与 CPA issue #4196 的耦合**：若上游 fallback 行为修复，本设计哪些决策（尤其 D7、最高层隔离）需要预留演进路径？
10. **性能与资源**：意图队列 + 每 tick 重算在几十账号规模下的开销评估；状态文件写入频率是否需要合批。

---

## 附：本方案定死的决策一览

| 决策 | 一句话结论 |
|---|---|
| D1 缓存与调度 | 四态缓存 + 三档可选性；乐观放行 + usage_limit_reached 兜底；Aging 纯标签 |
| D2 凭据竞争 | 引入 per-account CredentialEpoch，凭据写回双重校验 (G,E) |
| D3 并发模型 | account lock 串行 + global slot 按 HTTP 计费；固定获取顺序；Probe 序列持锁但失败即释放 |
| D4 Probe×熔断 | 双向完全隔离；Probe 仅做事实纠正；半开只由真实业务推进 |
| D5 时间与恢复 | 绝对时间截止；启动/唤醒重算不重放；不持久化 in-flight；执行租约 |
| D6 去重 | 键 = (身份, op-class, G)；quota_read 全线可合并；probe_send 不合并；手动刷新 bypass 新鲜度但可 join |
| D7 低层账号 | Management 禁止对低层账号手动刷新 |
| D8 refresh_on_startup | 只约束普通刷新，不约束 Probe 启动读取 |
| D9 同步降级 | last-known-good + degraded_roster 日志标记 |
| D10 身份不可解析 | 不纳入活动集合，不以文件名降级管理 |
