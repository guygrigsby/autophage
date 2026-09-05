package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/guygrigsby/autophage/internal/resolution"
)

// querier is the subset of pool and tx the loader needs.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) GetCase(ctx context.Context, repository string, number int) (*resolution.Case, error) {
	var id string
	err := s.pool.QueryRow(ctx, `select id from cases where repository = $1 and number = $2`, repository, number).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return loadCase(ctx, s.pool, id)
}

func (s *Store) GetCaseByID(ctx context.Context, id string) (*resolution.Case, error) {
	c, err := loadCase(ctx, s.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *Store) GetCaseByAttempt(ctx context.Context, attemptID string) (*resolution.Case, error) {
	var id string
	err := s.pool.QueryRow(ctx, `select case_id from attempts where id = $1`, attemptID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return loadCase(ctx, s.pool, id)
}

// loadCase assembles the snapshot from every fact table and rebuilds the
// aggregate through LoadCase, so the invariants are re-checked on the way in.
func loadCase(ctx context.Context, q querier, id string) (*resolution.Case, error) {
	var snap resolution.Snapshot
	var login, assoc, trust, state string
	err := q.QueryRow(ctx, `select id, repository, number, requester_login, requester_association, requester_trust, state, received_at from cases where id = $1`, id).
		Scan(&snap.ID, &snap.Repository, &snap.Number, &login, &assoc, &trust, &state, &snap.ReceivedAt)
	if err != nil {
		return nil, err
	}
	snap.Requester = resolution.Requester{Login: login, Association: resolution.Association(assoc), Trust: resolution.Trust(trust)}
	snap.State = resolution.CaseState(state)

	var tr resolution.Triage
	var size string
	err = q.QueryRow(ctx, `select size, rationale, model, triaged_at from case_triages where case_id = $1`, id).Scan(&size, &tr.Rationale, &tr.Model, &tr.TriagedAt)
	if err == nil {
		tr.Size = resolution.Size(size)
		snap.Triage = &tr
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	rows, err := q.Query(ctx, `select a.id, a.approver_login, a.approver_association, a.approver_trust, a.source, a.approved_at, coalesce(d.delivery_id, '')
		from case_approvals a left join case_approval_deliveries d on d.approval_id = a.id
		where a.case_id = $1 order by a.approved_at, a.id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a resolution.Approval
		var alogin, aassoc, atrust, source string
		if err := rows.Scan(&a.ID, &alogin, &aassoc, &atrust, &source, &a.ApprovedAt, &a.DeliveryID); err != nil {
			rows.Close()
			return nil, err
		}
		a.Approver = resolution.Requester{Login: alogin, Association: resolution.Association(aassoc), Trust: resolution.Trust(atrust)}
		a.Source = resolution.ApprovalSource(source)
		snap.Approvals = append(snap.Approvals, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	snap.Attempts, err = loadAttempts(ctx, q, id)
	if err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `select from_state, to_state, cause, occurred_at from case_transitions where case_id = $1 order by id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t resolution.Transition
		var from, to, cause string
		if err := rows.Scan(&from, &to, &cause, &t.OccurredAt); err != nil {
			rows.Close()
			return nil, err
		}
		t.From, t.To, t.Cause = resolution.CaseState(from), resolution.CaseState(to), resolution.TransitionCause(cause)
		snap.Transitions = append(snap.Transitions, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var cl resolution.Closure
	err = q.QueryRow(ctx, `select delivery_id, closed_at from case_closures where case_id = $1`, id).Scan(&cl.DeliveryID, &cl.ClosedAt)
	if err == nil {
		snap.Closure = &cl
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return resolution.LoadCase(snap)
}

func loadAttempts(ctx context.Context, q querier, caseID string) ([]resolution.Attempt, error) {
	rows, err := q.Query(ctx, `select a.id, a.ordinal, a.kind, a.max_turns, a.max_wall_clock, a.max_diff_lines, a.brief, a.started_at,
			r.run_id, r.model, r.base_sha, r.began_at,
			o.kind, o.ended_at, o.turns, o.input_tokens, o.output_tokens, o.wall_clock, o.diff_lines, o.summary,
			p.pr_number, p.head_sha, e.limit_name, f.class, f.message, b.reason
		from attempts a
		left join attempt_runs r on r.attempt_id = a.id
		left join attempt_outcomes o on o.attempt_id = a.id
		left join attempt_outcome_pull_requests p on p.attempt_id = a.id
		left join attempt_outcome_exhaustions e on e.attempt_id = a.id
		left join attempt_outcome_failures f on f.attempt_id = a.id
		left join attempt_outcome_aborts b on b.attempt_id = a.id
		where a.case_id = $1 order by a.ordinal`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []resolution.Attempt
	for rows.Next() {
		var a resolution.Attempt
		var kind string
		var maxTurns, maxDiff int
		var maxWall pgtype.Interval
		var runID, runModel, baseSha *string
		var beganAt *time.Time
		var oKind, summary *string
		var endedAt *time.Time
		var turns, inTok, outTok, diff *int
		var wall *pgtype.Interval
		var prNumber *int
		var headSha, limit, class, message, reason *string
		if err := rows.Scan(&a.ID, &a.Ordinal, &kind, &maxTurns, &maxWall, &maxDiff, &a.Brief, &a.StartedAt,
			&runID, &runModel, &baseSha, &beganAt,
			&oKind, &endedAt, &turns, &inTok, &outTok, &wall, &diff, &summary,
			&prNumber, &headSha, &limit, &class, &message, &reason); err != nil {
			return nil, err
		}
		a.Kind = resolution.AttemptKind(kind)
		if a.Budget, err = resolution.NewBudget(maxTurns, fromInterval(maxWall), maxDiff); err != nil {
			return nil, err
		}
		if runID != nil {
			a.Run = &resolution.Run{RunID: *runID, Model: *runModel, BaseSha: *baseSha, BeganAt: *beganAt}
		}
		if oKind != nil {
			o := resolution.Outcome{Kind: resolution.OutcomeKind(*oKind), EndedAt: *endedAt, Summary: *summary,
				Usage: resolution.Usage{Turns: *turns, InputTokens: *inTok, OutputTokens: *outTok, WallClock: fromInterval(*wall), DiffLines: *diff}}
			if prNumber != nil {
				o.PRNumber, o.HeadSha = *prNumber, *headSha
			}
			if limit != nil {
				o.Limit = resolution.Limit(*limit)
			}
			if class != nil {
				o.Class, o.Message = resolution.FailureClass(*class), *message
			}
			if reason != nil {
				o.Reason = resolution.AbortReason(*reason)
			}
			a.Outcome = &o
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
