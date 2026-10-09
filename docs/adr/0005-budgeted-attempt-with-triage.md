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

## Amendment: 2026-10-13 — exhaustion opens the partial work as a pull request

The operator asked for a more permissive posture: a smarter attempt model
(anthropic/claude-sonnet-5.5) under wider budgets, and no more changes
stranded by a budget stop. "No work is lost on a budget stop" was already
true of the bytes — the branch was pushed — but reviewing them meant
opening the PR by hand, and the exhaustion comment read like an abort.

Decision: when a budget stop leaves committed, pushed, marker-free work,
the runner opens a pull request for it (or reuses the one a prior attempt
on the same branch already opened) and records `pr_number`/`head_sha` on
the `BudgetExhausted` outcome. The case still goes to AwaitingApproval:
approval still re-queues, the approved attempt still resumes the same
branch, and its PR is the same one. A stop with nothing committed, a tree
with conflict markers, or a PR-open failure records the outcome without
the PR, exactly as before — opening the partial PR is best effort and
never turns an exhaustion into an infrastructure failure.
