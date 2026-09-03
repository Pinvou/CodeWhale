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
	from   *time.Time
	to     *time.Time
}

func (stub *tokenOperationsOverviewStub) AdminTokenOperationsOverview(_ context.Context, from, to *time.Time) (tokenOperationsOverviewValues, error) {
	stub.from, stub.to = from, to
	return stub.values, stub.err
}

func TestTokenOperationsOverviewWindowIsBoundedUTC(t *testing.T) {
	valid := url.Values{"from": {"2026-08-01T00:00:00Z"}, "to": {"2026-09-01T00:00:00+00:00"}}
	from, to, err := parseTokenOperationsOverviewWindow(valid)
	if err != nil || from == nil || to == nil || to.Sub(*from) != 31*24*time.Hour {
		t.Fatalf("valid window rejected: from=%v to=%v err=%v", from, to, err)
	}
	for _, invalid := range []url.Values{
		{"from": {"2026-08-01T00:00:00Z"}},
		{"from": {"2026-08-01T00:00:00+08:00"}, "to": {"2026-08-02T00:00:00+08:00"}},
		{"from": {"2026-08-01T00:00:00Z"}, "to": {"2026-09-01T00:00:01Z"}},
		{"window": {"24h"}},
	} {
		if _, _, err := parseTokenOperationsOverviewWindow(invalid); err == nil {
			t.Fatalf("invalid window accepted: %#v", invalid)
		}
	}
}

func TestTokenOperationsOverviewIsAtomicAndDeclaresDenominator(t *testing.T) {
	asOf := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	stub := &tokenOperationsOverviewStub{values: tokenOperationsOverviewValues{
		AsOf: asOf, WindowFrom: asOf.Add(-24 * time.Hour), WindowTo: asOf,
		PublishedModels: 2, StableRoutes: 3, HealthyRoutes: 2, RouteAttention: 1, UsageRequests: 5,
	}}
	service := NewService(stub, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).WithAdminOperationsOverview(true)
	response := httptest.NewRecorder()
	service.adminTokenOperationsOverview(response, httptest.NewRequest(http.MethodGet, "/admin/v1/operations-overview", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload TokenOperationsOverview
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if stub.from != nil || stub.to != nil || len(payload.Metrics) != 11 || payload.Window.Timezone != "UTC" || !payload.AsOf.Equal(asOf) {
		t.Fatalf("unexpected payload=%+v default bounds=%v/%v", payload, stub.from, stub.to)
	}
	if payload.Metrics[4].DenominatorKey != "stable_route_count" || payload.Metrics[8].Aggregation != "WINDOW" {
		t.Fatalf("metric semantics are incomplete: %+v", payload.Metrics)
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
			testCase.service.adminTokenOperationsOverview(response, httptest.NewRequest(http.MethodGet, "/admin/v1/operations-overview", nil))
			var payload map[string]any
			if response.Code != testCase.status || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload["code"] != testCase.code || payload["metrics"] != nil {
				t.Fatalf("status=%d payload=%#v", response.Code, payload)
			}
		})
	}
}
