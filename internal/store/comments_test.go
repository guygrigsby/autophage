package store_test

import (
	"testing"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/storetest"
)

func TestCommentOutbox(t *testing.T) {
	s := storetest.Open(t)
	seedRepo(t, s, "guy/repo")
	ctx := t.Context()
	newCase(t, s, 7, owner(t))
	c, err := s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordTriage(resolution.Triage{Size: resolution.Large, Rationale: "big", Model: "m", TriagedAt: t0})
	})
	if err != nil {
		t.Fatal(err)
	}
	need, _ := s.TriagesNeedingComment(ctx)
	if len(need) != 1 || need[0].Number != 7 {
		t.Fatalf("need = %+v", need)
	}
	if err := s.EnqueueTriageComment(ctx, c.ID(), "Sized large: big"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTriageComment(ctx, c.ID(), "again"); err != nil {
		t.Fatal(err)
	}
	un, err := s.UnpostedComments(ctx)
	if err != nil || len(un) != 1 || un[0].Body != "Sized large: big" || un[0].Repository != "guy/repo" || un[0].Number != 7 {
		t.Fatalf("unposted = %+v %v", un, err)
	}
	need, _ = s.TriagesNeedingComment(ctx)
	if len(need) != 0 {
		t.Errorf("still needing: %+v", need)
	}
	if err := s.RecordCommentPost(ctx, un[0].ID, 99001); err != nil {
		t.Fatal(err)
	}
	un, _ = s.UnpostedComments(ctx)
	if len(un) != 0 {
		t.Errorf("still unposted: %+v", un)
	}

	b, _ := resolution.NewBudget(10, time.Hour, 500)
	if err := s.StoreDeliveryForTest(ctx, "d-1"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordApproval(resolution.Approval{Approver: owner(t), Source: resolution.SourceLabel, ApprovedAt: t0, DeliveryID: "d-1"})
	})
	c, _ = s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		_, err := c.StartAttempt(resolution.Approved, b, "brief", t0)
		return err
	})
	attemptID := c.OpenAttempt().ID
	o, _ := resolution.OutcomeFailed(resolution.FailureInfra, "podman died", resolution.Usage{}, t0)
	if _, err := s.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error { return c.RecordOutcome(attemptID, o) }); err != nil {
		t.Fatal(err)
	}
	ids, _ := s.OutcomesNeedingComment(ctx)
	if len(ids) != 1 || ids[0] != attemptID {
		t.Fatalf("outcomes needing = %+v", ids)
	}
	if err := s.EnqueueOutcomeComment(ctx, attemptID, "Failed: podman died"); err != nil {
		t.Fatal(err)
	}
	ids, _ = s.OutcomesNeedingComment(ctx)
	if len(ids) != 0 {
		t.Errorf("still needing: %+v", ids)
	}
	un, _ = s.UnpostedComments(ctx)
	if len(un) != 1 || un[0].Body != "Failed: podman died" {
		t.Errorf("unposted = %+v", un)
	}
}
