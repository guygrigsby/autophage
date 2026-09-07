package app

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// Commenter fills the outbox from facts that need a comment and posts what
// is unposted. Both halves are idempotent. Label is the approval label named
// in the comment bodies' call to action.
type Commenter struct {
	Store  *store.Store
	GitHub resolution.GitHub
	Label  string
	// Metrics counts the posts. Nil is NopCommentMetrics.
	Metrics CommentMetrics
}

// metrics tolerates an unwired Metrics.
func (m *Commenter) metrics() CommentMetrics {
	if m.Metrics == nil {
		return NopCommentMetrics{}
	}
	return m.Metrics
}

func (m *Commenter) Run(ctx context.Context) error {
	if err := m.enqueue(ctx); err != nil {
		return err
	}
	pending, err := m.Store.UnpostedComments(ctx)
	if err != nil {
		return err
	}
	for _, c := range pending {
		id, err := m.GitHub.PostComment(ctx, c.Repository, c.Number, c.Body)
		if err != nil {
			m.metrics().Post(resultError)
			log.Printf("comment %s#%d: %v (will retry)", c.Repository, c.Number, err)
			continue
		}
		m.metrics().Post(resultOK)
		if err := m.Store.RecordCommentPost(ctx, c.ID, id); err != nil {
			log.Printf("record comment post %s: %v", c.ID, err)
		}
	}
	return nil
}

// enqueue queues a comment for every triage and outcome that needs one. A
// failure on one case or attempt is logged; the rest are still enqueued.
func (m *Commenter) enqueue(ctx context.Context) error {
	triages, err := m.Store.TriagesNeedingComment(ctx)
	if err != nil {
		return err
	}
	for _, k := range triages {
		if err := m.enqueueTriage(ctx, k); err != nil {
			log.Printf("enqueue triage comment %s#%d: %v", k.Repository, k.Number, err)
		}
	}
	attempts, err := m.Store.OutcomesNeedingComment(ctx)
	if err != nil {
		return err
	}
	for _, id := range attempts {
		if err := m.enqueueOutcome(ctx, id); err != nil {
			log.Printf("enqueue outcome comment %s: %v", id, err)
		}
	}
	return nil
}

func (m *Commenter) enqueueTriage(ctx context.Context, k store.CaseKey) error {
	c, err := m.Store.GetCase(ctx, k.Repository, k.Number)
	if err != nil {
		return err
	}
	return m.Store.EnqueueTriageComment(ctx, c.ID(), TriageBody(*c.Triage(), m.Label))
}

func (m *Commenter) enqueueOutcome(ctx context.Context, attemptID string) error {
	c, err := m.Store.GetCaseByAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	for _, a := range c.Attempts() {
		if a.ID == attemptID {
			return m.Store.EnqueueOutcomeComment(ctx, attemptID, OutcomeBody(c, a, m.Label))
		}
	}
	return nil
}

// TriageBody is the comment for a Large triage.
func TriageBody(t resolution.Triage, label string) string {
	return fmt.Sprintf("autophage sized this issue **large** and will not start on its own.\n\n%s\n\nAdd the `%s` label to have autophage attempt it.", t.Rationale, label)
}

// OutcomeBody is the comment for an exhausted, failed or stopped attempt.
func OutcomeBody(c *resolution.Case, a resolution.Attempt, label string) string {
	o := a.Outcome
	var head string
	switch o.Kind {
	case resolution.BudgetExhausted:
		head = fmt.Sprintf("autophage attempt %d stopped: budget exhausted (%s). The work so far is on branch `%s`.", a.Ordinal, strings.ReplaceAll(string(o.Limit), "_", " "), c.Branch())
	case resolution.FailedOutcome:
		head = fmt.Sprintf("autophage attempt %d failed (%s).", a.Ordinal, o.Class)
	case resolution.Aborted:
		head = fmt.Sprintf("autophage attempt %d was stopped by the operator. The work so far is on branch `%s`.", a.Ordinal, c.Branch())
	default:
		head = fmt.Sprintf("autophage attempt %d ended: %s.", a.Ordinal, o.Kind)
	}
	usage := fmt.Sprintf("Used %d turns, %s, %d diff lines.", o.Usage.Turns, o.Usage.WallClock.Round(1e9), o.Usage.DiffLines)
	return fmt.Sprintf("%s\n\n%s\n\n%s\n\nAdd the `%s` label to run again with the larger budget.", head, o.Summary, usage, label)
}
