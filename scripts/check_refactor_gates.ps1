param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("S0")]
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
}
finally {
    Pop-Location
}
