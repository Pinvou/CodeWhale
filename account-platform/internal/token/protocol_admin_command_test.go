package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestProtocolDefinitionETagUsesCanonicalJSON(t *testing.T) {
	id := uuid.MustParse("018f57a9-9c17-7efc-a450-35c445491001")
	left, ok := canonicalJSONObject(json.RawMessage(`{"b":2,"a":1}`))
	if !ok {
		t.Fatal("first JSON object must be valid")
	}
	right, ok := canonicalJSONObject(json.RawMessage(`{"a":1,"b":2}`))
	if !ok {
		t.Fatal("second JSON object must be valid")
	}
	createdAt := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	first := ProtocolDefinition{ID: id, ProtocolCode: "chat-v2", Version: 2, Status: "DRAFT", RequestSchema: left, CreatedAt: createdAt}
	second := first
	second.RequestSchema = right
	if string(left) != string(right) || protocolDefinitionETag(first) != protocolDefinitionETag(second) {
		t.Fatalf("equivalent JSON must produce one stable ETag: %s != %s", left, right)
	}
}

func TestFormalProtocolAndFormulaPublishRequireRecentAuthentication(t *testing.T) {
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Authorization-Version", "1")
		_, _ = w.Write([]byte(`{"allowed":false}`))
	}))
	defer authority.Close()
	client, err := authorization.NewClient(authority.URL, "controlled-test")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{authorizer: client}
	for _, command := range []string{"TOKEN_PROTOCOL_PUBLISH", "TOKEN_FORMULA_PUBLISH"} {
		for _, age := range []struct {
			name   string
			at     time.Time
			status int
		}{{"absent", time.Time{}, 401}, {"stale", time.Now().Add(-16 * time.Minute), 401}, {"future", time.Now().Add(2 * time.Minute), 401}, {"recent", time.Now().Add(-time.Minute), 403}, {"clock tolerance", time.Now().Add(30 * time.Second), 403}} {
			t.Run(command+"/"+age.name, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodPost, "/token/admin/v1/commands", strings.NewReader(`{"command_type":"`+command+`"}`))
				response := httptest.NewRecorder()
				handler := identity.Middleware(func(context.Context, string) (identity.Principal, error) {
					return identity.Principal{Subject: uuid.NewString(), AuthenticatedAt: age.at}, nil
				}, http.HandlerFunc(service.adminCreateTokenCommand))
				handler.ServeHTTP(response, withBearer(request))
				if response.Code != age.status {
					t.Fatalf("formal status=%d body=%s", response.Code, response.Body.String())
				}
			})
		}
	}
}

func withBearer(request *http.Request) *http.Request {
	request.Header.Set("Authorization", "Bearer test")
	return request
}
