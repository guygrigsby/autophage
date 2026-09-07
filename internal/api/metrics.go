package api

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/guygrigsby/autophage/internal/store"
)

// scrapeTimeout bounds every query a metrics collector runs against the
// store, so a stuck database cannot hang a scrape indefinitely.
const scrapeTimeout = 5 * time.Second

// scrapeContext returns a fresh, bounded context for one collector query.
// The caller must defer the cancel func.
func scrapeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), scrapeTimeout)
}

// metricsHandler serves Prometheus text. Gauges are read from the store on
// every scrape; the counters and histograms are moved by the packages that
// emit, through the implementations in metrics_hooks.go and
// metrics_runner.go. version and startedAt are fixed for the life of the
// process, so their gauges are set once, here.
func metricsHandler(st *store.Store, version string, startedAt time.Time) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(AttemptsTotal, AttemptTokens, AttemptWallClock, DeliveriesTotal)
	reg.MustRegister(AttemptsRunning, AttemptStepSeconds, AttemptStepErrors, AttemptToolCalls, AttemptToolSeconds, AttemptTurns, AttemptDiffLines, AttemptStops)
	reg.MustRegister(Sweeps, SweepStepSeconds, SweepStepErrors, CommentPosts, Triages, TriageSeconds)
	reg.MustRegister(GitHubRequests, GitHubRequestSeconds, GitHubRetries, GitHubRateLimitRemaining)
	reg.MustRegister(ModelRequests, ModelLatency, ModelTokens, ModelCost)

	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "autophage_daemon_info", Help: "Always 1, labelled with the running version"}, []string{"version"})
	info.WithLabelValues(version).Set(1)
	reg.MustRegister(info)
	start := prometheus.NewGauge(prometheus.GaugeOpts{Name: "autophage_daemon_start_time_seconds", Help: "When this daemon started, in epoch seconds"})
	if !startedAt.IsZero() {
		start.Set(float64(startedAt.Unix()))
	}
	reg.MustRegister(start)
	// Read on every scrape rather than counted in memory: it answers "has
	// anything reached the daemon lately", which a restart must not reset.
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "autophage_last_delivery_timestamp_seconds", Help: "The most recent webhook delivery, in epoch seconds, 0 when none"}, func() float64 {
		ctx, cancel := scrapeContext()
		defer cancel()
		at, ok, err := st.LastDeliveryAt(ctx)
		if err != nil || !ok {
			return 0
		}
		return float64(at.Unix())
	}))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "autophage_queue_depth", Help: "Queued cases"}, func() float64 {
		ctx, cancel := scrapeContext()
		defer cancel()
		q, err := st.QueuedCases(ctx)
		if err != nil {
			return 0
		}
		return float64(len(q))
	}))
	reg.MustRegister(&stateCollector{st: st})
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

var (
	AttemptsTotal    = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_attempts_total", Help: "Attempts ended, by kind and outcome"}, []string{"kind", "outcome"})
	AttemptTokens    = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_attempt_tokens_total", Help: "Model tokens by direction"}, []string{"direction"})
	AttemptWallClock = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "autophage_attempt_wall_clock_seconds", Help: "Attempt wall clock", Buckets: prometheus.ExponentialBuckets(30, 2, 10)})
	DeliveriesTotal  = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_deliveries_total", Help: "Webhook deliveries processed, by event and result"}, []string{"event", "result"})

	AttemptsRunning = prometheus.NewGauge(prometheus.GaugeOpts{Name: "autophage_attempts_running", Help: "Attempts in flight on this daemon"})
	// Up to four hours, not one: the approved budget's wall clock is three,
	// so a ceiling of an hour would put every approved run's step="run" in
	// the overflow bucket and leave the quantile that matters unreadable.
	AttemptStepSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "autophage_attempt_step_seconds", Help: "One step of the attempt pipeline", Buckets: prometheus.ExponentialBucketsRange(0.1, 14400, 16)}, []string{"step"})
	AttemptStepErrors  = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_attempt_step_errors_total", Help: "Attempt pipeline steps that failed"}, []string{"step"})
	AttemptToolCalls   = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_attempt_tool_calls_total", Help: "Tool calls the agent made, by tool and result"}, []string{"tool", "result"})
	AttemptToolSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "autophage_attempt_tool_seconds", Help: "One tool call", Buckets: prometheus.ExponentialBucketsRange(0.05, 600, 14)}, []string{"tool"})
	AttemptTurns       = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "autophage_attempt_turns", Help: "Turns an attempt spent", Buckets: prometheus.ExponentialBucketsRange(1, 256, 9)})
	AttemptDiffLines   = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "autophage_attempt_diff_lines", Help: "Diff lines an attempt produced", Buckets: prometheus.ExponentialBucketsRange(1, 4096, 13)})
	AttemptStops       = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_attempt_stops_total", Help: "Why attempt runs ended"}, []string{"stop"})

	Sweeps           = prometheus.NewCounter(prometheus.CounterOpts{Name: "autophage_sweeps_total", Help: "Sweeps the dispatcher ran"})
	SweepStepSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "autophage_sweep_step_seconds", Help: "One step of a sweep", Buckets: prometheus.ExponentialBucketsRange(0.05, 300, 14)}, []string{"step"})
	SweepStepErrors  = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_sweep_step_errors_total", Help: "Sweep steps that failed"}, []string{"step"})

	Triages       = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_triages_total", Help: "Triages, by the size the model returned and the result"}, []string{"size", "result"})
	TriageSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "autophage_triage_seconds", Help: "One triage model call", Buckets: prometheus.ExponentialBucketsRange(0.5, 120, 12)})
	CommentPosts  = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_comment_posts_total", Help: "Issue comments posted, by result"}, []string{"result"})

	GitHubRequests           = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_github_requests_total", Help: "GitHub API attempts, by method and status"}, []string{"method", "status"})
	GitHubRequestSeconds     = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "autophage_github_request_seconds", Help: "One GitHub API attempt", Buckets: prometheus.ExponentialBucketsRange(0.05, 60, 12)}, []string{"method"})
	GitHubRetries            = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_github_retries_total", Help: "Throttled GitHub attempts waited out, by what decided the wait"}, []string{"reason"})
	GitHubRateLimitRemaining = prometheus.NewGauge(prometheus.GaugeOpts{Name: "autophage_github_rate_limit_remaining", Help: "The last X-RateLimit-Remaining GitHub sent"})

	ModelRequests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_model_requests_total", Help: "Model calls, by model and result"}, []string{"model", "result"})
	ModelLatency  = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "autophage_model_latency_seconds", Help: "One model call", Buckets: prometheus.ExponentialBucketsRange(0.5, 600, 12)}, []string{"model"})
	ModelTokens   = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_model_tokens_total", Help: "Model tokens, by model and direction"}, []string{"model", "direction"})
	ModelCost     = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_model_cost_usd_total", Help: "Provider-reported cost in USD, by model"}, []string{"model"})
)

// stateCollector exposes autophage_cases{state} from CountByState.
type stateCollector struct{ st *store.Store }

var casesDesc = prometheus.NewDesc("autophage_cases", "Cases by state", []string{"state"}, nil)

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) { ch <- casesDesc }

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := scrapeContext()
	defer cancel()
	counts, err := c.st.CountByState(ctx)
	if err != nil {
		return
	}
	for state, n := range counts {
		ch <- prometheus.MustNewConstMetric(casesDesc, prometheus.GaugeValue, float64(n), state)
	}
}
