-- +goose Up
-- Spec S2 part E (kch delegate). The Owner's delegate policy of a project:
-- {"required_checks":[{"id","command"}], "protected_paths":["…"]}, copied into
-- every task a Lead delegates (PM-S2-21, E.1-4). '{}' = no policy yet, so a
-- delegate is refused with 409 until the Owner sets required_checks.
ALTER TABLE project
  ADD COLUMN delegate_policy jsonb NOT NULL DEFAULT '{}'
  CONSTRAINT project_delegate_policy_object CHECK (jsonb_typeof(delegate_policy) = 'object');

-- project already grants INSERT, UPDATE to harness_app and its UPDATE policy
-- (project_edit) lets only the Owner change a row, so the new column needs no
-- grant or policy of its own (00001, 00003 conventions).

-- Rate limit of E.1-10 (REV-006b N3): successful delegates of one caller in
-- the last 60 minutes are counted from the journal; without this index the
-- count would scan the whole journal.
CREATE INDEX journal_delegate_rate_idx ON journal (actor, created_at)
  WHERE type = 'TASK_CREATED' AND payload->>'via' = 'delegate';

-- +goose Down
DROP INDEX journal_delegate_rate_idx;
ALTER TABLE project DROP COLUMN delegate_policy;
