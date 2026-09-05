package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type tokenOperationsOverviewStub struct {
	Repository
	values tokenOperationsOverviewValues
	err    error
	window time.Duration
}

func (stub *tokenOperationsOverviewStub) AdminTokenOperationsOverview(_ context.Context, window time.Duration) (tokenOperationsOverviewValues, error) {
	stub.window = window
	return stub.values, stub.err
}

func TestTokenOperationsOverviewWindowIsCanonicalEnum(t *testing.T) {
	for raw, expected := range map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour} {
		window, err := parseTokenOperationsOverviewWindow(url.Values{"window": {raw}})
		if err != nil || window != expected {
			t.Fatalf("window %q rejected: window=%v err=%v", raw, window, err)
		}
	}
	for _, invalid := range []url.Values{
		{},
		{"from": {"2026-08-01T00:00:00Z"}},
		{"window": {" 24h"}},
		{"window": {"24H"}},
		{"window": {"24h", "7d"}},
		{"window": {"31d"}},
	} {
		if _, err := parseTokenOperationsOverviewWindow(invalid); err == nil {
			t.Fatalf("invalid window accepted: %#v", invalid)
		}
	}
}

func TestTokenOperationsOverviewIsAtomicAndDeclaresDenominator(t *testing.T) {
	asOf := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	stub := &tokenOperationsOverviewStub{values: tokenOperationsOverviewValues{
		AsOf: asOf, WindowFrom: asOf.Add(-24 * time.Hour), WindowTo: asOf,
		PublishedModels: 2, StableRoutes: 3, HealthyRoutes: 2, RouteAttention: 1, UsageRequests: 5,
		TrendBucketSeconds: 3600,
		TrendPoints: func() []TokenOperationsOverviewTrendPoint {
			points := make([]TokenOperationsOverviewTrendPoint, 24)
			for index := range points {
				points[index] = TokenOperationsOverviewTrendPoint{From: asOf.Add(time.Duration(index-24) * time.Hour), To: asOf.Add(time.Duration(index-23) * time.Hour)}
			}
			points[23].UsageRequests, points[23].ReconciliationAttention = 5, 1
			return points
		}(),
	}}
	service := NewService(stub, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).WithAdminOperationsOverview(true)
	response := httptest.NewRecorder()
	service.adminTokenOperationsOverview(response, httptest.NewRequest(http.MethodGet, "/admin/v1/operations-overview?window=24h", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload TokenOperationsOverview
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if stub.window != 24*time.Hour || len(payload.Metrics) != 11 || payload.Window.Timezone != "UTC" || !payload.AsOf.Equal(asOf) {
		t.Fatalf("unexpected payload=%+v default window=%v", payload, stub.window)
	}
	if payload.Metrics[4].DenominatorKey != "stable_route_count" || payload.Metrics[8].Aggregation != "WINDOW" {
		t.Fatalf("metric semantics are incomplete: %+v", payload.Metrics)
	}
	if payload.Trend.BucketSeconds != 3600 || len(payload.Trend.Points) != 24 || payload.Trend.Points[23].UsageRequests != 5 {
		t.Fatalf("trend semantics are incomplete: %+v", payload.Trend)
	}
}

func TestTokenOperationsOverviewGateAndFailuresNeverReturnMetrics(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		service *Service
		status  int
		code    string
	}{
		{name: "disabled", service: NewService(nil, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0), status: 503, code: "TOKEN_OPERATIONS_OVERVIEW_DISABLED"},
		{name: "failed", service: NewService(&tokenOperationsOverviewStub{err: errors.New("database failed")}, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).WithAdminOperationsOverview(true), status: 503, code: "TOKEN_OPERATIONS_OVERVIEW_UNAVAILABLE"},
		{name: "timeout", service: NewService(&tokenOperationsOverviewStub{err: context.DeadlineExceeded}, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).WithAdminOperationsOverview(true), status: 504, code: "TOKEN_OPERATIONS_OVERVIEW_TIMEOUT"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			path := "/admin/v1/operations-overview"
			if testCase.name != "disabled" {
				path += "?window=24h"
			}
			testCase.service.adminTokenOperationsOverview(response, httptest.NewRequest(http.MethodGet, path, nil))
			var payload map[string]any
			if response.Code != testCase.status || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload["code"] != testCase.code || payload["metrics"] != nil {
				t.Fatalf("status=%d payload=%#v", response.Code, payload)
			}
		})
	}
}
