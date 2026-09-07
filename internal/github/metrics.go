package github

import "time"

// RequestMetrics reports what the retry transport sees on the wire. It is
// implemented in internal/api over Prometheus vectors; this package keeps
// the interface so nothing here depends on a metrics library.
type RequestMetrics interface {
	// Request reports one HTTP attempt. status is the numeric code as a
	// string, or "error" when the request never produced a response.
	Request(method, status string, d time.Duration)
	// Retry counts one throttled attempt about to be waited out. reason is
	// the header that decided the wait: retry_after, rate_limit or backoff.
	Retry(reason string)
	// RateLimitRemaining is the last X-RateLimit-Remaining GitHub sent.
	RateLimitRemaining(n int)
}

// NopRequestMetrics is the default when a ClientConfig carries no Metrics.
type NopRequestMetrics struct{}

func (NopRequestMetrics) Request(string, string, time.Duration) {}
func (NopRequestMetrics) Retry(string)                          {}
func (NopRequestMetrics) RateLimitRemaining(int)                {}

// TranslateMetrics counts the deliveries the translator turned into a
// processing row. event is the GitHub event name and result is the
// processing vocabulary: translated, ignored or rejected.
type TranslateMetrics interface {
	Delivery(event, result string)
}

// NopTranslateMetrics is the default when a Translator carries no Metrics.
type NopTranslateMetrics struct{}

func (NopTranslateMetrics) Delivery(string, string) {}

// The retry reasons, which are the metric's label vocabulary.
const (
	reasonRetryAfter = "retry_after"
	reasonRateLimit  = "rate_limit"
	reasonBackoff    = "backoff"
)

// statusError is the status label for a request that never got a response.
const statusError = "error"
