package agent

import (
	"time"

	"github.com/guygrigsby/llm"

	"github.com/guygrigsby/autophage/internal/resolution"
)

// Metrics is what the runner reports: how many attempts are in flight, how
// long each step of the pipeline took, every tool call the agent made and
// the end of the attempt. internal/api implements it over Prometheus
// vectors; nothing here depends on a metrics library.
type Metrics interface {
	// Running moves the in-flight gauge, +1 when an attempt registers and
	// -1 when it leaves.
	Running(delta int)
	// Step reports one pipeline step, with the error it failed with, if any.
	// step is the closed vocabulary below.
	Step(step string, d time.Duration, err error)
	// ToolCall reports one tool the agent ran, by the toolbox's name for it.
	ToolCall(tool string, d time.Duration, err error)
	// Ended reports the attempt's outcome. stop is why the run ended, or
	// "none" when it ended on its own terms.
	Ended(kind, outcome, stop string, usage resolution.Usage)
}

// NopMetrics is the default when a Runner or a RunInput has no Metrics.
type NopMetrics struct{}

func (NopMetrics) Running(int)                                    {}
func (NopMetrics) Step(string, time.Duration, error)              {}
func (NopMetrics) ToolCall(string, time.Duration, error)          {}
func (NopMetrics) Ended(string, string, string, resolution.Usage) {}

// ModelMetrics reports one model call: the tokens, latency and cost an
// adapter's Meter saw, or the error a call returned instead. model is the
// configured OpenRouter id, which is three values in production.
type ModelMetrics interface {
	Observe(model string, u llm.Usage, err error)
}

// NopModelMetrics is the default when nothing wired a ModelMetrics.
type NopModelMetrics struct{}

func (NopModelMetrics) Observe(string, llm.Usage, error) {}

// The pipeline's step names. They are the dashboard's contract: a renamed
// step is a panel that stops drawing.
const (
	stepMint        = "mint"
	stepPrepare     = "prepare"
	stepStart       = "start"
	stepTools       = "tools"
	stepRun         = "run"
	stepRemint      = "remint"
	stepPush        = "push"
	stepConflicts   = "conflicts"
	stepPullRequest = "pull_request"
	stepTeardown    = "teardown"
)

// stopNone is the label for an attempt whose run ended on its own terms.
// The Stop vocabulary's own zero value is the empty string, which is no
// label at all.
const stopNone = "none"

// stopLabel is stop as a bounded label.
func stopLabel(stop Stop) string {
	if stop == StopNone {
		return stopNone
	}
	return string(stop)
}

// timeStep runs one pipeline step, reports how long it took and hands back
// what it returned. Ten steps report the same three lines otherwise.
func timeStep[T any](m Metrics, name string, fn func() (T, error)) (T, error) {
	start := time.Now()
	v, err := fn()
	m.Step(name, time.Since(start), err)
	return v, err
}
