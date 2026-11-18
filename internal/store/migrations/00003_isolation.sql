-- +goose Up
-- Spec S2 part A (isolation). No new table: two owner-edited columns.

-- Read-only reference roots the Owner declares for a project (PM-S2-04). Like
-- repo roots they are realpaths; members may read them (sandbox allowRead and
-- the Read/Glob/Grep allowlist of the hook), nobody writes them.
ALTER TABLE project
  ADD COLUMN reference_roots text[] NOT NULL DEFAULT '{}'
  CONSTRAINT project_reference_roots_abs CHECK (array_to_string(reference_roots, E'\n') !~ '(^|\n)[^/]');

-- Specialty of an agent (PM-S2-04); who may be consulted is decided from it (spec S2 C.3).
ALTER TABLE principal
  ADD COLUMN specialty text CHECK (specialty IN ('product','engineering','design','quality'));

-- Both tables already grant INSERT, UPDATE to harness_app and their UPDATE
-- policies (project_edit, principal_edit) let only the Owner change a row, so
-- the new columns need no grant or policy of their own (00001 conventions).
-- hook_install needs no new column: seen_sha256 keeps the last reported
-- fingerprint (file hash plus the extra config files that were present).

-- +goose Down
ALTER TABLE principal DROP COLUMN specialty;
ALTER TABLE project DROP COLUMN reference_roots;
