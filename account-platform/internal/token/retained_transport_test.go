package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPersonalAuthorityResponsesAreCompleteAndBounded(t *testing.T) {
	userID, walletID := uuid.New(), uuid.New()
	contextBody := fmt.Sprintf(`{"user_id":%q,"context_type":"PERSONAL","context_version":1,"authorization_version":1}`, userID)
	walletBody := fmt.Sprintf(`{"wallet_id":%q,"owner_type":"PERSONAL","owner_id":%q,"status":"ACTIVE","version":1}`, walletID, userID)
	for _, suffix := range []string{" {}", " invalid", strings.Repeat(" ", 1<<20)} {
		t.Run(fmt.Sprintf("suffix-%d", len(suffix)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/authorization-context") {
					_, _ = w.Write([]byte(contextBody + suffix))
					return
				}
				_, _ = w.Write([]byte(walletBody + suffix))
			}))
			defer server.Close()
			contexts, err := NewHTTPContextResolver(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := contexts.Resolve(context.Background(), "Bearer fixture-user"); err == nil {
				t.Error("accepted incomplete or oversized context authority")
			}
			wallets, err := NewHTTPWalletResolver(server.URL, "fixture-service")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := wallets.Ensure(context.Background(), "PERSONAL", userID); err == nil {
				t.Error("accepted incomplete or oversized wallet authority")
			}
		})
	}
}

func TestPersonalHTTPClientsRejectUnsafeEndpointConfiguration(t *testing.T) {
	for _, endpoint := range []string{"file://host/path", "https://user:secret@example.test", "https://example.test/?x=1", "https://example.test/?", "https://example.test/#fragment"} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := NewHTTPContextResolver(endpoint); err == nil {
				t.Error("accepted unsafe Account endpoint")
			}
			if _, err := NewHTTPWalletResolver(endpoint, "fixture-service"); err == nil {
				t.Error("accepted unsafe wallet endpoint")
			}
			if _, err := NewHTTPBillingClient(endpoint, "fixture-service"); err == nil {
				t.Error("accepted unsafe balance endpoint")
			}
		})
	}
}

func TestPersonalAccountOutageIsNotAPermissionDenial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"TEMPORARILY_UNAVAILABLE"}`))
	}))
	defer server.Close()
	contexts, err := NewHTTPContextResolver(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contexts.Resolve(context.Background(), "Bearer fixture-user"); err == nil || errors.Is(err, ErrForbidden) {
		t.Fatalf("outage misclassified as success/permission denial: %v", err)
	}
}
