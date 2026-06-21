$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force dist | Out-Null

$env:CGO_ENABLED = "1"
$cc = (go env CC).Trim()
if (-not $cc) {
    throw "A C compiler is required to build the CPA dynamic plugin DLL (-buildmode=c-shared), but go env CC is empty. Install a C toolchain such as MinGW-w64 and ensure it is on PATH."
}

$ccCommand = ($cc -split "\s+")[0]
if (-not (Get-Command $ccCommand -ErrorAction SilentlyContinue)) {
    throw "A C compiler is required to build the CPA dynamic plugin DLL (-buildmode=c-shared), but '$ccCommand' was not found on PATH. Install a C toolchain such as MinGW-w64 and ensure it is on PATH."
}

go test ./...
go build -buildmode=c-shared -o dist/codex-quota-scheduler.dll .
