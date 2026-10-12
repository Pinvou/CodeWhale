package account

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestIdentityMutationsRequireCompletedAcknowledgement(t *testing.T) {
	operationID := uuid.New()
	actions := map[string]func(*HTTPIdentityProvisioner) error{
		"password validation": func(p *HTTPIdentityProvisioner) error {
			return p.ValidateMobileUser(context.Background(), operationID, "fixture-name", "+8613800138000", "fixture-handle", "user-v1")
		},
		"username update": func(p *HTTPIdentityProvisioner) error {
			return p.UpdateUsername(context.Background(), "fixture-subject", operationID, 1, "fixture-name")
		},
		"mobile update": func(p *HTTPIdentityProvisioner) error {
			return p.UpdateMobile(context.Background(), "fixture-subject", operationID, 1, "+8613800138000")
		},
		"password reset": func(p *HTTPIdentityProvisioner) error {
			return p.ResetPassword(context.Background(), "fixture-subject", operationID, 1, "+8613800138000", "fixture-handle", "user-v1")
		},
		"session revocation": func(p *HTTPIdentityProvisioner) error {
			return p.RevokeSessions(context.Background(), "fixture-subject")
		},
		"disable confirmation": func(p *HTTPIdentityProvisioner) error {
			return p.DisableUserAndConfirm(context.Background(), "fixture-subject")
		},
	}
	for name, action := range actions {
		for _, status := range []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent} {
			t.Run(fmt.Sprintf("%s/%d", name, status), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.WriteHeader(status)
				}))
				defer server.Close()
				provisioner, err := NewHTTPIdentityProvisioner(server.URL, "fixture-service-token")
				if err != nil {
					t.Fatal(err)
				}
				err = action(provisioner)
				if calls != 1 {
					t.Fatalf("expected one authority request, got %d", calls)
				}
				if status == http.StatusNoContent && err != nil {
					t.Fatalf("completed acknowledgement rejected: %v", err)
				}
				if status != http.StatusNoContent && err == nil {
					t.Fatal("unconfirmed mutation accepted as completed")
				}
			})
		}
	}
}
