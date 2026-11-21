package billing

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// These observations are retained evidence. Scraping their physical size never
// scans observation history or gives the collector any cleanup authority.
type healthRetentionCollector struct {
	pool             *pgxpool.Pool
	bytes, available *prometheus.Desc
}

func (r *PostgresRepository) HealthRetentionCollectors() []prometheus.Collector {
	if r == nil {
		return nil
	}
	return []prometheus.Collector{&healthRetentionCollector{
		pool:      r.pool,
		bytes:     prometheus.NewDesc("pinvou_payment_health_retained_storage_bytes", "Billing health observation table bytes including indexes and TOAST; excludes related attempts and cases.", nil, nil),
		available: prometheus.NewDesc("pinvou_payment_health_storage_available", "Whether this scrape read health observation storage successfully; missing size is not zero.", nil, nil),
	}}
}

func (c *healthRetentionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.bytes
	ch <- c.available
}

func (c *healthRetentionCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var bytes int64
	if err := c.pool.QueryRow(ctx, `SELECT pg_total_relation_size('public.payment_provider_health_observations'::regclass)`).Scan(&bytes); err != nil {
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.bytes, prometheus.GaugeValue, float64(bytes))
}
