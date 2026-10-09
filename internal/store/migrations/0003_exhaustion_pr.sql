-- +goose Up
-- A budget stop with committed work no longer strands it on a branch: the
-- runner opens (or reuses) a pull request and records it on the exhaustion.
-- Amendment of 2026-10-13 to docs/adr/0005-budgeted-attempt-with-triage.md;
-- DDL verbatim from docs/specs/2026-10-13-autophage-partial-pr-contracts.md.
--
-- Up-only, like 0001 and 0002. Existing rows predate partial PRs and keep
-- both columns null, which the CHECK reads as "nothing to review".

ALTER TABLE attempt_outcome_exhaustions
  ADD COLUMN pr_number INTEGER,
  ADD COLUMN head_sha TEXT,
  ADD CONSTRAINT attempt_outcome_exhaustions_pr CHECK (
    (pr_number IS NULL AND head_sha IS NULL)
    OR (pr_number IS NOT NULL AND pr_number >= 1 AND head_sha ~ '^[0-9a-f]{40}$')
  );
