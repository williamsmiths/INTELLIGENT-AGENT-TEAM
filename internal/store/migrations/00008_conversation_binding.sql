-- +goose Up
-- Spec S2 E.9: the Owner binds an OpenBot conversation (a channel, or an
-- agent's own thread) to a project, so a verb called without a lease knows its
-- project (PM-S2-23). A conversation belongs to at most one project: the
-- primary key makes a second PUT overwrite the first.
CREATE TABLE conversation_binding (
  kind       text NOT NULL CHECK (kind IN ('channel','thread')),
  openbot_id text NOT NULL CHECK (length(openbot_id) BETWEEN 1 AND 200),
  project_id uuid NOT NULL REFERENCES project(id),
  bound_by   uuid REFERENCES principal(id),
  bound_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (kind, openbot_id)
);
CREATE INDEX conversation_binding_project_idx ON conversation_binding (project_id);

-- Only the Owner writes; the runtime reads it under its own scope; agents see
-- nothing (E.9-6). Unbinding is a real DELETE (E.9-1): the binding is
-- configuration, the journal keeps CONVERSATION_BOUND / CONVERSATION_UNBOUND.
GRANT SELECT, INSERT, UPDATE, DELETE ON conversation_binding TO harness_app;
ALTER TABLE conversation_binding ENABLE ROW LEVEL SECURITY;
CREATE POLICY conversation_binding_read  ON conversation_binding FOR SELECT USING (kch_trusted());
CREATE POLICY conversation_binding_write ON conversation_binding FOR INSERT WITH CHECK (kch_role() = 'owner');
CREATE POLICY conversation_binding_edit  ON conversation_binding FOR UPDATE USING (kch_role() = 'owner') WITH CHECK (kch_role() = 'owner');
CREATE POLICY conversation_binding_drop  ON conversation_binding FOR DELETE USING (kch_role() = 'owner');

-- +goose Down
DROP TABLE conversation_binding;
