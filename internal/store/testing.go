package store

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

var (
	testOnce     sync.Once
	testDSN      string
	testExplicit bool
	testErr      error
)

// skipper is the subset of *testing.T that skipOrFatal needs, so the skip
// versus fatal decision is testable without a real *testing.T's Goexit-on-
// Fatal behavior.
type skipper interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// skipOrFatal reports err as a skip when the DSN came from the
// testcontainers-managed database (any failure there, whatever its message,
// is environmental: Docker absent, daemon unreachable, image pull failed),
// or as a fatal failure when the operator supplied AUTOPHAGE_TEST_DSN
// explicitly (a real, actionable failure).
func skipOrFatal(t skipper, err error, explicitDSN bool) {
	t.Helper()
	if explicitDSN {
		t.Fatalf("open store: %v", err)
		return
	}
	t.Skipf("postgres testcontainer unavailable (%v); set AUTOPHAGE_TEST_DSN to use a server", err)
}

// OpenTest returns a migrated Store on a real Postgres 17 started once per
// test binary through testcontainers, with every non-vocabulary table
// truncated when the test ends. It skips when Docker is unreachable and
// AUTOPHAGE_TEST_DSN is unset; set that variable to use an existing server.
func OpenTest(t *testing.T) *Store {
	t.Helper()
	testOnce.Do(func() {
		if dsn := os.Getenv("AUTOPHAGE_TEST_DSN"); dsn != "" {
			testDSN = dsn
			testExplicit = true
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
		skipOrFatal(t, testErr, testExplicit)
	}
	s, err := Open(t.Context(), testDSN)
	if err != nil {
		skipOrFatal(t, err, testExplicit)
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
