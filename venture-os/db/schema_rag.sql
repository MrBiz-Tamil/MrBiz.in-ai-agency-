-- Per-tenant RAG store. Needs the pgvector extension (created by infra/postgres/init/01-bootstrap.sh, which runs
-- as superuser). Kept apart from schema.sql so the core schema has no extension dependency.
--
-- Embedding dimension is fixed at 1536 (OpenAI text-embedding-3-small / compatible). Changing models means a new
-- column or table, not an ALTER of live vectors.
BEGIN;

CREATE TABLE knowledge_chunks (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES organizations(id),
  source_type  text NOT NULL CHECK (source_type IN ('KB_DOCUMENT','SOP','PRODUCT','POLICY','CONVERSATION')),
  source_id    text NOT NULL,
  chunk_index  integer NOT NULL CHECK (chunk_index >= 0),
  content      text NOT NULL,
  embedding    vector(1536) NOT NULL,
  metadata     jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(metadata) = 'object'),
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, source_type, source_id, chunk_index)
);
-- Queries always filter on tenant_id (RLS adds it), so the planner can combine this with the tenant btree.
CREATE INDEX knowledge_chunks_embedding_idx ON knowledge_chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX knowledge_chunks_tenant_idx ON knowledge_chunks (tenant_id, source_type, source_id);

ALTER TABLE knowledge_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_chunks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON knowledge_chunks FOR SELECT USING (tenant_id = ANY (app_tenant_ids()));
CREATE POLICY tenant_insert ON knowledge_chunks FOR INSERT WITH CHECK (tenant_id = app_tenant_id());
CREATE POLICY tenant_update ON knowledge_chunks FOR UPDATE
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());
CREATE POLICY tenant_delete ON knowledge_chunks FOR DELETE USING (tenant_id = app_tenant_id());
CREATE TRIGGER knowledge_chunks_tenant_immutable BEFORE UPDATE ON knowledge_chunks
  FOR EACH ROW EXECUTE FUNCTION forbid_tenant_change();

GRANT SELECT, INSERT, UPDATE, DELETE ON knowledge_chunks TO venture_app;

INSERT INTO schema_version (version) VALUES (4);
COMMIT;
