# Management Boundary Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move all state-changing plugin UI actions behind CPA Management API authentication and remove the unauthenticated resource query action API.

**Architecture:** Keep the resource route as a browser UI shell. Use existing Management API routes for data, settings, refresh, import/export, and annotation operations. Add endpoint validation so Codex bearer tokens are sent only to the expected ChatGPT quota endpoint.

**Tech Stack:** Go, CPA plugin SDK `management.*`, `go test ./...`.

---

### Task 1: Resource Route Boundary Tests

**Files:**
- Modify: `management_test.go`

- [x] Add a failing test that calls `GET /v0/resource/plugins/codex-quota-scheduler/status?action=settings&payload=...` and asserts the request does not update config.
- [x] Add the same resource-route rejection coverage for `refresh`, `refresh_account`, `import`, `annotations_replace`, `annotations_account`, and `annotations_group`.
- [x] Run `go test ./...` and confirm the new tests fail because the current resource route still dispatches actions.

### Task 2: Management UI Request Tests

**Files:**
- Modify: `management_test.go`

- [x] Update HTML tests so the rendered page contains a Management key field and `/v0/management/plugins/codex-quota-scheduler` API paths.
- [x] Update HTML tests so the page no longer contains `requestPlugin(action,options)` or resource `?action=` API markers.
- [x] Run `go test ./...` and confirm the tests fail before implementation.

### Task 3: Quota Endpoint Validation Tests

**Files:**
- Modify: `config_test.go`
- Modify: `management_test.go`

- [x] Add a config test rejecting `quota_endpoint: https://example.test/usage`.
- [x] Add a settings/import test rejecting non-ChatGPT quota endpoints.
- [x] Run `go test ./...` and confirm the tests fail before implementation.

### Task 4: Implement Resource Boundary

**Files:**
- Modify: `management.go`

- [x] Remove resource action dispatch from `HandleManagementRequest`.
- [x] Ensure `/v0/resource/plugins/codex-quota-scheduler/status` only renders HTML.
- [x] Keep Management API routes unchanged for protected operations.
- [x] Run `go test ./...` and confirm resource boundary tests pass or expose the next failing behavior.

### Task 5: Implement Management-Key UI Calls

**Files:**
- Modify: `management.go`

- [x] Add a Management key input to the resource UI shell.
- [x] Replace `requestPlugin(action, options)` with helpers that call `/v0/management/plugins/codex-quota-scheduler/...` using `Authorization: Bearer <key>`.
- [x] Keep the key only in an in-memory JavaScript variable or password input value.
- [x] Reload status data from the Management status JSON endpoint after authenticated load.
- [x] Run `go test ./...`.

### Task 6: Implement Quota Endpoint Restriction

**Files:**
- Modify: `config.go`
- Modify: `management.go`

- [x] Add validation that accepts only `https://chatgpt.com/backend-api/wham/usage`.
- [x] Apply validation in YAML decode, settings save, and import.
- [x] Run `go test ./...`.

### Task 7: Documentation Update

**Files:**
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-06-21-codex-quota-scheduler-design.md`

- [x] Replace the old resource `action` API documentation with the Management-key-protected API model.
- [x] Add the review checklist from the design spec to the original project spec.
- [x] Run `go test ./...`.

### Task 8: Final Verification

**Files:**
- All changed files

- [x] Run `go test ./...`.
- [x] Run `git diff --check`.
- [x] Review the diff for any resource route state changes, key persistence, or non-ChatGPT quota endpoint acceptance.
