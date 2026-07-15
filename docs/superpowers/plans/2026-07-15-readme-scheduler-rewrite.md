# README Scheduler Rewrite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the migration- and history-heavy README with accurate English and Chinese documentation centered on the v0.2.0 scheduling model.

**Architecture:** Keep `README.md` as the canonical English entry point and create `README.zh-CN.md` as a complete structural translation. Both files use the same section order, literal configuration keys, API paths, examples, and behavioral claims; verification checks required rules, removed historical text, and cross-language heading parity.

**Tech Stack:** GitHub-flavored Markdown, PowerShell validation commands, Go test suite.

## Global Constraints

- Do not change plugin behavior, source code, configuration defaults, API paths, or version numbers.
- `README.md` remains English; `README.zh-CN.md` is a complete Chinese counterpart.
- Add reciprocal language links near the title of both files.
- The plugin handles Codex accounts only; other providers are ignored.
- Missing Codex CPA auth priority is normalized to `0`.
- Only the highest confirmed CPA auth-priority tier participates in plugin scheduling.
- Selectability precedes plugin priority; unavailable accounts never outrank usable accounts.
- Unavailable accounts ignore plugin priority and sort by known recovery time, earliest first; unknown recovery is last.
- A missing five-hour window is allowed when a valid weekly or monthly long window exists.
- A missing or invalid long window remains Unknown/unavailable and delegates to CPA fallback.
- Remove the v0.2.0 state-file migration notice, the `v0.1.6 Priority Isolation` section, and historical release-by-release narration.
- Avoid unexplained internal terms including `WaitingRoster`, `Capability A/B`, `OperationProbeSequence`, and `provisional roster` in user-facing prose.

---

### Task 1: Rewrite the English README around scheduling behavior

**Files:**
- Modify: `README.md`
- Reference: `docs/superpowers/specs/2026-07-15-readme-scheduler-rewrite-design.md`
- Reference: `docs/fable-spec/refactor-decision-spec.md`

**Interfaces:**
- Consumes: v0.2.0 user-visible scheduling rules and current installation/configuration/API details.
- Produces: the canonical English section order and behavioral wording used by Task 2.

- [ ] **Step 1: Capture the expected failing documentation checks**

Run:

```powershell
rg -n '^## v0\.2\.0 upgrade|^## v0\.1\.6 Priority Isolation|automatically migrates|Historical version' README.md
rg -n '^## How Scheduling Works$|^## Quota Refresh And Reset-Window Activation$' README.md
```

Expected: the first command finds obsolete migration/history content, and the second command finds no new scheduling headings.

- [ ] **Step 2: Replace the English document with the approved structure**

Use this exact heading order:

```markdown
# Codex Quota Scheduler

[简体中文](README.zh-CN.md) | English

## v0.2.0 Highlights
## How Scheduling Works
### 1. Admit the active CPA priority tier
### 2. Classify real availability
### 3. Order selectable accounts
### 4. Order unavailable accounts
### Example
## Quota Refresh And Reset-Window Activation
### Quota refresh
### Reset-window activation
### When CPA cannot confirm the account list
## Features
## Installation
## CPA Configuration
## Management UI
## Privacy And Data Disclosure
## Build
## GitHub Releases
## Management API
## License
```

The opening summary must say that the plugin is a quota-aware optimized Fill First scheduler for Codex accounts inside CLIProxyAPI. The v0.2.0 highlights must emphasize availability-first ordering, optional five-hour quota compatibility, long-window safety, authoritative roster handling, and safe reset-window activation rather than state-file migration.

The four scheduling subsections must state, in plain English:

```text
Admission: ignore non-Codex providers; treat missing Codex CPA priority as 0;
admit only the highest confirmed CPA priority tier; recommend equal priority 0
when every Codex account should participate.

Availability: usable accounts precede exhausted, auth-blocked, circuit-open, and
Unknown accounts. A valid weekly/monthly long window is mandatory. The five-hour
window is optional.

Selectable order: apply the plugin-owned account priority only after the account
is selectable. Preserve the configured weekly/monthly mode and existing
Fill First tie-breakers inside selectable classes.

Unavailable order: ignore plugin priority; order by expected reset/recovery time
from earliest to latest; put unknown recovery last; allow CPA fallback when no
account is selectable.
```

The example must contain four accounts and produce this visible order:

```text
1. Available account with plugin priority 10
2. Available account with plugin priority 0
3. Exhausted account that recovers in 2 hours, regardless of high plugin priority
4. Exhausted or Unknown account that recovers later or has no known recovery time
```

The refresh section must distinguish:

```text
Quota refresh reads ChatGPT quota endpoints and sends no ordinary model request.
Reset-window activation is optional and disabled by default; after reset is due,
it sends one tiny Codex request, verifies quota afterward, and may consume a small
amount of quota.
The high-risk option uses the last saved Codex account list only while CPA cannot
confirm current membership/priorities; credentials are revalidated, but account
membership and priority may be stale, so the option should normally stay off.
```

Keep current literal configuration keys, defaults, endpoint URLs, installation paths, privacy boundaries, build commands, and Management API routes. Condense duplicate resource/management-route explanations and replace release history with current release mechanics only.

- [ ] **Step 3: Verify the English content removes obsolete emphasis and includes every scheduling rule**

Run:

```powershell
$old = rg -n '^## v0\.2\.0 upgrade|^## v0\.1\.6 Priority Isolation|automatically migrates|Historical version' README.md
if ($LASTEXITCODE -eq 0) { $old; exit 1 }
$required = @(
  '## How Scheduling Works',
  'highest confirmed CPA',
  'priority `0`',
  'five-hour window is optional',
  'long window',
  'expected recovery time',
  'CPA fallback',
  '## Quota Refresh And Reset-Window Activation'
)
$text = Get-Content -Raw -Encoding utf8 README.md
foreach ($item in $required) {
  if (-not $text.Contains($item)) { throw "README.md missing: $item" }
}
```

Expected: exit 0 with no missing item.

- [ ] **Step 4: Check Markdown whitespace and review the diff**

Run:

```powershell
git diff --check -- README.md
git diff -- README.md
```

Expected: no whitespace errors; the diff changes documentation only.

- [ ] **Step 5: Commit the English rewrite**

```powershell
git add -- README.md
git diff --cached --check
git commit -m "docs: rewrite scheduler overview"
```

### Task 2: Add the complete Chinese README

**Files:**
- Create: `README.zh-CN.md`
- Reference: `README.md`

**Interfaces:**
- Consumes: the final English section order, examples, configuration blocks, URLs, and scheduling claims from Task 1.
- Produces: a complete Chinese document with reciprocal navigation and equivalent technical meaning.

- [ ] **Step 1: Verify the Chinese README does not exist yet**

Run:

```powershell
if (Test-Path README.zh-CN.md) { throw 'README.zh-CN.md already exists; inspect before overwriting' }
```

Expected: exit 0.

- [ ] **Step 2: Create the full Chinese counterpart**

Use this exact heading order:

```markdown
# Codex 额度调度器

简体中文 | [English](README.md)

## v0.2.0 主要更新
## 调度逻辑
### 1. 只接管当前 CPA 最高优先级层
### 2. 先判断账号是否真的可用
### 3. 对可用账号排序
### 4. 对不可用账号排序
### 排序示例
## 额度刷新与新周期激活
### 额度刷新
### 新周期激活
### CPA 暂时无法确认账号列表时
## 功能
## 安装
## CPA 配置
## 管理界面
## 隐私与数据说明
## 构建
## GitHub Release
## Management API
## 许可证
```

Translate the English content completely rather than summarizing it. Keep code blocks, filenames, configuration keys, URLs, route paths, enum values, and commands byte-for-byte equivalent to the English document. Use the approved plain Chinese labels:

```text
quota refresh -> 额度刷新
reset-window activation -> 新周期激活
plugin priority -> 插件优先级
CPA auth priority -> CPA 账号优先级
long window -> 长周期额度（周额度或月额度）
Unknown/unavailable -> 未知/不可用
```

Do not use `provisional roster` in Chinese prose. Describe it as “CPA 暂时无法确认当前账号及优先级时，使用最近一次保存的账号列表尝试新周期激活”，and include the stale-membership risk and normal-off recommendation.

- [ ] **Step 3: Verify reciprocal links and structural parity**

Run:

```powershell
$en = Get-Content -Raw -Encoding utf8 README.md
$zh = Get-Content -Raw -Encoding utf8 README.zh-CN.md
if (-not $en.Contains('[简体中文](README.zh-CN.md)')) { throw 'English language link missing' }
if (-not $zh.Contains('[English](README.md)')) { throw 'Chinese language link missing' }
$enH2 = (Select-String -Path README.md -Pattern '^## ').Count
$zhH2 = (Select-String -Path README.zh-CN.md -Pattern '^## ').Count
if ($enH2 -ne $zhH2) { throw "H2 count mismatch: en=$enH2 zh=$zhH2" }
$requiredZh = @('调度逻辑','插件优先级','预计恢复时间','五小时额度','长周期额度','CPA fallback','新周期激活')
foreach ($item in $requiredZh) {
  if (-not $zh.Contains($item)) { throw "README.zh-CN.md missing: $item" }
}
```

Expected: exit 0.

- [ ] **Step 4: Check Markdown whitespace and review the Chinese diff**

Run:

```powershell
git diff --check -- README.zh-CN.md
git diff -- README.zh-CN.md
```

Expected: no whitespace errors and no untranslated explanatory paragraphs.

- [ ] **Step 5: Commit the Chinese README**

```powershell
git add -- README.zh-CN.md
git diff --cached --check
git commit -m "docs: add Chinese README"
```

### Task 3: Cross-check documentation against v0.2.0 behavior

**Files:**
- Verify: `README.md`
- Verify: `README.zh-CN.md`
- Reference: `docs/fable-spec/refactor-decision-spec.md`
- Reference: `docs/superpowers/specs/2026-07-15-management-queue-selection-order-design.md`
- Reference: `docs/superpowers/specs/2026-07-14-optional-five-hour-window-design.md`
- Reference: `docs/superpowers/specs/2026-07-14-codex-missing-priority-compatibility-design.md`

**Interfaces:**
- Consumes: both completed README files and existing executable specifications.
- Produces: final evidence that the documentation is consistent, clean, and limited to documentation changes.

- [ ] **Step 1: Scan for forbidden historical and internal wording**

Run:

```powershell
$forbidden = rg -n 'v0\.1\.6 Priority Isolation|automatically migrates|Historical version|WaitingRoster|Capability A|Capability B|OperationProbeSequence|provisional roster' README.md README.zh-CN.md
if ($LASTEXITCODE -eq 0) { $forbidden; exit 1 }
```

Expected: no matches.

- [ ] **Step 2: Verify literal blocks remain aligned**

Run:

```powershell
$requiredLiteral = @(
  'handle_enabled: true',
  'monthly_mode: expiry_order',
  'https://chatgpt.com/backend-api/wham/usage',
  'GET  /v0/management/plugins/codex-quota-scheduler/status?format=json',
  'POST /v0/management/plugins/codex-quota-scheduler/refresh'
)
foreach ($item in $requiredLiteral) {
  foreach ($file in @('README.md','README.zh-CN.md')) {
    $text = Get-Content -Raw -Encoding utf8 $file
    if (-not $text.Contains($item)) { throw "$file missing literal: $item" }
  }
}
```

Expected: exit 0.

- [ ] **Step 3: Run repository verification**

Run:

```powershell
git diff --check
go test ./... -count=1 -timeout 300s
```

Expected: no whitespace errors and all Go packages pass.

- [ ] **Step 4: Confirm final change scope and history**

Run:

```powershell
git status --short
git log -3 --oneline
git diff origin/main...HEAD --stat
```

Expected: README work is committed; the previously existing untracked S7 plan remains untouched; no source file changed as part of the README rewrite.
