package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type usageLookupRepository struct {
	Repository
	usage UsageRequest
	err   error
}

func (r usageLookupRepository) GetUsage(context.Context, uuid.UUID) (UsageRequest, error) {
	return r.usage, r.err
}

func TestUsageStatusHTTPKeepsStorageFailureDistinctFromHiddenOrMissingUsage(t *testing.T) {
	resolved := AuthorizationContext{UserID: uuid.New(), SessionID: "session", ContextType: "PERSONAL", ContextVersion: 1, AuthorizationVersion: 1}
	actual := int64(3)
	usage := UsageRequest{ID: uuid.New(), UserID: resolved.UserID, SessionID: resolved.SessionID,
		TokenAccountID: uuid.New(), WalletID: uuid.New(), Model: "model-test", Status: "COMMITTED",
		MaximumMicrocredits: 5, ActualMicrocredits: &actual, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	otherSession := usage
	otherSession.SessionID = "other-session"
	for _, test := range []struct {
		name   string
		usage  UsageRequest
		err    error
		status int
		code   string
	}{
		{name: "matching-session", usage: usage, status: http.StatusOK},
		{name: "missing", err: ErrNotFound, status: http.StatusNotFound, code: "TOKEN_ACCOUNT_NOT_FOUND"},
		{name: "other-session-hidden", usage: otherSession, status: http.StatusNotFound, code: "TOKEN_ACCOUNT_NOT_FOUND"},
		{name: "storage-unavailable", err: errors.New("private storage details must not escape"), status: http.StatusInternalServerError, code: "INTERNAL_ERROR"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := usageLookupRepository{usage: test.usage, err: test.err}
			service := NewService(repository, fixtureContexts{resolved}, nil, nil, ProviderURLPolicy{}, nil, nil, 100)
			server := httptest.NewServer(service.Handler(func(context.Context, string) (identity.Principal, error) { return identity.Principal{}, nil }))
			defer server.Close()
			request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/usage-requests/"+usage.ID.String(), nil)
			request.Header.Set("Authorization", "Bearer test-only-access-token")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body map[string]any
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != test.status {
				t.Fatalf("usage status response=%d decode=%v", response.StatusCode, err)
			}
			if test.code != "" {
				if body["code"] != test.code || body["message"] == "private storage details must not escape" {
					t.Fatal("lookup failure was misclassified or leaked storage details")
				}
			} else if body["usage_request_id"] != usage.ID.String() || body["display_status"] != "SETTLED" || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("matching usage status did not use the production response contract")
			}
		})
	}
}

func TestUsageDisplayStatusKeepsSettlementPendingDistinct(t *testing.T) {
	cases := map[string]string{
		"CREATED": "IN_PROGRESS", "RESERVED": "IN_PROGRESS", "UPSTREAM_ACTIVE": "IN_PROGRESS",
		"COMMITTED": "SETTLED", "SETTLEMENT_PENDING": "SETTLEMENT_PENDING", "RELEASED": "RELEASED", "REVIEW": "MANUAL_REVIEW",
	}
	for status, expected := range cases {
		if actual := usageDisplayStatus(UsageRequest{Status: status}); actual != expected {
			t.Fatalf("usageDisplayStatus(%q) = %q, want %q", status, actual, expected)
		}
	}
}

func TestUsageHistoryCursorRoundTripAndRejectsInvalidInput(t *testing.T) {
	want := UsageHistoryCursor{
		StartedAt: time.Date(2026, time.August, 18, 9, 30, 0, 123, time.UTC),
		ID:        uuid.MustParse("0198f8e3-2d27-7a2b-93aa-4f1a8f796e8c"),
	}
	got, err := decodeUsageCursor(encodeUsageCursor(want))
	if err != nil || !got.StartedAt.Equal(want.StartedAt) || got.ID != want.ID {
		t.Fatalf("cursor round trip = %#v, %v", got, err)
	}
	for _, invalid := range []string{"not-base64!", "YWJj", string(make([]byte, 257))} {
		if _, err := decodeUsageCursor(invalid); err == nil {
			t.Fatalf("accepted invalid cursor %q", invalid)
		}
	}
}

func TestUsageStatusRequiresSameSessionAndActiveContext(t *testing.T) {
	userID := uuid.New()
	organizationID := uuid.New()
	usage := UsageRequest{UserID: userID, SessionID: "session", OrganizationID: &organizationID}
	context := AuthorizationContext{UserID: userID, SessionID: "session", ContextType: "ORGANIZATION", ContextVersion: 1, AuthorizationVersion: 1, OrganizationID: &organizationID}
	if !usageContextMatches(context, usage) {
		t.Fatal("same session and organization should be authorized")
	}
	personal := context
	personal.ContextType, personal.OrganizationID = "PERSONAL", nil
	if usageContextMatches(personal, usage) {
		t.Fatal("organization usage leaked into personal context")
	}
	otherSession := context
	otherSession.SessionID = "other"
	if usageContextMatches(otherSession, usage) {
		t.Fatal("usage leaked across sessions")
	}
}
