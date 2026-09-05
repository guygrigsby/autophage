# autophage contracts

Pass 1, 2026-09-04. Derived from the [domain model](2026-09-04-autophage-domain-model.md). Findings that changed the model are listed at the end.

## Error taxonomy

One closed set, chosen from by every endpoint.

| Error | HTTP | Means |
|---|---|---|
| `unauthenticated` | 401 | Missing or invalid credential for the caller class |
| `invalid_request` | 400 | Malformed body, missing header, unknown field, bad value |
| `not_found` | 404 | The case, attempt, repository or upstream issue does not exist |
| `conflict` | 409 | The aggregate refused the transition in its current state |
| `upstream_unavailable` | 502 | GitHub could not be reached or answered with an error during a synchronous call |
| `internal` | 500 | Anything else. Logged with a request id |

## Caller classes

| Class | Authenticates by | Trusted to assert | Never trusted to assert |
|---|---|---|---|
| GitHub webhook | HMAC-SHA256 of the raw body with the App webhook secret, `X-Hub-Signature-256` | The delivery id, event, action, sender and payload exactly as GitHub sent them, including `author_association` | Anything about our state. That a sender is the operator. That a label add came from someone with permission (GitHub already enforces that; we record the association as evidence) |
| Operator | perch loopback bearer token, minted by `POST /api/auth/mint` from 127.0.0.1 only | Operator intent: run, stop, read anything | Nothing further; single operator |
| Tailnet scraper | None. Reachable only on the tailnet via `tailscale serve`, never through Funnel | Nothing | Nothing. Read-only metrics |

Funnel exposes exactly one path publicly, `POST /webhook/github`. `tailscale serve` exposes `/metrics` to the tailnet. Everything under `/api` is loopback only.

## API

Rookery already provides `GET /healthz`, `POST /api/auth/mint` (loopback) and `GET /api/whoami` (bearer). autophage adds the rows below.

| Route | Caller | Authn | Authz | Request | Response | Errors | Idempotency | Domain behavior |
|---|---|---|---|---|---|---|---|---|
| `POST /webhook/github` | GitHub webhook | HMAC | Signature valid; `X-GitHub-Event` and `X-GitHub-Delivery` present | Headers `X-GitHub-Delivery`, `X-GitHub-Event`, `X-Hub-Signature-256`; body raw JSON, verified byte for byte | `202 {delivery_id, duplicate: bool}` | `unauthenticated`, `invalid_request`, `internal` | Keyed by delivery id, unique. A redelivery returns 202 with `duplicate: true` and is not stored twice | None directly. Stores a Delivery. The translator later invokes `NewCase`, `Case.RecordApproval`, `Case.Close`, `Repository` enroll or remove, per event |
| `GET /api/status` | Operator | Bearer | Any operator | none | `{version, uptime_s, cases_by_state{state: n}, attempting[{attempt_id, repository, number, ordinal, elapsed_s}], queue_depth, concurrency, pending_deliveries, last_delivery_at}` | `unauthenticated` | Read | None. Read model over `cases`, `attempts` and `webhook_deliveries` |
| `GET /api/cases` | Operator | Bearer | Any operator | Query `state` (optional, one of CaseState), `repository` (optional), `limit` (default 50, max 500), `cursor` (opaque) | `{cases[{repository, number, state, requester_login, requester_trust, received_at, latest_outcome_kind or "none"}], next_cursor}` | `unauthenticated`, `invalid_request` | Read | None |
| `GET /api/cases/{owner}/{repo}/{number}` | Operator | Bearer | Any operator | Path | The case: fields, triage, approvals, transitions, attempts each with budget, brief, run, outcome and variant, closure | `unauthenticated`, `not_found` | Read | None |
| `POST /api/cases/{owner}/{repo}/{number}/run` | Operator | Bearer | Any operator | Path; empty body | `202 {repository, number, state}` | `unauthenticated`, `not_found` (repository not enrolled, or issue absent on GitHub), `conflict` (state is Queued, Attempting, Done or Closed), `upstream_unavailable` | Not idempotent by design: a second call while Queued or Attempting is `conflict` | If no case exists: `GitHub.GetIssue` then `NewCase`. Then `Case.RecordApproval` with source `operator`, approver = the App owner's login, trust Trusted. Received goes through triage as usual; Gated, AwaitingApproval and Failed go to Queued |
| `POST /api/attempts/{id}/stop` | Operator | Bearer | Any operator | Path; empty body | `202 {attempt_id, case: {repository, number}, state}` | `unauthenticated`, `not_found`, `conflict` (attempt already has an outcome) | Second call is `conflict` | Cancels the run context; the runner records `Case.RecordOutcome(Aborted{OperatorStop})` after push |
| `GET /api/attempts/{id}` | Operator | Bearer | Any operator | Path | The attempt: fields, budget, brief, run, outcome with variant and usage | `unauthenticated`, `not_found` | Read | None |
| `GET /api/attempts/{id}/why` | Operator | Bearer | Any operator | Path | The jess ledger chain for the attempt's run: request, available context, actions with gate verdicts, in the shape `ledger.Chain` renders | `unauthenticated`, `not_found` (no run recorded for the attempt) | Read | None. Conformist read of jess/ledger by `attempt_runs.run_id` |
| `GET /metrics` | Tailnet scraper | None | Tailnet reachability | none | Prometheus text: `autophage_cases{state}`, `autophage_attempts_total{kind, outcome}`, `autophage_attempt_tokens_total{direction}`, `autophage_attempt_wall_clock_seconds` histogram, `autophage_queue_depth`, `autophage_deliveries_total{event, result}` | none | Read | None |

Every caller class reaches at least one endpoint. No endpoint admits a class not listed.

### Internal ports

Not endpoints, but the seams the adapters implement. All in Resolution's types.

| Port | Method | Adapter | Notes |
|---|---|---|---|
| `GitHub` | `VerifyAndStore(headers, body) (Delivery, duplicate bool, error)` | `internal/github` | HMAC then insert; the endpoint handler is a thin wrapper |
| `GitHub` | `GetIssue(repository, number) (title, body, Requester, error)` | | For operator `run` on an issue with no case, and to refresh title and body at attempt start |
| `GitHub` | `MintToken(repository) (token, expiresAt, error)` | | App JWT to installation token scoped to the one repository |
| `GitHub` | `PostComment(repository, number, body) (githubCommentID, error)` | | Honors `Retry-After`; retried by the outbox poster |
| `GitHub` | `OpenPullRequest(repository, head, base, title, body) (prNumber, error)` | | Body carries `Fixes #<number>` |
| `GitHub` | `EnsureLabel(repository, name) error` | | Idempotent on GitHub's side |
| `Sandbox` | `Prepare(repository, branch, defaultBranch, token) (Workspace{path, baseSha}, error)` | `internal/sandbox` | Clone or fetch, rebase branch onto default, warm deps in the prep container |
| `Sandbox` | `Start(workspace) (Container, error)` | | Agent container, no network, hardened |
| `Sandbox` | `Tools(container) ([]Tool, closer, error)` | | Dial the toolbox over `podman exec -i`, adapt via `jess/mcp` |
| `Sandbox` | `DiffLines(container, baseSha) (int, error)` | | `git diff --shortstat` inside the container |
| `Sandbox` | `CommitAndPush(workspace, token, message) (headSha, dirty bool, error)` | | Host side; token as a per-command header |
| `Sandbox` | `Teardown(container) error` | | |
| `Agent` | `Run(ctx, brief, tools, budget, hooks) (RunReport{runId, usage, finalSummary, stop Limit or none}, error)` | `internal/agent` | Builds one jess agent; wires steers at `Budget.WarnAt()`; forces the summary turn; `hooks.AfterTool` lets the runner check `DiffLines` |
| `Triager` | `Classify(title, body) (Size, rationale, model, error)` | `internal/agent` | Structured output; one model call on the triage tier |

## Domain events

All events are internal to the daemon. Delivery: the fact rows written in the aggregate's transaction are the event log; the transaction also issues `NOTIFY autophage_events, '<kind>:<key>'` as a wake-up. Consumers are idempotent, act on current state read from the tables, and on boot scan their input tables so a missed NOTIFY loses nothing. At-least-once. Per-case ordering by the row lock on `cases` held for every transition. Nothing is published across a context boundary; there are no versioning obligations.

| Name | Emitting aggregate | Transition | Payload | Consumers | Delivery | Boundary | Domain service |
|---|---|---|---|---|---|---|---|
| `DeliveryStored` | Delivery (GitHub boundary record, not an aggregate) | insert | `delivery_id, event, action, sender_login, received_at` | Translator (application): verifies subscription, maps to a command, records a Processing | at-least-once; boot scan: deliveries with no processing row | internal | none. Reaction is a command on one aggregate: `NewCase`, `RecordApproval`, `Close`, or Repository enroll or remove |
| `CaseReceived` | Case | `[*] -> Received` | `case_id, repository, number, requester{login, association, trust}, received_at` | Triage service (application): `Triager.Classify` then `Case.RecordTriage` | at-least-once; boot scan: cases in Received with no triage row | internal | none (`Case.RecordTriage`) |
| `CaseGated` | Case | `[*] -> Gated` | same as CaseReceived | Metrics only | at-least-once | internal | none |
| `CaseTriaged` | Case | `Received -> Queued` or `Received -> AwaitingApproval` | `case_id, repository, number, size, rationale, model, triaged_at, to_state` | Scheduler when `to_state = queued`. Commenter when `awaiting_approval`: enqueues the rationale comment (`case_triage_comments`) | at-least-once; boot scans: Queued cases; triages with no comment link | internal | Scheduler |
| `CaseApproved` | Case | `RecordApproval`; transition to Queued from Gated, AwaitingApproval or Failed, else none | `case_id, repository, number, approval_id, approver{login, association, trust}, source, approved_at, from_state, to_state` | Scheduler when `to_state = queued` | at-least-once; boot scan: Queued cases | internal | Scheduler |
| `AttemptStarted` | Case | `Queued -> Attempting` | `attempt_id, case_id, repository, number, ordinal, kind, budget{max_turns, max_wall_clock, max_diff_lines}, started_at` | AttemptRunner (application): prepare, start, run, push, record. Metrics | at-least-once; boot scan: attempts with no outcome are instead ended `Aborted{DaemonRestart}` (an attempt never resumes mid-run) | internal | none (`Case.RecordRun`, `Case.RecordOutcome`) |
| `RunBegan` | Case | `RecordRun` | `attempt_id, run_id, model, base_sha, began_at` | Metrics. `why` reads it later | at-least-once | internal | none |
| `AttemptEnded` | Case | `Attempting -> Done`, `-> AwaitingApproval`, `-> Failed`, `-> Queued` (restart requeue), or no transition when already Closed | `attempt_id, case_id, repository, number, outcome{kind, ended_at, usage, summary, variant fields}, from_state, to_state` | Commenter for BudgetExhausted, Failed and Aborted{OperatorStop}: enqueues the summary comment (`attempt_outcome_comments`). Scheduler when `to_state = queued`. Sandbox teardown (application). Metrics | at-least-once; boot scans: outcomes of those kinds with no comment link; Queued cases | internal | Scheduler for the requeue. The "requeue once" rule is `Case.RecordOutcome`'s own |
| `CaseClosed` | Case | `-> Closed` | `case_id, repository, number, closed_at, from_state` | AttemptRunner: cancels the open attempt's context when `from_state = attempting`; the run then ends `Aborted{IssueClosed}` | at-least-once; boot scan: Closed cases with an open attempt are ended `Aborted{IssueClosed}` | internal | none (`Case.Close`, then `Case.RecordOutcome`) |
| `RepositoryEnrolled` | Repository | insert, or Removal row deleted | `full_name, installation_id, default_branch, enrolled_at` | LabelSetup (application): `GitHub.EnsureLabel` then records `repository_label_setups` | at-least-once; boot scan: enrolled repositories with no label setup row | internal | none |
| `RepositoryRemoved` | Repository | Removal row inserted | `full_name, removed_at` | Scheduler: never starts an attempt for a removed repository. A running attempt fails at the next GitHub call with `Failed{Infra}` | at-least-once | internal | Scheduler |

Domain services named above:

- **Scheduler**: owns the rules that span cases. At most `concurrency` attempts open at once across all cases; Queued cases start in order of the time they became Queued (latest `case_transitions` row with `to_state = queued`); no attempt starts for a removed repository. Invoked by the dispatcher on every wake and on boot.

Application services, for the record and not counting as owners of any rule: Translator, Triage, AttemptRunner, Commenter (outbox poster), LabelSetup, Dispatcher.

## DDL

Postgres 17. Every closed vocabulary is a seeded table and an FK target. Timestamps are `TIMESTAMPTZ` with database defaults. Surrogate keys are database-generated and used only where the natural key is compound and widely referenced (`cases`) or absent (event rows). jess's ledger tables live in the same database and are owned by `jess/ledger`; `attempt_runs.run_id` refers to them logically with no FK across the module boundary (recorded exception).

### Vocabularies

```sql
CREATE TABLE case_states        (state TEXT PRIMARY KEY);
INSERT INTO case_states VALUES ('received'), ('gated'), ('queued'), ('attempting'), ('awaiting_approval'), ('failed'), ('done'), ('closed');

CREATE TABLE trusts             (trust TEXT PRIMARY KEY);
INSERT INTO trusts VALUES ('trusted'), ('untrusted');

CREATE TABLE associations       (association TEXT PRIMARY KEY);
INSERT INTO associations VALUES ('owner'), ('member'), ('collaborator'), ('contributor'), ('first_time_contributor'), ('first_timer'), ('mannequin'), ('none');

CREATE TABLE sizes              (size TEXT PRIMARY KEY);
INSERT INTO sizes VALUES ('small'), ('large');

CREATE TABLE attempt_kinds      (kind TEXT PRIMARY KEY);
INSERT INTO attempt_kinds VALUES ('auto'), ('approved');

CREATE TABLE outcome_kinds      (kind TEXT PRIMARY KEY);
INSERT INTO outcome_kinds VALUES ('pull_request_opened'), ('budget_exhausted'), ('failed'), ('aborted');

CREATE TABLE budget_limits      (limit_name TEXT PRIMARY KEY);
INSERT INTO budget_limits VALUES ('turns'), ('wall_clock'), ('diff_lines');

CREATE TABLE failure_classes    (class TEXT PRIMARY KEY);
INSERT INTO failure_classes VALUES ('infra'), ('model'), ('agent');

CREATE TABLE abort_reasons      (reason TEXT PRIMARY KEY);
INSERT INTO abort_reasons VALUES ('issue_closed'), ('operator_stop'), ('daemon_restart');

CREATE TABLE approval_sources   (source TEXT PRIMARY KEY);
INSERT INTO approval_sources VALUES ('label'), ('operator');

CREATE TABLE transition_causes  (cause TEXT PRIMARY KEY);
INSERT INTO transition_causes VALUES
  ('triage_small'), ('triage_large'), ('approval'), ('attempt_started'),
  ('outcome_pull_request_opened'), ('outcome_budget_exhausted'), ('outcome_failed'),
  ('outcome_aborted_operator_stop'), ('outcome_aborted_daemon_restart_requeued'),
  ('outcome_aborted_daemon_restart_failed'), ('closed');

CREATE TABLE processing_results (result TEXT PRIMARY KEY);
INSERT INTO processing_results VALUES ('translated'), ('ignored'), ('rejected');
```

### Enrollment

```sql
-- Owning aggregate: Repository.
CREATE TABLE repositories (
  full_name       TEXT        PRIMARY KEY,           -- natural key, owner/name
  installation_id BIGINT      NOT NULL,              -- opaque GitHub reference used to mint tokens
  default_branch  TEXT        NOT NULL CHECK (default_branch <> ''),
  enrolled_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owning aggregate: Repository. Exists exactly while the repository is removed from the installation.
-- Re-enrollment deletes the row (recorded: the one place a fact row is deleted rather than appended).
CREATE TABLE repository_removals (
  repository TEXT        PRIMARY KEY REFERENCES repositories(full_name) ON DELETE CASCADE,
  removed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owning aggregate: Repository. Exists once the approved label has been ensured on GitHub.
CREATE TABLE repository_label_setups (
  repository TEXT        PRIMARY KEY REFERENCES repositories(full_name) ON DELETE CASCADE,
  label      TEXT        NOT NULL CHECK (label <> ''),
  ensured_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Deliveries

```sql
-- Owner: the GitHub boundary. Every delivery, verified before insert, byte-exact.
CREATE TABLE webhook_deliveries (
  delivery_id  TEXT        PRIMARY KEY,          -- X-GitHub-Delivery, natural key and idempotency key
  event        TEXT        NOT NULL CHECK (event <> ''),   -- X-GitHub-Event; GitHub's open vocabulary
  action       TEXT        NOT NULL DEFAULT '',            -- payload.action; empty for events without one (ping)
  sender_login TEXT        NOT NULL,                       -- payload.sender.login; empty only for ping
  payload      BYTEA       NOT NULL,                       -- the body the HMAC was verified over
  received_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX webhook_deliveries_by_received ON webhook_deliveries (received_at);
  -- serves: status last_delivery_at, the Translator boot scan (deliveries with no processing row)

-- Owner: the GitHub boundary. Exists exactly when a delivery has been processed, whatever the result.
CREATE TABLE webhook_delivery_processings (
  delivery_id  TEXT        PRIMARY KEY REFERENCES webhook_deliveries(delivery_id) ON DELETE CASCADE,
  result       TEXT        NOT NULL REFERENCES processing_results(result),
  detail       TEXT        NOT NULL CHECK (detail <> ''),   -- the command issued, or why ignored or rejected
  processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Casework

```sql
-- Owning aggregate: Case.
CREATE TABLE cases (
  id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
      -- surrogate: the natural key (repository, number) is compound and referenced by eight tables
  repository            TEXT        NOT NULL REFERENCES repositories(full_name) ON DELETE RESTRICT,
  number                INTEGER     NOT NULL CHECK (number >= 1),
  requester_login       TEXT        NOT NULL CHECK (requester_login <> ''),
  requester_association TEXT        NOT NULL REFERENCES associations(association),
  requester_trust       TEXT        NOT NULL REFERENCES trusts(trust),
      -- derivable from requester_association under the rule in force at receipt; stored as the verdict
      -- so a later rule change does not rewrite history (recorded exception)
  state                 TEXT        NOT NULL REFERENCES case_states(state),
      -- equals the latest case_transitions.to_state, or the initial state when no transition exists;
      -- stored for the state index below (recorded exception)
  received_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (repository, number),
  CHECK (NOT (state = 'gated'    AND requester_trust = 'trusted')),
  CHECK (NOT (state = 'received' AND requester_trust = 'untrusted'))
);
CREATE INDEX cases_by_state ON cases (state, received_at);
  -- serves: GET /api/cases?state=, status counts, the triage boot scan (received), the Scheduler (queued)

-- Owning aggregate: Case. Append-only log of state changes. The initial state is not logged;
-- it is received or gated by requester_trust at received_at.
CREATE TABLE case_transitions (
  id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,   -- event rows, no natural key
  case_id     UUID        NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
  from_state  TEXT        NOT NULL REFERENCES case_states(state),
  to_state    TEXT        NOT NULL REFERENCES case_states(state),
  cause       TEXT        NOT NULL REFERENCES transition_causes(cause),
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (from_state <> to_state)
);
CREATE INDEX case_transitions_by_case ON case_transitions (case_id, id);
  -- serves: case detail history, latest transition per case
CREATE INDEX case_transitions_queued ON case_transitions (occurred_at) WHERE to_state = 'queued';
  -- serves: Scheduler FIFO order

-- Owning aggregate: Case. At most one; only recorded while Received (store-enforced in RecordTriage).
CREATE TABLE case_triages (
  case_id    UUID        PRIMARY KEY REFERENCES cases(id) ON DELETE CASCADE,
  size       TEXT        NOT NULL REFERENCES sizes(size),
  rationale  TEXT        NOT NULL CHECK (rationale <> ''),
  model      TEXT        NOT NULL CHECK (model <> ''),
  triaged_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owning aggregate: Case. One row per approval, label or operator.
CREATE TABLE case_approvals (
  id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),   -- event rows, no natural key
  case_id              UUID        NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
  approver_login       TEXT        NOT NULL CHECK (approver_login <> ''),
  approver_association TEXT        NOT NULL REFERENCES associations(association),
  approver_trust       TEXT        NOT NULL REFERENCES trusts(trust),
  source               TEXT        NOT NULL REFERENCES approval_sources(source),
      -- 'label' iff a case_approval_deliveries row exists; stored so the row reads alone,
      -- consistency store-enforced in RecordApproval (recorded exception)
  approved_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX case_approvals_by_case ON case_approvals (case_id, approved_at);
  -- serves: NextAttemptKind (latest approval vs latest attempt start), case detail

-- Owning aggregate: Case. The webhook delivery behind a label-sourced approval.
CREATE TABLE case_approval_deliveries (
  approval_id UUID PRIMARY KEY REFERENCES case_approvals(id) ON DELETE CASCADE,
  delivery_id TEXT NOT NULL UNIQUE REFERENCES webhook_deliveries(delivery_id) ON DELETE RESTRICT
);

-- Owning aggregate: Case. At most one; terminal.
CREATE TABLE case_closures (
  case_id     UUID        PRIMARY KEY REFERENCES cases(id) ON DELETE CASCADE,
  delivery_id TEXT        NOT NULL UNIQUE REFERENCES webhook_deliveries(delivery_id) ON DELETE RESTRICT,
  closed_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owning aggregate: Case (owned entity Attempt).
CREATE TABLE attempts (
  id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  case_id        UUID        NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
  ordinal        INTEGER     NOT NULL CHECK (ordinal >= 1),
  kind           TEXT        NOT NULL REFERENCES attempt_kinds(kind),
  max_turns      INTEGER     NOT NULL CHECK (max_turns > 0),
  max_wall_clock INTERVAL    NOT NULL CHECK (max_wall_clock > INTERVAL '0'),
  max_diff_lines INTEGER     NOT NULL CHECK (max_diff_lines > 0),
  brief          TEXT        NOT NULL CHECK (brief <> ''),
  started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (case_id, ordinal)
);
-- "at most one attempt without an outcome per case" spans attempt_outcomes: enforced in StartAttempt's
-- transaction under the cases row lock (recorded here).
CREATE INDEX attempts_by_case ON attempts (case_id, ordinal);
  -- serves: case detail, latest attempt, ordinal allocation

-- Owning aggregate: Case. Exists exactly when the agent began executing.
CREATE TABLE attempt_runs (
  attempt_id UUID        PRIMARY KEY REFERENCES attempts(id) ON DELETE CASCADE,
  run_id     TEXT        NOT NULL UNIQUE,   -- jess ledger run id; logical reference, no FK across modules
  model      TEXT        NOT NULL CHECK (model <> ''),
  base_sha   TEXT        NOT NULL CHECK (base_sha ~ '^[0-9a-f]{40}$'),
  began_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owning aggregate: Case. Exists exactly when the attempt ended. Common fields; the variant is its own table.
CREATE TABLE attempt_outcomes (
  attempt_id    UUID        PRIMARY KEY REFERENCES attempts(id) ON DELETE CASCADE,
  kind          TEXT        NOT NULL REFERENCES outcome_kinds(kind),
  ended_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  turns         INTEGER     NOT NULL CHECK (turns >= 0),
  input_tokens  BIGINT      NOT NULL CHECK (input_tokens >= 0),
  output_tokens BIGINT      NOT NULL CHECK (output_tokens >= 0),
  wall_clock    INTERVAL    NOT NULL CHECK (wall_clock >= INTERVAL '0'),
  diff_lines    INTEGER     NOT NULL CHECK (diff_lines >= 0),
  summary       TEXT        NOT NULL CHECK (summary <> ''),
  UNIQUE (attempt_id, kind)   -- exists only to let each variant table pin its kind by composite FK
);
CREATE INDEX attempt_outcomes_by_kind ON attempt_outcomes (kind, ended_at);
  -- serves: metrics, the Commenter boot scan
-- "exactly one variant row, matching kind" is half structural (the composite FK below forbids a
-- mismatched variant) and half store-enforced (that a variant row exists), recorded here.

CREATE TABLE attempt_outcome_pull_requests (
  attempt_id UUID    PRIMARY KEY,
  kind       TEXT    NOT NULL CHECK (kind = 'pull_request_opened'),
  pr_number  INTEGER NOT NULL CHECK (pr_number >= 1),
  head_sha   TEXT    NOT NULL CHECK (head_sha ~ '^[0-9a-f]{40}$'),
  FOREIGN KEY (attempt_id, kind) REFERENCES attempt_outcomes(attempt_id, kind) ON DELETE CASCADE
);

CREATE TABLE attempt_outcome_exhaustions (
  attempt_id UUID PRIMARY KEY,
  kind       TEXT NOT NULL CHECK (kind = 'budget_exhausted'),
  limit_name TEXT NOT NULL REFERENCES budget_limits(limit_name),
  FOREIGN KEY (attempt_id, kind) REFERENCES attempt_outcomes(attempt_id, kind) ON DELETE CASCADE
);

CREATE TABLE attempt_outcome_failures (
  attempt_id UUID PRIMARY KEY,
  kind       TEXT NOT NULL CHECK (kind = 'failed'),
  class      TEXT NOT NULL REFERENCES failure_classes(class),
  message    TEXT NOT NULL CHECK (message <> ''),
  FOREIGN KEY (attempt_id, kind) REFERENCES attempt_outcomes(attempt_id, kind) ON DELETE CASCADE
);

CREATE TABLE attempt_outcome_aborts (
  attempt_id UUID PRIMARY KEY,
  kind       TEXT NOT NULL CHECK (kind = 'aborted'),
  reason     TEXT NOT NULL REFERENCES abort_reasons(reason),
  FOREIGN KEY (attempt_id, kind) REFERENCES attempt_outcomes(attempt_id, kind) ON DELETE CASCADE
);
-- "Aborted{daemon_restart} requeues once" reads attempt_outcome_aborts for the case: store-enforced in
-- RecordOutcome (recorded here).
```

### Comment outbox

```sql
-- Owner: the GitHub boundary. Outbox of comments to post; the body is what will be, and was, posted.
CREATE TABLE github_comments (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),   -- outbox rows, no natural key
  case_id    UUID        NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
      -- derivable through case_triage_comments or attempt_outcome_comments; stored so the poster
      -- reads one row (recorded exception)
  body       TEXT        NOT NULL CHECK (body <> ''),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX github_comments_by_created ON github_comments (created_at);
  -- serves: the Commenter scan for rows with no github_comment_posts row

-- Owner: the GitHub boundary. Exists exactly when the comment landed on GitHub.
CREATE TABLE github_comment_posts (
  comment_id        UUID        PRIMARY KEY REFERENCES github_comments(id) ON DELETE CASCADE,
  github_comment_id BIGINT      NOT NULL UNIQUE,
  posted_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Which fact a comment reports. Exactly one link per comment (store-enforced, recorded);
-- at most one comment per triage and per outcome (structural).
CREATE TABLE case_triage_comments (
  case_id    UUID PRIMARY KEY REFERENCES case_triages(case_id) ON DELETE CASCADE,
  comment_id UUID NOT NULL UNIQUE REFERENCES github_comments(id) ON DELETE CASCADE
);
CREATE TABLE attempt_outcome_comments (
  attempt_id UUID PRIMARY KEY REFERENCES attempt_outcomes(attempt_id) ON DELETE CASCADE,
  comment_id UUID NOT NULL UNIQUE REFERENCES github_comments(id) ON DELETE CASCADE
);
```

Nullable columns: none. Every column reads alone.

## Cross-check

Every Case transition through the three contracts.

| Transition | API | Event | DDL |
|---|---|---|---|
| `[*] -> Received` / `Gated` | `POST /webhook/github` (issues.opened); `POST .../run` when no case exists | `CaseReceived` / `CaseGated` | `cases` insert; CHECKs pin trust to initial state |
| `Received -> Queued` / `AwaitingApproval` | none: internal reaction to `CaseReceived` (deliberate: triage is not operator-invoked) | `CaseTriaged` | `case_triages` insert, `case_transitions` row, `cases.state` |
| `Gated` / `AwaitingApproval` / `Failed -> Queued` | `POST /webhook/github` (issues.labeled approved); `POST .../run` | `CaseApproved` | `case_approvals` (+ `case_approval_deliveries` for label), `case_transitions`, `cases.state` |
| approval recorded, no transition | same | `CaseApproved` with `from_state = to_state` | `case_approvals` only |
| `Queued -> Attempting` | none: Scheduler (deliberate: the operator cannot jump the queue) | `AttemptStarted` | `attempts` insert, `case_transitions`, `cases.state`; one-open-attempt store-enforced |
| `RecordRun` | none | `RunBegan` | `attempt_runs` insert |
| `Attempting -> Done` | none: runner | `AttemptEnded` | `attempt_outcomes` + `attempt_outcome_pull_requests`, `case_transitions`, `cases.state` |
| `Attempting -> AwaitingApproval` (exhausted) | none: runner | `AttemptEnded` | `attempt_outcomes` + `attempt_outcome_exhaustions`, transition |
| `Attempting -> AwaitingApproval` (operator stop) | `POST /api/attempts/{id}/stop` | `AttemptEnded` | `attempt_outcomes` + `attempt_outcome_aborts(operator_stop)`, transition |
| `Attempting -> Failed` | none: runner | `AttemptEnded` | `attempt_outcomes` + `attempt_outcome_failures`, transition |
| `Attempting -> Queued` / `Failed` (daemon restart) | none: boot | `AttemptEnded` | `attempt_outcomes` + `attempt_outcome_aborts(daemon_restart)`, transition; requeue-once store-enforced |
| `-> Closed` | `POST /webhook/github` (issues.closed) | `CaseClosed`, then `AttemptEnded` if one was open | `case_closures`, transition, then `attempt_outcome_aborts(issue_closed)` |
| Repository enrolled / removed | `POST /webhook/github` (installation, installation_repositories) | `RepositoryEnrolled` / `RepositoryRemoved` | `repositories`, `repository_removals`, `repository_label_setups` |

Invariants and where they live:

| Invariant | Enforcement |
|---|---|
| Trusted never Gated, Untrusted never Received | `cases` CHECKs |
| At most one triage, only while Received | PK on `case_triages`; state check in `RecordTriage` |
| Ordinal unique, at least 1 | `attempts` UNIQUE and CHECK |
| Budget values positive | `attempts` CHECKs |
| At most one open attempt per case | store-enforced under the `cases` row lock (recorded) |
| Run and outcome at most once per attempt | PKs on `attempt_runs`, `attempt_outcomes` |
| Variant matches outcome kind | composite FK with pinned `kind` CHECK |
| Exactly one variant row | store-enforced (recorded) |
| At most one closure | PK on `case_closures` |
| Requeue once on daemon restart | store-enforced in `RecordOutcome` (recorded) |
| Delivery processed at most once | PK on `webhook_delivery_processings` |
| One comment per triage, per outcome | PKs on the link tables |
| Rationale, brief, summary, message non-empty | CHECKs |

Completeness gate: every endpoint has every field; every event has every field; every table has owner, columns, keys, constraints and indexes naming their query; both caller classes with write access reach an endpoint and the scraper reaches `/metrics`; every transition traces or carries its reason above.

## Findings folded back into the model

1. `Attempt.id` is a database-generated UUID, not a ULID. Ordering is by `started_at`.
2. `Approval` gains `source` (label or operator) because `POST .../run` records an approval with no webhook behind it. `deliveryId` moves to an owned 0..1 `ApprovalDelivery`.
3. `Case` gains an owned append-only `Transition` (from, to, cause, at). The Scheduler orders the queue by it, and `cases.state` is its recorded denormalization.
4. `Scheduler` is a domain service: concurrency limit, FIFO order, removed repositories. It was orchestration in the model and is a rule.
5. The GitHub port gains `GetIssue`, needed by operator `run` and by the brief builder refreshing title and body at attempt start.
6. Comments are an outbox in the GitHub boundary with one link table per originating fact, so posting is durable and never duplicated. Not a domain object.
