package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/upkeep"
)

// GetBump loads one bump by its natural key.
func (s *Store) GetBump(ctx context.Context, repository string, number int) (*upkeep.Bump, error) {
	var id string
	err := s.pool.QueryRow(ctx, `select id from bumps where repository = $1 and number = $2`, repository, number).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return loadBump(ctx, s.pool, id)
}

// GetBumpByRepair loads the bump that owns a round, for the runner.
func (s *Store) GetBumpByRepair(ctx context.Context, repairID string) (*upkeep.Bump, error) {
	var id string
	err := s.pool.QueryRow(ctx, `select bump_id from bump_repairs where id = $1`, repairID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return loadBump(ctx, s.pool, id)
}

// loadBump assembles the snapshot from every fact table and rebuilds the
// aggregate through LoadBump, so the invariants are re-checked on the way in.
func loadBump(ctx context.Context, q querier, id string) (*upkeep.Bump, error) {
	var snap upkeep.Snapshot
	var state string
	err := q.QueryRow(ctx, `select id, repository, number, branch, base_branch, head_sha, state, opened_at from bumps where id = $1`, id).
		Scan(&snap.ID, &snap.Repository, &snap.Number, &snap.Branch, &snap.BaseBranch, &snap.HeadSha, &state, &snap.OpenedAt)
	if err != nil {
		return nil, err
	}
	snap.State = upkeep.BumpState(state)

	rows, err := q.Query(ctx, `select id, head_sha, conclusion, failing_contexts, details_url, concluded_at
		from bump_check_verdicts where bump_id = $1 order by concluded_at, id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v upkeep.CheckVerdict
		var conclusion string
		if err := rows.Scan(&v.ID, &v.HeadSha, &conclusion, &v.FailingContexts, &v.DetailsURL, &v.ConcludedAt); err != nil {
			rows.Close()
			return nil, err
		}
		v.Conclusion = upkeep.CheckConclusion(conclusion)
		snap.Verdicts = append(snap.Verdicts, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	attempts, err := loadRepairs(ctx, q, id)
	if err != nil {
		return nil, err
	}
	snap.Attempts = attempts

	rows, err = q.Query(ctx, `select from_state, to_state, cause, occurred_at from bump_transitions where bump_id = $1 order by id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tr upkeep.BumpTransition
		var from, to, cause string
		if err := rows.Scan(&from, &to, &cause, &tr.OccurredAt); err != nil {
			rows.Close()
			return nil, err
		}
		tr.From, tr.To, tr.Cause = upkeep.BumpState(from), upkeep.BumpState(to), upkeep.BumpTransitionCause(cause)
		snap.Transitions = append(snap.Transitions, tr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var ab upkeep.BumpAbandonment
	var reason string
	err = q.QueryRow(ctx, `select reason, detail, abandoned_at from bump_abandonments where bump_id = $1`, id).Scan(&reason, &ab.Detail, &ab.AbandonedAt)
	if err == nil {
		ab.Reason = upkeep.AbandonReason(reason)
		snap.Abandonment = &ab
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var cl upkeep.BumpClosure
	var kind string
	err = q.QueryRow(ctx, `select kind, delivery_id, closed_at from bump_closures where bump_id = $1`, id).Scan(&kind, &cl.DeliveryID, &cl.ClosedAt)
	if err == nil {
		cl.Kind = upkeep.ClosureKind(kind)
		snap.Closure = &cl
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	return upkeep.LoadBump(snap)
}

// loadRepairs reads every round with its run and its outcome, variant
// included. The left joins are what make an open round and a finished one
// come back from one query.
func loadRepairs(ctx context.Context, q querier, bumpID string) ([]upkeep.RepairAttempt, error) {
	rows, err := q.Query(ctx, `select r.id, r.round, r.base_sha, r.max_turns, r.max_wall_clock, r.max_diff_lines, r.brief, r.started_at,
			run.run_id, run.model, run.base_sha, run.began_at,
			o.kind, o.ended_at, o.turns, o.input_tokens, o.output_tokens, o.wall_clock, o.diff_lines, o.summary,
			p.head_sha, e.limit_name, f.class, f.message, ab.reason
		from bump_repairs r
		left join bump_repair_runs run on run.repair_id = r.id
		left join bump_repair_outcomes o on o.repair_id = r.id
		left join bump_repair_outcome_pushes p on p.repair_id = r.id
		left join bump_repair_outcome_exhaustions e on e.repair_id = r.id
		left join bump_repair_outcome_failures f on f.repair_id = r.id
		left join bump_repair_outcome_aborts ab on ab.repair_id = r.id
		where r.bump_id = $1 order by r.round`, bumpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []upkeep.RepairAttempt
	for rows.Next() {
		var a upkeep.RepairAttempt
		var turns, diffLines int
		var wall pgtype.Interval
		var runID, runModel, runBase *string
		var runBegan *time.Time
		var kind *string
		var endedAt *time.Time
		var oTurns, oDiff *int
		var oIn, oOut *int64
		var oWall *pgtype.Interval
		var summary *string
		var headSha, limitName, class, message, reason *string
		if err := rows.Scan(&a.ID, &a.Round, &a.BaseSha, &turns, &wall, &diffLines, &a.Brief, &a.StartedAt,
			&runID, &runModel, &runBase, &runBegan,
			&kind, &endedAt, &oTurns, &oIn, &oOut, &oWall, &oDiff, &summary,
			&headSha, &limitName, &class, &message, &reason); err != nil {
			return nil, err
		}
		budget, err := resolution.NewBudget(turns, fromInterval(wall), diffLines)
		if err != nil {
			return nil, err
		}
		a.Budget = budget
		if runID != nil {
			a.Run = &resolution.Run{RunID: *runID, Model: deref(runModel), BaseSha: deref(runBase), BeganAt: derefTime(runBegan)}
		}
		if kind != nil {
			o := upkeep.RepairOutcome{
				Kind: upkeep.RepairOutcomeKind(*kind), EndedAt: derefTime(endedAt), Summary: deref(summary),
				Usage:   resolution.Usage{Turns: derefInt(oTurns), InputTokens: int(derefInt64(oIn)), OutputTokens: int(derefInt64(oOut)), WallClock: derefInterval(oWall), DiffLines: derefInt(oDiff)},
				HeadSha: deref(headSha), Limit: resolution.Limit(deref(limitName)),
				Class: resolution.FailureClass(deref(class)), Message: deref(message),
				Reason: upkeep.RepairAbortReason(deref(reason)),
			}
			a.Outcome = &o
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}

func derefInt64(i *int64) int64 {
	if i == nil {
		return 0
	}
	return *i
}

func derefTime(t *time.Time) (z time.Time) {
	if t == nil {
		return z
	}
	return *t
}

// derefInterval is fromInterval for a column that is NULL when the round has
// no outcome yet.
func derefInterval(iv *pgtype.Interval) time.Duration {
	if iv == nil {
		return 0
	}
	return fromInterval(*iv)
}
