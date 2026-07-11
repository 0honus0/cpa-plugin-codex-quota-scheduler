# Codex Quota Scheduler 重构决策方案（Decision Spec v3）

> **版本说明**：v3 依据 Codex 对 v2 的复审意见（6 阻断项 + 3 问题答复 + 补充项）修订。v1、v2 作废。
>
> **v2 → v3 变更对照**：
>
> | 复审项 | 处置 | 落点 |
> |---|---|---|
> | 阻断 1：G 仍不闭环 / LoginEpoch 误报 | 接受：InstanceAdmissionEpoch 入 G；自有轮换登记；显式声明残余不可检测风险 | §1.2、§1.4、S1 |
> | 阻断 2：barrier 必须单调序号 | 接受：fence_seq，高水位持久化，send 前随 WAL 落盘 | §3.3、§8.1、INV-38 |
> | 阻断 3：判定表不可达分支 | 接受：按 无效→向后→超大跳跃→翻转 重排 | §6.2 |
> | 阻断 4：S3/S4 切分不成立 | 接受：合并为完整刷新事务迁移 + legacy_probe_txn 非重入适配 | §9 S3 |
> | 阻断 5：Provisional Probe 风险 | 接受：默认 WaitingRoster；Provisional Probe 改高级 opt-in（默认关），4h 时限 + Probe 前实例验证 | §2.2 |
> | 阻断 6：probe_send 租约不可重签 | 接受：租约策略按 op 类型拆分；send 超时只 verify-first | §5.4、INV-36 |
> | Q4：strict 优先级开关 | 采纳：首版不加开关；插件优先级正式定义为"相同可信等级内优先" | §4.3 |
> | 身份不可解析账号 | 采纳四分法细化 | §1.5 |
> | 注解持久化 / Token 范围 / 恢复抑制 | 全部采纳 | §8.1、§5.4、§6.3、INV-37 |

---

## 1. 身份、实例与版本模型

### 1.1 三个"是谁"（同 v2）

| 概念 | 定义 | 用途 |
|---|---|---|
| **AccountIdentity** | ChatGPT 账号 ID（凭据 claims），次键 email（大小写不敏感） | 仅用于继承插件内部配置（别名、插件优先级、分组、标签、备注、Probe 历史） |
| **AuthInstanceID** | CPA 宿主标识的具体认证实例 | 操作单位：admission、锁、去重、请求、写回、日志 |
| Identity↔Instance 绑定 | 由 CPA 同步维护；一个 Identity 可对应多个 Instance | 多实例独立调度，共享一份 Identity 配置 |

### 1.2 版本号族（修订）

| 版本号 | 作用域 | 递增时机 | 用途 |
|---|---|---|---|
| **InstanceAdmissionEpoch（IAE）** | 每 Instance | **优先**：直接采用宿主提供的 instance revision / CAS token（S1 P0 调查项）。**宿主不提供时**由插件维护，在以下任一事件递增：观察到该 InstanceID 删除后重加；绑定指纹（§1.3）变化；LoginEpoch++ | 进入 G；写回门控 |
| **TierGeneration（G）** | 全局 | 最高层的 **(AuthInstanceID, IAE) 多重集** 变化 | 层级写回防护 |
| **AuthBindingEpoch** | 每 Identity | Identity↔Instance 绑定关系变化 | 配置继承迁移一致性检查 |
| **LoginEpoch** | 每 Instance | **外部重新登录**（§1.4 分类算法判定） | 唯一解除 AuthBlocked 的信号 |
| **TokenEpoch** | 每 Instance | access/refresh token 更新且被分类为自有轮换 | 仅日志调试；不门控、不解除 AuthBlocked |
| **ExecutionToken** | 每次授权的**异步执行** | 协调层签发 | worker 结果写回 fencing（§5.4）。纯本地 Management 修改走普通状态事务，不依赖它 |

### 1.3 绑定指纹

`fingerprint = hash(subject, hash(refresh_token), 宿主实例元数据)`。状态文件只存哈希。协调层写回前，除 (G, IAE) 校验外，对凭据保存类写回额外比对最新同步到的绑定指纹。

### 1.4 凭据变化分类算法（消除 LoginEpoch 误报）

协调层每次 SaveAuth 成功后，登记该实例的**预期指纹**（自有轮换登记）。CPA 同步观察到实例 I 的指纹 F_new 时：

```text
F_new == 已知指纹            → 无事件
F_new == 自有轮换登记的预期指纹 → TokenEpoch++（插件自身刷新，含 refresh token 轮换）
subject 变化，或 refresh_token 变化且不匹配自有登记
                              → LoginEpoch++（外部重新登录）→ IAE++ → G++
仅元数据/字段顺序变化          → 忽略
```

推论：插件自身的 token 刷新**永远不会**导致 LoginEpoch++、G++ 或解除 AuthBlocked（INV-33 修订版）。

### 1.5 残余风险声明（必读）

**宿主不提供 instance revision/tombstone 时，"同 InstanceID、同 refresh token、同 subject 的删除重加"若完整发生在两次 CPA 同步之间，在信息论上不可检测。** 本 spec 接受该残余风险，缓解措施：活跃期 CPA 同步 TTL（约 5 分钟）约束盲区上界；写回前绑定指纹复核；S1 向 CPA 上游提交 revision/tombstone 能力需求（与 priority roster API 同一需求单）。

### 1.6 身份不可解析实例（细化，采纳复审意见）

- 参与宿主提供的真实 `scheduler.pick`（避免 CPA 与插件行为分裂）。
- 仅以 AuthInstanceID 临时管理，**不继承**任何旧配置；重新登录后也**不得**按文件名继承。
- 若缺少身份 claims 但仍能取得 quota/Probe 所需的 ChatGPT account ID：后台业务线正常运行。
- 若连请求所需 account ID 都不可得：不执行 quota/Probe，仅按 optimistic trial 规则参与真实请求。

---

## 2. 启动 Roster 来源与 Bootstrap（修订）

### 2.1 三个合法来源（同 v2）

宿主 roster API（S1 P0 调查，含 priority 与 instance revision 两项能力）＞ `scheduler.pick` candidates（每次 pick 均为一次 roster 确认，喂给同步控制器）＞ 持久化 last-confirmed。

### 2.2 Bootstrap（修订：默认保守）

```text
启动
→ 来源 1 可用：Confirmed，正常运行
→ 来源 1 不可用：
    加载 last-confirmed 为 Provisional（沿用保存的 G），Management 标记 provisional
    普通刷新：Dormant（无影响）
    Probe：默认 → 全部窗口 WaitingRoster，等待首次 pick 或 API 确认
→ 首次 pick candidates → 确认/替换 roster（可能 G++），WaitingRoster 解除
```

**Provisional Probe 为高级 opt-in**（`probe_on_provisional_roster`，默认 `false`）。开启后仍受三重约束：

1. roster 确认时间距今 < `provisional_probe_max_age`（内部策略，默认 **4 小时**）；
2. 每次 Probe 前通过 `ListAuths/GetAuth` 验证 AuthInstance 仍存在且绑定指纹未变，失败则该实例回 WaitingRoster；
3. Management 显著提示，且配置说明明确写出：无权威 priority API 时，实例存在**不能证明**其仍属最高 CPA 层。

---

## 3. 统一协调层执行模型

### 3.1 两级资源与固定获取顺序（同 v2）

per-AuthInstance mutex + global semaphore（按单次 HTTP 计费）。固定顺序：`lock → (每次 HTTP 前 slot → HTTP → 立即还 slot)* → unlock`。禁止持 lock 等 lock、持 slot 等 lock、持 slot 做非 HTTP 等待。

### 3.2 Probe 关键序列（同 v2）

每 attempt 全局唯一 `attempt_id`；序列持锁，任一步失败立即释放锁、重试为新意图；send↔verify 传播等待不持 slot，超锁租约则释放锁、verify 携带原 attempt_id 与 barrier 独立排队。

### 3.3 Causal Barrier（修订：单调序号）

协调层维护全局单调序号 `fence_seq`（uint64）：

- 每次对外读取启动时分配 `read_start_seq`；每次 Probe 发送前分配 `send_fence_seq`。
- **`send_fence_seq` 在发送前随 `phase=sending` WAL 记录一并持久化**——崩溃后仅有 sending 记录时，verify 的 barrier 依然有定义。
- verify 合法条件：`read_start_seq > send_fence_seq`。
- **禁止**使用墙钟时间戳做 barrier 比较（时间回拨、精度、休眠、NTP 均不可靠）。
- 序号高水位随状态持久化；重启后从 `高水位 + 大步长`（如 +2^20）继续，保证跨重启严格单调不回退（INV-38）。

### 3.4 去重与合并（同 v2，barrier 语义按 §3.3）

去重键 `(AuthInstanceID, op-class, G)`。`quota_read` 意图携带可选 `started_after`（fence 序号）：只 join `read_start_seq > started_after` 的 in-flight 读取。precheck 空 barrier 自由合并；verify barrier = 对应 attempt 的 `send_fence_seq`，不跨 attempt 合并；`probe_send` 不合并；`token_refresh` 同实例可合并、保存校验 LoginEpoch 与指纹；`manual_refresh` bypass 新鲜度但可 join。

### 3.5 推荐实现模型（同 v2，待 S1 验证宿主线程约定）

意图队列 + 单协调 goroutine 持有可变状态 + worker 只做 HTTP。

---

## 4. 缓存状态机与调度可选性

### 4.1 / 4.2（同 v2）

四态缓存，Aging 纯标签；刷新触发来源仅五类。Exhausted 且 reset 已过视同 Unknown，现场判断。

### 4.3 插件优先级的正式定义（修订，采纳复审 Q4 答复）

> **插件优先级：在相同可用性可信等级内，优先级越高越先使用。**

```text
Preferred     : Fresh/Aging 且未耗尽、无熔断、无 AuthBlocked
Opportunistic : Unknown；Stale 且上次已知可用；Exhausted 但 reset 已过
Excluded      : 确认耗尽未到 reset；AuthBlocked；熔断开；临时不可用；trial 进行中

选取：全部 Preferred 按插件优先级排序取首
   → Preferred 全空：全部 Opportunistic 按插件优先级排序取首，开启 trial
   → 全部 Excluded：返回无法选取，进入 CPA fallback
```

首版**不提供** strict 优先级开关；未来如有需求，作为显式风险模式另行评估，本次不维护两套调度语义。

### 4.4 Optimistic Trial Lease（同 v2）

每实例同时至多一个 trial；trial 期间归入 Excluded；释放条件：usage feedback / 刷新写回 / 超时（内部策略 60s）；开启即提交对应刷新意图。

---

## 5. 时间、租约与 Fencing

1–3 同 v2（绝对时间截止；启动/唤醒重算不重放；执行中状态不持久化、外部副作用 WAL 先行落盘）。

### 5.4 租约策略按操作类型拆分（修订）

任务执行持有 ExecutionToken，超租约（内部策略 2 分钟）由协调层回收——**回收后的处置按 op 类型区分**：

| op 类型 | 租约超时处置 |
|---|---|
| 纯读取（quota_read 各来源） | 作废旧 token，重签执行 |
| token_refresh / 凭据保存 | 作废旧 token，重新获取凭据后重签 |
| **probe_send** | **不重签**。作废旧 token，对应窗口进入 `SentUnknown`（等价于 SentAwaitingVerify 的结果未知形态），只能 verify-first + 完整抑制窗口（INV-36） |

ExecutionToken 的校验范围限定为 **worker/异步执行的结果写回**；Management 发起的纯本地状态修改（注解、设置）走普通状态事务与持久化路径，不涉及 worker token。

---

## 6. Probe 控制器

### 6.1 双窗口状态机（v3）

```text
Idle → WaitingReset → PendingCheck
PendingCheck --授权--> [precheck 读取] --classify-->
   ActivatedNew / ActivatedInferred → Confirmed
   NotDueYet → WaitingReset（重算截止）
   Anomaly → AnomalyHold（记录，下周期重评，不发 Probe）
   StillLazy → 分配 send_fence_seq → [WAL: sending] → [send]
             → [WAL: sent] → SentAwaitingVerify
             → [verify 读取, barrier=send_fence_seq] --classify-->
                 Activated* → Confirmed
                 StillLazy / Ambiguous → RetryWait
send 租约超时 → SentUnknown → verify-first（§5.4）
任一步失败 → RetryWait；401 → AuthBlocked
AuthBlocked --外部 LoginEpoch++--> PendingCheck
WaitingRoster --roster 确认--> 重算进入对应状态
任意状态 --实例退出最高层 / G 失效--> Idle（取消截止，作废未完成 attempt）
```

持久化状态集：`Idle / WaitingReset / PendingCheck / SentAwaitingVerify / SentUnknown / RetryWait / Confirmed / AuthBlocked / AnomalyHold / WaitingRoster`。

### 6.2 窗口判定纯函数（修订：重排匹配顺序）

`classifyWindow(window_type, prev_reset_at, prev_snapshot, snap, now, cfg)`，cfg 含 `skew_tol`（默认 120s）、`window_len`、`refresh_after_reset_delay`。按序首个命中：

| # | 条件 | 判定 |
|---|---|---|
| 0 | snap 无效 / 关键字段类型错误 / prev 基线不存在（首次） | **Baseline**（以 snap 建立基线 → WaitingReset；无效则 RetryWait） |
| 1 | snap.reset_at 存在 且 < prev_reset_at − skew_tol | **Anomaly**（向后倒退） |
| 2 | snap.reset_at 存在 且 snap.reset_at − prev_reset_at > 2 × window_len | **Anomaly**（向前超大跳跃） |
| 3 | snap.reset_at 存在 且 > prev_reset_at + skew_tol | **ActivatedNew**（正常翻转；含 ≤ 2×window_len 的合法前跳） |
| 4 | now < prev_reset_at + refresh_after_reset_delay | **NotDueYet** |
| 5 | snap.reset_at 缺失 且 用量相对 prev_snapshot 已清零/回满 | **ActivatedInferred** |
| 6 | snap.reset_at 缺失 且 用量与 prev 相同 | **Ambiguous**（按 StillLazy 处理一次；verify 后仍 Ambiguous → RetryWait，不连发） |
| 7 | snap.reset_at ≈ prev（±tol）且 已过 delay 且 用量未清零 | **StillLazy** |
| 8 | 其余 | **Ambiguous** |

`window_len` 未知（长周期上游未报）时：规则 2 退化为固定上限（内部策略，如 45 天），并记录观测值用于校准。双窗口独立 classify、独立推进（同 v2）。

### 6.3 崩溃恢复与重复抑制（修订）

目标 best-effort-once。WAL 存在 `phase=sending`（结果未知）或 `phase=sent` → 启动后先等 `verify_not_before` 再 verify，禁止立即重发。**verify 结果仍为 StillLazy/Ambiguous 时，同样必须等满剩余抑制窗口**（自 attempt `created_at` 起 `probe_resend_suppress`，内部策略，默认 max(verify 宽限, 10 分钟)）才允许新 attempt——恢复路径与正常路径的抑制规则完全一致。

### 6.4 与熔断的双向隔离（同 v2）

四条硬规则不变。AuthBlocked 仅外部 LoginEpoch++ 解除；自有轮换（TokenEpoch）永不解除。

---

## 7. 边界决策（同 v2）

**7.1** 正常 Management payload 仅最高层；手动刷新仅限最高层；全量 roster 观察留待未来独立诊断端点（默认关），本次不实现。
**7.2** `refresh_on_startup` 只约束普通刷新，不约束 Probe 启动读取。
**7.3** 同步失败：后台业务线有限期 fail-open（`roster_degraded_max`，内部策略 30 分钟，带 `degraded_roster` 标记）→ 超阈值 RosterFailClosed（后台请求全停、Probe 入 RosterHold、真实 pick 不受影响）→ 任一成功同步或 pick candidates 到达即恢复。

---

## 8. 持久化规范

### 8.1 落盘时机（修订）

**必须立即落盘（写穿）**：Probe attempt WAL（`sending` 含 `send_fence_seq`，先于 HTTP；`sent` 紧随发送成功）；AuthBlocked 置位/解除；各 Epoch 与 G 变化；roster 确认；fence_seq 高水位（可按块预留后批量推进）；**用户通过 Management 修改的别名、插件优先级、标签、备注、设置——持久化成功后才返回保存成功响应**（INV-37）。
**可合批**：额度快照、日志、统计（内部策略 ≤1 次/5s，退出 flush）。

### 8.2 / 8.3（同 v2）

schema_version + 逐版本迁移 + 高版本只读；temp → fsync → rename → fsync 目录（Windows 语义 S2 验证）；`.bak` 降级、双失败 `.corrupt` 取证 + 安全默认启动；状态文件从不存储 token/cookie/header，指纹只存哈希。

---

## 9. 实施顺序（v3）

| 阶段 | 内容 | 验收 |
|---|---|---|
| **S0** | 锁死 Resource/Management 安全边界（补测试） | INV-01 |
| **S1** | 宿主能力验证（三项）：priority roster API；**instance revision/tombstone**；`scheduler.pick` 线程模型与延迟预算。产出 Bootstrap 与 IAE 来源定稿；必要时向 CPA 上游提交合并能力需求单 | 书面结论 |
| **S2** | Identity / Instance / Epoch 族 + 指纹 + 持久化框架（schema、原子写、迁移、损坏恢复、fence 高水位） | INV-28、30、38；迁移测试 |
| **S3（原 S3+S4 合并）** | 协调层一次性接管**完整单账号刷新事务**（GetAuth → token refresh → SaveAuth → quota read 作为单事务，含去重、barrier、锁、slot、ExecutionToken、来源标签、§1.4 分类）。旧 Probe 路径同阶段包装为 **`legacy_probe_txn` 非重入适配事务**：一次锁获取、事务内部直接调用不产生嵌套意图，从结构上排除自锁 | INV-04、05、09、24、25、26；新旧并存竞争回归测试 |
| **S4** | 普通刷新控制器状态机（活跃窗口 + interval + stale_recovery；删除 stale 独立时间线） | INV-10、11、22；时间线用例 |
| **S5** | 调度三档 + trial + §4.3 正式优先级语义 | INV-12、13、29 |
| **S6** | Probe 状态机重写（替换 legacy_probe_txn；双窗口、判定表、WAL、SentUnknown、抑制、熔断隔离） | INV-06、07、08、14～19、27、36 |
| **S7** | CPA 同步控制器（TTL、pick candidates 喂入、Degraded/FailClosed）、Bootstrap 接线、Management 收尾 | INV-02、03、20、21、23、31～35、37 |

---

## 10. 不变量总表（v3 增补）

INV-01～24 沿用（02/20/21 为 v2 修订措辞）；INV-25～35 沿用 v2。修订与新增：

| 编号 | 不变量 |
|---|---|
| INV-33（修订） | 插件自身 token 刷新（含 refresh token 轮换、TokenEpoch 变化）不递增 LoginEpoch、不递增 G、不解除 AuthBlocked；仅外部重新登录（LoginEpoch++）解除 |
| INV-36 | `probe_send` 租约超时后仅进入 SentUnknown 走 verify-first，不自动重签发送；抑制窗口对恢复路径与正常路径一致生效 |
| INV-37 | 用户经 Management 修改的注解与设置，在持久化成功前不返回保存成功 |
| INV-38 | fence 序号跨崩溃/重启严格单调不回退；send_fence_seq 先于发送随 WAL 落盘 |

---

## 11. 提交给 Codex 的 v3 复审任务清单

1. §1.4 分类算法：自有轮换登记在"SaveAuth 成功但同步先观察到更早指纹"的乱序场景下是否误判为外部登录？登记需要保留多少代预期指纹？
2. §1.5 残余风险声明的接受度：盲区 = 同步 TTL 的论证是否成立（Degraded 期间盲区扩大到 `roster_degraded_max`，是否需要在声明中并入）？
3. §3.3 fence 高水位按块预留（8.1）与"严格单调"的一致性；崩溃丢失未落盘块内序号是否有影响（应无：只要求不回退，不要求连续）。
4. §6.2 v3 判定表穷举复核：规则 0 的 Baseline 引入后，与 Bootstrap/WaitingRoster 的转换是否有环；规则 2 在 window_len 未知退化路径下的误报面。
5. S3 的 `legacy_probe_txn` 适配：审阅 v0.1.6 `refreshAuthVersioned` 及 Probe 调用点，确认"一次锁 + 内部直调"覆盖全部旧路径，列出遗漏调用点清单。
6. §2.2 opt-in Provisional Probe 的三重约束是否足够；`provisional_probe_max_age=4h` 与实际部署场景（无人值守 NAS）的匹配度。
7. INV-01～38 与全文规则最终对齐检查。
8. 内部策略默认值组合矛盾扫描（trial 60s、租约 2min、degraded 30min、suppress 10min、skew 120s、provisional 4h、fence 块预留大小）。
9. 批准判定：以上无阻断项后，确认可进入 S0/S1，并给出 Mock 测试设计应优先覆盖的前 10 个场景排序。

---

## 附：v3 决策一览

| 决策 | 一句话结论 |
|---|---|
| G 定义 | (AuthInstanceID, InstanceAdmissionEpoch) 多重集；IAE 优先取宿主 revision，缺失时插件维护 |
| 残余风险 | 无宿主 revision 时，同步盲区内的完全相同删除重加不可检测——显式接受并以 TTL 约束 + 指纹复核缓解 |
| 凭据分类 | 自有轮换登记区分 TokenEpoch/LoginEpoch；自身刷新永不解除 AuthBlocked |
| Barrier | 协调层单调 fence_seq，禁止墙钟；send_fence_seq 随 sending WAL 先行落盘；高水位跨重启不回退 |
| 判定表 | 无效 → 向后 → 超大跳跃 → 翻转 → 未到期 → 用量推断 → lazy，消除不可达分支 |
| 租约 | 按 op 类型拆分：读可重签、凭据保存重取后重签、probe_send 只 verify-first |
| Bootstrap | 默认 WaitingRoster；Provisional Probe 为 opt-in（默认关、4h、Probe 前实例验证、显著提示） |
| 迁移 | S3 一次接管完整刷新事务 + legacy_probe_txn 非重入适配；S6 替换 |
| 优先级语义 | 相同可用性可信等级内优先；首版无 strict 开关 |
| 持久化 | 用户注解写穿；ExecutionToken 限异步写回；恢复路径抑制窗口与正常一致 |