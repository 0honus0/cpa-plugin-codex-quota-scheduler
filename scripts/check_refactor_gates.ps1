param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("S0", "S1", "S5", "S7")]
    [string]$Stage,

    [string]$RepoRoot = (Split-Path -Parent $PSScriptRoot),

    [switch]$VerifyNegativeFixture
)

function Get-PickPathViolations {
    param([string[]]$Files)

    $detectors = @(
        @{ Type = "network"; Regex = "\bhttp\.(?:Method[A-Za-z]+|Header|Status[A-Za-z]+)\b" },
        @{ Type = "disk"; Regex = "\bos\.(?:Open|ReadFile|WriteFile)\b" },
        @{ Type = "sleep"; Regex = "\btime\.Sleep\b" },
        @{ Type = "blocking_wait"; Regex = "\b[A-Za-z_][A-Za-z0-9_.]*\.Wait\(\)" },
        @{ Type = "host_roster"; Regex = "\b(?:[A-Za-z_][A-Za-z0-9_.]*\.)?ListAuths\b|\bListHostAuths\b|\bMethodHostAuthList\b|\bDetectHostRoster\b" }
    )
    $grouped = @{}
    foreach ($file in $Files) {
        $relative = $file.Replace('\', '/')
        foreach ($line in Get-Content $file) {
            foreach ($detector in $detectors) {
                foreach ($match in [regex]::Matches($line, $detector.Regex)) {
                    $symbol = $match.Value -replace '\(\)$', ''
                    $key = "$relative|$($detector.Type)|$symbol"
                    if (-not $grouped.ContainsKey($key)) {
                        $grouped[$key] = [ordered]@{ file = $relative; type = $detector.Type; symbol = $symbol; count = 0 }
                    }
                    $grouped[$key].count++
                }
            }
        }
    }
    return @($grouped.Values)
}

function Test-RatchetBaseline {
    param(
        [array]$Actual,
        [array]$Baseline
    )

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

Push-Location $RepoRoot
try {
    if ($Stage -eq "S0") {
        & go test ./... -run TestSuiteBoundary -count=1
        exit $LASTEXITCODE
    }

    $pickFiles = @("dispatch.go", "scheduler.go", "state.go", "refresh.go")
    $baselinePath = "scripts/refactor_gates/s1-pick-path-baseline.json"
    $baseline = Get-Content -Raw $baselinePath | ConvertFrom-Json
    $actual = @(Get-PickPathViolations -Files $pickFiles)
    $newViolations = @(Test-RatchetBaseline -Actual $actual -Baseline $baseline)

    if ($Stage -eq "S1") {
        if ($newViolations.Count -gt 0) {
            $newViolations | ForEach-Object { Write-Error "new pick-path violation: $_" }
            exit 1
        }
        if ($VerifyNegativeFixture) {
            $fixtureActual = @(Get-PickPathViolations -Files @("scripts/testdata/s1-new-pick-violation.go"))
            $fixtureViolations = @(Test-RatchetBaseline -Actual $fixtureActual -Baseline $baseline)
            if ($fixtureViolations.Count -eq 0) {
                Write-Error "negative fixture was not rejected"
                exit 1
            }
            Write-Output "S1 negative ratchet fixture rejected as expected"
        }
        Write-Output "S1 pick hot-path ratchet passed ($($pickFiles -join ', ')); baseline entries=$($baseline.Count)"
        exit 0
    }

    if ($Stage -eq "S5") {
        if ($baseline.Count -ne 0) {
            Write-Error "S5 requires an empty pick-path ratchet baseline; found $($baseline.Count) entries"
            exit 1
        }
        if ($actual.Count -ne 0) {
            Write-Error "S5 INV-43 requires zero pick-path violations; found $($actual.Count)"
            exit 1
        }
        if (-not (Select-String -Path "*_test.go" -Pattern "TestSchedulerPickABIPathSnapshotOnly" -Quiet)) {
            Write-Error "S5 requires TestSchedulerPickABIPathSnapshotOnly"
            exit 1
        }
        & go test ./... -run TestSchedulerPickABIPathSnapshotOnly -count=1
        exit $LASTEXITCODE
    }

    if ($Stage -eq "S7") {
        if (-not (Select-String -Path "*_test.go" -Pattern "TestStartupCapabilityBRecoversThroughRosterSynchronization" -Quiet)) {
            Write-Error "S7 requires TestStartupCapabilityBRecoversThroughRosterSynchronization"
            exit 1
        }
        & go test ./... -run TestStartupCapabilityBRecoversThroughRosterSynchronization -count=1
        exit $LASTEXITCODE
    }
}
finally {
    Pop-Location
}
