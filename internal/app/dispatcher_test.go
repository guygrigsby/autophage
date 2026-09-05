package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/github"
	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/storetest"
)

func TestRecoveryAbortsOpenAttemptsOnce(t *testing.T) {
	st := storetest.Open(t)
	queueCases(t, st, 1)
	b, _ := resolution.NewBudget(10, time.Hour, 500)
	if _, err := st.UpdateCase(t.Context(), "guy/repo", 1, func(c *resolution.Case) error {
		_, err := c.StartAttempt(resolution.Auto, b, "brief", t0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := &Recovery{Store: st, Clock: fixedClock{t0.Add(time.Minute)}}
	if err := r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetCase(t.Context(), "guy/repo", 1)
	if c.State() != resolution.Queued || c.OpenAttempt() != nil || c.Attempts()[0].Outcome.Reason != resolution.AbortDaemonRestart {
		t.Errorf("after recovery: %s %+v", c.State(), c.Attempts())
	}
}

// TestRecoveryAbortsAClosedCaseAsIssueClosed proves the boot scan reads the
// case before naming a reason. An attempt still open under a Closed case was
// ended by the closure, not by the restart, and calling it daemon_restart
// both misreports it and burns the aggregate's one requeue on a case that
// can never run again.
func TestRecoveryAbortsAClosedCaseAsIssueClosed(t *testing.T) {
	st := storetest.Open(t)
	queueCases(t, st, 1)
	ctx := t.Context()
	b, _ := resolution.NewBudget(10, time.Hour, 500)
	if _, err := st.UpdateCase(ctx, "guy/repo", 1, func(c *resolution.Case) error {
		_, err := c.StartAttempt(resolution.Auto, b, "brief", t0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.StoreDeliveryForTest(ctx, "d-close"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateCase(ctx, "guy/repo", 1, func(c *resolution.Case) error {
		return c.Close(resolution.Closure{DeliveryID: "d-close", ClosedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	r := &Recovery{Store: st, Clock: fixedClock{t0.Add(time.Minute)}}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetCase(ctx, "guy/repo", 1)
	if c.State() != resolution.Closed || c.OpenAttempt() != nil {
		t.Fatalf("after recovery: %s open %+v", c.State(), c.OpenAttempt())
	}
	if got := c.Attempts()[0].Outcome.Reason; got != resolution.AbortIssueClosed {
		t.Errorf("reason = %s, want issue_closed", got)
	}
}

func TestDispatcherSweepWakesOnNotify(t *testing.T) {
	st := storetest.Open(t)
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{}}
	runner := &fakeRunner{release: make(chan struct{})}
	d := &Dispatcher{
		Store:      st,
		Translator: &github.Translator{Store: st, Clock: fixedClock{t0}, ApprovedLabel: "approved", BotLogin: "autophage[bot]"},
		Triage:     &Triage{Store: st, Triager: fakeTriager{size: resolution.Small}, GitHub: gh, Clock: fixedClock{t0}},
		Scheduler:  &Scheduler{Store: st, Runner: runner, Concurrency: 1, Clock: fixedClock{t0}, Budgets: policy(t), GitHub: gh},
		Commenter:  &Commenter{Store: st, GitHub: gh, Label: "approved"},
		Enrollment: &Enrollment{Store: st, GitHub: gh, Label: "approved"},
		Recovery:   &Recovery{Store: st, Clock: fixedClock{t0}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = d.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	r, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := st.EnrollRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	gh.setIssue("guy/repo", 7, issue(true))
	req, _ := resolution.NewRequester("guy", resolution.AssociationOwner)
	c, _ := resolution.NewCase("guy/repo", 7, req, t0)
	if err := st.CreateCase(ctx, c); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runner.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if runner.count() != 1 {
		t.Fatalf("attempt not started by the sweep chain: %d", runner.count())
	}
	if labels := gh.ensuredLabels(); len(labels) != 1 || labels[0] != "guy/repo:approved" {
		t.Errorf("label setup = %v", labels)
	}
	got, _ := st.GetCase(ctx, "guy/repo", 7)
	if got.State() != resolution.Attempting {
		t.Errorf("state = %s", got.State())
	}
	close(runner.release)
	cancel()
	d.Scheduler.Wait()
}

// TestDispatcherFloorRetriesAFailedPost proves the floor cadence makes the
// Commenter's "will retry" true. A post that fails writes nothing to the
// store, so it issues no NOTIFY and nothing else in the daemon will ever
// wake the sweep again on a quiet repository. Only the ticker can.
func TestDispatcherFloorRetriesAFailedPost(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	seed(t, st)
	if _, err := st.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordTriage(resolution.Triage{Size: resolution.Large, Rationale: "touches the auth layer", Model: "m", TriagedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{keyOf("guy/repo", 7): issue(true)}, failPost: true}
	runner := &fakeRunner{release: make(chan struct{})}
	d := &Dispatcher{
		Store:      st,
		Translator: &github.Translator{Store: st, Clock: fixedClock{t0}, ApprovedLabel: "approved", BotLogin: "autophage[bot]"},
		Triage:     &Triage{Store: st, Triager: fakeTriager{size: resolution.Small}, GitHub: gh, Clock: fixedClock{t0}},
		Scheduler:  &Scheduler{Store: st, Runner: runner, Concurrency: 1, Clock: fixedClock{t0}, Budgets: policy(t), GitHub: gh},
		Commenter:  &Commenter{Store: st, GitHub: gh, Label: "approved"},
		Enrollment: &Enrollment{Store: st, GitHub: gh, Label: "approved"},
		Recovery:   &Recovery{Store: st, Clock: fixedClock{t0}},
		Floor:      100 * time.Millisecond,
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = d.Run(runCtx) }()

	// Wait for the comment to have been attempted and refused, twice: the
	// boot sweep and the sweep its own enqueue notified. From here the
	// store is quiet, so nothing but the ticker can drive another attempt.
	waitFor(t, func() bool { return gh.postAttempts() >= 2 }, "the failing post to be attempted")
	gh.setFailPost(false)
	waitFor(t, func() bool { return len(gh.postedComments()) == 1 }, "the retried post to land")
	if posted := gh.postedComments(); !strings.Contains(posted[0], "touches the auth layer") {
		t.Errorf("posted = %q", posted)
	}
	close(runner.release)
	cancel()
	d.Scheduler.Wait()
}

// waitFor polls cond until it holds or the test gives up.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// blockingTriager blocks its first Classify call until told to proceed, and
// signals started the moment that first call is entered, so a test can hold
// the boot sweep's triage step open at a known point.
type blockingTriager struct {
	size    resolution.Size
	started chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (b *blockingTriager) Classify(ctx context.Context, _, _ string) (resolution.Triage, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.proceed:
	case <-ctx.Done():
	}
	return resolution.Triage{Size: b.size, Rationale: "because", Model: "fake", TriagedAt: t0}, nil
}

// TestDispatcherCatchesNotifyDuringBootSweep proves the fix for the boot-time
// NOTIFY loss: Run must subscribe to store notifications before Recovery and
// the boot sweep run, so a case created while the boot sweep is still busy
// with an earlier case is still dispatched, with no further external
// trigger, once the boot sweep finishes.
func TestDispatcherCatchesNotifyDuringBootSweep(t *testing.T) {
	st := storetest.Open(t)
	r, _ := resolution.NewRepository("guy/repo", 42, "main", t0)
	if err := st.EnrollRepository(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	req, _ := resolution.NewRequester("guy", resolution.AssociationOwner)
	c1, _ := resolution.NewCase("guy/repo", 1, req, t0)
	if err := st.CreateCase(t.Context(), c1); err != nil {
		t.Fatal(err)
	}

	gh := &fakeGitHub{issues: map[string]resolution.IssueDetail{
		keyOf("guy/repo", 1): issue(true),
		keyOf("guy/repo", 2): issue(true),
	}}
	runner := &fakeRunner{release: make(chan struct{})}
	triager := &blockingTriager{size: resolution.Small, started: make(chan struct{}), proceed: make(chan struct{})}
	d := &Dispatcher{
		Store:      st,
		Translator: &github.Translator{Store: st, Clock: fixedClock{t0}, ApprovedLabel: "approved", BotLogin: "autophage[bot]"},
		Triage:     &Triage{Store: st, Triager: triager, GitHub: gh, Clock: fixedClock{t0}},
		Scheduler:  &Scheduler{Store: st, Runner: runner, Concurrency: 2, Clock: fixedClock{t0}, Budgets: policy(t), GitHub: gh},
		Commenter:  &Commenter{Store: st, GitHub: gh, Label: "approved"},
		Enrollment: &Enrollment{Store: st, GitHub: gh, Label: "approved"},
		Recovery:   &Recovery{Store: st, Clock: fixedClock{t0}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	select {
	case <-triager.started:
	case <-time.After(5 * time.Second):
		t.Fatal("boot sweep never reached triage")
	}

	// Case 2 is created while the boot sweep is still stuck triaging case 1.
	// The boot sweep already fetched its case list before case 2 existed, so
	// only a subscription active before the sweep started can catch this.
	c2, _ := resolution.NewCase("guy/repo", 2, req, t0)
	if err := st.CreateCase(ctx, c2); err != nil {
		t.Fatal(err)
	}
	close(triager.proceed)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := st.GetCase(ctx, "guy/repo", 2)
		if err == nil && got.State() != resolution.Received {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, err := st.GetCase(ctx, "guy/repo", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.State() == resolution.Received {
		t.Fatalf("case 2 never triaged: state = %s", got.State())
	}
	close(runner.release)
	cancel()
	d.Scheduler.Wait()
}
