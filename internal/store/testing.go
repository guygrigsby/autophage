package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/guygrigsby/autophage/internal/resolution"
)

// The helpers here exist for tests but live in a non-test file so other
// packages' tests can call them. None pulls in a test-only dependency; the
// container harness that does lives in internal/storetest.

// Truncate empties every table except the vocabularies and goose's, so
// tests start clean.
func (s *Store) Truncate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `truncate table
		github_comment_posts, attempt_outcome_comments, case_triage_comments, github_comments,
		attempt_outcome_aborts, attempt_outcome_failures, attempt_outcome_exhaustions, attempt_outcome_pull_requests,
		attempt_outcomes, attempt_runs, attempts, case_closures, case_approval_deliveries, case_approvals,
		case_triages, case_transitions, cases, webhook_delivery_processings, webhook_deliveries,
		repository_label_setups, repository_removals, repositories
		restart identity cascade`)
	return err
}

// PersistRunForTest writes a run against an arbitrary attempt ordinal. The
// aggregate only ever produces the open attempt's ordinal, so this is the
// only way to exercise what persistChanges does with one it cannot resolve.
func PersistRunForTest(ctx context.Context, s *Store, caseID string, ordinal int, r resolution.Run) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		attemptID, err := attemptIDFor(ctx, tx, caseID, ordinal)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `insert into attempt_runs (attempt_id, run_id, model, base_sha, began_at)
			values ($1, $2, $3, $4, $5)`, attemptID, r.RunID, r.Model, r.BaseSha, r.BeganAt)
		return err
	})
}

// StoreDeliveryForTest inserts a minimal delivery row so facts that
// reference a delivery can be exercised without the webhook path.
func (s *Store) StoreDeliveryForTest(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `insert into webhook_deliveries (delivery_id, event, action, sender_login, payload)
		values ($1, 'issues', 'labeled', 'guy', '{}'::bytea) on conflict do nothing`, id)
	return err
}
