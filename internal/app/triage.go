package app

import (
	"context"
	"log"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// Triage sizes every Received case with one model call.
type Triage struct {
	Store   *store.Store
	Triager resolution.Triager
	GitHub  resolution.GitHub
	Clock   resolution.Clock
}

// Run sizes each Received case without a triage. A failure on one case is
// logged and the case stays Received for the next sweep.
func (t *Triage) Run(ctx context.Context) error {
	keys, err := t.Store.ReceivedWithoutTriage(ctx)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := t.one(ctx, k); err != nil {
			log.Printf("triage %s#%d: %v", k.Repository, k.Number, err)
		}
	}
	return nil
}

func (t *Triage) one(ctx context.Context, k store.CaseKey) error {
	detail, err := t.GitHub.GetIssue(ctx, k.Repository, k.Number)
	if err != nil {
		return err
	}
	tr, err := t.Triager.Classify(ctx, detail.Title, detail.Body)
	if err != nil {
		return err
	}
	tr.TriagedAt = t.Clock.Now()
	_, err = t.Store.UpdateCase(ctx, k.Repository, k.Number, func(c *resolution.Case) error { return c.RecordTriage(tr) })
	return err
}
