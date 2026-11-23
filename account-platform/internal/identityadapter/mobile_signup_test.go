package identityadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestSMSOnlyIdentityCreatesNoPasswordAndNeverRedispatches(t *testing.T) {
	created := make(chan map[string]json.RawMessage, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_, _ = w.Write([]byte(`{"access_token":"test-fixture"}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/admin/realms/fixture/users":
			var user map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&user); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			created <- user
			w.Header().Set("Location", "/admin/realms/fixture/users/sms-user")
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	directory, err := NewKeycloakDirectory(server.URL, server.URL+"/token", "fixture", "fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := uuid.New()
	const username, mobile = "user-0123456789abcdef01234567", "+8613800138000"
	mode := identity.MobileSignupCredentialMode
	if _, err := vault.CreateMobileUserFirstAttempt(ctx, id, "password-handle", mode, username, mobile, true); !errors.Is(err, ErrReferenceInvalid) {
		t.Fatal("password reference was accepted by SMS-only creation", err)
	}
	if _, err := vault.CreateMobileUserFirstAttempt(ctx, id, "", mode, username, mobile, false); !errors.Is(err, ErrCreateUnconfirmed) {
		t.Fatal("nonfirst request could create an identity", err)
	}
	if subject, err := vault.CreateMobileUserFirstAttempt(ctx, id, "", mode, username, mobile, true); err != nil || subject != "sms-user" {
		t.Fatal("failed to create SMS identity", subject, err)
	}
	if _, err := vault.CreateMobileUserFirstAttempt(ctx, id, "", mode, username, mobile, true); !errors.Is(err, ErrCreateUnconfirmed) {
		t.Fatal("a stale first grant was allowed to repeat remote mutation", err)
	}
	if len(created) != 1 {
		t.Fatal("unexpected identity create count", len(created))
	}
	user := <-created
	if _, hasCredentials := user["credentials"]; hasCredentials {
		t.Fatal("the SMS-only account acquired a password credential")
	}
	var attrs map[string][]string
	if err := json.Unmarshal(user["attributes"], &attrs); err != nil || strings.Join(attrs["phone_number_verified"], "") != "true" ||
		strings.Join(attrs["phone_number"], "") != mobile || strings.Join(attrs["pinvou_registration_operation_id"], "") != id.String() {
		t.Fatal("identity lacks the verified mobile and exact operation correlation", err)
	}
}
