package token

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestCredentialWindowRevocationOwnerPostgres(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	owner, other, account := uuid.New(), uuid.New(), uuid.New()
	if _, err := r.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'controlled account','ACTIVE',100,$2,now(),now(),'PLATFORM')`, account, owner, uuid.New()); err != nil {
		t.Fatal(err)
	}
	authority := CredentialIsolationAuthoritySnapshot{ActorID: owner, Issuer: "https://controlled.example.test", Subject: owner.String(), AuthorizationVersion: 1, GateEpoch: 1}
	r.WithCredentialIsolationAuthorityGuard(credentialAuthorityGuardFunc(func(_ context.Context, _ pgx.Tx, snapshot CredentialIsolationAuthoritySnapshot) error {
		if snapshot != authority {
			return ErrForbidden
		}
		return nil
	}))
	launch, err := r.CreateCredentialLaunch(withCredentialIsolationAuthority(ctx, authority), account, owner,
		CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 1, Slot: "PRIMARY", Reason: "controlled window"}, uuid.NewString(), "controlled-window")
	if err != nil {
		t.Fatal(err)
	}
	window, err := r.ConsumeCredentialLaunch(ctx, launch.LaunchID, "https://credentials.example.test",
		strings.Repeat("b", 64), strings.Repeat("s", 64), strings.Repeat("n", 64), strings.Repeat("v", 64), strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	verify := func(_ context.Context, bearer string) (identity.Principal, error) {
		switch bearer {
		case "controlled-window-owner":
			return identity.Principal{Issuer: authority.Issuer, Subject: owner.String()}, nil
		case "controlled-other-actor":
			return identity.Principal{Issuer: authority.Issuer, Subject: other.String()}, nil
		default:
			return identity.Principal{}, errors.New("unrecognized fixture bearer")
		}
	}
	s := &Service{repository: r, managementEnabled: true, managementVerify: verify}
	handler := s.Handler(verify)
	revoke := func(bearer string) int {
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/credential-results/"+launch.ResultRef.String()+"/revoke-window",
			strings.NewReader(`{"reason_code":"CREDENTIAL_FEATURE_DISABLED"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+bearer)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	check := func(wantLaunch, wantResult, wantWindow string, cleared bool) {
		t.Helper()
		var launchStatus, resultStatus, windowStatus string
		var verifierCleared bool
		err := r.pool.QueryRow(ctx, `SELECT l.status,r.status,s.status,octet_length(s.pkce_verifier_ciphertext)=0 AND s.key_id=''
FROM credential_launches l JOIN credential_isolation_results r USING(result_ref)
JOIN credential_isolation_sessions s ON s.launch_id=l.id WHERE l.result_ref=$1 AND s.id=$2`, launch.ResultRef, window.ID).
			Scan(&launchStatus, &resultStatus, &windowStatus, &verifierCleared)
		if err != nil || launchStatus != wantLaunch || resultStatus != wantResult || windowStatus != wantWindow || verifierCleared != cleared {
			t.Fatalf("launch/result/window state=%s/%s/%s verifierCleared=%v err=%v", launchStatus, resultStatus, windowStatus, verifierCleared, err)
		}
	}
	if status := revoke("controlled-invalid-bearer"); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated revocation status=%d", status)
	}
	if status := revoke("controlled-other-actor"); status < 400 {
		t.Fatalf("another authenticated actor revoked the window: status=%d", status)
	}
	check("CONSUMED", "PENDING", "AUTHENTICATING", false)
	if status := revoke("controlled-window-owner"); status != http.StatusNoContent {
		t.Fatalf("owner could not revoke the window after feature disable: status=%d", status)
	}
	check("REVOKED", "REVOKED", "REVOKED", true)
}
