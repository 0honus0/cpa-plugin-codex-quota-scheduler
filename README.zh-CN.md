# Codex Quota Scheduler

这是一个用于 CLIProxyAPI（CPA）的 Codex 调度/额度维护插件。v0.3.0 是一次不兼容旧实现的重写：旧 admission、legacy refresher、roster controller、WAL 和旧管理页逻辑已经移除。

## 调度规则

- **CPA priority 是跨账号层级的唯一权威。** 插件不会跨 CPA priority 选择账号。
- 插件只在 CPA 当前提供的**同一 priority 层**内排序。
- 同层先比较插件 `scheduler_priority`，再参考当前缓存额度，最后用稳定账号 ID 决胜。
- 所有 CPA 中启用的 Codex 账号都会进入插件管理页，不再只显示最高 priority 层。
- 每个账号都有独立的“后台刷新”开关，**默认开启**。关闭只影响额度刷新/Reset Probe，不改变 CPA priority，也不会修改正常业务请求。

## 额度刷新与 5 小时窗口

后台额度读取使用：

`GET https://chatgpt.com/backend-api/wham/usage`

默认每 30 分钟刷新一次所有启用了后台刷新的 Codex 账号。

5 小时窗口采用 lazy-reset 检测：

1. 到达已知 `reset_at` 后不提前发请求。
2. 默认等待 **5 分钟**。
3. 先重新读取额度。
4. 如果窗口已经自然推进，不发送 Probe。
5. 如果窗口仍停留在旧 reset，才发送原有的最小 Probe。
6. Probe 后约 3 秒重新读取额度确认新窗口。

Reset Probe 使用：

`POST https://chatgpt.com/backend-api/codex/responses`

Probe 的请求体保持既有实现逐字节不变。v0.3.0 只改变**何时调用、哪个账号调用、失败后如何处理**，不改正常业务请求内容，也不重写用户请求。

## 401 / 429

- **429**：记录 Retry-After/恢复时间并在同 priority 层内暂时避开该账号；CPA 自己的原生 cooldown 仍然是跨 priority fallback 的权威。
- **401**：将对应 CPA auth 的 `disabled` 状态设为 `true`，避免 CPA 原生 30 分钟冷却结束后再次误选。插件只修改顶层 `disabled` 字段，不改 token、email、account_id 或其他 credential 字段。
- 401 禁用后，插件仍保留最小状态用于检测凭据变化；重新登录/换 token 导致凭据指纹变化后，插件会把 CPA auth 重新启用。

## 安全边界

- 正常 Codex 业务请求体不会经过本插件修改。
- 插件不会记录 access token、refresh token、id token、Authorization 或 Cookie。
- 持久状态只保存额度、调度偏好、429/401 状态及不可逆凭据指纹，不保存原始 token。
- credential 写权限被收敛成单一操作：只允许切换 CPA auth 的 `disabled` 字段。
- 管理页 API 返回 `Cache-Control: no-store`，并设置基础 CSP / `nosniff`。

## 配置

CPA 配置：

```yaml
plugins:
  enabled: true
  configs:
    codex-quota-scheduler:
      enabled: true
      priority: 1
      handle_enabled: true
      quota_refresh_interval: 30m
      quota_stale_after: 5h
      enable_reset_probe: true
      reset_probe_after_reset_delay: 5m
      reset_probe_retry_delay: 5m
      autoban_429: true
      disable_on_401: true
```

账号自己的 CPA `priority` 不需要设成相同值。例如一个账号 priority 9、另一个 priority 8，CPA 仍会先使用 9；只有 CPA 同时提供同层候选时，插件自己的 `scheduler_priority` 才参与选择。

## 管理页

插件注册 `/plugins/codex-quota-scheduler/status` 管理资源。页面会显示所有启用的 Codex 账号、CPA priority、插件 priority、后台刷新开关、5h/长周期额度、429 ban、401 disabled 和 Probe 状态。

## 构建

需要 Go 1.26 和 C 编译器：

```bash
make test
make vet
make build VERSION=0.3.0
```

Linux amd64 输出：`dist/codex-quota-scheduler.so`。

## 安装

把对应平台动态库放入 CPA 插件目录。例如 Linux amd64：

```text
<CPA data>/plugins/linux/amd64/codex-quota-scheduler.so
```

然后重启 CPA。升级前建议备份现有插件文件和 `config.yaml`。
