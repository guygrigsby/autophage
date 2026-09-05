package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// fakeRunner records attempts and blocks each until released, so the
// concurrency limit is observable.
type fakeRunner struct {
	mu      sync.Mutex
	started []string
	release chan struct{}
}

func (f *fakeRunner) Run(ctx context.Context, attemptID string) {
	f.mu.Lock()
	f.started = append(f.started, attemptID)
	f.mu.Unlock()
	select {
	case <-f.release:
	case <-ctx.Done():
	}
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

func queueCases(t *testing.T, st *store.Store, numbers ...int) {
	t.Helper()
	r, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := st.EnrollRepository(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	req, _ := resolution.NewRequester("guy", resolution.AssociationOwner)
	for i, n := range numbers {
		c, _ := resolution.NewCase("guy/repo", n, req, t0)
		if err := st.CreateCase(t.Context(), c); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateCase(t.Context(), "guy/repo", n, func(c *resolution.Case) error {
			return c.RecordTriage(resolution.Triage{Size: resolution.Small, Rationale: "r", Model: "m", TriagedAt: t0.Add(time.Duration(i) * time.Minute)})
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func policy(t *testing.T) BudgetPolicy {
	t.Helper()
	a, _ := resolution.NewBudget(10, time.Hour, 500)
	b, _ := resolution.NewBudget(40, 4*time.Hour, 2000)
	return BudgetPolicy{Auto: a, Approved: b}
}

func TestSchedulerStartsInOrderWithinConcurrency(t *testing.T) {
	st := store.OpenTest(t)
	queueCases(t, st, 1, 2, 3)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{}}
	for _, n := range []int{1, 2, 3} {
		gh.issues[keyOf("guy/repo", n)] = issue(true)
	}
	runner := &fakeRunner{release: make(chan struct{})}
	s := &Scheduler{Store: st, Runner: runner, Concurrency: 2, Clock: fixedClock{t0}, Budgets: policy(t), GitHub: gh}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runner.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runner.count() != 2 {
		t.Fatalf("started %d, want 2", runner.count())
	}
	open, _ := st.OpenAttempts(t.Context())
	if len(open) != 2 || open[0].Number != 1 || open[1].Number != 2 {
		t.Errorf("open = %+v", open)
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if runner.count() != 2 {
		t.Errorf("exceeded concurrency: %d", runner.count())
	}
	c, _ := st.GetCase(t.Context(), "guy/repo", 1)
	a := c.OpenAttempt()
	if a == nil || a.Kind != resolution.Auto || a.Budget.MaxTurns() != 10 || a.Brief == "" {
		t.Errorf("attempt = %+v", a)
	}
	close(runner.release)
	s.Wait()
}

func TestSchedulerSkipsClosedIssue(t *testing.T) {
	st := store.OpenTest(t)
	queueCases(t, st, 1)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{keyOf("guy/repo", 1): issue(false)}}
	runner := &fakeRunner{release: make(chan struct{})}
	s := &Scheduler{Store: st, Runner: runner, Concurrency: 2, Clock: fixedClock{t0}, Budgets: policy(t), GitHub: gh}
	if err := s.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if runner.count() != 0 {
		t.Error("closed issue was started")
	}
	c, _ := st.GetCase(t.Context(), "guy/repo", 1)
	if c.State() != resolution.Queued {
		t.Errorf("state = %s", c.State())
	}
}
