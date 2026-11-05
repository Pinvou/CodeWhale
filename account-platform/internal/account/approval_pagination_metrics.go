package account

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

type ApprovalPaginationMetrics struct {
	activeSnapshots prometheus.Gauge
	activeMembers   prometheus.Gauge
	storageBytes    prometheus.Gauge
	cleanupLag      prometheus.Gauge
	capacityLevel   *prometheus.GaugeVec
	creations       *prometheus.CounterVec
}

func newApprovalPaginationMetrics() *ApprovalPaginationMetrics {
	return &ApprovalPaginationMetrics{
		activeSnapshots: prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_account_approval_pagination_active_snapshots", Help: "Active Account approval pagination snapshots."}),
		activeMembers:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_account_approval_pagination_active_members", Help: "Frozen members in active Account approval pagination snapshots."}),
		storageBytes:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_account_approval_pagination_storage_bytes", Help: "PostgreSQL bytes occupied by Account approval pagination snapshot tables."}),
		cleanupLag:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_account_approval_pagination_cleanup_lag_seconds", Help: "Age of the oldest expired Account approval snapshot awaiting cleanup."}),
		capacityLevel:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "pinvou_account_approval_pagination_capacity_level", Help: "Account approval pagination capacity signal: normal, warning, or critical."}, []string{"level"}),
		creations:       prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_account_approval_pagination_snapshot_creations_total", Help: "Account approval pagination snapshot creation outcomes."}, []string{"outcome"}),
	}
}

func (r *PostgresRepository) ApprovalPaginationCollectors() []prometheus.Collector {
	if r == nil || r.approvalPaginationMetrics == nil {
		return nil
	}
	metrics := r.approvalPaginationMetrics
	return []prometheus.Collector{metrics.activeSnapshots, metrics.activeMembers, metrics.storageBytes, metrics.cleanupLag, metrics.capacityLevel, metrics.creations}
}

func (r *PostgresRepository) recordApprovalPaginationCreation(err error) {
	if r == nil || r.approvalPaginationMetrics == nil {
		return
	}
	outcome := "success"
	switch {
	case errors.Is(err, ErrApprovalPaginationFilterRequired):
		outcome = "filter_required"
	case errors.Is(err, ErrApprovalPaginationSnapshotLimited):
		outcome = "limited"
	case err != nil:
		outcome = "unavailable"
	}
	r.approvalPaginationMetrics.creations.WithLabelValues(outcome).Inc()
}

func (r *PostgresRepository) refreshApprovalPaginationMetrics(ctx context.Context) error {
	if r == nil || r.approvalPaginationMetrics == nil {
		return nil
	}
	capacity, err := r.ApprovalPaginationCapacity(ctx)
	if err != nil {
		return err
	}
	metrics := r.approvalPaginationMetrics
	metrics.activeSnapshots.Set(float64(capacity.ActiveSnapshots))
	metrics.activeMembers.Set(float64(capacity.ActiveMembers))
	metrics.storageBytes.Set(float64(capacity.StorageBytes))
	metrics.cleanupLag.Set(capacity.OldestCleanupLag.Seconds())
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
		metrics.capacityLevel.WithLabelValues(candidate).Set(value)
	}
	return nil
}
