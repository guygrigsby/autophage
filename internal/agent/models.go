package agent

import (
	"errors"

	"github.com/guygrigsby/llm"
	"github.com/guygrigsby/llm/aperture"
)

// Models builds one llm.LLM per Aperture model id. The gateway is on the
// tailnet and authenticates the host, so there is no key to carry: the
// endpoint and its trust live with the network, not the process.
type Models struct {
	// Metrics observes every metered call each adapter makes. Nil is
	// NopModelMetrics.
	Metrics ModelMetrics
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
