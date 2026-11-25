#!/bin/bash
# Runs once, on first initialisation of the data directory, as the `postgres` superuser.
# Creates one role + database per application, and the two roles the Venture OS schema relies on:
#   venture_owner  owns tables, runs migrations
#   venture_app    runtime role: DML only, NOSUPERUSER, NOBYPASSRLS (so row-level security always applies)
set -euo pipefail

psql_admin() { psql -v ON_ERROR_STOP=1 --username postgres --dbname postgres "$@"; }

# Third-party applications: a login role that owns exactly one database.
create_app_db() {
  local name="$1" password="$2"
  psql_admin -v pw="$password" <<SQL
CREATE ROLE ${name} LOGIN PASSWORD :'pw' NOSUPERUSER NOCREATEROLE NOBYPASSRLS;
CREATE DATABASE ${name} OWNER ${name} ENCODING 'UTF8';
REVOKE ALL ON DATABASE ${name} FROM PUBLIC;
SQL
}

create_app_db keycloak  "${KEYCLOAK_DB_PASSWORD}"
create_app_db twenty    "${TWENTY_DB_PASSWORD}"
create_app_db libredesk "${LIBREDESK_DB_PASSWORD}"

# Twenty creates per-workspace schemas and relies on these extensions; creating them needs superuser.
psql -v ON_ERROR_STOP=1 --username postgres --dbname twenty <<'SQL'
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS pgcrypto;
SQL

# Venture OS.
psql_admin -v owner_pw="${VENTURE_OWNER_PASSWORD}" -v app_pw="${VENTURE_APP_PASSWORD}" <<'SQL'
CREATE ROLE venture_owner LOGIN PASSWORD :'owner_pw' NOSUPERUSER NOCREATEROLE NOBYPASSRLS;
CREATE ROLE venture_app   LOGIN PASSWORD :'app_pw'   NOSUPERUSER NOCREATEROLE NOBYPASSRLS;
CREATE DATABASE venture OWNER venture_owner ENCODING 'UTF8';
REVOKE ALL ON DATABASE venture FROM PUBLIC;
GRANT CONNECT ON DATABASE venture TO venture_app;
SQL

psql -v ON_ERROR_STOP=1 --username postgres --dbname venture <<'SQL'
-- Extensions need superuser; db/schema_rag.sql (run as venture_owner) only uses them.
-- The core schema needs none (gen_random_uuid() and sha256() are built in).
CREATE EXTENSION IF NOT EXISTS vector;     -- per-tenant RAG embeddings

REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT ALL   ON SCHEMA public TO venture_owner;
GRANT USAGE ON SCHEMA public TO venture_app;
-- No default privileges on purpose: db/schema.sql grants venture_app exactly what each table needs, so
-- append-only tables (ledger, audit) can never silently become writable by the application.
SQL

echo "venture-os: databases and roles created"
