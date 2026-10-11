package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type repairAuditRepository struct {
	personalBoundaryRepository
	operation                                 RegistrationOperation
	actions                                   []string
	requestedFail, terminalFail, postReadFail bool
	reads, effects, prepares                  int
	businessErr                               error
}

func (r *repairAuditRepository) RegistrationRepairQueue(context.Context, RegistrationRepairFilter) ([]RegistrationRepairItem, error) {
	return nil, nil
}
func (r *repairAuditRepository) RegistrationOperation(context.Context, uuid.UUID) (RegistrationOperation, error) {
	r.reads++
	if r.postReadFail && r.reads > 1 {
		return RegistrationOperation{}, errors.New("controlled post-effect read failure")
	}
	return r.operation, nil
}
func (r *repairAuditRepository) RetryWalletProvisioning(context.Context, identity.Principal, uuid.UUID) error {
	r.effects++
	if r.businessErr == nil {
		r.operation.Status = "PROVISIONING"
	}
	return r.businessErr
}
func (r *repairAuditRepository) PrepareRegistrationCompensation(context.Context, identity.Principal, uuid.UUID) (RegistrationOperation, error) {
	r.prepares++
	return RegistrationOperation{}, ErrRegistrationCompensationForbidden
}
func (r *repairAuditRepository) CompleteRegistrationCompensation(context.Context, identity.Principal, uuid.UUID) error {
	return errors.New("unexpected compensation completion")
}
func (r *repairAuditRepository) AuditRegistrationRepair(_ context.Context, _ identity.Principal, _ uuid.UUID, action, result string) error {
	r.actions = append(r.actions, action+":"+result)
	if r.requestedFail && strings.HasSuffix(action, ".requested") || r.terminalFail && !strings.HasSuffix(action, ".requested") {
		return errors.New("controlled audit storage failure")
	}
	return nil
}

func TestRegistrationRepairAuditDoesNotRedispatchOrInventResult(t *testing.T) {
	for _, name := range []string{"requested-failure", "terminal-failure-known-success", "terminal-failure-business-error", "post-effect-read-failure", "compensate-requested-failure"} {
		t.Run(name, func(t *testing.T) {
			user, id := uuid.New(), uuid.New()
			repo := &repairAuditRepository{operation: RegistrationOperation{ID: id, UserID: &user, Status: "REPAIR_REQUIRED"}}
			repo.requestedFail = name == "requested-failure" || name == "compensate-requested-failure"
			repo.terminalFail = strings.HasPrefix(name, "terminal-failure")
			repo.postReadFail = name == "post-effect-read-failure"
			if name == "terminal-failure-business-error" {
				repo.businessErr = ErrRegistrationConflict
			}
			service := &Service{repository: repo, mobileAuth: &MobileAuthService{}}
			action := http.HandlerFunc(service.adminRetryRegistration)
			if name == "compensate-requested-failure" {
				action = service.adminCompensateRegistration
			}
			handler := identity.Middleware(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "controlled", Subject: "subject", SessionID: "session"}, nil
			}, action)
			req := httptest.NewRequest(http.MethodPost, "/controlled", nil)
			req.SetPathValue("operation_id", id.String())
			req.Header.Set("Authorization", "Bearer controlled")
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			if repo.requestedFail {
				if repo.effects != 0 || repo.prepares != 0 || len(repo.actions) != 1 || out.Code < 500 {
					t.Fatal("failed requested audit permitted effects", out.Code, repo.actions)
				}
				return
			}
			if repo.effects != 1 {
				t.Fatal("business effect redispatched", repo.effects)
			}
			if repo.actions[0] != "registration.repair.retry.requested:SUCCEEDED" {
				t.Fatal("requested fact missing", repo.actions)
			}
			if repo.terminalFail && out.Header().Get("X-Registration-Repair-Audit") != "pending" {
				t.Fatal("terminal audit loss silently ignored")
			}
			if name == "terminal-failure-known-success" && (out.Code != http.StatusAccepted || !strings.Contains(out.Body.String(), "PROVISIONING")) {
				t.Fatal("known success converted to failure or invented result", out.Code, out.Body.String())
			}
			if name == "post-effect-read-failure" && (out.Code != http.StatusServiceUnavailable || !strings.Contains(out.Body.String(), "REGISTRATION_REPAIR_RESULT_UNKNOWN") || strings.Contains(out.Body.String(), `"operation_id"`)) {
				t.Fatal("failed read returned a fake operation", out.Code, out.Body.String())
			}
			if repo.businessErr != nil && out.Code == http.StatusAccepted {
				t.Fatal("business failure disguised as success")
			}
		})
	}
}
