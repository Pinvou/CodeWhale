package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type recoveryTokenRepositoryStub struct {
	Repository
	state string
	calls int
}

func (stub *recoveryTokenRepositoryStub) ObserveTokenApprovalResult(_ context.Context, _ uuid.UUID, _, _, _ string) (string, time.Time, error) {
	stub.calls++
	return stub.state, time.Now().UTC(), nil
}

func TestTokenRecoveryResultRequiresDedicatedIdentityAndOriginalDigest(t *testing.T) {
	requestID := uuid.New()
	fingerprint, digest := strings.Repeat("a", 64), strings.Repeat("b", 64)
	stub := &recoveryTokenRepositoryStub{state: "SUCCEEDED"}
	service := &Service{repository: stub}
	base := "/internal/v1/approval-recovery/results/" + requestID.String() + "?command_type=TOKEN_CREDENTIAL_REVOKE&command_fingerprint=" + fingerprint + "&original_key_digest=" + digest
	for _, tc := range []struct {
		name, identity, rawURL string
		status, calls          int
	}{
		{"dedicated", "account-approval-recovery", base, http.StatusOK, 1},
		{"executor refused", "token-command-executor", base, http.StatusForbidden, 1},
		{"duplicate digest", "account-approval-recovery", base + "&original_key_digest=" + digest, http.StatusBadRequest, 1},
		{"wrong digest", "account-approval-recovery", strings.Replace(base, digest, "invalid", 1), http.StatusBadRequest, 1},
		{"cross domain", "account-approval-recovery", strings.Replace(base, "TOKEN_CREDENTIAL_REVOKE", "WALLET_ADJUSTMENT", 1), http.StatusBadRequest, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := serviceauth.ContextForJob(context.Background(), tc.identity, "APPROVAL_RECOVERY_QUERY", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, tc.rawURL, nil).WithContext(ctx)
			request.SetPathValue("approval_request_id", requestID.String())
			response := httptest.NewRecorder()
			service.tokenApprovalRecoveryResult(response, request)
			if response.Code != tc.status || stub.calls != tc.calls || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, stub.calls, response.Body.String())
			}
			if response.Code == http.StatusOK {
				var result tokenRecoveryResult
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.State != "SUCCEEDED" || result.OriginalKeyDigest != digest {
					t.Fatalf("invalid result: %s", response.Body.String())
				}
			}
		})
	}
}

func TestTokenRecoveryResultBindsOriginalIdempotencyDigestIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	requestID, commandReference := uuid.New(), uuid.New()
	fingerprint, digest := strings.Repeat("a", 64), strings.Repeat("b", 64)
	_, err := repository.pool.Exec(ctx, `INSERT INTO token_command_results(
command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,
idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,
reason,expected_versions,precheck_fingerprint,approval_request_id,approval_command_fingerprint,status,audit_request_id)
VALUES($1,'TOKEN_CREDENTIAL_REVOKE',1,$2,'admin.token-accounts.manage',1,$3,$3,$3,
'TOKEN_ACCOUNT',$4,'approval recovery result fixture','{}'::jsonb,$3,$5,$6,'PENDING',$7)`,
		commandReference, uuid.New(), digest, uuid.NewString(), requestID, fingerprint, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	state, observedAt, err := repository.ObserveTokenApprovalResult(ctx, requestID, "TOKEN_CREDENTIAL_REVOKE", fingerprint, digest)
	if err != nil || state != "PENDING" || observedAt.IsZero() {
		t.Fatalf("pending result not found: %s %v", state, err)
	}
	state, _, err = repository.ObserveTokenApprovalResult(ctx, requestID, "TOKEN_CREDENTIAL_REVOKE", fingerprint, strings.Repeat("c", 64))
	if err != nil || state != "UNCONFIRMED" {
		t.Fatalf("wrong original key matched: %s %v", state, err)
	}
	_, err = repository.pool.Exec(ctx, `UPDATE token_command_results SET status='SUCCEEDED',completed_at=transaction_timestamp() WHERE command_reference=$1`, commandReference)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = repository.ObserveTokenApprovalResult(ctx, requestID, "TOKEN_CREDENTIAL_REVOKE", fingerprint, digest)
	if err != nil || state != "SUCCEEDED" {
		t.Fatalf("terminal result not found: %s %v", state, err)
	}
}
