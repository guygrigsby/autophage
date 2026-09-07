package app

import "time"

// The interfaces here are what this package reports and nothing more:
// internal/api implements them over Prometheus vectors, and the daemon wires
// those in. Keeping the interface here rather than importing a metrics
// library keeps Prometheus out of the sweep loop's dependencies, and the
// exported no-ops mean a zero-valued Dispatcher, Triage or Commenter still
// runs, which is what every test in this package relies on.

// SweepMetrics reports the dispatcher's sweeps and the steps inside one.
// step is a closed vocabulary: translate, cancel, enroll, triage, comment
// and schedule.
type SweepMetrics interface {
	// Sweep counts one whole sweep.
	Sweep()
	// Step reports one step of a sweep, with the error it returned, if any.
	Step(step string, d time.Duration, err error)
}

// NopSweepMetrics is the default when a Dispatcher has no Metrics.
type NopSweepMetrics struct{}

func (NopSweepMetrics) Sweep()                            {}
func (NopSweepMetrics) Step(string, time.Duration, error) {}

// TriageMetrics reports one triage's model call. size is small, large, or
// none when the model returned no size at all; result is ok, error or
// parked.
type TriageMetrics interface {
	Triage(size, result string, d time.Duration)
}

// NopTriageMetrics is the default when a Triage has no Metrics.
type NopTriageMetrics struct{}

func (NopTriageMetrics) Triage(string, string, time.Duration) {}

// CommentMetrics reports one comment post, ok or error.
type CommentMetrics interface {
	Post(result string)
}

// NopCommentMetrics is the default when a Commenter has no Metrics.
type NopCommentMetrics struct{}

func (NopCommentMetrics) Post(string) {}

// The label vocabularies. They are the dashboard's contract: a value added
// here has to be added to the panels that group by it.
const (
	sizeNone     = "none"
	resultOK     = "ok"
	resultError  = "error"
	resultParked = "parked"
)
