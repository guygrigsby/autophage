# 7. Dependabot bumps as an Upkeep context

Status: Accepted

## Context

Autophage handles issues. `resolution.Case` is keyed by (repository, issue
number), `Case.Branch()` is `autophage/<number>`, and an attempt clones the
default branch, cuts a new branch, works, pushes and opens a pull request.
The App subscribes to `issues`, `installation` and
`installation_repositories` (`docs/runbooks/deploy.md:89`).

A dependabot pull request inverts most of that. The branch and the diff
already exist. Nobody requested anything, so there is no requester to
trust. There is no size to triage, because the question is not how big the
change is but whether it breaks the build, and CI already answers that. The
work is making an existing branch go green, not writing a fix from a
description.

Two cheaper framings were considered first and rejected:

- Run the repository's own tests in the sandbox against the pull request
  head and comment the verdict. Actions already ran those tests on that head
  and GitHub pushes the conclusion as `check_suite` and `workflow_run`.
  Recomputing it spends a sandbox run per bump across every enrolled
  repository against `sandbox.concurrency = 2`, for an answer already in
  hand.
- Approve and merge on green. GitHub does this natively through dependabot
  auto-merge and `gh pr merge --auto`. Reimplementing it buys centralized
  policy and nothing else.

What neither of those does, and nothing free does, is fix a red bump.

## Decision

Autophage reacts to the CI check conclusion and repairs the red ones. It
never merges.

**Upkeep is a second bounded context, not a second flavour of Case.** Five
of Resolution's load-bearing terms do not survive the crossing: `Requester`
and `Approval` do not exist, `Trust` is a constant rather than a verdict,
`Triage` is replaced by a fact from CI rather than a model judgement, and
`Outcome`'s success is green checks rather than a pull request opened. Same
word, different meaning is the seam, so the split goes there.

**`Budget`, `Limit`, `Usage`, `FailureClass`, `Run` and `Brief` are a
shared kernel.** They mean exactly the same thing on both sides, and
duplicating them would leave two near-identical copies to drift apart.
`AbortReason` is deliberately out of it: `issue_closed` means nothing to a
bump and `pull_request_closed` means nothing to a case, so a shared
vocabulary would be a table with rows that are invalid on one side. Everything else is owned by one
context. `Bump` names `Repository` by full name and nothing else crosses; a
Case and a Bump open on the same repository never reference each other.

**`RepairAttempt` is the Case-side attempt shape with a different owner.**
Bump owns its own rounds in its own tables rather than sharing a table with
Case, because an object owned by two roots is a modeling error that does not
surface until something deletes one of them.

**Dependabot is trusted by identity.** Its `author_association` is `NONE`,
which the existing gate would read as untrusted and park every bump waiting
for a label. The bot login is the trust evidence, not the association,
because the association is a statement about a person and there is no person
here.

**Enrollment is not enough; a repository needs a `Watch`.** Being installed
on a repository authorises autophage to act on its issues. Acting on its
dependency branches is a second, explicit opt-in, so a rollout is
incremental and a bad interaction cannot appear on a repository nobody was
watching.

**Two loop guards, because there is a real cycle here.** Autophage pushes,
checks re-run, a verdict arrives, autophage pushes again. First guard: a
`CheckVerdict` whose sha is not the bump's current head is refused rather
than applied, so a late verdict for a replaced head cannot move anything.
Second guard: a round cap, so the cycle is bounded even when every verdict
is legitimately current.

**`green` and `abandoned` are not terminal states.** Only `closed` is.
Someone merging a green bump is the most ordinary way one ends, and it
arrives as a delivery after the bump reached `green`. Making `green`
terminal would refuse that delivery and record the normal ending as a
rejection, which is exactly what `autophage-2lq` observed live on the Case
side when `Done` refused the `issues.closed` that a merge produced.

**Abandonment is sticky.** A dependabot force-push does not revive an
abandoned bump. A rebase of a version autophage already failed to repair is
not new information, and reviving on every force-push is an unbounded retry
loop under another name. A genuinely newer version arrives as a new pull
request, which is a new bump.

## Consequences

The App gains three subscriptions it does not have: `pull_request`,
`check_suite` and `workflow_run`. Permissions already cover the work
(contents write, pull requests write), so no new grant is needed and the
installation does not have to be re-accepted.

`internal/github` gains a check rollup query. A single `check_suite.completed`
does not mean the checks are done, because a head can have several suites and
nothing in the payload says how many, so the adapter asks GitHub for the
combined rollup and records a verdict only when it is conclusive. A pending
rollup records nothing, which is why a bump that runs no checks at all needs
a wait window rather than waiting for ever.

`internal/sandbox` gains a checkout that does not rebase. The existing
`checkout` always replays the branch onto the default branch, which is right
for a branch autophage owns and wrong here: rebasing changes the tree CI
just judged, so the repair would start from a state no verdict describes.
The repair checks out the exact head sha instead, and pushes back under the
same force-with-lease `CommitAndPush` already uses.

Autophage now writes to branches it did not create. The lease is what keeps
that honest: a push whose expected head no longer matches fails rather than
clobbering a dependabot force-push that landed mid-round.

Volume is the operational risk. Bumps arrive on a cadence nobody controls,
across every watched repository, into the same dispatcher pool the Case side
uses. `Watch` is the throttle for now, and a repair round competing with an
issue attempt for one of two slots is a real contention the design does not
otherwise solve.

A repair round that fails on infrastructure requeues rather than abandoning.
The round is spent, so the cap still bounds it, but treating a podman hiccup
or a GitHub outage as a verdict about the code would abandon bumps for
reasons that have nothing to do with them. Model and agent failures are
verdicts about the code and do abandon.

Rejected alternatives, one line each: a Case subtype keyed by pull request
number (five terms mean different things, so it is a seam, not a flag);
recomputing CI in the sandbox (duplicates Actions); merging on green
(duplicates native auto-merge); reviving abandoned bumps on force-push
(unbounded retry).
