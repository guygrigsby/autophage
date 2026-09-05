-- +goose Up
CREATE TABLE case_states        (state TEXT PRIMARY KEY);
INSERT INTO case_states VALUES ('received'), ('gated'), ('queued'), ('attempting'), ('awaiting_approval'), ('failed'), ('done'), ('closed');

CREATE TABLE trusts             (trust TEXT PRIMARY KEY);
INSERT INTO trusts VALUES ('trusted'), ('untrusted');

CREATE TABLE associations       (association TEXT PRIMARY KEY);
INSERT INTO associations VALUES ('owner'), ('member'), ('collaborator'), ('contributor'), ('first_time_contributor'), ('first_timer'), ('none');

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

