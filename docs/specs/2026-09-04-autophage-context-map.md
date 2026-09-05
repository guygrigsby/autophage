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

## Contexts

| Context | Subdomain | About |
|---|---|---|
| Resolution | core | Cases, trust, triage, approval, budgeted attempts, outcomes |

Groupings inside Resolution, one language throughout:

| Grouping | Owns |
|---|---|
| Casework | Case, Requester, Triage, Approval, ApprovalDelivery, Transition, Attempt, Run, Outcome, Closure |
| Enrollment | Repository, Removal |

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

## Ambiguous terms

| Term | GitHub or jess meaning | Our meaning | Resolution |
|---|---|---|---|
| issue | The payload: title, body, labels, author_association, forty fields | A Case: an issue we are handling with a trust verdict | We say Case. "issue number" survives as the identity field |
| run | jess: one Stream call with a run id and a ledger chain | Our Run: the executed part of an Attempt | Our Run references jess's by run id; Attempt is the unit we talk about |
| approved | A label name | An Approval fact | The label is the trigger; the fact is what we store |

Repository means the same thing on both sides. No split.

## Stored and derived

- Stored: every delivery verbatim; Case facts (triage, approvals, transitions, attempts, runs, outcomes, closure); the trust verdict at receipt; the case state summary; the brief as sent; the budget snapshot per attempt; usage per outcome.
- Derived, never stored: branch name (`autophage/<number>`); workspace path (from repository and branch); the kind of the next attempt (Approved if an approval postdates the latest attempt's start or no attempt exists, else Auto); "is enrolled" (no Removal row).

## Still open

- Budget numbers for `auto` and `approved`: turns, wall clock, diff lines.
- OpenRouter model ids per tier: triage, auto attempt, approved attempt.
- Label name: `approved` as asked, but a repo already using that label for something else would trigger attempts. `autophage:approved` avoids the collision.
- Workspace retention after Done or Closed.
- A Closed case whose issue is reopened: v1 does nothing.
- Whether Gated cases get any comment: v1 is silent.
- Base image toolchain versions and which registries the prep container may reach.
- Tailnet ACL permits Funnel on trig: unverified.
- Default concurrency (2).
- Re-enrollment deletes the Removal row rather than appending a second enrollment fact.
