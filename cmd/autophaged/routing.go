package main

import (
	"context"
	"log"
	"time"

	"github.com/guygrigsby/llm/aperture"

	"github.com/guygrigsby/agent-select/route"

	appconfig "github.com/guygrigsby/autophage/internal/config"
	"github.com/guygrigsby/autophage/internal/routing"
)

// registryTimeout bounds the live registry read a routed tier makes before
// each use. A slow gateway falls back to the configured id rather than
// holding an attempt up.
const registryTimeout = 5 * time.Second

// newRouter builds the registry-backed router for the tiers that set
// route, and nil when none do.
func newRouter(cfg appconfig.Config) *routing.Router {
	modes := map[routing.Tier]routing.Mode{}
	for tier, mt := range map[routing.Tier]appconfig.ModelTier{
		routing.Triage: cfg.Model.Triage, routing.Attempt: cfg.Model.Auto,
		routing.Approved: cfg.Model.Approved, routing.Repair: cfg.Model.Repair,
	} {
		if mt.Route != "" {
			modes[tier] = routing.Mode(mt.Route)
		}
	}
	if len(modes) == 0 {
		log.Print("routing: off (no tier sets route)")
		return nil
	}
	rt, err := routing.New(routing.Config{
		Registry: func(ctx context.Context) ([]aperture.ModelInfo, error) {
			ctx, cancel := context.WithTimeout(ctx, registryTimeout)
			defer cancel()
			return aperture.ListModelInfo(ctx, "", "")
		},
		Modes: modes, Local: cfg.Routing.LocalProviders, LocalContext: cfg.Routing.LocalContextTokens, Scorer: route.Table(cfg.Routing.Scores),
	})
	if err != nil {
		log.Fatalf("routing: %v", err)
	}
	log.Printf("routing: on for %d tier(s), local providers %v", len(modes), cfg.Routing.LocalProviders)
	return rt
}
