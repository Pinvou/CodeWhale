package billing

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"net/http"
	"time"
)

type financePricingOverviewValues struct {
	PriceDraftCount            int64
	PriceFutureCount           int64
	PriceActiveCount           int64
	PriceEndedOrCancelledCount int64
	MeteringActiveCount        int64
	MeteringInactiveCount      int64
}

func (s *Service) adminFinancePricingOverview(w http.ResponseWriter, r *http.Request) {
	if rejectFinanceOverviewQuery(w, r) {
		return
	}
	repository, ok := s.repository.(interface {
		AdminFinancePricingOverview(context.Context, time.Time) (financePricingOverviewValues, error)
	})
	if !ok {
		writeFinanceOverviewUnavailable(w, r)
		return
	}
	asOf := s.now().UTC()
	values, err := repository.AdminFinancePricingOverview(r.Context(), asOf)
	if err != nil {
		writeFinanceOverviewUnavailable(w, r)
		return
	}
	api.WriteJSON(w, http.StatusOK, FinanceOverview{AsOf: asOf, Window: nil, Metrics: []FinanceOverviewMetric{
		{Key: "price_draft_count", Value: values.PriceDraftCount, Unit: "COUNT", IncludedStatuses: []string{"DRAFT"}},
		{Key: "price_future_count", Value: values.PriceFutureCount, Unit: "COUNT", IncludedStatuses: []string{"PUBLISHED"}},
		{Key: "price_active_count", Value: values.PriceActiveCount, Unit: "COUNT", IncludedStatuses: []string{"ACTIVE"}},
		{Key: "price_ended_or_cancelled_count", Value: values.PriceEndedOrCancelledCount, Unit: "COUNT", IncludedStatuses: []string{"ENDED", "CANCELLED"}},
		{Key: "metering_active_count", Value: values.MeteringActiveCount, Unit: "COUNT", IncludedStatuses: []string{"ACTIVE"}},
		{Key: "metering_inactive_count", Value: values.MeteringInactiveCount, Unit: "COUNT", IncludedStatuses: []string{"INACTIVE"}},
	}})
}

func (r *PostgresRepository) AdminFinancePricingOverview(ctx context.Context, asOf time.Time) (financePricingOverviewValues, error) {
	var result financePricingOverviewValues
	err := r.pool.QueryRow(ctx, `WITH derived_prices AS (
    SELECT CASE
        WHEN lifecycle_status IN ('CANCELLED','ENDED','DRAFT') THEN lifecycle_status
        WHEN $1<effective_from THEN 'PUBLISHED'
        WHEN effective_to IS NOT NULL AND $1>=effective_to THEN 'ENDED'
        ELSE 'ACTIVE'
    END AS derived_status
    FROM pricing_versions
)
SELECT
    count(*) FILTER (WHERE derived_status='DRAFT'),
    count(*) FILTER (WHERE derived_status='PUBLISHED'),
    count(*) FILTER (WHERE derived_status='ACTIVE'),
    count(*) FILTER (WHERE derived_status IN ('ENDED','CANCELLED')),
    (SELECT count(*) FROM metering_unit_definitions WHERE status='ACTIVE'),
    (SELECT count(*) FROM metering_unit_definitions WHERE status='INACTIVE')
FROM derived_prices`, asOf).Scan(
		&result.PriceDraftCount, &result.PriceFutureCount, &result.PriceActiveCount,
		&result.PriceEndedOrCancelledCount, &result.MeteringActiveCount, &result.MeteringInactiveCount,
	)
	return result, err
}
