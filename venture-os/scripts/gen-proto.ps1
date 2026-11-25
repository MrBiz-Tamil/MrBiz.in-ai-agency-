# Regenerates gen/ from proto/ with pinned tool versions (installed into .\bin, not your global GOBIN).
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
$env:GOBIN = Join-Path (Get-Location) "bin"
go install github.com/bufbuild/buf/cmd/buf@v1.50.0
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.5
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.18.1
$env:PATH = "$env:GOBIN;$env:PATH"
buf lint
buf generate
if (Test-Path "web
ode_modules") {
    $env:PATH = "$((Get-Location).Path)\web
ode_modules\.bin;$env:PATH"
    buf generate --template buf.gen.web.yaml
}
