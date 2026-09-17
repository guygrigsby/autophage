package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/github"
	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
	"github.com/guygrigsby/autophage/internal/upkeep"
)

const (
	repairHeadA = "1111111111111111111111111111111111111111"
	repairHeadB = "2222222222222222222222222222222222222222"
)

type fakeRepairRunner struct {
	mu      sync.Mutex
	started []string
	release chan struct{}
}

func (f *fakeRepairRunner) Run(ctx context.Context, repairID string) {
	f.mu.Lock()
	f.started = append(f.started, repairID)
	f.mu.Unlock()
	select {
	case <-f.release:
	case <-ctx.Done():
	}
}

type fakeBumpGitHub struct {
	pulls map[int]github.PullRequestDetail
	err   error
}

func (f *fakeBumpGitHub) GetPullRequest(_ context.Context, _ string, number int) (github.PullRequestDetail, error) {
	if f.err != nil {
		return github.PullRequestDetail{}, f.err
	}
	pr, ok := f.pulls[number]
	if !ok {
		return github.PullRequestDetail{}, github.ErrPullRequestNotFound
	}
	return pr, nil
}

func repairPolicy(t *testing.T) BudgetPolicy {
	t.Helper()
	p := policy(t)
	r, _ := resolution.NewBudget(15, 2*time.Hour, 800)
	p.Repair = r
	return p
}

// queueBumps enrols and watches guy/repo, then opens numbers as bumps and
// fails their checks, oldest queued first.
func queueBumps(t *testing.T, st *store.Store, numbers ...int) *fakeBumpGitHub {
	t.Helper()
	r, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := st.EnrollRepository(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	w, _ := upkeep.NewWatch("guy/repo", t0)
	if err := st.WatchRepository(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	gh := &fakeBumpGitHub{pulls: map[int]github.PullRequestDetail{}}
	for i, n := range numbers {
		b, err := upkeep.NewBump("guy/repo", n, "dependabot/go_modules/y", "main", repairHeadA, t0)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CreateBump(t.Context(), b); err != nil {
			t.Fatal(err)
		}
		v := upkeep.CheckVerdict{HeadSha: repairHeadA, Conclusion: upkeep.CheckFailure, FailingContexts: "test (1.26)", ConcludedAt: t0.Add(time.Duration(i) * time.Minute)}
		if _, err := st.UpdateBump(t.Context(), "guy/repo", n, func(b *upkeep.Bump) error { return b.RecordVerdict(v, 3) }); err != nil {
			t.Fatal(err)
		}
		gh.pulls[n] = github.PullRequestDetail{Title: "Bump y from 1.2.2 to 1.2.3", HeadBranch: "dependabot/go_modules/y", HeadSha: repairHeadA, BaseBranch: "main", AuthorLogin: "dependabot[bot]", Open: true}
	}
	return gh
}

func TestRepairSchedulerStartsInQueuedOrder(t *testing.T) {
	st := storetest.Open(t)
	gh := queueBumps(t, st, 11, 12, 13)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := &fakeRepairRunner{release: make(chan struct{})}
	s := &RepairScheduler{Store: st, Runner: runner, GitHub: gh, Concurrency: 2, Clock: fixedClock{t0}, Budgets: repairPolicy(t), RoundCap: 3}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Asserted on the store, not on the runner's counter: the round row is
	// written before the goroutine is spawned, so the counter is a race and
	// the row is the fact.
	open, err := st.OpenRepairs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("open rounds = %d, want 2 (concurrency)", len(open))
	}
	for _, n := range []int{11, 12} {
		b, err := st.GetBump(t.Context(), "guy/repo", n)
		if err != nil {
			t.Fatal(err)
		}
		if b.State() != upkeep.Repairing {
			t.Errorf("#%d state = %s, want repairing", n, b.State())
		}
		a := b.Attempts()
		if len(a) != 1 || a[0].BaseSha != repairHeadA {
			t.Errorf("#%d round = %+v", n, a)
		}
		if a[0].Brief == "" {
			t.Errorf("#%d started with an empty brief", n)
		}
	}
	b, err := st.GetBump(t.Context(), "guy/repo", 13)
	if err != nil {
		t.Fatal(err)
	}
	if b.State() != upkeep.Queued {
		t.Errorf("#13 state = %s, want queued: concurrency was 2", b.State())
	}
	// A shutdown, so the backstop stands down and the rounds stay open for
	// recovery, which is the runner's real contract.
	cancel()
	s.Wait()
}

// One sandbox pool serves both halves, so a repair round may not start on a
// slot an issue attempt is already holding.
func TestRepairSchedulerSharesConcurrencyWithIssueAttempts(t *testing.T) {
	st := storetest.Open(t)
	gh := queueBumps(t, st, 11)
	queueCases(t, st, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	issueGH := &fakeGitHub{issues: map[string]resolution.IssueDetail{keyOf("guy/repo", 1): issue(true)}}
	issueRunner := &fakeRunner{release: make(chan struct{})}
	is := &Scheduler{Store: st, Runner: issueRunner, Concurrency: 1, Clock: fixedClock{t0}, Budgets: repairPolicy(t), GitHub: issueGH}
	if err := is.Run(ctx); err != nil {
		t.Fatal(err)
	}
	attempts, err := st.OpenAttempts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("open issue attempts = %d, want 1", len(attempts))
	}

	repairRunner := &fakeRepairRunner{release: make(chan struct{})}
	rs := &RepairScheduler{Store: st, Runner: repairRunner, GitHub: gh, Concurrency: 1, Clock: fixedClock{t0}, Budgets: repairPolicy(t), RoundCap: 3}
	if err := rs.Run(ctx); err != nil {
		t.Fatal(err)
	}
	rounds, err := st.OpenRepairs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 0 {
		t.Errorf("open repair rounds = %d, want 0: the one slot is held by an issue attempt", len(rounds))
	}
	cancel()
	is.Wait()
	rs.Wait()
}

// Unwatching stops new rounds without touching the bumps already open.
func TestRepairSchedulerSkipsAnUnwatchedRepository(t *testing.T) {
	st := storetest.Open(t)
	gh := queueBumps(t, st, 11)
	if err := st.UnwatchRepository(t.Context(), "guy/repo"); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRepairRunner{release: make(chan struct{})}
	s := &RepairScheduler{Store: st, Runner: runner, GitHub: gh, Concurrency: 2, Clock: fixedClock{t0}, Budgets: repairPolicy(t), RoundCap: 3}
	if err := s.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(runner.release)
	s.Wait()
	assertNoRounds(t, st)
}

// A pull request that closed while the bump sat queued has nothing to
// repair; the closing delivery will move the bump.
func TestRepairSchedulerSkipsAClosedPullRequest(t *testing.T) {
	st := storetest.Open(t)
	gh := queueBumps(t, st, 11)
	pr := gh.pulls[11]
	pr.Open = false
	gh.pulls[11] = pr
	runner := &fakeRepairRunner{release: make(chan struct{})}
	s := &RepairScheduler{Store: st, Runner: runner, GitHub: gh, Concurrency: 2, Clock: fixedClock{t0}, Budgets: repairPolicy(t), RoundCap: 3}
	if err := s.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(runner.release)
	s.Wait()
	assertNoRounds(t, st)
}

// A head that moved between the verdict and the slot means the verdict is
// about a tree that no longer exists; the synchronize delivery restarts the
// wait, so the round must not start on the stale sha.
func TestRepairSchedulerSkipsWhenTheHeadMovedUnderIt(t *testing.T) {
	st := storetest.Open(t)
	gh := queueBumps(t, st, 11)
	pr := gh.pulls[11]
	pr.HeadSha = repairHeadB
	gh.pulls[11] = pr
	runner := &fakeRepairRunner{release: make(chan struct{})}
	s := &RepairScheduler{Store: st, Runner: runner, GitHub: gh, Concurrency: 2, Clock: fixedClock{t0}, Budgets: repairPolicy(t), RoundCap: 3}
	if err := s.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(runner.release)
	s.Wait()
	assertNoRounds(t, st)
}

func TestStaleCheckSweeperAbandonsAfterTheWindow(t *testing.T) {
	st := storetest.Open(t)
	queueBumps(t, st) // enrol and watch, no bumps
	b, err := upkeep.NewBump("guy/repo", 21, "dependabot/x", "main", repairHeadA, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateBump(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	sw := &StaleCheckSweeper{Store: st, Clock: fixedClock{t0.Add(7 * time.Hour)}, Window: 6 * time.Hour}
	if err := sw.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetBump(t.Context(), "guy/repo", 21)
	if err != nil {
		t.Fatal(err)
	}
	if got.State() != upkeep.Abandoned {
		t.Fatalf("state = %s, want abandoned", got.State())
	}
	ab := got.Abandonment()
	if ab == nil || ab.Reason != upkeep.AbandonChecksNeverConcluded {
		t.Errorf("abandonment = %+v", ab)
	}
}

func TestStaleCheckSweeperLeavesAFreshWaitAlone(t *testing.T) {
	st := storetest.Open(t)
	queueBumps(t, st)
	b, _ := upkeep.NewBump("guy/repo", 21, "dependabot/x", "main", repairHeadA, t0)
	if err := st.CreateBump(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	sw := &StaleCheckSweeper{Store: st, Clock: fixedClock{t0.Add(time.Hour)}, Window: 6 * time.Hour}
	if err := sw.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetBump(t.Context(), "guy/repo", 21)
	if got.State() != upkeep.AwaitingChecks {
		t.Errorf("state = %s, want awaiting_checks", got.State())
	}
}

func TestBumpRecoveryEndsOpenRounds(t *testing.T) {
	st := storetest.Open(t)
	gh := queueBumps(t, st, 11)
	ctx, cancel := context.WithCancel(t.Context())
	runner := &fakeRepairRunner{release: make(chan struct{})}
	s := &RepairScheduler{Store: st, Runner: runner, GitHub: gh, Concurrency: 1, Clock: fixedClock{t0}, Budgets: repairPolicy(t), RoundCap: 3}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// A shutdown under a running round: the backstop stands down and leaves
	// the round open, which is exactly what recovery exists to clean up.
	cancel()
	s.Wait()

	rec := &BumpRecovery{Store: st, Clock: fixedClock{t0.Add(time.Hour)}, RoundCap: 3}
	if err := rec.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	b, err := st.GetBump(t.Context(), "guy/repo", 11)
	if err != nil {
		t.Fatal(err)
	}
	if b.State() != upkeep.Queued {
		t.Errorf("state = %s, want queued: the first restart re-queues", b.State())
	}
	o := b.Attempts()[0].Outcome
	if o == nil || o.Kind != upkeep.RepairAborted || o.Reason != upkeep.AbortDaemonRestart {
		t.Errorf("outcome = %+v", o)
	}
	open, err := st.OpenRepairs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("open rounds = %d, want 0", len(open))
	}
}

// A round still open under a closed bump was running when the pull request
// closed. Calling that a daemon restart both reads wrong and spends the
// requeue-once allowance on a bump that will never run again.
func TestBumpRecoveryNamesAClosedPullRequest(t *testing.T) {
	st := storetest.Open(t)
	gh := queueBumps(t, st, 11)
	ctx, cancel := context.WithCancel(t.Context())
	runner := &fakeRepairRunner{release: make(chan struct{})}
	s := &RepairScheduler{Store: st, Runner: runner, GitHub: gh, Concurrency: 1, Clock: fixedClock{t0}, Budgets: repairPolicy(t), RoundCap: 3}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// A shutdown under a running round: the backstop stands down and leaves
	// the round open, which is exactly what recovery exists to clean up.
	cancel()
	s.Wait()
	if err := st.StoreDeliveryForTest(t.Context(), "d-close"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateBump(t.Context(), "guy/repo", 11, func(b *upkeep.Bump) error {
		return b.Close(upkeep.BumpClosure{Kind: upkeep.Merged, DeliveryID: "d-close", ClosedAt: t0.Add(time.Minute)})
	}); err != nil {
		t.Fatal(err)
	}
	rec := &BumpRecovery{Store: st, Clock: fixedClock{t0.Add(time.Hour)}, RoundCap: 3}
	if err := rec.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	b, _ := st.GetBump(t.Context(), "guy/repo", 11)
	o := b.Attempts()[0].Outcome
	if o == nil || o.Reason != upkeep.AbortPullRequestClosed {
		t.Errorf("outcome = %+v, want pull_request_closed", o)
	}
	if b.State() != upkeep.Closed {
		t.Errorf("state = %s, want closed", b.State())
	}
}

// assertNoRounds is the "nothing started" check, read from the store rather
// than the runner's counter: the row lands before the goroutine is spawned.
func assertNoRounds(t *testing.T, st *store.Store) {
	t.Helper()
	open, err := st.OpenRepairs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("open rounds = %d, want 0", len(open))
	}
}
