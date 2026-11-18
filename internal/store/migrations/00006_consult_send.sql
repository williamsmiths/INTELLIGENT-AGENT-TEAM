-- +goose Up
-- Spec S2 C.3 after REV-005 #4: a consult is PENDING_SEND until the host took
-- its message, retried by the worker with a bounded backoff until expires_at;
-- never sent, it ends FAILED and does not count against the task's allowance.
ALTER TABLE consult DROP CONSTRAINT consult_status_check;
ALTER TABLE consult ADD CONSTRAINT consult_status_check
  CHECK (status IN ('PENDING_SEND','OPEN','ANSWERED','EXPIRED','FAILED'));
ALTER TABLE consult
  ADD COLUMN send_attempts int NOT NULL DEFAULT 0 CHECK (send_attempts >= 0),
  ADD COLUMN next_send_at  timestamptz; -- next retry of a PENDING_SEND consult (worker)
-- The same question may be asked again after a FAILED consult.
ALTER TABLE consult DROP CONSTRAINT consult_task_id_question_hash_key;
CREATE UNIQUE INDEX consult_task_question_idx ON consult (task_id, question_hash) WHERE status <> 'FAILED';
CREATE INDEX consult_send_idx ON consult (next_send_at) WHERE status = 'PENDING_SEND';
GRANT UPDATE (send_attempts, next_send_at) ON consult TO harness_app;

-- Lookups by task and journal order (missed feedback, REV-005 #3; phase at occurrence, #10; lesson stop, #7).
CREATE INDEX journal_task_idx ON journal (task_id, type, seq) WHERE task_id IS NOT NULL;

-- +goose Down
DROP INDEX journal_task_idx;
REVOKE UPDATE (send_attempts, next_send_at) ON consult FROM harness_app;
DROP INDEX consult_send_idx;
DROP INDEX consult_task_question_idx;
ALTER TABLE consult ADD CONSTRAINT consult_task_id_question_hash_key UNIQUE (task_id, question_hash);
ALTER TABLE consult DROP COLUMN next_send_at, DROP COLUMN send_attempts;
ALTER TABLE consult DROP CONSTRAINT consult_status_check;
ALTER TABLE consult ADD CONSTRAINT consult_status_check CHECK (status IN ('OPEN','ANSWERED','EXPIRED'));
