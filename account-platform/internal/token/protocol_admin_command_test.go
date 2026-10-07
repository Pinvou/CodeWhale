package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestProtocolFormulaCommandMetadataAndUnknownResultAreStable(t *testing.T) {
	body := map[string]any{"reason": "publish the verified formula", "expected_formula_version": 3}
	request := httptest.NewRequest(http.MethodPost, "/token/admin/v1/formulas/018f57a9-9c17-7efc-a450-35c445491003/publish", nil)
	request.Header.Set("Idempotency-Key", "formula-publish-0001")
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Subject: "018f57a9-9c17-7efc-a450-35c445491004", AuthenticatedAt: time.Now().UTC()}, nil
	}
	var first ProtocolFormulaCommandMetadata
	identity.Middleware(verify, http.HandlerFunc(func(_ http.ResponseWriter, authenticated *http.Request) {
		var err error
		first, err = protocolFormulaCommandMetadata(authenticated, "TOKEN_FORMULA_PUBLISH", "publish the verified formula", body)
		if err != nil {
			t.Fatal(err)
		}
	})).ServeHTTP(httptest.NewRecorder(), withBearer(request))
	if len(first.CommandReference) != 64 || len(first.IdempotencyKeyDigest) != 64 || len(first.RequestFingerprint) != 64 {
		t.Fatalf("unexpected command metadata: %#v", first)
	}

	response := httptest.NewRecorder()
	writeProtocolFormulaCommandError(response, request, first, context.DeadlineExceeded)
	if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), `"code":"COMMAND_RESULT_UNKNOWN"`) || !strings.Contains(response.Body.String(), first.CommandReference) {
		t.Fatalf("unexpected unknown-result response: %d %s", response.Code, response.Body.String())
	}
}

func TestProtocolAndFormulaPublishRequireRecentAuthentication(t *testing.T) {
	for _, authenticatedAt := range []time.Time{time.Time{}, time.Now().Add(-16 * time.Minute), time.Now().Add(2 * time.Minute)} {
		request := httptest.NewRequest(http.MethodPost, "/publish", nil)
		response := httptest.NewRecorder()
		verify := func(context.Context, string) (identity.Principal, error) {
			return identity.Principal{Subject: uuid.NewString(), AuthenticatedAt: authenticatedAt}, nil
		}
		identity.Middleware(verify, http.HandlerFunc(func(w http.ResponseWriter, authenticated *http.Request) {
			if requireRecentProtocolFormulaAuthentication(w, authenticated, 15*time.Minute) {
				t.Fatal("stale, absent, or future authentication must not pass")
			}
		})).ServeHTTP(response, withBearer(request))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("authenticated_at=%v returned %d", authenticatedAt, response.Code)
		}
	}
}

func withBearer(request *http.Request) *http.Request {
	request.Header.Set("Authorization", "Bearer test")
	return request
}
