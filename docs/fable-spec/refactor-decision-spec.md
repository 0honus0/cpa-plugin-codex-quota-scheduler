# Codex Quota Scheduler 重构决策规格（Decision Spec v4）

> **文档地位**：本文件是本次重构的**唯一生效规格**（single source of truth），自包含全部规则，实施者与审核者无需回看任何历史版本。与《目标设计文档》并行有效，冲突时以本文件为准。
>
> **v3 → v4 变更记录**（历史版本仅用于解释变更，不构成规格依赖）：
> 展开全部"同 v2/沿用"引用为正文（阻断 1）；重写 IAE 残余风险声明（阻断 2）；补全 fence_seq 块预留协议（阻断 3）；自有轮换登记改为有序转换链 + WAL + CredentialAmbiguous；legacy 事务范围扩至完整 `refreshAuthVersioned`（含 reset-credits 与 Probe 后置请求）并定义 drain 规则；Probe Baseline 规则拆分为五种输入；未知 window_len 禁用倍数异常判定；Trial 释放规则重设计（evidence pending 不释放、TrialUnknown 递增退避）；新增 INV-39～42；附 Mock 优先场景排序。

---

## 0. 系统概览与安全边界

系统由三个控制器与一个共享协调层组成：**CPA 账号同步控制器**（决定当前应管理哪些账号）、**普通额度刷新控制器**（为真实 Codex 调度维护足够新的额度）、**Probe 控制器**（主动激活 lazy 额度窗口）、**统一请求协调层**（接收请求意图，负责去重、并发、认证与安全写回）。控制器决定"为什么、什么时候"；协调层决定"怎样安全执行"。

**安全边界**：Resource 端点（`/v0/resource/plugins/codex-quota-scheduler/status`）只提供 HTML/CSS/JS/图标等通用静态资源，不含账号 ID、别名、优先级、额度、重置时间、熔断状态、调度记录或任何缓存业务数据。全部业务数据与状态变更操作经 Management 端点（`/v0/management/plugins/codex-quota-scheduler/...`），需 CPA 管理密钥。管理密钥只存于浏览器页面会话，不写入插件状态、导出、日志或浏览器存储。

**全系统额度刷新触发来源只有五类**（除此之外任何机制不得触发对外额度请求）：

1. `scheduler_initial`：账号从未成功取得额度，真实请求触发首次刷新。
2. `scheduler_interval`：活跃窗口内达到 `quota_refresh_interval`。
3. `scheduler_stale_recovery`：真实请求到来且该账号缓存已 Stale。
4. `probe_*`：Probe 业务线（`probe_startup` / `probe_precheck` / `probe_activation` / `probe_verify`）。
5. `manual_refresh`：Management 手动刷新。

每个对外请求必须携带上述来源标签之一（S3 迁移期允许过渡标签 `legacy_refresh_txn`，S6 完成后移除）。

---

## 1. 身份、实例与版本模型

### 1.1 三个"是谁"

| 概念 | 定义 | 用途 |
|---|---|---|
| **AccountIdentity** | ChatGPT 账号 ID（凭据 claims），次键 email（大小写不敏感比较） | 仅用于继承插件内部配置：别名、插件优先级、分组、标签、备注、Probe 历史统计 |
| **AuthInstanceID** | CPA 宿主标识的具体认证实例（宿主提供的 auth 标识） | 操作单位：admission 集合、锁、去重、请求、写回、日志全部以它为键 |
| Identity↔Instance 绑定 | 由 CPA 同步维护；一个 Identity 可对应 0/1/多个 Instance | 多实例独立参与调度，共享同一份 Identity 配置 |

### 1.2 版本号族

| 版本号 | 作用域 | 递增时机 | 用途 |
|---|---|---|---|
| **InstanceAdmissionEpoch（IAE）** | 每 Instance | 优先直接采用宿主 instance revision / CAS token（S1 P0 调查项）；宿主不提供时由插件维护，在以下任一事件递增：观察到该 InstanceID 删除后重加；绑定指纹变化；LoginEpoch++ | 进入 G；写回门控 |
| **TierGeneration（G）** | 全局 | 最高层的 **(AuthInstanceID, IAE) 多重集**发生任何变化 | 层级写回防护 |
| **AuthBindingEpoch** | 每 Identity | Identity↔Instance 绑定关系变化（换实例、增删实例） | 配置继承迁移一致性检查 |
| **LoginEpoch** | 每 Instance | 被 §1.4 分类算法判定为**外部重新登录** | 唯一解除 AuthBlocked 的信号 |
| **TokenEpoch** | 每 Instance | 被分类为自有轮换的凭据更新 | 仅日志调试；不门控、不解除 AuthBlocked |
| **ExecutionToken** | 每次授权的**异步执行** | 协调层签发 | worker 结果写回 fencing（§5.4）；Management 纯本地修改走普通状态事务，不依赖它 |

### 1.3 绑定指纹

`fingerprint = hash(subject, hash(refresh_token), 宿主实例元数据)`。状态文件只存哈希，从不存原文。协调层对凭据保存类写回，除 (G, IAE, ExecutionToken) 外额外比对最新同步到的绑定指纹。

### 1.4 凭据变化分类：自有轮换转换链

**目的**：区分插件自身 token 刷新（含 refresh token 轮换）与外部真实重新登录，消除 LoginEpoch 误报。

协调层为每个实例持久化一条**有序转换链**（不是单个预期指纹，也不是无序历史集合）：

```text
F0 → F1（save_seq=10）
F1 → F2（save_seq=11）
```

**转换链 WAL**：每次凭据保存前先持久化 planned transition（`F_prev → F_next, save_seq`），SaveAuth 成功后标记 applied。SaveAuth 成功但 applied 标记落盘前崩溃时，重启后链上仍有该 planned 记录，同步观察到 F_next 不会误判为外部登录（INV-40）。

**同步观察到实例指纹 F 时的判定**：

```text
F == 链游标当前指纹        → 无事件
F == 链上游标之后的某一代    → 自有轮换确认（允许跨代确认），游标推进到该代 → TokenEpoch++
F 不在当前可达链中：
   subject 变化，或 refresh_token 与链上任何可达代都不匹配
                            → LoginEpoch++（外部重新登录）→ IAE++ → G++
   仅元数据/字段顺序差异     → 忽略
链容量（8～16 代，内部策略）或时间上限（内部策略，默认 24h）耗尽
                            → CredentialAmbiguous：暂停自动解除 AuthBlocked，
                              不得直接判为外部登录；Management 告警，等待人工确认或
                              下一次可明确分类的观察
```

推论：插件自身的 token 刷新**永远不会**导致 LoginEpoch++、G++ 或解除 AuthBlocked（INV-33）。

### 1.5 残余风险声明（必读，措辞为规范性内容）

无宿主 revision/tombstone 时：

- CPA 同步 TTL（活跃期约 5 分钟）只约束**两次正常观察之间的间隔**；Degraded 状态（§7.3）会把观察间隔扩大到最多 `roster_degraded_max`（默认 30 分钟）。
- **完全相同的删除重加**（同 InstanceID、同 refresh token、同 subject）若完整发生在任一观察间隙内并恢复为相同最终状态，插件**之后可能永久无法识别该事件曾经发生**——这不是"最多 N 分钟的风险窗口"，观察间隔只是事件发生而未被观察到的机会窗口，一旦漏检，事实可能永久不可恢复。
- Fail-closed 只能停止新的后台请求，**不能恢复实例连续性的事实**。
- 因此插件维护的 IAE 是 **best-effort fencing，不是强实例身份**。强实例身份只能来自宿主 revision/tombstone/CAS——S1 向 CPA 上游提交该能力需求（与 priority roster API 同一需求单）。

### 1.6 身份不可解析实例

- 参与宿主提供的真实 `scheduler.pick`（避免 CPA 与插件行为分裂）。
- 仅以 AuthInstanceID 临时管理，不继承任何旧配置；重新登录后**不得**按文件名继承。
- 缺少身份 claims 但仍能取得 quota/Probe 所需的 ChatGPT account ID：后台业务线正常运行。
- 连请求所需 account ID 都不可得：不执行 quota/Probe，仅按 optimistic trial 规则参与真实请求。

---

## 2. Roster 来源与 Bootstrap

### 2.1 三个合法来源（按权威度）

1. **宿主 roster API**（若存在）：带优先级的受保护账号清单。S1 P0 调查项（连同 instance revision 能力一并确认；缺失则提上游需求）。
2. **`scheduler.pick` candidates**：宿主逐次提供，是该时刻的权威 roster + 优先级信息。**每次 pick 均视为一次 roster 确认**，喂给 CPA 同步控制器（更新 last-confirmed、必要时重算 G）。真实请求路径因此永不依赖同步缓存。
3. **持久化 last-confirmed roster**：仅作启动时 Provisional。

### 2.2 Bootstrap 状态机

```text
启动
→ 来源 1 可用：Confirmed，正常运行
→ 来源 1 不可用：
    加载 last-confirmed 为 Provisional（沿用保存的 G），Management 标记 provisional
    普通刷新：Dormant（本就等真实请求，无影响）
    Probe：默认 → 全部窗口 WaitingRoster，等待首次 pick 或 API 确认
→ 首次 pick candidates 到达 → 确认/替换 roster（可能 G++），WaitingRoster 解除
```

**Provisional Probe 为显式风险模式**（`probe_on_provisional_roster`，默认 `false`）。开启后受三重约束：

1. roster 确认时间距今 < `provisional_probe_max_age`（内部策略，默认 4 小时）；
2. 每次 Probe 前经 `ListAuths/GetAuth` 验证实例仍存在且绑定指纹未变，失败则该实例回 WaitingRoster；
3. Management 显著警告，配置说明明确写出：无权威 priority API 时，实例存在**不能证明**其仍属最高 CPA 层。

**迁移说明（必须写入 README 与升级公告）**：无 roster API 且重启后没有真实 Codex 请求时，默认配置下 Probe 将**无限期停留在 WaitingRoster**。无人值守部署（如 NAS 保活场景）需要主动开启上述风险选项，或等待 CPA 提供权威 priority roster API。

---

## 3. 统一请求协调层

### 3.1 两级资源与固定获取顺序

```text
资源一：per-AuthInstance mutex（同实例所有修改凭据或状态的操作串行）
资源二：global semaphore，容量 = max_refresh_concurrency
        计费单位 = 单次对外 HTTP 请求（不是账号，不是序列）
```

固定顺序（防死锁唯一合法路径）：`acquire(instance_lock) → 每次 HTTP 前 acquire(slot) → HTTP → 返回后立即 release(slot) →（序列内重复）→ release(instance_lock)`。

禁止：持 instance lock 等待另一 instance lock；持 slot 等待任何 lock；持 slot 期间做任何非 HTTP 的等待（含 Probe 激活传播等待）。

### 3.2 fence_seq：单调序号与块预留协议

协调层维护全局单调序号 `fence_seq`（uint64），用于因果 barrier。**禁止使用墙钟时间戳做 barrier 比较**（时间回拨、精度不足、休眠唤醒、NTP 调整均不可靠）。

**块预留协议**（`reserved_ceiling` 表示**已预留上界**，不是最后签发值）：

```text
内存序号块耗尽
→ 计算新的 reserved_ceiling（当前 ceiling + 块大小，内部策略如 2^20）
→ 原子持久化并 fsync reserved_ceiling
→ 落盘成功后，才允许签发该块中的序号
→（Probe 路径）分配 send_fence_seq
→ 写入并 fsync 该 attempt 的 sending WAL（含 send_fence_seq）
→ WAL 成功后，才允许发送 Probe HTTP
```

重启后从 `persisted_reserved_ceiling + 1` 开始预留新块。未使用序号丢失无影响：正确性只要求**严格不回退**，不要求连续（INV-38、INV-39）。

### 3.3 去重与合并

去重键：`(AuthInstanceID, op-class, G)`。

`quota_read` 意图携带可选 `started_after`（fence 序号）：只允许 join 满足 `read_start_seq > started_after` 的 in-flight 读取；无满足者则新起读取。每次对外读取启动时分配 `read_start_seq`。

| op-class | 合并规则 |
|---|---|
| `quota_read` | 五类来源之间可合并（受各自 `started_after` 约束），等待方共享同一结果、各自回到自己的状态机 |
| `probe_precheck` | `started_after` 为空，可与任何 in-flight `quota_read` 合并 |
| `probe_verify` | `started_after = 对应 attempt 的 send_fence_seq`；只使用 send 成功之后启动的读取，不跨 attempt 合并 |
| `probe_send` | 不与任何请求合并 |
| `token_refresh` | 同实例 in-flight 可合并；保存前校验 LoginEpoch 与绑定指纹 |
| `manual_refresh` | bypass 缓存新鲜度（必产生一次读取意图），但可 join 同实例 in-flight `quota_read` |

### 3.4 认证管理

协调层统一处理：获取 CPA 认证、解析凭据、判断 access token 过期、必要时刷新、按 §1.4 转换链 WAL 保存新凭据、日志对 token 与敏感字段脱敏。普通刷新与 Probe 不得各自实现 token 刷新逻辑。

### 3.5 推荐实现模型

意图队列 + 单一协调 goroutine 持有全部可变状态 + worker goroutine 只做 HTTP，结果作为事件投回队列；lock/slot 退化为协调 goroutine 内簿记。S1 需确认 `scheduler.pick` 在 c-shared/CGO 下的调用线程模型与延迟预算后定稿。

---

## 4. 缓存状态机与调度

### 4.1 缓存年龄四态

```text
age = now − last_success_fetch_time
Unknown : 从未成功获取
Fresh   : age ≤ quota_refresh_interval
Aging   : quota_refresh_interval < age ≤ stale_after
Stale   : age > stale_after
```

**Aging 是纯标签**：只影响 UI 与排序内轻微降权，不触发任何刷新。`stale_after` 是缓存可信度安全阈值，**在任何时间线上不独立触发请求**——真实请求遇 Stale 才触发一次 `scheduler_stale_recovery`。

### 4.2 Exhausted 时效

Exhausted 标记必须携带来源 reset 时间（无 reset 时按 2 分钟）。**reset 已过的 Exhausted 视同 Unknown**——调度器读取时现场判断，不依赖后台清除任务。

### 4.3 插件优先级正式定义与三档选取

> **插件优先级：在相同可用性可信等级内，优先级越高越先使用。**

```text
Preferred     : Fresh/Aging 且未耗尽、无熔断、无 AuthBlocked
Opportunistic : Unknown；Stale 且上次已知可用；Exhausted 但 reset 已过
Excluded      : 确认耗尽未到 reset；AuthBlocked；熔断开；临时不可用；trial 进行中（含 TrialUnknown 退避期）

选取：全部 Preferred 按插件优先级（同级内按既有业务策略：monthly_mode、到期时间、剩余额度、稳定 ID 决胜）排序取首
   → Preferred 全空：全部 Opportunistic 按同规则排序取首，开启 optimistic trial
   → 全部 Excluded：返回无法选取，进入 CPA fallback
```

即已知可用的低插件优先级账号优先于状态未知的高插件优先级账号；冷启动全 Unknown 时自然回落纯优先级顺序。首版**不提供** strict 优先级开关；未来如有需求作为显式风险模式另行评估。

绝不因高优先级账号耗尽而直接触发 CPA 内置 fallback；只有当前最高层全部 Excluded 才 fallback。

### 4.4 Optimistic Trial（v4 重设计）

- 每 AuthInstance 同时至多一个 trial。选中 Opportunistic 账号即开启 trial 并**同时提交对应刷新意图**（evidence refresh）；trial 期间该实例归入 Excluded。
- **释放条件（仅限以下三类真实证据）**：真实请求 usage feedback 到达（成功或 `usage_limit_reached`）；可靠额度结果写回；真实请求成功完成。
- **超时不自动放行**：达到 `optimistic_trial_timeout`（内部策略 60s）时——
  - 若 evidence refresh 仍在排队/执行/退避中 → **不释放 trial**，继续 Excluded。
  - 若无任何 pending evidence（异常情形）→ 进入 **TrialUnknown**：按递增退避（内部策略 1m → 2m → 5m，上限 15m）后才允许下一次乐观试用；期间保持 Excluded。
- 真实证据到达即清退避、恢复正常状态归档。周期性 60 秒重新放行造成的真实请求打击被此规则排除（INV-41）。

---

## 5. 时间、租约与 Fencing

1. **全部绝对时间**：待执行任务持久化"截止时刻"，tick 只是检查器；禁止依赖 tick 计数或相对累计。
2. **启动/唤醒 = 重算不重放**：启动或检测到时间跳跃（`now − last_tick > 2 × tick 间隔`）后，对每实例每窗口用 `(持久化 reset 时间, 最近快照, now)` 推导当前应处状态，丢弃积压到期事件；同一窗口至多产生一次序列。
3. **持久化原则**：执行中状态不持久化；**外部副作用的 WAL 记录必须先于副作用落盘**（§3.2、§8.1）。启动时无 WAL 记录的中间态一律回退 pending；有 WAL 记录的按 §6.4 恢复。
4. **租约与 Fencing**：任务执行持有 ExecutionToken，记录 `startedAt`；超过租约（内部策略 2 分钟）由协调层回收——作废该 token、释放锁。旧 worker 稍后返回时因 token 失效，**结果与成功日志全部丢弃**。回收后的重签**按 op 类型区分**：

| op 类型 | 租约超时处置 |
|---|---|
| 纯读取（quota_read 各来源） | 重签执行 |
| token_refresh / 凭据保存 | 重新获取凭据后重签 |
| **probe_send** | **不重签**。对应窗口进入 `SentUnknown`，只能 verify-first + 完整抑制窗口（INV-36） |

---

## 6. Probe 控制器

### 6.1 与熔断的双向隔离（四条硬规则）

1. **熔断不阻断 Probe**：熔断开启、半开、额度耗尽、临时不可用，均不阻止已到期的 Probe 序列。
2. **Probe 不修改熔断**：Probe 成功不推进半开计数、不关闭熔断、不清零失败计数。
3. **Probe 失败不计熔断**：含 429、5xx、超时，只入 Probe 自己的失败计数与退避。
4. **Probe 仅做事实纠正**：允许写回窗口额度数据、重置时间；precheck/verify 读到窗口已激活时清除**对应窗口**的 Exhausted 标记。除此之外不触碰任何业务可用性状态。

熔断半开成功计数**只**由真实 `scheduler.pick` 选中后的业务请求成功推进。AuthBlocked 是唯一跨业务线状态：凭据确认不可恢复后，普通刷新、Probe、半开验证全部停止对该实例的自动请求，实例入 Excluded，UI 提示重新登录；**仅外部 LoginEpoch++ 解除**（自有轮换、TokenEpoch 变化不解除）。

### 6.2 双窗口状态机

五小时窗口与长周期窗口各持有独立的一份状态（互不共享任何字段）：

```text
Idle → WaitingReset → PendingCheck
PendingCheck --协调层授权--> [precheck 读取] --classify-->
   ActivatedNew / ActivatedInferred → Confirmed
   NotDueYet → WaitingReset（重算截止）
   Anomaly → AnomalyHold（记录，下周期重评，不发 Probe）
   StillLazy → 分配 send_fence_seq → [WAL: sending] → [send]
             → [WAL: sent] → SentAwaitingVerify
             → [verify 读取, barrier=send_fence_seq] --classify-->
                 Activated* → Confirmed
                 StillLazy / Ambiguous → RetryWait
send 租约超时 → SentUnknown → verify-first（§5.4）
任一步失败 → RetryWait（Probe 独立退避）；401 → AuthBlocked
AuthBlocked --外部 LoginEpoch++--> PendingCheck
WaitingRoster --roster 确认--> 按 §6.3 P3 恢复
任意状态 --实例退出最高层 / G 失效--> Idle（取消截止，作废未完成 attempt）
Confirmed --可推导下一 reset--> WaitingReset
```

持久化状态集：`Idle / WaitingReset / PendingCheck / SentAwaitingVerify / SentUnknown / RetryWait / Confirmed / AuthBlocked / AnomalyHold / WaitingRoster`。每窗口另持久化：上次 reset 时间、失败计数、下次截止、最近激活方式。一次 send 的 attempt 记录其意图激活的窗口集合，verify 按窗口分别判定；一次额度响应同时喂给两窗口，各自独立 classify、独立推进。

### 6.3 窗口判定纯函数

`classifyWindow(window_type, baseline, snap, now, cfg)`，cfg 含 `skew_tol`（默认 120s）、`window_len`（可能未知）、`refresh_after_reset_delay`。

**预处理（Baseline 五种输入，按序）**：

| # | 条件 | 处置 |
|---|---|---|
| P1 | snap 无效 / 关键字段类型错误 | **InvalidSnapshot** → RetryWait |
| P2 | 无持久化 baseline，snap.reset_at 存在且 > now | 建立 baseline → **WaitingReset** |
| P3 | 无持久化 baseline，snap.reset_at 存在且 ≤ now | 建立 baseline → **PendingCheck**（经 `refresh_after_reset_delay` 延迟检查）——首次安装即已 lazy 的窗口不被推迟一个完整周期 |
| P4 | 无持久化 baseline，snap.reset_at 缺失 | 保存 usage baseline，设独立复查截止（`unknown_reset_recheck`，内部策略 30 分钟）→ WaitingReset |
| P5 | 有持久化 baseline（含从 WaitingRoster 恢复） | 恢复既有 baseline，**不作首次处理**，进入主判定表 |

**主判定表（按序首个命中）**：

| # | 条件 | 判定 |
|---|---|---|
| 1 | snap.reset_at 存在 且 < prev_reset_at − skew_tol | **Anomaly**（向后倒退） |
| 2 | window_len **已知** 且 snap.reset_at − prev_reset_at > 2 × window_len | **Anomaly**（向前超大跳跃） |
| 3 | snap.reset_at 存在 且 > prev_reset_at + skew_tol | **ActivatedNew**（窗口翻转；window_len 未知时命中此规则并标记 `length_unknown`） |
| 4 | now < prev_reset_at + refresh_after_reset_delay | **NotDueYet** |
| 5 | snap.reset_at 缺失 且 用量相对 baseline 已清零/回满 | **ActivatedInferred** |
| 6 | snap.reset_at 缺失 且 用量与 baseline 相同 | **Ambiguous**（按 StillLazy 处理一次；verify 后仍 Ambiguous → RetryWait，不连发） |
| 7 | snap.reset_at ≈ prev（±tol）且 已过 delay 且 用量未清零 | **StillLazy** |
| 8 | 其余 | **Ambiguous** |

**未知 window_len 的处理**：规则 2 不适用（禁用倍数异常判定，不得用固定天数替代——固定值会同时误判短周期与合法 60/90 天周期）；控制器记录相邻 reset 间隔，连续两次一致（±tol）后确立 window_len 并启用规则 2。

### 6.4 崩溃恢复与重复抑制

目标为 **best-effort-once**（严格 exactly-once 不可达）。

- WAL 存在 `phase=sending`（发送结果未知，barrier 由随 WAL 落盘的 send_fence_seq 定义）或 `phase=sent` → 启动后先等 `verify_not_before` 再 verify，禁止立即重发。
- 重复抑制窗口：自 attempt `created_at` 起 `probe_resend_suppress`（内部策略，默认 max(verify 宽限, 10 分钟)）内不允许对同窗口的新 send。**恢复路径与正常路径的抑制规则完全一致**：恢复后 verify 结果仍为 StillLazy/Ambiguous 时，同样必须等满剩余抑制窗口才允许新 attempt。

### 6.5 Probe 失败

Probe 拥有独立的失败计数、下次重试时间、最后失败阶段、对应窗口类型。普通刷新成功不清除 Probe 重试；Probe 失败不污染其他实例或另一窗口；不进入秒级忙循环。

---

## 7. 三个控制器

### 7.1 CPA 账号同步控制器

只访问 CPA，不访问 OpenAI。启动同步一次（或走 §2.2 Bootstrap）；完全空闲（无真实请求、无将执行的 Probe、无 Management 操作）时休眠、不固定轮询；真实请求活跃期按 TTL 同步（约 5 分钟，几十次 `scheduler.pick` 通常只一次 CPA 读取）；Probe 唤醒前确保清单在 TTL 内；Management 加载时按需检查（只读静态信息，不查额度）。**每次 `scheduler.pick` 的 candidates 均作为一次 roster 确认喂入**。

账号变化处置：新实例进最高层 → 加入活动集合；实例删除 → 移除并取消待执行任务；降层 → 立即退出；新的更高层出现 → 原子替换整层（G++）；凭据变化 → §1.4 分类；同步失败 → §7.3。多个唤醒来源并发时同一时刻只执行一次 roster sync（INV-31）。

### 7.2 普通额度刷新控制器

```text
Dormant --真实 scheduler.pick--> Active（记 last_activity；窗口截止 = last_activity + refresh_active_window）
Active 内每次 pick：延长窗口；账号 Unknown → scheduler_initial；Stale → scheduler_stale_recovery；
                   距上次刷新 ≥ quota_refresh_interval → scheduler_interval
Active --窗口截止且无新 pick--> Dormant
```

Dormant 时：普通后台刷新停止，账号不卸载、卡片不消失、缓存保留、Management 可读，页面标记"普通后台刷新已休眠"；已启用的 Probe 照常到期执行。参考时间线（interval=30m、window=1h）：`10:00 请求→刷新；10:20 请求→只延长窗口；10:30 到间隔→刷新；11:00 刷新；11:20 窗口结束→休眠`。

`refresh_on_startup` 只约束本控制器；Probe 启动读取不受其控制（文档与 UI 显式说明）。

### 7.3 同步失败：有限期 fail-open → fail-closed

**关键事实**：真实 pick 的 candidates 由宿主逐次提供，真实请求路径不依赖同步缓存——降级策略只覆盖后台业务线。

```text
同步失败 → Degraded（记 degraded_since；活动集合不清空、G 不变）
Degraded ≤ roster_degraded_max（内部策略 30 分钟）：
   后台业务线继续使用 last-confirmed roster，所有请求标记 degraded_roster: true
超过阈值 → RosterFailClosed：
   后台 OpenAI 请求全停（普通刷新意图丢弃、Probe 窗口入 RosterHold）
   真实 pick 不受影响；Management 显著告警
恢复：任一成功同步或任一次 pick candidates → 确认 roster、解除、RosterHold 窗口按重算恢复
```

### 7.4 失败与熔断职责隔离

普通刷新失败只影响普通刷新（网络/429/5xx 退避不忙循环；401 尝试一次合法 token 恢复；确认不可恢复 → AuthBlocked；本地认证损坏不重复外发）。普通刷新与 Probe **不共用**任何 NextRetryAt。业务熔断只保护真实 Codex 调度，不承担普通刷新定时、Probe 定时、认证重试或 CPA 同步重试。`usage_limit_reached` 立即标记临时耗尽（至 reset 或 2 分钟），不计熔断。

---

## 8. 持久化规范

### 8.1 落盘时机

**必须立即落盘（写穿，成功后才继续/返回）**：
Probe attempt WAL（`sending` 含 send_fence_seq，先于 HTTP；`sent` 紧随发送成功）；fence 块 `reserved_ceiling`（先于签发）；凭据转换链 planned/applied 记录（planned 先于 SaveAuth）；AuthBlocked 置位/解除；各 Epoch 与 G 变化；roster 确认；**用户经 Management 修改的别名、插件优先级、标签、备注、设置——持久化成功后才返回保存成功**。

**可合批**（内部策略 ≤1 次/5s，进程退出 flush）：额度快照、日志、统计。

### 8.2 写入路径

文件头含 `schema_version`，逐版本迁移，识别更高版本 → 只读加载不写回并告警。原子写：序列化 → 同目录临时文件 → fsync → rename 覆盖 → fsync 目录（Windows rename 语义为 best-effort，S2 验证并记录平台差异）。成功后保留上一份 `.bak`。

### 8.3 损坏恢复与敏感数据

主文件解析失败 → 加载 `.bak`；双失败 → 损坏文件改名 `.corrupt` 保留取证，以安全默认启动（Probe 窗口回 Idle/WaitingRoster、无 roster → Bootstrap）并告警。**状态文件从不存储** access/refresh token、cookie、authorization header；指纹只存哈希。任何恢复路径不得泄露凭据。

---

## 9. 实施顺序与迁移

| 阶段 | 状态 | 内容 | 验收 |
|---|---|---|---|
| **S0** | **已批准可开始** | 锁死 Resource/Management 安全边界（补测试） | INV-01 |
| **S1** | **已批准可开始（只读调查）** | 宿主能力验证三项：priority roster API；instance revision/tombstone；`scheduler.pick` 线程模型与延迟预算。产出 Bootstrap 与 IAE 来源定稿；提交上游能力需求单 | 书面结论 |
| **S2** | 待批准 | Identity/Instance/Epoch 族 + 转换链 + 指纹 + 持久化框架（schema、原子写、迁移、损坏恢复、fence 块预留） | INV-28、30、38、39、40；迁移测试 |
| **S3** | 待批准 | 协调层一次性接管**完整单账号刷新事务**。**Legacy transaction envelope = 整个 `refreshAuthVersioned`**，包住其全部对外调用：GetAuth → token refresh → SaveAuth → 额度读取 → **reset-credits 请求 → Probe POST → Probe 后额度读取 → Probe 后 reset-credits**（对应 v0.1.6 refresh.go:622/649/662/669）。envelope 内部一次锁获取、全部经"已持锁直调"接口，不产生嵌套协调意图。**Drain 规则**：切换时刻先停止签发旧路径新任务 → 有界等待（= 锁租约 2 分钟）已启动旧 goroutine 完成或按租约回收作废 → 之后才启用新事务路径 | INV-04、05、09、24、25、26、42；新旧并存竞争回归 |
| **S4** | 待批准 | 普通刷新控制器状态机（§7.2；删除 stale 独立时间线） | INV-10、11、22、32；时间线用例 |
| **S5** | 待批准 | 调度三档 + trial（§4.4）+ 正式优先级语义 | INV-12、13、29、41 |
| **S6** | 待批准 | Probe 状态机重写，替换 legacy envelope（双窗口、判定表、WAL、SentUnknown、抑制、熔断隔离）；移除 `legacy_refresh_txn` 标签 | INV-06、07、08、14～19、27、36 |
| **S7** | 待批准 | CPA 同步控制器（TTL、candidates 喂入、Degraded/FailClosed）、Bootstrap 接线、Management 收尾 | INV-02、03、20、21、23、31、33、34、35、37 |

---

## 10. 不变量总表（INV-01～42，完整列出）

| 编号 | 不变量 |
|---|---|
| INV-01 | Resource 端点响应中不出现任何账号 ID、别名、额度、优先级、熔断或日志等业务数据 |
| INV-02 | 任意时刻，插件对 OpenAI 的请求只针对**最后确认的最高层**实例；Degraded 期间必须带 `degraded_roster` 标记，且 Degraded 持续不超过 `roster_degraded_max` |
| INV-03 | G 失效后返回的结果不写回活动状态，不产生误导性成功日志 |
| INV-04 | LoginEpoch/绑定指纹校验失败的凭据写回被静默丢弃，不覆盖新凭据（含 in-flight token 刷新与外部重登录的竞争） |
| INV-05 | 同实例同 op-class 的 in-flight 请求最多一个；等待方共享同一结果 |
| INV-06 | `probe_send` 永不与任何请求合并 |
| INV-07 | Probe precheck→send→verify 序列不被同实例其他写操作插入 |
| INV-08 | Probe 序列任一步失败后，instance lock 在有界时间内释放，普通刷新可继续 |
| INV-09 | 全局并发按单次 HTTP 请求计费：并发=1 时，Probe 等待期内其他实例仍能发出请求 |
| INV-10 | `stale_after` 在任何时间线上不独立触发请求（仅真实请求遇 Stale 触发一次 stale_recovery） |
| INV-11 | Aging 状态不触发请求 |
| INV-12 | 全部实例 Unknown/Stale 时，首条真实请求能选出账号（乐观放行），且只触发一轮刷新 |
| INV-13 | Exhausted 且 reset 已过的实例按 Unknown 处理，可被乐观选取 |
| INV-14 | 熔断开启不阻断到期 Probe；Probe 成功或失败均不改变熔断计数与状态 |
| INV-15 | 熔断半开成功计数只被真实业务请求推进 |
| INV-16 | `usage_limit_reached` 不递增熔断失败计数 |
| INV-17 | 五小时窗口与长周期窗口的状态、退避、截止互不影响；一次 Probe 结果可同时更新两窗口数据但不合并任务状态 |
| INV-18 | 时间跳跃（睡眠唤醒）后，同一窗口至多产生一次 Probe 序列，无积压重放 |
| INV-19 | 持久化状态仅限已定义的合法状态集（副作用 WAL 记录除外）；从任意持久化组合启动均能收敛到合法状态 |
| INV-20 | roster 确认实例删除或降层后，其全部待执行截止被取消，后续无任何请求；例外仅限 Degraded 窗口内且带标记 |
| INV-21 | 同步失败时活动集合不清空、G 不变；Degraded 超阈值后后台请求停止而非继续 |
| INV-22 | 普通刷新 Dormant 期间实例与额度缓存保留，账号卡片不消失，Management 可读 |
| INV-23 | 认证确认不可恢复（AuthBlocked）后，该实例所有业务线自动请求停止；仅外部 LoginEpoch++ 恢复 |
| INV-24 | 每个对外请求带且只带一个来源标签，与实际触发方一致（S3 迁移期允许 `legacy_refresh_txn`） |
| INV-25 | 租约回收后，旧 ExecutionToken 的结果与成功日志不写回、不记录 |
| INV-26 | Probe verify 只使用满足 `read_start_seq > send_fence_seq` 的额度读取 |
| INV-27 | 崩溃发生在 send 与 verify 之间时，重启先 verify；抑制窗口内不重发 |
| INV-28 | 同一 Identity 的实例替换后，旧实例的 in-flight 写回不被新实例继承 |
| INV-29 | 每 AuthInstance 同时最多一个 optimistic trial；trial 期间不再被乐观放行 |
| INV-30 | 状态文件截断、损坏或版本不符时安全恢复，任何路径不泄露凭据 |
| INV-31 | 多个 CPA 同步唤醒来源并发时，同一时刻只执行一次 roster sync |
| INV-32 | Dormant 期间普通刷新零 OpenAI 请求，但已启用的到期 Probe 照常唤醒执行 |
| INV-33 | 插件自身 token 刷新（含 refresh token 轮换）不递增 LoginEpoch、不递增 G、不解除 AuthBlocked；仅外部重新登录解除 |
| INV-34 | 低 CPA 层账号不出现在正常 Management payload，也不产生任何 OpenAI 请求 |
| INV-35 | RosterFailClosed 状态下无任何后台 OpenAI 请求；真实 pick 不受影响 |
| INV-36 | `probe_send` 租约超时后仅进入 SentUnknown 走 verify-first，不自动重签发送；抑制窗口对恢复路径与正常路径一致生效 |
| INV-37 | 用户经 Management 修改的注解与设置，在持久化成功前不返回保存成功 |
| INV-38 | fence 序号跨崩溃/重启严格单调不回退；send_fence_seq 先于发送随 sending WAL 落盘 |
| INV-39 | 任何已签发的 fence 序号 ≤ 已持久化并 fsync 的 `reserved_ceiling` |
| INV-40 | SaveAuth 成功与转换链 applied 标记之间崩溃，重启后凭据观察不被误判为外部登录（planned transition WAL 保证） |
| INV-41 | evidence refresh 仍 pending 时 trial 不释放；TrialUnknown 按递增退避，不存在周期性自动重新放行 |
| INV-42 | S3 迁移期，旧路径全部对外请求（含 reset-credits、Probe POST、Probe 后置读取）均在 legacy envelope 锁内执行；旧 goroutine drain/回收完成后才启用新事务路径 |

---

## 11. 配置项职责

**面向用户**：`quota_refresh_interval`（活跃期刷新节奏）、`stale_after`（缓存可信阈值，不触发请求）、`refresh_active_window`（无真实请求多久后普通刷新休眠）、`max_refresh_concurrency`（同时 HTTP 数）、`handle_enabled`、`enable_reset_probe`（是否启用 Probe）、`probe_on_provisional_roster`（默认 false，风险模式）、`monthly_mode`、`refresh_on_startup`（仅普通刷新）。

**内部/高级策略**（不与用户配置争夺计时器）：普通刷新退避、Probe 退避、`refresh_after_reset_delay`、熔断阈值/等待/半开条件、锁租约（2m）、trial 超时（60s）与 TrialUnknown 退避（1/2/5m，cap 15m）、`roster_degraded_max`（30m）、`probe_resend_suppress`（max(verify 宽限, 10m)）、`skew_tol`（120s）、`provisional_probe_max_age`（4h）、`unknown_reset_recheck`（30m）、fence 块大小（2^20）、转换链容量（8–16 代）与时限（24h）、CPA 同步 TTL（5m）。

---

## 12. Mock 测试设计优先场景（已获审核排序，v4 批准后展开）

1. Probe 已发送、`sent` WAL 落盘前崩溃。
2. `sending` WAL 落盘后、HTTP 发出前崩溃。
3. fence 块预留协议各崩溃点（ceiling 落盘前/后、签发后）。
4. 连续 SaveAuth `F0→F1→F2` 与同步跳读/乱序观察。
5. 相同实例删除重加：有宿主 revision 与无 revision 两条路径。
6. Degraded 满 30 分钟后后台 fail-closed，真实 pick 不受影响。
7. legacy envelope 与 manual/quota 意图并发。
8. 首次 baseline 的四种输入（P2/P3/P4/P5）。
9. 五小时、周与未知长度窗口的 reset 异常组合（倒退、超大跳跃、缺失）。
10. 高频 pick 且 evidence refresh 超过 60 秒，验证 trial 不重复放行。

---

## 13. 提交给 Codex 的 v4 复审任务清单

1. 自包含性核验：确认 v4 无任何依赖历史版本的规范引用；INV-01～42 与全文规则逐条对齐，列出遗漏。
2. §1.4 转换链：跨代确认 + WAL 的完整性；CredentialAmbiguous 的人工确认出路是否需要 Management 操作定义。
3. §3.2 块预留协议各崩溃点推演（对应 Mock 场景 3）。
4. §6.3 P1–P5 与主表 1–8 的穷举复核：未知 window_len 下"前跳一律 ActivatedNew"的误报面是否可接受；周期确立条件（连续两次一致）是否足够。
5. §9 S3 的 envelope 范围对照 v0.1.6 源码复核（refresh.go:622/649/662/669 及其他遗漏调用点）；drain 规则与租约回收的交互。
6. §4.4 TrialUnknown 退避与熔断、Probe 退避的组合矛盾扫描。
7. 内部策略默认值全表组合矛盾扫描（§11 列出的全部数值）。
8. 批准判定：确认 S2 可进入；若批准，按 §12 场景顺序展开全量 Mock 设计。

---

## 附：核心原则

> 账号加载不等于额度刷新；普通额度刷新不等于 Probe；业务熔断不等于后台任务熔断；插件维护的实例身份是 best-effort fencing 而非强身份；不同业务线共享安全的请求执行设施，但绝不共享混乱的触发状态。