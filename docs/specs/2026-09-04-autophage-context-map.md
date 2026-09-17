# Context map: autophage

## Ubiquitous language

| Term | Means | Lives in |
|---|---|---|
| Case | One issue in one enrolled repository that autophage is handling. Identified by (repository, number) | Resolution |
| Repository | An enrolled GitHub repository autophage may act on | Resolution |
| Requester | The person who opened the issue or added the label, with the trust verdict made at the time | Resolution |
| Trust | Trusted or Untrusted: whether the requester may start an attempt without an approval | Resolution |
| Triage | The size verdict on a trusted case, Small or Large, with the model's rationale | Resolution |
| Approval | A maintainer added the `approved` label. Permits an attempt regardless of trust or size | Resolution |
| Attempt | One budgeted try at a case on the case's branch | Resolution |
| Run | The part of an attempt where the agent actually executed. Carries the jess run id | Resolution |
| Budget | The limits on one attempt: turns, wall clock, diff lines | Resolution |
| Brief | The complete prompt handed to the agent for one attempt | Resolution |
| Outcome | How an attempt ended: PullRequestOpened, BudgetExhausted, Failed or Aborted, with usage | Resolution |
| Transition | One recorded state change of a case, with its cause. The Scheduler orders the queue by it | Resolution |
| Closure | The issue was closed on GitHub. Terminal for the case | Resolution |
| Delivery | One webhook delivery from GitHub, stored byte-exact, processed at most once | GitHub boundary |
| Workspace | The host directory holding a case's checkout on its branch | Sandbox boundary |
| Toolbox | The process inside the container that executes the agent's tools | Sandbox boundary |
| Bump | One dependabot pull request on a watched repository that autophage is shepherding to green. Identified by (repository, pull request number) | Upkeep |
| Watch | A repository Upkeep acts on. Enrollment authorises issues; a watch authorises bumps | Upkeep |
| CheckVerdict | The conclusive CI rollup for one head sha: success or failure, with the failing run names | Upkeep |
| Round | One budgeted repair attempt on dependabot's own branch. The Nth go at this bump | Upkeep |
| Green | The current head's checks succeeded. Autophage is done with the bump; merging is someone else's | Upkeep |
| Abandonment | Autophage has stopped trying this bump, with the reason and the evidence | Upkeep |

## Contexts

| Context | Subdomain | About |
|---|---|---|
| Resolution | core | Cases, trust, triage, approval, budgeted attempts, outcomes |
| Upkeep | core | Bumps, watches, check verdicts, budgeted repair rounds, abandonment |

Groupings inside Resolution, one language throughout:

| Grouping | Owns |
|---|---|
| Casework | Case, Requester, Triage, Approval, ApprovalDelivery, Transition, Attempt, Run, Outcome, Closure |
| Enrollment | Repository, Removal |

Upkeep owns: Bump, Watch, CheckVerdict, RepairAttempt, RepairOutcome,
BumpAbandonment, BumpClosure, BumpTransition.

Shared kernel, owned by neither and meaning exactly the same thing in both:
Budget, Limit, Usage, FailureClass, Run, Brief. AbortReason is deliberately
not in it: `issue_closed` means nothing in Upkeep and `pull_request_closed`
means nothing in Resolution, so each context keeps its own. Nothing else
crosses. A Bump names a Repository
by full name; a Case and a Bump open on the same repository never reference
each other. See `2026-09-17-autophage-upkeep-domain-model.md` and ADR 0007.

External systems, each behind a port in Resolution's types and one adapter package:

| System | Package | Owns on our side |
|---|---|---|
| GitHub (App, webhooks, REST) | `internal/github` | Delivery store, HMAC verification, translation to Resolution commands, token minting, comments, labels, pull requests |
| podman | `internal/sandbox` | Workspace, prep container, agent container, toolbox dial |
| jess, agentcore, llm | `internal/agent` | Run construction, budget steers, summary turn, outcome translation |
| perch | `cmd/autophaged`, `cmd/autophage` | Daemon and CLI skeleton, loopback auth |

## Relationships

| Upstream | Downstream | Pattern | Notes |
|---|---|---|---|
| GitHub | Resolution | ACL | `internal/github` is the only importer of the GitHub SDK. Deliveries in, commands out to Resolution; comments, labels, PRs and tokens out to GitHub |
| Resolution | podman | Port + adapter | `Sandbox` port in Resolution types; `internal/sandbox` is the only package that shells out to podman |
| Resolution | jess | Port + adapter | `Agent` port in Resolution types; `internal/agent` is the only importer of jess, agentcore and llm |
| jess/ledger | `autophage why` | Conformist | The CLI reads the ledger chain in jess's language by run id |
| perch | autophaged | Conformist | Daemon skeleton as scaffolded from rookery |
| GitHub | Upkeep | ACL | Same adapter. `pull_request`, `check_suite` and `workflow_run` deliveries in, Bump commands out; the check rollup query out to GitHub |
| Resolution | Upkeep | Shared kernel | Budget, Limit, Usage, FailureClass, Run and Brief only. Neither context owns the other's aggregates |
| Upkeep | podman | Port + adapter | Same `internal/sandbox`, plus a checkout that pins an exact head sha instead of rebasing onto the default branch |
| Upkeep | jess | Port + adapter | Same `internal/agent`; a repair round is a budgeted run like any other |

## Ambiguous terms

| Term | GitHub or jess meaning | Our meaning | Resolution |
|---|---|---|---|
| issue | The payload: title, body, labels, author_association, forty fields | A Case: an issue we are handling with a trust verdict | We say Case. "issue number" survives as the identity field |
| run | jess: one Stream call with a run id and a ledger chain | Our Run: the executed part of an Attempt | Our Run references jess's by run id; Attempt is the unit we talk about |
| approved | A label name | An Approval fact | The label is the trigger; the fact is what we store |

| attempt | Resolution: a budgeted try at writing a fix on `autophage/<n>`, a branch we cut | Upkeep: a budgeted round at making dependabot's existing branch go green | Different owners, different tables. We say Round inside Upkeep |
| trust | Resolution: a verdict about the person who opened the issue, deciding whether an attempt needs an approval | Upkeep: a constant. Dependabot is trusted by identity; there is no person | The word does not cross. Upkeep gates on the Watch instead |
| triage | Resolution: the model's Small or Large size verdict | Upkeep: absent. CI answers the equivalent question as a fact, not a judgement | No triage in Upkeep |
| outcome | Resolution: success is `pull_request_opened` | Upkeep: a round's success is `pushed`; the bump's success is `green` | Separate vocabularies, separate tables |

Repository means the same thing on both sides. No split.

## Stored and derived

- Stored: every delivery verbatim; Case facts (triage, approvals, transitions, attempts, runs, outcomes, closure); the trust verdict at receipt; the case state summary; the brief as sent; the budget snapshot per attempt; usage per outcome.
- Stored for a Bump: the branch name, because dependabot named it and
  autophage cannot derive it; the current head sha; every conclusive check
  verdict by head sha; each round's base sha, budget snapshot, brief and
  usage.
- Derived for a Bump, never stored: the round count (count of attempts);
  "is watched" (a Watch row exists); whether a verdict is current (its sha
  equals the bump's head sha).
- Derived, never stored: branch name (`autophage/<number>`); workspace path (from repository and branch); the kind of the next attempt (Approved if an approval postdates the latest attempt's start or no attempt exists, else Auto); "is enrolled" (no Removal row).

## Still open

- Budget numbers for `auto` and `approved`: turns, wall clock, diff lines.
- OpenRouter model ids per tier: triage, auto attempt, approved attempt.
- Label name: `approved` as asked, but a repo already using that label for something else would trigger attempts. `autophage:approved` avoids the collision.
- Workspace retention after Done or Closed.
- A Closed case whose issue is reopened: v1 does nothing.
- Whether Gated cases get any comment: v1 is silent.
- Base image toolchain versions and which registries the prep container may reach.
- Tailnet ACL permits Funnel on <host>: unverified.
- Default concurrency (2).
- Re-enrollment deletes the Removal row rather than appending a second enrollment fact.
- How a Watch is expressed on GitHub. Proposal: a repository topic (`autophage-upkeep`) read on the enrollment sweep. Decides whether the App also subscribes to `repository`.
- Repair rounds and issue attempts share one dispatcher pool of two. Nothing prioritises between them, and a bump cadence nobody controls can starve the Case side.
- The repair round cap, the `awaiting_checks` wait window, and whether a round gets its own budget tier. See the Upkeep model's open list.
