package main

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type relayMetrics struct {
	pending                  atomic.Int64
	deadLettered             atomic.Int64
	oldestMillis             atomic.Int64
	authorizationPending     atomic.Int64
	authorizationActive      atomic.Int64
	authorizationReclaimable atomic.Int64
	authorizationDead        atomic.Int64
	authorizationHandlers    atomic.Int64
	registry                 *prometheus.Registry
	cycles                   *prometheus.CounterVec
	events                   *prometheus.CounterVec
	duration                 prometheus.Histogram
	approvalCycles           *prometheus.CounterVec
	approvalEvents           *prometheus.CounterVec
	approvalDuration         prometheus.Histogram
}

func newRelayMetrics(source string) *relayMetrics {
	metrics := &relayMetrics{registry: prometheus.NewRegistry()}
	labels := prometheus.Labels{"source": source}
	metrics.cycles = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_relay_cycles_total", Help: "Event relay cycles by bounded outcome.", ConstLabels: labels}, []string{"outcome"})
	metrics.events = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_relay_events_total", Help: "Event relay event outcomes.", ConstLabels: labels}, []string{"outcome"})
	metrics.duration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "pinvou_relay_cycle_duration_seconds", Help: "Event relay cycle duration.", ConstLabels: labels})
	metrics.approvalCycles = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_authorization_relay_cycles_total", Help: "Authorization relay cycles by bounded outcome.", ConstLabels: labels}, []string{"outcome"})
	metrics.approvalEvents = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_authorization_relay_events_total", Help: "Authorization relay event outcomes.", ConstLabels: labels}, []string{"outcome"})
	metrics.approvalDuration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "pinvou_authorization_relay_cycle_duration_seconds", Help: "Authorization relay cycle duration.", ConstLabels: labels})
	metrics.registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_outbox_pending_events", Help: "Outbox events awaiting successful publication.", ConstLabels: labels}, func() float64 { return float64(metrics.pending.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_outbox_dead_lettered_events", Help: "Outbox events that stopped automatic delivery retries.", ConstLabels: labels}, func() float64 { return float64(metrics.deadLettered.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_outbox_oldest_pending_age_seconds", Help: "Age of the oldest unpublished outbox event.", ConstLabels: labels}, func() float64 { return float64(metrics.oldestMillis.Load()) / 1000 }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_authorization_outbox_pending_events", Help: "Authorization events awaiting publication.", ConstLabels: labels}, func() float64 { return float64(metrics.authorizationPending.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_authorization_outbox_active_claims", Help: "Authorization relay claims with a current lease.", ConstLabels: labels}, func() float64 { return float64(metrics.authorizationActive.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_authorization_outbox_reclaimable_claims", Help: "Authorization relay claims whose lease expired.", ConstLabels: labels}, func() float64 { return float64(metrics.authorizationReclaimable.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_authorization_outbox_unclassified_dead_letters", Help: "Authorization dead letters requiring explicit reconciliation.", ConstLabels: labels}, func() float64 { return float64(metrics.authorizationDead.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "pinvou_authorization_relay_active_handlers", Help: "Process-local authorization Broker.Publish handlers currently executing.", ConstLabels: labels}, func() float64 { return float64(metrics.authorizationHandlers.Load()) }),
		metrics.cycles,
		metrics.events,
		metrics.duration,
		metrics.approvalCycles, metrics.approvalEvents, metrics.approvalDuration,
	)
	return metrics
}

func (metrics *relayMetrics) BeginFrozenPublish() {
	metrics.authorizationHandlers.Add(1)
}

func (metrics *relayMetrics) EndFrozenPublish() {
	for {
		current := metrics.authorizationHandlers.Load()
		if current == 0 || metrics.authorizationHandlers.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func (metrics *relayMetrics) recordApprovalCycle(result events.RelayResult, err error, elapsed time.Duration) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	metrics.approvalCycles.WithLabelValues(outcome).Inc()
	metrics.approvalEvents.WithLabelValues("published").Add(float64(result.Published))
	metrics.approvalEvents.WithLabelValues("retry").Add(float64(result.Failed))
	metrics.approvalEvents.WithLabelValues("dead_lettered").Add(float64(result.DeadLettered))
	metrics.approvalDuration.Observe(elapsed.Seconds())
}

func (metrics *relayMetrics) recordApproval(stats account.ApprovalRelayStats) {
	metrics.authorizationPending.Store(stats.Pending)
	metrics.authorizationActive.Store(stats.ActiveClaims)
	metrics.authorizationReclaimable.Store(stats.ReclaimableClaims)
	metrics.authorizationDead.Store(stats.UnclassifiedDeadRows)
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
	return metrics.serverWithApproval(address, ready, nil)
}

func (metrics *relayMetrics) serverWithApproval(address string, ready, approvalReady func(context.Context) error) *http.Server {
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
	if approvalReady != nil {
		router.GET("/health/ready/approval-authorization", httpserver.AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			w.Header().Set("Cache-Control", "no-store")
			if approvalReady(ctx) != nil {
				http.Error(w, `{"status":"not_ready"}`, http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		}))
	}
	return &http.Server{Addr: address, Handler: httpserver.CorrelationHandler(router), ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second}
}

func shutdownServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
