package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type registrationResponseRepository struct {
	*PostgresRepository
	operation  RegistrationOperation
	readCalls  int
	retryCalls int
	readError  error
	retryError error
}

func (r *registrationResponseRepository) RegistrationOperation(context.Context, uuid.UUID) (RegistrationOperation, error) {
	r.readCalls++
	if r.readCalls > 1 && r.readError != nil {
		return RegistrationOperation{}, r.readError
	}
	return r.operation, nil
}

func (r *registrationResponseRepository) RetryWalletProvisioning(context.Context, identity.Principal, uuid.UUID) error {
	r.retryCalls++
	if r.retryError != nil {
		return r.retryError
	}
	r.operation.Status = "PROVISIONING"
	return nil
}

func (r *registrationResponseRepository) PrepareRegistrationCompensation(context.Context, identity.Principal, uuid.UUID) (RegistrationOperation, error) {
	return RegistrationOperation{}, ErrRegistrationCompensationForbidden
}

func TestPostgresRegistrationRepairResponseRequiresAuditAndRead(t *testing.T) {
	for _, scenario := range []string{"retry_audit_error", "retry_read_error", "retry_domain_error", "retry_already_started", "retry_confirmed", "compensate_audit_error", "compensate_domain_error"} {
		t.Run(scenario, func(t *testing.T) {
			pg := accountDatabase(t)
			principal := fixturePlatformAdmin(t, pg)
			userID := uuid.New()
			repository := &registrationResponseRepository{PostgresRepository: pg, operation: RegistrationOperation{
				ID: uuid.New(), UserID: &userID, Username: "controlled-registration", Status: "REPAIR_REQUIRED", NextRetryAt: time.Now().UTC(),
			}}
			if scenario == "retry_read_error" {
				repository.readError = errors.New("controlled operation read unavailable")
			}
			if scenario == "retry_domain_error" {
				repository.retryError = ErrRegistrationConflict
			}
			if scenario == "retry_already_started" {
				repository.operation.Status = "PROVISIONING"
			}
			if strings.Contains(scenario, "audit_error") {
				// Permission evaluation and audit persistence use the real owned
				// database. Only the retry domain result/read is controlled here.
				accountExec(t, pg, `CREATE FUNCTION reject_registration_repair_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action IN ('registration.repair.retry','registration.repair.compensate') THEN RAISE EXCEPTION 'controlled audit unavailable'; END IF; RETURN NEW; END $$; CREATE TRIGGER registration_repair_audit_failure BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_registration_repair_audit()`)
			}
			action := "retry"
			if strings.HasPrefix(scenario, "compensate") {
				action = "compensate"
			}
			service := NewService(repository, nil).WithMobileAuth(&MobileAuthService{}, "controlled-internal-token")
			handler := service.Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
			request := httptest.NewRequest(http.MethodPost, "/admin/v1/registration-operations/"+repository.operation.ID.String()+"/actions/"+action, nil)
			request.Header.Set("Authorization", "Bearer controlled-fixture")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var audits int
			if err := pg.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE resource_id=$1 AND action=$2`, repository.operation.ID.String(), "registration.repair."+action).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if scenario == "retry_confirmed" {
				var result RegistrationResult
				if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.OperationID != repository.operation.ID || result.Status != "PROVISIONING" || result.NextRetryAt.IsZero() || audits != 1 || repository.retryCalls != 1 {
					t.Fatal("confirmed repair did not return its authoritative operation and audit")
				}
				return
			}
			expectedStatus := http.StatusInternalServerError
			if scenario == "retry_domain_error" || scenario == "retry_already_started" || scenario == "compensate_domain_error" {
				expectedStatus = http.StatusConflict
			}
			if response.Code != expectedStatus || strings.Contains(response.Body.String(), "registration_operation_id") || strings.Contains(response.Body.String(), "controlled operation") || strings.Contains(response.Body.String(), "controlled audit") {
				t.Fatal("unconfirmed repair exposed success, a fabricated registration DTO, or private error")
			}
			if scenario == "retry_domain_error" || scenario == "retry_already_started" {
				var result struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Code != "REGISTRATION_IDEMPOTENCY_CONFLICT" {
					t.Fatal("registration conflict did not preserve the existing public error code")
				}
			}
			if strings.Contains(scenario, "audit_error") && audits != 0 || !strings.Contains(scenario, "audit_error") && audits != 1 {
				t.Fatal("repair audit persistence did not match the controlled transaction outcome")
			}
			if action == "retry" && scenario != "retry_already_started" && repository.retryCalls != 1 || scenario == "retry_already_started" && repository.retryCalls != 0 {
				t.Fatal("a response failure retried the domain action")
			}
			if scenario == "retry_read_error" && repository.operation.Status != "PROVISIONING" {
				t.Fatal("a response read error reversed a known completed retry")
			}
		})
	}
}

type compensatedResponseRepository struct {
	*mobileAuthRepositoryFake
	readError error
}

func (r *compensatedResponseRepository) RegistrationOperation(ctx context.Context, operationID uuid.UUID) (RegistrationOperation, error) {
	if r.readError != nil {
		return RegistrationOperation{}, r.readError
	}
	return r.mobileAuthRepositoryFake.RegistrationOperation(ctx, operationID)
}

type neverDispatchedResponseIdentity struct{ identityProvisionerFake }

func (p *neverDispatchedResponseIdentity) CreateMobileUser(context.Context, uuid.UUID, string, string, string, string, bool, bool) (RegistrationIdentity, error) {
	p.createCalls++
	return RegistrationIdentity{}, ErrIdentityCreateNotDispatched
}

func TestRegistrationCompensatedResponseRequiresSuccessfulRead(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "read_unavailable"}[unavailable], func(t *testing.T) {
			operation := RegistrationOperation{ID: uuid.New(), Status: "REGISTRATION_IN_PROGRESS", Username: "controlled-registration", Mobile: "+8613800000000", NextRetryAt: time.Now().UTC()}
			repository := &compensatedResponseRepository{mobileAuthRepositoryFake: &mobileAuthRepositoryFake{operation: operation}}
			readError := errors.New("controlled compensation read unavailable")
			if unavailable {
				repository.readError = readError
			}
			identity := &neverDispatchedResponseIdentity{}
			auth := newMobileAuthForTest(t, repository, &smsProviderFake{}, identity)
			operation.IssuerDigest = registrationIssuerDigest(auth.issuer)
			repository.operation = operation
			result, err := auth.resumeRegistration(context.Background(), operation)
			if identity.createCalls != 1 || repository.operation.Status != "COMPENSATED" {
				t.Fatal("compensation response handling reopened or reversed a completed operation")
			}
			if unavailable {
				if !errors.Is(err, readError) || errors.Is(err, ErrRegistrationRestart) || result.OperationID != uuid.Nil || result.Status != "" {
					t.Fatal("failed compensation read fabricated a confirmed restart response")
				}
			} else if !errors.Is(err, ErrRegistrationRestart) || result.OperationID != operation.ID || result.Status != "COMPENSATED" {
				t.Fatal("confirmed compensation lost its durable operation identity")
			}
		})
	}
}
