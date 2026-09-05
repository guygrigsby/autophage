package api

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/guygrigsby/autophage/internal/store"
)

// metricsHandler serves Prometheus text. Gauges are read from the store on
// every scrape; counters and the histogram are incremented by the runner
// through the exported vectors below.
func metricsHandler(st *store.Store) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(AttemptsTotal, AttemptTokens, AttemptWallClock, DeliveriesTotal)
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "autophage_queue_depth", Help: "Queued cases"}, func() float64 {
		q, err := st.QueuedCases(contextBackground())
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
)

// stateCollector exposes autophage_cases{state} from CountByState.
type stateCollector struct{ st *store.Store }

var casesDesc = prometheus.NewDesc("autophage_cases", "Cases by state", []string{"state"}, nil)

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) { ch <- casesDesc }

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	counts, err := c.st.CountByState(contextBackground())
	if err != nil {
		return
	}
	for state, n := range counts {
		ch <- prometheus.MustNewConstMetric(casesDesc, prometheus.GaugeValue, float64(n), state)
	}
}

func contextBackground() context.Context { return context.Background() }
