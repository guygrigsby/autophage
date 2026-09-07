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
// every scrape; counters and the histogram are incremented by the runner
// through the exported vectors below.
func metricsHandler(st *store.Store) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(AttemptsTotal, AttemptTokens, AttemptWallClock, DeliveriesTotal)
	reg.MustRegister(AttemptsRunning, AttemptStepSeconds, AttemptStepErrors, AttemptToolCalls, AttemptToolSeconds, AttemptTurns, AttemptDiffLines, AttemptStops)
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

	AttemptsRunning    = prometheus.NewGauge(prometheus.GaugeOpts{Name: "autophage_attempts_running", Help: "Attempts in flight on this daemon"})
	AttemptStepSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "autophage_attempt_step_seconds", Help: "One step of the attempt pipeline", Buckets: prometheus.ExponentialBucketsRange(0.1, 3600, 14)}, []string{"step"})
	AttemptStepErrors  = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_attempt_step_errors_total", Help: "Attempt pipeline steps that failed"}, []string{"step"})
	AttemptToolCalls   = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_attempt_tool_calls_total", Help: "Tool calls the agent made, by tool and result"}, []string{"tool", "result"})
	AttemptToolSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "autophage_attempt_tool_seconds", Help: "One tool call", Buckets: prometheus.ExponentialBucketsRange(0.05, 600, 14)}, []string{"tool"})
	AttemptTurns       = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "autophage_attempt_turns", Help: "Turns an attempt spent", Buckets: prometheus.ExponentialBucketsRange(1, 256, 9)})
	AttemptDiffLines   = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "autophage_attempt_diff_lines", Help: "Diff lines an attempt produced", Buckets: prometheus.ExponentialBucketsRange(1, 4096, 13)})
	AttemptStops       = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autophage_attempt_stops_total", Help: "Why attempt runs ended"}, []string{"stop"})
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
