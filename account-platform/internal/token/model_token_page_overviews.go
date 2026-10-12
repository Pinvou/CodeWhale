package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	modelCatalogOverviewKind    = "model-catalog"
	tokenResourcesOverviewKind  = "token-resources"
	modelOperationsOverviewKind = "model-operations"
)

const adminModelCatalogOverviewSQL = `WITH clock AS MATERIALIZED (
    SELECT transaction_timestamp() AS as_of
), catalog_head AS MATERIALIZED (
    SELECT max(version) AS version FROM model_catalog_versions
), entry_counts AS MATERIALIZED (
    SELECT count(*) FILTER (WHERE entry.lifecycle_status='PUBLISHED')::bigint AS published_count,
           count(*) FILTER (WHERE entry.lifecycle_status='DEPRECATED')::bigint AS deprecated_count
      FROM model_catalog_entries entry
      JOIN catalog_head head ON entry.catalog_version=head.version
), draft_counts AS MATERIALIZED (
    SELECT count(*)::bigint AS draft_count FROM model_catalog_drafts
)
SELECT clock.as_of,entry_counts.published_count,entry_counts.deprecated_count,draft_counts.draft_count
  FROM clock CROSS JOIN entry_counts CROSS JOIN draft_counts`

const adminTokenResourcesOverviewSQL = `WITH clock AS MATERIALIZED (
    SELECT transaction_timestamp() AS as_of
), route_counts AS MATERIALIZED (
    SELECT count(*)::bigint AS stable_count,
           count(*) FILTER (WHERE route.health_status='HEALTHY' AND route.health_expires_at>clock.as_of)::bigint AS healthy_count,
           count(*) FILTER (WHERE NOT COALESCE(route.health_status='HEALTHY' AND route.health_expires_at>clock.as_of,false))::bigint AS attention_count
      FROM routing_policies route CROSS JOIN clock
     WHERE route.reference_status='RESOLVED'
)
SELECT clock.as_of,route_counts.stable_count,route_counts.healthy_count,route_counts.attention_count
  FROM clock CROSS JOIN route_counts`

const adminModelOperationsTokenOnlySQL = `WITH clock AS MATERIALIZED (
    SELECT transaction_timestamp() AS as_of
), provisioning_counts AS MATERIALIZED (
    SELECT count(*) FILTER (WHERE provisioning.status='PENDING')::bigint AS pending_count,
           count(*) FILTER (WHERE provisioning.status IN ('FAILED','REPAIR_REQUIRED'))::bigint AS attention_count
      FROM route_provisionings provisioning
)
SELECT clock.as_of,provisioning_counts.pending_count,provisioning_counts.attention_count
  FROM clock CROSS JOIN provisioning_counts`

const adminModelOperationsUsageCTEs = `clock AS MATERIALIZED (
    SELECT transaction_timestamp() AS as_of
), bounds AS MATERIALIZED (
    SELECT as_of-make_interval(secs=>$1::int) AS window_from,
           as_of AS window_to,as_of
      FROM clock
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
    SELECT date_bin(make_interval(secs=>bucket_config.bucket_seconds),request.started_at,bucket_config.window_from) bucket_from,count(*)::bigint usage_count
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
    SELECT date_bin(make_interval(secs=>bucket_config.bucket_seconds),review.created_at,bucket_config.window_from) bucket_from,count(*)::bigint reconciliation_attention_count
      FROM bucket_config JOIN manual_review_cases review ON review.created_at>=bucket_config.window_from AND review.created_at<bucket_config.window_to
     WHERE review.domain='TOKEN' AND review.case_type='USAGE_RECONCILIATION_REVIEW' AND review.resource_type='USAGE_REQUEST'
       AND EXISTS(SELECT 1 FROM usage_requests request WHERE request.id=review.resource_id)
     GROUP BY bucket_config.bucket_seconds,bucket_config.window_from,bucket_from
), trend AS MATERIALIZED (
    SELECT buckets.bucket_seconds,buckets.bucket_from,buckets.bucket_to,
           COALESCE(usage_trend.usage_count,0)::bigint usage_count,
           COALESCE(reconciliation_trend.reconciliation_attention_count,0)::bigint reconciliation_attention_count
      FROM buckets
      LEFT JOIN usage_trend USING(bucket_from)
      LEFT JOIN reconciliation_trend USING(bucket_from)
)`

const adminModelOperationsUsageOnlySQL = `WITH ` + adminModelOperationsUsageCTEs + `
SELECT bounds.as_of,bounds.window_from,bounds.window_to,
       (SELECT usage_count FROM usage_window),
       (SELECT pending_count FROM reconciliation_point),
       (SELECT stopped_count FROM reconciliation_point),
       (SELECT bucket_seconds FROM bucket_config),
       COALESCE((SELECT jsonb_agg(jsonb_build_object('from',bucket_from,'to',bucket_to,'usage_request_count',usage_count,'reconciliation_attention_count',reconciliation_attention_count) ORDER BY bucket_from) FROM trend),'[]'::jsonb)
  FROM bounds`

const adminModelOperationsBothSQL = `WITH ` + adminModelOperationsUsageCTEs + `, provisioning_counts AS MATERIALIZED (
    SELECT count(*) FILTER (WHERE provisioning.status='PENDING')::bigint AS pending_count,
           count(*) FILTER (WHERE provisioning.status IN ('FAILED','REPAIR_REQUIRED'))::bigint AS attention_count
      FROM route_provisionings provisioning
)
SELECT bounds.as_of,
       (SELECT pending_count FROM provisioning_counts),
       (SELECT attention_count FROM provisioning_counts),
       bounds.window_from,bounds.window_to,
       (SELECT usage_count FROM usage_window),
       (SELECT pending_count FROM reconciliation_point),
       (SELECT stopped_count FROM reconciliation_point),
       (SELECT bucket_seconds FROM bucket_config),
       COALESCE((SELECT jsonb_agg(jsonb_build_object('from',bucket_from,'to',bucket_to,'usage_request_count',usage_count,'reconciliation_attention_count',reconciliation_attention_count) ORDER BY bucket_from) FROM trend),'[]'::jsonb)
  FROM bounds`

type ModelTokenPageOverviewMetric struct {
	Key              string   `json:"key"`
	Value            int64    `json:"value"`
	Unit             string   `json:"unit"`
	Aggregation      string   `json:"aggregation"`
	IncludedStatuses []string `json:"included_statuses"`
	DenominatorKey   *string  `json:"denominator_key"`
}

type ModelCatalogPageOverview struct {
	SchemaVersion string                         `json:"schema_version"`
	AsOf          time.Time                      `json:"as_of"`
	Metrics       []ModelTokenPageOverviewMetric `json:"metrics"`
}

type TokenResourcesPageOverview struct {
	SchemaVersion string                         `json:"schema_version"`
	AsOf          time.Time                      `json:"as_of"`
	Metrics       []ModelTokenPageOverviewMetric `json:"metrics"`
}

type ModelOperationsProvisioningOverview struct {
	Metrics []ModelTokenPageOverviewMetric `json:"metrics"`
}

type ModelOperationsUsageOverview struct {
	Window  TokenOperationsOverviewWindow  `json:"window"`
	Metrics []ModelTokenPageOverviewMetric `json:"metrics"`
	Trend   TokenOperationsOverviewTrend   `json:"trend"`
}

type ModelOperationsPageOverview struct {
	SchemaVersion  string                               `json:"schema_version"`
	AsOf           time.Time                            `json:"as_of"`
	PermissionView string                               `json:"permission_view"`
	Provisioning   *ModelOperationsProvisioningOverview `json:"provisioning,omitempty"`
	Usage          *ModelOperationsUsageOverview        `json:"usage,omitempty"`
}

type modelTokenPageOverviewValues struct {
	AsOf                       time.Time
	PublishedModels            int64
	DeprecatedModels           int64
	ModelDrafts                int64
	StableRoutes               int64
	HealthyRoutes              int64
	RouteAttention             int64
	ProvisioningPending        int64
	ProvisioningFailedOrRepair int64
	WindowFrom                 time.Time
	WindowTo                   time.Time
	UsageRequests              int64
	ReconciliationPending      int64
	ReconciliationStopped      int64
	TrendBucketSeconds         int64
	TrendPoints                []TokenOperationsOverviewTrendPoint
}

type modelTokenPageOverviewRepository interface {
	reserveAdminOverviewRate(context.Context, uuid.UUID, string, string) error
	AdminModelCatalogOverview(context.Context) (modelTokenPageOverviewValues, error)
	AdminTokenResourcesOverview(context.Context) (modelTokenPageOverviewValues, error)
	AdminModelOperationsOverview(context.Context, time.Duration, bool, bool) (modelTokenPageOverviewValues, error)
}

func parseNoOverviewQuery(rawQuery string) error {
	if rawQuery == "" {
		return nil
	}
	if _, err := url.ParseQuery(rawQuery); err != nil {
		return err
	}
	return errors.New("overview query is not allowed")
}

func parseModelOperationsOverviewWindow(rawQuery string, usageAllowed bool) (time.Duration, error) {
	if !usageAllowed {
		if rawQuery == "" {
			return 0, nil
		}
		return 0, errInvalidTokenOperationsOverviewWindow
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil || len(query) != 1 {
		return 0, errInvalidTokenOperationsOverviewWindow
	}
	return parseTokenOperationsOverviewWindow(query)
}

func (s *Service) adminModelCatalogOverview(w http.ResponseWriter, r *http.Request) {
	if err := parseNoOverviewQuery(r.URL.RawQuery); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_MODEL_CATALOG_OVERVIEW_QUERY", "the model catalog overview does not accept query parameters", httpserver.RequestID(r.Context()))
		return
	}
	s.serveModelTokenPageOverview(w, r, modelCatalogOverviewKind, 0, true, false)
}

func (s *Service) adminTokenResourcesOverview(w http.ResponseWriter, r *http.Request) {
	if err := parseNoOverviewQuery(r.URL.RawQuery); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_TOKEN_RESOURCES_OVERVIEW_QUERY", "the Token resources overview does not accept query parameters", httpserver.RequestID(r.Context()))
		return
	}
	s.serveModelTokenPageOverview(w, r, tokenResourcesOverviewKind, 0, true, false)
}

func (s *Service) adminModelOperationsPageOverview(w http.ResponseWriter, r *http.Request) {
	tokenAllowed := authorization.GrantedPermission(r.Context(), "platform.tokens.read")
	usageAllowed := authorization.GrantedPermission(r.Context(), "platform.usage.read")
	if !tokenAllowed && !usageAllowed {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "none of the requested permissions is granted", httpserver.RequestID(r.Context()))
		return
	}
	window, err := parseModelOperationsOverviewWindow(r.URL.RawQuery, usageAllowed)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_TIME_WINDOW", "window must be exactly one of 24h, 7d, or 30d when usage is visible", httpserver.RequestID(r.Context()))
		return
	}
	s.serveModelTokenPageOverview(w, r, modelOperationsOverviewKind, window, tokenAllowed, usageAllowed)
}

func (s *Service) serveModelTokenPageOverview(w http.ResponseWriter, r *http.Request, kind string, window time.Duration, tokenAllowed, usageAllowed bool) {
	if !s.pageOverviewsEnabled {
		writeModelTokenPageOverviewUnavailable(w, r, kind)
		return
	}
	repository, ok := s.repository.(modelTokenPageOverviewRepository)
	if !ok {
		writeModelTokenPageOverviewUnavailable(w, r, kind)
		return
	}
	source, ok := s.verifiedAdminQuerySource(r)
	if !ok {
		writeModelTokenPageOverviewUnavailable(w, r, kind)
		return
	}
	if err := repository.reserveAdminOverviewRate(r.Context(), adminSubject(r), kind, source); err != nil {
		var limited adminQueryRateLimitError
		if errors.As(err, &limited) {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, min(60, limited.RetryAfter))))
			api.WriteError(w, http.StatusTooManyRequests, "TOKEN_OVERVIEW_RATE_LIMITED", "the Token overview request rate limit was exceeded", httpserver.RequestID(r.Context()))
			return
		}
		if isModelTokenPageOverviewTimeout(err) {
			writeModelTokenPageOverviewTimeout(w, r, kind)
			return
		}
		writeModelTokenPageOverviewUnavailable(w, r, kind)
		return
	}
	var values modelTokenPageOverviewValues
	var err error
	switch kind {
	case modelCatalogOverviewKind:
		values, err = repository.AdminModelCatalogOverview(r.Context())
	case tokenResourcesOverviewKind:
		values, err = repository.AdminTokenResourcesOverview(r.Context())
	case modelOperationsOverviewKind:
		values, err = repository.AdminModelOperationsOverview(r.Context(), window, tokenAllowed, usageAllowed)
	default:
		err = errors.New("unknown model and Token overview")
	}
	if err != nil {
		if isModelTokenPageOverviewTimeout(err) {
			writeModelTokenPageOverviewTimeout(w, r, kind)
			return
		}
		writeModelTokenPageOverviewUnavailable(w, r, kind)
		return
	}
	if err := validateModelTokenPageOverviewValues(kind, values, window, tokenAllowed, usageAllowed); err != nil {
		writeModelTokenPageOverviewUnavailable(w, r, kind)
		return
	}
	writeModelTokenPageOverview(w, kind, values, tokenAllowed, usageAllowed)
}

func isModelTokenPageOverviewTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "57014"
}

func writeModelTokenPageOverview(w http.ResponseWriter, kind string, values modelTokenPageOverviewValues, tokenAllowed, usageAllowed bool) {
	metric := func(key string, value int64, aggregation string, statuses []string, denominator *string) ModelTokenPageOverviewMetric {
		return ModelTokenPageOverviewMetric{Key: key, Value: value, Unit: "COUNT", Aggregation: aggregation, IncludedStatuses: statuses, DenominatorKey: denominator}
	}
	stableRoutes := "stable_route_count"
	switch kind {
	case modelCatalogOverviewKind:
		api.WriteJSON(w, http.StatusOK, ModelCatalogPageOverview{SchemaVersion: "1", AsOf: values.AsOf.UTC(), Metrics: []ModelTokenPageOverviewMetric{
			metric("published_model_count", values.PublishedModels, "POINT_IN_TIME", []string{"PUBLISHED"}, nil),
			metric("deprecated_model_count", values.DeprecatedModels, "POINT_IN_TIME", []string{"DEPRECATED"}, nil),
			metric("model_draft_count", values.ModelDrafts, "POINT_IN_TIME", []string{"DRAFT"}, nil),
		}})
	case tokenResourcesOverviewKind:
		api.WriteJSON(w, http.StatusOK, TokenResourcesPageOverview{SchemaVersion: "1", AsOf: values.AsOf.UTC(), Metrics: []ModelTokenPageOverviewMetric{
			metric("stable_route_count", values.StableRoutes, "POINT_IN_TIME", []string{"RESOLVED"}, nil),
			metric("healthy_route_count", values.HealthyRoutes, "POINT_IN_TIME", []string{"HEALTHY"}, &stableRoutes),
			metric("route_attention_count", values.RouteAttention, "POINT_IN_TIME", []string{"UNKNOWN", "PROBING", "DEGRADED", "UNHEALTHY", "STALE", "EXPIRED_HEALTH"}, &stableRoutes),
		}})
	case modelOperationsOverviewKind:
		response := ModelOperationsPageOverview{SchemaVersion: "1", AsOf: values.AsOf.UTC()}
		if tokenAllowed {
			response.PermissionView = "TOKEN_ONLY"
			response.Provisioning = &ModelOperationsProvisioningOverview{Metrics: []ModelTokenPageOverviewMetric{
				metric("provisioning_pending_count", values.ProvisioningPending, "POINT_IN_TIME", []string{"PENDING"}, nil),
				metric("provisioning_attention_count", values.ProvisioningFailedOrRepair, "POINT_IN_TIME", []string{"FAILED", "REPAIR_REQUIRED"}, nil),
			}}
		}
		if usageAllowed {
			response.PermissionView = "USAGE_ONLY"
			response.Usage = &ModelOperationsUsageOverview{
				Window: TokenOperationsOverviewWindow{From: values.WindowFrom.UTC(), To: values.WindowTo.UTC(), Timezone: "UTC"},
				Metrics: []ModelTokenPageOverviewMetric{
					metric("usage_request_count", values.UsageRequests, "WINDOW", []string{"CREATED", "RESERVED", "UPSTREAM_ACTIVE", "SETTLEMENT_PENDING", "COMMITTED", "RELEASED", "REVIEW"}, nil),
					metric("reconciliation_pending_count", values.ReconciliationPending, "POINT_IN_TIME", []string{"SETTLEMENT_PENDING", "REVIEW"}, nil),
					metric("reconciliation_stopped_count", values.ReconciliationStopped, "POINT_IN_TIME", []string{"SETTLEMENT_PENDING", "REVIEW"}, nil),
				},
				Trend: TokenOperationsOverviewTrend{BucketSeconds: values.TrendBucketSeconds, Points: values.TrendPoints},
			}
		}
		if tokenAllowed && usageAllowed {
			response.PermissionView = "TOKEN_AND_USAGE"
		}
		api.WriteJSON(w, http.StatusOK, response)
	}
}

func validateModelTokenPageOverviewValues(kind string, values modelTokenPageOverviewValues, window time.Duration, tokenAllowed, usageAllowed bool) error {
	if values.AsOf.IsZero() {
		return errors.New("overview as_of is unavailable")
	}
	nonNegative := func(items ...int64) bool {
		for _, item := range items {
			if item < 0 {
				return false
			}
		}
		return true
	}
	switch kind {
	case modelCatalogOverviewKind:
		if !nonNegative(values.PublishedModels, values.DeprecatedModels, values.ModelDrafts) {
			return errors.New("invalid model catalog overview counts")
		}
	case tokenResourcesOverviewKind:
		if !nonNegative(values.StableRoutes, values.HealthyRoutes, values.RouteAttention) || values.HealthyRoutes+values.RouteAttention != values.StableRoutes {
			return errors.New("invalid Token resources overview counts")
		}
	case modelOperationsOverviewKind:
		if tokenAllowed && !nonNegative(values.ProvisioningPending, values.ProvisioningFailedOrRepair) {
			return errors.New("invalid provisioning overview counts")
		}
		if usageAllowed {
			if !nonNegative(values.UsageRequests, values.ReconciliationPending, values.ReconciliationStopped) || window <= 0 || !values.WindowTo.Equal(values.AsOf) || values.WindowTo.Sub(values.WindowFrom) != window {
				return errors.New("invalid usage overview")
			}
			expectedBucket, expectedPoints := int64(86400), int(window/(24*time.Hour))
			if window == 24*time.Hour {
				expectedBucket, expectedPoints = 3600, 24
			}
			if values.TrendBucketSeconds != expectedBucket || len(values.TrendPoints) != expectedPoints {
				return errors.New("invalid usage overview trend")
			}
			for index, point := range values.TrendPoints {
				if point.UsageRequests < 0 || point.ReconciliationAttention < 0 || !point.To.After(point.From) || index == 0 && !point.From.Equal(values.WindowFrom) || index > 0 && !point.From.Equal(values.TrendPoints[index-1].To) {
					return errors.New("invalid usage overview trend point")
				}
			}
			if !values.TrendPoints[len(values.TrendPoints)-1].To.Equal(values.WindowTo) {
				return errors.New("invalid usage overview trend boundary")
			}
		}
	default:
		return errors.New("unknown model and Token overview")
	}
	return nil
}

func writeModelTokenPageOverviewUnavailable(w http.ResponseWriter, r *http.Request, kind string) {
	code, message := modelTokenPageOverviewProblem(kind, "UNAVAILABLE")
	api.WriteError(w, http.StatusServiceUnavailable, code, message, httpserver.RequestID(r.Context()))
}

func writeModelTokenPageOverviewTimeout(w http.ResponseWriter, r *http.Request, kind string) {
	code, message := modelTokenPageOverviewProblem(kind, "TIMEOUT")
	api.WriteError(w, http.StatusGatewayTimeout, code, message, httpserver.RequestID(r.Context()))
}

func modelTokenPageOverviewProblem(kind, suffix string) (string, string) {
	prefix, label := "MODEL_OPERATIONS_OVERVIEW", "model operations overview"
	switch kind {
	case modelCatalogOverviewKind:
		prefix, label = "MODEL_CATALOG_OVERVIEW", "model catalog overview"
	case tokenResourcesOverviewKind:
		prefix, label = "TOKEN_RESOURCES_OVERVIEW", "Token resources overview"
	}
	if suffix == "TIMEOUT" {
		return prefix + "_TIMEOUT", "the " + label + " timed out"
	}
	return prefix + "_UNAVAILABLE", "the " + label + " is temporarily unavailable"
}

func (r *PostgresRepository) reserveAdminOverviewRate(ctx context.Context, subjectID uuid.UUID, endpoint, source string) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('token-admin-overview-rate'))`); err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	window := now.Truncate(time.Minute)
	if _, err = tx.Exec(ctx, `DELETE FROM token_admin_pagination_creation_windows WHERE window_start<$1`, window.Add(-2*time.Minute)); err != nil {
		return err
	}
	sourceDigest := sha256.Sum256([]byte(strings.TrimSpace(source)))
	limited := false
	for _, quota := range []struct {
		key   string
		limit int
	}{
		{key: "overview:subject:" + subjectID.String() + ":" + endpoint, limit: 60},
		{key: "overview:source:" + hex.EncodeToString(sourceDigest[:]), limit: 120},
	} {
		var count int
		if err = tx.QueryRow(ctx, `INSERT INTO token_admin_pagination_creation_windows(rate_key,window_start,creation_count) VALUES($1,$2,1)
ON CONFLICT(rate_key,window_start) DO UPDATE SET creation_count=token_admin_pagination_creation_windows.creation_count+1 RETURNING creation_count`, quota.key, window).Scan(&count); err != nil {
			return err
		}
		limited = limited || count > quota.limit
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if limited {
		return adminQueryRateLimitError{RetryAfter: adminOverviewRetryAfter(window.Add(time.Minute), now)}
	}
	return nil
}

func adminOverviewRetryAfter(windowEnd, now time.Time) int {
	return max(1, min(60, int(math.Ceil(windowEnd.Sub(now).Seconds()))))
}

func (r *PostgresRepository) withModelTokenPageOverviewTx(ctx context.Context, read func(pgx.Tx) error) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("model and Token overview deadline is unavailable")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	remaining = time.Until(deadline)
	if remaining <= 0 || ctx.Err() != nil {
		return context.DeadlineExceeded
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('statement_timeout',$1,true)`, strconv.FormatInt(max(1, remaining.Milliseconds()), 10)+"ms"); err != nil {
		return err
	}
	if err = read(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminModelCatalogOverview(ctx context.Context) (modelTokenPageOverviewValues, error) {
	var result modelTokenPageOverviewValues
	err := r.withModelTokenPageOverviewTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, adminModelCatalogOverviewSQL).Scan(&result.AsOf, &result.PublishedModels, &result.DeprecatedModels, &result.ModelDrafts)
	})
	return result, err
}

func (r *PostgresRepository) AdminTokenResourcesOverview(ctx context.Context) (modelTokenPageOverviewValues, error) {
	var result modelTokenPageOverviewValues
	err := r.withModelTokenPageOverviewTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, adminTokenResourcesOverviewSQL).Scan(&result.AsOf, &result.StableRoutes, &result.HealthyRoutes, &result.RouteAttention)
	})
	return result, err
}

func (r *PostgresRepository) AdminModelOperationsOverview(ctx context.Context, window time.Duration, tokenAllowed, usageAllowed bool) (modelTokenPageOverviewValues, error) {
	var result modelTokenPageOverviewValues
	if !tokenAllowed && !usageAllowed || usageAllowed && window != 24*time.Hour && window != 7*24*time.Hour && window != 30*24*time.Hour || !usageAllowed && window != 0 {
		return result, errors.New("invalid model operations overview permission view")
	}
	err := r.withModelTokenPageOverviewTx(ctx, func(tx pgx.Tx) error {
		var trendJSON []byte
		switch {
		case tokenAllowed && !usageAllowed:
			return tx.QueryRow(ctx, adminModelOperationsTokenOnlySQL).Scan(&result.AsOf, &result.ProvisioningPending, &result.ProvisioningFailedOrRepair)
		case !tokenAllowed && usageAllowed:
			err := tx.QueryRow(ctx, adminModelOperationsUsageOnlySQL, int64(window/time.Second)).Scan(&result.AsOf, &result.WindowFrom, &result.WindowTo, &result.UsageRequests, &result.ReconciliationPending, &result.ReconciliationStopped, &result.TrendBucketSeconds, &trendJSON)
			if err == nil {
				err = json.Unmarshal(trendJSON, &result.TrendPoints)
			}
			return err
		default:
			err := tx.QueryRow(ctx, adminModelOperationsBothSQL, int64(window/time.Second)).Scan(&result.AsOf, &result.ProvisioningPending, &result.ProvisioningFailedOrRepair, &result.WindowFrom, &result.WindowTo, &result.UsageRequests, &result.ReconciliationPending, &result.ReconciliationStopped, &result.TrendBucketSeconds, &trendJSON)
			if err == nil {
				err = json.Unmarshal(trendJSON, &result.TrendPoints)
			}
			return err
		}
	})
	return result, err
}
