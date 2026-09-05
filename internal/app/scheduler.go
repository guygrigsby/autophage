package app

import (
	"context"
	"log"
	"sync"

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
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.Runner.Run(ctx, attemptID)
	}()
	return true, nil
}

// Wait blocks until every started runner returns; the daemon calls it on
// shutdown.
func (s *Scheduler) Wait() { s.wg.Wait() }
