package main

import (
	"context"
	"log"

	"github.com/guygrigsby/jess/ledger"

	appconfig "github.com/guygrigsby/autophage/internal/config"
	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// runner is replaced by the sandboxed agent runner in the agent plan. This
// placeholder ends every attempt with Failed{Infra} so the state machine
// keeps moving and the operator can see the daemon is not yet able to run.
type runner struct {
	st    *store.Store
	clock resolution.Clock
}

func newRunner(_ context.Context, st *store.Store, _ resolution.GitHub, _ *ledger.Postgres, _ appconfig.Config, _ appconfig.Secrets) *runner {
	return &runner{st: st, clock: resolution.SystemClock{}}
}

func (r *runner) Run(ctx context.Context, attemptID string) {
	c, err := r.st.GetCaseByAttempt(ctx, attemptID)
	if err != nil {
		log.Printf("runner: %v", err)
		return
	}
	o, _ := resolution.OutcomeFailed(resolution.FailureInfra, "no agent runner is built into this daemon yet", resolution.Usage{}, r.clock.Now())
	if _, err := r.st.UpdateCase(ctx, c.Repository(), c.Number(), func(c *resolution.Case) error { return c.RecordOutcome(attemptID, o) }); err != nil {
		log.Printf("runner: record outcome: %v", err)
	}
}

func (r *runner) Stop(string) bool { return false }

// Triager sizes everything Large until the agent plan wires the model, so
// nothing runs unattended before the sandbox exists.
func (r *runner) Triager() resolution.Triager { return largeTriager{} }

type largeTriager struct{}

func (largeTriager) Classify(context.Context, string, string) (resolution.Triage, error) {
	return resolution.Triage{Size: resolution.Large, Rationale: "no triage model is wired yet; every case waits for approval", Model: "none"}, nil
}
