package api

import (
	"time"

	"github.com/guygrigsby/llm"
)

// The metrics interfaces the other packages declare, implemented here over
// the Prometheus vectors. Each is an empty struct, so the daemon wires one
// by value and nothing has to be built or torn down. Nothing here asserts
// it satisfies those interfaces: importing internal/app or internal/agent
// from here would be a cycle, and cmd/autophaged wiring each one into the
// field that takes it is the compile-time check.

// SweepMetrics implements app.SweepMetrics.
type SweepMetrics struct{}

// Sweep counts one whole sweep.
func (SweepMetrics) Sweep() { Sweeps.Inc() }

// Step times one sweep step and counts the failures.
func (SweepMetrics) Step(step string, d time.Duration, err error) {
	SweepStepSeconds.WithLabelValues(step).Observe(d.Seconds())
	if err != nil {
		SweepStepErrors.WithLabelValues(step).Inc()
	}
}

// TriageMetrics implements app.TriageMetrics.
type TriageMetrics struct{}

// Triage counts one sizing call and times it.
func (TriageMetrics) Triage(size, result string, d time.Duration) {
	Triages.WithLabelValues(size, result).Inc()
	TriageSeconds.Observe(d.Seconds())
}

// CommentMetrics implements app.CommentMetrics.
type CommentMetrics struct{}

// Post counts one comment post.
func (CommentMetrics) Post(result string) { CommentPosts.WithLabelValues(result).Inc() }

// RequestMetrics implements github.RequestMetrics.
type RequestMetrics struct{}

// Request counts one GitHub attempt and times it.
func (RequestMetrics) Request(method, status string, d time.Duration) {
	GitHubRequests.WithLabelValues(method, status).Inc()
	GitHubRequestSeconds.WithLabelValues(method).Observe(d.Seconds())
}

// Retry counts one throttled attempt waited out.
func (RequestMetrics) Retry(reason string) { GitHubRetries.WithLabelValues(reason).Inc() }

// RateLimitRemaining records the quota GitHub last reported.
func (RequestMetrics) RateLimitRemaining(n int) { GitHubRateLimitRemaining.Set(float64(n)) }

// TranslateMetrics implements github.TranslateMetrics.
type TranslateMetrics struct{}

// Delivery counts one processing row.
func (TranslateMetrics) Delivery(event, result string) {
	DeliveriesTotal.WithLabelValues(event, result).Inc()
}

// ModelMetrics implements agent.ModelMetrics.
type ModelMetrics struct{}

// Observe counts one model call. A failed call carries no usage: the
// adapter's Meter only ever sees a call the provider answered and priced,
// so the error arrives from the caller instead, with nothing to add to the
// token and cost counters.
func (ModelMetrics) Observe(model string, u llm.Usage, err error) {
	ModelRequests.WithLabelValues(model, resultOf(err)).Inc()
	if err != nil {
		return
	}
	ModelLatency.WithLabelValues(model).Observe(u.Latency.Seconds())
	// ReasoningTokens is a part of CompletionTokens, not an addition, so
	// the directions are reported as the adapter splits them and summing
	// them is the caller's business.
	ModelTokens.WithLabelValues(model, "prompt").Add(float64(u.PromptTokens))
	ModelTokens.WithLabelValues(model, "completion").Add(float64(u.CompletionTokens))
	ModelTokens.WithLabelValues(model, "reasoning").Add(float64(u.ReasoningTokens))
	ModelTokens.WithLabelValues(model, "cache_read").Add(float64(u.CacheReadTokens))
	ModelTokens.WithLabelValues(model, "cache_write").Add(float64(u.CacheWriteTokens))
	ModelCost.WithLabelValues(model).Add(u.Cost)
}
