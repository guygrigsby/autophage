package app

import (
	"context"
	"log"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// Recovery ends every attempt the previous daemon left open. An attempt
// never resumes mid-run; the case re-queues once through the aggregate rule.
type Recovery struct {
	Store *store.Store
	Clock resolution.Clock
}

// Run ends every open attempt with Aborted{DaemonRestart}. A failure on one
// attempt is logged; the rest still get ended.
func (r *Recovery) Run(ctx context.Context) error {
	open, err := r.Store.OpenAttempts(ctx)
	if err != nil {
		return err
	}
	for _, a := range open {
		if err := r.one(ctx, a); err != nil {
			log.Printf("recovery %s#%d attempt %d: %v", a.Repository, a.Number, a.Ordinal, err)
		}
	}
	return nil
}

// one ends a single attempt. The reason follows the case: an attempt still
// open under a Closed case was running when the issue was closed, and
// naming that daemon_restart both reads wrong to the operator and spends
// the aggregate's requeue-once allowance on a case that will never run
// again.
func (r *Recovery) one(ctx context.Context, a store.OpenAttempt) error {
	_, err := r.Store.UpdateCase(ctx, a.Repository, a.Number, func(c *resolution.Case) error {
		reason, summary := resolution.AbortDaemonRestart, "The daemon restarted during the attempt."
		if c.State() == resolution.Closed {
			reason, summary = resolution.AbortIssueClosed, "The issue was closed while the attempt was running."
		}
		o, err := resolution.OutcomeAborted(reason, summary, resolution.Usage{}, r.Clock.Now())
		if err != nil {
			return err
		}
		return c.RecordOutcome(a.AttemptID, o)
	})
	return err
}
