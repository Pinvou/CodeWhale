package billing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// All calls below target the current production handlers. The pending
// predecessor assembly is exercised separately from this controlled test.
// actual production handlers; controlled repository counters only prove that
// rejected JSON does not reach readiness or persistence methods.
type registrationBonusInputGuardRepository struct {
	registrationBonusRepositoryStub
	calls atomic.Int32
}

func (s *registrationBonusInputGuardRepository) ReplayRegistrationBonusPolicy(context.Context, uuid.UUID, bool, int64, int, int64, string, string) (RegistrationBonusPolicy, bool, error) {
	s.calls.Add(1)
	return RegistrationBonusPolicy{}, false, ErrRegistrationBonusInvalid
}
func (s *registrationBonusInputGuardRepository) UpdateRegistrationBonusPolicy(context.Context, uuid.UUID, bool, int64, int, int64, string, string) (RegistrationBonusPolicy, bool, error) {
	s.calls.Add(1)
	return RegistrationBonusPolicy{}, false, ErrRegistrationBonusInvalid
}
func (s *registrationBonusInputGuardRepository) ReplayRegistrationBonusProvision(context.Context, RegistrationBonusProvisionRequest) (RegistrationBonusReceipt, bool, error) {
	s.calls.Add(1)
	return RegistrationBonusReceipt{}, false, ErrRegistrationBonusInvalid
}
func (s *registrationBonusInputGuardRepository) ProvisionRegistrationBonus(context.Context, RegistrationBonusProvisionRequest) (RegistrationBonusReceipt, bool, error) {
	s.calls.Add(1)
	return RegistrationBonusReceipt{}, false, ErrRegistrationBonusInvalid
}

type registrationBonusInputGuardReadiness struct{ calls atomic.Int32 }

func (s *registrationBonusInputGuardReadiness) Readiness(context.Context) (RegistrationBonusEligibilityReadiness, error) {
	s.calls.Add(1)
	return RegistrationBonusEligibilityReadiness{}, ErrRegistrationBonusEligibilityUnavailable
}
func (s *registrationBonusInputGuardReadiness) RequireReady(context.Context) error {
	s.calls.Add(1)
	return ErrRegistrationBonusEligibilityUnavailable
}

func bodyForEndpoint(input struct{ name, body string }, endpoint string) string {
	if input.name == "oversized" && endpoint == "grant" {
		return strings.Replace(input.body, `"reason":`, `"fingerprint_key_id":`, 1)
	}
	return input.body
}

func TestRegistrationBonusMalformedJSONHasNoSideEffects(t *testing.T) {
	inputs := []struct{ name, body string }{
		{"empty", ""},
		{"malformed", "{"},
		{"wrong-type", `"not an object"`},
		{"unknown-field", `{"unexpected":true}`},
		{"trailing-value", `{} {}`},
		{"oversized", `{"reason":"` + strings.Repeat("x", 1<<20) + `"}`},
	}
	for _, endpoint := range []string{"grant", "policy"} {
		t.Run(endpoint, func(t *testing.T) {
			for _, input := range inputs {
				t.Run(input.name, func(t *testing.T) {
					repository := &registrationBonusInputGuardRepository{}
					readiness := &registrationBonusInputGuardReadiness{}
					service := &Service{registrationBonuses: repository, registrationBonusEligibility: readiness}
					handler := service.internalProvisionRegistrationBonus
					method := http.MethodPost
					if endpoint == "policy" {
						handler = service.adminUpdateRegistrationBonusPolicy
						method = http.MethodPut
					}
					server := httptest.NewServer(http.HandlerFunc(handler))
					defer server.Close()
					client := &http.Client{Timeout: time.Second}
					request, err := http.NewRequest(method, server.URL, strings.NewReader(bodyForEndpoint(input, endpoint)))
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("Content-Type", "application/json")
					response, err := client.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					raw, err := io.ReadAll(io.LimitReader(response.Body, 4096))
					if err != nil {
						t.Fatal(err)
					}
					if response.StatusCode != http.StatusBadRequest {
						t.Fatalf("expected400 got%d", response.StatusCode)
					}
					var body struct {
						Code string `json:"code"`
					}
					if err := json.Unmarshal(raw, &body); err != nil || body.Code != "REGISTRATION_BONUS_INVALID" {
						t.Fatalf("missing typed invalid-input response: code=%q error=%v", body.Code, err)
					}
					if repository.calls.Load() != 0 || readiness.calls.Load() != 0 {
						t.Fatalf("rejected JSON reached persistence/readiness: %d/%d", repository.calls.Load(), readiness.calls.Load())
					}
					if response.Header.Get("Cache-Control") != "no-store" {
						t.Fatal("missing no-store")
					}
				})
			}
		})
	}
}
