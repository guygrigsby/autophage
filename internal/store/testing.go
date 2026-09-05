package store

import "context"

// The two helpers here exist for tests but live in a non-test file so other
// packages' tests can call them. Neither pulls in a test-only dependency;
// the container harness that does lives in internal/storetest.

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

// StoreDeliveryForTest inserts a minimal delivery row so facts that
// reference a delivery can be exercised without the webhook path.
func (s *Store) StoreDeliveryForTest(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `insert into webhook_deliveries (delivery_id, event, action, sender_login, payload)
		values ($1, 'issues', 'labeled', 'guy', '{}'::bytea) on conflict do nothing`, id)
	return err
}
