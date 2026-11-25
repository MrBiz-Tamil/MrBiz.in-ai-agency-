-- psql entrypoint used by the compose `schema` service:
--   psql -v ON_ERROR_STOP=1 -f /schema/apply.sql      (as venture_owner, database venture)
-- Idempotent and incremental: each file records its own row in schema_version, and a file is applied only if its
-- row is missing. So `docker compose up` can re-run this on every start, and an existing database picks up a file
-- added in a later release. Each file is one transaction. Order matters: schema.sql, then schema_crm.sql, then
-- schema_controls.sql, then schema_rag.sql, then schema_tasks.sql (version 1, 2, 3, 4, 5).
\set v1 false
\set v2 false
\set v3 false
\set v4 false
\set v5 false
SELECT to_regclass('public.schema_version') IS NOT NULL AS has_version \gset
\if :has_version
  SELECT EXISTS (SELECT 1 FROM schema_version WHERE version = 1) AS v1,
         EXISTS (SELECT 1 FROM schema_version WHERE version = 2) AS v2,
         EXISTS (SELECT 1 FROM schema_version WHERE version = 3) AS v3,
         EXISTS (SELECT 1 FROM schema_version WHERE version = 4) AS v4,
         EXISTS (SELECT 1 FROM schema_version WHERE version = 5) AS v5 \gset
\endif

\if :v1
  \echo venture-os: schema.sql already applied
\else
  \echo venture-os: applying schema.sql
  \i /schema/schema.sql
\endif

\if :v2
  \echo venture-os: schema_crm.sql already applied
\else
  \echo venture-os: applying schema_crm.sql
  \i /schema/schema_crm.sql
\endif

\if :v3
  \echo venture-os: schema_controls.sql already applied
\else
  \echo venture-os: applying schema_controls.sql
  \i /schema/schema_controls.sql
\endif

\if :v4
  \echo venture-os: schema_rag.sql already applied
\else
  \echo venture-os: applying schema_rag.sql
  \i /schema/schema_rag.sql
\endif

\if :v5
  \echo venture-os: schema_tasks.sql already applied
\else
  \echo venture-os: applying schema_tasks.sql
  \i /schema/schema_tasks.sql
\endif
