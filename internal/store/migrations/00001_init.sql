-- +goose Up
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;

-- +goose StatementBegin
CREATE FUNCTION kch_unaccent(text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
AS $$ SELECT public.unaccent('public.unaccent'::regdictionary, $1) $$;
-- +goose StatementEnd

-- Identity -----------------------------------------------------------------
CREATE TABLE project (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug       text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
  name       text NOT NULL,
  repo_path  text NOT NULL DEFAULT '',
  -- WIP limit so one project cannot take every agent (V5L multi-project §6). NULL = no limit.
  max_active_leases int CHECK (max_active_leases >= 0),
  -- Copied into each new task budget; a task cannot become RUNNABLE without a cost limit (T-H-01).
  default_task_max_cost_microusd bigint CHECK (default_task_max_cost_microusd >= 0),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE principal (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind             text NOT NULL CHECK (kind IN ('owner','agent')),
  name             text NOT NULL,
  openbot_agent_id text UNIQUE,
  token_hash       bytea NOT NULL UNIQUE,
  -- HOOK: provider has blocking hooks (Claude, Full access). SUPERVISED: observe only (limitation L5).
  enforcement      text NOT NULL DEFAULT 'SUPERVISED' CHECK (enforcement IN ('HOOK','SUPERVISED')),
  created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE project_member (
  project_id   uuid NOT NULL REFERENCES project(id),
  principal_id uuid NOT NULL REFERENCES principal(id),
  role         text NOT NULL CHECK (role IN ('lead','worker')),
  PRIMARY KEY (project_id, principal_id)
);

-- A repo root belongs to exactly one project (spec §0.4). Roots are realpaths and never nested (checked by the service).
CREATE TABLE project_repo (
  project_id uuid NOT NULL REFERENCES project(id),
  root       text NOT NULL UNIQUE CHECK (root LIKE '/%'),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, root)
);

-- Memory & Context ---------------------------------------------------------
CREATE TABLE source (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id   uuid NOT NULL REFERENCES project(id),
  kind         text NOT NULL,
  origin       text NOT NULL CHECK (origin IN ('OWNER','AGENT','RUNTIME')),
  uri          text NOT NULL DEFAULT '',
  content      text NOT NULL,
  content_hash text NOT NULL,
  created_by   uuid NOT NULL REFERENCES principal(id),
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX source_project_idx ON source (project_id, created_at);

CREATE TABLE knowledge (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- One memory system, three levels: GLOBAL (no project), PROJECT (shared in a project), AGENT (one agent in one project).
  level      text NOT NULL DEFAULT 'PROJECT' CHECK (level IN ('GLOBAL','PROJECT','AGENT')),
  project_id uuid REFERENCES project(id),
  key        text NOT NULL CHECK (key <> ''),
  -- Key namespace: the author for AGENT items and unconfirmed GLOBAL items, NULL otherwise (AC-H-107).
  ns         uuid REFERENCES principal(id),
  version    int  NOT NULL CHECK (version >= 1),
  kind       text NOT NULL CHECK (kind IN ('goal','requirement','rule','ui','flow','api','data','architecture','dependency','decision','experience')),
  aspect     text NOT NULL CHECK (aspect IN ('AS_IS','TO_BE')),
  trust      text NOT NULL CHECK (trust IN ('AUTHORITATIVE','OBSERVED','CANDIDATE','HISTORICAL')),
  basis      text CHECK (basis IN ('proposed','inferred','unknown')),
  scope      text NOT NULL DEFAULT '',
  title      text NOT NULL,
  body       text NOT NULL DEFAULT '',
  -- Required for GLOBAL experience: when it applies and where it does not.
  applies_when text NOT NULL DEFAULT '',
  limits       text NOT NULL DEFAULT '',
  source_ids uuid[] NOT NULL DEFAULT '{}',
  is_current boolean NOT NULL DEFAULT true,
  -- sha256 of level + normalized title + body: writing the same knowledge again returns the old row.
  content_hash text NOT NULL,
  ended_reason text NOT NULL DEFAULT '' CHECK (ended_reason IN ('','superseded','invalidated','merged')),
  replaced_by  uuid REFERENCES knowledge(id),
  ended_note   text NOT NULL DEFAULT '', -- free-text reason given when the item was invalidated
  created_by uuid NOT NULL REFERENCES principal(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  search     tsvector GENERATED ALWAYS AS (
               to_tsvector('simple', kch_unaccent(key || ' ' || title || ' ' || body))
             ) STORED,
  UNIQUE NULLS NOT DISTINCT (project_id, level, ns, key, version),
  CHECK ((level = 'GLOBAL') = (project_id IS NULL)),
  -- Business knowledge never becomes global; only generalized experience does.
  CHECK (level <> 'GLOBAL' OR (kind = 'experience' AND applies_when <> '' AND limits <> '' AND cardinality(source_ids) = 0)),
  CHECK (level <> 'AGENT' OR ns IS NOT NULL),
  CHECK (level <> 'PROJECT' OR ns IS NULL),
  CHECK ((trust = 'HISTORICAL') = (NOT is_current)),
  CHECK (trust <> 'OBSERVED' OR (aspect = 'AS_IS' AND cardinality(source_ids) > 0))
);
-- Exactly one current version per key (AC-H-007).
CREATE UNIQUE INDEX knowledge_current_uq ON knowledge (project_id, level, ns, key) NULLS NOT DISTINCT WHERE is_current;
CREATE INDEX knowledge_search_idx ON knowledge USING gin (search);
CREATE INDEX knowledge_trgm_idx ON knowledge USING gin (kch_unaccent(title || ' ' || body) gin_trgm_ops);
CREATE INDEX knowledge_scope_idx ON knowledge (project_id, scope) WHERE is_current;
-- Selective remembering: the same author cannot store the same content twice (AC-H-101).
CREATE UNIQUE INDEX knowledge_hash_uq ON knowledge (project_id, level, created_by, content_hash) NULLS NOT DISTINCT WHERE is_current;

-- Knowledge graph as SQL relations. A relation is closed (valid_to), never deleted.
CREATE TABLE relation (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES project(id),
  from_key   text NOT NULL,
  to_key     text NOT NULL,
  type       text NOT NULL CHECK (type IN ('implements','depends_on','verifies','impacts','consumes','owns')),
  created_by uuid NOT NULL REFERENCES principal(id),
  valid_from timestamptz NOT NULL DEFAULT now(),
  valid_to   timestamptz,
  CHECK (from_key <> to_key)
);
CREATE UNIQUE INDEX relation_live_uq ON relation (project_id, from_key, to_key, type) WHERE valid_to IS NULL;
CREATE INDEX relation_to_idx ON relation (project_id, to_key) WHERE valid_to IS NULL;

-- Adaptive Runtime ---------------------------------------------------------
CREATE TABLE task (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id          uuid NOT NULL REFERENCES project(id),
  key                 text NOT NULL CHECK (key <> ''),
  title               text NOT NULL,
  desired_outcome     text NOT NULL DEFAULT '',
  scope               text NOT NULL DEFAULT '',
  state               text NOT NULL DEFAULT 'PROPOSED'
                        CHECK (state IN ('PROPOSED','RUNNABLE','RUNNING','VERIFIED','WAITING_DECISION','SUPERSEDED')),
  revision            int  NOT NULL DEFAULT 1,
  repo_path           text NOT NULL DEFAULT '',
  allowed_paths       text[] NOT NULL DEFAULT '{}',
  protected_paths     text[] NOT NULL DEFAULT '{}',
  required_checks     jsonb NOT NULL DEFAULT '[]',
  requirement_keys    text[] NOT NULL DEFAULT '{}',
  dependency_versions jsonb NOT NULL DEFAULT '{}',
  verified_candidate  text,
  -- HEAD of the repo when the first lease was granted; the scope check diffs against it.
  base_commit         text NOT NULL DEFAULT '',
  created_by          uuid NOT NULL REFERENCES principal(id),
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, key),
  UNIQUE (id, project_id)
);
CREATE INDEX task_open_repo_idx ON task (repo_path) WHERE state <> 'SUPERSEDED';

CREATE TABLE lease (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id     uuid NOT NULL REFERENCES project(id),
  task_id        uuid NOT NULL,
  principal_id   uuid NOT NULL REFERENCES principal(id),
  epoch          int  NOT NULL,
  task_revision  int  NOT NULL,
  repo_path      text NOT NULL DEFAULT '',
  -- Eval arm of this assignment: OBSERVE = A, CONTEXT = B, FULL = C.
  mode           text NOT NULL DEFAULT 'FULL' CHECK (mode IN ('OBSERVE','CONTEXT','FULL')),
  state          text NOT NULL DEFAULT 'ACTIVE' CHECK (state IN ('ACTIVE','RELEASED','REVOKED','EXPIRED')),
  context_acked  boolean NOT NULL DEFAULT false,
  pending_notice text NOT NULL DEFAULT '',
  wrote_files    boolean NOT NULL DEFAULT false,
  reason         text NOT NULL DEFAULT '',
  granted_at     timestamptz NOT NULL DEFAULT now(),
  expires_at     timestamptz NOT NULL,
  ended_at       timestamptz,
  UNIQUE (task_id, epoch),
  -- A lease can only point at a task of its own project.
  FOREIGN KEY (task_id, project_id) REFERENCES task(id, project_id)
);
-- One responsible agent per task, one task per agent (T-H-02, AC-H-013, AC-H-014).
CREATE UNIQUE INDEX lease_active_task_uq ON lease (task_id) WHERE state = 'ACTIVE';
CREATE UNIQUE INDEX lease_active_principal_uq ON lease (principal_id) WHERE state = 'ACTIVE';
-- One working tree, one writer (AC-H-018). Parallel work uses separate git worktrees.
CREATE UNIQUE INDEX lease_active_repo_uq ON lease (repo_path) WHERE state = 'ACTIVE' AND repo_path <> '';
CREATE INDEX lease_principal_time_idx ON lease (principal_id, granted_at);
CREATE INDEX lease_project_active_idx ON lease (project_id) WHERE state = 'ACTIVE';

-- Three levels: SYSTEM (whole harness), PROJECT, TASK. A ledger row adds to the task,
-- its parent tasks, its project and the system in one transaction.
CREATE TABLE budget (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  level              text NOT NULL CHECK (level IN ('SYSTEM','PROJECT','TASK')),
  project_id         uuid REFERENCES project(id),
  task_id            uuid REFERENCES task(id),
  max_cost_microusd  bigint CHECK (max_cost_microusd >= 0),
  max_input_tokens   bigint CHECK (max_input_tokens >= 0),
  max_output_tokens  bigint CHECK (max_output_tokens >= 0),
  max_tool_calls     bigint CHECK (max_tool_calls >= 0),
  max_turns          bigint CHECK (max_turns >= 0),
  used_cost_microusd bigint NOT NULL DEFAULT 0,
  used_input_tokens  bigint NOT NULL DEFAULT 0,
  used_output_tokens bigint NOT NULL DEFAULT 0,
  used_tool_calls    bigint NOT NULL DEFAULT 0,
  used_turns         bigint NOT NULL DEFAULT 0,
  state              text NOT NULL DEFAULT 'OK' CHECK (state IN ('OK','WARN','EXHAUSTED')),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CHECK ((level = 'SYSTEM'  AND project_id IS NULL     AND task_id IS NULL)
      OR (level = 'PROJECT' AND project_id IS NOT NULL AND task_id IS NULL)
      OR (level = 'TASK'    AND project_id IS NOT NULL AND task_id IS NOT NULL)),
  FOREIGN KEY (task_id, project_id) REFERENCES task(id, project_id)
);
CREATE UNIQUE INDEX budget_task_uq    ON budget (task_id)    WHERE level = 'TASK';
CREATE UNIQUE INDEX budget_project_uq ON budget (project_id) WHERE level = 'PROJECT';
CREATE UNIQUE INDEX budget_system_uq  ON budget ((true))     WHERE level = 'SYSTEM';
INSERT INTO budget (level) VALUES ('SYSTEM');

-- Derived from OpenBot's raw metering and from hook tool events. Idempotent by (source, external_id).
CREATE TABLE budget_ledger (
  id               bigserial PRIMARY KEY,
  source           text NOT NULL CHECK (source IN ('openbot_usage','openbot_turn','hook_tool')),
  external_id      text NOT NULL,
  project_id       uuid REFERENCES project(id),
  task_id          uuid REFERENCES task(id),
  principal_id     uuid REFERENCES principal(id),
  openbot_agent_id text NOT NULL DEFAULT '',
  provider         text NOT NULL DEFAULT '',
  model            text NOT NULL DEFAULT '',
  cost_microusd    bigint NOT NULL DEFAULT 0,
  input_tokens     bigint NOT NULL DEFAULT 0,
  output_tokens    bigint NOT NULL DEFAULT 0,
  tool_calls       bigint NOT NULL DEFAULT 0,
  turns            bigint NOT NULL DEFAULT 0,
  occurred_at      timestamptz NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (source, external_id),
  FOREIGN KEY (task_id, project_id) REFERENCES task(id, project_id)
);
CREATE INDEX budget_ledger_task_idx ON budget_ledger (task_id, occurred_at);

-- Intelligence & Collaboration --------------------------------------------
CREATE TABLE blackboard (
  id         bigserial PRIMARY KEY,
  project_id uuid NOT NULL REFERENCES project(id),
  task_id    uuid REFERENCES task(id),
  kind       text NOT NULL CHECK (kind IN ('finding','evidence','proposal','conflict','decision','impact','next_action')),
  body       text NOT NULL,
  refs       jsonb NOT NULL DEFAULT '[]',
  author     uuid NOT NULL REFERENCES principal(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (task_id, project_id) REFERENCES task(id, project_id)
);
CREATE INDEX blackboard_project_idx ON blackboard (project_id, id);
CREATE INDEX blackboard_task_idx ON blackboard (task_id, id);

-- Engineering & Verification ----------------------------------------------
CREATE TABLE evidence (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id     uuid NOT NULL REFERENCES project(id),
  task_id        uuid NOT NULL,
  task_revision  int  NOT NULL,
  candidate_hash text NOT NULL,
  check_id       text NOT NULL,
  command        text NOT NULL,
  exit_code      int,
  result         text NOT NULL CHECK (result IN ('PASS','FAIL','ERROR')),
  log_sha256     text NOT NULL DEFAULT '',
  log_path       text NOT NULL DEFAULT '',
  detail         jsonb NOT NULL DEFAULT '{}',
  runner_id      text NOT NULL,
  started_at     timestamptz NOT NULL,
  finished_at    timestamptz NOT NULL,
  -- PASS needs a real exit code 0 (V4.1 docs/12).
  CHECK (result <> 'PASS' OR (exit_code IS NOT NULL AND exit_code = 0)),
  FOREIGN KEY (task_id, project_id) REFERENCES task(id, project_id)
);
CREATE INDEX evidence_task_idx ON evidence (task_id, candidate_hash, check_id, finished_at);

-- Shared plumbing ----------------------------------------------------------
CREATE TABLE journal (
  seq        bigserial PRIMARY KEY,
  project_id uuid REFERENCES project(id),
  task_id    uuid REFERENCES task(id),
  type       text NOT NULL,
  actor      uuid REFERENCES principal(id),
  payload    jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (task_id, project_id) REFERENCES task(id, project_id)
);
CREATE INDEX journal_project_idx ON journal (project_id, seq);

CREATE TABLE idempotency (
  principal_id uuid NOT NULL REFERENCES principal(id),
  route        text NOT NULL, -- method + full path
  key          text NOT NULL,
  body_hash    text NOT NULL,
  status       int  NOT NULL,
  body         jsonb NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (principal_id, route, key)
);

-- Observed from OpenBot (read-only tail of openbot.db). OpenBot stays the owner of turns.
CREATE TABLE agent_turn (
  turn_id          text PRIMARY KEY,
  openbot_agent_id text NOT NULL DEFAULT '',
  thread_id        text NOT NULL DEFAULT '',
  status           text NOT NULL,
  started_at       timestamptz,
  completed_at     timestamptz,
  project_id       uuid REFERENCES project(id),
  task_id          uuid REFERENCES task(id)
);

-- Hash of each installed agent hook file, to detect tampering (limitation L2, AC-H-070).
CREATE TABLE hook_install (
  principal_id  uuid PRIMARY KEY REFERENCES principal(id),
  settings_path text NOT NULL,
  sha256        text NOT NULL,             -- hash registered at install; never overwritten by a scan
  seen_sha256   text NOT NULL DEFAULT '',  -- last differing hash already reported
  tampered      boolean NOT NULL DEFAULT false,
  checked_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tail_cursor (
  name       text PRIMARY KEY,
  value      text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- Context isolation at the database (V5L multi-project §3) ------------------
-- The service runs every request inside a transaction that does
--   SET LOCAL ROLE harness_app; set_config('kch.role' | 'kch.principal' | 'kch.projects', ..., true)
-- so these policies apply even if a query forgets its WHERE project_id.
-- +goose StatementBegin
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'harness_app') THEN
    CREATE ROLE harness_app NOLOGIN;
  END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION kch_role() RETURNS text LANGUAGE sql STABLE
AS $$ SELECT coalesce(nullif(current_setting('kch.role', true), ''), 'none') $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION kch_principal() RETURNS uuid LANGUAGE sql STABLE
AS $$ SELECT nullif(current_setting('kch.principal', true), '')::uuid $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION kch_projects() RETURNS uuid[] LANGUAGE sql STABLE
AS $$ SELECT coalesce(string_to_array(nullif(current_setting('kch.projects', true), ''), ',')::uuid[], '{}') $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION kch_lead_projects() RETURNS uuid[] LANGUAGE sql STABLE
AS $$ SELECT coalesce(string_to_array(nullif(current_setting('kch.lead_projects', true), ''), ',')::uuid[], '{}') $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION kch_trusted() RETURNS boolean LANGUAGE sql STABLE
AS $$ SELECT kch_role() IN ('owner','runtime') $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION kch_sees(pid uuid) RETURNS boolean LANGUAGE sql STABLE
AS $$ SELECT kch_trusted() OR (pid IS NOT NULL AND pid = ANY (kch_projects())) $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION kch_leads(pid uuid) RETURNS boolean LANGUAGE sql STABLE
AS $$ SELECT kch_trusted() OR (pid IS NOT NULL AND pid = ANY (kch_lead_projects())) $$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO harness_app;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO harness_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO harness_app;
-- No DELETE anywhere (spec: no delete permission in S1). Append-only tables get INSERT only.
GRANT INSERT ON source, journal, evidence, budget_ledger, blackboard TO harness_app;
GRANT INSERT, UPDATE ON project, principal, project_member, relation, task, budget,
                        idempotency, agent_turn, tail_cursor, hook_install TO harness_app;
GRANT INSERT ON project_repo, knowledge, lease TO harness_app;
-- Identity columns of knowledge and lease can never be rewritten (level, project, key, author, task, epoch).
GRANT UPDATE (trust, is_current, ended_reason, ended_note, replaced_by, ns) ON knowledge TO harness_app;
GRANT UPDATE (state, context_acked, pending_notice, wrote_files, reason, expires_at, ended_at) ON lease TO harness_app;

ALTER TABLE project        ENABLE ROW LEVEL SECURITY;
ALTER TABLE principal      ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_member ENABLE ROW LEVEL SECURITY;
ALTER TABLE source         ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge      ENABLE ROW LEVEL SECURITY;
ALTER TABLE task           ENABLE ROW LEVEL SECURITY;
ALTER TABLE lease          ENABLE ROW LEVEL SECURITY;
ALTER TABLE budget         ENABLE ROW LEVEL SECURITY;
ALTER TABLE budget_ledger  ENABLE ROW LEVEL SECURITY;
ALTER TABLE blackboard     ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence       ENABLE ROW LEVEL SECURITY;
ALTER TABLE journal        ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency    ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_turn     ENABLE ROW LEVEL SECURITY;
ALTER TABLE tail_cursor    ENABLE ROW LEVEL SECURITY;
ALTER TABLE relation       ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_repo   ENABLE ROW LEVEL SECURITY;
ALTER TABLE hook_install   ENABLE ROW LEVEL SECURITY;

-- Identity: only owner/runtime manage; an agent sees its own rows.
CREATE POLICY project_read  ON project FOR SELECT USING (kch_sees(id));
CREATE POLICY project_write ON project FOR INSERT WITH CHECK (kch_role() = 'owner');
CREATE POLICY project_edit  ON project FOR UPDATE USING (kch_role() = 'owner');
CREATE POLICY principal_read  ON principal FOR SELECT USING (kch_trusted() OR id = kch_principal());
CREATE POLICY principal_write ON principal FOR INSERT WITH CHECK (kch_trusted());
CREATE POLICY principal_edit  ON principal FOR UPDATE USING (kch_role() = 'owner');
CREATE POLICY member_read  ON project_member FOR SELECT USING (kch_trusted() OR principal_id = kch_principal());
CREATE POLICY member_write ON project_member FOR INSERT WITH CHECK (kch_role() = 'owner');
CREATE POLICY member_edit  ON project_member FOR UPDATE USING (kch_role() = 'owner');

-- Project-scoped rows.
CREATE POLICY source_all     ON source     FOR ALL USING (kch_sees(project_id))
  WITH CHECK (kch_trusted() OR (kch_sees(project_id) AND created_by = kch_principal() AND origin = 'AGENT'));
CREATE POLICY repo_read      ON project_repo FOR SELECT USING (kch_role() <> 'none'); -- every authenticated scope may read roots, to refuse cross-project writes
CREATE POLICY repo_write     ON project_repo FOR INSERT WITH CHECK (kch_role() = 'owner');
-- Tasks and leases: members read; only owner, runtime and the project's leads write.
-- A lease holder may update its own lease (heartbeat, context ack, release).
CREATE POLICY task_read      ON task  FOR SELECT USING (kch_sees(project_id));
CREATE POLICY task_insert    ON task  FOR INSERT WITH CHECK (kch_leads(project_id));
CREATE POLICY task_update    ON task  FOR UPDATE USING (kch_leads(project_id)) WITH CHECK (kch_leads(project_id));
CREATE POLICY lease_read     ON lease FOR SELECT USING (kch_sees(project_id));
CREATE POLICY lease_insert   ON lease FOR INSERT WITH CHECK (kch_leads(project_id));
CREATE POLICY lease_update   ON lease FOR UPDATE
  USING (kch_leads(project_id) OR (kch_sees(project_id) AND principal_id = kch_principal()))
  WITH CHECK (kch_leads(project_id) OR (kch_sees(project_id) AND principal_id = kch_principal()));
CREATE POLICY blackboard_all ON blackboard FOR ALL USING (kch_sees(project_id))
  WITH CHECK (kch_trusted() OR (kch_sees(project_id) AND author = kch_principal()));
CREATE POLICY relation_read   ON relation FOR SELECT USING (kch_sees(project_id));
CREATE POLICY relation_insert ON relation FOR INSERT WITH CHECK (
  kch_role() = 'owner' OR (kch_role() = 'agent' AND kch_sees(project_id) AND created_by = kch_principal()));
CREATE POLICY relation_update ON relation FOR UPDATE
  USING (kch_trusted() OR (kch_sees(project_id) AND created_by = kch_principal()))
  WITH CHECK (kch_sees(project_id));
CREATE POLICY hook_install_all ON hook_install FOR ALL USING (kch_trusted()) WITH CHECK (kch_trusted());
CREATE POLICY journal_all    ON journal    FOR ALL USING (kch_sees(project_id))
  WITH CHECK (kch_trusted() OR (kch_sees(project_id) AND actor = kch_principal()));

-- Memory: GLOBAL is readable once AUTHORITATIVE (or by its author); AGENT only by its author.
-- No agent can write an AUTHORITATIVE row, whatever the application code does (PM-H-06).
CREATE POLICY knowledge_read ON knowledge FOR SELECT USING (
  kch_trusted()
  OR (level = 'GLOBAL'  AND (trust = 'AUTHORITATIVE' OR created_by = kch_principal()))
  OR (level = 'PROJECT' AND kch_sees(project_id))
  OR (level = 'AGENT'   AND kch_sees(project_id) AND created_by = kch_principal()));
CREATE POLICY knowledge_insert ON knowledge FOR INSERT WITH CHECK (
  kch_role() = 'owner'
  OR (kch_role() = 'agent' AND trust IN ('CANDIDATE','OBSERVED') AND created_by = kch_principal()
      AND (level = 'GLOBAL' OR kch_sees(project_id))));
CREATE POLICY knowledge_update ON knowledge FOR UPDATE
  USING (kch_role() = 'owner' OR (kch_role() = 'agent' AND created_by = kch_principal() AND trust <> 'AUTHORITATIVE'))
  WITH CHECK (kch_role() = 'owner' OR trust IN ('CANDIDATE','OBSERVED','HISTORICAL'));

-- Runtime-only writers: evidence, ledger, observed turns, cursors (PM-H-18).
CREATE POLICY evidence_read   ON evidence FOR SELECT USING (kch_sees(project_id));
CREATE POLICY evidence_write  ON evidence FOR INSERT WITH CHECK (kch_role() = 'runtime');
CREATE POLICY ledger_read     ON budget_ledger FOR SELECT USING (kch_sees(project_id));
CREATE POLICY ledger_write    ON budget_ledger FOR INSERT WITH CHECK (kch_role() = 'runtime');
CREATE POLICY turn_read       ON agent_turn FOR SELECT USING (kch_sees(project_id));
CREATE POLICY turn_write      ON agent_turn FOR INSERT WITH CHECK (kch_role() = 'runtime');
CREATE POLICY turn_edit       ON agent_turn FOR UPDATE USING (kch_role() = 'runtime');
CREATE POLICY cursor_all      ON tail_cursor FOR ALL USING (kch_role() = 'runtime') WITH CHECK (kch_role() = 'runtime');

-- Budgets: everyone may read the SYSTEM row; limits and totals change only under owner/runtime.
CREATE POLICY budget_read   ON budget FOR SELECT USING (level = 'SYSTEM' OR kch_sees(project_id));
CREATE POLICY budget_insert ON budget FOR INSERT WITH CHECK (kch_trusted() OR (level = 'TASK' AND kch_leads(project_id)));
CREATE POLICY budget_edit   ON budget FOR UPDATE USING (kch_trusted());

CREATE POLICY idem_all ON idempotency FOR ALL
  USING (kch_trusted() OR principal_id = kch_principal())
  WITH CHECK (kch_trusted() OR principal_id = kch_principal());

-- +goose Down
DROP TABLE hook_install, relation, project_repo, tail_cursor, agent_turn, idempotency, journal, evidence, blackboard,
           budget_ledger, budget, lease, task, knowledge, source, project_member, principal, project;
DROP FUNCTION kch_leads(uuid), kch_lead_projects(), kch_sees(uuid), kch_trusted(), kch_projects(), kch_principal(), kch_role(), kch_unaccent(text);
