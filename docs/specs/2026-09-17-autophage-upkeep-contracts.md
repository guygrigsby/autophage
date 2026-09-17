# autophage Upkeep contracts

Pass 1, 2026-09-17. Derived from the [Upkeep domain model](2026-09-17-autophage-upkeep-domain-model.md)
and [ADR 0007](../adr/0007-dependabot-bumps-as-an-upkeep-context.md). Extends
[the Resolution contracts](2026-09-04-autophage-contracts.md); everything not
restated here is unchanged. Findings that changed the model are at the end.

## Error taxonomy

Unchanged. The closed set in the Resolution contracts covers every endpoint
below; no new error was needed. `not_found` reads "the bump, repair round or
repository does not exist", `conflict` reads "the Bump refused the transition
in its current state".

## Caller classes

Unchanged: GitHub webhook, Operator, Tailnet scraper. No new class. The
GitHub webhook class gains three events it is trusted to assert verbatim
(`pull_request`, `check_suite`, `workflow_run`) and is still never trusted
to assert anything about our state, including whether a check rollup is
conclusive.

That last point is the whole reason for the rollup query below. A single
`check_suite.completed` says one suite finished, not that the checks are
done, and nothing in the payload says how many suites a head will have.

## API

Rows added to the Resolution table. Rookery's `/healthz`, `/api/auth/mint`
and `/api/whoami` are unchanged.

| Route | Caller | Authn | Authz | Request | Response | Errors | Idempotency | Domain behavior |
|---|---|---|---|---|---|---|---|---|
| `POST /webhook/github` | GitHub webhook | HMAC | Signature valid; `X-GitHub-Event` and `X-GitHub-Delivery` present | Unchanged. Now also delivers `pull_request`, `check_suite` and `workflow_run` | `202 {delivery_id, duplicate: bool}` | `unauthenticated`, `invalid_request`, `internal` | Keyed by delivery id, unique. Unchanged | None directly. Stores a Delivery. The translator later invokes `NewBump`, `Bump.AdvanceHead`, `Bump.RecordVerdict` or `Bump.Close`, per event |
| `GET /api/bumps` | Operator | Bearer | Any operator | Query `state` (optional, one of BumpState), `repository` (optional), `limit` (default 50, max 500), `cursor` (opaque) | `{bumps[{repository, number, branch, state, head_sha, rounds, latest_conclusion or "none", opened_at}], next_cursor}` | `unauthenticated`, `invalid_request` | Read | None |
| `GET /api/bumps/{owner}/{repo}/{number}` | Operator | Bearer | Any operator | Path | The bump: fields, every check verdict, every round with budget, brief, run and outcome variant, abandonment, closure, transitions | `unauthenticated`, `not_found` | Read | None |
| `POST /api/bumps/{owner}/{repo}/{number}/retry` | Operator | Bearer | Any operator | Path; empty body | `202 {repository, number, state}` | `unauthenticated`, `not_found`, `conflict` (state is `awaiting_checks`, `queued`, `repairing` or `closed`) | Not idempotent by design: a second call while `queued` or `repairing` is `conflict` | `Bump.Retry(at)`: clears the abandonment, resets the round cap allowance to one further round, moves `abandoned` or `green` to `queued`. The operator's recourse against sticky abandonment, and the only way one is undone |
| `POST /api/repairs/{id}/stop` | Operator | Bearer | Any operator | Path; empty body | `202 {repair_id, bump: {repository, number}, state}` | `unauthenticated`, `not_found`, `conflict` (round already has an outcome) | Second call is `conflict` | Cancels the run context; the runner records `Bump.RecordOutcome(Aborted{OperatorStop})` after push |
| `GET /api/repairs/{id}` | Operator | Bearer | Any operator | Path | The round: fields, budget, brief, run, outcome with variant and usage | `unauthenticated`, `not_found` | Read | None |
| `GET /api/repairs/{id}/why` | Operator | Bearer | Any operator | Path | The jess ledger chain for the round's run, as `ledger.Chain` renders it | `unauthenticated`, `not_found` (no run recorded) | Read | None. Conformist read of jess/ledger by `bump_repair_runs.run_id` |
| `POST /api/repositories/{owner}/{repo}/watch` | Operator | Bearer | Any operator; the repository must be enrolled | Path; empty body | `200 {repository, watched_at}` | `unauthenticated`, `not_found` (repository not enrolled) | Idempotent. A repeat returns the existing `watched_at` | `NewWatch(repository, at)`, or a no-op when the row exists |
| `POST /api/repositories/{owner}/{repo}/unwatch` | Operator | Bearer | Any operator | Path; empty body | `200 {repository, watched: false}` | `unauthenticated` | Idempotent. Unwatching a repository that is not watched is `200` | Deletes the watch row. Open bumps are left alone; only new ones stop being created |
| `GET /api/status` | Operator | Bearer | Any operator | none | Unchanged fields, plus `bumps_by_state{state: n}`, `repairing[{repair_id, repository, number, round, elapsed_s}]`, `repair_queue_depth`, `watched_repositories` | `unauthenticated` | Read | None. Read model over `bumps`, `bump_repairs`, `upkeep_watches` |
| `GET /metrics` | Tailnet scraper | None | Tailnet reachability | none | Unchanged series, plus `autophage_bumps{state}`, `autophage_repairs_total{outcome}`, `autophage_repair_tokens_total{direction}`, `autophage_repair_wall_clock_seconds` histogram, `autophage_check_verdicts_total{conclusion}`, `autophage_bump_abandonments_total{reason}`, `autophage_repair_queue_depth`, `autophage_watched_repositories` | none | Read | None |

Every caller class reaches at least one endpoint. No endpoint admits a class
not listed.

Both watch endpoints are POST rather than PUT and DELETE. Every other action
in this API is a POST (`/run`, `/stop`), and perch's client speaks GET and
POST only, so the verbs would have been correct and unreachable.

Deliberately absent from the API: starting a repair round. Like an issue
attempt, a round is started by the scheduler and never by the operator, so
the operator cannot jump the queue. `retry` moves a bump to `queued`; the
scheduler still decides when it runs.

### Internal ports

Added to the Resolution port table. Everything else is reused verbatim,
including `MintToken`, `PostComment` and `CommitAndPush`.

| Port | Method | Adapter | Notes |
|---|---|---|---|
| `GitHub` | `GetPullRequest(repository, number) (PullRequestDetail{headBranch, headSha, baseBranch, authorLogin, open bool, merged bool}, error)` | `internal/github` | Wraps `ErrPullRequestNotFound` on a 404. Used on operator `retry` for a bump the daemon never saw, and to refresh the head before a round starts |
| `GitHub` | `CheckRollup(repository, headSha) (Rollup{conclusion, conclusive bool, failingContexts []string, detailsURL string}, error)` | `internal/github` | Combines check runs and commit statuses for the sha. `conclusive` is false while any run is queued or in progress, and the caller records nothing. This is where GitHub's eight conclusions collapse onto our two: success, skipped and neutral are success; failure, timed_out and action_required are failure; cancelled and stale leave the rollup inconclusive |
| `Sandbox` | `PrepareHead(ctx, repository, cloneURL, branch, headSha, token) (Workspace{path, cloneURL, baseSha, remoteHead}, error)` | `internal/sandbox` | Fetch, then check out `headSha` exactly. **No rebase onto the default branch**, unlike `Prepare`: rebasing would change the tree CI just judged, so the round would start from a state no verdict describes. `baseSha` and `remoteHead` are both `headSha`, which is what `CommitAndPush` then leases against |

`Agent.Run` and `Triager` are unchanged. Upkeep does not triage.

## Domain events

Same mechanism as Resolution: the fact rows written in the aggregate's
transaction are the event log, and the transaction issues
`NOTIFY autophage_events, '<kind>:<key>'` as a wake-up. Consumers are
idempotent, act on current state, and boot-scan their input tables so a
missed NOTIFY loses nothing. At-least-once. Per-bump ordering by the row
lock on `bumps` held for every transition. Nothing is published across a
context boundary, so there are no versioning obligations.

| Name | Emitting aggregate | Transition | Payload | Consumers | Delivery | Boundary | Domain service |
|---|---|---|---|---|---|---|---|
| `BumpOpened` | Bump | `[*] -> awaiting_checks` | `bump_id, repository, number, branch, base_branch, head_sha, opened_at` | StaleCheckSweeper (application): starts the wait window. Metrics | at-least-once; boot scan: bumps in `awaiting_checks` | internal | none (`NewBump`) |
| `VerdictRecorded` | Bump | `awaiting_checks -> green`, `-> queued`, or `-> abandoned` (cap reached) | `bump_id, repository, number, head_sha, conclusion, failing_contexts, details_url, concluded_at, to_state` | RepairScheduler when `to_state = queued`. Metrics | at-least-once; boot scan: bumps in `queued` | internal | RepairScheduler |
| `HeadAdvanced` | Bump | `green`, `queued` or `repairing -> awaiting_checks` | `bump_id, repository, number, previous_head_sha, head_sha, source (dependabot or repair; derived, a head we pushed matches a bump_repair_outcome_pushes row), advanced_at, from_state` | RepairRunner: cancels the open round's context when `from_state = repairing` and the source is dependabot. StaleCheckSweeper: restarts the wait window. Metrics | at-least-once; boot scan: bumps in `awaiting_checks` | internal | none (`Bump.AdvanceHead`) |
| `RepairStarted` | Bump | `queued -> repairing` | `repair_id, bump_id, repository, number, round, base_sha, budget{max_turns, max_wall_clock, max_diff_lines}, started_at` | RepairRunner (application): prepare at head, start, run, push, record. Metrics | at-least-once; boot scan: rounds with no outcome are instead ended `Aborted{DaemonRestart}`, or `Aborted{PullRequestClosed}` when the bump is already `closed`. A round never resumes mid-run | internal | none (`Bump.RecordRun`, `Bump.RecordOutcome`) |
| `RepairRunBegan` | Bump | `RecordRun` | `repair_id, run_id, model, base_sha, began_at` | Metrics. `why` reads it later | at-least-once | internal | none |
| `RepairEnded` | Bump | `repairing -> awaiting_checks` (pushed), `-> queued` (infra failure, or first restart), or `-> abandoned` | `repair_id, bump_id, repository, number, outcome{kind, ended_at, usage, summary, variant fields}, from_state, to_state` | RepairScheduler when `to_state = queued`. Sandbox teardown (application). Metrics | at-least-once; boot scan: bumps in `queued`, rounds with no outcome | internal | RepairScheduler for the requeue. "Infra requeues, model and agent abandon" and "a second restart abandons" are `Bump.RecordOutcome`'s own rules |
| `BumpAbandoned` | Bump | `* -> abandoned` | `bump_id, repository, number, reason, detail, abandoned_at, from_state` | Metrics | at-least-once; boot scan: none needed, the row is the state | internal | none (`Bump.Abandon`) |
| `BumpGreen` | Bump | `awaiting_checks -> green` | `bump_id, repository, number, head_sha, at` | Metrics | at-least-once | internal | none |
| `BumpClosed` | Bump | `* -> closed` | `bump_id, repository, number, closure_kind, closed_at, from_state` | RepairRunner: cancels the open round's context when `from_state = repairing`; the run then ends `Aborted{PullRequestClosed}` | at-least-once; boot scan: Recovery reads each open round's bump and ends it `Aborted{PullRequestClosed}` when that bump is `closed` | internal | none (`Bump.Close`, then `Bump.RecordOutcome`) |
| `RepositoryWatched` | Watch | insert | `repository, watched_at` | Metrics. Nothing backfills: a watch does not create bumps for pull requests already open. See the open list | at-least-once | internal | none |
| `RepositoryUnwatched` | Watch | delete | `repository, unwatched_at` | RepairScheduler: starts no further round for the repository. An open round finishes | at-least-once | internal | RepairScheduler |

`BumpGreen` and `VerdictRecorded` overlap deliberately: the first is the one
a future consumer (a comment, a merge policy) would subscribe to, the second
is what the scheduler reads. Recorded rather than merged so the split stays
visible if either grows a consumer.

Domain services named above:

- **RepairScheduler**: owns the rules that span bumps. Queued bumps start in
  order of the time they became queued (latest `bump_transitions` row with
  `to_state = 'queued'`); no round starts for a repository with no watch
  row; no round starts once the bump has reached the round cap; no round
  starts for a removed repository.

The shared concurrency limit is **not** a domain rule of either context: at
most `sandbox.concurrency` agent runs exist at once across issue attempts
and repair rounds together, because there is one sandbox pool. That is the
Dispatcher's, an application service arbitrating a shared resource. Nothing
currently prioritises between the two, which ADR 0007 records as the
operational risk.

Application services, for the record and owning no rule: Translator,
RepairRunner, RepairScheduler's caller the Dispatcher, StaleCheckSweeper,
Recovery.

## DDL

Postgres 17, same conventions: every closed vocabulary is a seeded table and
an FK target, timestamps are `TIMESTAMPTZ` with database defaults, surrogate
keys only where the natural key is compound and widely referenced or absent.

### Vocabularies

```sql
CREATE TABLE bump_states (state TEXT PRIMARY KEY);
INSERT INTO bump_states VALUES ('awaiting_checks'), ('queued'), ('repairing'), ('green'), ('abandoned'), ('closed');

CREATE TABLE check_conclusions (conclusion TEXT PRIMARY KEY);
INSERT INTO check_conclusions VALUES ('success'), ('failure');

CREATE TABLE repair_outcome_kinds (kind TEXT PRIMARY KEY);
INSERT INTO repair_outcome_kinds VALUES ('pushed'), ('no_change'), ('budget_exhausted'), ('failed'), ('aborted');

CREATE TABLE repair_abort_reasons (reason TEXT PRIMARY KEY);
INSERT INTO repair_abort_reasons VALUES ('operator_stop'), ('daemon_restart'), ('pull_request_closed');
  -- Upkeep's own, not Resolution's abort_reasons: 'issue_closed' has no meaning here and
  -- 'pull_request_closed' has none there.

CREATE TABLE abandon_reasons (reason TEXT PRIMARY KEY);
INSERT INTO abandon_reasons VALUES
  ('rounds_exhausted'), ('budget_exhausted'), ('repair_failed'),
  ('no_change'), ('checks_never_concluded'), ('operator_stop'), ('restart_failed');

CREATE TABLE closure_kinds (kind TEXT PRIMARY KEY);
INSERT INTO closure_kinds VALUES ('merged'), ('discarded');

CREATE TABLE bump_transition_causes (cause TEXT PRIMARY KEY);
INSERT INTO bump_transition_causes VALUES
  ('verdict_success'), ('verdict_failure'), ('verdict_failure_cap_reached'),
  ('checks_never_concluded'), ('repair_started'), ('repair_pushed'), ('repair_no_change'),
  ('repair_budget_exhausted'), ('repair_failed_infra_requeued'), ('repair_requeue_capped'),
  ('repair_failed_abandoned'),
  ('repair_aborted_operator_stop'), ('repair_aborted_restart_requeued'),
  ('repair_aborted_restart_abandoned'), ('head_advanced'), ('operator_retry'), ('closed');
```

`budget_limits` and `failure_classes` are reused from the Resolution
migration: they are shared kernel and mean exactly the same thing here.

### Upkeep

```sql
-- Owning aggregate: Watch. Exists exactly while Upkeep acts on the repository's bumps.
-- Enrollment authorises issues; this authorises bumps. Deleting it stops new bumps only.
CREATE TABLE upkeep_watches (
  repository TEXT        PRIMARY KEY REFERENCES repositories(full_name) ON DELETE CASCADE,
  watched_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owning aggregate: Bump.
CREATE TABLE bumps (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
      -- surrogate: the natural key (repository, number) is compound and referenced by six tables
  repository  TEXT        NOT NULL REFERENCES repositories(full_name) ON DELETE RESTRICT,
      -- not a reference to upkeep_watches: unwatching must not delete the bumps already open
  number      INTEGER     NOT NULL CHECK (number >= 1),
  branch      TEXT        NOT NULL CHECK (branch <> ''),
      -- dependabot named it; unlike a Case's autophage/<n>, it cannot be derived
  base_branch TEXT        NOT NULL CHECK (base_branch <> ''),
  head_sha    TEXT        NOT NULL CHECK (head_sha ~ '^[0-9a-f]{40}$'),
      -- the current tip. Mutated by AdvanceHead, which is also what invalidates stale verdicts
  state       TEXT        NOT NULL REFERENCES bump_states(state),
      -- equals the latest bump_transitions.to_state; stored for the index below (recorded exception)
  opened_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (repository, number)
);
CREATE INDEX bumps_by_state ON bumps (state, opened_at);
  -- serves: GET /api/bumps?state=, status counts, the RepairScheduler (queued),
  -- the StaleCheckSweeper (awaiting_checks)
-- "the repository has a watch row at construction" and "the author is the dependabot login"
-- are store-enforced in NewBump, not structural: a watch may be deleted afterwards and the
-- bump must survive it (recorded exception).

-- Owning aggregate: Bump. Append-only log of state changes. The initial state is not logged;
-- it is awaiting_checks at opened_at.
CREATE TABLE bump_transitions (
  id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  bump_id     UUID        NOT NULL REFERENCES bumps(id) ON DELETE CASCADE,
  from_state  TEXT        NOT NULL REFERENCES bump_states(state),
  to_state    TEXT        NOT NULL REFERENCES bump_states(state),
  cause       TEXT        NOT NULL REFERENCES bump_transition_causes(cause),
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (from_state <> to_state)
);
CREATE INDEX bump_transitions_by_bump ON bump_transitions (bump_id, id);
  -- serves: bump detail history, latest transition per bump
CREATE INDEX bump_transitions_queued ON bump_transitions (occurred_at) WHERE to_state = 'queued';
  -- serves: RepairScheduler FIFO order
CREATE INDEX bump_transitions_awaiting ON bump_transitions (occurred_at) WHERE to_state = 'awaiting_checks';
  -- serves: StaleCheckSweeper, which needs when the current wait window started

-- Owning aggregate: Bump. Exists exactly when CI reached a conclusive rollup for one head sha.
-- A pending rollup records nothing; the absence has a deadline, which is why there is no
-- 'pending' conclusion.
CREATE TABLE bump_check_verdicts (
  id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  bump_id          UUID        NOT NULL REFERENCES bumps(id) ON DELETE CASCADE,
  head_sha         TEXT        NOT NULL CHECK (head_sha ~ '^[0-9a-f]{40}$'),
  conclusion       TEXT        NOT NULL REFERENCES check_conclusions(conclusion),
  failing_contexts TEXT        NOT NULL,
      -- newline-separated run names, verbatim from GitHub; empty exactly when conclusion = 'success'
  details_url      TEXT        NOT NULL,   -- empty when GitHub gave none; failing_contexts is the evidence
  concluded_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (bump_id, head_sha),
  CHECK ((conclusion = 'success') = (failing_contexts = ''))
);
-- "a verdict's head_sha must equal bumps.head_sha at insert" is the loop guard and is
-- store-enforced under the bumps row lock (recorded here). A CHECK cannot reach another table.

-- Owning aggregate: Bump (owned entity RepairAttempt).
CREATE TABLE bump_repairs (
  id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  bump_id        UUID        NOT NULL REFERENCES bumps(id) ON DELETE CASCADE,
  round          INTEGER     NOT NULL CHECK (round >= 1),
  base_sha       TEXT        NOT NULL CHECK (base_sha ~ '^[0-9a-f]{40}$'),
  max_turns      INTEGER     NOT NULL CHECK (max_turns > 0),
  max_wall_clock INTERVAL    NOT NULL CHECK (max_wall_clock > INTERVAL '0'),
  max_diff_lines INTEGER     NOT NULL CHECK (max_diff_lines > 0),
  brief          TEXT        NOT NULL CHECK (brief <> ''),
  started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (bump_id, round)
);
CREATE INDEX bump_repairs_by_bump ON bump_repairs (bump_id, round);
  -- serves: bump detail, latest round, round allocation, the cap check
-- "at most one round without an outcome per bump" spans bump_repair_outcomes: enforced in
-- StartRepair's transaction under the bumps row lock (recorded here).
-- "round <= the configured cap" reads config, so it is store-enforced too (recorded here).

-- Owning aggregate: Bump. Exists exactly when the agent began executing.
CREATE TABLE bump_repair_runs (
  repair_id UUID        PRIMARY KEY REFERENCES bump_repairs(id) ON DELETE CASCADE,
  run_id    TEXT        NOT NULL UNIQUE,   -- jess ledger run id; logical reference, no FK across modules
  model     TEXT        NOT NULL CHECK (model <> ''),
  base_sha  TEXT        NOT NULL CHECK (base_sha ~ '^[0-9a-f]{40}$'),
  began_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owning aggregate: Bump. Exists exactly when the round ended. Common fields; the variant is its own table.
CREATE TABLE bump_repair_outcomes (
  repair_id     UUID        PRIMARY KEY REFERENCES bump_repairs(id) ON DELETE CASCADE,
  kind          TEXT        NOT NULL REFERENCES repair_outcome_kinds(kind),
  ended_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  turns         INTEGER     NOT NULL CHECK (turns >= 0),
  input_tokens  BIGINT      NOT NULL CHECK (input_tokens >= 0),
  output_tokens BIGINT      NOT NULL CHECK (output_tokens >= 0),
  wall_clock    INTERVAL    NOT NULL CHECK (wall_clock >= INTERVAL '0'),
  diff_lines    INTEGER     NOT NULL CHECK (diff_lines >= 0),
  summary       TEXT        NOT NULL CHECK (summary <> ''),
  UNIQUE (repair_id, kind)   -- exists only to let each variant table pin its kind by composite FK
);
CREATE INDEX bump_repair_outcomes_by_kind ON bump_repair_outcomes (kind, ended_at);
  -- serves: metrics
-- 'no_change' has no variant table: the kind carries all of its meaning and the summary is
-- the evidence (recorded here, so a reader does not go looking for one).

CREATE TABLE bump_repair_outcome_pushes (
  repair_id UUID PRIMARY KEY,
  kind      TEXT NOT NULL CHECK (kind = 'pushed'),
  head_sha  TEXT NOT NULL CHECK (head_sha ~ '^[0-9a-f]{40}$'),
  FOREIGN KEY (repair_id, kind) REFERENCES bump_repair_outcomes(repair_id, kind) ON DELETE CASCADE
);

CREATE TABLE bump_repair_outcome_exhaustions (
  repair_id  UUID PRIMARY KEY,
  kind       TEXT NOT NULL CHECK (kind = 'budget_exhausted'),
  limit_name TEXT NOT NULL REFERENCES budget_limits(limit_name),
  FOREIGN KEY (repair_id, kind) REFERENCES bump_repair_outcomes(repair_id, kind) ON DELETE CASCADE
);

CREATE TABLE bump_repair_outcome_failures (
  repair_id UUID PRIMARY KEY,
  kind      TEXT NOT NULL CHECK (kind = 'failed'),
  class     TEXT NOT NULL REFERENCES failure_classes(class),
  message   TEXT NOT NULL CHECK (message <> ''),
  FOREIGN KEY (repair_id, kind) REFERENCES bump_repair_outcomes(repair_id, kind) ON DELETE CASCADE
);
-- class drives the transition: 'infra' requeues the bump, 'model' and 'agent' abandon it.
-- Store-enforced in RecordOutcome (recorded here).

CREATE TABLE bump_repair_outcome_aborts (
  repair_id UUID PRIMARY KEY,
  kind      TEXT NOT NULL CHECK (kind = 'aborted'),
  reason    TEXT NOT NULL REFERENCES repair_abort_reasons(reason),
  FOREIGN KEY (repair_id, kind) REFERENCES bump_repair_outcomes(repair_id, kind) ON DELETE CASCADE
);

-- Owning aggregate: Bump. Exists exactly when autophage has stopped trying.
-- Not derivable from the last outcome: 'checks_never_concluded' abandons a bump with no round.
-- Deleted by operator retry (recorded: the second place a fact row is deleted rather than appended).
CREATE TABLE bump_abandonments (
  bump_id      UUID        PRIMARY KEY REFERENCES bumps(id) ON DELETE CASCADE,
  reason       TEXT        NOT NULL REFERENCES abandon_reasons(reason),
  detail       TEXT        NOT NULL CHECK (detail <> ''),   -- the failing contexts, limit, or message
  abandoned_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owning aggregate: Bump. Exists exactly when the pull request closed on GitHub. Terminal.
CREATE TABLE bump_closures (
  bump_id     UUID        PRIMARY KEY REFERENCES bumps(id) ON DELETE CASCADE,
  kind        TEXT        NOT NULL REFERENCES closure_kinds(kind),
  delivery_id TEXT        NOT NULL UNIQUE REFERENCES webhook_deliveries(delivery_id) ON DELETE RESTRICT,
  closed_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Nullable columns: none. Every column reads alone.

## Cross-check

Every Bump transition through the three contracts.

| Transition | API | Event | DDL |
|---|---|---|---|
| `[*] -> awaiting_checks` | `POST /webhook/github` (pull_request.opened / .reopened) | `BumpOpened` | `bumps` insert; UNIQUE (repository, number); watch check store-enforced |
| `awaiting_checks -> green` | `POST /webhook/github` (check_suite / workflow_run .completed, then the rollup query) | `VerdictRecorded`, `BumpGreen` | `bump_check_verdicts` insert, `bump_transitions`, `bumps.state` |
| `awaiting_checks -> queued` | same | `VerdictRecorded` | same, `to_state = queued` |
| `awaiting_checks -> abandoned` (cap reached) | same | `VerdictRecorded`, `BumpAbandoned` | verdict insert + `bump_abandonments(rounds_exhausted)` + transition |
| `awaiting_checks -> abandoned` (window elapsed) | none: StaleCheckSweeper (deliberate: a deadline is not an operator action) | `BumpAbandoned` | `bump_abandonments(checks_never_concluded)` + transition. No verdict row, which is the point |
| `queued -> repairing` | none: RepairScheduler (deliberate: the operator cannot jump the queue) | `RepairStarted` | `bump_repairs` insert, transition, `bumps.state`; one-open-round and cap store-enforced |
| `RecordRun` | none | `RepairRunBegan` | `bump_repair_runs` insert |
| `repairing -> awaiting_checks` (pushed) | none: runner | `RepairEnded`, `HeadAdvanced` | `bump_repair_outcomes` + `bump_repair_outcome_pushes`, `bumps.head_sha` update, transition |
| `repairing -> abandoned` (no_change) | none: runner | `RepairEnded`, `BumpAbandoned` | outcome row with no variant table + `bump_abandonments(no_change)` + transition |
| `repairing -> abandoned` (exhausted) | none: runner | `RepairEnded`, `BumpAbandoned` | outcome + `bump_repair_outcome_exhaustions` + `bump_abandonments(budget_exhausted)` + transition |
| `repairing -> queued` (infra failure) | none: runner | `RepairEnded` | outcome + `bump_repair_outcome_failures(infra)` + transition |
| `repairing -> abandoned` (model or agent failure) | none: runner | `RepairEnded`, `BumpAbandoned` | outcome + `bump_repair_outcome_failures` + `bump_abandonments(repair_failed)` + transition |
| `repairing -> abandoned` (operator stop) | `POST /api/repairs/{id}/stop` | `RepairEnded`, `BumpAbandoned` | outcome + `bump_repair_outcome_aborts(operator_stop)` + `bump_abandonments(operator_stop)` + transition |
| `repairing -> queued` (first restart) | none: Recovery on boot | `RepairEnded` | outcome + `bump_repair_outcome_aborts(daemon_restart)` + transition |
| `repairing -> abandoned` (second restart) | none: Recovery on boot | `RepairEnded`, `BumpAbandoned` | same + `bump_abandonments(restart_failed)` + transition |
| `green` / `queued` / `repairing -> awaiting_checks` (force-push) | `POST /webhook/github` (pull_request.synchronize) | `HeadAdvanced` | `bumps.head_sha` update + transition. Stale verdicts stay on file and stop being current |
| `abandoned` / `green -> queued` (operator) | `POST /api/bumps/.../retry` | `VerdictRecorded` is not re-emitted; the transition carries `operator_retry` | `bump_abandonments` delete + transition |
| `* -> closed` | `POST /webhook/github` (pull_request.closed) | `BumpClosed` | `bump_closures` insert + transition + `bumps.state` |

Deliberately absent from the event contract: nothing. Deliberately absent
from the API: every transition the scheduler or the runner owns, listed
above as "none" with its reason.

Two refusals the translator must record as `ignored`, not `rejected`. A
verdict arriving while the bump is `queued` or `repairing` is refused by the
state machine, and so is one whose sha is no longer the head. Both are
ordinary: the first is CI finishing on a head a round is already working,
the second is a force-push winning a race. Recording either as a rejected
processing row would repeat `autophage-2lq`, where the single most normal
end to a case showed up in the delivery ledger as a rejection.

## Findings that changed the model

- **An infra failure was abandoning the bump.** The first pass sent every
  `failed` outcome to `abandoned`. Writing the DDL put `failure_classes`
  next to `abandon_reasons` and made it obvious that a podman hiccup and a
  model that cannot fix the code were being recorded as the same verdict.
  `infra` now requeues; the round is still spent, so the cap bounds it.
- **`AbortReason` could not be shared.** The kernel list had it until the
  vocabulary tables were written out: `issue_closed` is invalid for a bump
  and `pull_request_closed` is invalid for a case, so one shared table would
  have rows that are wrong on one side. Upkeep now owns
  `repair_abort_reasons`.
- **Sticky abandonment had no escape hatch.** Enumerating the API showed
  every terminalish state reachable and nothing that undid one, so a bump
  abandoned by a transient problem was dead for ever.
  `POST /api/bumps/.../retry` exists because the cross-check found the gap,
  and it is the second place in the system that deletes a fact row rather
  than appending, which the DDL records.
- **The wait-window sweep had no index.** `bump_transitions_awaiting` was
  added once the StaleCheckSweeper's query was written down: it needs when
  the *current* awaiting window started, which is the latest transition, not
  `opened_at`.

## Open, not assumed

Carried from the domain model, plus what this pass raised:

- The round cap (placeholder 3), the `awaiting_checks` wait window
  (placeholder 6h), and whether a repair round gets its own budget tier.
- How a Watch is created besides the operator endpoint. A repository topic
  (`autophage-upkeep`) read on the enrollment sweep would make it
  self-service, and would decide whether the App also subscribes to
  `repository`.
- Whether watching a repository backfills the dependabot pull requests
  already open on it. Today it does not, so a newly watched repository picks
  up only the next bump, which is the quiet option and possibly the wrong
  one.
- Whether a push or an abandonment says anything on the pull request.
- Whether the operator `retry` grants one further round or resets the count
  to zero. One is written above; nobody decided.
