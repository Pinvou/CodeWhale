package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type tokenApprovalEffectStub struct {
	observed bool
	at       time.Time
	calls    int
}

func (stub *tokenApprovalEffectStub) ObserveApprovalEffect(_ context.Context, _ uuid.UUID, _ string) (bool, time.Time, error) {
	stub.calls++
	return stub.observed, stub.at, nil
}

func TestTokenApprovalEffectObservationIsClosedReadOnlyAuthority(t *testing.T) {
	requestID := uuid.Must(uuid.NewV7())
	fingerprint := strings.Repeat("b", 64)
	reader := &tokenApprovalEffectStub{at: time.Now().UTC()}
	service := (&Service{}).WithApprovalEffectReader(reader)
	ctx, err := serviceauth.ContextForJob(context.Background(), "account-service", "APPROVAL_RECONCILIATION", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/approval-effects/"+requestID.String()+"?command_type=TOKEN_CREDENTIAL_REVOKE&command_fingerprint="+fingerprint, nil).WithContext(ctx)
	request.SetPathValue("approval_request_id", requestID.String())
	response := httptest.NewRecorder()
	service.approvalEffectObservation(response, request)
	var observation approvaleffect.Observation
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &observation) != nil || observation.Validate() != nil ||
		observation.State != approvaleffect.StateAbsent || observation.ApprovalRequestID != requestID || reader.calls != 1 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("closed absent observation failed: status=%d calls=%d body=%s", response.Code, reader.calls, response.Body.String())
	}
	reader.observed = true
	response = httptest.NewRecorder()
	service.approvalEffectObservation(response, request)
	if json.Unmarshal(response.Body.Bytes(), &observation) != nil || observation.State != approvaleffect.StateObserved {
		t.Fatalf("observed effect was not conservative: %s", response.Body.String())
	}
	invalid := httptest.NewRequest(http.MethodGet, "/?command_type=WALLET_ADJUSTMENT&command_fingerprint="+fingerprint, nil).WithContext(ctx)
	invalid.SetPathValue("approval_request_id", requestID.String())
	response = httptest.NewRecorder()
	service.approvalEffectObservation(response, invalid)
	if response.Code != http.StatusBadRequest || reader.calls != 2 {
		t.Fatal("cross-domain command reached Token authority")
	}
	recovery, _ := serviceauth.ContextForJob(context.Background(), "account-approval-recovery", "APPROVAL_RECOVERY_QUERY", uuid.NewString())
	recoveryRequest := request.Clone(recovery)
	recoveryResponse := httptest.NewRecorder()
	service.approvalEffectObservation(recoveryResponse, recoveryRequest)
	if recoveryResponse.Code != http.StatusOK || reader.calls != 3 || recoveryResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("dedicated recovery identity cannot query Token authority: status=%d calls=%d", recoveryResponse.Code, reader.calls)
	}
	other, _ := serviceauth.ContextForJob(context.Background(), "token-command-executor", "APPROVAL_EXECUTION", uuid.NewString())
	denied := request.Clone(other)
	denied.SetPathValue("approval_request_id", requestID.String())
	response = httptest.NewRecorder()
	service.approvalEffectObservation(response, denied)
	if response.Code != http.StatusForbidden || reader.calls != 3 {
		t.Fatal("non-Account workload reached Token effect authority")
	}
}

func TestTokenApprovalEffectObservationPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	requestID := uuid.Must(uuid.NewV7())
	commandReference := uuid.Must(uuid.NewV7())
	fingerprint := strings.Repeat("c", 64)
	digest := strings.Repeat("d", 64)
	if _, err := repository.pool.Exec(ctx, `INSERT INTO token_command_results(
command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,
idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,
reason,expected_versions,precheck_fingerprint,approval_request_id,approval_command_fingerprint,status,audit_request_id)
VALUES($1,'TOKEN_CREDENTIAL_REVOKE',1,$2,'admin.token-accounts.manage',1,$3,$3,$3,
'TOKEN_ACCOUNT',$4,'approval effect integration fixture','{}'::jsonb,$3,$5,$6,'PENDING',$7)`,
		commandReference, uuid.New(), digest, uuid.NewString(), requestID, fingerprint, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	observed, observedAt, err := repository.ObserveApprovalEffect(ctx, requestID, "TOKEN_CREDENTIAL_REVOKE")
	if err != nil || !observed || observedAt.IsZero() || observedAt.Location() != time.UTC {
		t.Fatalf("expected authoritative observed result: observed=%v at=%v err=%v", observed, observedAt, err)
	}
	absent, absentAt, err := repository.ObserveApprovalEffect(ctx, uuid.New(), "TOKEN_CREDENTIAL_REVOKE")
	if err != nil || absent || absentAt.IsZero() || absentAt.Location() != time.UTC {
		t.Fatalf("expected authoritative absent result: observed=%v at=%v err=%v", absent, absentAt, err)
	}
	if _, _, err = repository.ObserveApprovalEffect(ctx, requestID, "WALLET_ADJUSTMENT"); err == nil {
		t.Fatal("cross-domain command was accepted by Token authority")
	}
}
