package token

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

type AdminPaginationMetrics struct {
	activeSnapshots prometheus.Gauge
	activeMembers   prometheus.Gauge
	storageBytes    prometheus.Gauge
	cleanupLag      prometheus.Gauge
	capacityLevel   *prometheus.GaugeVec
	creations       *prometheus.CounterVec
}

func newAdminPaginationMetrics() *AdminPaginationMetrics {
	return &AdminPaginationMetrics{
		activeSnapshots: prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_token_admin_pagination_active_snapshots", Help: "Active Token admin pagination snapshots."}),
		activeMembers:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_token_admin_pagination_active_members", Help: "Stored members in active Token admin pagination snapshots."}),
		storageBytes:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_token_admin_pagination_storage_bytes", Help: "PostgreSQL bytes occupied by Token admin pagination snapshot tables."}),
		cleanupLag:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_token_admin_pagination_cleanup_lag_seconds", Help: "Age of the oldest expired Token admin snapshot awaiting cleanup."}),
		capacityLevel:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "pinvou_token_admin_pagination_capacity_level", Help: "Token pagination capacity signal: normal, warning (70%), or critical (85%)."}, []string{"level"}),
		creations:       prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_token_admin_pagination_snapshot_creations_total", Help: "Token admin pagination snapshot creation outcomes."}, []string{"resource", "outcome"}),
	}
}

func (r *PostgresRepository) AdminPaginationCollectors() []prometheus.Collector {
	if r == nil || r.adminPaginationMetrics == nil {
		return nil
	}
	m := r.adminPaginationMetrics
	collectors := []prometheus.Collector{m.activeSnapshots, m.activeMembers, m.storageBytes, m.cleanupLag, m.capacityLevel, m.creations}
	if r.routeHealthMetrics != nil {
		collectors = append(collectors, r.routeHealthMetrics.probes, r.routeHealthMetrics.state, r.routeHealthMetrics.backlog, r.routeHealthMetrics.lag, r.routeHealthMetrics.inflight)
	}
	return collectors
}

func (r *PostgresRepository) recordAdminPaginationCreation(resource string, err error) {
	if r.adminPaginationMetrics == nil {
		return
	}
	outcome := "success"
	switch {
	case errors.Is(err, ErrAdminPaginationFilterRequired):
		outcome = "filter_required"
	case errors.Is(err, ErrAdminPaginationSnapshotLimited):
		outcome = "limited"
	case errors.Is(err, ErrAdminPaginationSnapshotUnavailable):
		outcome = "unavailable"
	}
	r.adminPaginationMetrics.creations.WithLabelValues(resource, outcome).Inc()
}

func (r *PostgresRepository) refreshAdminPaginationMetrics(ctx context.Context) error {
	if r.adminPaginationMetrics == nil {
		return nil
	}
	capacity, err := r.AdminPaginationCapacity(ctx)
	if err != nil {
		return err
	}
	m := r.adminPaginationMetrics
	m.activeSnapshots.Set(float64(capacity.ActiveSnapshots))
	m.activeMembers.Set(float64(capacity.ActiveMembers))
	m.storageBytes.Set(float64(capacity.StorageBytes))
	m.cleanupLag.Set(capacity.OldestCleanupLag.Seconds())
	level := "normal"
	if capacity.Critical {
		level = "critical"
	} else if capacity.Warning {
		level = "warning"
	}
	for _, candidate := range []string{"normal", "warning", "critical"} {
		value := 0.0
		if candidate == level {
			value = 1
		}
		m.capacityLevel.WithLabelValues(candidate).Set(value)
	}
	return nil
}
