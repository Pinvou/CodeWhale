package billing

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

const financeOverviewMaxWindow = 31 * 24 * time.Hour

type FinanceOverviewWindow struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Timezone string    `json:"timezone"`
}

type FinanceOverviewMetric struct {
	Key              string   `json:"key"`
	Value            int64    `json:"value"`
	Unit             string   `json:"unit"`
	IncludedStatuses []string `json:"included_statuses"`
}

type FinanceOverview struct {
	AsOf    time.Time               `json:"as_of"`
	Window  *FinanceOverviewWindow  `json:"window"`
	Metrics []FinanceOverviewMetric `json:"metrics"`
}

type financeAccountsOverviewValues struct {
	WalletCount            int64
	PostedMicrocredits     int64
	ReservedMicrocredits   int64
	AvailableMicrocredits  int64
	ReceivableMicrocredits int64
	PendingAdjustmentCount int64
	AtRiskHoldCount        int64
}

type financePaymentsOverviewValues struct {
	OrdersCreatedCount      int64
	PaymentsReceivedFen     int64
	LateReviewOpenCount     int64
	PaymentAnomalyOpenCount int64
	ReconciliationOpenCount int64
	RefundAttentionCount    int64
	ProviderControlledCount int64
	ProductControlledCount  int64
}

type financePricingOverviewValues struct {
	PriceDraftCount            int64
	PriceFutureCount           int64
	PriceActiveCount           int64
	PriceEndedOrCancelledCount int64
	MeteringActiveCount        int64
	MeteringInactiveCount      int64
}

type financeProductsOverviewValues struct {
	ProductDraftCount     int64
	ProductOnSaleCount    int64
	ProductOffSaleCount   int64
	ProductDeletedCount   int64
	AdmissionPendingCount int64
	AdmissionBlockedCount int64
}

type financeOverviewRepository interface {
	AdminFinanceAccountsOverview(context.Context, time.Time) (financeAccountsOverviewValues, error)
	AdminFinancePaymentsOverview(context.Context, time.Time, time.Time, time.Time) (financePaymentsOverviewValues, error)
	AdminFinancePricingOverview(context.Context, time.Time) (financePricingOverviewValues, error)
	AdminFinanceProductsOverview(context.Context, time.Time) (financeProductsOverviewValues, error)
}

func (s *Service) requireFinanceOverviewRepository(w http.ResponseWriter, r *http.Request) (financeOverviewRepository, bool) {
	repository, ok := s.repository.(financeOverviewRepository)
	if !ok {
		writeFinanceOverviewUnavailable(w, r)
	}
	return repository, ok
}

func writeFinanceOverviewUnavailable(w http.ResponseWriter, r *http.Request) {
	api.WriteError(w, http.StatusServiceUnavailable, "FINANCE_OVERVIEW_UNAVAILABLE", "finance overview is temporarily unavailable", httpserver.RequestID(r.Context()))
}

func rejectFinanceOverviewQuery(w http.ResponseWriter, r *http.Request) bool {
	if len(r.URL.Query()) == 0 {
		return false
	}
	api.WriteError(w, http.StatusBadRequest, "INVALID_TIME_WINDOW", "this finance overview does not accept a time window", httpserver.RequestID(r.Context()))
	return true
}

func parseFinancePaymentsWindow(query map[string][]string, now time.Time) (FinanceOverviewWindow, error) {
	now = now.UTC()
	for key := range query {
		if key != "from" && key != "to" {
			return FinanceOverviewWindow{}, ErrConflict
		}
	}
	fromValues, toValues := query["from"], query["to"]
	if len(fromValues) == 0 && len(toValues) == 0 {
		return FinanceOverviewWindow{From: now.Add(-24 * time.Hour), To: now, Timezone: "UTC"}, nil
	}
	if len(fromValues) != 1 || len(toValues) != 1 || strings.TrimSpace(fromValues[0]) == "" || strings.TrimSpace(toValues[0]) == "" {
		return FinanceOverviewWindow{}, ErrConflict
	}
	from, fromErr := time.Parse(time.RFC3339, fromValues[0])
	to, toErr := time.Parse(time.RFC3339, toValues[0])
	if fromErr != nil || toErr != nil {
		return FinanceOverviewWindow{}, ErrConflict
	}
	_, fromOffset := from.Zone()
	_, toOffset := to.Zone()
	if fromOffset != 0 || toOffset != 0 || !to.After(from) || to.Sub(from) > financeOverviewMaxWindow {
		return FinanceOverviewWindow{}, ErrConflict
	}
	return FinanceOverviewWindow{From: from.UTC(), To: to.UTC(), Timezone: "UTC"}, nil
}

func (s *Service) adminFinanceAccountsOverview(w http.ResponseWriter, r *http.Request) {
	if rejectFinanceOverviewQuery(w, r) {
		return
	}
	repository, ok := s.requireFinanceOverviewRepository(w, r)
	if !ok {
		return
	}
	asOf := s.now().UTC()
	values, err := repository.AdminFinanceAccountsOverview(r.Context(), asOf)
	if err != nil {
		writeFinanceOverviewUnavailable(w, r)
		return
	}
	allWalletStatuses := []string{"ACTIVE", "FROZEN", "CLOSED"}
	api.WriteJSON(w, http.StatusOK, FinanceOverview{AsOf: asOf, Window: nil, Metrics: []FinanceOverviewMetric{
		{Key: "wallet_count", Value: values.WalletCount, Unit: "COUNT", IncludedStatuses: allWalletStatuses},
		{Key: "posted_microcredits", Value: values.PostedMicrocredits, Unit: "MICROCREDITS", IncludedStatuses: allWalletStatuses},
		{Key: "reserved_microcredits", Value: values.ReservedMicrocredits, Unit: "MICROCREDITS", IncludedStatuses: allWalletStatuses},
		{Key: "available_microcredits", Value: values.AvailableMicrocredits, Unit: "MICROCREDITS", IncludedStatuses: allWalletStatuses},
		{Key: "receivable_microcredits", Value: values.ReceivableMicrocredits, Unit: "MICROCREDITS", IncludedStatuses: allWalletStatuses},
		{Key: "pending_adjustment_count", Value: values.PendingAdjustmentCount, Unit: "COUNT", IncludedStatuses: []string{"PENDING_APPROVAL"}},
		{Key: "at_risk_hold_count", Value: values.AtRiskHoldCount, Unit: "COUNT", IncludedStatuses: []string{"ACTIVE", "REVIEW"}},
	}})
}

func (s *Service) adminFinancePaymentsOverview(w http.ResponseWriter, r *http.Request) {
	asOf := s.now().UTC()
	window, err := parseFinancePaymentsWindow(r.URL.Query(), asOf)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_TIME_WINDOW", "from and to must form a UTC half-open interval of at most 31 days", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.requireFinanceOverviewRepository(w, r)
	if !ok {
		return
	}
	values, err := repository.AdminFinancePaymentsOverview(r.Context(), window.From, window.To, asOf)
	if err != nil {
		writeFinanceOverviewUnavailable(w, r)
		return
	}
	api.WriteJSON(w, http.StatusOK, FinanceOverview{AsOf: asOf, Window: &window, Metrics: []FinanceOverviewMetric{
		{Key: "orders_created_count", Value: values.OrdersCreatedCount, Unit: "COUNT", IncludedStatuses: []string{"CREATED", "PENDING", "PAID", "PAID_LATE", "FAILED", "CLOSED", "EXPIRED", "MANUAL_REVIEW"}},
		{Key: "payments_received_fen", Value: values.PaymentsReceivedFen, Unit: "CNY_FEN", IncludedStatuses: []string{"PAID", "LATE_PAID"}},
		{Key: "late_review_open_count", Value: values.LateReviewOpenCount, Unit: "COUNT", IncludedStatuses: []string{"REVIEW_REQUIRED_LATE_PAYMENT", "PROVIDER_BLIND"}},
		{Key: "payment_anomaly_open_count", Value: values.PaymentAnomalyOpenCount, Unit: "COUNT", IncludedStatuses: []string{"OPEN"}},
		{Key: "reconciliation_open_count", Value: values.ReconciliationOpenCount, Unit: "COUNT", IncludedStatuses: []string{"OPEN", "IN_REVIEW"}},
		{Key: "refund_attention_count", Value: values.RefundAttentionCount, Unit: "COUNT", IncludedStatuses: []string{"FAILED", "MANUAL_REVIEW"}},
		{Key: "provider_controlled_count", Value: values.ProviderControlledCount, Unit: "COUNT", IncludedStatuses: []string{"NEW_ORDERS_DISABLED"}},
		{Key: "product_controlled_count", Value: values.ProductControlledCount, Unit: "COUNT", IncludedStatuses: []string{"NEW_ORDERS_DISABLED"}},
	}})
}

func (s *Service) adminFinancePricingOverview(w http.ResponseWriter, r *http.Request) {
	if rejectFinanceOverviewQuery(w, r) {
		return
	}
	repository, ok := s.requireFinanceOverviewRepository(w, r)
	if !ok {
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

func (s *Service) adminFinanceProductsOverview(w http.ResponseWriter, r *http.Request) {
	if rejectFinanceOverviewQuery(w, r) {
		return
	}
	repository, ok := s.requireFinanceOverviewRepository(w, r)
	if !ok {
		return
	}
	asOf := s.now().UTC()
	values, err := repository.AdminFinanceProductsOverview(r.Context(), asOf)
	if err != nil {
		writeFinanceOverviewUnavailable(w, r)
		return
	}
	api.WriteJSON(w, http.StatusOK, FinanceOverview{AsOf: asOf, Window: nil, Metrics: []FinanceOverviewMetric{
		{Key: "product_draft_count", Value: values.ProductDraftCount, Unit: "COUNT", IncludedStatuses: []string{"DRAFT"}},
		{Key: "product_on_sale_count", Value: values.ProductOnSaleCount, Unit: "COUNT", IncludedStatuses: []string{"ON_SALE"}},
		{Key: "product_off_sale_count", Value: values.ProductOffSaleCount, Unit: "COUNT", IncludedStatuses: []string{"OFF_SALE"}},
		{Key: "product_deleted_count", Value: values.ProductDeletedCount, Unit: "COUNT", IncludedStatuses: []string{"DELETED"}},
		{Key: "admission_pending_count", Value: values.AdmissionPendingCount, Unit: "COUNT", IncludedStatuses: []string{"PENDING"}},
		{Key: "admission_blocked_count", Value: values.AdmissionBlockedCount, Unit: "COUNT", IncludedStatuses: []string{"MISSING", "FAILED", "EXPIRED", "REVOKED"}},
	}})
}

func (r *PostgresRepository) AdminFinanceAccountsOverview(ctx context.Context, asOf time.Time) (financeAccountsOverviewValues, error) {
	var result financeAccountsOverviewValues
	err := r.pool.QueryRow(ctx, `SELECT
    (SELECT count(*) FROM wallets),
    (SELECT COALESCE(sum(posted_microcredits),0)::bigint FROM wallets),
    (SELECT COALESCE(sum(reserved_microcredits),0)::bigint FROM wallets),
    (SELECT COALESCE(sum(posted_microcredits-reserved_microcredits-receivable_microcredits),0)::bigint FROM wallets),
    (SELECT COALESCE(sum(receivable_microcredits),0)::bigint FROM wallets),
    (SELECT count(*) FROM manual_adjustments WHERE status='PENDING_APPROVAL'),
    (SELECT count(*) FROM usage_holds WHERE status='REVIEW' OR (status='ACTIVE' AND expires_at<=$1))`, asOf).Scan(
		&result.WalletCount, &result.PostedMicrocredits, &result.ReservedMicrocredits,
		&result.AvailableMicrocredits, &result.ReceivableMicrocredits,
		&result.PendingAdjustmentCount, &result.AtRiskHoldCount,
	)
	return result, err
}

func (r *PostgresRepository) AdminFinancePaymentsOverview(ctx context.Context, from, to, asOf time.Time) (financePaymentsOverviewValues, error) {
	var result financePaymentsOverviewValues
	err := r.pool.QueryRow(ctx, `SELECT
    (SELECT count(*) FROM payment_orders WHERE created_at>=$1 AND created_at<$2),
    (SELECT COALESCE(sum(amount_fen),0)::bigint FROM payment_transactions WHERE status IN ('PAID','LATE_PAID') AND COALESCE(provider_paid_at,verified_at)>=$1 AND COALESCE(provider_paid_at,verified_at)<$2),
    (SELECT count(*) FROM late_payment_review_cases WHERE review_status IN ('REVIEW_REQUIRED_LATE_PAYMENT','PROVIDER_BLIND')),
    (SELECT count(*) FROM payment_anomaly_cases WHERE status='OPEN'),
    (SELECT count(*) FROM reconciliation_cases WHERE status IN ('OPEN','IN_REVIEW')),
    (SELECT count(*) FROM refunds WHERE status IN ('FAILED','MANUAL_REVIEW')),
    (SELECT count(*) FROM payment_provider_controls WHERE NOT new_orders_enabled AND updated_at<=$3),
    (SELECT count(*) FROM payment_product_controls WHERE NOT new_orders_enabled AND updated_at<=$3)`, from, to, asOf).Scan(
		&result.OrdersCreatedCount, &result.PaymentsReceivedFen, &result.LateReviewOpenCount,
		&result.PaymentAnomalyOpenCount, &result.ReconciliationOpenCount,
		&result.RefundAttentionCount, &result.ProviderControlledCount, &result.ProductControlledCount,
	)
	return result, err
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

func (r *PostgresRepository) AdminFinanceProductsOverview(ctx context.Context, asOf time.Time) (financeProductsOverviewValues, error) {
	var result financeProductsOverviewValues
	err := r.pool.QueryRow(ctx, `WITH product_admission AS (
    SELECT item.lifecycle_status,
           latest.status AS latest_status,
           latest_evidence.evidence_id AS latest_evidence_id,
           latest_evidence.expires_at AS latest_expires_at,
           EXISTS(SELECT 1 FROM product_admission_revocations revocation WHERE revocation.evidence_id=latest_evidence.evidence_id) AS latest_revoked,
           current_binding.evidence_id AS current_evidence_id,
           current_evidence.expires_at AS current_expires_at,
           EXISTS(SELECT 1 FROM product_admission_revocations revocation WHERE revocation.evidence_id=current_binding.evidence_id) AS current_revoked
    FROM recharge_product_catalog_items item
    LEFT JOIN LATERAL (
        SELECT verification_id,status
        FROM product_admission_verifications verification
        WHERE verification.catalog_item_id=item.catalog_item_id
        ORDER BY requested_at DESC,verification_id DESC
        LIMIT 1
    ) latest ON TRUE
    LEFT JOIN product_admission_evidence latest_evidence ON latest_evidence.verification_id=latest.verification_id
    LEFT JOIN product_admission_bindings current_binding ON current_binding.catalog_item_id=item.catalog_item_id AND current_binding.effective_to IS NULL
    LEFT JOIN product_admission_evidence current_evidence ON current_evidence.evidence_id=current_binding.evidence_id
)
SELECT
    count(*) FILTER (WHERE lifecycle_status='DRAFT'),
    count(*) FILTER (WHERE lifecycle_status='ON_SALE'),
    count(*) FILTER (WHERE lifecycle_status='OFF_SALE'),
    count(*) FILTER (WHERE lifecycle_status='DELETED'),
    count(*) FILTER (WHERE lifecycle_status<>'DELETED' AND latest_status='PENDING'),
    count(*) FILTER (WHERE lifecycle_status<>'DELETED' AND COALESCE(latest_status,'')<>'PENDING' AND (
        latest_status IS NULL OR latest_status='FAILED' OR
        (lifecycle_status='ON_SALE' AND current_evidence_id IS NULL) OR
        (latest_status='VERIFIED' AND (
            latest_evidence_id IS NULL OR latest_expires_at<=$1 OR latest_revoked OR
            current_expires_at<=$1 OR current_revoked
        ))
    ))
FROM product_admission`, asOf).Scan(
		&result.ProductDraftCount, &result.ProductOnSaleCount, &result.ProductOffSaleCount,
		&result.ProductDeletedCount, &result.AdmissionPendingCount, &result.AdmissionBlockedCount,
	)
	return result, err
}
