package api

import "github.com/guygrigsby/autophage/internal/resolution"

// RunnerMetrics feeds the runner's outcomes into the Prometheus vectors.
type RunnerMetrics struct{}

func (RunnerMetrics) Ended(kind, outcome string, u resolution.Usage) {
	AttemptsTotal.WithLabelValues(kind, outcome).Inc()
	AttemptTokens.WithLabelValues("input").Add(float64(u.InputTokens))
	AttemptTokens.WithLabelValues("output").Add(float64(u.OutputTokens))
	AttemptWallClock.Observe(u.WallClock.Seconds())
}
