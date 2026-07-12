param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("S0", "S1", "S2", "S2.5", "S3", "S4", "S5", "S7")]
    [string]$Stage,
    [string]$RepoRoot = (Split-Path -Parent $PSScriptRoot),
    [switch]$VerifyNegativeFixture
)

function Get-PickPathViolations {
    param([string]$Root)
    $json = & go run ./scripts/refactor_gates/analyze_pick_path.go -root $Root -entry handleSchedulerPick
    if ($LASTEXITCODE -ne 0) { throw "pick-path analyzer failed for $Root" }
    $decoded = $json | ConvertFrom-Json
    foreach ($item in $decoded) { Write-Output $item }
}

function Test-RatchetBaseline {
    param([array]$Actual, [array]$Baseline)
    $allowed = @{}
    foreach ($entry in $Baseline) {
        $allowed["$($entry.file)|$($entry.type)|$($entry.symbol)"] = [int]$entry.count
    }
    $newViolations = @()
    foreach ($entry in $Actual) {
        $key = "$($entry.file)|$($entry.type)|$($entry.symbol)"
        if (-not $allowed.ContainsKey($key) -or [int]$entry.count -gt $allowed[$key]) {
            $newViolations += "$key count=$($entry.count) baseline=$($allowed[$key])"
        }
    }
    return $newViolations
}

function Invoke-ExactGoTest {
    param([string]$TestName)
    $listed = @(& go test ./... -list "^$TestName`$")
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    $exact = @($listed | Where-Object { $_.Trim() -eq $TestName })
    if ($exact.Count -ne 1) {
        Write-Error "required exact test $TestName found $($exact.Count) times"
        exit 1
    }
    & go test ./... -run "^$TestName`$" -count=1
    exit $LASTEXITCODE
}

Push-Location $RepoRoot
try {
    if ($Stage -eq "S0") {
        & go test ./... -run TestSuiteBoundary -count=1
        exit $LASTEXITCODE
    }

    $baselinePath = "scripts/refactor_gates/s1-pick-path-baseline.json"
    $baseline = Get-Content -Raw $baselinePath | ConvertFrom-Json
    $actual = @(Get-PickPathViolations -Root ".")
    $newViolations = @(Test-RatchetBaseline -Actual $actual -Baseline $baseline)

    if ($Stage -eq "S1") {
        if ($newViolations.Count -gt 0) {
            $newViolations | ForEach-Object { Write-Error "new pick-path violation: $_" }
            exit 1
        }
        if ($VerifyNegativeFixture) {
            foreach ($fixture in @("pick_gate_transitive", "pick_gate_alias", "pick_gate_count")) {
                $fixtureActual = @(Get-PickPathViolations -Root "scripts/testdata/$fixture")
                $fixtureViolations = @(Test-RatchetBaseline -Actual $fixtureActual -Baseline $baseline)
                if ($fixtureViolations.Count -eq 0) {
                    Write-Error "negative fixture $fixture was not rejected"
                    exit 1
                }
                Write-Output "S1 negative fixture $fixture rejected as expected"
            }
        }
        Write-Output "S1 ABI pick closure ratchet passed; actual=$($actual.Count) baseline=$($baseline.Count)"
        exit 0
    }

    if ($Stage -eq "S2") {
        if ($newViolations.Count -gt 0) {
            $newViolations | ForEach-Object { Write-Error "new pick-path violation: $_" }
            exit 1
        }
        & go test ./... -run 'Test(SuiteIdentityPersist|MockGroupAIdentityPersist|MockGroupDIdentitySecurity|MockGroupDBoundary|S2KPointRegistryMatchesSource)$' -count=1
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
        Write-Output "S2 identity/persistence, boundary, sensitive-state, and K-point gates passed"
        exit 0
    }

    if ($Stage -eq "S2.5") {
        if ($newViolations.Count -gt 0) { $newViolations | ForEach-Object { Write-Error "new pick-path violation: $_" }; exit 1 }
        & go test ./... -run 'Test(SuiteRuntimeWiring|S2KPointRegistryMatchesSource)$' -count=1
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
        Write-Output "S2.5 runtime wiring, semantic migration, genesis, probe seam, and K-point gates passed"
        exit 0
    }

    if ($Stage -eq "S3") {
        if ($newViolations.Count -gt 0) {
            $newViolations | ForEach-Object { Write-Error "new pick-path violation: $_" }
            exit 1
        }
        & go test ./... -run 'Test(SuiteCoordinator|MockGroupACoordinatorMigration|MockGroupECoordinatorInterleavings)$' -count=1
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
        Write-Output "S3 coordinator, complete legacy envelope, fencing, and drain gates passed"
        exit 0
    }

    if ($Stage -eq "S4") {
        if ($newViolations.Count -gt 0) {
            $newViolations | ForEach-Object { Write-Error "new pick-path violation: $_" }
            exit 1
        }
        & go test ./... -run 'Test(SuiteRefresh|MockGroupERefresh|RefreshSourcePriorityTruthTable|NormalRefreshDeadlineHasSingleControllerOwner|RefreshActiveWindowDeadlineIsExclusive|AuxiliaryDeadlineAtActiveCutoffIsNotOwnedByLegacyLoop|ExactCutoffDoesNotAssignLegacyRetryResetOrProbeOwnership)$' -count=1
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
        Write-Output "S4 Dormant/Active normal refresh, unique source priority, and failure-isolation gates passed"
        exit 0
    }

    if ($Stage -eq "S5") {
        if ($baseline.Count -ne 0 -or $actual.Count -ne 0) {
            Write-Error "S5 requires empty baseline and zero ABI pick-closure violations"
            exit 1
        }
        Invoke-ExactGoTest -TestName "TestSchedulerPickABIPathSnapshotOnly"
    }

    if ($Stage -eq "S7") {
        Invoke-ExactGoTest -TestName "TestStartupCapabilityBRecoversThroughRosterSynchronization"
    }
}
finally {
    Pop-Location
}
