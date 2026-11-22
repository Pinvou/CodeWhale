package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type registrationBonusRepositoryStub struct{ policy RegistrationBonusPolicy }

func (s registrationBonusRepositoryStub) ActiveRegistrationBonusPolicy(context.Context) (RegistrationBonusPolicy, error) {
	return s.policy, nil
}
func (registrationBonusRepositoryStub) ReplayRegistrationBonusPolicy(context.Context, uuid.UUID, bool, int64, int, int64, string, string) (RegistrationBonusPolicy, bool, error) {
	return RegistrationBonusPolicy{}, false, nil
}
func (registrationBonusRepositoryStub) UpdateRegistrationBonusPolicy(context.Context, uuid.UUID, bool, int64, int, int64, string, string) (RegistrationBonusPolicy, bool, error) {
	return RegistrationBonusPolicy{}, false, nil
}
func (registrationBonusRepositoryStub) ReplayRegistrationBonusProvision(context.Context, RegistrationBonusProvisionRequest) (RegistrationBonusReceipt, bool, error) {
	return RegistrationBonusReceipt{}, false, nil
}
func (registrationBonusRepositoryStub) ProvisionRegistrationBonus(context.Context, RegistrationBonusProvisionRequest) (RegistrationBonusReceipt, bool, error) {
	return RegistrationBonusReceipt{}, false, nil
}

type registrationBonusEligibilityVerifierStub struct {
	readiness RegistrationBonusEligibilityReadiness
}

func (s registrationBonusEligibilityVerifierStub) Readiness(context.Context) (RegistrationBonusEligibilityReadiness, error) {
	return s.readiness, nil
}
func (s registrationBonusEligibilityVerifierStub) RequireReady(context.Context) error {
	if s.readiness.Status != "READY" {
		return ErrRegistrationBonusEligibilityNotReady
	}
	return nil
}

func TestHTTPRegistrationBonusEligibilityVerifierFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{}`},
		{"forbidden", http.StatusForbidden, `{}`},
		{"server error", http.StatusServiceUnavailable, `{}`},
		{"malformed", http.StatusOK, `{`},
		{"unknown field", http.StatusOK, `{"status":"READY","recoverable_registration_count":0,"unrecoverable_registration_count":0,"unexpected":true}`},
		{"oversized", http.StatusOK, `{"status":"READY","recoverable_registration_count":0,"unrecoverable_registration_count":0}` + strings.Repeat(" ", 64<<10)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/internal/v1/registration-bonus-eligibility/readiness" || r.Header.Get("Authorization") != "Bearer readiness-token" {
					t.Fatalf("request path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			verifier, err := NewHTTPRegistrationBonusEligibilityVerifier(server.URL, "readiness-token")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = verifier.Readiness(context.Background()); !errors.Is(err, ErrRegistrationBonusEligibilityUnavailable) {
				t.Fatalf("expected unavailable, got %v", err)
			}
		})
	}
}

func TestHTTPRegistrationBonusEligibilityVerifierStatesAndTimeout(t *testing.T) {
	body := `{"status":"BLOCKED","fingerprint_key_id":"registration-bonus-v1","recoverable_registration_count":12,"unrecoverable_registration_count":2,"completed_at":"2026-09-29T18:31:58.775954Z"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer server.Close()
	verifier, err := NewHTTPRegistrationBonusEligibilityVerifier(server.URL, "readiness-token")
	if err != nil {
		t.Fatal(err)
	}
	readiness, err := verifier.Readiness(context.Background())
	if err != nil || readiness.Status != "BLOCKED" || readiness.FingerprintKeyID != "registration-bonus-v1" || readiness.RecoverableRegistrationCount != 12 || readiness.UnrecoverableRegistrationCount != 2 || readiness.CompletedAt == nil {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	if err = verifier.RequireReady(context.Background()); !errors.Is(err, ErrRegistrationBonusEligibilityNotReady) {
		t.Fatalf("expected not ready, got %v", err)
	}

	timeoutServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer timeoutServer.Close()
	verifier, err = NewHTTPRegistrationBonusEligibilityVerifier(timeoutServer.URL, "readiness-token")
	if err != nil {
		t.Fatal(err)
	}
	verifier.client.Timeout = 10 * time.Millisecond
	if _, err = verifier.Readiness(context.Background()); !errors.Is(err, ErrRegistrationBonusEligibilityUnavailable) {
		t.Fatalf("expected timeout to fail closed, got %v", err)
	}
}

func TestAdminRegistrationBonusPolicyIncludesZeroReadinessCounts(t *testing.T) {
	service := &Service{
		registrationBonuses:          registrationBonusRepositoryStub{policy: RegistrationBonusPolicy{Revision: 1, ValidityDays: 30, UpdatedAt: time.Now().UTC()}},
		registrationBonusEligibility: registrationBonusEligibilityVerifierStub{readiness: RegistrationBonusEligibilityReadiness{Status: "READY"}},
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/registration-bonus-policy", nil)
	service.adminRegistrationBonusPolicy(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"eligibility_status", "recoverable_registration_count", "unrecoverable_registration_count"} {
		if _, ok := body[field]; !ok {
			t.Fatalf("required readiness field %q missing from %s", field, recorder.Body.String())
		}
	}
}
