package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/guygrigsby/autophage/internal/upkeep"
)

// BumpKey names a bump.
type BumpKey struct {
	Repository string
	Number     int
}

// OpenRepair is a round with no outcome, for the dispatcher and for recovery
// after a restart.
type OpenRepair struct {
	ID         string
	BumpID     string
	Repository string
	Number     int
	Round      int
}

// WatchRepository records that Upkeep acts on the repository's bumps.
// Idempotent: watching twice keeps the first watched_at, because the answer
// to "since when" should not move when an operator repeats themselves.
func (s *Store) WatchRepository(ctx context.Context, w upkeep.Watch) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into upkeep_watches (repository, watched_at) values ($1, $2)
			on conflict (repository) do nothing`, w.Repository, w.WatchedAt); err != nil {
			return err
		}
		return notify(ctx, tx, "watch:"+w.Repository)
	})
}

// UnwatchRepository stops new bumps being created for the repository. The
// bumps already open are deliberately left alone, including a round that is
// running: killing work mid-push leaves the branch in a state nobody asked
// for, and deleting the history loses what autophage did to the repository.
func (s *Store) UnwatchRepository(ctx context.Context, repository string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `delete from upkeep_watches where repository = $1`, repository); err != nil {
			return err
		}
		return notify(ctx, tx, "unwatch:"+repository)
	})
}

// WatchedRepositories lists every watched repository, oldest watch first.
func (s *Store) WatchedRepositories(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `select repository from upkeep_watches order by watched_at, repository`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateBump inserts a new bump and assigns its id. The watch is checked
// inside the transaction rather than by the caller: enrollment authorises
// issues, a watch authorises bumps, and a rule that only lives in a caller
// is one caller away from being forgotten.
func (s *Store) CreateBump(ctx context.Context, b *upkeep.Bump) error {
	var id string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var watched bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from upkeep_watches where repository = $1)`, b.Repository()).Scan(&watched); err != nil {
			return err
		}
		if !watched {
			return fmt.Errorf("%s is enrolled but not watched for upkeep: %w", b.Repository(), ErrNotFound)
		}
		err := tx.QueryRow(ctx, `insert into bumps (repository, number, branch, base_branch, head_sha, state, opened_at)
			values ($1, $2, $3, $4, $5, $6, $7) returning id`,
			b.Repository(), b.Number(), b.Branch(), b.BaseBranch(), b.HeadSha(), string(b.State()), b.OpenedAt()).Scan(&id)
		if isUnique(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		return notify(ctx, tx, bumpPayload(b))
	})
	if err != nil {
		return err
	}
	if err := b.AssignID(id); err != nil {
		return err
	}
	b.ClearChanges()
	return nil
}

func bumpPayload(b *upkeep.Bump) string {
	return fmt.Sprintf("bump:%s#%d:%s", b.Repository(), b.Number(), b.State())
}

// UpdateBump loads the bump under a row lock, applies fn, persists exactly
// the changes fn produced and notifies. fn's error rolls everything back,
// which is what makes a refused transition leave no trace.
func (s *Store) UpdateBump(ctx context.Context, repository string, number int, fn func(*upkeep.Bump) error) (*upkeep.Bump, error) {
	var out *upkeep.Bump
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `select id from bumps where repository = $1 and number = $2 for update`, repository, number).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		b, err := loadBump(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
		if err := persistBumpChanges(ctx, tx, b); err != nil {
			return err
		}
		if err := notify(ctx, tx, bumpPayload(b)); err != nil {
			return err
		}
		out, err = loadBump(ctx, tx, id)
		return err
	})
	return out, err
}

// UpdateBumpByRepair is UpdateBump addressed by a round id, for the runner,
// which knows the round it is executing and not the pull request number.
func (s *Store) UpdateBumpByRepair(ctx context.Context, repairID string, fn func(*upkeep.Bump) error) (*upkeep.Bump, error) {
	var key BumpKey
	err := s.pool.QueryRow(ctx, `select b.repository, b.number from bump_repairs r join bumps b on b.id = r.bump_id where r.id = $1`, repairID).
		Scan(&key.Repository, &key.Number)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.UpdateBump(ctx, key.Repository, key.Number, fn)
}

// persistBumpChanges writes every new fact in b.Changes(), the head and the
// state summary.
func persistBumpChanges(ctx context.Context, tx pgx.Tx, b *upkeep.Bump) error {
	ch := b.Changes()
	id := b.ID()
	for _, v := range ch.Verdicts {
		if _, err := tx.Exec(ctx, `insert into bump_check_verdicts (bump_id, head_sha, conclusion, failing_contexts, details_url, concluded_at)
			values ($1, $2, $3, $4, $5, $6)`,
			id, v.HeadSha, string(v.Conclusion), v.FailingContexts, v.DetailsURL, v.ConcludedAt); err != nil {
			return fmt.Errorf("verdict: %w", err)
		}
	}
	for _, a := range ch.Attempts {
		if _, err := tx.Exec(ctx, `insert into bump_repairs (bump_id, round, base_sha, max_turns, max_wall_clock, max_diff_lines, brief, started_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, a.Round, a.BaseSha, a.Budget.MaxTurns(), toInterval(a.Budget.MaxWallClock()), a.Budget.MaxDiffLines(), a.Brief, a.StartedAt); err != nil {
			return fmt.Errorf("repair: %w", err)
		}
	}
	for round, r := range ch.Runs {
		repairID, err := repairIDFor(ctx, tx, id, round)
		if err != nil {
			return fmt.Errorf("run: %w", err)
		}
		if _, err := tx.Exec(ctx, `insert into bump_repair_runs (repair_id, run_id, model, base_sha, began_at)
			values ($1, $2, $3, $4, $5)`, repairID, r.RunID, r.Model, r.BaseSha, r.BeganAt); err != nil {
			return fmt.Errorf("run: %w", err)
		}
	}
	for round, o := range ch.Outcomes {
		repairID, err := repairIDFor(ctx, tx, id, round)
		if err != nil {
			return fmt.Errorf("outcome: %w", err)
		}
		if err := insertRepairOutcome(ctx, tx, repairID, o); err != nil {
			return err
		}
	}
	if ch.DropAbandon {
		if _, err := tx.Exec(ctx, `delete from bump_abandonments where bump_id = $1`, id); err != nil {
			return fmt.Errorf("drop abandonment: %w", err)
		}
	}
	if ch.Abandonment != nil {
		if _, err := tx.Exec(ctx, `insert into bump_abandonments (bump_id, reason, detail, abandoned_at) values ($1, $2, $3, $4)`,
			id, string(ch.Abandonment.Reason), ch.Abandonment.Detail, ch.Abandonment.AbandonedAt); err != nil {
			return fmt.Errorf("abandonment: %w", err)
		}
	}
	if ch.Closure != nil {
		if _, err := tx.Exec(ctx, `insert into bump_closures (bump_id, kind, delivery_id, closed_at) values ($1, $2, $3, $4)`,
			id, string(ch.Closure.Kind), ch.Closure.DeliveryID, ch.Closure.ClosedAt); err != nil {
			return fmt.Errorf("closure: %w", err)
		}
	}
	for _, tr := range ch.Transitions {
		if _, err := tx.Exec(ctx, `insert into bump_transitions (bump_id, from_state, to_state, cause, occurred_at) values ($1, $2, $3, $4, $5)`,
			id, string(tr.From), string(tr.To), string(tr.Cause), tr.OccurredAt); err != nil {
			return fmt.Errorf("transition: %w", err)
		}
	}
	_, err := tx.Exec(ctx, `update bumps set state = $2, head_sha = $3 where id = $1`, id, string(ch.State), ch.HeadSha)
	return err
}

// insertRepairOutcome writes the common row and the one variant row its kind
// calls for. no_change has no variant table: the kind says everything and
// summary is the evidence.
func insertRepairOutcome(ctx context.Context, tx pgx.Tx, repairID string, o upkeep.RepairOutcome) error {
	u := o.Usage
	if _, err := tx.Exec(ctx, `insert into bump_repair_outcomes (repair_id, kind, ended_at, turns, input_tokens, output_tokens, wall_clock, diff_lines, summary)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		repairID, string(o.Kind), o.EndedAt, u.Turns, u.InputTokens, u.OutputTokens, toInterval(u.WallClock), u.DiffLines, o.Summary); err != nil {
		return fmt.Errorf("outcome: %w", err)
	}
	var err error
	switch o.Kind {
	case upkeep.Pushed:
		_, err = tx.Exec(ctx, `insert into bump_repair_outcome_pushes (repair_id, kind, head_sha) values ($1, 'pushed', $2)`, repairID, o.HeadSha)
	case upkeep.BudgetExhausted:
		_, err = tx.Exec(ctx, `insert into bump_repair_outcome_exhaustions (repair_id, kind, limit_name) values ($1, 'budget_exhausted', $2)`, repairID, string(o.Limit))
	case upkeep.RepairFailed:
		_, err = tx.Exec(ctx, `insert into bump_repair_outcome_failures (repair_id, kind, class, message) values ($1, 'failed', $2, $3)`, repairID, string(o.Class), o.Message)
	case upkeep.RepairAborted:
		_, err = tx.Exec(ctx, `insert into bump_repair_outcome_aborts (repair_id, kind, reason) values ($1, 'aborted', $2)`, repairID, string(o.Reason))
	}
	if err != nil {
		return fmt.Errorf("outcome variant %s: %w", o.Kind, err)
	}
	return nil
}

// repairIDFor resolves a round ordinal to its store id. The aggregate keys
// changes by round because ids belong here, not to it.
func repairIDFor(ctx context.Context, tx pgx.Tx, bumpID string, round int) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `select id from bump_repairs where bump_id = $1 and round = $2`, bumpID, round).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("round %d of bump %s: %w", round, bumpID, ErrNotFound)
	}
	return id, err
}

// QueuedBumps lists queued bumps oldest first by the transition that queued
// them, which is the order the RepairScheduler starts rounds in.
func (s *Store) QueuedBumps(ctx context.Context) ([]BumpKey, error) {
	return s.bumpKeys(ctx, `select b.repository, b.number
		from bumps b
		join lateral (
			select occurred_at from bump_transitions t
			where t.bump_id = b.id and t.to_state = 'queued'
			order by t.id desc limit 1
		) q on true
		where b.state = 'queued'
		order by q.occurred_at, b.id`)
}

// BumpsAwaitingChecksSince lists bumps that have been awaiting checks since
// before cutoff, for the StaleCheckSweeper. The wait starts at the latest
// transition into awaiting_checks, or at opened_at when there is none, so a
// repair push restarts the clock rather than inheriting the original wait.
func (s *Store) BumpsAwaitingChecksSince(ctx context.Context, cutoff time.Time) ([]BumpKey, error) {
	return s.bumpKeys(ctx, `select b.repository, b.number
		from bumps b
		where b.state = 'awaiting_checks'
		  and coalesce((
		    select occurred_at from bump_transitions t
		    where t.bump_id = b.id and t.to_state = 'awaiting_checks'
		    order by t.id desc limit 1
		  ), b.opened_at) <= $1
		order by b.opened_at, b.id`, cutoff)
}

func (s *Store) bumpKeys(ctx context.Context, sql string, args ...any) ([]BumpKey, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BumpKey
	for rows.Next() {
		var k BumpKey
		if err := rows.Scan(&k.Repository, &k.Number); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// OpenRepairs lists every round with no outcome. On boot each one is ended
// Aborted{DaemonRestart}: a round never resumes mid-run.
func (s *Store) OpenRepairs(ctx context.Context) ([]OpenRepair, error) {
	rows, err := s.pool.Query(ctx, `select r.id, r.bump_id, b.repository, b.number, r.round
		from bump_repairs r
		join bumps b on b.id = r.bump_id
		left join bump_repair_outcomes o on o.repair_id = r.id
		where o.repair_id is null
		order by r.started_at, r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenRepair
	for rows.Next() {
		var r OpenRepair
		if err := rows.Scan(&r.ID, &r.BumpID, &r.Repository, &r.Number, &r.Round); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OpenRepairsOnClosedBumps lists rounds still running on a bump whose pull
// request has closed. A closed bump can never move again, so a round left
// running would spend turns and tokens on an outcome nothing will read.
func (s *Store) OpenRepairsOnClosedBumps(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `select r.id
		from bump_repairs r
		join bumps b on b.id = r.bump_id
		left join bump_repair_outcomes o on o.repair_id = r.id
		where o.repair_id is null and b.state = 'closed'
		order by r.started_at, r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
