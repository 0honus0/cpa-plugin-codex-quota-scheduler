# Management Boundary Hardening Design

**Goal:** Fix the CPA plugin-store security boundary issue by ensuring browser resource routes serve only UI/resource content, while all state-changing and credential-adjacent operations run behind CPA Management API authentication.

**Context:** CPA documents two plugin management surfaces: `Resources` are browser resource pages exposed under `/v0/resource/plugins/<pluginID>/...` and do not use Management API authentication; `Routes` are plugin-owned Management API endpoints under `/v0/management/...` and require the Management key. The current plugin violates this boundary by accepting `GET /v0/resource/plugins/codex-quota-scheduler/status?action=<action>&payload=<payload>` for settings, import, annotations, and refresh actions.

## Required Security Boundary

- `/v0/resource/plugins/codex-quota-scheduler/status` may serve the management UI shell and static browser resources only.
- `/v0/resource/plugins/codex-quota-scheduler/status` must not mutate plugin state, import configuration, replace annotations, trigger quota refresh, or call privileged host callbacks through query parameters.
- All state-changing operations must use registered Management API routes under `/v0/management/plugins/codex-quota-scheduler/...`.
- The UI may ask the user for a CPA Management key and hold it in memory for the current page session.
- The UI must not store the CPA Management key in `localStorage`, `sessionStorage`, rendered HTML, exported JSON, logs, or plugin state.
- Resource route tests and reviews must specifically check that `action`/`payload` query APIs do not reappear.

## Page Model

The plugin keeps the existing browser experience by serving one resource page:

```text
GET /v0/resource/plugins/codex-quota-scheduler/status
```

That page is an unauthenticated UI shell. It renders a Management key input and then calls the protected Management API endpoints with:

```text
Authorization: Bearer <management-key>
```

The page calls:

- `GET /v0/management/plugins/codex-quota-scheduler/status?format=json`
- `GET /v0/management/plugins/codex-quota-scheduler/logs`
- `GET /v0/management/plugins/codex-quota-scheduler/export`
- `PUT /v0/management/plugins/codex-quota-scheduler/settings`
- `POST /v0/management/plugins/codex-quota-scheduler/refresh`
- `POST /v0/management/plugins/codex-quota-scheduler/refresh/account`
- `POST /v0/management/plugins/codex-quota-scheduler/import`
- `PATCH /v0/management/plugins/codex-quota-scheduler/annotations/account`
- `PATCH /v0/management/plugins/codex-quota-scheduler/annotations/group`

This follows the same security shape as `cpa-plugin-codex-invite`: the browser resource page provides the UI, while sensitive work is performed through Management-key-protected endpoints.

## Quota Endpoint Restriction

The plugin sends Codex access tokens as `Authorization: Bearer <access_token>` when refreshing quota. To prevent credential exfiltration through configuration, `quota_endpoint` must be restricted to the expected ChatGPT quota endpoint:

```text
https://chatgpt.com/backend-api/wham/usage
```

This validation applies to YAML config decoding, settings updates, and imported plugin state.

## Expected User Experience

Users can continue to open the plugin page from CPA Management UI or directly at the resource URL. On first use in a browser page session, the page asks for the CPA Management key. After the key is entered, settings, refresh, import/export, logs, account notes, tags, and groups work from the same page. Refreshing or closing the page clears the in-memory key.

## Review Checklist

- Resource routes do not perform writes or trigger refresh callbacks.
- Resource routes do not read `action`, `payload`, or similar query fields to dispatch business operations.
- Management routes continue to handle writes with the correct HTTP methods.
- The UI sends writes only to `/v0/management/...` endpoints.
- The UI does not persist the Management key.
- `quota_endpoint` accepts only the ChatGPT quota usage endpoint.
- No Codex access token, refresh token, ID token, cookie, or Management key is rendered or logged.
