package app

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// BudgetPolicy is the two named budgets from config.
type BudgetPolicy struct {
	Auto     resolution.Budget
	Approved resolution.Budget
}

// For picks the budget for an attempt kind.
func (p BudgetPolicy) For(kind resolution.AttemptKind) resolution.Budget {
	if kind == resolution.Approved {
		return p.Approved
	}
	return p.Auto
}

// Scheduler is the domain service spanning cases: at most Concurrency open
// attempts, Queued cases started oldest first, none for removed
// repositories (QueuedCases already excludes them).
type Scheduler struct {
	Store       *store.Store
	Runner      resolution.Runner
	Concurrency int
	Clock       resolution.Clock
	Budgets     BudgetPolicy
	GitHub      resolution.GitHub

	wg sync.WaitGroup

	mu      sync.Mutex
	running map[string]struct{}
}

// Run starts attempts for Queued cases until the concurrency limit is
// reached. Each started attempt runs on its own goroutine through Runner.
func (s *Scheduler) Run(ctx context.Context) error {
	open, err := s.Store.OpenAttempts(ctx)
	if err != nil {
		return err
	}
	slots := s.Concurrency - len(open)
	if slots <= 0 {
		return nil
	}
	queued, err := s.Store.QueuedCases(ctx)
	if err != nil {
		return err
	}
	for _, k := range queued {
		if slots == 0 {
			break
		}
		started, err := s.start(ctx, k)
		if err != nil {
			log.Printf("schedule %s#%d: %v", k.Repository, k.Number, err)
			continue
		}
		if started {
			slots--
		}
	}
	return nil
}

// start opens the attempt for one queued case and hands it to the runner.
// A case whose issue is closed on GitHub is skipped; the closing delivery
// will move it.
func (s *Scheduler) start(ctx context.Context, k store.CaseKey) (bool, error) {
	repo, err := s.Store.GetRepository(ctx, k.Repository)
	if err != nil {
		return false, err
	}
	detail, err := s.GitHub.GetIssue(ctx, k.Repository, k.Number)
	if err != nil {
		return false, err
	}
	if !detail.Open {
		log.Printf("schedule %s#%d: issue is closed on GitHub, waiting for the delivery", k.Repository, k.Number)
		return false, nil
	}
	c, err := s.Store.UpdateCase(ctx, k.Repository, k.Number, func(c *resolution.Case) error {
		kind := c.NextAttemptKind()
		budget := s.Budgets.For(kind)
		var prior *resolution.Outcome
		if as := c.Attempts(); len(as) > 0 {
			prior = as[len(as)-1].Outcome
		}
		brief, err := resolution.BuildBrief(resolution.BriefInput{Repository: repo, Case: c, Kind: kind, Budget: budget, IssueTitle: detail.Title, IssueBody: detail.Body, Prior: prior})
		if err != nil {
			return err
		}
		_, err = c.StartAttempt(kind, budget, brief, s.Clock.Now())
		return err
	})
	if err != nil {
		return false, err
	}
	var attemptID string
	if a := c.OpenAttempt(); a != nil {
		attemptID = a.ID
	}
	s.mark(attemptID, true)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.mark(attemptID, false)
		defer s.backstop(ctx, attemptID)
		s.Runner.Run(ctx, attemptID)
	}()
	return true, nil
}

// backstop is the runner's deferred supervisor. Nothing else in a live
// daemon ends an attempt, so a runner that panics or returns without
// recording an outcome would leave the case in Attempting until the next
// restart's boot scan. Recording Failed{Infra} here keeps the state machine
// moving and says why in the comment the operator sees. It runs deferred so
// recover() catches the runner's panic instead of it killing the daemon.
func (s *Scheduler) backstop(ctx context.Context, attemptID string) {
	message := "runner returned without recording an outcome"
	if v := recover(); v != nil {
		message = fmt.Sprintf("runner panicked: %v", v)
		log.Printf("attempt %s: %s\n%s", attemptID, message, debug.Stack())
	}
	// Stand down while the daemon is going down, after recovering the panic
	// but before recording anything. The runner's contract is that a
	// shutdown under a running attempt records no outcome, so Recovery can
	// end it with Aborted{DaemonRestart} on the next boot and re-queue the
	// case; a backstop that wrote here would spend that answer on
	// Failed{Infra} and park the case for a human instead.
	if ctx.Err() != nil {
		return
	}
	c, err := s.Store.GetCaseByAttempt(ctx, attemptID)
	if err != nil {
		log.Printf("backstop attempt %s: %v", attemptID, err)
		return
	}
	// The ordinary path: the runner recorded its own outcome, so the
	// attempt is closed and there is nothing to say.
	if a := c.OpenAttempt(); a == nil || a.ID != attemptID {
		return
	}
	o, err := resolution.OutcomeFailed(resolution.FailureInfra, message, resolution.Usage{}, s.Clock.Now())
	if err != nil {
		log.Printf("backstop attempt %s: %v", attemptID, err)
		return
	}
	if _, err := s.Store.UpdateCase(ctx, c.Repository(), c.Number(), func(c *resolution.Case) error {
		return c.RecordOutcome(attemptID, o)
	}); err != nil {
		log.Printf("backstop attempt %s: %v", attemptID, err)
	}
}

func (s *Scheduler) mark(attemptID string, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = map[string]struct{}{}
	}
	if running {
		s.running[attemptID] = struct{}{}
		return
	}
	delete(s.running, attemptID)
}

// Running lists the attempts whose runner has not returned, for the
// shutdown log.
func (s *Scheduler) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.running))
	for id := range s.running {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Wait blocks until every started runner returns.
func (s *Scheduler) Wait() { s.wg.Wait() }

// WaitTimeout blocks until every started runner returns or d elapses,
// reporting whether they all returned. Shutdown is bounded: a runner stuck
// on a hung podman call must not hold the daemon open for ever.
func (s *Scheduler) WaitTimeout(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}
