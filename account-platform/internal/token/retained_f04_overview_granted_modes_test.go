package token

import (
	"context"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRetainedF04OverviewGrantedModes(t *testing.T) {
	grants := [][]string{
		{"platform.tokens.read", "platform.usage.read"},
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
		{"/admin/v1/overviews/model-operations", "", http.StatusBadRequest},
		{"/admin/v1/overviews/model-operations?window=24h", "TOKEN_AND_USAGE", http.StatusOK},
		{"/admin/v1/overviews/model-operations?window=24h", "USAGE_ONLY", http.StatusOK},
		{"/admin/v1/overviews/model-operations", "TOKEN_ONLY", http.StatusOK},
		{"/admin/v1/overviews/model-operations", "", http.StatusForbidden},
	} {
		t.Run(strconv.Itoa(index+1), func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, item.path, nil)
			request.RemoteAddr = "10.0.0.8:4321"
			signAdminQuerySource(request, pageOverviewProxyKey, "203.0.113.19")
			request.Header.Set("Authorization", "Bearer user-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if index == 0 && (stub.queryCalls != 0 || stub.rateCalls != 0) {
				t.Fatal("missing dual window reached owner")
			}
			if response.Code != item.status {
				t.Fatalf("request %d status=%d body=%s", index+1, response.Code, response.Body.String())
			}
			if item.view != "" && !strings.Contains(response.Body.String(), `"permission_view":"`+item.view+`"`) {
				t.Fatalf("request %d did not use fresh permission view %s: %s", index+1, item.view, response.Body.String())
			}
		})
	}
	if authorizationCalls != 5 || stub.queryCalls != 3 || !stub.tokenAllowed || stub.usageAllowed {
		t.Fatalf("authorization_calls=%d query_calls=%d final_variant=%v/%v", authorizationCalls, stub.queryCalls, stub.tokenAllowed, stub.usageAllowed)
	}
}
