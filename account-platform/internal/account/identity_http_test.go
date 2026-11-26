package account

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestSMSOnlyDispatchDistinguishesProvenAbsenceFromUnknownResult(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		status int
		body   string
		proof  bool
	}{
		{"old route absent", http.StatusNotFound, "", true},
		{"workload expired before dispatch", http.StatusUnauthorized, `{"code":"SERVICE_AUTH_REQUIRED"}`, true},
		{"workload not admitted", http.StatusForbidden, `{"code":"SERVICE_AUTH_FORBIDDEN"}`, true},
		{"unrecognized rejection", http.StatusUnauthorized, `{"code":"UNKNOWN"}`, false},
		{"unknown upstream effect", http.StatusServiceUnavailable, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != identity.MobileSignupCreatePath {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				hits.Add(1)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := NewHTTPIdentityProvisioner(server.URL, "file:"+filepath.Join(t.TempDir(), "missing-token"))
			if err != nil {
				t.Fatal(err)
			}
			request := func() error {
				_, err := client.CreateMobileUserFirstAttempt(ctx, uuid.New(), "fixture", "+8613800138000", "", identity.MobileSignupCredentialMode)
				return err
			}
			if err := request(); !errors.Is(err, ErrIdentityCreateNotDispatched) || hits.Load() != 0 {
				t.Fatal("local unreadable workload identity was not recognized as zero dispatch", err)
			}
			client.serviceToken = "controlled-token"
			if err := request(); err == nil || errors.Is(err, ErrIdentityCreateNotDispatched) != test.proof || hits.Load() != 1 {
				t.Fatal("wrong dispatch classification or endpoint", err, hits.Load())
			}
		})
	}
}

func TestIdentityCreateTransportFailurePreservesCauseWithoutProvingNoDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		mode string
	}{
		{"password", mobileIdentityCreatePath, "password-fixture"},
		{"sms only", identity.MobileSignupCreatePath, identity.MobileSignupCredentialMode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dispatched atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != tc.path {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				dispatched.Add(1)
				_ = conn.Close()
			}))
			defer server.Close()
			client, err := NewHTTPIdentityProvisioner(server.URL, "fixture-service-token")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.CreateMobileUserFirstAttempt(context.Background(), uuid.New(), "fixture", "+8613800138000", "", tc.mode)
			if dispatched.Load() != 1 || !errors.Is(err, ErrIdentityCreateUnconfirmed) || !errors.Is(err, io.EOF) || errors.Is(err, ErrIdentityCreateNotDispatched) {
				t.Fatalf("expected one dispatch with an unconfirmed EOF: requests=%d error=%v", dispatched.Load(), err)
			}
			if err.Error() != "identity adapter request failed" {
				t.Fatal("transport diagnostic included more than the public error")
			}
		})
	}
}

func TestProfileRevocationV2IsRejectedByLegacyDecoderBeforeDispatch(t *testing.T) {
	var dispatched atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var legacy struct {
			OperationID  uuid.UUID `json:"operation_id"`
			FirstAttempt *bool     `json:"first_attempt"`
		}
		if api.DecodeJSON(w, request, &legacy) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		dispatched.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := NewHTTPIdentityProvisioner(server.URL, "controlled-profile-token")
	if err != nil {
		t.Fatal(err)
	}
	if err = client.RevokeProfileSessions(context.Background(), "subject", uuid.New(), true); err == nil || dispatched.Load() {
		t.Fatal("new durable grant reached a legacy provider dispatch", err)
	}
}

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
