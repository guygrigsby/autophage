# Autophage contracts: the partial pull request on exhaustion

Dated: 2026-10-13. An amendment to
[2026-09-04-autophage-contracts.md](2026-09-04-autophage-contracts.md), which
stays normative for everything it already covered. The DDL here is migration
`0003_exhaustion_pr.sql` verbatim.

## What changed

A budget stop (turns, wall clock or diff lines) on an attempt whose run left
committed, pushed, marker-free work now opens a pull request for that work,
or reuses the one a prior attempt on the same branch already opened, and the
`BudgetExhausted` outcome records it. Nothing else about exhaustion moves:
the case still goes to `AwaitingApproval`, approval still re-queues, the
approved attempt still resumes the same branch, and its pull request is the
same one. A stop with nothing committed, a tree carrying conflict markers, or
a pull request that will not open records the outcome without the pull
request, exactly as before — opening the partial pull request is best effort
and never turns an exhaustion into an infrastructure failure.

## DDL

Existing rows predate partial pull requests and keep both columns null, which
the CHECK reads as "nothing to review".

```sql
ALTER TABLE attempt_outcome_exhaustions
  ADD COLUMN pr_number INTEGER,
  ADD COLUMN head_sha TEXT,
  ADD CONSTRAINT attempt_outcome_exhaustions_pr CHECK (
    (pr_number IS NULL AND head_sha IS NULL)
    OR (pr_number IS NOT NULL AND pr_number >= 1 AND head_sha ~ '^[0-9a-f]{40}$')
  );
```

## Cross-check

The one transition this touches, in the base document's terms:

| Transition | API | Event | DDL |
|---|---|---|---|
| `Attempting -> AwaitingApproval` (exhausted) | none: runner | `AttemptEnded` | `attempt_outcomes` + `attempt_outcome_exhaustions` (`pr_number`/`head_sha` set when the partial work was opened as a PR), transition |

The GitHub port gains `PullRequestForBranch(repository, branch) (prNumber, found, err)`,
used by the runner when opening fails because the branch's pull request
already exists.
