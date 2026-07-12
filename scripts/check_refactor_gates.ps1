param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("S0", "S1")]
    [string]$Stage,

    [string]$RepoRoot = (Split-Path -Parent $PSScriptRoot)
)

Push-Location $RepoRoot
try {
    if ($Stage -eq "S0") {
        & go test ./... -run TestSuiteBoundary -count=1
        if ($LASTEXITCODE -ne 0) {
            exit $LASTEXITCODE
        }
    }
    elseif ($Stage -eq "S1") {
        $pickFiles = @("dispatch.go", "scheduler.go", "state.go")
        $forbidden = @(
            "http\.",
            "os\.(Open|ReadFile|WriteFile)",
            "time\.Sleep",
            "WaitGroup\.Wait",
            "ListAuths",
            "ListHostAuths",
            "MethodHostAuthList",
            "DetectHostRoster"
        )
        $violations = @()
        foreach ($file in $pickFiles) {
            $content = Get-Content -Raw $file
            foreach ($pattern in $forbidden) {
                if ($content -match $pattern) {
                    $violations += "${file}: forbidden pick-path pattern ${pattern}"
                }
            }
        }
        if ($violations.Count -gt 0) {
            $violations | ForEach-Object { Write-Error $_ }
            exit 1
        }
        Write-Output "S1 pick hot-path static gate passed ($($pickFiles -join ', '))"
    }
}
finally {
    Pop-Location
}
