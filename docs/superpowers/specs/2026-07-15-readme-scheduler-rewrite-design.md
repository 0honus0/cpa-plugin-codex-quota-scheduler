# README Scheduler Rewrite Design

**Date:** 2026-07-15
**Scope:** Rewrite the public README documentation without changing plugin behavior.

## Goals

- Make the v0.2.0 scheduling model the main subject of the README.
- Keep `README.md` in English and add a complete Chinese counterpart at
  `README.zh-CN.md`.
- Explain user-visible behavior in plain language while remaining consistent
  with the current specification and production implementation.
- Remove migration-focused and historical release material that distracts from
  normal installation and use.

## Document Structure

Both README files will use the same section order and equivalent content:

1. Project summary and language switch.
2. v0.2.0 highlights.
3. How scheduling works.
4. Quota refresh and reset-window activation.
5. Features.
6. Installation and CPA configuration.
7. Management UI.
8. Privacy and data disclosure.
9. Build and release information.
10. Management API and license.

The English README remains the repository default. Each document links to the
other language near the title.

## Scheduling Explanation

The core scheduling section must describe these rules in user-facing terms:

- The plugin handles Codex accounts only and ignores other providers.
- A Codex account without an explicit CPA auth priority is treated as priority
  `0`.
- Only accounts in the highest confirmed CPA auth-priority tier are admitted.
  Users who want all Codex accounts to participate should give them the same CPA
  auth priority; `0` is the recommended simple configuration.
- Accounts are divided by real selectability before plugin priority is applied.
  Selectable accounts always appear before exhausted, blocked, or unknown
  accounts.
- Plugin-owned account priority is considered only within selectable classes.
  It cannot move an unusable account ahead of a usable account.
- Unavailable accounts ignore plugin priority and are ordered by expected
  recovery time, earliest first. Unknown recovery times are placed last.
- The short five-hour window is optional. A valid weekly or monthly long window
  is sufficient for scheduling when OpenAI omits the five-hour window.
- A missing or invalid long window remains Unknown and unavailable so CPA can
  perform fallback.
- Authoritative weekly or monthly exhaustion takes precedence over historical
  temporary-exhaustion feedback when the UI explains why an account is blocked.
- Quota-exhaustion feedback and ordinary repeated failures are distinct:
  exhaustion blocks selection until reset, while repeated non-quota failures
  use the circuit breaker.

The section should include a short ordered example instead of exposing internal
state-machine names. The example should show available accounts first, plugin
priority ordering only among those accounts, then unavailable accounts ordered
by recovery time.

## Refresh And Probe Explanation

The README will distinguish the two user-visible mechanisms:

- **Quota refresh:** reads the current quota state through the ChatGPT quota
  endpoint. It does not send a normal model request.
- **Reset-window activation:** when a reported reset time has passed but OpenAI
  has not created the new quota window, the optional feature sends one tiny
  Codex request and then verifies the quota again. It is disabled by default
  because it may consume a small amount of quota.

The high-risk setting currently known internally as provisional-roster probing
will be described as allowing reset-window activation while CPA cannot confirm
the current account list and priorities. The README must state that it uses the
last saved list, revalidates credentials, cannot guarantee that account
membership or priority is still current, and should normally remain disabled.

## Removed Or Reduced Content

- Remove the opening v0.2.0 state-file migration notice.
- Remove the entire `v0.1.6 Priority Isolation` section.
- Remove the historical version-by-version release narrative.
- Remove the exhaustive v0.2.0 asset filename list and duplicated endpoint
  explanations.
- Keep only concise configuration and release instructions needed by users or
  contributors.

## Accuracy And Consistency

- English and Chinese documents must carry the same behavioral claims.
- Configuration keys and API paths remain literal and unchanged.
- User-facing prose should avoid unexplained internal terms such as
  `WaitingRoster`, `Capability A/B`, `OperationProbeSequence`, and
  `provisional roster`.
- Descriptions must be checked against the v0.2.0 tests and the refactor decision
  specification, particularly highest-tier admission, optional five-hour quota,
  missing-long-window fallback, and management queue ordering.

## Verification

- Scan both README files for required scheduling rules and removed historical
  headings.
- Compare their headings and code/config blocks for structural parity.
- Run the existing test suite to ensure documentation-only changes did not
  accidentally alter tracked source files or release metadata.
- Review Markdown links and relative language links.
