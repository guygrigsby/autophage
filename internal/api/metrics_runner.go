package api

import (
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
)

// RunnerMetrics feeds the runner's steps, tool calls and outcomes into the
// Prometheus vectors. It implements agent.Metrics.
type RunnerMetrics struct{}

// Running moves the in-flight gauge.
func (RunnerMetrics) Running(delta int) { AttemptsRunning.Add(float64(delta)) }

// Step times one pipeline step and counts the failures.
func (RunnerMetrics) Step(step string, d time.Duration, err error) {
	AttemptStepSeconds.WithLabelValues(step).Observe(d.Seconds())
	if err != nil {
		AttemptStepErrors.WithLabelValues(step).Inc()
	}
}

// ToolCall times one tool call and counts it by result.
func (RunnerMetrics) ToolCall(tool string, d time.Duration, err error) {
	AttemptToolSeconds.WithLabelValues(tool).Observe(d.Seconds())
	AttemptToolCalls.WithLabelValues(tool, resultOf(err)).Inc()
}

// Ended records the attempt's outcome, what stopped its run and what it
// spent getting there.
func (RunnerMetrics) Ended(kind, outcome, stop string, u resolution.Usage) {
	AttemptsTotal.WithLabelValues(kind, outcome).Inc()
	AttemptTokens.WithLabelValues("input").Add(float64(u.InputTokens))
	AttemptTokens.WithLabelValues("output").Add(float64(u.OutputTokens))
	AttemptWallClock.Observe(u.WallClock.Seconds())
	AttemptTurns.Observe(float64(u.Turns))
	AttemptDiffLines.Observe(float64(u.DiffLines))
	AttemptStops.WithLabelValues(stop).Inc()
}

// resultOf is the ok or error label for a call that may have failed.
func resultOf(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
