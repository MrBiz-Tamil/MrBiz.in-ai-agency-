#!/usr/bin/env bash
# Creates venture-os/.env from .env.example, replacing every CHANGE_ME with a generated URL-safe secret.
# Usage: scripts/gen-env.sh [--force] [--domain example.com] [--scheme https] [--founder you@example.com]
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
example="$root/.env.example"
target="$root/.env"
force=0 domain="localhost" scheme="http" founder=""

while [ $# -gt 0 ]; do
  case "$1" in
    --force) force=1 ;;
    --domain) domain="$2"; shift ;;
    --scheme) scheme="$2"; shift ;;
    --founder) founder="$2"; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

if [ -e "$target" ] && [ "$force" -ne 1 ]; then
  echo ".env already exists. Re-run with --force only if you are also deleting the Docker volumes." >&2
  exit 1
fi

hex() { openssl rand -hex "$1"; }

: > "$target"
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    LIBREDESK_ENCRYPTION_KEY=CHANGE_ME_32) echo "LIBREDESK_ENCRYPTION_KEY=$(hex 16)" ;;
    VENTURE_SECRETS_KEYS=CHANGE_ME_B64KEY) echo "VENTURE_SECRETS_KEYS=1:$(openssl rand -base64 32)" ;;
    *=CHANGE_ME) echo "${line%%=*}=$(hex 24)" ;;
    BASE_DOMAIN=*) echo "BASE_DOMAIN=$domain" ;;
    SCHEME=*) echo "SCHEME=$scheme" ;;
    ERP_SITE_NAME=*) echo "ERP_SITE_NAME=erp.$domain" ;;
    FOUNDER_EMAIL=*) if [ -n "$founder" ]; then echo "FOUNDER_EMAIL=$founder"; else echo "$line"; fi ;;
    *) echo "$line" ;;
  esac
done < "$example" >> "$target"

chmod 600 "$target"
echo "Wrote $target"
echo "FOUNDER_INITIAL_PASSWORD is in .env; Keycloak forces you to change it at first login."
echo "Add ANTHROPIC_API_KEY (and/or OPENAI_API_KEY) before starting the agent plane."
