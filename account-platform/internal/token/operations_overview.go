package token

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

const tokenOperationsOverviewMaxWindow = 31 * 24 * time.Hour

var errInvalidTokenOperationsOverviewWindow = errors.New("invalid Token operations overview window")

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
}

type tokenOperationsOverviewRepository interface {
	AdminTokenOperationsOverview(context.Context, *time.Time, *time.Time) (tokenOperationsOverviewValues, error)
}

func parseTokenOperationsOverviewWindow(query url.Values) (*time.Time, *time.Time, error) {
	for key := range query {
		if key != "from" && key != "to" {
			return nil, nil, errInvalidTokenOperationsOverviewWindow
		}
	}
	fromValues, toValues := query["from"], query["to"]
	if len(fromValues) == 0 && len(toValues) == 0 {
		return nil, nil, nil
	}
	if len(fromValues) != 1 || len(toValues) != 1 || fromValues[0] == "" || toValues[0] == "" {
		return nil, nil, errInvalidTokenOperationsOverviewWindow
	}
	from, fromErr := time.Parse(time.RFC3339, fromValues[0])
	to, toErr := time.Parse(time.RFC3339, toValues[0])
	_, fromOffset := from.Zone()
	_, toOffset := to.Zone()
	if fromErr != nil || toErr != nil || fromOffset != 0 || toOffset != 0 || !to.After(from) || to.Sub(from) > tokenOperationsOverviewMaxWindow {
		return nil, nil, errInvalidTokenOperationsOverviewWindow
	}
	from, to = from.UTC(), to.UTC()
	return &from, &to, nil
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
	from, to, err := parseTokenOperationsOverviewWindow(r.URL.Query())
	if err != nil {
		outcome = "invalid"
		api.WriteError(w, http.StatusBadRequest, "INVALID_TIME_WINDOW", "from and to must be a bounded UTC RFC3339 pair", httpserver.RequestID(r.Context()))
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
	values, err := repository.AdminTokenOperationsOverview(ctx, from, to)
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
	})
}

func (r *PostgresRepository) AdminTokenOperationsOverview(ctx context.Context, explicitFrom, explicitTo *time.Time) (tokenOperationsOverviewValues, error) {
	var result tokenOperationsOverviewValues
	err := r.pool.QueryRow(ctx, `WITH clock AS MATERIALIZED (
    SELECT transaction_timestamp() AS as_of
), bounds AS MATERIALIZED (
    SELECT COALESCE($1::timestamptz,as_of-interval '24 hours') AS window_from,
           COALESCE($2::timestamptz,as_of) AS window_to,as_of
      FROM clock
), current_catalog AS MATERIALIZED (
    SELECT max(version) AS version FROM model_catalog_versions
)
SELECT bounds.as_of,bounds.window_from,bounds.window_to,
    (SELECT count(*) FROM model_catalog_entries entry,current_catalog WHERE entry.catalog_version=current_catalog.version AND entry.lifecycle_status='PUBLISHED'),
    (SELECT count(*) FROM model_catalog_entries entry,current_catalog WHERE entry.catalog_version=current_catalog.version AND entry.lifecycle_status='DEPRECATED'),
    (SELECT count(*) FROM model_catalog_drafts),
    (SELECT count(*) FROM routing_policies route WHERE route.reference_status='RESOLVED'),
    (SELECT count(*) FROM routing_policies route WHERE route.reference_status='RESOLVED' AND route.health_status='HEALTHY' AND route.health_expires_at>bounds.as_of),
    (SELECT count(*) FROM routing_policies route WHERE route.reference_status='RESOLVED' AND NOT(route.health_status='HEALTHY' AND route.health_expires_at>bounds.as_of)),
    (SELECT count(*) FROM route_provisionings provisioning WHERE provisioning.status='PENDING'),
    (SELECT count(*) FROM route_provisionings provisioning WHERE provisioning.status IN ('FAILED','REPAIR_REQUIRED')),
    (SELECT count(*) FROM usage_requests request WHERE request.started_at>=bounds.window_from AND request.started_at<bounds.window_to),
    (SELECT count(*) FROM usage_requests request WHERE request.status IN ('SETTLEMENT_PENDING','REVIEW') AND request.reconciliation_stopped_at IS NULL),
    (SELECT count(*) FROM usage_requests request WHERE request.status IN ('SETTLEMENT_PENDING','REVIEW') AND request.reconciliation_stopped_at IS NOT NULL)
FROM bounds`, explicitFrom, explicitTo).Scan(
		&result.AsOf, &result.WindowFrom, &result.WindowTo,
		&result.PublishedModels, &result.DeprecatedModels, &result.ModelDrafts,
		&result.StableRoutes, &result.HealthyRoutes, &result.RouteAttention,
		&result.ProvisioningPending, &result.ProvisioningFailedOrRepair, &result.UsageRequests,
		&result.ReconciliationPending, &result.ReconciliationStopped,
	)
	return result, err
}
