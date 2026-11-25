<#
.SYNOPSIS
  Creates venture-os/.env from .env.example, replacing every CHANGE_ME with a generated URL-safe secret.
.DESCRIPTION
  Refuses to overwrite an existing .env unless -Force is given: regenerating secrets against existing
  volumes would lock you out of Postgres, Twenty and LibreDesk (their keys are stored in the volumes).
#>
[CmdletBinding()]
param(
    [switch]$Force,
    [string]$BaseDomain = "localhost",
    [string]$Scheme = "http",
    [string]$FounderEmail = ""
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$example = Join-Path $root ".env.example"
$target = Join-Path $root ".env"

if ((Test-Path $target) -and -not $Force) {
    throw ".env already exists. Re-run with -Force only if you are also deleting the Docker volumes."
}

function New-HexSecret([int]$Bytes) {
    $buf = New-Object byte[] $Bytes
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($buf) } finally { $rng.Dispose() }
    return -join ($buf | ForEach-Object { $_.ToString("x2") })
}

$lines = Get-Content -LiteralPath $example | ForEach-Object {
    if ($_ -match '^LIBREDESK_ENCRYPTION_KEY=CHANGE_ME_32$') {
        "LIBREDESK_ENCRYPTION_KEY=$(New-HexSecret 16)"        # 32 hex chars
    }
    elseif ($_ -match '^VENTURE_SECRETS_KEYS=CHANGE_ME_B64KEY$') {
        $k = New-Object byte[] 32
        $r = [System.Security.Cryptography.RandomNumberGenerator]::Create(); try { $r.GetBytes($k) } finally { $r.Dispose() }
        "VENTURE_SECRETS_KEYS=1:$([Convert]::ToBase64String($k))"   # AES-256 master key for sealed credentials
    }
    elseif ($_ -match '^([A-Z0-9_]+)=CHANGE_ME$') {
        "$($Matches[1])=$(New-HexSecret 24)"                   # 48 hex chars
    }
    elseif ($_ -match '^BASE_DOMAIN=') { "BASE_DOMAIN=$BaseDomain" }
    elseif ($_ -match '^SCHEME=') { "SCHEME=$Scheme" }
    elseif ($_ -match '^ERP_SITE_NAME=') { "ERP_SITE_NAME=erp.$BaseDomain" }
    elseif ($_ -match '^FOUNDER_EMAIL=' -and $FounderEmail) { "FOUNDER_EMAIL=$FounderEmail" }
    else { $_ }
}

# Write UTF-8 without BOM: docker compose chokes on a BOM in the first variable name.
[System.IO.File]::WriteAllLines($target, $lines, (New-Object System.Text.UTF8Encoding($false)))
Write-Host "Wrote $target"
Write-Host "VENTURE_SECRETS_KEYS seals connected accounts: back it up, losing it means reconnecting them."
Write-Host "FOUNDER_INITIAL_PASSWORD is in .env; Keycloak forces you to change it at first login."
Write-Host "Add ANTHROPIC_API_KEY (and/or OPENAI_API_KEY) before starting the agent plane."
