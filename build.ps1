$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force dist | Out-Null
go test ./...
go build -buildmode=c-shared -o dist/codex-quota-scheduler.dll .
