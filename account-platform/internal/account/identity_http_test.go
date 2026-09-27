package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestIdentityProvisionerPreservesPasswordPolicyErrors(t *testing.T) {
	tests := []struct {
		code string
		want error
	}{
		{code: "PASSWORD_INVALID", want: ErrPasswordInvalid},
		{code: "PASSWORD_EDGE_SPACE_NOT_ALLOWED", want: ErrPasswordEdgeSpace},
	}
	for _, item := range tests {
		t.Run(item.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":"` + item.code + `"}`))
			}))
			defer server.Close()
			provisioner, err := NewHTTPIdentityProvisioner(server.URL, "test-service-token")
			if err != nil {
				t.Fatal(err)
			}
			err = provisioner.ResetPassword(context.Background(), "subject", uuid.New(), 1, "+8613800138000", "handle", "user-v1")
			if !errors.Is(err, item.want) {
				t.Fatalf("error=%v, want %v", err, item.want)
			}
		})
	}
}
