package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/guygrigsby/autophage/internal/github"
	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/upkeep"
)

// RepairRunner executes one started repair round end to end and records its
// run and outcome through the store.
type RepairRunner interface {
	Run(ctx context.Context, repairID string)
}

// BumpGitHub is the slice of the GitHub boundary the Upkeep services need.
type BumpGitHub interface {
	GetPullRequest(ctx context.Context, repository string, number int) (github.PullRequestDetail, error)
}

// RepairScheduler is the domain service spanning bumps: queued bumps start
// oldest first by the transition that queued them, none for a repository
// with no watch row, none past the round cap.
//
// Concurrency is shared with the issue side rather than owned here. One
// sandbox pool serves both, so the count subtracts open issue attempts as
// well as open rounds. Nothing prioritises between the two: whichever
// scheduler the sweep reaches first takes the free slots, which ADR 0007
// records as the operational risk rather than pretending it is solved.
type RepairScheduler struct {
	Store       *store.Store
	Runner      RepairRunner
	GitHub      BumpGitHub
	Concurrency int
	Clock       resolution.Clock
	Budgets     BudgetPolicy
	RoundCap    int

	wg sync.WaitGroup

	mu      sync.Mutex
	running map[string]struct{}
}

// Run starts rounds for queued bumps until the shared concurrency limit is
// reached.
func (s *RepairScheduler) Run(ctx context.Context) error {
	slots, err := freeSlots(ctx, s.Store, s.Concurrency)
	if err != nil || slots <= 0 {
		return err
	}
	queued, err := s.Store.QueuedBumps(ctx)
	if err != nil {
		return err
	}
	for _, k := range queued {
		if slots == 0 {
			break
		}
		started, err := s.start(ctx, k)
		if err != nil {
			log.Printf("repair %s#%d: %v", k.Repository, k.Number, err)
			continue
		}
		if started {
			slots--
		}
	}
	return nil
}

// freeSlots is how many agent runs may still start. Both halves are counted
// because there is one sandbox pool; a repair round that ignored issue
// attempts would oversubscribe it by exactly the number running.
func freeSlots(ctx context.Context, st *store.Store, concurrency int) (int, error) {
	attempts, err := st.OpenAttempts(ctx)
	if err != nil {
		return 0, err
	}
	rounds, err := st.OpenRepairs(ctx)
	if err != nil {
		return 0, err
	}
	return concurrency - len(attempts) - len(rounds), nil
}

// start opens the next round for one queued bump and hands it to the runner.
func (s *RepairScheduler) start(ctx context.Context, k store.BumpKey) (bool, error) {
	watched, err := s.Store.WatchedRepositories(ctx)
	if err != nil {
		return false, err
	}
	if !contains(watched, k.Repository) {
		// Unwatched since the bump was queued. The bump keeps its history;
		// it simply stops getting new rounds.
		return false, nil
	}
	pr, err := s.GitHub.GetPullRequest(ctx, k.Repository, k.Number)
	if errors.Is(err, github.ErrPullRequestNotFound) {
		log.Printf("repair %s#%d: pull request is gone on GitHub", k.Repository, k.Number)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !pr.Open {
		log.Printf("repair %s#%d: pull request is closed on GitHub, waiting for the delivery", k.Repository, k.Number)
		return false, nil
	}
	current, err := s.Store.GetBump(ctx, k.Repository, k.Number)
	if err != nil {
		return false, err
	}
	if pr.HeadSha != current.HeadSha() {
		// The branch moved between the verdict and this slot. The verdict
		// describes a tree that no longer exists, and the synchronize
		// delivery is on its way to restart the wait.
		log.Printf("repair %s#%d: head moved to %s, waiting for the delivery", k.Repository, k.Number, pr.HeadSha)
		return false, nil
	}
	b, err := s.Store.UpdateBump(ctx, k.Repository, k.Number, func(b *upkeep.Bump) error {
		budget := s.Budgets.Repair
		var prior *upkeep.RepairOutcome
		if rounds := b.Attempts(); len(rounds) > 0 {
			prior = rounds[len(rounds)-1].Outcome
		}
		brief, err := upkeep.BuildRepairBrief(upkeep.RepairBriefInput{
			Bump: b, Round: b.Rounds() + 1, Budget: budget,
			Verdict: b.CurrentVerdict(), PullRequestTitle: pr.Title, Prior: prior,
		})
		if err != nil {
			return err
		}
		_, err = b.StartRepair(budget, brief, s.Clock.Now(), s.RoundCap)
		return err
	})
	if err != nil {
		return false, err
	}
	var repairID string
	if a := b.OpenRepair(); a != nil {
		repairID = a.ID
	}
	s.mark(repairID, true)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.mark(repairID, false)
		defer s.backstop(ctx, repairID)
		s.Runner.Run(ctx, repairID)
	}()
	return true, nil
}

// backstop is the runner's deferred supervisor, the same contract the issue
// side has: nothing else in a live daemon ends a round, so a runner that
// panics or returns without recording an outcome would leave the bump
// repairing until the next restart's boot scan.
func (s *RepairScheduler) backstop(ctx context.Context, repairID string) {
	message := "runner returned without recording an outcome"
	if v := recover(); v != nil {
		message = fmt.Sprintf("runner panicked: %v", v)
		log.Printf("repair %s: %s\n%s", repairID, message, debug.Stack())
	}
	// Stand down while the daemon is going down, so BumpRecovery can end the
	// round with Aborted{DaemonRestart} on the next boot and re-queue the
	// bump. Writing Failed{Infra} here would spend that answer instead.
	if ctx.Err() != nil {
		return
	}
	b, err := s.Store.GetBumpByRepair(ctx, repairID)
	if err != nil {
		log.Printf("backstop repair %s: %v", repairID, err)
		return
	}
	if a := b.OpenRepair(); a == nil || a.ID != repairID {
		return
	}
	o := upkeep.RepairOutcome{Kind: upkeep.RepairFailed, Class: resolution.FailureInfra, Message: message, Summary: message, EndedAt: s.Clock.Now()}
	if _, err := s.Store.UpdateBumpByRepair(ctx, repairID, func(b *upkeep.Bump) error {
		return b.RecordOutcome(repairID, o, s.RoundCap)
	}); err != nil {
		log.Printf("backstop repair %s: %v", repairID, err)
	}
}

func (s *RepairScheduler) mark(repairID string, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = map[string]struct{}{}
	}
	if running {
		s.running[repairID] = struct{}{}
		return
	}
	delete(s.running, repairID)
}

// Running lists the rounds whose runner has not returned, for the shutdown
// log.
func (s *RepairScheduler) Running() []string {
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
func (s *RepairScheduler) Wait() { s.wg.Wait() }

// WaitTimeout blocks until every started runner returns or d elapses.
func (s *RepairScheduler) WaitTimeout(d time.Duration) bool {
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

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// StaleCheckSweeper abandons a bump whose checks never concluded. Without it
// a repository that runs no checks on pull requests, or one whose workflow
// is stuck, leaves bumps awaiting_checks for ever: nothing else in the
// system has a reason to look at them again.
type StaleCheckSweeper struct {
	Store  *store.Store
	Clock  resolution.Clock
	Window time.Duration
}

// Run abandons every bump that has been awaiting checks longer than Window.
// The wait starts at the latest transition into awaiting_checks, so a repair
// push restarts the clock rather than inheriting the original wait.
func (s *StaleCheckSweeper) Run(ctx context.Context) error {
	if s.Window <= 0 {
		return nil
	}
	cutoff := s.Clock.Now().Add(-s.Window)
	stale, err := s.Store.BumpsAwaitingChecksSince(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, k := range stale {
		detail := fmt.Sprintf("no conclusive check rollup within %s", s.Window)
		_, err := s.Store.UpdateBump(ctx, k.Repository, k.Number, func(b *upkeep.Bump) error {
			return b.Abandon(upkeep.BumpAbandonment{Reason: upkeep.AbandonChecksNeverConcluded, Detail: detail, AbandonedAt: s.Clock.Now()})
		})
		if err != nil {
			log.Printf("stale checks %s#%d: %v", k.Repository, k.Number, err)
		}
	}
	return nil
}

// BumpRecovery ends every repair round the previous daemon left open. A
// round never resumes mid-run; the bump re-queues once through the
// aggregate rule.
type BumpRecovery struct {
	Store *store.Store
	Clock resolution.Clock
	// RoundCap is the same number the scheduler uses. Recovery needs it
	// because the aggregate refuses to re-queue a bump the cap would never
	// let run again: without it every restart would park bumps at their cap
	// in queued for ever instead of abandoning them.
	RoundCap int
}

// Run ends every open round. A failure on one is logged; the rest still get
// ended.
func (r *BumpRecovery) Run(ctx context.Context) error {
	open, err := r.Store.OpenRepairs(ctx)
	if err != nil {
		return err
	}
	for _, o := range open {
		if err := r.one(ctx, o); err != nil {
			log.Printf("bump recovery %s#%d round %d: %v", o.Repository, o.Number, o.Round, err)
		}
	}
	return nil
}

// one ends a single round. The reason follows the bump: a round still open
// under a closed bump was running when the pull request closed, and naming
// that daemon_restart both reads wrong to the operator and spends the
// aggregate's requeue-once allowance on a bump that will never run again.
func (r *BumpRecovery) one(ctx context.Context, open store.OpenRepair) error {
	_, err := r.Store.UpdateBump(ctx, open.Repository, open.Number, func(b *upkeep.Bump) error {
		reason, summary := upkeep.AbortDaemonRestart, "The daemon restarted during the repair round."
		if b.State() == upkeep.Closed {
			reason, summary = upkeep.AbortPullRequestClosed, "The pull request closed while the repair round was running."
		}
		return b.RecordOutcome(open.ID, upkeep.RepairOutcome{
			Kind: upkeep.RepairAborted, Reason: reason, Summary: summary, EndedAt: r.Clock.Now(),
		}, r.RoundCap)
	})
	return err
}
