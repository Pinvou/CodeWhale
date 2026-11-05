package billing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const adminSnapshotMaxStorageBytes int64 = 8 * 1024 * 1024 * 1024

const adminPaginationStorageQuery = `SELECT pg_total_relation_size('admin_pagination_snapshots')+pg_total_relation_size('admin_pagination_snapshot_members')+pg_total_relation_size('admin_pagination_snapshot_tombstones')`

var adminPaginationResources = []string{
	adminPageWallets, adminPageWalletLedger, adminPageReceivables, adminPagePaymentOrders,
	adminPageRefunds, adminPageAdjustments, adminPageUsageHolds, adminPageLatePaymentReviews,
	adminPagePaymentAnomalies, adminPageReconciliationRuns, adminPageReconciliationCases,
}

type AdminPaginationMetrics struct {
	activeSnapshots *prometheus.GaugeVec
	members         *prometheus.GaugeVec
	storageBytes    prometheus.Gauge
	cleanupOldest   prometheus.Gauge
	creations       *prometheus.CounterVec
}

func newAdminPaginationMetrics() *AdminPaginationMetrics {
	return &AdminPaginationMetrics{
		activeSnapshots: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "pinvou_admin_pagination_active_snapshots", Help: "Active Billing admin pagination snapshots by resource."}, []string{"resource"}),
		members:         prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "pinvou_admin_pagination_snapshot_members", Help: "Stored active Billing admin pagination members by resource."}, []string{"resource"}),
		storageBytes:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_admin_pagination_storage_bytes", Help: "PostgreSQL bytes occupied by Billing admin pagination snapshot tables."}),
		cleanupOldest:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "pinvou_admin_pagination_cleanup_oldest_age_seconds", Help: "Age of the oldest expired Billing admin pagination snapshot awaiting cleanup."}),
		creations:       prometheus.NewCounterVec(prometheus.CounterOpts{Name: "pinvou_admin_pagination_snapshot_creations_total", Help: "Billing admin pagination snapshot creation outcomes."}, []string{"resource", "outcome"}),
	}
}

func (metrics *AdminPaginationMetrics) Collectors() []prometheus.Collector {
	if metrics == nil {
		return nil
	}
	return []prometheus.Collector{metrics.activeSnapshots, metrics.members, metrics.storageBytes, metrics.cleanupOldest, metrics.creations}
}

func (metrics *AdminPaginationMetrics) recordCreation(resource string, err error) {
	if metrics == nil {
		return
	}
	outcome := "success"
	switch {
	case errors.Is(err, ErrPaginationFilterRequired):
		outcome = "filter_required"
	case errors.Is(err, ErrPaginationSnapshotLimited):
		outcome = "limited"
	case errors.Is(err, ErrPaginationSnapshotUnavailable):
		outcome = "unavailable"
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "query timeout") {
			outcome = "timeout"
		}
	}
	metrics.creations.WithLabelValues(resource, outcome).Inc()
}

func (r *PostgresRepository) AdminPaginationCollectors() []prometheus.Collector {
	if r == nil {
		return nil
	}
	return r.paginationMetrics.Collectors()
}

func (r *PostgresRepository) refreshAdminPaginationMetrics(ctx context.Context) error {
	if r.paginationMetrics == nil {
		return nil
	}
	type resourceStats struct {
		resource           string
		snapshots, members int64
	}
	rows, err := r.pool.Query(ctx, `SELECT resource_type,count(*),COALESCE(sum(member_count),0) FROM admin_pagination_snapshots WHERE expires_at>now() GROUP BY resource_type`)
	if err != nil {
		return err
	}
	stats := make(map[string]resourceStats, len(adminPaginationResources))
	for rows.Next() {
		var item resourceStats
		if err := rows.Scan(&item.resource, &item.snapshots, &item.members); err != nil {
			rows.Close()
			return err
		}
		stats[item.resource] = item
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, resource := range adminPaginationResources {
		item := stats[resource]
		r.paginationMetrics.activeSnapshots.WithLabelValues(resource).Set(float64(item.snapshots))
		r.paginationMetrics.members.WithLabelValues(resource).Set(float64(item.members))
	}
	var storageBytes int64
	var oldestSeconds float64
	err = r.pool.QueryRow(ctx, adminPaginationStorageQuery+`,COALESCE(EXTRACT(EPOCH FROM now()-(min(expires_at) FILTER (WHERE expires_at<=now()))),0) FROM admin_pagination_snapshots`).Scan(&storageBytes, &oldestSeconds)
	if err != nil {
		return err
	}
	r.paginationMetrics.storageBytes.Set(float64(storageBytes))
	r.paginationMetrics.cleanupOldest.Set(oldestSeconds)
	return nil
}

func (r *PostgresRepository) adminPaginationStorageBytes(ctx context.Context) (int64, error) {
	var bytes int64
	err := r.pool.QueryRow(ctx, adminPaginationStorageQuery).Scan(&bytes)
	return bytes, err
}

func (r *PostgresRepository) markAdminPaginationMetricsStale() {
	if r.paginationMetrics == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.refreshAdminPaginationMetrics(ctx)
}
