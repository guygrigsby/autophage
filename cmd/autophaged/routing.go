package main

import (
	"context"
	"log"
	"time"

	"github.com/guygrigsby/llm/aperture"

	"github.com/guygrigsby/agent-select/route"

	appconfig "github.com/guygrigsby/autophage/internal/config"
	"github.com/guygrigsby/autophage/internal/routing"
	"github.com/guygrigsby/autophage/internal/store"
)

// readTimeout bounds each read a routed tier makes before each use: the
// live registry and the outcome history. A slow gateway falls back to the
// configured id and a slow store to the declared scores, rather than
// holding an attempt up.
const readTimeout = 5 * time.Second

// newRouter builds the registry-backed router for the tiers that set
// route, and nil when none do. Repair rounds are the only work CI judges, so
// they are the only history (ADR 0012).
func newRouter(cfg appconfig.Config, st *store.Store) *routing.Router {
	modes := map[routing.Tier]routing.Mode{}
	localOnly := map[routing.Tier]bool{}
	for tier, mt := range map[routing.Tier]appconfig.ModelTier{
		routing.Triage: cfg.Model.Triage, routing.Attempt: cfg.Model.Auto,
		routing.Approved: cfg.Model.Approved, routing.Repair: cfg.Model.Repair,
	} {
		if mt.Route != "" {
			modes[tier] = routing.Mode(mt.Route)
			localOnly[tier] = mt.LocalOnly
		}
	}
	if len(modes) == 0 {
		log.Print("routing: off (no tier sets route)")
		return nil
	}
	rt, err := routing.New(routing.Config{
		Registry: func(ctx context.Context) ([]aperture.ModelInfo, error) {
			ctx, cancel := context.WithTimeout(ctx, readTimeout)
			defer cancel()
			return aperture.ListModelInfo(ctx, "", "")
		},
		Modes: modes, LocalOnly: localOnly, Local: cfg.Routing.LocalProviders, LocalContext: cfg.Routing.LocalContextTokens, Scorer: route.Table(cfg.Routing.Scores),
		Outcomes: func(ctx context.Context) ([]routing.Tally, error) {
			ctx, cancel := context.WithTimeout(ctx, readTimeout)
			defer cancel()
			verdicts, err := st.RepairVerdictsByModel(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]routing.Tally, len(verdicts))
			for i, v := range verdicts {
				out[i] = routing.Tally{Tier: routing.Repair, Model: v.Model, Passes: v.Passes, Fails: v.Fails}
			}
			return out, nil
		},
		MinRuns: cfg.Routing.HistoryMinRuns,
	})
	if err != nil {
		log.Fatalf("routing: %v", err)
	}
	log.Printf("routing: on for %d tier(s), local providers %v", len(modes), cfg.Routing.LocalProviders)
	return rt
}
