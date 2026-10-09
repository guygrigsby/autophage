package app

import (
	"strings"
	"testing"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/storetest"
)

func TestCommenterPostsTriageAndOutcomeOnceEach(t *testing.T) {
	st := storetest.Open(t)
	seed(t, st)
	ctx := t.Context()
	if _, err := st.UpdateCase(ctx, "guy/repo", 7, func(c *resolution.Case) error {
		return c.RecordTriage(resolution.Triage{Size: resolution.Large, Rationale: "touches the auth layer", Model: "m", TriagedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{failPost: true}
	cm := &Commenter{Store: st, GitHub: gh, Label: "approved"}
	if err := cm.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(gh.postedComments()) != 0 {
		t.Fatal("posted despite failure")
	}
	gh.setFailPost(false)
	_ = cm.Run(ctx)
	_ = cm.Run(ctx)
	if posted := gh.postedComments(); len(posted) != 1 || !strings.Contains(posted[0], "touches the auth layer") || !strings.Contains(posted[0], "approved") {
		t.Errorf("comments = %q", posted)
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
	if posted := gh.postedComments(); len(posted) != 2 || !strings.Contains(posted[1], "What I found: a lot.") || !strings.Contains(posted[1], "turns") {
		t.Errorf("comments = %q", posted)
	}
}

func TestOutcomeBodyNamesThePartialPullRequest(t *testing.T) {
	b, _ := resolution.NewBudget(10, 3600e9, 500)
	req, _ := resolution.NewRequester("guy", resolution.AssociationOwner)
	outcome, err := resolution.OutcomeExhaustedWithPullRequest(resolution.LimitDiffLines, 12, "0123456789abcdef0123456789abcdef01234567", "What I found: a lot.\nWhat I did: some.\nWhat is left: the rest.\nWhat I would do with more budget: the rest.", resolution.Usage{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := resolution.NewCase("guy/repo", 7, req, t0)
	if err := c.RecordTriage(resolution.Triage{Size: resolution.Small, Rationale: "r", Model: "m", TriagedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.StartAttempt(resolution.Auto, b, "brief", t0); err != nil {
		t.Fatal(err)
	}
	a := c.OpenAttempt()
	if err := c.RecordOutcome(a.ID, outcome); err != nil {
		t.Fatal(err)
	}
	withPR := OutcomeBody(c, c.Attempts()[0], "approved")
	if !strings.Contains(withPR, "pull request #12") || !strings.Contains(withPR, "incomplete") || strings.Contains(withPR, "branch `") {
		t.Errorf("body with PR = %q", withPR)
	}
	if !strings.Contains(withPR, "approved") {
		t.Errorf("body lost the approval instruction: %q", withPR)
	}
	plain, err := resolution.OutcomeExhausted(resolution.LimitTurns, "s", resolution.Usage{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := resolution.NewCase("guy/repo", 7, req, t0)
	if err := c2.RecordTriage(resolution.Triage{Size: resolution.Small, Rationale: "r", Model: "m", TriagedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.StartAttempt(resolution.Auto, b, "brief", t0); err != nil {
		t.Fatal(err)
	}
	a2 := c2.OpenAttempt()
	if err := c2.RecordOutcome(a2.ID, plain); err != nil {
		t.Fatal(err)
	}
	without := OutcomeBody(c2, c2.Attempts()[0], "approved")
	if !strings.Contains(without, "branch `autophage/7`") || strings.Contains(without, "pull request #") {
		t.Errorf("body without PR = %q", without)
	}
}
