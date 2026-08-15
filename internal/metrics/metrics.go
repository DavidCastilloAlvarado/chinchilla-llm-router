// Package metrics exposes Prometheus metrics for the router.
//
// All collectors are registered on a dedicated prometheus.Registry so the
// router does not collide with the process default registry. The registry is
// served at GET /metrics by the API layer.
//
// Every method on *Metrics is nil-safe: a nil receiver is a no-op, so
// callers can pass the metrics unconditionally without feature checks.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// durationBuckets fits LLM request latencies (tens of ms to minutes).
var durationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// Metrics holds the router's Prometheus collectors.
type Metrics struct {
	Registry *prometheus.Registry

	// RequestsTotal counts routed requests by outcome (status: success|error).
	RequestsTotal *prometheus.CounterVec
	// RequestDurationSeconds is the total duration of a routed request,
	// all attempts included.
	RequestDurationSeconds *prometheus.HistogramVec
	// AttemptsTotal counts individual backend attempts by result
	// (result: success|failure|timeout).
	AttemptsTotal *prometheus.CounterVec
	// ReroutesTotal counts requests that had to be retried on another
	// backend after a failed attempt (reason: error|timeout).
	ReroutesTotal *prometheus.CounterVec
	// BackendUp reports backend availability: 1 = available, 0 = in cooldown.
	BackendUp *prometheus.GaugeVec
	// Inflight counts requests currently being routed.
	Inflight prometheus.Gauge
}

// New creates and registers all collectors on a fresh registry.
func New() *Metrics {
	m := &Metrics{Registry: prometheus.NewRegistry()}

	m.RequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_router_requests_total",
		Help: "Routed requests by logical model, mode, streaming and outcome.",
	}, []string{"model", "mode", "stream", "status"})

	m.RequestDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llm_router_request_duration_seconds",
		Help:    "Total duration of routed requests (all attempts included).",
		Buckets: durationBuckets,
	}, []string{"model", "mode", "stream"})

	m.AttemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_router_backend_attempts_total",
		Help: "Individual backend attempts by result (success, failure, timeout).",
	}, []string{"model", "backend", "result"})

	m.ReroutesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_router_reroutes_total",
		Help: "Requests rerouted to another backend after a failed attempt.",
	}, []string{"model", "reason"})

	m.BackendUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llm_router_backend_up",
		Help: "Backend availability: 1 = available, 0 = in cooldown.",
	}, []string{"model", "backend"})

	m.Inflight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "llm_router_inflight_requests",
		Help: "Requests currently being routed.",
	})

	m.Registry.MustRegister(
		m.RequestsTotal,
		m.RequestDurationSeconds,
		m.AttemptsTotal,
		m.ReroutesTotal,
		m.BackendUp,
		m.Inflight,
	)
	return m
}

func streamLabel(stream bool) string {
	if stream {
		return "true"
	}
	return "false"
}

// ObserveRequest records a completed routed request.
func (m *Metrics) ObserveRequest(model, mode string, stream bool, status string, d time.Duration) {
	if m == nil {
		return
	}
	s := streamLabel(stream)
	m.RequestsTotal.WithLabelValues(model, mode, s, status).Inc()
	m.RequestDurationSeconds.WithLabelValues(model, mode, s).Observe(d.Seconds())
}

// ObserveAttempt records one backend attempt.
func (m *Metrics) ObserveAttempt(model, backend, result string, d time.Duration) {
	if m == nil {
		return
	}
	m.AttemptsTotal.WithLabelValues(model, backend, result).Inc()
}

// ObserveReroute records a reroute to another backend (reason: error|timeout).
func (m *Metrics) ObserveReroute(model, reason string) {
	if m == nil {
		return
	}
	m.ReroutesTotal.WithLabelValues(model, reason).Inc()
}

// SetBackendUp updates the availability gauge of a backend.
func (m *Metrics) SetBackendUp(model, backend string, up bool) {
	if m == nil {
		return
	}
	v := 0.0
	if up {
		v = 1.0
	}
	m.BackendUp.WithLabelValues(model, backend).Set(v)
}

// InflightInc increments the in-flight request gauge.
func (m *Metrics) InflightInc() {
	if m == nil {
		return
	}
	m.Inflight.Inc()
}

// InflightDec decrements the in-flight request gauge.
func (m *Metrics) InflightDec() {
	if m == nil {
		return
	}
	m.Inflight.Dec()
}

// Handler returns the Prometheus scrape handler for the registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}
