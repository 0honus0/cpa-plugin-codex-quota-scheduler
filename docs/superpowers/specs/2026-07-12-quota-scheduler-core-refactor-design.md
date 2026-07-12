# Codex Quota Scheduler Core Refactor Design

## Status

Approved and frozen on 2026-07-12. This is the Superpowers-formatted authoritative design for implementation stages S0 through S7.

## Source and preservation rule

This document adapts the jointly reviewed Decision Spec v5 into the repository's Superpowers design location. The frozen specification below is preserved in full. Formatting metadata in this preface clarifies workflow only and does not weaken, replace, or reinterpret any normative rule.

**Erratum:** the frozen v5 changelog says the full Mock design and automation protocol were added as §14 and §15. Their final locations are §12 and §13. This preface corrects only that historical section-number reference; the frozen text remains unchanged.

## Goal

Refactor the plugin into three independent controllers backed by one coordinated request layer, while preserving the Management security boundary, admitting only the highest CPA priority tier, keeping scheduler pick free of I/O, and making refresh, Probe, identity, persistence, and crash recovery behavior executable through invariant-driven tests.

## Scope

- Implement stages S0 through S7 serially.
- Preserve INV-01 through INV-46, all frozen numeric defaults, state machines, decision tables, capability branches, and the full Mock test design.
- Use test-driven development and machine-verifiable stage gates.
- Route implementation ambiguities through the frozen deviation protocol instead of revising this design.

## Non-goals

- Modifying CPA source code.
- Treating scheduler candidates as an authoritative roster.
- Reopening approved product or architecture decisions through a new brainstorming cycle.
- Publishing, pushing, tagging, releasing, or mutating remote issues as part of this documentation conversion.

## Authority and conflict resolution

The frozen rules below remain the single source of truth. During implementation, resolve conflicts in this order: invariants, state machines and decision tables, normative prose, then examples. If that order cannot decide the issue, prefer no outbound request, discard unsafe writeback, and wait or back off; record the result in `docs/deviations.md`.

---

## Frozen Decision Specification v5

# Codex Quota Scheduler 重构决策规格（Decision Spec v5 · FINAL）

> **文档地位**：唯一生效规格（single source of truth），自包含，无历史版本依赖。与《目标设计文档》冲突时以本文件为准。
>
> **冻结声明**：本版本为最终修订。此后实现期发现的任何矛盾或缺口，**不再回改本文件**，按 §13 偏差协议现场裁决并记录。上游依赖（CPA 契约确认）与人工出口（CredentialAmbiguous）均已定义自动默认路径，开发流程可在无人工介入下完成（§13）。
>
> **v4 → v5 变更记录**：吸收 S1 本地调查结论（`host.auth.list` 带 Priority 已存在；candidates 非 roster；无 revision/CAS；pick 为同步热路径）；修正四个规格阻断（WaitingRoster 恢复映射、Baseline tagged union、INV-07 跨租约语义、凭据 WAL 结果协议）；指纹拆分四分量；刷新来源唯一优先级；INV-02/34 增列 provisional 显式例外；trial evidence 预算；未知 window_len 绝对上限；CredentialAmbiguous 出口定义；S3 drain cancel/join 与 SentUnknown 继承；新增 INV-43～46；新增 §14 全量 Mock 测试设计与 §15 自动化执行协议。

---

## 0. 系统概览与安全边界

三个控制器 + 一个协调层：**CPA 账号同步控制器**（管理哪些账号）、**普通额度刷新控制器**（维护额度新鲜度）、**Probe 控制器**（激活 lazy 窗口）、**统一请求协调层**（去重、并发、认证、安全写回）。控制器决定"为什么、什么时候"；协调层决定"怎样安全执行"。

**安全边界**：Resource 端点只提供 HTML/CSS/JS/图标等通用静态资源，不含任何账号 ID、别名、优先级、额度、重置时间、熔断状态、调度记录或缓存业务数据。业务数据与状态变更经 Management 端点，需 CPA 管理密钥；密钥只存浏览器页面会话。

**刷新触发来源（全系统仅五类，其余机制不得触发对外额度请求）**：
`scheduler_initial`（首次）、`scheduler_interval`（活跃期到间隔）、`scheduler_stale_recovery`（真实请求遇 Stale）、`probe_*`（`probe_startup/probe_precheck/probe_activation/probe_verify`）、`manual_refresh`。

**来源唯一优先级**：一次 pick 对同一实例可能同时满足多个条件，只产生**一个**意图，按 `scheduler_initial > scheduler_stale_recovery > scheduler_interval` 取最高者。每个对外请求带且只带一个来源标签（S3 迁移期允许过渡标签 `legacy_refresh_txn`，S6 移除）。

---

## 1. 宿主契约（S1 结论，规范性）

### 1.1 `host.auth.list`：roster 权威来源

CPA（本地验证版本 v7.2.42）的 `host.auth.list` 枚举账号并返回 `Priority`（ABI types.go:76；host_callbacks.go:98；auth_callbacks.go:135/455；pluginapi types.go:668）。规范流程：

```text
插件启动 → host.auth.list → 筛选 provider=codex → 取最高 CPA Priority → 加载该层全部账号
```

**能力检测（运行时代码化，不等待上游答复）**：启动及每次同步调用 `host.auth.list`；成功且 entries 含 Priority 字段 → **Capability-A**（权威 roster 路径，§4.1）；调用失败或字段缺失 → **Capability-B**（回退路径，§4.2）。两分支均完全规格化，上游"是否正式稳定契约"的确认为**非阻塞任务**（结果只影响文档措辞与最低版本标注，不影响代码分支）。上游需求单（非阻塞提交）：正式化带 Priority 的 roster 契约与原子一致性；不可复用 incarnation/revision；删除 tombstone/change sequence；`SaveAuth(expected_revision)` CAS。

### 1.2 `scheduler.pick.candidates`：单次授权域，不是 roster

CPA 构造 candidates 前已按 provider、disabled、pinned、tried、model support、可用状态、路由条件过滤（conductor.go:4112/1450；scheduler.go:74）。因此：

- candidates 仅限定**本次请求**插件允许返回的账号集合。
- **禁止**据 candidates 推断账号缺失、删除账号、重算最高层或修改任何 roster 状态（INV-44）。
- **选取域 = candidates ∩ 插件活动层**；交集为空 → 返回无法选取，进入 CPA fallback。
- candidates 中出现活动层外账号（同步滞后）时，不选取、不写回，仅计数日志。

### 1.3 `scheduler.pick`：同步热路径规范

宿主同步等待返回，多个 pick 可并发进入，无明确延迟预算。规范（INV-43）：

- pick 只读**协调层发布的不可变内存快照**（原子指针，copy-on-write），执行纯函数排序与选取后立即返回。
- pick 路径零网络、零磁盘、零后台等待；不等待 roster 同步、不等待落盘。
- 副作用异步化：刷新意图、活跃窗口延长经有界无锁队列投递（队满丢弃并计数——刷新意图可丢，下次 pick 重试）。**唯一例外**：optimistic trial 开启标记为 pick 路径可原子 CAS 的独立结构，开启即刻对并发 pick 可见（保证 INV-29）。

---

## 2. 身份、实例与版本模型

### 2.1 三个"是谁"

| 概念 | 定义 | 用途 |
|---|---|---|
| **AccountIdentity** | ChatGPT 账号 ID（凭据 claims），次键 email（大小写不敏感） | 仅用于继承插件配置：别名、插件优先级、分组、标签、备注、Probe 历史 |
| **AuthInstanceID** | CPA 宿主标识的认证实例 | 操作单位：admission、锁、去重、请求、写回、日志 |
| Identity↔Instance 绑定 | CPA 同步维护；一对零/一/多 | 多实例独立调度，共享 Identity 配置 |

### 2.2 版本号族

| 版本号 | 作用域 | 递增时机 | 用途 |
|---|---|---|---|
| **InstanceAdmissionEpoch（IAE）** | 每 Instance | 宿主 revision（当前不存在，预留）；插件维护时：观察到删除重加、绑定指纹分量变化、LoginEpoch++ | 进入 G；写回门控。**best-effort fencing，非强身份**（§2.6） |
| **TierGeneration（G）** | 全局 | 最高层 **(AuthInstanceID, IAE) 多重集**变化 | 层级写回防护 |
| **AuthBindingEpoch** | 每 Identity | Identity↔Instance 绑定变化 | 配置继承一致性 |
| **LoginEpoch** | 每 Instance | §2.4 判定为外部重新登录 | 唯一解除 AuthBlocked 的信号 |
| **TokenEpoch** | 每 Instance | 判定为自有轮换 | 仅日志；不门控、不解锁 |
| **ExecutionToken** | 每次授权异步执行 | 协调层签发 | worker 写回 fencing；Management 本地修改不依赖它 |

### 2.3 绑定指纹（四分量）

分别持久化（只存哈希，从不存原文）：`subject_hash`、`refresh_token_hash`、`normalized_binding_metadata_hash`、`composite_hash`。分类算法（§2.4）按分量判定；写回门控用 composite 快速比对，凭据保存类写回额外比对分量。

### 2.4 凭据变化分类：自有轮换转换链

每实例持久化**有序转换链**（非单指纹、非无序集合）：`F0 → F1(save_seq=10) → F2(save_seq=11)`，容量 8–16 代且时限 24h（内部策略）。

**转换 WAL 完整协议**（四态）：

```text
planned（先于 SaveAuth 落盘：F_prev → F_next, save_seq）
→ SaveAuth 调用
   → 明确成功 → applied
   → 明确失败 → aborted
   → 超时/结果未知/进程崩溃 → outcome_unknown
outcome_unknown 恢复对账：重新 GetAuth 读当前指纹
   → == F_next：applied
   → == F_prev：aborted
   → 其他：CredentialAmbiguous
```

**同步观察实例指纹 F**：

```text
F == 链游标当前指纹                → 无事件
F == 游标之后可达代（含跨代）        → 自有轮换确认，游标推进 → TokenEpoch++
F 不在可达链：
   subject_hash 变化，或 refresh_token_hash 与所有可达代不匹配
                                   → 外部重新登录 → LoginEpoch++ → IAE++ → G++
   仅 metadata 分量差异             → 忽略
链容量/时限耗尽，或对账落入歧义      → CredentialAmbiguous
```

推论（INV-33/40）：插件自身刷新永不 LoginEpoch++、永不 G++、永不解除 AuthBlocked；SaveAuth 与 applied 之间崩溃不误判外部登录；失败/未知的 planned 经对账收敛，不污染后续判定。

### 2.5 CredentialAmbiguous：自动默认 + 可选人工出口

**自动保守默认**（无人工也能运行）：冻结转换链推进；不解除 AuthBlocked；不递增 LoginEpoch/IAE；现有凭据在未 AuthBlocked 时仍可用于读取与真实请求；每次同步重试分类，后续观察若可明确归类（命中链或明确外部特征）→ 自动解除。

**可选 Management 出口**（均写审计日志）：`确认自有轮换`（游标推进至观察指纹、TokenEpoch++、解除 Ambiguous）；`确认外部登录`（LoginEpoch++、IAE++、G++、解除 AuthBlocked、链重置为新基线）；`重新读取`（触发 GetAuth 对账）。

### 2.6 残余风险声明（规范性）

无宿主 revision/tombstone 时：CPA 同步 TTL（活跃期约 5 分钟）只约束**两次正常观察之间的间隔**；Degraded 将间隔扩大到最多 `roster_degraded_max`（30 分钟）。**完全相同的删除重加**（同 InstanceID、同 refresh token、同 subject）若完整发生在任一观察间隙并恢复为相同最终状态，插件**之后可能永久无法识别该事件**——观察间隔只是漏检的机会窗口，一旦漏检，事实可能永久不可恢复；fail-closed 只能停止新的后台请求，不能恢复实例连续性。插件维护的 IAE 因此是 best-effort fencing，不是强实例身份。

### 2.7 身份不可解析实例

参与宿主真实 pick；仅以 AuthInstanceID 临时管理，不继承旧配置，重新登录后不得按文件名继承；有 account ID 可后台运行 quota/Probe；连 account ID 都不可得则不执行 quota/Probe，仅按 trial 规则参与真实请求。

---

## 3. 统一请求协调层

### 3.1 两级资源与固定顺序

```text
per-AuthInstance mutex（同实例修改凭据/状态的操作串行）
global semaphore = max_refresh_concurrency，计费单位 = 单次对外 HTTP
顺序：acquire(lock) → (每 HTTP 前 acquire(slot) → HTTP → 立即 release(slot))* → release(lock)
禁止：持 lock 等另一 lock；持 slot 等任何 lock；持 slot 做非 HTTP 等待
```

### 3.2 fence_seq 与块预留协议

全局单调 uint64；**禁止墙钟做 barrier**。`reserved_ceiling` = **已预留上界**（非最后签发值）：

```text
内存块耗尽 → 计算新 reserved_ceiling（+块大小 2^20）→ 原子持久化并 fsync
→ 落盘成功后才可签发块内序号
→（Probe）分配 send_fence_seq → 写入并 fsync sending WAL（含该序号）
→ WAL 成功后才允许发送 Probe HTTP
重启：从 persisted_reserved_ceiling + 1 预留新块
```

正确性只要求严格不回退，不要求连续（INV-38/39）。

### 3.3 去重与合并

去重键 `(AuthInstanceID, op-class, G)`。`quota_read` 携带可选 `started_after`（fence 序号），只 join `read_start_seq > started_after` 的 in-flight 读取。

| op-class | 规则 |
|---|---|
| `quota_read` | 五类来源间可合并（受各自 barrier 约束），等待方共享结果、各回自己状态机 |
| `probe_precheck` | barrier 空，可与任何 in-flight `quota_read` 合并 |
| `probe_verify` | barrier = 对应 attempt 的 send_fence_seq；不跨 attempt 合并 |
| `probe_send` | 不与任何请求合并 |
| `token_refresh` | 同实例 in-flight 可合并；保存按 §2.4 WAL 协议 |
| `manual_refresh` | bypass 新鲜度（必产生一次读取意图），可 join 同实例 in-flight `quota_read` |

### 3.4 认证管理

协调层统一：GetAuth、解析、过期判断、刷新、按转换链 WAL 保存、日志脱敏。任何控制器不得自建 token 刷新。

### 3.5 实现模型（定稿）

意图队列 + 单协调 goroutine 持有可变状态 + worker 只做 HTTP；协调 goroutine 定期发布不可变快照（原子指针）供 pick 无锁读取；trial 标记为独立 CAS 结构（§1.3）。lock/slot 为协调 goroutine 内簿记。

---

## 4. Roster 与 Bootstrap

### 4.1 Capability-A（权威 roster，当前 CPA 的默认路径）

启动即 `host.auth.list` 计算最高层（G 建立），Probe 与普通刷新按各自控制器正常运行——**Provisional/WaitingRoster 机制在 A 路径下不启用**。同步控制器行为：启动一次；完全空闲休眠；活跃期 TTL≈5 分钟；Probe 唤醒前确保 TTL 内；Management 按需；并发唤醒只执行一次 sync（INV-31）。账号变化：入层加入、删除移除并取消任务、降层退出、更高层原子替换（G++）、凭据变化走 §2.4、同步失败走 §6.3。

### 4.2 Capability-B（回退：无权威 roster）

```text
启动 → 加载 last-confirmed 为 Provisional（沿用保存的 G），Management 标记 provisional
普通刷新：Dormant（无影响）
Probe：默认全部窗口 WaitingRoster
获得任何一次权威确认（能力恢复的 host.auth.list 成功）→ Confirmed
```

B 路径下 candidates 不能替代 roster（§1.2），WaitingRoster 的解除只能来自权威确认。**Provisional Probe 为显式风险模式** `probe_on_provisional_roster`（默认 false）：确认时效 < 4h；每次 Probe 前 GetAuth 验证实例存在且指纹分量未变，失败回 WaitingRoster；Management 显著警告并注明无法证明层级归属；其请求带 `provisional: true` 标记（INV-02/34 的唯一例外）。

**迁移说明（写入 README 与升级公告）**：Capability-B 且重启后无权威确认时，默认 Probe 无限期停留在 WaitingRoster；无人值守部署需开启风险选项或升级宿主。

---

## 5. 缓存状态机与调度

### 5.1 缓存四态

`Unknown（从未成功）/ Fresh（age ≤ interval）/ Aging（≤ stale_after）/ Stale（>）`。Aging 纯标签不触发刷新；`stale_after` 不独立触发请求（INV-10/11）。

### 5.2 Exhausted 时效

标记必带 reset（缺失按 2 分钟）；reset 已过视同 Unknown，调度时现场判断（INV-13）。

### 5.3 优先级定义与三档选取

> 插件优先级：在相同可用性可信等级内，优先级越高越先使用。

```text
Preferred     : Fresh/Aging 且未耗尽、无熔断、无 AuthBlocked
Opportunistic : Unknown；Stale 且上次已知可用；Exhausted 但 reset 已过
Excluded      : 确认耗尽未到 reset；AuthBlocked；熔断开；临时不可用；trial 进行中（含 TrialUnknown 退避）

选取域 = candidates ∩ 活动层：
全部 Preferred 按插件优先级（同级内 monthly_mode/到期/剩余额度/稳定 ID 决胜）取首
→ 全空：全部 Opportunistic 同规则取首，CAS 开启 trial
→ 全部 Excluded 或交集空：无法选取 → CPA fallback
```

首版无 strict 优先级开关。绝不因高优先级耗尽直接 fallback；只有选取域全 Excluded 才 fallback。

### 5.4 Optimistic Trial（含 evidence 预算）

- 每实例至多一个 trial（CAS 即刻可见）；开启同时提交 evidence refresh 意图；trial 期间 Excluded。
- **释放仅凭真实证据**：usage feedback（成功或 limit）、可靠额度写回、真实请求成功。
- 超时（60s）时：evidence 仍在排队/执行/退避 → **不释放**；无任何 pending evidence → **TrialUnknown**（递增退避 1m→2m→5m，cap 15m）。
- **evidence 总预算**（INV-45）：自 trial 开启起墙钟 `trial_evidence_budget`（内部策略 5 分钟）或 evidence 重试 3 次（先到者）仍无真实证据 → 强制进入 TrialUnknown；证据到达即清退避。不存在任何周期性自动重新放行（INV-41）。

---

## 6. 普通刷新控制器与同步失败

### 6.1 活跃窗口状态机

```text
Dormant --真实 pick--> Active（last_activity；截止 = last_activity + refresh_active_window）
Active 内每 pick：延长窗口；按 §0 唯一优先级产生至多一个刷新意图
Active --截止且无新 pick--> Dormant
```

Dormant：后台刷新停止；实例不卸载、卡片不消失、缓存保留、Management 可读、页面标记休眠；到期 Probe 照常（INV-22/32）。时间线（interval=30m, window=1h）：`10:00 刷新；10:20 只延长；10:30 刷新；11:00 刷新；11:20 休眠`。`refresh_on_startup` 只约束本控制器。

### 6.2 失败隔离

普通刷新失败只影响自身（网络/429/5xx 退避；401 一次合法恢复；不可恢复 → AuthBlocked；本地损坏不外发）。与 Probe 不共用任何 NextRetryAt。熔断只保护真实调度；`usage_limit_reached` 标记临时耗尽、不计熔断（INV-16）。

### 6.3 同步失败：有限期 fail-open → fail-closed

真实 pick 的选取域来自 candidates，不依赖同步缓存——降级只覆盖后台。

```text
失败 → Degraded（集合不清空、G 不变；请求带 degraded_roster: true）
≤ roster_degraded_max（30m）：后台沿用 last-confirmed
> 阈值 → RosterFailClosed：后台全停（刷新意图丢弃、Probe 入 RosterHold）；真实 pick 不受影响；显著告警
恢复：任一成功 host.auth.list → 确认、解除、RosterHold 重算恢复
```

---

## 7. Probe 控制器

### 7.1 与熔断双向隔离（四条硬规则）

1. 熔断/半开/耗尽/临时不可用不阻断到期 Probe。
2. Probe 成败不推进半开、不关闭熔断、不清失败计数。
3. Probe 失败（含 429/5xx/超时）只入 Probe 自己的计数与退避。
4. Probe 仅事实纠正：写回窗口额度/重置时间；读到已激活时清除**对应窗口** Exhausted。

半开只由真实业务成功推进（INV-15）。AuthBlocked 为唯一跨业务线状态：置位后全业务线停自动请求，仅外部 LoginEpoch++ 解除。

### 7.2 双窗口状态机

两窗口各持独立状态（互不共享字段；INV-17）：

```text
Idle → WaitingReset → PendingCheck
PendingCheck --授权--> [precheck 读取] --classify-->
   ActivatedNew/ActivatedInferred → Confirmed
   NotDueYet → WaitingReset ；Anomaly → AnomalyHold（下周期重评）
   StillLazy → 分配 send_fence_seq → [WAL sending] → [send] → [WAL sent]
             → SentAwaitingVerify →（持锁传播等待 propagation_grace=3s，不持 slot）
             → [verify, barrier=send_fence_seq] --classify-->
                 Activated* → Confirmed ；StillLazy/Ambiguous → RetryWait
send 租约超时 → SentUnknown → verify-first（§7.5）
任一步失败 → RetryWait；401 → AuthBlocked --外部 LoginEpoch++--> PendingCheck
WaitingRoster --权威确认--> 按 §7.3 恢复映射
任意状态 --实例退出最高层/G 失效--> Idle（取消截止，作废 attempt）
Confirmed --可推导下一 reset--> WaitingReset
```

持久化状态集：`Idle/WaitingReset/PendingCheck/SentAwaitingVerify/SentUnknown/RetryWait/Confirmed/AuthBlocked/AnomalyHold/WaitingRoster`。attempt 记录意图激活的窗口集合；一次响应喂两窗口、各自独立 classify 推进。

**序列原子性语义（INV-07 定稿）**：同一 instance lock 租约内，precheck→send→verify 不被同实例其他写操作插入；正常传播等待在**持锁、不持 slot**下完成。跨租约恢复（崩溃、租约回收）**不保证**无插入——因果正确性由 send_fence_seq、G、IAE、ExecutionToken、attempt_id 保证；verify 的 classify 只依据额度快照与 barrier，不得把中间写入归因于 Probe。

### 7.3 Baseline（tagged union）与 WaitingRoster 恢复映射

```text
ResetBaseline     { reset_at, usage }
UsageOnlyBaseline { usage, next_recheck_at }
```

**输入分派（含 WaitingRoster 恢复，统一映射）**：

| 条件 | 处置 |
|---|---|
| snapshot 无效 / 字段类型错误 | **P1** → RetryWait |
| 无 baseline，reset_at 存在且 > now | **P2** 建 ResetBaseline → WaitingReset |
| 无 baseline，reset_at 存在且 ≤ now | **P3** 建 ResetBaseline → PendingCheck（经 delay 延迟检查；首装即 lazy 不被推迟整周期） |
| 无 baseline，reset_at 缺失 | **P4** 建 UsageOnlyBaseline（next_recheck = 30m）→ WaitingReset |
| 已有 baseline（含 WaitingRoster 恢复） | **P5** 恢复既有 baseline，按其类型进入对应表，不作首次 |

**UsageOnlyBaseline 专用表**（不得进入需要 prev_reset_at 的主表）：

| 观察 | 处置 |
|---|---|
| snapshot 无效 | RetryWait |
| reset_at 出现且 > now | 升级 ResetBaseline → WaitingReset |
| reset_at 出现且 ≤ now | 升级 ResetBaseline → PendingCheck（延迟检查） |
| reset 仍缺失，usage 清零/回满 | ActivatedInferred（更新 usage 基线，保持 UsageOnly，续设复查截止） |
| reset 仍缺失，usage 不变 | 按 next_recheck_at 继续复查 |

**主判定表（仅 ResetBaseline；按序首个命中）**：

| # | 条件 | 判定 |
|---|---|---|
| 1 | reset_at 存在且 < prev − skew_tol(120s) | Anomaly（倒退） |
| 2 | window_len 已知 且 reset_at − prev > 2 × window_len | Anomaly（超大跳跃） |
| 3 | window_len 未知 且 reset_at − prev > max_plausible_window(120 天) | Ambiguous → AnomalyHold |
| 4 | reset_at 存在且 > prev + skew_tol | ActivatedNew（未知长度时标记 length_unknown） |
| 5 | now < prev + refresh_after_reset_delay | NotDueYet |
| 6 | reset 缺失且 usage 清零/回满 | ActivatedInferred |
| 7 | reset 缺失且 usage 不变 | Ambiguous（按 StillLazy 处理一次；verify 后仍 Ambiguous → RetryWait） |
| 8 | reset ≈ prev（±tol）且已过 delay 且 usage 未清零 | StillLazy |
| 9 | 其余 | Ambiguous |

window_len 确立：记录相邻 reset 间隔，连续两次一致（±tol）后启用规则 2。

### 7.4 崩溃恢复与抑制

best-effort-once。WAL 有 `sending`（barrier 由随 WAL 落盘的 send_fence_seq 定义）或 `sent` → 启动先等 `verify_not_before` 再 verify，禁止立即重发。抑制窗口 `probe_resend_suppress = max(verify 宽限, 10m)` 自 `created_at` 起算；恢复路径与正常路径抑制规则一致：verify 仍 lazy/Ambiguous 也须等满剩余抑制期。

### 7.5 租约按 op 类型

纯读取超时→重签；token/凭据保存超时→重取凭据后重签；`probe_send` 超时→**不重签**，窗口入 SentUnknown、verify-first + 完整抑制（INV-36）。

---

## 8. 持久化规范

**写穿（成功后才继续/返回）**：Probe WAL（sending 含 send_fence_seq 先于 HTTP；sent 紧随成功）；fence `reserved_ceiling` 先于签发；凭据转换 planned 先于 SaveAuth、applied/aborted/outcome_unknown 紧随结果；AuthBlocked 置位/解除；各 Epoch/G 变化；roster 确认；Management 修改的注解与设置（先落盘后返回成功，INV-37）。
**合批**（≤1 次/5s，退出 flush）：额度快照、日志、统计。
**写入路径**：`schema_version` + 逐版本迁移 + 高版本只读告警；temp → fsync → rename → fsync 目录（Windows best-effort，S2 验证记录）；保留 `.bak`。
**损坏恢复**：主失败 → `.bak`；双失败 → 改名 `.corrupt` 取证 + 安全默认启动（窗口回 Idle/WaitingRoster、走 Bootstrap）+ 告警。状态文件从不存 token/cookie/header；指纹只存哈希（INV-30）。

---

## 9. 实施顺序与迁移

| 阶段 | 内容 | 门（全自动判定） |
|---|---|---|
| **S0** | Resource/Management 安全边界测试 | 套件 `suite_boundary` 绿（INV-01） |
| **S1** | 能力检测代码化（§1.1）；pick 线程模型压测基线；上游需求单提交（非阻塞） | `suite_capability` 绿 + 基线报告生成 |
| **S2** | Identity/Instance/Epoch/指纹四分量/转换链 WAL + 持久化框架（schema、原子写、迁移、损坏恢复、fence 块预留） | `suite_identity_persist` 绿（INV-28/30/38/39/40） |
| **S3** | 协调层一次接管完整刷新事务。**Legacy envelope = 整个 `refreshAuthVersioned`**：GetAuth → token refresh → SaveAuth → 额度读取 → reset-credits → Probe POST → Probe 后额度 → Probe 后 reset-credits（refresh.go:622/649/662/669）；envelope 内一次锁、已持锁直调、零嵌套意图。**Drain**：停发旧任务 → cancel context → join（上限 = 租约 2m）→ 全部确认结束则切换；**存在无法确认的旧执行**：含 Probe 发送阶段 → 对应窗口继承 `SentUnknown` + 完整抑制（INV-46）；其余 → 作废 ExecutionToken 由 fencing 丢弃 → 切换 | `suite_coordinator` 绿（INV-04/05/09/24/25/26/42/46） |
| **S4** | 普通刷新状态机（删除 stale 独立时间线） | `suite_refresh` 绿（INV-10/11/22/32） |
| **S5** | 调度三档 + trial + 快照发布 + candidates 交集 | `suite_scheduling` 绿（INV-12/13/29/41/43/44/45） |
| **S6** | Probe 重写替换 envelope（双窗口、判定表、WAL、SentUnknown、抑制、隔离）；移除过渡标签 | `suite_probe` 绿（INV-06/07/08/14～19/27/36） |
| **S7** | 同步控制器（TTL、Degraded/FailClosed）、Capability-A/B 接线、Management 收尾 | `suite_roster_mgmt` 绿（INV-02/03/20/21/23/31/33/34/35/37）+ traceability 100% |

---

## 10. 不变量总表（INV-01～46）

| 编号 | 不变量 |
|---|---|
| INV-01 | Resource 端点响应不出现任何账号 ID、别名、额度、优先级、熔断或日志业务数据 |
| INV-02 | 插件对 OpenAI 的请求只针对最后确认的最高层实例；Degraded 期间带 `degraded_roster` 且不超过 `roster_degraded_max`。**唯一例外**：`probe_on_provisional_roster` 显式开启时的 Provisional Probe 及其前置验证，须带 `provisional: true` |
| INV-03 | G 失效结果不写回、不产生误导性成功日志 |
| INV-04 | LoginEpoch/指纹分量校验失败的凭据写回被静默丢弃，不覆盖新凭据 |
| INV-05 | 同实例同 op-class in-flight 最多一个；等待方共享结果 |
| INV-06 | `probe_send` 永不合并 |
| INV-07 | 同一 instance lock 租约内，precheck→send→verify 不被同实例其他写操作插入（传播等待持锁不持 slot）；跨租约恢复不保证无插入，因果正确性由 send_fence_seq/G/IAE/ExecutionToken/attempt_id 保证，verify 不得把中间写入归因于 Probe |
| INV-08 | Probe 序列任一步失败后 instance lock 有界释放，普通刷新可继续 |
| INV-09 | 全局并发按单次 HTTP 计费；并发=1 时 Probe 等待期内其他实例仍可请求 |
| INV-10 | `stale_after` 不在任何时间线上独立触发请求 |
| INV-11 | Aging 不触发请求 |
| INV-12 | 全实例 Unknown/Stale 时首条真实请求能选出账号且只触发一轮刷新 |
| INV-13 | Exhausted 且 reset 已过按 Unknown 处理，可被乐观选取 |
| INV-14 | 熔断不阻断到期 Probe；Probe 成败不改熔断计数与状态 |
| INV-15 | 半开成功计数只被真实业务请求推进 |
| INV-16 | `usage_limit_reached` 不递增熔断失败计数 |
| INV-17 | 双窗口状态/退避/截止互不影响；一次结果可更新两窗口数据但不合并任务状态 |
| INV-18 | 时间跳跃后同一窗口至多一次 Probe 序列，无积压重放 |
| INV-19 | 持久化仅限已定义合法状态集（副作用 WAL 除外）；任意持久化组合启动可收敛 |
| INV-20 | roster 确认删除/降层后取消全部截止、零请求；例外仅限 Degraded 窗口内带标记 |
| INV-21 | 同步失败不清空集合、G 不变；超阈值后台停止而非继续 |
| INV-22 | Dormant 期间实例与缓存保留、卡片不消失、Management 可读 |
| INV-23 | AuthBlocked 后该实例全业务线自动请求停止；仅外部 LoginEpoch++ 恢复 |
| INV-24 | 每对外请求带且只带一个来源标签且与触发方一致（多条件时按 initial > stale_recovery > interval 唯一化；S3 允许 `legacy_refresh_txn`） |
| INV-25 | 租约回收后旧 ExecutionToken 的结果与成功日志不写回、不记录 |
| INV-26 | verify 只使用 `read_start_seq > send_fence_seq` 的读取 |
| INV-27 | send 与 verify 间崩溃 → 重启先 verify；抑制窗口内不重发 |
| INV-28 | 同 Identity 实例替换后旧实例 in-flight 写回不被新实例继承 |
| INV-29 | 每实例同时至多一个 trial（并发 pick 下由 CAS 保证）；trial 期间不再被乐观放行 |
| INV-30 | 状态文件截断/损坏/版本不符安全恢复；任何路径不泄露凭据 |
| INV-31 | 并发唤醒同一时刻只执行一次 roster sync |
| INV-32 | Dormant 期间普通刷新零 OpenAI 请求，到期 Probe 照常执行 |
| INV-33 | 自有轮换（含 refresh token 轮换）不递增 LoginEpoch/G、不解除 AuthBlocked；仅外部登录解除 |
| INV-34 | 低 CPA 层账号不入正常 Management payload、零 OpenAI 请求。例外同 INV-02（provisional 标记请求仅限已开启风险模式的前置验证与 Probe） |
| INV-35 | RosterFailClosed 下零后台请求；真实 pick 不受影响 |
| INV-36 | `probe_send` 租约超时仅入 SentUnknown 走 verify-first，不自动重签；抑制对恢复与正常路径一致 |
| INV-37 | Management 修改的注解与设置在持久化成功前不返回成功 |
| INV-38 | fence 序号跨崩溃/重启严格单调不回退；send_fence_seq 先于发送随 sending WAL 落盘 |
| INV-39 | 任何已签发序号 ≤ 已持久化并 fsync 的 reserved_ceiling |
| INV-40 | 凭据转换 WAL 四态完备：SaveAuth 与 applied 间崩溃、明确失败、结果未知均经对账收敛，不误判外部登录 |
| INV-41 | evidence pending 时 trial 不释放；TrialUnknown 递增退避；不存在周期性自动重新放行 |
| INV-42 | S3 期间旧路径全部对外请求（含 reset-credits、Probe POST、后置读取）在 legacy envelope 锁内；drain 完成后才启用新路径 |
| INV-43 | pick 同步热路径零网络、零磁盘、零后台等待；并发 pick 安全；副作用异步（trial CAS 除外） |
| INV-44 | candidates 仅限定单次选取授权域，不触发 roster 删除/重算/层级变更；pick 返回 ∈ candidates ∩ 活动层 |
| INV-45 | trial evidence 有墙钟与重试预算上限，超限强制 TrialUnknown |
| INV-46 | S3 drain 无法确认结束的旧 Probe 发送，对应窗口继承 SentUnknown 并执行完整抑制 |

---

## 11. 配置与内部策略默认值（决策冻结，自动化不得擅改）

**用户配置**：`quota_refresh_interval=30m`；`stale_after=5h`；`refresh_active_window=1h`；`max_refresh_concurrency=1`；`handle_enabled=true`；`enable_reset_probe`；`probe_on_provisional_roster=false`；`monthly_mode=expiry_order`；`refresh_on_startup=false`（仅普通刷新）。

**内部策略（冻结值）**：锁租约 2m；propagation_grace 3s；trial 超时 60s；trial_evidence_budget 5m / 重试 3 次；TrialUnknown 退避 1m→2m→5m cap 15m；roster_degraded_max 30m；probe_resend_suppress max(verify 宽限, 10m)；skew_tol 120s；max_plausible_window 120 天；provisional_probe_max_age 4h；unknown_reset_recheck 30m；fence 块 2^20；转换链 12 代 / 24h；CPA 同步 TTL 5m；refresh_after_reset_delay 1m；普通刷新退避 1m,5m,15m；熔断阈值 5 / 开启 30m / 半开成功 2。

---

## 12. 全量 Mock 测试设计

### 12.0 方法学基座（全部测试的公共设施）

- **虚拟时钟**：零真实 sleep；时间跳跃可注入。**确定性事件调度器**：单线程事件循环仿真 + 受控交错枚举（loom/DST 风格）。
- **Fake CPA Host**：可脚本化 `host.auth.list` 结果、Priority、凭据变化、失败注入、candidates 构造、pick 并发发起。**Fake OpenAI**：usage / reset-credits / probe 端点，可编程响应体、延迟、429/5xx/401、部分字段缺失。
- **崩溃注入点全枚举**（K 系列，机器可读清单随代码维护）：每个写穿落盘点前/后、每次 HTTP 发出前/后、rename 前/后——当前规格共 18 个 K 点起步，代码中以注入钩子标注，测试框架自动发现，遗漏标注 → CI 失败。
- **Oracle（可执行规格）**：测试侧**独立**实现 `selectAccount()`、`classifyWindow()`、`labelSource()`、`writebackGate()`、`trialTransition()`、`credentialClassify()`（禁止 import 生产实现），来源为本文件表格。所有枚举断言 = 生产实现 vs oracle 对拍。
- **Traceability 强制**：每用例带 `//inv:INV-xx[,INV-yy]` 标签，区分正例（不变量成立）与反例（构造违反前提，断言防护生效）。CI 工具生成矩阵：**任一 INV-01～46 缺正例或缺反例 → CI 失败**。
- **全量的定义**：连续量（时间、额度）按边界值等价类离散化；离散化后的模型空间**穷举**；离散化之外由 property-based 随机测试兜底（固定回归种子 + CI 每日新种子，失败自动缩小并固化为回归用例）。

### 12.A 组：崩溃与迁移高风险（10 场景，优先执行）

1. Probe 已发送、`sent` WAL 前崩溃 → 恢复 verify-first。
2. `sending` WAL 后、HTTP 前崩溃 → SentUnknown 语义。
3. fence 块预留各 K 点（ceiling 落盘前/后、签发后）。
4. 连续 SaveAuth F0→F1→F2 与同步跳读/乱序观察。
5. 相同实例删除重加：{有 revision 预留通道, 无 revision} × {同指纹, 变指纹} × {观察到中间态, 未观察到}（"未观察 + 同指纹"断言为无事件并对照 §2.6 声明）。
6. Degraded 满 30m 后台 fail-closed，真实 pick 不受影响。
7. legacy envelope 与 manual/quota 意图并发。
8. Baseline 五种输入 P1–P5 + UsageOnly 专用表全行。
9. 五小时 / 周 / 未知长度窗口 × reset {倒退, 超大跳跃, 超 plausible, 缺失} 组合。
10. 高频并发 pick 且 evidence 超 60s，trial 不重复放行；evidence 预算耗尽入 TrialUnknown。

### 12.B 组：调度全量矩阵（覆盖"调度"）

**单实例分类穷举**：状态向量 = cache{4} × exhausted{无, 5h 未到, 长周期未到, 双耗尽, 已过 reset}{5} × auth{2} × circuit{3} × temp_unavailable{2} × trial{无, active, TrialUnknown}{3} = **720 组合 × 优先级{2} = 1440，全枚举**，断言三档归类与 oracle 一致；不可达组合由 oracle 标注并断言生产实现同样拒绝进入。
**多实例选取**：档位×优先级共 6 类代表元，N∈{1,2,3} 实例 → 6^N（≤216）**全枚举**；类内具体向量用 3-wise 覆盖阵抽代表；再叉乘 candidates 关系 {⊇活动层, ⊂活动层, 与活动层交集空}{3}。断言：选取结果、fallback 触发、trial 开启、意图来源标签（含唯一优先级规则）全部对拍。
**并发 pick**：2～4 路并发对同一快照，断言 INV-29/43。
**Property-based 兜底**：随机状态向量 + 随机 candidates，oracle 对拍，每日种子。

### 12.C 组：Probe 全量（覆盖"Probe"）

**状态机全转移**：10 持久化状态 × 全部合法事件 → 全转移路径覆盖；每状态 × 全部非法事件 → 断言零副作用。
**classify 全输入网格（golden 表）**：baseline{Reset, UsageOnly, 无}{3} × reset 偏移等价类{缺失, 倒退>tol, prev−tol, prev, prev+tol, ∈(tol,2W], >2W(已知), >120d(未知)}{8} × window_len{5h, 7d, 未知}{3} × usage{清零, 回满, 不变, 减少}{4} × now 相对 delay{未到, 边界, 已过}{3} ≈ **864 行全笛卡尔**，golden 文件脚本再生成，diff 即评审。
**双窗口隔离**：两窗口状态对 10×10 全枚举 × 单响应同时喂两窗 → INV-17。
**崩溃恢复**：每 K 点 × 到达该点的每条状态机路径 → 重启收敛断言（INV-18/19/27/36/38/39/40）。
**时间跳跃**：每状态 × 跳跃{< 截止, 跨一个截止, 跨多个截止}。
**抑制窗口**：正常路径 / 恢复路径 / SentUnknown 三入口 × verify 结果{Activated, StillLazy, Ambiguous} 全组合。

### 12.D 组：身份与安全边界

转换链：链长 1–4 × 观察序列（前缀/跳代/乱序/交错外部变化的受限全排列）× SaveAuth 结果{成功, 失败, 未知} × K 点；CredentialAmbiguous 自动解除与三个 Management 出口各自的 Epoch 影响断言。Resource 端点响应对敏感字段词典（账号 ID、alias、priority、quota、reset、token、circuit）零命中扫描（INV-01）。身份不可解析四分支行为。

### 12.E 组：并发、熔断与失败恢复（覆盖"刷新"及交叉）

**意图交错**：事件集 {pick, initial/interval/stale 刷新, precheck, send, verify, manual, token_refresh, 租约回收, roster 变更, Degraded 转换} 在同实例上任取 2–3 事件的**全部交错序**（受限深度系统化探索），断言去重、barrier、fencing、锁顺序（INV-05/07/25/26）。
**刷新时间线**：§6.1 时间线逐点 + 窗口边界三点（到期前/时/后）×（有/无新 pick）；来源唯一优先级三条件 2^3 组合全枚举。
**熔断**：真实失败序列 × Probe 交错 × 半开推进来源 → INV-14/15/16。
**Degraded**：失败起点 × 30m 边界三点 × 恢复来源{成功同步, 无}。
**租约**：慢 worker 返回{回收前, 后} × op 三类 → §7.5 全行。
**S3 迁移**：drain 中旧 goroutine {正常结束, 超时含 Probe 发送, 超时不含} × 新路径首请求时机 → INV-42/46。

### 12.F 预算与产出

全套虚拟时钟目标 CI < 5 分钟（golden 与覆盖阵预生成）。产出物：traceability 矩阵（自动生成）、K 点清单、golden 表、覆盖阵定义、每日种子失败自动缩小回归库。

---

## 13. 自动化执行协议（无人工介入的机制保障）

1. **规格冻结与裁决优先级**：实现期发现矛盾时按 `不变量表(§10) > 状态机与判定表 > 正文散文 > 示例` 现场裁决；无法裁决时套用**保守默认**：不发请求 > 发请求；丢弃写回 > 写回；进入等待/退避 > 立即执行。裁决写入 `docs/deviations.md`（编号、条款、处置、对应测试），**不回改本文件、不设人工审批门**。
2. **阶段门全部机器判定**：§9 每阶段 = 命名测试套件绿 + traceability 覆盖该阶段 INV + 静态检查（pick 路径 IO 扫描、状态文件敏感词扫描、K 点标注完整性）。人工代码评审为可选，不阻塞流水线。
3. **外部依赖非阻塞化**：上游契约确认结果只更新文档与最低版本标注（Capability-A/B 双分支已完全规格化）；CredentialAmbiguous 有自动保守默认（§2.5），Management 出口为可选增强。
4. **数值冻结**：§11 全部默认值视为规格常量，agent 不得擅改；调整只能经 deviations 记录并附测试证据。
5. **执行序**：S0 → S1 →…→ S7 串行推进，每阶段一个 PR，PR 模板含该阶段 INV 勾选清单与 deviations 增量；S6 完成后运行 §12 全量套件作为终验。
6. **完成定义**：S7 门通过 + §12 A–E 全绿 + traceability 100% + deviations 全部闭环 = 重构完成，无需任何批准动作。

---

## 附：核心原则

> 账号加载不等于额度刷新；普通刷新不等于 Probe；业务熔断不等于后台任务熔断；candidates 不等于 roster；插件维护的实例身份是 best-effort fencing 而非强身份；不同业务线共享安全的执行设施，但绝不共享混乱的触发状态。
