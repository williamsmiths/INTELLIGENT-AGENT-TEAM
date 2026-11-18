-- +goose Up
-- Spec S2 part C (Intelligence Layer), tables of C.11. Every new table has
-- project_id, the composite foreign key (task_id, project_id), row-level
-- security on kch_sees(project_id) and no DELETE grant (00001 conventions).

-- C.1 router, C.7 phase, C.4 steer text, AC-S2-039 deny-once notice: state of one lease.
ALTER TABLE lease
  ADD COLUMN intel_mode     text    NOT NULL DEFAULT 'DIRECT' CHECK (intel_mode IN ('DIRECT','ASSISTED','DEEP')),
  ADD COLUMN intel_signals  jsonb   NOT NULL DEFAULT '{}',
  ADD COLUMN intel_features text[]  NOT NULL DEFAULT '{gate,questions,consult,convergence,lesson}'
    CHECK (intel_features <@ '{gate,questions,consult,convergence,lesson}'::text[]),
  ADD COLUMN intel_forced   boolean NOT NULL DEFAULT false, -- the owner chose the mode at assign (kept on re-grant)
  ADD COLUMN intel_phase    text    NOT NULL DEFAULT 'BUILD' CHECK (intel_phase IN ('DISCOVERY','BUILD','REWORK')),
  ADD COLUMN intel_mark     jsonb,                          -- working tree at the first tool call of the lease (C.2, C.5)
  ADD COLUMN intel_inbox    text    NOT NULL DEFAULT '',    -- steer text for the next injected context (C.4)
  ADD COLUMN write_notice   text    NOT NULL DEFAULT '';    -- refused once at the first write (AC-S2-039)
GRANT UPDATE (intel_phase, intel_mark, intel_inbox, write_notice) ON lease TO harness_app;

-- C.8: the reasons a task waits, as a set. WAITING_DECISION rows of S1 waited for the budget.
ALTER TABLE task
  ADD COLUMN waiting_reasons text[] NOT NULL DEFAULT '{}'
    CHECK (waiting_reasons <@ '{budget,question,nonprogress}'::text[]);
UPDATE task SET waiting_reasons = '{budget}' WHERE state = 'WAITING_DECISION';

-- C.7: cost phase and the host turn of each ledger row. COLLAB rows are moved
-- to the asker's task once the consult turn is known (runtime only).
ALTER TABLE budget_ledger
  ADD COLUMN phase   text CHECK (phase IN ('DISCOVERY','BUILD','REWORK','COLLAB')),
  ADD COLUMN turn_id text NOT NULL DEFAULT '';
CREATE INDEX budget_ledger_turn_idx ON budget_ledger (principal_id, turn_id) WHERE turn_id <> '';
GRANT UPDATE (project_id, task_id, phase) ON budget_ledger TO harness_app;
CREATE POLICY ledger_edit ON budget_ledger FOR UPDATE USING (kch_role() = 'runtime') WITH CHECK (kch_role() = 'runtime');

-- C.2: every submitted understanding, rejected or accepted; immutable.
CREATE TABLE understanding (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id    uuid NOT NULL REFERENCES project(id),
  task_id       uuid NOT NULL,
  lease_id      uuid NOT NULL REFERENCES lease(id),
  task_revision int  NOT NULL,
  author        uuid NOT NULL REFERENCES principal(id),
  kind          text NOT NULL CHECK (kind IN ('FULL','DELTA')),
  status        text NOT NULL CHECK (status IN ('REJECTED','ACCEPTED','FORCED')),
  body          jsonb NOT NULL,
  unaddressed   jsonb NOT NULL DEFAULT '[]',
  created_at    timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (task_id, project_id) REFERENCES task(id, project_id)
);
CREATE INDEX understanding_task_idx ON understanding (task_id, task_revision, kind);

-- C.2: questions of a lease holder to the owner.
CREATE TABLE question (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id       uuid NOT NULL REFERENCES project(id),
  task_id          uuid NOT NULL,
  seq              int  NOT NULL CHECK (seq >= 1),
  understanding_id uuid REFERENCES understanding(id),
  asked_by         uuid NOT NULL REFERENCES principal(id),
  task_revision    int  NOT NULL,
  text             text NOT NULL CHECK (char_length(text) BETWEEN 1 AND 2000),
  options          jsonb NOT NULL,
  default_option   text NOT NULL,
  impact           text NOT NULL CHECK (impact IN ('low','medium','high')),
  blocking         boolean NOT NULL,
  status           text NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','ANSWERED')),
  answer           text,
  answered_by      uuid REFERENCES principal(id),
  answered_at      timestamptz,
  differs          boolean,
  decision_id      uuid REFERENCES knowledge(id),
  surfaced_at      timestamptz, -- listed once in a Stop block (C.2)
  waiting_noted_at timestamptz, -- QUESTION_WAITING journaled
  created_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (task_id, seq),
  FOREIGN KEY (task_id, project_id) REFERENCES task(id, project_id)
);
CREATE INDEX question_open_idx ON question (status, created_at) WHERE status = 'OPEN';

-- C.3: a consult; asker and answerer are members of the consult's project (FK).
CREATE TABLE consult (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id    uuid NOT NULL REFERENCES project(id),
  task_id       uuid NOT NULL,
  lease_id      uuid NOT NULL REFERENCES lease(id),
  asker         uuid NOT NULL,
  answerer      uuid NOT NULL,
  specialty     text NOT NULL CHECK (specialty IN ('product','engineering','design','quality')),
  question      text NOT NULL CHECK (char_length(question) BETWEEN 1 AND 1200),
  question_hash text NOT NULL,
  refs          jsonb NOT NULL DEFAULT '[]',
  status        text NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','ANSWERED','EXPIRED')),
  delivery_id   text NOT NULL DEFAULT '',
  turn_id       text NOT NULL DEFAULT '',
  answer        text CHECK (answer IS NULL OR char_length(answer) BETWEEN 1 AND 1500),
  answer_refs   jsonb NOT NULL DEFAULT '[]',
  finding_id    bigint REFERENCES blackboard(id),
  answered_at   timestamptz,
  informed_at   timestamptz, -- the asker was told the outcome (answer or expiry)
  expires_at    timestamptz NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CHECK (asker <> answerer),
  UNIQUE (task_id, question_hash),
  FOREIGN KEY (task_id, project_id) REFERENCES task(id, project_id),
  FOREIGN KEY (project_id, asker) REFERENCES project_member(project_id, principal_id),
  FOREIGN KEY (project_id, answerer) REFERENCES project_member(project_id, principal_id)
);
CREATE INDEX consult_answerer_idx ON consult (answerer, status);
-- Two consults delivered in one host turn share it; its usage goes to the oldest one (work.RecordUsage).
CREATE INDEX consult_turn_idx ON consult (answerer, turn_id) WHERE turn_id <> '';

-- C.4, C.5: one row per verify of a lease holder. Runtime only.
CREATE TABLE attempt (
  id             bigserial PRIMARY KEY,
  project_id     uuid NOT NULL REFERENCES project(id),
  task_id        uuid NOT NULL,
  lease_id       uuid NOT NULL REFERENCES lease(id),
  principal_id   uuid NOT NULL REFERENCES principal(id),
  task_revision  int  NOT NULL,
  candidate      text NOT NULL,
  all_pass       boolean NOT NULL,
  passing        text[] NOT NULL DEFAULT '{}',
  failing        text[] NOT NULL DEFAULT '{}',
  fingerprint    text NOT NULL,
  repeats        int  NOT NULL CHECK (repeats >= 0), -- identical failing attempts in a row, this one included
  delta_mismatch jsonb,
  created_at     timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (task_id, project_id) REFERENCES task(id, project_id)
);
CREATE INDEX attempt_task_idx ON attempt (task_id, id);

-- C.7: every context packet injected by a hook. Runtime only.
CREATE TABLE packet_log (
  id           bigserial PRIMARY KEY,
  project_id   uuid NOT NULL REFERENCES project(id),
  task_id      uuid NOT NULL,
  lease_id     uuid NOT NULL REFERENCES lease(id),
  principal_id uuid NOT NULL REFERENCES principal(id),
  event        text NOT NULL,
  lease_mode   text NOT NULL,
  intel_mode   text NOT NULL,
  chars        int  NOT NULL CHECK (chars >= 0),
  packet_hash  text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (task_id, project_id) REFERENCES task(id, project_id)
);
CREATE INDEX packet_log_task_idx ON packet_log (task_id, id);

GRANT SELECT, INSERT ON understanding, question, consult, attempt, packet_log TO harness_app;
GRANT USAGE, SELECT ON SEQUENCE attempt_id_seq, packet_log_id_seq TO harness_app;
GRANT UPDATE (status, answer, answered_by, answered_at, differs, decision_id, surfaced_at, waiting_noted_at) ON question TO harness_app;
GRANT UPDATE (status, delivery_id, turn_id, answer, answer_refs, finding_id, answered_at, informed_at) ON consult TO harness_app;

ALTER TABLE understanding ENABLE ROW LEVEL SECURITY;
ALTER TABLE question      ENABLE ROW LEVEL SECURITY;
ALTER TABLE consult       ENABLE ROW LEVEL SECURITY;
ALTER TABLE attempt       ENABLE ROW LEVEL SECURITY;
ALTER TABLE packet_log    ENABLE ROW LEVEL SECURITY;

-- understanding: the author (an agent of the project) or the runtime writes; never updated.
CREATE POLICY understanding_read   ON understanding FOR SELECT USING (kch_sees(project_id));
CREATE POLICY understanding_insert ON understanding FOR INSERT WITH CHECK (
  kch_trusted() OR (kch_role() = 'agent' AND kch_sees(project_id) AND author = kch_principal()));
-- question: the agent creates an OPEN question; only owner/runtime change status and answer.
CREATE POLICY question_read   ON question FOR SELECT USING (kch_sees(project_id));
CREATE POLICY question_insert ON question FOR INSERT WITH CHECK (
  kch_trusted() OR (kch_role() = 'agent' AND kch_sees(project_id) AND asked_by = kch_principal()
                    AND status = 'OPEN' AND answer IS NULL AND answered_by IS NULL AND decision_id IS NULL));
CREATE POLICY question_edit   ON question FOR UPDATE USING (kch_trusted()) WITH CHECK (kch_trusted());
-- consult: the asker creates an OPEN consult; the answer is written by the runtime after its checks.
CREATE POLICY consult_read   ON consult FOR SELECT USING (kch_sees(project_id));
CREATE POLICY consult_insert ON consult FOR INSERT WITH CHECK (
  kch_trusted() OR (kch_role() = 'agent' AND kch_sees(project_id) AND asker = kch_principal()
                    AND status = 'OPEN' AND answer IS NULL));
CREATE POLICY consult_edit   ON consult FOR UPDATE USING (kch_trusted()) WITH CHECK (kch_trusted());
-- attempt, packet_log: runtime only.
CREATE POLICY attempt_read    ON attempt    FOR SELECT USING (kch_sees(project_id));
CREATE POLICY attempt_write   ON attempt    FOR INSERT WITH CHECK (kch_role() = 'runtime');
CREATE POLICY packet_log_read  ON packet_log FOR SELECT USING (kch_sees(project_id));
CREATE POLICY packet_log_write ON packet_log FOR INSERT WITH CHECK (kch_role() = 'runtime');

-- +goose Down
DROP TABLE packet_log, attempt, consult, question, understanding;
DROP POLICY ledger_edit ON budget_ledger;
REVOKE UPDATE (project_id, task_id, phase) ON budget_ledger FROM harness_app;
DROP INDEX budget_ledger_turn_idx;
ALTER TABLE budget_ledger DROP COLUMN turn_id, DROP COLUMN phase;
ALTER TABLE task DROP COLUMN waiting_reasons;
REVOKE UPDATE (intel_phase, intel_mark, intel_inbox, write_notice) ON lease FROM harness_app;
ALTER TABLE lease DROP COLUMN write_notice, DROP COLUMN intel_inbox, DROP COLUMN intel_mark, DROP COLUMN intel_phase,
  DROP COLUMN intel_forced, DROP COLUMN intel_features, DROP COLUMN intel_signals, DROP COLUMN intel_mode;
