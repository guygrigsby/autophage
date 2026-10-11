package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/guygrigsby/llm"
	"github.com/guygrigsby/llm/aperture"

	"github.com/guygrigsby/autophage/internal/routing"
)

// Models builds one llm.LLM per Aperture model id. The gateway is on the
// tailnet and authenticates the host, so there is no key to carry: the
// endpoint and its trust live with the network, not the process.
type Models struct {
	// Metrics observes every metered call each adapter makes. Nil is
	// NopModelMetrics.
	Metrics ModelMetrics
	// Chooser picks the model for a routed tier from the live registry
	// (ADR 0010). Nil means every tier runs its configured id.
	Chooser Chooser
	Logf    func(string, ...any)
}

// Chooser is the routing seam: the registry-backed pick for one tier.
type Chooser interface {
	Choose(ctx context.Context, tier routing.Tier) (routing.Choice, error)
}

// Resolve returns the model id a tier runs on right now: the routed pick
// when the tier is routed and routing answers, static otherwise. A routing
// failure logs a warning and keeps the configured id, so an unreachable
// gateway registry never stops work the static id could still do.
func (m Models) Resolve(ctx context.Context, tier routing.Tier, static string) string {
	if m.Chooser == nil {
		return static
	}
	c, err := m.Chooser.Choose(ctx, tier)
	switch {
	case errors.Is(err, routing.ErrNotRouted):
		return static
	case err != nil:
		m.logf("routing: %s falls back to %s: %v", tier, static, err)
		return static
	}
	m.logf("routing: %s -> %s (offer %s, snapshot %.12s, warnings: %s)", tier, c.ModelID, c.OfferID, c.SnapshotHash, strings.Join(c.Warnings, ","))
	return c.ModelID
}

func (m Models) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// Tier returns the adapter for modelID.
func (m Models) Tier(modelID string) (llm.LLM, error) {
	if modelID == "" {
		return nil, errors.New("agent: model id is empty")
	}
	return aperture.New(aperture.Config{Model: modelID, Meter: m.meter(modelID)})
}

// meter is the adapter's Meter for one tier: it labels the usage with the
// model id this adapter was built for. The adapter reports usage only for a
// call the provider answered and priced, so nothing that reaches here is a
// failure; the errors are counted where the call returns one, in
// Triager.Classify and in RunAttempt's model_error stop.
func (m Models) meter(modelID string) llm.Meter {
	metrics := m.metrics()
	return llm.MeterFunc(func(u llm.Usage) { metrics.Observe(modelID, u, nil) })
}

// metrics tolerates an unwired Metrics.
func (m Models) metrics() ModelMetrics {
	if m.Metrics == nil {
		return NopModelMetrics{}
	}
	return m.Metrics
}
