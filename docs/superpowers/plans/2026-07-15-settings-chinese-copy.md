# 调度设置中文文案 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把中文设置区的英文技术词替换为用户可理解的中文，并为两个额度探测开关添加说明。

**Architecture:** 只修改 `management.go` 内嵌资源页面的中文静态文案和中文翻译表；配置键、Management JSON 与保存逻辑保持不变。使用资源页面测试锁定文案，同时断言旧的混合中英文标签不再出现。

**Tech Stack:** Go、`html/template` 内嵌页面、Go `testing`

## Global Constraints

- 不修改 `enable_reset_probe`、`probe_on_provisional_roster`、`monthly_mode` 等配置键。
- 不修改默认值、保存逻辑、导入导出结构或调度行为。
- 英文界面保留现有英文文案。
- 中文设置区不得再展示 `reset probe`、`provisional roster` 或 `Monthly 模式`。
- 保留未跟踪的 `docs/superpowers/plans/2026-07-13-s7-final-review-fixes.md`。

---

### Task 1: 替换中文设置文案并增加帮助说明

**Files:**
- Modify: `management_test.go:829-860`
- Modify: `management.go:1237-1243`
- Modify: `management.go:1291-1295`

**Interfaces:**
- Consumes: `renderStatusPageForTest(t, store) string`
- Produces: 保持现有 HTML 控件 ID 和 `collectSettingsPayload()` 字段不变的中文设置页面

- [ ] **Step 1: Write the failing resource-page test**

在 `management_test.go` 增加：

```go
func TestStatusPageUsesPlainChineseProbeAndMonthlyCopy(t *testing.T) {
	page := renderStatusPageForTest(t, NewPluginState(DefaultConfig()))
	for _, want := range []string{
		"自动激活新的额度周期",
		"当额度重置时间已经到达，但 OpenAI 尚未生成新的额度周期时",
		"账号列表未确认时仍允许额度探测（高风险）",
		"通常应保持关闭",
		"月度账号使用方式",
		"优先使用月度账号",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("status page missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"启用自动 reset probe",
		"启用 provisional roster Probe 风险模式",
		"Monthly 模式",
		"优先使用 Monthly",
	} {
		if strings.Contains(page, unwanted) {
			t.Fatalf("status page still contains mixed-language copy %q", unwanted)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify RED**

Run:

```powershell
go test . -run '^TestStatusPageUsesPlainChineseProbeAndMonthlyCopy$' -count=1
```

Expected: FAIL，报告缺少 `自动激活新的额度周期`。

- [ ] **Step 3: Replace the Chinese labels and add help text**

在 `management.go` 的设置 HTML 中使用：

```html
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.enableResetProbe">自动激活新的额度周期</span><input id="enableResetProbe" name="enable_reset_probe" type="checkbox"></label><p class="setting-help" data-i18n="settings.enableResetProbeHelp">当额度重置时间已经到达，但 OpenAI 尚未生成新的额度周期时，发送一次极小的 Codex 请求尝试激活新周期，然后重新读取额度确认结果。可能消耗少量额度。</p></div>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.provisionalProbe">账号列表未确认时仍允许额度探测（高风险）</span><input id="probeOnProvisionalRoster" name="probe_on_provisional_roster" type="checkbox"></label><p class="setting-help" data-i18n="settings.provisionalProbeHelp">CPA 暂时无法确认当前账号及优先级时，允许插件使用最近一次保存的账号列表执行额度重置探测。每次都会重新验证账号凭据，但仍无法保证账号未被删除或调整优先级。通常应保持关闭。</p></div>
<label class="field"><span data-i18n="settings.monthlyMode">月度账号使用方式</span><select id="monthlyMode"><option value="expiry_order" data-i18n="settings.expiryOrder">按到期时间排序</option><option value="priority" data-i18n="settings.monthlyPriority">优先使用月度账号</option></select></label>
```

在现有 `.toggle` 样式旁加入：

```css
.setting-with-help{margin-bottom:12px}.setting-with-help .toggle{margin-bottom:4px}.setting-help{margin:0;color:#6b7280;font-size:12px;line-height:1.45}
```

同时把 reset Probe 顶部警告改为纯中文：

```html
<strong data-i18n="resetProbe.warningTitle">自动激活新的额度周期默认关闭</strong>
<span data-i18n="resetProbe.warningBody">开启后，调度器会在额度重置时间已到但新周期尚未生成时，发送一次极小的 Codex 请求尝试激活新周期。</span>
```

在 `zh-CN` 翻译表加入相同中文；英文翻译表新增 `settings.enableResetProbeHelp`、`settings.provisionalProbe` 和 `settings.provisionalProbeHelp`，保留英文界面能力。

- [ ] **Step 4: Run focused tests to verify GREEN**

Run:

```powershell
go test . -run 'TestStatusPage(UsesPlainChineseProbeAndMonthlyCopy|ShowsResetProbeWarningOnlyAfterProtectedLoadWhenDisabled|UsesCollapsedSettingsAndNoHardReload)' -count=1
```

Expected: PASS。

- [ ] **Step 5: Run full verification**

Run:

```powershell
go test ./... -count=1 -timeout 180s
go test -race ./... -count=1 -timeout 300s
go vet ./...
.\scripts\check_refactor_gates.ps1 -Stage S7
git diff --check
```

Expected: 全部退出码为 `0`，S7 报告 49 个精确 Mock owner 通过。

- [ ] **Step 6: Commit**

```powershell
git add management.go management_test.go docs/superpowers/plans/2026-07-15-settings-chinese-copy.md
git commit -m "fix: clarify Chinese scheduler settings"
```

- [ ] **Step 7: Build and deploy the DLL**

Run:

```powershell
.\build.ps1
Get-FileHash -Algorithm SHA256 .\dist\codex-quota-scheduler.dll
```

通过 CPA 插件管理先卸载当前 DLL、复制新 DLL、重新启用插件，然后在已登录页面确认三个新标签及两段帮助文字可见，现有设置值保持不变。
