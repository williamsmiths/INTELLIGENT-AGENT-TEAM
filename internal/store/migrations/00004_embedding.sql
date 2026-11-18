-- +goose Up
-- Spec S2 part B (semantic retrieval), table of C.11. One embedding per
-- (knowledge version, model). Knowledge content never changes in place, so a
-- row is written once per content_hash and model (AC-S2-020).
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE knowledge_embedding (
  knowledge_id uuid NOT NULL REFERENCES knowledge(id),
  model        text NOT NULL CHECK (model <> ''),
  dim          int  NOT NULL CHECK (dim > 0),
  -- knowledge.content_hash at the time of embedding; a different value means "embed again".
  content_hash text NOT NULL,
  -- No fixed dimension: another model may be evaluated side by side (evals/retrieval).
  embedding    vector NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (knowledge_id, model),
  CHECK (vector_dims(embedding) = dim)
);

-- The model chosen by evals/retrieval (BAAI/bge-m3, 1024 dimensions, cosine).
-- A query must use the same cast expression and the same model literal to use it.
CREATE INDEX knowledge_embedding_bge_m3_idx ON knowledge_embedding
  USING hnsw ((embedding::vector(1024)) vector_cosine_ops) WHERE model = 'BAAI/bge-m3';

-- Only harnessd's embed worker writes; rows are replaced, never deleted (no DELETE grant).
GRANT SELECT, INSERT ON knowledge_embedding TO harness_app;
GRANT UPDATE (dim, content_hash, embedding, created_at) ON knowledge_embedding TO harness_app;

ALTER TABLE knowledge_embedding ENABLE ROW LEVEL SECURITY;
-- An embedding is visible exactly when its knowledge row is: the sub-select runs
-- under the caller's own row-level security on knowledge (project, level, trust),
-- so a vector query cannot return another project's row even without a project
-- predicate (AC-S2-022).
CREATE POLICY embedding_read ON knowledge_embedding FOR SELECT
  USING (EXISTS (SELECT 1 FROM knowledge k WHERE k.id = knowledge_id));
CREATE POLICY embedding_write ON knowledge_embedding FOR INSERT WITH CHECK (kch_role() = 'runtime');
CREATE POLICY embedding_edit ON knowledge_embedding FOR UPDATE
  USING (kch_role() = 'runtime') WITH CHECK (kch_role() = 'runtime');

-- +goose Down
DROP TABLE knowledge_embedding;
-- The extension stays: dropping it would fail if anything else uses the type.
