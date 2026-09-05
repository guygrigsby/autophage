package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/guygrigsby/autophage/internal/resolution"
)

// ErrConflict is returned when a row the caller is creating already exists.
var ErrConflict = errors.New("conflict")

// CaseKey names a case.
type CaseKey struct {
	Repository string
	Number     int
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateCase inserts a new case and assigns its id.
func (s *Store) CreateCase(ctx context.Context, c *resolution.Case) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var id string
		r := c.Requester()
		err := tx.QueryRow(ctx, `insert into cases (repository, number, requester_login, requester_association, requester_trust, state, received_at)
			values ($1, $2, $3, $4, $5, $6, $7) returning id`,
			c.Repository(), c.Number(), r.Login, string(r.Association), string(r.Trust), string(c.State()), c.ReceivedAt()).Scan(&id)
		if isUnique(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if err := c.AssignID(id); err != nil {
			return err
		}
		c.ClearChanges()
		return notify(ctx, tx, casePayload(c))
	})
}

func casePayload(c *resolution.Case) string {
	return fmt.Sprintf("case:%s#%d:%s", c.Repository(), c.Number(), c.State())
}

// UpdateCase loads the case under a row lock, applies fn, persists exactly
// the changes fn produced and notifies. fn's error rolls everything back.
func (s *Store) UpdateCase(ctx context.Context, repository string, number int, fn func(*resolution.Case) error) (*resolution.Case, error) {
	var out *resolution.Case
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `select id from cases where repository = $1 and number = $2 for update`, repository, number).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		c, err := loadCase(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := fn(c); err != nil {
			return err
		}
		if err := persistChanges(ctx, tx, c); err != nil {
			return err
		}
		if err := notify(ctx, tx, casePayload(c)); err != nil {
			return err
		}
		out, err = loadCase(ctx, tx, id)
		return err
	})
	return out, err
}

// persistChanges writes every new fact in c.Changes() and the state summary.
func persistChanges(ctx context.Context, tx pgx.Tx, c *resolution.Case) error {
	ch := c.Changes()
	id := c.ID()
	if ch.Triage != nil {
		if _, err := tx.Exec(ctx, `insert into case_triages (case_id, size, rationale, model, triaged_at) values ($1, $2, $3, $4, $5)`,
			id, string(ch.Triage.Size), ch.Triage.Rationale, ch.Triage.Model, ch.Triage.TriagedAt); err != nil {
			return fmt.Errorf("triage: %w", err)
		}
	}
	for _, a := range ch.Approvals {
		var approvalID string
		if err := tx.QueryRow(ctx, `insert into case_approvals (case_id, approver_login, approver_association, approver_trust, source, approved_at)
			values ($1, $2, $3, $4, $5, $6) returning id`,
			id, a.Approver.Login, string(a.Approver.Association), string(a.Approver.Trust), string(a.Source), a.ApprovedAt).Scan(&approvalID); err != nil {
			return fmt.Errorf("approval: %w", err)
		}
		if a.Source == resolution.SourceLabel {
			if _, err := tx.Exec(ctx, `insert into case_approval_deliveries (approval_id, delivery_id) values ($1, $2)`, approvalID, a.DeliveryID); err != nil {
				return fmt.Errorf("approval delivery: %w", err)
			}
		}
	}
	for _, a := range ch.Attempts {
		if _, err := tx.Exec(ctx, `insert into attempts (case_id, ordinal, kind, max_turns, max_wall_clock, max_diff_lines, brief, started_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, a.Ordinal, string(a.Kind), a.Budget.MaxTurns(), toInterval(a.Budget.MaxWallClock()), a.Budget.MaxDiffLines(), a.Brief, a.StartedAt); err != nil {
			return fmt.Errorf("attempt: %w", err)
		}
	}
	for ordinal, r := range ch.Runs {
		if _, err := tx.Exec(ctx, `insert into attempt_runs (attempt_id, run_id, model, base_sha, began_at)
			select id, $3, $4, $5, $6 from attempts where case_id = $1 and ordinal = $2`,
			id, ordinal, r.RunID, r.Model, r.BaseSha, r.BeganAt); err != nil {
			return fmt.Errorf("run: %w", err)
		}
	}
	for ordinal, o := range ch.Outcomes {
		if err := insertOutcome(ctx, tx, id, ordinal, o); err != nil {
			return err
		}
	}
	if ch.Closure != nil {
		if _, err := tx.Exec(ctx, `insert into case_closures (case_id, delivery_id, closed_at) values ($1, $2, $3)`, id, ch.Closure.DeliveryID, ch.Closure.ClosedAt); err != nil {
			return fmt.Errorf("closure: %w", err)
		}
	}
	for _, tr := range ch.Transitions {
		if _, err := tx.Exec(ctx, `insert into case_transitions (case_id, from_state, to_state, cause, occurred_at) values ($1, $2, $3, $4, $5)`,
			id, string(tr.From), string(tr.To), string(tr.Cause), tr.OccurredAt); err != nil {
			return fmt.Errorf("transition: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `update cases set state = $2 where id = $1`, id, string(ch.State)); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	return nil
}

func insertOutcome(ctx context.Context, tx pgx.Tx, caseID string, ordinal int, o resolution.Outcome) error {
	var attemptID string
	if err := tx.QueryRow(ctx, `select id from attempts where case_id = $1 and ordinal = $2`, caseID, ordinal).Scan(&attemptID); err != nil {
		return fmt.Errorf("outcome attempt: %w", err)
	}
	if _, err := tx.Exec(ctx, `insert into attempt_outcomes (attempt_id, kind, ended_at, turns, input_tokens, output_tokens, wall_clock, diff_lines, summary)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		attemptID, string(o.Kind), o.EndedAt, o.Usage.Turns, o.Usage.InputTokens, o.Usage.OutputTokens, toInterval(o.Usage.WallClock), o.Usage.DiffLines, o.Summary); err != nil {
		return fmt.Errorf("outcome: %w", err)
	}
	var err error
	switch o.Kind {
	case resolution.PullRequestOpened:
		_, err = tx.Exec(ctx, `insert into attempt_outcome_pull_requests (attempt_id, kind, pr_number, head_sha) values ($1, $2, $3, $4)`, attemptID, string(o.Kind), o.PRNumber, o.HeadSha)
	case resolution.BudgetExhausted:
		_, err = tx.Exec(ctx, `insert into attempt_outcome_exhaustions (attempt_id, kind, limit_name) values ($1, $2, $3)`, attemptID, string(o.Kind), string(o.Limit))
	case resolution.FailedOutcome:
		_, err = tx.Exec(ctx, `insert into attempt_outcome_failures (attempt_id, kind, class, message) values ($1, $2, $3, $4)`, attemptID, string(o.Kind), string(o.Class), o.Message)
	case resolution.Aborted:
		_, err = tx.Exec(ctx, `insert into attempt_outcome_aborts (attempt_id, kind, reason) values ($1, $2, $3)`, attemptID, string(o.Kind), string(o.Reason))
	}
	if err != nil {
		return fmt.Errorf("outcome variant: %w", err)
	}
	return nil
}
