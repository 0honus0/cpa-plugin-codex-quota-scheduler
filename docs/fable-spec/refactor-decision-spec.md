# Codex Quota Scheduler 重构决策方案（Decision Spec v2）

> **版本说明**：v2 依据 Codex 审核意见（12 项 + 补充不变量）全面修订。v1 作废。
> 本文件仍是对《目标设计文档》的决策补充；冲突时以本文件为准。
>
> **v1 → v2 变更对照**（对应 Codex 审核编号）：
>
> | 审核项 | 处置 | 落点 |
> |---|---|---|
> | 1 verify 合并因果性 | 接受：causal barrier | §3.3、INV-26 |
> | 2 租约 fencing | 接受：ExecutionToken | §1.3、§5.4、INV-25 |
> | 3 启动 roster 来源 | 接受并细化：三来源 + Provisional 有界放行 | §2（新增）、S1 |
> | 4 Probe 崩溃重发 | 接受：副作用 WAL + best-effort-once | §5.3、§8.1、INV-27 |
> | 5 身份/实例混淆 | 接受：Identity / AuthInstance / BindingEpoch 拆分 | §1 全面重写、INV-28 |
> | 6 同步失败矛盾 | 接受有限期 fail-open；范围收窄至后台业务线 | §7.3、INV-02/20/21 修订、INV-35 |
> | 7 D7 越界 | 接受：正常 payload 仅最高层 | §7.1 |
> | 8 乐观放行风暴 | 接受：optimistic trial lease + Preferred 跨层优先 | §4.3、§4.4、INV-29 |
> | 9 CredentialEpoch 过宽 | 接受：拆为 BindingEpoch / LoginEpoch / TokenEpoch | §1.2 |
> | 10 lazy 判定不精确 | 接受：纯函数判定表 | §6.2 |
> | 11 实施顺序 | 接受：改为 8 步 | §9 |
> | 12 持久化不完整 | 接受：新增持久化规范 | §8 |
> | 补充不变量 | 全部采纳（INV-25～34），另增 INV-35 | §10 |

---

## 1. 身份、实例与版本模型（重写）

### 1.1 三个不同的"是谁"

| 概念 | 定义 | 用途 |
|---|---|---|
| **AccountIdentity** | ChatGPT 账号 ID（凭据 ID token claims），次键 email（大小写不敏感） | **仅**用于继承插件内部配置：别名、插件优先级、分组、标签、备注、Probe 历史统计 |
| **AuthInstanceID** | CPA 宿主标识的具体认证实例（宿主提供的 auth ID / 文件标识） | **操作单位**：admission 集合、锁、去重、请求、写回、日志全部以它为键 |
| Identity↔Instance 绑定 | 一个 Identity 可能对应 0/1/多个 Instance；绑定关系由 CPA 同步维护 | 同一身份出现两个 auth 实例时，两个实例独立参与调度，配置继承同一份 Identity 记录 |

身份不可解析（无 account ID 且无 email）的实例：不做配置继承，以 AuthInstanceID 为独立记录参与调度，UI 标记"身份不可识别"。**修订 v1 D10**：不再拒绝纳入活动集合——排除会造成"CPA 认为可用而插件拒绝调度"的行为分裂；只降级为无继承。

### 1.2 版本号族

| 版本号 | 作用域 | 递增时机 | 用途 |
|---|---|---|---|
| **TierGeneration（G）** | 全局 | 最高层的 **(AuthInstanceID, LoginEpoch) 多重集** 发生任何变化 | 层级写回防护。"删除又加回同一身份"必然导致 InstanceID 或 LoginEpoch 变化 → G++ |
| **AuthBindingEpoch** | 每 Identity | Identity 与 Instance 的绑定关系变化（换文件、增删实例） | 配置继承迁移时的一致性检查 |
| **LoginEpoch** | 每 Instance | **证明真实重新登录**的字段变化：refresh token 值、账号主体（sub/account id）变化 | 唯一能解除 AuthBlocked 的信号；凭据写回校验 |
| **TokenEpoch** | 每 Instance | access token 更新（含插件自身刷新） | 仅用于日志与调试，**不**参与任何门控，**不**解除 AuthBlocked |
| **ExecutionToken** | 每次授权执行 | 协调层每签发一次执行（含租约回收后重签） | 写回 fencing（§5.4） |

凭据比较不做整体 JSON 哈希（字段顺序、无关 metadata 会误报）：LoginEpoch 只比较提取后的具名字段（refresh token、subject）。

### 1.3 写回校验矩阵（v2）

**所有写回统一先校验 ExecutionToken**（fencing：租约回收即旧 token 作废，旧 worker 的结果与成功日志一律丢弃）。在此之上：

| 写回内容 | 额外校验 |
|---|---|
| 额度数据 / 重置时间 | G 有效，Instance 仍在当前层 |
| 刷新后的凭据保存 | + 该 Instance 的 LoginEpoch 与请求 snapshot 一致（不一致 → 静默丢弃凭据写回，记录日志；额度数据部分仍可写） |
| Probe 窗口状态推进 | G 有效，Instance 仍在当前层 |
| Exhausted / 临时不可用标记 | G 有效，Instance 仍在当前层 |

---

## 2. 启动 Roster 来源与 Bootstrap（新增，回应审核项 3）

**前置事实**：v0.1.6 中 CPA 优先级仅出现在 `scheduler.pick` candidates 中；现有 `ListAuths/GetAuth` 路径未确认包含优先级字段。因此"启动即计算最高层"目前缺少已验证的数据来源。

### 2.1 Roster 的三个合法来源（按权威度）

1. **宿主 roster API**（若存在）：带优先级的受保护账号清单接口。**S1 阶段的 P0 调查项**：确认 CPA Host API / Management API 是否提供；不提供则向上游提交能力需求（关联 issue #4196 的沟通渠道）。
2. **`scheduler.pick` candidates**：每次真实请求宿主逐次提供，是该时刻的权威 roster + 优先级信息。**每次 pick 都视为一次免费的 roster 确认**，喂给 CPA 同步控制器（更新 last-confirmed、必要时重算 G）。
3. **持久化的 last-confirmed roster**：仅作启动时的 Provisional。

### 2.2 Bootstrap 状态机

```text
启动
→ 尝试来源 1（若 API 已确认存在）：成功 → Confirmed roster，正常运行
→ API 不存在或失败：
    加载持久化 last-confirmed roster → 状态 = Provisional（沿用保存的 G）
    Management 页标记 provisional
    普通刷新：Dormant（本来就等真实请求，无影响）
    Probe：
      - roster 确认时间距今 < provisional_probe_max_age（内部策略，默认 24h）
        → 允许在 Provisional roster 上执行到期 Probe
      - 否则 → 全部窗口进入 WaitingRoster，等待首次 pick 或 API 确认
→ 首次 pick candidates 到达 → roster 确认/替换（可能 G++），WaitingRoster 解除
```

理由：pick candidates 保证真实请求路径**永远**基于权威 roster；Provisional 只承担后台业务线的有界风险，且以确认时效为界。

---

## 3. 统一协调层执行模型

### 3.1 两级资源与固定获取顺序（同 v1）

```text
资源一：per-AuthInstance mutex（同实例所有修改凭据或状态的操作串行）
资源二：global semaphore，容量 = max_refresh_concurrency
        计费单位 = 单次对外 HTTP 请求
```

固定顺序：`acquire(instance_lock) → 每次 HTTP 前 acquire(slot) → HTTP → 立即 release(slot) → … → release(instance_lock)`。
禁止：持 instance lock 等另一 instance lock；持 slot 等任何 lock；持 slot 做非 HTTP 等待。

### 3.2 Probe 关键序列（同 v1 + attempt_id）

每次 Probe attempt 分配全局唯一 `attempt_id`。`precheck → send → verify` 在一次 instance lock 持有期内完成；任一步失败立即释放锁，重试为新意图（新 attempt_id）；send 与 verify 之间的传播等待不持 slot，超过锁租约阈值则释放锁、verify 以独立意图重新排队（携带原 attempt_id 与 causal barrier）。

### 3.3 去重与合并（修订：causal barrier）

去重键：`(AuthInstanceID, op-class, G)`。

`quota_read` 意图携带可选参数 `started_after`（causal barrier，时间戳或单调序号）：

- **协调层合并规则**：一个 `quota_read` 意图只能 join 满足 `read.started_at > intent.started_after` 的 in-flight 读取；无满足者则新起一次读取。
- `probe_precheck`：`started_after` 为空——它只需当前额度，可与任何 in-flight `quota_read` 合并。
- `probe_verify`：`started_after = 该 attempt 的 sent_at`——**只能使用对应 Probe 发送成功之后启动的读取**；不与 send 前启动的任何读取合并，不跨 attempt 合并。
- `probe_send`：不与任何请求合并。
- `token_refresh`：同实例 in-flight 可合并；结果保存校验 LoginEpoch。
- `manual_refresh`：bypass 缓存新鲜度，但可 join 同实例 in-flight `quota_read`。

### 3.4 推荐实现模型（同 v1，待 S1 验证宿主线程约定）

意图队列 + 单一协调 goroutine 持有全部可变状态 + worker 只做 HTTP。S1 需确认 `scheduler.pick` 在 c-shared/CGO 下的调用线程模型与允许延迟。

---

## 4. 缓存状态机与调度可选性

### 4.1 缓存年龄四态（同 v1）

`Unknown / Fresh(≤interval) / Aging(≤stale_after) / Stale(>stale_after)`。Aging 纯标签，不触发刷新。全系统刷新触发来源仍仅五类。

### 4.2 Exhausted 时效（同 v1）

reset 已过的 Exhausted 视同 Unknown，调度器现场判断。

### 4.3 三档可选性 + 跨层扫描顺序（修订）

```text
Preferred     : Fresh/Aging 且未耗尽、无熔断、无认证失败
Opportunistic : Unknown；Stale 且上次已知可用；Exhausted 但 reset 已过
Excluded      : 确认耗尽未到 reset；AuthBlocked；熔断开；临时不可用；optimistic trial 进行中
```

**选取顺序（采纳审核项 8 建议）**：

1. 按插件优先级从高到低扫描**全部层的 Preferred**，取第一个。
2. Preferred 全空 → 按插件优先级从高到低扫描 **Opportunistic**，取第一个并开启 optimistic trial。
3. 全部 Excluded → 返回无法选取，进入 CPA fallback。

即：已知可用的低插件优先级账号，优先于状态未知的高插件优先级账号。可靠性压过优先级；冷启动全 Unknown 时自然回落到纯优先级顺序。

### 4.4 Optimistic Trial Lease（新增）

- 每 AuthInstance 同时最多一个 trial。选中 Opportunistic 账号即开启 trial（记录开启时间），trial 期间该实例归入 Excluded（不再被乐观放行；若期间刷新确认可用则转 Preferred 正常参与）。
- 释放条件（任一）：usage feedback 到达（成功或 `usage_limit_reached`）；额度刷新结果写回；超时（`optimistic_trial_timeout`，内部策略，默认 60s）。
- trial 开启同时提交对应刷新意图（`scheduler_initial` / `scheduler_stale_recovery`）。

---

## 5. 时间、租约与 Fencing

1. **绝对时间截止**、tick 仅为检查器（同 v1）。
2. **启动/唤醒重算不重放**（同 v1）：时间跳跃检测后按 `(持久化 reset 时间, 最近快照, now)` 推导，每窗口至多一次序列。
3. **持久化原则（修订）**：执行中状态不持久化；**但外部副作用的 WAL 记录必须先于副作用落盘**（详见 §8.1）。启动时无 WAL 记录的中间态一律回退 pending；有 WAL 记录的按 §6.3 恢复。
4. **租约 + Fencing（修订）**：任务进入执行态记 `startedAt` 并持有 ExecutionToken；超过租约（内部策略，默认 2 分钟）→ 协调层回收：作废该 token、释放锁、按退避重签。旧 worker 稍后返回时因 token 失效，结果与成功日志全部丢弃（INV-25）。

---

## 6. Probe 控制器

### 6.1 双窗口状态机（v2）

```text
Idle → WaitingReset → PendingCheck
PendingCheck --授权--> [precheck 读取] --classify-->
   ActivatedNew / ActivatedInferred → Confirmed
   NotDueYet → WaitingReset（重算截止）
   StillLazy → [WAL: attempt sending] → [send] → [WAL: sent] 
             → SentAwaitingVerify → [verify 读取, barrier=sent_at] --classify-->
                 Activated* → Confirmed
                 StillLazy / Ambiguous → RetryWait
   Anomaly → AnomalyHold（记录，下周期重评，不发 Probe）
任一步失败 → RetryWait（Probe 退避）；401 → AuthBlocked
AuthBlocked --LoginEpoch++--> PendingCheck
WaitingRoster（Bootstrap §2.2）--roster 确认--> 按重算进入对应状态
任意状态 --实例退出最高层 / G 失效--> Idle（取消截止，作废未完成 attempt）
```

持久化状态集：`Idle / WaitingReset / PendingCheck / SentAwaitingVerify / RetryWait / Confirmed / AuthBlocked / AnomalyHold / WaitingRoster`。其中 SentAwaitingVerify 是**副作用事实**，不是执行中状态，必须持久化。

### 6.2 窗口判定纯函数（新增，回应审核项 10）

```text
classifyWindow(window_type, prev_reset_at, prev_snapshot, snap, now, cfg) → 判定
cfg：skew_tol（默认 120s）、window_len（5h / 上游报告的长周期）、
     refresh_after_reset_delay（沿用现配置）
```

| # | 条件（按序匹配，首个命中生效） | 判定 |
|---|---|---|
| 1 | snap.reset_at 存在 且 snap.reset_at > prev_reset_at + skew_tol | **ActivatedNew**（窗口已翻转） |
| 2 | snap.reset_at 存在 且 snap.reset_at < prev_reset_at − skew_tol | **Anomaly**（reset 倒退） |
| 3 | snap.reset_at 与 prev 差值 > 2 × window_len | **Anomaly**（异常跳跃） |
| 4 | now < prev_reset_at + refresh_after_reset_delay | **NotDueYet** |
| 5 | snap.reset_at 缺失 且 snap 用量相对 prev_snapshot 已清零/回满 | **ActivatedInferred** |
| 6 | snap.reset_at 缺失 且 用量与 prev 相同 | **Ambiguous**（按 StillLazy 处理，但该 attempt 后若仍 Ambiguous → RetryWait，不连发） |
| 7 | snap.reset_at ≈ prev（±tol）且 已过 delay 且 用量未清零 | **StillLazy** |
| 8 | 其余 | **Ambiguous** |

- prev_reset_at 从未知（首次）：以 snap 建立基线，判定 NotDueYet 并进入 WaitingReset。
- **双窗口推进**：一次额度响应同时喂给两个窗口，各自独立调用 classify、独立推进；一次 send 的 attempt 记录其意图激活的窗口集合，verify 按窗口分别判定，一个窗口 Confirmed 不代表另一个。

### 6.3 崩溃恢复与重复抑制（回应审核项 4）

目标明确为 **best-effort-once**：严格 exactly-once 不可达。

- WAL 中存在 `phase=sending`（发送结果未知）或 `phase=sent` 的 attempt → 启动后**先 verify**（等待 `verify_not_before`），禁止立即重发。
- 重复抑制窗口：同一窗口自该 attempt `created_at` 起 `probe_resend_suppress`（内部策略，默认 max(verify 宽限, 10 分钟)）内不允许新的 send；抑制期后仍 StillLazy 才允许新 attempt。

### 6.4 与熔断的双向隔离（同 v1 四条硬规则）

熔断不阻断 Probe；Probe 不修改熔断；Probe 失败不计熔断；Probe 仅做事实纠正（窗口数据 + 对应窗口 Exhausted 清除）。半开只由真实业务请求推进。AuthBlocked 是唯一跨业务线状态，仅 LoginEpoch++ 解除（TokenEpoch 变化不解除，INV-33）。

---

## 7. 边界决策（修订）

### 7.1 D7（修订，回应审核项 7）

正常 Management 账号 payload **只包含当前最高层账号**——与"低层不加载、不显示、不调度"的既有产品要求一致。手动刷新仅限最高层实例，越界返回明确错误。若未来需要观察 CPA 全量 roster：另设独立的受保护诊断端点 + 显式配置开关（默认关闭），不混入活动账号列表；本次重构不实现。

### 7.2 D8（同 v1）

`refresh_on_startup` 只约束普通刷新控制器；Probe 启动读取不受其控制，文档与 UI 显式说明。

### 7.3 CPA 同步失败策略（修订，回应审核项 6）

**关键事实**：真实 `scheduler.pick` 的 candidates 由宿主逐次提供，真实请求路径不依赖同步缓存——降级策略只需覆盖后台业务线（普通刷新、Probe）。

有限期 fail-open：

```text
同步失败 → 进入 Degraded（记 degraded_since）
Degraded 且 now − degraded_since ≤ roster_degraded_max（内部策略，默认 30 分钟）：
  后台业务线继续使用 last-confirmed roster
  所有请求标记 degraded_roster: true
超过阈值 → RosterFailClosed：
  暂停全部后台 OpenAI 请求（普通刷新意图丢弃、Probe 窗口进入 RosterHold）
  真实 pick 不受影响（candidates 权威）
  Management 页显著告警
恢复：任一成功同步或任一次 pick candidates 到达 → 确认 roster、解除、RosterHold 窗口重算
```

INV-02、INV-20、INV-21 的措辞相应修订（见 §10）。

---

## 8. 持久化规范（新增，回应审核项 12）

### 8.1 落盘时机

**必须立即落盘（写穿）**：Probe attempt WAL 记录（sending 先于 HTTP、sent 紧随发送成功）；AuthBlocked 置位/解除；LoginEpoch / AuthBindingEpoch / G 变化；roster 确认（last-confirmed 集合与时间）。
**可合批**：额度快照、日志、统计、UI 注解（内部策略：≤1 次/5s，进程退出时 flush）。

### 8.2 写入路径

- 文件头含 `schema_version`；升级提供逐版本迁移；识别到更高版本 → 只读加载不写回并告警。
- 原子写：序列化 → 同目录临时文件 → fsync → rename 覆盖 → fsync 目录（Windows 下 rename 语义为 best-effort，S2 验证并记录平台差异）。
- 成功写入后保留上一份为 `.bak`。

### 8.3 损坏恢复

主文件解析失败 → 加载 `.bak`；两者皆失败 → 将损坏文件改名 `.corrupt` 保留取证，以安全默认值启动（所有 Probe 窗口回 Idle/WaitingRoster、无 roster → Bootstrap 流程），Management 页告警。任何恢复路径不得导致凭据泄露——**状态文件从不存储** access/refresh token、cookie、authorization header；LoginEpoch 比较所需内容仅存哈希。

---

## 9. 实施顺序（v2，8 步，回应审核项 11）

| 阶段 | 内容 | 验收 |
|---|---|---|
| **S0** | 锁死 Resource/Management 安全边界（补测试；行为应已符合 v0.1.x） | INV-01 |
| **S1** | 宿主能力验证：roster/priority API 是否存在、`scheduler.pick` 线程模型与延迟预算；产出 §2 Bootstrap 路线的最终形态；必要时向 CPA 上游提能力需求 | 书面结论 + Bootstrap 决策定稿 |
| **S2** | Identity / AuthInstance / 各 Epoch 的 vertical slice + 持久化框架（schema version、原子写、迁移、损坏恢复） | INV-28、30；旧状态迁移测试 |
| **S3** | 协调层**只接管 `quota_read`**（去重、barrier、锁、按 HTTP 计费的 slot、ExecutionToken、来源标签） | INV-05、09、24、25、26 |
| **S4** | 协调层接管 `token_refresh`；普通刷新控制器状态机（活跃窗口 + interval + stale_recovery，删除 stale 独立时间线） | INV-04、10、11、22、§7.2 时间线用例 |
| **S5** | 调度可选性三档 + optimistic trial lease + 跨层扫描顺序 | INV-12、13、29 |
| **S6** | Probe 状态机重写（双窗口、classify 判定表、WAL、恢复、抑制窗口、熔断隔离）；`probe_send` 入协调层 | INV-06、07、08、14～19、27 |
| **S7** | CPA 同步控制器（TTL、pick candidates 喂入、Degraded/FailClosed）、Management 收尾、Bootstrap 接线 | INV-02、03、20、21、23、31～35 |

每阶段独立合入；前阶段测试持续通过。

---

## 10. 不变量总表（v2）

INV-01 ～ INV-24 沿用 v1，其中三条修订措辞：

- **INV-02（修订）**：任意时刻，插件对 OpenAI 的请求只针对**最后确认的最高层**实例；处于 Degraded 时必须带 `degraded_roster` 标记，且 Degraded 持续不超过 `roster_degraded_max`。
- **INV-20（修订）**：roster 确认账号删除或降层后，其所有待执行截止被取消，后续无任何请求；同步失败期间的例外仅限 Degraded 窗口内且带标记。
- **INV-21（修订）**：同步失败时活动集合不清空、G 不变；Degraded 超阈值后后台请求停止而非继续。

新增：

| 编号 | 不变量 |
|---|---|
| INV-25 | 租约回收后，旧 ExecutionToken 的结果与成功日志不写回、不记录 |
| INV-26 | Probe verify 只使用对应 attempt send 成功之后启动的额度读取（causal barrier） |
| INV-27 | 崩溃发生在 send 与 verify 之间时，重启先 verify；抑制窗口内不重发 |
| INV-28 | 同一 Identity 的 auth 实例替换后，旧实例的 in-flight 写回不被新实例继承 |
| INV-29 | 每 AuthInstance 同时最多一个 optimistic trial；trial 期间不再被乐观放行 |
| INV-30 | 状态文件截断、损坏或版本不符时安全恢复，且任何路径不泄露凭据 |
| INV-31 | 多个 CPA 同步唤醒来源并发时，同一时刻只执行一次 roster sync |
| INV-32 | 普通刷新 Dormant 期间无 OpenAI 访问，但到期 Probe 仍照常唤醒执行 |
| INV-33 | TokenEpoch 变化（含插件自身刷新）不解除 AuthBlocked；LoginEpoch 变化解除 |
| INV-34 | 低 CPA 层账号不出现在正常 Management payload，也不产生任何 OpenAI 请求 |
| INV-35 | RosterFailClosed 状态下无任何后台 OpenAI 请求；真实 pick 不受影响 |

---

## 11. 提交给 Codex 的 v2 复审任务清单

1. §3.3 的 causal barrier 实现语义：`started_at` 用墙钟还是协调层单调序号？（建议单调序号，请确认无剩余竞争。）
2. §1.2 的 G 定义改为 (AuthInstanceID, LoginEpoch) 多重集后，构造反例验证"删除又加回"的全部路径是否都触发 G++（含 CPA 复用同名文件、复用同 refresh token 的极端情况）。
3. §2.2 Bootstrap：`provisional_probe_max_age=24h` 的风险面评估；WaitingRoster 是否需要对"Probe 启用但长期无人访问"的部署给出额外出路（如可选的定期 roster API 轮询——仅当 S1 确认 API 存在）。
4. §4.3 跨层扫描顺序改变了插件优先级语义，请评估是否需要配置开关保留旧行为（严格优先级模式）。
5. §6.2 判定表：对上游可能的响应形态（reset 缺失、用量字段缺失、长周期窗口 window_len 未知）做穷举核对；skew_tol=120s 是否足够覆盖实际观测到的时钟漂移。
6. §6.3 抑制窗口与 Probe 退避、锁租约三者的取值关系是否存在 verify 永远排不上的组合（v1 问题 5 在 v2 参数下复验）。
7. §8.2 Windows 平台原子写语义的落地方案。
8. S3"只接管 quota_read"期间，未接管的 probe/token 路径与协调层并存的过渡态是否引入新竞争（建议过渡期对同实例加共享 instance lock）。
9. 全表 INV-01～35 与本文件规则逐条对齐检查，补充遗漏。
10. 各内部策略默认值（trial 60s、租约 2min、degraded 30min、suppress 10min、skew 120s、provisional 24h）之间的组合矛盾扫描。

---

## 附：v2 决策一览

| 决策 | 一句话结论 |
|---|---|
| 身份模型 | Identity（配置继承）与 AuthInstance（操作单位）分离；Binding/Login/Token 三个 Epoch 各司其职 |
| G 定义 | (AuthInstanceID, LoginEpoch) 多重集变化即 G++ |
| Fencing | 所有写回统一校验 ExecutionToken；租约回收即作废 |
| Roster 来源 | 宿主 API（待验证）> pick candidates（每次免费确认）> 持久化 Provisional（有界放行 Probe） |
| 同步失败 | 后台业务线有限期 fail-open（30min）后 fail-closed；真实 pick 永不受影响 |
| 调度 | 三档 + 跨层 Preferred 优先 + optimistic trial lease |
| Probe 因果 | verify 带 causal barrier，只用 send 后启动的读取；precheck 自由合并 |
| Probe 幂等 | best-effort-once：副作用 WAL 先行落盘 + verify-first 恢复 + 重复抑制窗口 |
| 判定 | classifyWindow 纯函数判定表，含倒退/缺失/跳跃异常处理 |
| Management | 正常 payload 仅最高层；低层零请求零展示 |
| 持久化 | schema version + 原子写 + .bak 降级 + 副作用写穿 + 无凭据落盘 |
| 实施 | 8 步：安全边界 → 宿主验证 → 身份/持久化 → quota_read → 刷新 → 调度 → Probe → 收尾 |