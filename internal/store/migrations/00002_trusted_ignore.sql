-- +goose Up
-- Trusted ignore list of a repo root (spec S2 D.1, AC-S2-050, AC-S2-052). The
-- verification snapshot ignores exactly these patterns (root .gitignore
-- syntax) and never reads a .gitignore from the working tree. Default: the
-- root .gitignore of HEAD at registration; afterwards only the Owner edits it
-- (PUT /v1/projects/{p}/repos/ignore).
ALTER TABLE project_repo
  ADD COLUMN ignore_patterns text[] NOT NULL DEFAULT '{}'
  CONSTRAINT project_repo_ignore_one_line CHECK (array_to_string(ignore_patterns, '') !~ '[\n\r]');

-- The root and the project of a registration stay immutable: only this column may be updated, and only by the Owner.
GRANT UPDATE (ignore_patterns) ON project_repo TO harness_app;
CREATE POLICY repo_edit ON project_repo FOR UPDATE USING (kch_role() = 'owner') WITH CHECK (kch_role() = 'owner');

-- +goose Down
DROP POLICY repo_edit ON project_repo;
REVOKE UPDATE (ignore_patterns) ON project_repo FROM harness_app;
ALTER TABLE project_repo DROP COLUMN ignore_patterns;
