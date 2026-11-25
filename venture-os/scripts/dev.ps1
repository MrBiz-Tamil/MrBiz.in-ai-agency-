<#
.SYNOPSIS
  Runs the whole Venture OS Command Center locally with no Docker, no Keycloak and no API key.
.DESCRIPTION
  Starts three processes and stops all of them when you press Ctrl+C:
    1. an in-process PostgreSQL (PGlite from tools/pgtest)             127.0.0.1:25499
    2. the control plane on that database, with fake external systems   127.0.0.1:8080
    3. the SvelteKit Command Center with authentication bypassed         http://127.0.0.1:5190
  Provisioning is SIMULATED and sign-in is skipped. This is for development and demos only.
.PARAMETER NoSeed
  Start with an empty database instead of two demo businesses with pending approvals.
.PARAMETER AnthropicKey
  Use the real Venture Architect (Anthropic) instead of the scripted demo interviewer.
#>
[CmdletBinding()]
param(
    [switch]$NoSeed,
    [string]$AnthropicKey = $env:ANTHROPIC_API_KEY,
    [int]$DbPort = 25499
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$pgtest = Join-Path $root "tools\pgtest"
$web = Join-Path $root "web"

foreach ($tool in "node", "npm", "go") {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "$tool is required but was not found on PATH." }
}
if (-not (Test-Path (Join-Path $pgtest "node_modules"))) {
    Write-Host "Installing the PGlite dev database (one time)..."
    Push-Location $pgtest; npm install --no-audit --no-fund; Pop-Location
}
if (-not (Test-Path (Join-Path $web "node_modules"))) {
    Write-Host "Installing web dependencies (one time)..."
    Push-Location $web; npm install --no-audit --no-fund; Pop-Location
}

$procs = @()
try {
    $procs += Start-Process node -ArgumentList "server.mjs", $DbPort -WorkingDirectory $pgtest -PassThru -NoNewWindow
    Start-Sleep -Seconds 2

    $env:DEVSTACK_DATABASE_URL = "postgres://postgres@127.0.0.1:$DbPort/postgres?sslmode=disable&pool_max_conns=1&default_query_exec_mode=describe_exec"
    $env:DEVSTACK_SEED = if ($NoSeed) { "" } else { "1" }
    if ($AnthropicKey) { $env:ANTHROPIC_API_KEY = $AnthropicKey }
    $procs += Start-Process go -ArgumentList "run", "./cmd/devstack" -WorkingDirectory $root -PassThru -NoNewWindow
    Start-Sleep -Seconds 6

    $env:AUTH_MODE = "dev"
    $env:CONTROLPLANE_URL = "http://127.0.0.1:8080"
    $env:ORIGIN = "http://127.0.0.1:5190"
    $env:SESSION_SECRET = "dev-only-session-secret-0123456789abcdef"
    $env:VENTURE_CONNECTORS = "fake"
    $env:VENTURE_CONNECTOR_CRM = "real"   # the devstack runs the real built-in CRM
    Write-Host ""
    Write-Host "Command Center: http://127.0.0.1:5190   (Ctrl+C to stop everything)" -ForegroundColor Green
    Push-Location $web
    npm run dev
    Pop-Location
}
finally {
    foreach ($p in $procs) {
        if ($p -and -not $p.HasExited) { & taskkill /PID $p.Id /T /F 2>$null | Out-Null }
    }
}
