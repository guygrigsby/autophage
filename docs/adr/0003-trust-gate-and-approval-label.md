# 3. Trust gate by author association, approval by label

Status: Accepted
Date: 2026-09-04

## Context

Anyone can open an issue on a public repository. The daemon must not run an agent on arbitrary input, and a human must be able to authorize it cheaply from GitHub itself.

GitHub's `author_association` has tiers. CONTRIBUTOR means any merged PR, ever, so one typo fix would buy an attacker auto-start on every later issue. Anyone can comment "approved"; only people with triage permission or better can add labels.

## Decision

- Trusted is OWNER, MEMBER or COLLABORATOR. Everything else is Untrusted. The verdict is made at receipt and stored with the association as evidence.
- Untrusted issues are Gated: no attempt, no comment, no label. Silent.
- Approval is the `approved` label. A comment is never an approval.
- An approval permits an attempt regardless of trust or triage size, and resumes a case from AwaitingApproval or Failed.
- Removing and re-adding the label is the retry mechanism.

## Consequences

- The label doubles as acceptance of the issue body as agent input. Approving an untrusted issue is a deliberate act of reading it first.
- Gated cases give no signal to the issue author, by design. The operator sees them in `autophage cases --state gated`.
- The label name `approved` can collide with an existing label in some repository. Open; `autophage:approved` is the fallback.
