package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

var errInvalidTokenOperationsOverviewWindow = errors.New("invalid Token operations overview window")

const tokenOperationsOverviewSQL = `WITH clock AS MATERIALIZED (
    SELECT transaction_timestamp() AS as_of
), bounds AS MATERIALIZED (
    SELECT as_of-make_interval(secs=>$1::int) AS window_from,
           as_of AS window_to,as_of
      FROM clock
), catalog_head AS MATERIALIZED (
    SELECT max(version) AS version FROM model_catalog_versions
), bucket_config AS MATERIALIZED (
    SELECT bounds.*,
           CASE WHEN window_to-window_from<=interval '48 hours' THEN 3600::bigint ELSE 86400::bigint END AS bucket_seconds
      FROM bounds
), buckets AS MATERIALIZED (
    SELECT bucket_config.bucket_seconds,
           bucket_config.window_from+(series.index*bucket_config.bucket_seconds)*interval '1 second' AS bucket_from,
           LEAST(bucket_config.window_to,bucket_config.window_from+((series.index+1)*bucket_config.bucket_seconds)*interval '1 second') AS bucket_to
      FROM bucket_config
      CROSS JOIN LATERAL generate_series(0,CEIL(EXTRACT(EPOCH FROM (window_to-window_from))/bucket_seconds)::bigint-1) series(index)
), usage_trend AS MATERIALIZED (
    SELECT date_bin(make_interval(secs=>bucket_config.bucket_seconds),request.started_at,bucket_config.window_from) bucket_from,count(*) usage_count
      FROM bucket_config JOIN usage_requests request ON request.started_at>=bucket_config.window_from AND request.started_at<bucket_config.window_to
     GROUP BY bucket_config.bucket_seconds,bucket_config.window_from,bucket_from
), usage_window AS MATERIALIZED (
    SELECT COALESCE(sum(usage_count),0)::bigint AS usage_count FROM usage_trend
), reconciliation_point AS MATERIALIZED (
    SELECT count(*) FILTER (WHERE request.reconciliation_stopped_at IS NULL)::bigint AS pending_count,
           count(*) FILTER (WHERE request.reconciliation_stopped_at IS NOT NULL)::bigint AS stopped_count
      FROM usage_requests request
     WHERE request.status IN ('SETTLEMENT_PENDING','REVIEW')
), reconciliation_trend AS MATERIALIZED (
    SELECT date_bin(make_interval(secs=>bucket_config.bucket_seconds),review.created_at,bucket_config.window_from) bucket_from,count(*) reconciliation_attention_count
      FROM bucket_config JOIN manual_review_cases review ON review.created_at>=bucket_config.window_from AND review.created_at<bucket_config.window_to
     WHERE review.domain='TOKEN' AND review.case_type='USAGE_RECONCILIATION_REVIEW' AND review.resource_type='USAGE_REQUEST'
       AND EXISTS(SELECT 1 FROM usage_requests request WHERE request.id=review.resource_id)
     GROUP BY bucket_config.bucket_seconds,bucket_config.window_from,bucket_from
), trend AS MATERIALIZED (
    SELECT buckets.bucket_seconds,buckets.bucket_from,buckets.bucket_to,
           COALESCE(usage_trend.usage_count,0) usage_count,
           COALESCE(reconciliation_trend.reconciliation_attention_count,0) reconciliation_attention_count
      FROM buckets
      LEFT JOIN usage_trend USING(bucket_from)
      LEFT JOIN reconciliation_trend USING(bucket_from)
)
SELECT bounds.as_of,bounds.window_from,bounds.window_to,
    (SELECT count(*) FROM model_catalog_entries entry,catalog_head WHERE entry.catalog_version=catalog_head.version AND entry.lifecycle_status='PUBLISHED'),
    (SELECT count(*) FROM model_catalog_entries entry,catalog_head WHERE entry.catalog_version=catalog_head.version AND entry.lifecycle_status='DEPRECATED'),
    (SELECT count(*) FROM model_catalog_drafts),
    (SELECT count(*) FROM routing_policies route WHERE route.reference_status='RESOLVED'),
    (SELECT count(*) FROM routing_policies route WHERE route.reference_status='RESOLVED' AND route.health_status='HEALTHY' AND route.health_expires_at>bounds.as_of),
    (SELECT count(*) FROM routing_policies route WHERE route.reference_status='RESOLVED' AND NOT COALESCE(route.health_status='HEALTHY' AND route.health_expires_at>bounds.as_of,false)),
    (SELECT count(*) FROM route_provisionings provisioning WHERE provisioning.status='PENDING'),
    (SELECT count(*) FROM route_provisionings provisioning WHERE provisioning.status IN ('FAILED','REPAIR_REQUIRED')),
    (SELECT usage_count FROM usage_window),
    (SELECT pending_count FROM reconciliation_point),
    (SELECT stopped_count FROM reconciliation_point),
    (SELECT bucket_seconds FROM bucket_config),
    COALESCE((SELECT jsonb_agg(jsonb_build_object('from',bucket_from,'to',bucket_to,'usage_request_count',usage_count,'reconciliation_attention_count',reconciliation_attention_count) ORDER BY bucket_from) FROM trend),'[]'::jsonb)
FROM bounds`

type TokenOperationsOverviewWindow struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Timezone string    `json:"timezone"`
}

type TokenOperationsOverviewMetric struct {
	Key              string   `json:"key"`
	Value            int64    `json:"value"`
	Unit             string   `json:"unit"`
	Aggregation      string   `json:"aggregation"`
	IncludedStatuses []string `json:"included_statuses"`
	DenominatorKey   string   `json:"denominator_key,omitempty"`
}

type TokenOperationsOverview struct {
	AsOf    time.Time                       `json:"as_of"`
	Window  TokenOperationsOverviewWindow   `json:"window"`
	Metrics []TokenOperationsOverviewMetric `json:"metrics"`
	Trend   TokenOperationsOverviewTrend    `json:"trend"`
}

type TokenOperationsOverviewTrendPoint struct {
	From                    time.Time `json:"from"`
	To                      time.Time `json:"to"`
	UsageRequests           int64     `json:"usage_request_count"`
	ReconciliationAttention int64     `json:"reconciliation_attention_count"`
}

type TokenOperationsOverviewTrend struct {
	BucketSeconds int64                               `json:"bucket_seconds"`
	Points        []TokenOperationsOverviewTrendPoint `json:"points"`
}

type tokenOperationsOverviewValues struct {
	AsOf                       time.Time
	WindowFrom                 time.Time
	WindowTo                   time.Time
	PublishedModels            int64
	DeprecatedModels           int64
	ModelDrafts                int64
	StableRoutes               int64
	HealthyRoutes              int64
	RouteAttention             int64
	ProvisioningPending        int64
	ProvisioningFailedOrRepair int64
	UsageRequests              int64
	ReconciliationPending      int64
	ReconciliationStopped      int64
	TrendBucketSeconds         int64
	TrendPoints                []TokenOperationsOverviewTrendPoint
}

type tokenOperationsOverviewRepository interface {
	AdminTokenOperationsOverview(context.Context, time.Duration) (tokenOperationsOverviewValues, error)
}

func parseTokenOperationsOverviewWindow(query url.Values) (time.Duration, error) {
	for key := range query {
		if key != "window" {
			return 0, errInvalidTokenOperationsOverviewWindow
		}
	}
	values := query["window"]
	if len(values) != 1 {
		return 0, errInvalidTokenOperationsOverviewWindow
	}
	switch values[0] {
	case "24h":
		return 24 * time.Hour, nil
	case "7d":
		return 7 * 24 * time.Hour, nil
	case "30d":
		return 30 * 24 * time.Hour, nil
	default:
		return 0, errInvalidTokenOperationsOverviewWindow
	}
}

func (s *Service) adminTokenOperationsOverview(w http.ResponseWriter, r *http.Request) {
	startedAt, outcome := time.Now(), "success"
	defer func() {
		if s.operationsOverviewMetrics != nil {
			s.operationsOverviewMetrics.requests.WithLabelValues(outcome).Inc()
			s.operationsOverviewMetrics.duration.WithLabelValues(outcome).Observe(time.Since(startedAt).Seconds())
		}
	}()
	if !s.operationsOverviewEnabled {
		outcome = "disabled"
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_OPERATIONS_OVERVIEW_DISABLED", "the Token operations overview is not enabled", httpserver.RequestID(r.Context()))
		return
	}
	window, err := parseTokenOperationsOverviewWindow(r.URL.Query())
	if err != nil {
		outcome = "invalid"
		api.WriteError(w, http.StatusBadRequest, "INVALID_TIME_WINDOW", "window must be one of 24h, 7d, or 30d", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(tokenOperationsOverviewRepository)
	if !ok {
		outcome = "unavailable"
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_OPERATIONS_OVERVIEW_UNAVAILABLE", "the Token operations overview is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	values, err := repository.AdminTokenOperationsOverview(ctx, window)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			outcome = "timeout"
			api.WriteError(w, http.StatusGatewayTimeout, "TOKEN_OPERATIONS_OVERVIEW_TIMEOUT", "the Token operations overview timed out", httpserver.RequestID(r.Context()))
			return
		}
		outcome = "unavailable"
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_OPERATIONS_OVERVIEW_UNAVAILABLE", "the Token operations overview is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, TokenOperationsOverview{
		AsOf:   values.AsOf.UTC(),
		Window: TokenOperationsOverviewWindow{From: values.WindowFrom.UTC(), To: values.WindowTo.UTC(), Timezone: "UTC"},
		Metrics: []TokenOperationsOverviewMetric{
			{Key: "published_model_count", Value: values.PublishedModels, Unit: "COUNT", Aggregation: "POINT_IN_TIME", IncludedStatuses: []string{"PUBLISHED"}},
			{Key: "deprecated_model_count", Value: values.DeprecatedModels, Unit: "COUNT", Aggregation: "POINT_IN_TIME", IncludedStatuses: []string{"DEPRECATED"}},
			{Key: "model_draft_count", Value: values.ModelDrafts, Unit: "COUNT", Aggregation: "POINT_IN_TIME", IncludedStatuses: []string{"DRAFT"}},
			{Key: "stable_route_count", Value: values.StableRoutes, Unit: "COUNT", Aggregation: "POINT_IN_TIME", IncludedStatuses: []string{"RESOLVED"}},
			{Key: "healthy_route_count", Value: values.HealthyRoutes, Unit: "COUNT", Aggregation: "POINT_IN_TIME", IncludedStatuses: []string{"HEALTHY"}, DenominatorKey: "stable_route_count"},
			{Key: "route_attention_count", Value: values.RouteAttention, Unit: "COUNT", Aggregation: "POINT_IN_TIME", IncludedStatuses: []string{"UNKNOWN", "PROBING", "DEGRADED", "UNHEALTHY", "STALE", "EXPIRED_HEALTH"}, DenominatorKey: "stable_route_count"},
			{Key: "provisioning_pending_count", Value: values.ProvisioningPending, Unit: "COUNT", Aggregation: "POINT_IN_TIME", IncludedStatuses: []string{"PENDING"}},
			{Key: "provisioning_attention_count", Value: values.ProvisioningFailedOrRepair, Unit: "COUNT", Aggregation: "POINT_IN_TIME", IncludedStatuses: []string{"FAILED", "REPAIR_REQUIRED"}},
			{Key: "usage_request_count", Value: values.UsageRequests, Unit: "COUNT", Aggregation: "WINDOW", IncludedStatuses: []string{"CREATED", "RESERVED", "UPSTREAM_ACTIVE", "SETTLEMENT_PENDING", "COMMITTED", "RELEASED", "REVIEW"}},
			{Key: "reconciliation_pending_count", Value: values.ReconciliationPending, Unit: "COUNT", Aggregation: "POINT_IN_TIME", IncludedStatuses: []string{"SETTLEMENT_PENDING", "REVIEW"}},
			{Key: "reconciliation_stopped_count", Value: values.ReconciliationStopped, Unit: "COUNT", Aggregation: "POINT_IN_TIME", IncludedStatuses: []string{"SETTLEMENT_PENDING", "REVIEW"}},
		},
		Trend: TokenOperationsOverviewTrend{BucketSeconds: values.TrendBucketSeconds, Points: values.TrendPoints},
	})
}

func (r *PostgresRepository) AdminTokenOperationsOverview(ctx context.Context, window time.Duration) (tokenOperationsOverviewValues, error) {
	var result tokenOperationsOverviewValues
	var trendJSON []byte
	if window != 24*time.Hour && window != 7*24*time.Hour && window != 30*24*time.Hour {
		return result, errInvalidTokenOperationsOverviewWindow
	}
	err := r.pool.QueryRow(ctx, tokenOperationsOverviewSQL, int64(window/time.Second)).Scan(
		&result.AsOf, &result.WindowFrom, &result.WindowTo,
		&result.PublishedModels, &result.DeprecatedModels, &result.ModelDrafts,
		&result.StableRoutes, &result.HealthyRoutes, &result.RouteAttention,
		&result.ProvisioningPending, &result.ProvisioningFailedOrRepair, &result.UsageRequests,
		&result.ReconciliationPending, &result.ReconciliationStopped, &result.TrendBucketSeconds, &trendJSON,
	)
	if err == nil {
		err = json.Unmarshal(trendJSON, &result.TrendPoints)
	}
	return result, err
}
