package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/storetest"
)

func seedRepo(t *testing.T, s *store.Store, name string) {
	t.Helper()
	r, _ := resolution.NewRepository(name, 42, "main", t0)
	if err := s.EnrollRepository(t.Context(), r); err != nil {
		t.Fatal(err)
	}
}

func owner(t *testing.T) resolution.Requester {
	t.Helper()
	r, err := resolution.NewRequester("guy", resolution.AssociationOwner)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newCase(t *testing.T, s *store.Store, number int, req resolution.Requester) *resolution.Case {
	t.Helper()
	c, err := resolution.NewCase("guy/repo", number, req, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCase(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreateAndGetCase(t *testing.T) {
	s := storetest.Open(t)
	seedRepo(t, s, "guy/repo")
	c := newCase(t, s, 7, owner(t))
	if c.ID() == "" {
		t.Fatal("id not assigned")
	}
	got, err := s.GetCase(t.Context(), "guy/repo", 7)
	if err != nil || got.ID() != c.ID() || got.State() != resolution.Received || got.Requester().Login != "guy" {
		t.Fatalf("get = %+v %v", got, err)
	}
	if err := s.CreateCase(t.Context(), c); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate = %v", err)
	}
	if _, err := s.GetCase(t.Context(), "guy/repo", 8); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
}

func TestUpdateCasePersistsEveryFact(t *testing.T) {
	s := storetest.Open(t)
	seedRepo(t, s, "guy/repo")
	newCase(t, s, 7, owner(t))
	ctx := t.Context()
	b, _ := resolution.NewBudget(10, time.Hour, 500)

	c, err := s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordTriage(resolution.Triage{Size: resolution.Small, Rationale: "typo", Model: "m", TriagedAt: t0})
	})
	if err != nil || c.State() != resolution.Queued || c.Triage() == nil {
		t.Fatalf("triage: %v %+v", err, c)
	}
	c, err = s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		_, err := c.StartAttempt(resolution.Auto, b, "brief", t0.Add(time.Minute))
		return err
	})
	if err != nil || c.OpenAttempt() == nil || c.OpenAttempt().ID == "" {
		t.Fatalf("start: %v %+v", err, c.OpenAttempt())
	}
	attemptID := c.OpenAttempt().ID
	c, err = s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordRun(attemptID, resolution.Run{RunID: "run-1", Model: "m", BaseSha: "0123456789abcdef0123456789abcdef01234567", BeganAt: t0.Add(2 * time.Minute)})
	})
	if err != nil || c.OpenAttempt().Run == nil || c.OpenAttempt().Run.RunID != "run-1" {
		t.Fatalf("run: %v %+v", err, c.OpenAttempt())
	}
	o, _ := resolution.OutcomeExhausted(resolution.LimitTurns, "ran out", resolution.Usage{Turns: 10, InputTokens: 100, OutputTokens: 20, WallClock: 3 * time.Minute, DiffLines: 12}, t0.Add(3*time.Minute))
	c, err = s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error { return c.RecordOutcome(attemptID, o) })
	if err != nil || c.State() != resolution.AwaitingApproval {
		t.Fatalf("outcome: %v %s", err, c.State())
	}
	got := c.Attempts()[0].Outcome
	if got == nil || got.Kind != resolution.BudgetExhausted || got.Limit != resolution.LimitTurns || got.Usage.InputTokens != 100 || got.Summary != "ran out" {
		t.Errorf("outcome loaded = %+v", got)
	}
	appr := resolution.Approval{Approver: owner(t), Source: resolution.SourceLabel, ApprovedAt: t0.Add(4 * time.Minute), DeliveryID: "d-1"}
	if err := s.StoreDeliveryForTest(ctx, "d-1"); err != nil {
		t.Fatal(err)
	}
	c, err = s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error { return c.RecordApproval(appr) })
	if err != nil || c.State() != resolution.Queued || len(c.Approvals()) != 1 || c.Approvals()[0].DeliveryID != "d-1" {
		t.Fatalf("approval: %v %s %+v", err, c.State(), c.Approvals())
	}
	if c.NextAttemptKind() != resolution.Approved {
		t.Error("next kind should be approved")
	}
	byAttempt, err := s.GetCaseByAttempt(ctx, attemptID)
	if err != nil || byAttempt.Number() != 7 {
		t.Errorf("by attempt: %+v %v", byAttempt, err)
	}
	if len(c.Transitions()) != 4 {
		t.Errorf("transitions = %+v", c.Transitions())
	}
	if err := s.StoreDeliveryForTest(ctx, "d-2"); err != nil {
		t.Fatal(err)
	}
	c, err = s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.Close(resolution.Closure{DeliveryID: "d-2", ClosedAt: t0.Add(5 * time.Minute)})
	})
	if err != nil || c.State() != resolution.Closed || c.Closure() == nil {
		t.Fatalf("close: %v %s", err, c.State())
	}
}

func TestUpdateCaseRollsBackOnRefusal(t *testing.T) {
	s := storetest.Open(t)
	seedRepo(t, s, "guy/repo")
	newCase(t, s, 7, owner(t))
	_, err := s.UpdateCase(t.Context(), "guy/repo", 7, func(c *resolution.Case) error {
		_, err := c.StartAttempt(resolution.Auto, resolution.Budget{}, "brief", t0)
		return err
	})
	if err == nil {
		t.Fatal("refusal not surfaced")
	}
	c, _ := s.GetCase(t.Context(), "guy/repo", 7)
	if c.State() != resolution.Received || len(c.Attempts()) != 0 {
		t.Errorf("state leaked: %s %d", c.State(), len(c.Attempts()))
	}
}

func TestQueries(t *testing.T) {
	s := storetest.Open(t)
	seedRepo(t, s, "guy/repo")
	seedRepo(t, s, "guy/gone")
	ctx := t.Context()
	b, _ := resolution.NewBudget(10, time.Hour, 500)
	trusted := owner(t)
	drive, _ := resolution.NewRequester("x", resolution.AssociationNone)

	newCase(t, s, 1, trusted)
	newCase(t, s, 2, trusted)
	newCase(t, s, 3, drive)
	gone, _ := resolution.NewCase("guy/gone", 4, trusted, t0)
	if err := s.CreateCase(ctx, gone); err != nil {
		t.Fatal(err)
	}
	queue := func(repo string, n int, at time.Time) {
		if _, err := s.UpdateCase(ctx, repo, n, func(c *resolution.Case) error {
			return c.RecordTriage(resolution.Triage{Size: resolution.Small, Rationale: "r", Model: "m", TriagedAt: at})
		}); err != nil {
			t.Fatal(err)
		}
	}
	queue("guy/repo", 2, t0.Add(time.Minute))
	queue("guy/repo", 1, t0.Add(2*time.Minute))
	queue("guy/gone", 4, t0)
	if err := s.RemoveRepository(ctx, "guy/gone", t0); err != nil {
		t.Fatal(err)
	}

	q, err := s.QueuedCases(ctx)
	if err != nil || len(q) != 2 || q[0].Number != 2 || q[1].Number != 1 {
		t.Errorf("queued = %+v %v", q, err)
	}
	rec, _ := s.ReceivedWithoutTriage(ctx)
	if len(rec) != 0 {
		t.Errorf("received without triage = %+v", rec)
	}
	if _, err := s.UpdateCase(ctx, "guy/repo", 1, func(c *resolution.Case) error {
		_, err := c.StartAttempt(resolution.Auto, b, "brief", t0.Add(3*time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	open, _ := s.OpenAttempts(ctx)
	if len(open) != 1 || open[0].Number != 1 || open[0].Ordinal != 1 {
		t.Errorf("open = %+v", open)
	}
	counts, _ := s.CountByState(ctx)
	if counts["attempting"] != 1 || counts["queued"] != 2 || counts["gated"] != 1 {
		t.Errorf("counts = %+v", counts)
	}
	rows, next, err := s.ListCases(ctx, store.CaseFilter{Repository: "guy/repo", Limit: 2})
	if err != nil || len(rows) != 2 || next == "" {
		t.Fatalf("page 1 = %+v %q %v", rows, next, err)
	}
	rows2, next2, err := s.ListCases(ctx, store.CaseFilter{Repository: "guy/repo", Limit: 2, After: next})
	if err != nil || len(rows2) != 1 || next2 != "" {
		t.Errorf("page 2 = %+v %q %v", rows2, next2, err)
	}
	gated, _, _ := s.ListCases(ctx, store.CaseFilter{State: "gated", Limit: 10})
	if len(gated) != 1 || gated[0].Number != 3 || gated[0].LatestOutcomeKind != "none" {
		t.Errorf("gated = %+v", gated)
	}
	if _, _, err := s.ListCases(ctx, store.CaseFilter{After: "not-a-cursor"}); !errors.Is(err, store.ErrBadCursor) {
		t.Errorf("bad cursor = %v, want ErrBadCursor", err)
	}
}

// TestScansSkipRemovedRepositories proves every scan that feeds a service
// leaves a removed repository's work alone, the way QueuedCases already did.
// The App has no access to a repository it was removed from, so triaging or
// commenting on one is a call that fails every sweep for ever.
func TestScansSkipRemovedRepositories(t *testing.T) {
	s := storetest.Open(t)
	ctx := t.Context()
	seedRepo(t, s, "guy/gone")
	b, _ := resolution.NewBudget(10, time.Hour, 500)
	triage := func(n int, size resolution.Size) {
		t.Helper()
		if _, err := s.UpdateCase(ctx, "guy/gone", n, func(c *resolution.Case) error {
			return c.RecordTriage(resolution.Triage{Size: size, Rationale: "r", Model: "m", TriagedAt: t0})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []int{2, 3, 4, 5} {
		c, err := resolution.NewCase("guy/gone", n, owner(t), t0)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CreateCase(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// 2 wants triage, 3 wants a triage comment, 4 already has one queued
	// and unposted, 5 has a failed outcome that wants a comment.
	triage(3, resolution.Large)
	triage(4, resolution.Large)
	four, _ := s.GetCase(ctx, "guy/gone", 4)
	if err := s.EnqueueTriageComment(ctx, four.ID(), "sized large"); err != nil {
		t.Fatal(err)
	}
	triage(5, resolution.Small)
	five, err := s.UpdateCase(ctx, "guy/gone", 5, func(c *resolution.Case) error {
		_, err := c.StartAttempt(resolution.Auto, b, "brief", t0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := five.OpenAttempt().ID
	o, _ := resolution.OutcomeFailed(resolution.FailureInfra, "podman died", resolution.Usage{}, t0)
	if _, err := s.UpdateCase(ctx, "guy/gone", 5, func(c *resolution.Case) error { return c.RecordOutcome(attemptID, o) }); err != nil {
		t.Fatal(err)
	}

	scans := map[string]func() int{
		"ReceivedWithoutTriage":  func() int { k, _ := s.ReceivedWithoutTriage(ctx); return len(k) },
		"TriagesNeedingComment":  func() int { k, _ := s.TriagesNeedingComment(ctx); return len(k) },
		"OutcomesNeedingComment": func() int { k, _ := s.OutcomesNeedingComment(ctx); return len(k) },
		"UnpostedComments":       func() int { k, _ := s.UnpostedComments(ctx); return len(k) },
	}
	for name, scan := range scans {
		if n := scan(); n != 1 {
			t.Fatalf("%s while enrolled = %d, want 1", name, n)
		}
	}
	if err := s.RemoveRepository(ctx, "guy/gone", t0); err != nil {
		t.Fatal(err)
	}
	for name, scan := range scans {
		if n := scan(); n != 0 {
			t.Errorf("%s after removal = %d, want 0", name, n)
		}
	}
}

// TestRunNeedsItsAttempt proves the run insert reports a missing attempt
// instead of writing nothing and calling it a success. Changes are keyed by
// ordinal, so an ordinal the store cannot resolve has to be an error.
func TestRunNeedsItsAttempt(t *testing.T) {
	s := storetest.Open(t)
	ctx := t.Context()
	seedRepo(t, s, "guy/repo")
	newCase(t, s, 7, owner(t))
	b, _ := resolution.NewBudget(10, time.Hour, 500)
	if _, err := s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordTriage(resolution.Triage{Size: resolution.Small, Rationale: "typo", Model: "m", TriagedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	c, err := s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		_, err := c.StartAttempt(resolution.Auto, b, "brief", t0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Persisting a run against an ordinal the case does not have: the
	// aggregate cannot produce this, so it is forced through the store's
	// own change set.
	run := resolution.Run{RunID: "run-1", Model: "m", BaseSha: "0123456789abcdef0123456789abcdef01234567", BeganAt: t0}
	err = store.PersistRunForTest(ctx, s, c.ID(), 9, run)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run against a missing attempt = %v, want ErrNotFound", err)
	}
	got, _ := s.GetCase(ctx, "guy/repo", 7)
	if got.OpenAttempt().Run != nil {
		t.Errorf("run landed anyway: %+v", got.OpenAttempt().Run)
	}
}

// TestCreateCaseLeavesTheAggregateAloneWhenItRollsBack proves the aggregate
// is only told about its id, and told to forget its changes, once the row is
// actually committed. Anything that fails after the insert but inside the
// transaction rolls the row back, and a Case left holding an id no row has
// and no changes to retry with cannot be recovered from.
//
// The failure is forced through pg_notify's 8000-byte payload ceiling: the
// notify is the last statement in the transaction, so a repository name long
// enough to overflow the payload fails exactly in the window that matters.
func TestCreateCaseLeavesTheAggregateAloneWhenItRollsBack(t *testing.T) {
	s := storetest.Open(t)
	ctx := t.Context()
	long := "guy/" + strings.Repeat("n", 8000)
	// Inserted directly: EnrollRepository notifies on the same channel and
	// would trip the same ceiling before the case exists.
	if _, err := s.Pool().Exec(ctx, `insert into repositories (full_name, installation_id, default_branch) values ($1, 1, 'main')`, long); err != nil {
		t.Fatal(err)
	}
	c, err := resolution.NewCase(long, 7, owner(t), t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCase(ctx, c); err == nil {
		t.Fatal("oversized notify payload accepted")
	}
	if c.ID() != "" {
		t.Errorf("id assigned though the row rolled back: %q", c.ID())
	}
	if c.Changes().State != resolution.Received {
		t.Errorf("changes cleared though the row rolled back: %+v", c.Changes())
	}
	if _, err := s.GetCase(ctx, long, 7); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("row survived the rollback: %v", err)
	}
	// A conflict is the other way in and must leave the same nothing.
	seedRepo(t, s, "guy/repo")
	newCase(t, s, 7, owner(t))
	second, err := resolution.NewCase("guy/repo", 7, owner(t), t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCase(ctx, second); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	if second.ID() != "" || second.Changes().State != resolution.Received {
		t.Errorf("conflict disturbed the aggregate: id %q changes %+v", second.ID(), second.Changes())
	}
}
