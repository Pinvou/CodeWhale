package main

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type relayMetrics struct {
	pending      atomic.Int64
	deadLettered atomic.Int64
	oldestMillis atomic.Int64
	registry     *prometheus.Registry
}

func newRelayMetrics(source string) *relayMetrics {
	metrics := &relayMetrics{registry: prometheus.NewRegistry()}
	labels := prometheus.Labels{"source": source}
	metrics.registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_outbox_pending_events", Help: "Outbox events awaiting successful publication.", ConstLabels: labels}, func() float64 { return float64(metrics.pending.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_outbox_dead_lettered_events", Help: "Outbox events requiring approved manual replay.", ConstLabels: labels}, func() float64 { return float64(metrics.deadLettered.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_outbox_oldest_pending_age_seconds", Help: "Age of the oldest unpublished outbox event.", ConstLabels: labels}, func() float64 { return float64(metrics.oldestMillis.Load()) / 1000 }),
	)
	return metrics
}

func (metrics *relayMetrics) record(stats events.OutboxStats) {
	metrics.pending.Store(stats.Pending)
	metrics.deadLettered.Store(stats.DeadLettered)
	metrics.oldestMillis.Store(stats.OldestPendingAge.Milliseconds())
}

func (metrics *relayMetrics) server(address string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.registry, promhttp.HandlerOpts{EnableOpenMetrics: true}))
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
	return &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second}
}

func shutdownServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
