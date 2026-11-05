package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type modelTokenPageOverviewStub struct {
	Repository
	values       modelTokenPageOverviewValues
	err          error
	rateErr      error
	rateCalls    int
	queryCalls   int
	kind         string
	window       time.Duration
	tokenAllowed bool
	usageAllowed bool
	source       string
}

func (stub *modelTokenPageOverviewStub) reserveAdminOverviewRate(_ context.Context, _ uuid.UUID, endpoint, source string) error {
	stub.rateCalls++
	stub.kind = endpoint
	stub.source = source
	return stub.rateErr
}

func (stub *modelTokenPageOverviewStub) AdminModelCatalogOverview(context.Context) (modelTokenPageOverviewValues, error) {
	stub.queryCalls++
	return stub.values, stub.err
}

func (stub *modelTokenPageOverviewStub) AdminTokenResourcesOverview(context.Context) (modelTokenPageOverviewValues, error) {
	stub.queryCalls++
	return stub.values, stub.err
}

func (stub *modelTokenPageOverviewStub) AdminModelOperationsOverview(_ context.Context, window time.Duration, tokenAllowed, usageAllowed bool) (modelTokenPageOverviewValues, error) {
	stub.queryCalls++
	stub.window, stub.tokenAllowed, stub.usageAllowed = window, tokenAllowed, usageAllowed
	return stub.values, stub.err
}

func overviewRequest(t *testing.T, path string, permissions ...string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = "10.0.0.8:4321"
	signAdminQuerySource(request, pageOverviewProxyKey, "203.0.113.19")
	ctx, cancel := context.WithDeadline(request.Context(), time.Now().Add(3*time.Second))
	t.Cleanup(cancel)
	ctx = authorization.WithGrantedPermissions(ctx, permissions...)
	return request.WithContext(ctx)
}

var pageOverviewProxyKey = bytes.Repeat([]byte{0x63}, 32)

func pageOverviewService(stub *modelTokenPageOverviewStub, enabled bool) *Service {
	return NewManagementService(stub, nil, nil, nil, ProviderURLPolicy{}, nil, nil).
		WithAdminProxyHMACKey(pageOverviewProxyKey).
		WithAdminPageOverviews(enabled)
}

func validUsageOverviewValues(window time.Duration) modelTokenPageOverviewValues {
	asOf := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	bucket := time.Hour
	count := int(window / bucket)
	if window > 24*time.Hour {
		bucket = 24 * time.Hour
		count = int(window / bucket)
	}
	points := make([]TokenOperationsOverviewTrendPoint, count)
	for index := range points {
		points[index] = TokenOperationsOverviewTrendPoint{From: asOf.Add(-window).Add(time.Duration(index) * bucket), To: asOf.Add(-window).Add(time.Duration(index+1) * bucket)}
	}
	return modelTokenPageOverviewValues{
		AsOf: asOf, WindowFrom: asOf.Add(-window), WindowTo: asOf,
		PublishedModels: 7, DeprecatedModels: 2, ModelDrafts: 3,
		StableRoutes: 10, HealthyRoutes: 8, RouteAttention: 2,
		ProvisioningPending: 4, ProvisioningFailedOrRepair: 1,
		UsageRequests: 6, ReconciliationPending: 2, ReconciliationStopped: 1,
		TrendBucketSeconds: int64(bucket / time.Second), TrendPoints: points,
	}
}

func TestPointOverviewQueriesRejectEveryParameterBeforeRepository(t *testing.T) {
	for _, item := range []struct {
		name, path, code string
		handler          func(*Service, http.ResponseWriter, *http.Request)
	}{
		{"model-unknown", "/admin/v1/overviews/model-catalog?unknown=1", "INVALID_MODEL_CATALOG_OVERVIEW_QUERY", func(s *Service, w http.ResponseWriter, r *http.Request) { s.adminModelCatalogOverview(w, r) }},
		{"model-empty", "/admin/v1/overviews/model-catalog?unknown", "INVALID_MODEL_CATALOG_OVERVIEW_QUERY", func(s *Service, w http.ResponseWriter, r *http.Request) { s.adminModelCatalogOverview(w, r) }},
		{"model-empty-name", "/admin/v1/overviews/model-catalog?=value", "INVALID_MODEL_CATALOG_OVERVIEW_QUERY", func(s *Service, w http.ResponseWriter, r *http.Request) { s.adminModelCatalogOverview(w, r) }},
		{"model-duplicate", "/admin/v1/overviews/model-catalog?x=1&x=2", "INVALID_MODEL_CATALOG_OVERVIEW_QUERY", func(s *Service, w http.ResponseWriter, r *http.Request) { s.adminModelCatalogOverview(w, r) }},
		{"token-unknown", "/admin/v1/overviews/token-resources?unknown=1", "INVALID_TOKEN_RESOURCES_OVERVIEW_QUERY", func(s *Service, w http.ResponseWriter, r *http.Request) { s.adminTokenResourcesOverview(w, r) }},
		{"token-empty", "/admin/v1/overviews/token-resources?unknown", "INVALID_TOKEN_RESOURCES_OVERVIEW_QUERY", func(s *Service, w http.ResponseWriter, r *http.Request) { s.adminTokenResourcesOverview(w, r) }},
		{"token-empty-name", "/admin/v1/overviews/token-resources?=value", "INVALID_TOKEN_RESOURCES_OVERVIEW_QUERY", func(s *Service, w http.ResponseWriter, r *http.Request) { s.adminTokenResourcesOverview(w, r) }},
		{"token-duplicate", "/admin/v1/overviews/token-resources?x=1&x=2", "INVALID_TOKEN_RESOURCES_OVERVIEW_QUERY", func(s *Service, w http.ResponseWriter, r *http.Request) { s.adminTokenResourcesOverview(w, r) }},
	} {
		t.Run(item.name, func(t *testing.T) {
			stub := &modelTokenPageOverviewStub{values: validUsageOverviewValues(24 * time.Hour)}
			service := pageOverviewService(stub, true)
			response := httptest.NewRecorder()
			item.handler(service, response, overviewRequest(t, item.path))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), item.code) || stub.rateCalls != 0 || stub.queryCalls != 0 {
				t.Fatalf("status=%d calls=%d/%d body=%s", response.Code, stub.rateCalls, stub.queryCalls, response.Body.String())
			}
		})
	}
}

func TestPointOverviewResponsesAreClosedAndOrdered(t *testing.T) {
	values := validUsageOverviewValues(24 * time.Hour)
	for _, item := range []struct {
		name, path, kind string
		handler          func(*Service, http.ResponseWriter, *http.Request)
		keys             []string
	}{
		{"model", "/admin/v1/overviews/model-catalog", modelCatalogOverviewKind, func(s *Service, w http.ResponseWriter, r *http.Request) { s.adminModelCatalogOverview(w, r) }, []string{"published_model_count", "deprecated_model_count", "model_draft_count"}},
		{"token", "/admin/v1/overviews/token-resources", tokenResourcesOverviewKind, func(s *Service, w http.ResponseWriter, r *http.Request) { s.adminTokenResourcesOverview(w, r) }, []string{"stable_route_count", "healthy_route_count", "route_attention_count"}},
	} {
		t.Run(item.name, func(t *testing.T) {
			stub := &modelTokenPageOverviewStub{values: values}
			service := pageOverviewService(stub, true)
			response := httptest.NewRecorder()
			item.handler(service, response, overviewRequest(t, item.path))
			var payload struct {
				SchemaVersion string                         `json:"schema_version"`
				AsOf          time.Time                      `json:"as_of"`
				Metrics       []ModelTokenPageOverviewMetric `json:"metrics"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.SchemaVersion != "1" || len(payload.Metrics) != 3 || stub.kind != item.kind || stub.rateCalls != 1 || stub.queryCalls != 1 {
				t.Fatalf("status=%d payload=%+v calls=%d/%d kind=%s body=%s", response.Code, payload, stub.rateCalls, stub.queryCalls, stub.kind, response.Body.String())
			}
			for index, key := range item.keys {
				if payload.Metrics[index].Key != key {
					t.Fatalf("metric order=%+v", payload.Metrics)
				}
			}
			if item.kind == modelCatalogOverviewKind && payload.Metrics[0].DenominatorKey != nil || item.kind == tokenResourcesOverviewKind && (payload.Metrics[0].DenominatorKey != nil || payload.Metrics[1].DenominatorKey == nil || *payload.Metrics[1].DenominatorKey != "stable_route_count") {
				t.Fatalf("denominator contract=%+v", payload.Metrics)
			}
		})
	}
}

func TestModelOperationsOverviewSelectsOneAuthorizedClosedVariant(t *testing.T) {
	for _, item := range []struct {
		name, query, view string
		permissions       []string
		token, usage      bool
		window            time.Duration
	}{
		{"token-only", "", "TOKEN_ONLY", []string{"platform.tokens.read"}, true, false, 0},
		{"usage-only", "?window=7d", "USAGE_ONLY", []string{"platform.usage.read"}, false, true, 7 * 24 * time.Hour},
		{"both", "?window=30d", "TOKEN_AND_USAGE", []string{"platform.tokens.read", "platform.usage.read"}, true, true, 30 * 24 * time.Hour},
	} {
		t.Run(item.name, func(t *testing.T) {
			stub := &modelTokenPageOverviewStub{values: validUsageOverviewValues(item.window)}
			if item.window == 0 {
				stub.values = validUsageOverviewValues(24 * time.Hour)
			}
			service := pageOverviewService(stub, true)
			response := httptest.NewRecorder()
			service.adminModelOperationsPageOverview(response, overviewRequest(t, "/admin/v1/overviews/model-operations"+item.query, item.permissions...))
			var payload map[string]json.RawMessage
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &payload) != nil || stub.window != item.window || stub.tokenAllowed != item.token || stub.usageAllowed != item.usage || stub.rateCalls != 1 || stub.queryCalls != 1 {
				t.Fatalf("status=%d call=%+v body=%s", response.Code, stub, response.Body.String())
			}
			var view string
			_ = json.Unmarshal(payload["permission_view"], &view)
			if view != item.view || (payload["provisioning"] != nil) != item.token || (payload["usage"] != nil) != item.usage {
				t.Fatalf("variant keys=%v view=%q", payload, view)
			}
		})
	}
}

func TestModelOperationsOverviewRejectsUnauthorizedOrInapplicableWindowBeforeRepository(t *testing.T) {
	for _, item := range []struct {
		name, query, code string
		permissions       []string
		status            int
	}{
		{"none", "", "PERMISSION_DENIED", nil, http.StatusForbidden},
		{"token-window", "?window=24h", "INVALID_TIME_WINDOW", []string{"platform.tokens.read"}, http.StatusBadRequest},
		{"usage-missing-window", "", "INVALID_TIME_WINDOW", []string{"platform.usage.read"}, http.StatusBadRequest},
		{"usage-duplicate-window", "?window=24h&window=7d", "INVALID_TIME_WINDOW", []string{"platform.usage.read"}, http.StatusBadRequest},
		{"usage-unknown", "?window=24h&extra=1", "INVALID_TIME_WINDOW", []string{"platform.usage.read"}, http.StatusBadRequest},
	} {
		t.Run(item.name, func(t *testing.T) {
			stub := &modelTokenPageOverviewStub{values: validUsageOverviewValues(24 * time.Hour)}
			service := pageOverviewService(stub, true)
			response := httptest.NewRecorder()
			service.adminModelOperationsPageOverview(response, overviewRequest(t, "/admin/v1/overviews/model-operations"+item.query, item.permissions...))
			if response.Code != item.status || !strings.Contains(response.Body.String(), item.code) || stub.rateCalls != 0 || stub.queryCalls != 0 {
				t.Fatalf("status=%d calls=%d/%d body=%s", response.Code, stub.rateCalls, stub.queryCalls, response.Body.String())
			}
		})
	}
}

func TestModelOperationsOverviewUsesEachFreshAuthorizationSnapshot(t *testing.T) {
	grants := [][]string{
		{"platform.tokens.read", "platform.usage.read"},
		{"platform.usage.read"},
		{"platform.tokens.read"},
		nil,
	}
	authorizationCalls := 0
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if authorizationCalls >= len(grants) {
			t.Fatal("unexpected authorization request")
		}
		current := grants[authorizationCalls]
		authorizationCalls++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Authorization-Version", strconv.Itoa(authorizationCalls))
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": len(current) > 0, "granted_permissions": current})
	}))
	defer authorizationServer.Close()
	authorizer, err := authorization.NewClient(authorizationServer.URL, "test-service-token")
	if err != nil {
		t.Fatal(err)
	}
	stub := &modelTokenPageOverviewStub{values: validUsageOverviewValues(24 * time.Hour)}
	service := NewManagementService(stub, nil, nil, nil, ProviderURLPolicy{}, nil, nil).
		WithAuthorizer(authorizer).
		WithAdminProxyHMACKey(pageOverviewProxyKey).
		WithAdminPageOverviews(true)
	subject := uuid.New().String()
	handler := service.ManagementHandler("", func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "test-issuer", Subject: subject, SessionID: "session"}, nil
	})
	for index, item := range []struct {
		path, view string
		status     int
	}{
		{"/admin/v1/overviews/model-operations?window=24h", "TOKEN_AND_USAGE", http.StatusOK},
		{"/admin/v1/overviews/model-operations?window=24h", "USAGE_ONLY", http.StatusOK},
		{"/admin/v1/overviews/model-operations", "TOKEN_ONLY", http.StatusOK},
		{"/admin/v1/overviews/model-operations", "", http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodGet, item.path, nil)
		request.RemoteAddr = "10.0.0.8:4321"
		signAdminQuerySource(request, pageOverviewProxyKey, "203.0.113.19")
		request.Header.Set("Authorization", "Bearer user-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != item.status {
			t.Fatalf("request %d status=%d body=%s", index+1, response.Code, response.Body.String())
		}
		if item.view != "" && !strings.Contains(response.Body.String(), `"permission_view":"`+item.view+`"`) {
			t.Fatalf("request %d did not use fresh permission view %s: %s", index+1, item.view, response.Body.String())
		}
	}
	if authorizationCalls != 4 || stub.queryCalls != 3 || !stub.tokenAllowed || stub.usageAllowed {
		t.Fatalf("authorization_calls=%d query_calls=%d final_variant=%v/%v", authorizationCalls, stub.queryCalls, stub.tokenAllowed, stub.usageAllowed)
	}
}

func TestEveryPageOverviewRequiresPositiveFreshAuthorizationVersionBeforeRepository(t *testing.T) {
	endpoints := []struct {
		name, path, permission string
	}{
		{"model-catalog", "/admin/v1/overviews/model-catalog", "platform.models.read"},
		{"token-resources", "/admin/v1/overviews/token-resources", "platform.tokens.read"},
		{"model-operations", "/admin/v1/overviews/model-operations", "platform.tokens.read"},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			versions := []string{"", "0", "not-a-version", "17", "18"}
			authorizationCalls := 0
			authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				version := versions[authorizationCalls]
				authorizationCalls++
				w.Header().Set("Content-Type", "application/json")
				if version != "" {
					w.Header().Set("X-Authorization-Version", version)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "granted_permissions": []string{endpoint.permission}})
			}))
			defer authorizationServer.Close()
			authorizer, err := authorization.NewClient(authorizationServer.URL, "test-service-token")
			if err != nil {
				t.Fatal(err)
			}
			stub := &modelTokenPageOverviewStub{values: validUsageOverviewValues(24 * time.Hour)}
			service := NewManagementService(stub, nil, nil, nil, ProviderURLPolicy{}, nil, nil).
				WithAuthorizer(authorizer).
				WithAdminProxyHMACKey(pageOverviewProxyKey).
				WithAdminPageOverviews(true)
			handler := service.ManagementHandler("", func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "test-issuer", Subject: uuid.NewString(), SessionID: "session"}, nil
			})
			for index, version := range versions {
				request := httptest.NewRequest(http.MethodGet, endpoint.path, nil)
				request.RemoteAddr = "10.0.0.8:4321"
				request.Header.Set("Authorization", "Bearer user-token")
				signAdminQuerySource(request, pageOverviewProxyKey, "203.0.113.19")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if index < 3 {
					if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "AUTHORIZATION_VERSION_INVALID") || stub.rateCalls != 0 || stub.queryCalls != 0 {
						t.Fatalf("version=%q status=%d calls=%d/%d body=%s", version, response.Code, stub.rateCalls, stub.queryCalls, response.Body.String())
					}
					continue
				}
				if response.Code != http.StatusOK || response.Header().Get("X-Authorization-Version") != version || stub.rateCalls != index-2 || stub.queryCalls != index-2 {
					t.Fatalf("version=%q status=%d response_version=%q calls=%d/%d body=%s", version, response.Code, response.Header().Get("X-Authorization-Version"), stub.rateCalls, stub.queryCalls, response.Body.String())
				}
			}
			if authorizationCalls != len(versions) {
				t.Fatalf("authorization calls=%d want=%d", authorizationCalls, len(versions))
			}
		})
	}
}

func TestPageOverviewGateRepositoryAndContextFailuresAreRouteSpecific(t *testing.T) {
	for _, item := range []struct {
		name, path, code string
		kind             string
		enabled          bool
		queryErr         error
	}{
		{"model-gate", "/admin/v1/overviews/model-catalog", "MODEL_CATALOG_OVERVIEW_UNAVAILABLE", modelCatalogOverviewKind, false, nil},
		{"model-repository", "/admin/v1/overviews/model-catalog", "MODEL_CATALOG_OVERVIEW_UNAVAILABLE", modelCatalogOverviewKind, true, errors.New("database failed")},
		{"model-context", "/admin/v1/overviews/model-catalog", "MODEL_CATALOG_OVERVIEW_TIMEOUT", modelCatalogOverviewKind, true, context.DeadlineExceeded},
		{"model-postgres-cancel", "/admin/v1/overviews/model-catalog", "MODEL_CATALOG_OVERVIEW_TIMEOUT", modelCatalogOverviewKind, true, &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}},
		{"token-gate", "/admin/v1/overviews/token-resources", "TOKEN_RESOURCES_OVERVIEW_UNAVAILABLE", tokenResourcesOverviewKind, false, nil},
		{"token-repository", "/admin/v1/overviews/token-resources", "TOKEN_RESOURCES_OVERVIEW_UNAVAILABLE", tokenResourcesOverviewKind, true, errors.New("database failed")},
		{"token-context", "/admin/v1/overviews/token-resources", "TOKEN_RESOURCES_OVERVIEW_TIMEOUT", tokenResourcesOverviewKind, true, context.DeadlineExceeded},
		{"token-postgres-cancel", "/admin/v1/overviews/token-resources", "TOKEN_RESOURCES_OVERVIEW_TIMEOUT", tokenResourcesOverviewKind, true, &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}},
		{"operations-gate", "/admin/v1/overviews/model-operations", "MODEL_OPERATIONS_OVERVIEW_UNAVAILABLE", modelOperationsOverviewKind, false, nil},
		{"operations-repository", "/admin/v1/overviews/model-operations", "MODEL_OPERATIONS_OVERVIEW_UNAVAILABLE", modelOperationsOverviewKind, true, errors.New("database failed")},
		{"operations-context", "/admin/v1/overviews/model-operations", "MODEL_OPERATIONS_OVERVIEW_TIMEOUT", modelOperationsOverviewKind, true, context.DeadlineExceeded},
		{"operations-postgres-cancel", "/admin/v1/overviews/model-operations", "MODEL_OPERATIONS_OVERVIEW_TIMEOUT", modelOperationsOverviewKind, true, &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}},
	} {
		t.Run(item.name, func(t *testing.T) {
			stub := &modelTokenPageOverviewStub{values: validUsageOverviewValues(24 * time.Hour), err: item.queryErr}
			service := pageOverviewService(stub, item.enabled)
			response := httptest.NewRecorder()
			switch item.kind {
			case modelCatalogOverviewKind:
				service.adminModelCatalogOverview(response, overviewRequest(t, item.path))
			case tokenResourcesOverviewKind:
				service.adminTokenResourcesOverview(response, overviewRequest(t, item.path))
			default:
				service.adminModelOperationsPageOverview(response, overviewRequest(t, item.path, "platform.tokens.read"))
			}
			var payload map[string]any
			expectedStatus := http.StatusServiceUnavailable
			if isModelTokenPageOverviewTimeout(item.queryErr) {
				expectedStatus = http.StatusGatewayTimeout
			}
			if response.Code != expectedStatus || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload["code"] != item.code || payload["metrics"] != nil || payload["provisioning"] != nil || payload["usage"] != nil {
				t.Fatalf("status=%d payload=%v", response.Code, payload)
			}
		})
	}
}

func TestPageOverviewTypedTimeoutClassification(t *testing.T) {
	for _, item := range []struct {
		name string
		err  error
		want bool
	}{
		{"deadline", context.DeadlineExceeded, true},
		{"cancel", context.Canceled, true},
		{"postgres-query-canceled", &pgconn.PgError{Code: "57014"}, true},
		{"postgres-unavailable", &pgconn.PgError{Code: "08006"}, false},
		{"plain-error-text-is-not-typed", errors.New("canceling statement due to statement timeout"), false},
	} {
		t.Run(item.name, func(t *testing.T) {
			if got := isModelTokenPageOverviewTimeout(item.err); got != item.want {
				t.Fatalf("timeout=%v want=%v", got, item.want)
			}
		})
	}
}

func TestPageOverviewRateLimitUsesCeilingRetryAfter(t *testing.T) {
	if got := adminOverviewRetryAfter(time.Unix(0, 0).Add(1501*time.Millisecond), time.Unix(0, 0)); got != 2 {
		t.Fatalf("Retry-After did not round up: %d", got)
	}
	stub := &modelTokenPageOverviewStub{values: validUsageOverviewValues(24 * time.Hour), rateErr: adminQueryRateLimitError{RetryAfter: 19}}
	service := pageOverviewService(stub, true)
	response := httptest.NewRecorder()
	service.adminTokenResourcesOverview(response, overviewRequest(t, "/admin/v1/overviews/token-resources"))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "19" || !strings.Contains(response.Body.String(), "TOKEN_OVERVIEW_RATE_LIMITED") || stub.queryCalls != 0 {
		t.Fatalf("status=%d retry=%q calls=%d body=%s", response.Code, response.Header().Get("Retry-After"), stub.queryCalls, response.Body.String())
	}
}

func TestPageOverviewRequiresValidSignedPortalSource(t *testing.T) {
	for _, item := range []struct {
		name       string
		serviceKey []byte
		requestKey []byte
		remote     string
		sign       bool
		status     int
	}{
		{"missing-key", nil, pageOverviewProxyKey, "10.0.0.8:4321", true, http.StatusServiceUnavailable},
		{"short-key", bytes.Repeat([]byte{1}, 31), bytes.Repeat([]byte{1}, 31), "10.0.0.8:4321", true, http.StatusServiceUnavailable},
		{"missing-signature", pageOverviewProxyKey, nil, "10.0.0.8:4321", false, http.StatusServiceUnavailable},
		{"mismatched-signature", pageOverviewProxyKey, bytes.Repeat([]byte{2}, 32), "10.0.0.8:4321", true, http.StatusServiceUnavailable},
		{"public-direct", pageOverviewProxyKey, pageOverviewProxyKey, "198.51.100.8:4321", true, http.StatusServiceUnavailable},
		{"valid-private-portal", pageOverviewProxyKey, pageOverviewProxyKey, "10.0.0.8:4321", true, http.StatusOK},
	} {
		t.Run(item.name, func(t *testing.T) {
			stub := &modelTokenPageOverviewStub{values: validUsageOverviewValues(24 * time.Hour)}
			service := NewManagementService(stub, nil, nil, nil, ProviderURLPolicy{}, nil, nil).
				WithAdminProxyHMACKey(item.serviceKey).
				WithAdminPageOverviews(true)
			request := overviewRequest(t, "/admin/v1/overviews/model-catalog")
			request.RemoteAddr = item.remote
			request.Header.Del("X-Pinvou-Client-Source-Signature")
			if item.sign {
				signAdminQuerySource(request, item.requestKey, "203.0.113.19")
			}
			response := httptest.NewRecorder()
			service.adminModelCatalogOverview(response, request)
			if response.Code != item.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if item.status == http.StatusOK {
				if stub.source != "203.0.113.19" || stub.rateCalls != 1 || stub.queryCalls != 1 {
					t.Fatalf("trusted source=%q calls=%d/%d", stub.source, stub.rateCalls, stub.queryCalls)
				}
			} else if stub.rateCalls != 0 || stub.queryCalls != 0 || !strings.Contains(response.Body.String(), "MODEL_CATALOG_OVERVIEW_UNAVAILABLE") {
				t.Fatalf("untrusted request reached repository: calls=%d/%d body=%s", stub.rateCalls, stub.queryCalls, response.Body.String())
			}
		})
	}
}

func TestPageOverviewRejectsEveryMalformedProjectionAsWholeResponse(t *testing.T) {
	type testCase struct {
		name, kind, path string
		permissions      []string
		mutate           func(*modelTokenPageOverviewValues)
	}
	cases := []testCase{
		{"model-missing-as-of", modelCatalogOverviewKind, "/admin/v1/overviews/model-catalog", nil, func(v *modelTokenPageOverviewValues) { v.AsOf = time.Time{} }},
		{"model-negative-published", modelCatalogOverviewKind, "/admin/v1/overviews/model-catalog", nil, func(v *modelTokenPageOverviewValues) { v.PublishedModels = -1 }},
		{"model-negative-deprecated", modelCatalogOverviewKind, "/admin/v1/overviews/model-catalog", nil, func(v *modelTokenPageOverviewValues) { v.DeprecatedModels = -1 }},
		{"model-negative-drafts", modelCatalogOverviewKind, "/admin/v1/overviews/model-catalog", nil, func(v *modelTokenPageOverviewValues) { v.ModelDrafts = -1 }},
		{"token-missing-as-of", tokenResourcesOverviewKind, "/admin/v1/overviews/token-resources", nil, func(v *modelTokenPageOverviewValues) { v.AsOf = time.Time{} }},
		{"token-negative-stable", tokenResourcesOverviewKind, "/admin/v1/overviews/token-resources", nil, func(v *modelTokenPageOverviewValues) { v.StableRoutes = -1 }},
		{"token-negative-healthy", tokenResourcesOverviewKind, "/admin/v1/overviews/token-resources", nil, func(v *modelTokenPageOverviewValues) { v.HealthyRoutes = -1 }},
		{"token-negative-attention", tokenResourcesOverviewKind, "/admin/v1/overviews/token-resources", nil, func(v *modelTokenPageOverviewValues) { v.RouteAttention = -1 }},
		{"token-denominator-mismatch", tokenResourcesOverviewKind, "/admin/v1/overviews/token-resources", nil, func(v *modelTokenPageOverviewValues) { v.StableRoutes++ }},
		{"operations-missing-as-of", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations", []string{"platform.tokens.read"}, func(v *modelTokenPageOverviewValues) { v.AsOf = time.Time{} }},
		{"operations-negative-pending", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations", []string{"platform.tokens.read"}, func(v *modelTokenPageOverviewValues) { v.ProvisioningPending = -1 }},
		{"operations-negative-attention", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations", []string{"platform.tokens.read"}, func(v *modelTokenPageOverviewValues) { v.ProvisioningFailedOrRepair = -1 }},
		{"usage-negative-total", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations?window=24h", []string{"platform.usage.read"}, func(v *modelTokenPageOverviewValues) { v.UsageRequests = -1 }},
		{"usage-negative-pending", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations?window=24h", []string{"platform.usage.read"}, func(v *modelTokenPageOverviewValues) { v.ReconciliationPending = -1 }},
		{"usage-negative-stopped", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations?window=24h", []string{"platform.usage.read"}, func(v *modelTokenPageOverviewValues) { v.ReconciliationStopped = -1 }},
		{"usage-window-as-of-mismatch", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations?window=24h", []string{"platform.usage.read"}, func(v *modelTokenPageOverviewValues) { v.WindowTo = v.WindowTo.Add(time.Second) }},
		{"usage-window-size-mismatch", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations?window=24h", []string{"platform.usage.read"}, func(v *modelTokenPageOverviewValues) { v.WindowFrom = v.WindowFrom.Add(time.Second) }},
		{"usage-bucket-size", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations?window=24h", []string{"platform.usage.read"}, func(v *modelTokenPageOverviewValues) { v.TrendBucketSeconds++ }},
		{"usage-point-count", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations?window=24h", []string{"platform.usage.read"}, func(v *modelTokenPageOverviewValues) { v.TrendPoints = v.TrendPoints[:23] }},
		{"usage-negative-point", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations?window=24h", []string{"platform.usage.read"}, func(v *modelTokenPageOverviewValues) { v.TrendPoints[0].UsageRequests = -1 }},
		{"usage-discontinuous-point", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations?window=24h", []string{"platform.usage.read"}, func(v *modelTokenPageOverviewValues) { v.TrendPoints[1].From = v.TrendPoints[1].From.Add(time.Second) }},
		{"usage-final-boundary", modelOperationsOverviewKind, "/admin/v1/overviews/model-operations?window=24h", []string{"platform.usage.read"}, func(v *modelTokenPageOverviewValues) { v.TrendPoints[23].To = v.TrendPoints[23].To.Add(-time.Second) }},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			values := validUsageOverviewValues(24 * time.Hour)
			item.mutate(&values)
			stub := &modelTokenPageOverviewStub{values: values}
			service := pageOverviewService(stub, true)
			response := httptest.NewRecorder()
			request := overviewRequest(t, item.path, item.permissions...)
			switch item.kind {
			case modelCatalogOverviewKind:
				service.adminModelCatalogOverview(response, request)
			case tokenResourcesOverviewKind:
				service.adminTokenResourcesOverview(response, request)
			default:
				service.adminModelOperationsPageOverview(response, request)
			}
			var payload map[string]any
			if response.Code != http.StatusServiceUnavailable || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload["metrics"] != nil || payload["provisioning"] != nil || payload["usage"] != nil || stub.rateCalls != 1 || stub.queryCalls != 1 {
				t.Fatalf("status=%d payload=%v calls=%d/%d", response.Code, payload, stub.rateCalls, stub.queryCalls)
			}
		})
	}
}

func TestPageOverviewProductionSQLKeepsDomainsIndependent(t *testing.T) {
	if strings.Contains(adminModelCatalogOverviewSQL, "routing_policies") || strings.Contains(adminModelCatalogOverviewSQL, "usage_requests") {
		t.Fatal("model catalog overview leaked another domain")
	}
	if strings.Contains(adminTokenResourcesOverviewSQL, "model_catalog_entries") || strings.Contains(adminTokenResourcesOverviewSQL, "usage_requests") {
		t.Fatal("Token resources overview leaked another domain")
	}
	if strings.Contains(adminModelOperationsTokenOnlySQL, "usage_requests") || strings.Contains(adminModelOperationsUsageOnlySQL, "route_provisionings") {
		t.Fatal("an unauthorized model operations branch is present")
	}
}
