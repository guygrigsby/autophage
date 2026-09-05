package store

import (
	"errors"
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
)

func seedRepo(t *testing.T, s *Store, name string) {
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

func newCase(t *testing.T, s *Store, number int, req resolution.Requester) *resolution.Case {
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
	s := OpenTest(t)
	seedRepo(t, s, "guy/repo")
	c := newCase(t, s, 7, owner(t))
	if c.ID() == "" {
		t.Fatal("id not assigned")
	}
	got, err := s.GetCase(t.Context(), "guy/repo", 7)
	if err != nil || got.ID() != c.ID() || got.State() != resolution.Received || got.Requester().Login != "guy" {
		t.Fatalf("get = %+v %v", got, err)
	}
	if err := s.CreateCase(t.Context(), c); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate = %v", err)
	}
	if _, err := s.GetCase(t.Context(), "guy/repo", 8); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
}

func TestUpdateCasePersistsEveryFact(t *testing.T) {
	s := OpenTest(t)
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
	s := OpenTest(t)
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
	s := OpenTest(t)
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
	rows, next, err := s.ListCases(ctx, CaseFilter{Repository: "guy/repo", Limit: 2})
	if err != nil || len(rows) != 2 || next == "" {
		t.Fatalf("page 1 = %+v %q %v", rows, next, err)
	}
	rows2, next2, err := s.ListCases(ctx, CaseFilter{Repository: "guy/repo", Limit: 2, After: next})
	if err != nil || len(rows2) != 1 || next2 != "" {
		t.Errorf("page 2 = %+v %q %v", rows2, next2, err)
	}
	gated, _, _ := s.ListCases(ctx, CaseFilter{State: "gated", Limit: 10})
	if len(gated) != 1 || gated[0].Number != 3 || gated[0].LatestOutcomeKind != "none" {
		t.Errorf("gated = %+v", gated)
	}
}
