# Codex Missing-Priority Compatibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow CPA `host.auth.list` Codex entries without a serialized `priority` field to participate as authoritative priority `0`, while ignoring non-Codex entries and preserving conservative fallback for empty, malformed, failed, or no-Codex rosters.

**Architecture:** Keep the compatibility rule at `ABIHostAuthLister`, the raw CPA callback boundary. The adapter filters to Codex entries and guarantees a non-nil `RosterEntry.Priority`, defaulting omission to `0`; the existing typed capability detector and roster controller continue unchanged.

**Tech Stack:** Go 1.26, CGO Windows `-buildmode=c-shared`, CLIProxyAPI plugin ABI, standard-library `encoding/json`, Go `testing`, PowerShell build/gate scripts.

## Global Constraints

- Do not edit `docs/fable-spec/refactor-decision-spec.md`; record the approved post-freeze decision through DEV-009.
- Non-Codex `host.auth.list` entries must never enter the Codex roster.
- Missing Codex priority means exactly `0`; explicit priority values remain unchanged.
- Empty, missing, null, malformed, callback-failed, and no-Codex rosters remain Capability-B.
- Scheduler candidates remain non-authoritative.
- Preserve all untracked `.superpowers/sdd/*` workflow artifacts.

---

### Task 1: Normalize the ABI Codex roster

**Files:**
- Modify: `capability_test.go`
- Modify: `main.go:78-110`
- Verify: `docs/deviations.md`

**Interfaces:**
- Consumes: `ABIHostAuthLister.ListHostAuths(context.Context) ([]RosterEntry, error)`, `DetectHostRoster(context.Context, HostAuthLister, time.Time) HostRosterSnapshot`
- Produces: an ABI adapter result containing only Codex `RosterEntry` values whose `Priority` pointers are always non-nil

- [ ] **Step 1: Write the failing ABI regression tests**

Add these tests to `capability_test.go`:

```go
func TestSuiteCapabilityABIMissingCodexPriorityDefaultsToZero(t *testing.T) {
	lister := ABIHostAuthLister{call: func(string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"files":[{"id":"codex-default","auth_index":"idx","provider":"codex"}]}`), nil
	}}

	entries, err := lister.ListHostAuths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Priority == nil || *entries[0].Priority != 0 {
		t.Fatalf("entries = %#v, want one Codex entry with explicit priority 0", entries)
	}
	snapshot := DetectHostRoster(context.Background(), lister, time.Now())
	if snapshot.Capability != CapabilityA {
		t.Fatalf("capability = %v, want CapabilityA", snapshot.Capability)
	}
}

func TestSuiteCapabilityABIIgnoresNonCodexEntries(t *testing.T) {
	lister := ABIHostAuthLister{call: func(string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"files":[{"id":"claude","provider":"claude"},{"id":"codex","provider":" CodEx ","priority":3}]}`), nil
	}}

	entries, err := lister.ListHostAuths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "codex" || entries[0].Provider != "codex" || entries[0].Priority == nil || *entries[0].Priority != 3 {
		t.Fatalf("entries = %#v, want only explicit-priority Codex entry", entries)
	}
}

func TestSuiteCapabilityABIOnlyNonCodexRemainsFallback(t *testing.T) {
	lister := ABIHostAuthLister{call: func(string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"files":[{"id":"claude","provider":"claude"}]}`), nil
	}}

	snapshot := DetectHostRoster(context.Background(), lister, time.Now())
	if snapshot.Capability != CapabilityB || len(snapshot.Entries) != 0 {
		t.Fatalf("snapshot = %#v, want empty CapabilityB", snapshot)
	}
}
```

Also rename `TestSuiteCapabilityABINormalizationPreservesMissingPriority` to
`TestSuiteCapabilityABINormalizationPreservesExplicitAndDefaultZero` and change
its second-entry assertion to require a non-nil priority pointer with value
`0`. This existing characterization test must fail under the old adapter along
with the new regressions.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```powershell
go test . -run 'TestSuiteCapabilityABI(MissingCodexPriorityDefaultsToZero|IgnoresNonCodexEntries|OnlyNonCodexRemainsFallback)$' -count=1
```

Expected: FAIL because the current ABI adapter returns non-Codex entries and leaves omitted Codex priority nil.

- [ ] **Step 3: Implement minimal ABI normalization**

In `ABIHostAuthLister.ListHostAuths`, replace the fixed-length conversion with filtered construction:

```go
entries := make([]RosterEntry, 0, len(response.Files))
for _, file := range response.Files {
	if !strings.EqualFold(strings.TrimSpace(file.Provider), "codex") {
		continue
	}
	priority := file.Priority
	if priority == nil {
		defaultPriority := 0
		priority = &defaultPriority
	}
	entries = append(entries, RosterEntry{
		ID:        file.ID,
		AuthIndex: file.AuthIndex,
		Provider:  "codex",
		Priority:  priority,
	})
}
return entries, nil
```

Add `strings` to the `main.go` imports.

- [ ] **Step 4: Run the focused tests and verify GREEN**

Run:

```powershell
go test . -run 'TestSuiteCapabilityABI(MissingCodexPriorityDefaultsToZero|IgnoresNonCodexEntries|OnlyNonCodexRemainsFallback)$' -count=1
```

Expected: PASS.

- [ ] **Step 5: Run capability and startup lifecycle regression tests**

Run:

```powershell
go test . -run 'TestSuiteCapability|TestStartupCapabilityBRecoversThroughRosterSynchronization|TestManagementDispatchWithoutControllerIsWaitingRoster' -count=1
```

Expected: PASS. Existing typed-host tests that directly supply nil Priority remain conservative and unchanged.

- [ ] **Step 6: Run the complete verification gate**

Run:

```powershell
./scripts/check_refactor_gates.ps1 -Stage S7
go test ./... -count=1 -timeout 180s
go test -race ./... -count=1 -timeout 300s
go vet ./...
git diff --check
```

Expected: all commands exit `0`; S7 reports all exact Mock row owners passing.

- [ ] **Step 7: Commit the production fix**

```powershell
git add -- main.go capability_test.go
git commit -m "fix: default missing Codex priority to zero"
```

- [ ] **Step 8: Build and verify the Windows DLL**

Run:

```powershell
./build.ps1
Get-Item ./dist/codex-quota-scheduler.dll
Get-FileHash -Algorithm SHA256 ./dist/codex-quota-scheduler.dll
```

Expected: `build.ps1` exits `0`, tests pass, and the DLL plus generated header exist in `dist/`.

- [ ] **Step 9: Perform a read-only runtime handoff check**

Compare the built DLL hash and size with the artifact intended for CPA. Do not overwrite CPA's installed plugin or restart its service without explicit authorization.
