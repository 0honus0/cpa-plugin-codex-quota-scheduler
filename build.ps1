$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force dist | Out-Null
go test ./...
$cgoEnabled = (go env CGO_ENABLED).Trim()
if ($cgoEnabled -ne "1") {
    throw "CGO_ENABLED=1 is required to build the CPA dynamic plugin DLL (-buildmode=c-shared). Install a C toolchain and rerun with CGO_ENABLED=1."
}
go build -buildmode=c-shared -o dist/codex-quota-scheduler.dll .
