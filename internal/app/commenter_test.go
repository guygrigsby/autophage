package app

import (
	"strings"
	"testing"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

func TestCommenterPostsTriageAndOutcomeOnceEach(t *testing.T) {
	st := store.OpenTest(t)
	seed(t, st)
	ctx := t.Context()
	if _, err := st.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordTriage(resolution.Triage{Size: resolution.Large, Rationale: "touches the auth layer", Model: "m", TriagedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{failPost: true}
	cm := &Commenter{Store: st, GitHub: gh}
	if err := cm.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(gh.comments) != 0 {
		t.Fatal("posted despite failure")
	}
	gh.failPost = false
	_ = cm.Run(ctx)
	_ = cm.Run(ctx)
	if len(gh.comments) != 1 || !strings.Contains(gh.comments[0], "touches the auth layer") || !strings.Contains(gh.comments[0], "approved") {
		t.Errorf("comments = %q", gh.comments)
	}

	b, _ := resolution.NewBudget(10, 3600e9, 500)
	if err := st.StoreDeliveryForTest(ctx, "d-1"); err != nil {
		t.Fatal(err)
	}
	req, _ := resolution.NewRequester("guy", resolution.AssociationOwner)
	if _, err := st.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordApproval(resolution.Approval{Approver: req, Source: resolution.SourceLabel, ApprovedAt: t0, DeliveryID: "d-1"})
	}); err != nil {
		t.Fatal(err)
	}
	c, _ := st.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		_, err := c.StartAttempt(resolution.Approved, b, "brief", t0)
		return err
	})
	id := c.OpenAttempt().ID
	o, _ := resolution.OutcomeExhausted(resolution.LimitTurns, "What I found: a lot.", resolution.Usage{Turns: 10}, t0)
	if _, err := st.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error { return c.RecordOutcome(id, o) }); err != nil {
		t.Fatal(err)
	}
	_ = cm.Run(ctx)
	_ = cm.Run(ctx)
	if len(gh.comments) != 2 || !strings.Contains(gh.comments[1], "What I found: a lot.") || !strings.Contains(gh.comments[1], "turns") {
		t.Errorf("comments = %q", gh.comments)
	}
}
