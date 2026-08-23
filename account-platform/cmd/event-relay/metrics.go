package main

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type relayMetrics struct {
	pending      atomic.Int64
	deadLettered atomic.Int64
	oldestMillis atomic.Int64
	registry     *prometheus.Registry
	cycles       *prometheus.CounterVec
	events       *prometheus.CounterVec
	duration     prometheus.Histogram
}

func newRelayMetrics(source string) *relayMetrics {
	metrics := &relayMetrics{registry: prometheus.NewRegistry()}
	labels := prometheus.Labels{"source": source}
	metrics.cycles = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_relay_cycles_total", Help: "Event relay cycles by bounded outcome.", ConstLabels: labels}, []string{"outcome"})
	metrics.events = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_relay_events_total", Help: "Event relay event outcomes.", ConstLabels: labels}, []string{"outcome"})
	metrics.duration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "pinvou_relay_cycle_duration_seconds", Help: "Event relay cycle duration.", ConstLabels: labels})
	metrics.registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_outbox_pending_events", Help: "Outbox events awaiting successful publication.", ConstLabels: labels}, func() float64 { return float64(metrics.pending.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_outbox_dead_lettered_events", Help: "Outbox events requiring approved manual replay.", ConstLabels: labels}, func() float64 { return float64(metrics.deadLettered.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_outbox_oldest_pending_age_seconds", Help: "Age of the oldest unpublished outbox event.", ConstLabels: labels}, func() float64 { return float64(metrics.oldestMillis.Load()) / 1000 }),
		metrics.cycles,
		metrics.events,
		metrics.duration,
	)
	return metrics
}

func (metrics *relayMetrics) recordCycle(result events.RelayResult, err error, elapsed time.Duration) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	metrics.cycles.WithLabelValues(outcome).Inc()
	metrics.events.WithLabelValues("published").Add(float64(result.Published))
	metrics.events.WithLabelValues("retry").Add(float64(result.Failed))
	metrics.events.WithLabelValues("dead_lettered").Add(float64(result.DeadLettered))
	metrics.duration.Observe(elapsed.Seconds())
}

func (metrics *relayMetrics) record(stats events.OutboxStats) {
	metrics.pending.Store(stats.Pending)
	metrics.deadLettered.Store(stats.DeadLettered)
	metrics.oldestMillis.Store(stats.OldestPendingAge.Milliseconds())
}

func (metrics *relayMetrics) server(address string, ready func(context.Context) error) *http.Server {
	router := httpserver.NewRouter()
	router.GET("/metrics", httpserver.Adapt(promhttp.HandlerFor(metrics.registry, promhttp.HandlerOpts{EnableOpenMetrics: true})))
	router.GET("/health/live", httpserver.AdaptFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}))
	router.GET("/health/ready", httpserver.AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Cache-Control", "no-store")
		if ready == nil || ready(ctx) != nil {
			http.Error(w, `{"status":"not_ready"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}))
	return &http.Server{Addr: address, Handler: httpserver.CorrelationHandler(router), ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second}
}

func shutdownServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
