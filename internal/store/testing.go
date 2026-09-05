package store

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

var (
	testOnce sync.Once
	testDSN  string
	testErr  error
)

// OpenTest returns a migrated Store on a real Postgres 17 started once per
// test binary through testcontainers, with every non-vocabulary table
// truncated when the test ends. It skips when Docker is unreachable and
// AUTOPHAGE_TEST_DSN is unset; set that variable to use an existing server.
func OpenTest(t *testing.T) *Store {
	t.Helper()
	testOnce.Do(func() {
		if dsn := os.Getenv("AUTOPHAGE_TEST_DSN"); dsn != "" {
			testDSN = dsn
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
			tcpostgres.WithDatabase("autophage"), tcpostgres.WithUsername("autophage"), tcpostgres.WithPassword("autophage"),
			tcpostgres.BasicWaitStrategies())
		if err != nil {
			testErr = err
			return
		}
		testDSN, testErr = ctr.ConnectionString(ctx, "sslmode=disable")
	})
	if testErr != nil {
		if strings.Contains(testErr.Error(), "Cannot connect to the Docker daemon") || strings.Contains(testErr.Error(), "docker") {
			t.Skipf("no Docker for testcontainers (%v); set AUTOPHAGE_TEST_DSN to use a server", testErr)
		}
		t.Fatalf("start postgres: %v", testErr)
	}
	s, err := Open(t.Context(), testDSN)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Truncate(ctx)
		s.Close()
	})
	return s
}

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
