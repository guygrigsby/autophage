# 5. Triage by a mid-tier model, budgeted attempt as the backstop

Status: Accepted
Date: 2026-09-04

## Context

Small bugs and small features should be fixed without a human in the loop; large work needs approval first. A model reading the issue text alone is an unreliable estimator of size. The agent's own investigation is the best estimator available, but it costs tokens and time.

## Decision

Both, in sequence.

- Trusted cases are triaged by a mid-tier model into Small or Large with a rationale. Large waits for approval with the rationale posted. Small proceeds.
- Every attempt runs under a Budget of turns, wall clock and diff lines, `auto` or `approved` by kind. The agent is told the situation and the exact numbers in its brief. At 80% of turns or wall clock a steer tells it to wrap up: commit what it has, then write the summary. Diff lines are checked after each tool call.
- Every attempt ends with one forced summary turn in a fixed shape: what I found, what I did, what is left, what I would do with more budget.
- On exhaustion the branch is pushed anyway and the summary is posted. The case waits for approval. The approved attempt resumes on the same branch, rebased onto the default branch, with the prior summary in its brief.

## Consequences

- Obvious calls ("rewrite the auth layer") cost one cheap model call. Everything else costs at most the auto budget before a human sees it, and the human sees findings, not a bare "this is big".
- No work is lost on a budget stop. The approved attempt starts from where the auto attempt stopped.
- Budget numbers are open and will need tuning against real issues.
