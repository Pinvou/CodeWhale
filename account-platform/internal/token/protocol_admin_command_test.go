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

func TestProtocolFormulaReasonUsesUTF8RuneBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "nine Chinese runes", value: strings.Repeat("审", 9), valid: false},
		{name: "ten Chinese runes", value: strings.Repeat("审", 10), valid: true},
		{name: "two thousand Chinese runes", value: strings.Repeat("审", 2000), valid: true},
		{name: "two thousand and one Chinese runes", value: strings.Repeat("审", 2001), valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if actual := validProtocolFormulaReason(test.value); actual != test.valid {
				t.Fatalf("valid=%v, want %v", actual, test.valid)
			}
		})
	}
}

func TestProtocolFormulaDomainMetadataRemainsRequired(t *testing.T) {
	metadata := ProtocolFormulaCommandMetadata{ActorID: uuid.New(), CommandType: "TOKEN_FORMULA_PUBLISH", Reason: "publish the verified formula", IdempotencyKeyDigest: strings.Repeat("a", 64), RequestFingerprint: strings.Repeat("b", 64), CommandReference: strings.Repeat("c", 64)}
	if err := validateProtocolFormulaCommandMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"actor", "reason", "key", "fingerprint", "reference", "type"} {
		invalid := metadata
		switch field {
		case "actor":
			invalid.ActorID = uuid.Nil
		case "reason":
			invalid.Reason = "short"
		case "key":
			invalid.IdempotencyKeyDigest = ""
		case "fingerprint":
			invalid.RequestFingerprint = ""
		case "reference":
			invalid.CommandReference = ""
		case "type":
			invalid.CommandType = ""
		}
		if validateProtocolFormulaCommandMetadata(invalid) == nil {
			t.Fatalf("missing %s accepted", field)
		}
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
