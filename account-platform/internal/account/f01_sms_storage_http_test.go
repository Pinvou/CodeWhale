package account

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type f01SMSHTTPRepository struct {
	*mobileAuthRepositoryFake
	admissionErr, consumeErr error
}

func (r *f01SMSHTTPRepository) AdmitSMSVerification(context.Context, uuid.UUID, string, SMSPurpose, string, string) error {
	return r.admissionErr
}

func (r *f01SMSHTTPRepository) ConsumeSMSChallenge(ctx context.Context, id uuid.UUID, mobile string, purpose SMSPurpose, verified bool) error {
	if r.consumeErr != nil {
		return r.consumeErr
	}
	return r.mobileAuthRepositoryFake.ConsumeSMSChallenge(ctx, id, mobile, purpose, verified)
}

// The HTTP/service chain is real; only its external repository/provider ports
// are controlled. Actual PostgreSQL faults are exercised in the sibling suite.
func TestF01SMSLoginHTTPPreservesFailureClassification(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(observability.NewLogger(&logs, observability.Config{Service: "account-service", Environment: "test", Version: "dev", Commit: "controlled"}))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	fault := &pgconn.PgError{Code: "P0001", Message: "controlled confidential database detail"}
	for _, scenario := range []struct {
		name, code, retry string
		status            int
	}{
		{"admission-storage", "AUTH_INTERNAL_ERROR", "", 500},
		{"identity-storage", "AUTH_INTERNAL_ERROR", "", 500},
		{"ready-storage", "AUTH_INTERNAL_ERROR", "", 500},
		{"consume-storage", "AUTH_INTERNAL_ERROR", "", 500},
		{"invalid-code", "SMS_CODE_INVALID", "", 401},
		{"rate-limit", "SMS_RATE_LIMITED", "43", 429},
		{"provider-unavailable", "SMS_UNAVAILABLE", "", 503},
		{"success", "", "", 204},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			logs.Reset()
			repo := &f01SMSHTTPRepository{mobileAuthRepositoryFake: &mobileAuthRepositoryFake{identitySubject: "controlled-subject"}}
			sms := &smsProviderFake{verified: true}
			switch scenario.name {
			case "admission-storage":
				repo.admissionErr = fault
			case "identity-storage":
				repo.identityErr = fault
			case "ready-storage":
				repo.readyErr = fault
			case "consume-storage":
				repo.consumeErr = fault
			case "invalid-code":
				sms.verified = false
			case "rate-limit":
				repo.admissionErr = &SMSRateLimitError{RetryAfter: 43 * time.Second}
			case "provider-unavailable":
				sms.verifyErr = fault
			}
			auth := newMobileAuthForTest(t, repo, sms, &identityProvisionerFake{})
			service := NewService(nil, nil).WithMobileAuth(auth, "internal-service-token-for-tests-123456")
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/auth/sms/verify", strings.NewReader(`{"mobile":"13800138000","challenge_id":"018f0f76-3f5d-7d43-b630-163e3f635e5a","code":"123456"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer internal-service-token-for-tests-123456")
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			body := response.Body.String()
			if response.Code != scenario.status || scenario.code != "" && !strings.Contains(body, `"code":"`+scenario.code+`"`) || response.Header().Get("Retry-After") != scenario.retry {
				t.Fatalf("failure classification changed: status=%d body=%s retry=%s", response.Code, body, response.Header().Get("Retry-After"))
			}
			if strings.Contains(body, fault.Message) || strings.Contains(body, fault.Code) {
				t.Fatal("private storage details escaped through the HTTP response")
			}
			if strings.Contains(logs.String(), fault.Message) || strings.Contains(logs.String(), fault.Code) {
				t.Fatal("private storage details escaped through authentication logging")
			}
			if scenario.status >= 500 && logs.Len() == 0 {
				t.Fatal("infrastructure failure did not reach the production logging pipeline")
			}
			if scenario.name == "success" {
				if response.Header().Get("X-Pinvou-Identity-Subject") != "controlled-subject" || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("successful identity response changed")
				}
			} else if response.Header().Get("X-Pinvou-Identity-Subject") != "" {
				t.Fatal("failed verification returned an identity")
			}
		})
	}
}
