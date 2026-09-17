-- +goose Up
-- Upkeep: dependabot bumps, their check verdicts and their repair rounds.
-- Up-only, like 0001. See docs/specs/2026-09-17-autophage-upkeep-contracts.md
-- and docs/adr/0007-dependabot-bumps-as-an-upkeep-context.md.
--
-- budget_limits and failure_classes from 0001 are reused rather than
-- duplicated: they are shared kernel and mean exactly the same thing here.
-- abort_reasons is NOT reused: 'issue_closed' is invalid for a bump and
-- 'pull_request_closed' is invalid for a case, so Upkeep owns its own.

CREATE TABLE bump_states (state TEXT PRIMARY KEY);
INSERT INTO bump_states VALUES ('awaiting_checks'), ('queued'), ('repairing'), ('green'), ('abandoned'), ('closed');

CREATE TABLE check_conclusions (conclusion TEXT PRIMARY KEY);
INSERT INTO check_conclusions VALUES ('success'), ('failure');

CREATE TABLE repair_outcome_kinds (kind TEXT PRIMARY KEY);
INSERT INTO repair_outcome_kinds VALUES ('pushed'), ('no_change'), ('budget_exhausted'), ('failed'), ('aborted');

CREATE TABLE repair_abort_reasons (reason TEXT PRIMARY KEY);
INSERT INTO repair_abort_reasons VALUES ('operator_stop'), ('daemon_restart'), ('pull_request_closed');

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
  ('repair_failed_abandoned'), ('repair_aborted_operator_stop'),
  ('repair_aborted_restart_requeued'), ('repair_aborted_restart_abandoned'),
  ('head_advanced'), ('operator_retry'), ('closed');

-- Owning aggregate: Watch. Exists exactly while Upkeep acts on the repository's
-- bumps. Enrollment authorises issues; this authorises bumps. Deleting it stops
-- new bumps only, which is why bumps does not reference it.
CREATE TABLE upkeep_watches (
  repository TEXT        PRIMARY KEY REFERENCES repositories(full_name) ON DELETE CASCADE,
  watched_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owning aggregate: Bump.
CREATE TABLE bumps (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
      -- surrogate: the natural key (repository, number) is compound and referenced by six tables
  repository  TEXT        NOT NULL REFERENCES repositories(full_name) ON DELETE RESTRICT,
  number      INTEGER     NOT NULL CHECK (number >= 1),
  branch      TEXT        NOT NULL CHECK (branch <> ''),
      -- dependabot named it; unlike a case's autophage/<n> it cannot be derived
  base_branch TEXT        NOT NULL CHECK (base_branch <> ''),
  head_sha    TEXT        NOT NULL CHECK (head_sha ~ '^[0-9a-f]{40}$'),
      -- the current tip. Mutated by AdvanceHead, which is also what makes a
      -- verdict for any other sha stale
  state       TEXT        NOT NULL REFERENCES bump_states(state),
      -- equals the latest bump_transitions.to_state, or awaiting_checks when there
      -- is no transition; stored for the index below (recorded exception)
  opened_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (repository, number)
);
CREATE INDEX bumps_by_state ON bumps (state, opened_at);
  -- serves: GET /api/bumps?state=, status counts, the RepairScheduler (queued),
  -- the StaleCheckSweeper (awaiting_checks)
-- "the repository has a watch row at creation" is enforced in CreateBump's
-- transaction, not structurally: a watch may be deleted afterwards and the
-- bump must survive it (recorded exception).
-- "the author is the dependabot login" is the GitHub boundary's check; the
-- domain never sees a login here.

-- Owning aggregate: Bump. Append-only log of state changes. The initial state
-- is not logged; it is awaiting_checks at opened_at.
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
  -- serves: bump detail history, latest transition per bump, retry grant count
CREATE INDEX bump_transitions_queued ON bump_transitions (occurred_at) WHERE to_state = 'queued';
  -- serves: RepairScheduler FIFO order
CREATE INDEX bump_transitions_awaiting ON bump_transitions (occurred_at) WHERE to_state = 'awaiting_checks';
  -- serves: StaleCheckSweeper, which needs when the current wait window started

-- Owning aggregate: Bump. Exists exactly when CI reached a conclusive rollup
-- for one head sha. A pending rollup records nothing; the absence has a
-- deadline, which is why there is no 'pending' conclusion.
CREATE TABLE bump_check_verdicts (
  id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  bump_id          UUID        NOT NULL REFERENCES bumps(id) ON DELETE CASCADE,
  head_sha         TEXT        NOT NULL CHECK (head_sha ~ '^[0-9a-f]{40}$'),
  conclusion       TEXT        NOT NULL REFERENCES check_conclusions(conclusion),
  failing_contexts TEXT        NOT NULL,
      -- newline-separated run names, verbatim from GitHub
  details_url      TEXT        NOT NULL,
      -- empty when GitHub gave none; failing_contexts is the evidence
  concluded_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (bump_id, head_sha),
  CHECK ((conclusion = 'success') = (failing_contexts = ''))
);
-- "a verdict's head_sha must equal bumps.head_sha at insert" is the loop guard
-- and is enforced in the aggregate under the bumps row lock (recorded here).
-- A CHECK cannot reach another table.

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
-- "at most one round without an outcome per bump" spans bump_repair_outcomes:
-- enforced in StartRepair's transaction under the bumps row lock (recorded here).
-- "round <= the configured cap plus retry grants" reads config, so it is
-- enforced in the aggregate too (recorded here).

-- Owning aggregate: Bump. Exists exactly when the agent began executing.
CREATE TABLE bump_repair_runs (
  repair_id UUID        PRIMARY KEY REFERENCES bump_repairs(id) ON DELETE CASCADE,
  run_id    TEXT        NOT NULL UNIQUE,   -- jess ledger run id; logical reference, no FK across modules
  model     TEXT        NOT NULL CHECK (model <> ''),
  base_sha  TEXT        NOT NULL CHECK (base_sha ~ '^[0-9a-f]{40}$'),
  began_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owning aggregate: Bump. Exists exactly when the round ended. Common fields;
-- the variant is its own table. 'no_change' has no variant table: the kind
-- carries all of its meaning and summary is the evidence.
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

-- class drives the transition: 'infra' requeues the bump, 'model' and 'agent'
-- abandon it. Enforced in the aggregate (recorded here).
CREATE TABLE bump_repair_outcome_failures (
  repair_id UUID PRIMARY KEY,
  kind      TEXT NOT NULL CHECK (kind = 'failed'),
  class     TEXT NOT NULL REFERENCES failure_classes(class),
  message   TEXT NOT NULL CHECK (message <> ''),
  FOREIGN KEY (repair_id, kind) REFERENCES bump_repair_outcomes(repair_id, kind) ON DELETE CASCADE
);

CREATE TABLE bump_repair_outcome_aborts (
  repair_id UUID PRIMARY KEY,
  kind      TEXT NOT NULL CHECK (kind = 'aborted'),
  reason    TEXT NOT NULL REFERENCES repair_abort_reasons(reason),
  FOREIGN KEY (repair_id, kind) REFERENCES bump_repair_outcomes(repair_id, kind) ON DELETE CASCADE
);

-- Owning aggregate: Bump. Exists exactly when autophage has stopped trying.
-- Not derivable from the last outcome: 'checks_never_concluded' abandons a bump
-- on which no round ever ran. Deleted by operator retry (recorded: the second
-- place a fact row is deleted rather than appended).
CREATE TABLE bump_abandonments (
  bump_id      UUID        PRIMARY KEY REFERENCES bumps(id) ON DELETE CASCADE,
  reason       TEXT        NOT NULL REFERENCES abandon_reasons(reason),
  detail       TEXT        NOT NULL CHECK (detail <> ''),   -- the failing contexts, limit, or message
  abandoned_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owning aggregate: Bump. Exists exactly when the pull request closed on GitHub.
CREATE TABLE bump_closures (
  bump_id     UUID        PRIMARY KEY REFERENCES bumps(id) ON DELETE CASCADE,
  kind        TEXT        NOT NULL REFERENCES closure_kinds(kind),
  delivery_id TEXT        NOT NULL UNIQUE REFERENCES webhook_deliveries(delivery_id) ON DELETE RESTRICT,
  closed_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
