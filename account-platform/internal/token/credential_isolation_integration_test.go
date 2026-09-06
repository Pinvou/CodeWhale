package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type blockingCredentialValidator struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

type countingCredentialValidator struct{ calls atomic.Int64 }

type credentialAuthorityGuardFunc func(context.Context, pgx.Tx, CredentialIsolationAuthoritySnapshot) error

func (guard credentialAuthorityGuardFunc) LockAndCheckCredentialIsolationAuthority(ctx context.Context, tx pgx.Tx, snapshot CredentialIsolationAuthoritySnapshot) error {
	return guard(ctx, tx, snapshot)
}

func (v *countingCredentialValidator) Validate(context.Context, string, []byte, []string) error {
	v.calls.Add(1)
	return nil
}

func (v *blockingCredentialValidator) Validate(ctx context.Context, _ string, _ []byte, _ []string) error {
	v.calls.Add(1)
	select {
	case v.entered <- struct{}{}:
	default:
	}
	select {
	case <-v.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestCredentialIsolationPostgresIntegration is the no-SKIP CI gate for the
// APT-133 one-time launch, browser binding, rotation and retained history.
func TestCredentialIsolationPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	secrets, err := NewLocalAESProvider("credential-integration-key", bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	repository.secrets = secrets
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository.now = func() time.Time { return now }
	actor, other, accountID, ownerID, walletID, activeID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var currentAuthorizationVersion, currentGateEpoch atomic.Int64
	currentAuthorizationVersion.Store(7)
	currentGateEpoch.Store(11)
	authority := CredentialIsolationAuthoritySnapshot{ActorID: actor, Issuer: "https://id.example/realms/pinvou", Subject: actor.String(), AuthorizationVersion: 7, GateEpoch: 11}
	repository.WithCredentialIsolationAuthorityGuard(credentialAuthorityGuardFunc(func(_ context.Context, _ pgx.Tx, observed CredentialIsolationAuthoritySnapshot) error {
		if observed != authority || observed.AuthorizationVersion != currentAuthorizationVersion.Load() || observed.GateEpoch != currentGateEpoch.Load() {
			return ErrTokenRouteActivationChanged
		}
		return nil
	}))
	ctx = withCredentialIsolationAuthority(ctx, authority)
	if _, err = repository.pool.Exec(ctx, `
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Credential integration','ACTIVE',$4,$5,$5,'PLATFORM');
INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$5);
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status,activated_at) VALUES($6,$1,'NEW_API','https://provider.example','old'::bytea,'old-key','old-fingerprint',1,'ACTIVE',ARRAY['model-a'],$5,$4,$5,'VALID',$5);`, pgx.QueryExecModeSimpleProtocol, accountID, ownerID, walletID, actor, now, activeID); err != nil {
		t.Fatal(err)
	}
	safeDetail, err := repository.AdminTokenAccountSafeDetail(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	safeJSON, err := json.Marshal(safeDetail)
	if err != nil {
		t.Fatal(err)
	}
	if safeDetail.ID != accountID || safeDetail.AsOf.IsZero() || safeDetail.ActiveCredentialCount != 1 || safeDetail.RouteCount != 0 {
		t.Fatalf("unexpected safe account detail: %#v", safeDetail)
	}
	for _, forbidden := range []string{"credentials", "routes", "https://provider.example", "old-key", "old-fingerprint"} {
		if bytes.Contains(safeJSON, []byte(forbidden)) {
			t.Fatalf("safe account detail leaked inline credential or route fact %q: %s", forbidden, safeJSON)
		}
	}
	var authorizationVersion atomic.Int64
	authorizationVersion.Store(1)
	handler := newAdminQueryIntegrationHandler(t, repository, actor, &authorizationVersion, map[string]bool{"platform.tokens.read": true})
	response := performAdminQueryRequest(handler, "/admin/v2/token-accounts/"+accountID.String())
	var safeHTTP AdminTokenAccountSafeDetail
	if decodeErr := json.Unmarshal(response.Body.Bytes(), &safeHTTP); response.Code != http.StatusOK || decodeErr != nil || safeHTTP.ID != accountID || safeHTTP.AsOf.IsZero() {
		t.Fatalf("safe detail handler status=%d body=%s projection=%#v err=%v", response.Code, response.Body.String(), safeHTTP, decodeErr)
	}
	for _, forbidden := range []string{"credentials", "routes", "https://provider.example", "old-key", "old-fingerprint"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("safe detail HTTP leaked inline credential or route fact %q: %s", forbidden, response.Body.String())
		}
	}

	input := CredentialLaunchRequest{Action: "ROTATE", ExpectedAccountVersion: 1, ExpectedCredentialVersion: 1, Slot: "PRIMARY", Reason: "rotate the active integration credential"}
	launch, err := repository.CreateCredentialLaunch(ctx, accountID, actor, input, "credential-integration-idempotency", "credential-integration-request")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := repository.CreateCredentialLaunch(ctx, accountID, actor, input, "credential-integration-idempotency", "another-request-id")
	if err != nil || replayed.LaunchID != launch.LaunchID || replayed.ResultRef != launch.ResultRef {
		t.Fatalf("launch replay=%#v err=%v", replayed, err)
	}
	if _, err = repository.CredentialIsolationResult(ctx, other, launch.ResultRef); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-actor result err=%v", err)
	}
	session, err := repository.ConsumeCredentialLaunch(ctx, launch.LaunchID, "https://credentials-uat.account.pinvou.com", strings.Repeat("b", 64), strings.Repeat("s", 64), strings.Repeat("n", 64), strings.Repeat("v", 64), strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	var storedAuthorizationVersion, storedGateEpoch int64
	if err = repository.pool.QueryRow(ctx, `SELECT authorization_version,gate_epoch FROM credential_isolation_sessions WHERE id=$1`, session.ID).Scan(&storedAuthorizationVersion, &storedGateEpoch); err != nil || storedAuthorizationVersion != authority.AuthorizationVersion || storedGateEpoch != authority.GateEpoch {
		t.Fatalf("session authority authorization_version=%d gate_epoch=%d err=%v", storedAuthorizationVersion, storedGateEpoch, err)
	}
	if _, err = repository.ConsumeCredentialLaunch(ctx, launch.LaunchID, "https://credentials-uat.account.pinvou.com", strings.Repeat("b", 64), strings.Repeat("s", 64), strings.Repeat("n", 64), strings.Repeat("v", 64), strings.Repeat("c", 64)); !errors.Is(err, ErrCredentialReplay) {
		t.Fatalf("replayed launch err=%v", err)
	}
	if _, err = repository.ConfirmCredentialSession(ctx, session.ID, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if err = repository.AuthenticateCredentialSession(ctx, session.ID, other, now.Add(-time.Minute)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-subject authentication was accepted: %v", err)
	}
	// Only the first heartbeat is subject to the ten-second window. OIDC may
	// take longer while the five-minute launch/session TTL remains authoritative.
	now = now.Add(2 * time.Minute)
	if err = repository.AuthenticateCredentialSession(ctx, session.ID, actor, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = repository.RevokeCredentialLaunch(ctx, actor, launch.ResultRef, "CREDENTIAL_WINDOW_BLOCKED"); !errors.Is(err, ErrCredentialReplay) {
		t.Fatalf("confirmed window was revoked: %v", err)
	}
	policy, err := NewProviderURLPolicy("test", []string{"provider.example"})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("isolated-integration-secret")
	result, err := repository.CompleteCredentialSession(ctx, session.ID, "https://credentials-uat.account.pinvou.com", strings.Repeat("b", 64), strings.Repeat("c", 64), "NEW_API", "https://provider.example", []string{"model-a"}, secret, acceptingCredentialValidator{}, policy)
	if err != nil || result.Status != "SUCCEEDED" || result.CredentialID == nil || result.CredentialVersion == nil || *result.CredentialVersion != 2 {
		t.Fatalf("completion=%#v err=%v", result, err)
	}

	// A launch freezes both Account authorization and governance epoch. Any
	// change is rejected at the next locked transition and cannot create a
	// credential, even if the Portal-side revoke response is lost.
	changedAccount, changedWallet := uuid.New(), uuid.New()
	if _, err = repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Changed authority','ACTIVE',$4,$5,$5,'PLATFORM'); INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$5)`, pgx.QueryExecModeSimpleProtocol, changedAccount, ownerID, changedWallet, actor, now); err != nil {
		t.Fatal(err)
	}
	changedLaunch, err := repository.CreateCredentialLaunch(ctx, changedAccount, actor, CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 1, Slot: "PRIMARY", Reason: "freeze authorization before isolated credential intake"}, "changed-authority-launch-key", "changed-authority-request")
	if err != nil {
		t.Fatal(err)
	}
	currentAuthorizationVersion.Store(8)
	if _, err = repository.ConsumeCredentialLaunch(ctx, changedLaunch.LaunchID, "https://credentials-uat.account.pinvou.com", strings.Repeat("x", 64), strings.Repeat("y", 64), strings.Repeat("z", 64), strings.Repeat("w", 64), strings.Repeat("c", 64)); !errors.Is(err, ErrTokenRouteActivationChanged) {
		t.Fatalf("authorization drift reached credential intake: %v", err)
	}
	var changedStatus string
	var changedCredentialCount int
	if err = repository.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM provider_credentials WHERE token_account_id=$1) FROM credential_launches WHERE id=$2`, changedAccount, changedLaunch.LaunchID).Scan(&changedStatus, &changedCredentialCount); err != nil || changedStatus != "CREATED" || changedCredentialCount != 0 {
		t.Fatalf("authorization drift status=%s credentials=%d err=%v", changedStatus, changedCredentialCount, err)
	}
	currentAuthorizationVersion.Store(7)
	currentGateEpoch.Store(12)
	if _, err = repository.ConsumeCredentialLaunch(ctx, changedLaunch.LaunchID, "https://credentials-uat.account.pinvou.com", strings.Repeat("x", 64), strings.Repeat("y", 64), strings.Repeat("z", 64), strings.Repeat("w", 64), strings.Repeat("c", 64)); !errors.Is(err, ErrTokenRouteActivationChanged) {
		t.Fatalf("governance epoch drift reached credential intake: %v", err)
	}
	currentGateEpoch.Store(11)
	finalAccount, finalWallet := uuid.New(), uuid.New()
	if _, err = repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Final authority race','ACTIVE',$4,$5,$5,'PLATFORM'); INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$5)`, pgx.QueryExecModeSimpleProtocol, finalAccount, ownerID, finalWallet, actor, now); err != nil {
		t.Fatal(err)
	}
	finalLaunch, err := repository.CreateCredentialLaunch(ctx, finalAccount, actor, CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 1, Slot: "PRIMARY", Reason: "detect authorization drift before final credential commit"}, "final-authority-launch-key", "final-authority-request")
	if err != nil {
		t.Fatal(err)
	}
	finalSession, err := repository.ConsumeCredentialLaunch(ctx, finalLaunch.LaunchID, "https://credentials-uat.account.pinvou.com", strings.Repeat("L", 64), strings.Repeat("S", 64), strings.Repeat("N", 64), strings.Repeat("V", 64), strings.Repeat("C", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ConfirmCredentialSession(ctx, finalSession.ID, strings.Repeat("L", 64)); err != nil {
		t.Fatal(err)
	}
	if err = repository.AuthenticateCredentialSession(ctx, finalSession.ID, actor, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	finalValidator := &blockingCredentialValidator{entered: make(chan struct{}, 1), release: make(chan struct{})}
	finalError := make(chan error, 1)
	go func() {
		_, completionErr := repository.CompleteCredentialSession(ctx, finalSession.ID, "https://credentials-uat.account.pinvou.com", strings.Repeat("L", 64), strings.Repeat("C", 64), "NEW_API", "https://provider.example", []string{"model-a"}, []byte("must-not-be-stored"), finalValidator, policy)
		finalError <- completionErr
	}()
	select {
	case <-finalValidator.entered:
	case completionErr := <-finalError:
		t.Fatalf("completion returned before provider validation: %v", completionErr)
	case <-ctx.Done():
		t.Fatalf("waiting for completion validation: %v", ctx.Err())
	}
	currentAuthorizationVersion.Store(8)
	close(finalValidator.release)
	select {
	case err = <-finalError:
	case <-ctx.Done():
		t.Fatalf("waiting for completion result: %v", ctx.Err())
	}
	if !errors.Is(err, ErrTokenRouteActivationChanged) {
		t.Fatalf("authorization drift during validation reached final credential commit: %v", err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM provider_credentials WHERE token_account_id=$1`, finalAccount).Scan(&changedCredentialCount); err != nil || changedCredentialCount != 0 {
		t.Fatalf("final authorization race credentials=%d err=%v", changedCredentialCount, err)
	}
	currentAuthorizationVersion.Store(7)
	completedSession, err := repository.ConfirmCredentialSession(ctx, session.ID, strings.Repeat("b", 64))
	if err != nil || completedSession.Status != "COMPLETED" || completedSession.Verifier != "" {
		t.Fatalf("completed session refresh=%#v err=%v", completedSession, err)
	}
	if len(secret) != 0 && bytes.Contains(secret, []byte("isolated")) {
		t.Fatal("caller secret buffer was not cleared")
	}
	var accountVersion int64
	var oldStatus, newStatus string
	if err = repository.pool.QueryRow(ctx, `SELECT account.management_version,old.status,new.status FROM token_accounts account JOIN provider_credentials old ON old.id=$2 JOIN provider_credentials new ON new.id=$3 WHERE account.id=$1`, accountID, activeID, *result.CredentialID).Scan(&accountVersion, &oldStatus, &newStatus); err != nil {
		t.Fatal(err)
	}
	if accountVersion != 2 || oldStatus != "RETIRED" || newStatus != "ACTIVE" {
		t.Fatalf("version=%d old=%s new=%s", accountVersion, oldStatus, newStatus)
	}
	commandReference := uuid.Must(uuid.NewV7())
	if _, err = repository.pool.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,status,before_state,audit_request_id) VALUES($1,'TOKEN_CREDENTIAL_VALIDATE',2,$2,'platform.tokens.credentials.validate',3,$3,$4,$5,'PROVIDER_CREDENTIAL',$6,'validate the isolated credential',$7,$8,'PENDING','{}','credential-validation-request')`, commandReference, actor, strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), result.CredentialID.String(), []byte(`{"expected_account_version":2,"credential_version":2}`), strings.Repeat("4", 64)); err != nil {
		t.Fatal(err)
	}
	validator := &blockingCredentialValidator{entered: make(chan struct{}, 1), release: make(chan struct{})}
	validationService := &Service{repository: repository, validator: validator}
	validationResults := make(chan TokenCommandResult, 2)
	validationErrors := make(chan error, 2)
	var concurrent sync.WaitGroup
	envelope := TokenCommandEnvelope{CommandType: "TOKEN_CREDENTIAL_VALIDATE", Reason: "validate the isolated credential", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: result.CredentialID.String(), ExpectedVersions: json.RawMessage(`{"expected_account_version":2,"credential_version":2}`), PrecheckFingerprint: strings.Repeat("4", 64), Command: json.RawMessage(`{"token_account_id":"` + accountID.String() + `","credential_id":"` + result.CredentialID.String() + `"}`)}
	concurrent.Add(1)
	go func() {
		defer concurrent.Done()
		item, validationErr := validationService.executeGovernedCredentialValidation(ctx, repository, commandReference, actor, envelope)
		validationResults <- item
		validationErrors <- validationErr
	}()
	select {
	case <-validator.entered:
	case validationErr := <-validationErrors:
		t.Fatalf("credential probe returned before provider validation: %v", validationErr)
	case <-ctx.Done():
		t.Fatalf("waiting for credential probe: %v", ctx.Err())
	}
	concurrent.Add(1)
	secondValidationFinished := make(chan struct{})
	go func() {
		defer concurrent.Done()
		defer close(secondValidationFinished)
		item, validationErr := validationService.executeGovernedCredentialValidation(ctx, repository, commandReference, actor, envelope)
		validationResults <- item
		validationErrors <- validationErr
	}()
	select {
	case <-secondValidationFinished:
	case <-ctx.Done():
		t.Fatalf("waiting for duplicate credential claim: %v", ctx.Err())
	}
	close(validator.release)
	concurrent.Wait()
	close(validationResults)
	close(validationErrors)
	claimedCount := 0
	for validationErr := range validationErrors {
		if errors.Is(validationErr, ErrCredentialProbeClaimed) {
			claimedCount++
		} else if validationErr != nil {
			t.Fatalf("concurrent validation error: %v", validationErr)
		}
	}
	statuses := map[string]int{}
	for item := range validationResults {
		if item.CommandReference != commandReference || (item.Status != "SUCCEEDED" && item.Status != "PENDING") {
			t.Fatalf("concurrent validation result=%#v", item)
		}
		statuses[item.Status]++
	}
	if validator.calls.Load() != 1 || claimedCount != 1 || statuses["SUCCEEDED"] != 1 || statuses["PENDING"] != 1 {
		t.Fatalf("credential validation probes=%d claimed=%d statuses=%v", validator.calls.Load(), claimedCount, statuses)
	}
	// If the process loses the provider response after claiming the durable
	// lease, the same command reference becomes a stable fail-closed terminal
	// result after the lease. It must never issue a second provider probe.
	crashedReference := uuid.Must(uuid.NewV7())
	claimTime := now.Add(-time.Minute)
	if _, err = repository.pool.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,status,before_state,audit_request_id,credential_probe_claimed_at,credential_probe_lease_until) VALUES($1,'TOKEN_CREDENTIAL_VALIDATE',2,$2,'platform.tokens.credentials.validate',3,$3,$4,$5,'PROVIDER_CREDENTIAL',$6,'recover a lost validation response',$7,$8,'PENDING','{}','credential-validation-crash',$9::timestamptz,$9::timestamptz+interval '30 seconds')`, crashedReference, actor, strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), result.CredentialID.String(), []byte(`{"expected_account_version":2,"credential_version":2}`), strings.Repeat("d", 64), claimTime); err != nil {
		t.Fatal(err)
	}
	crashValidator := &countingCredentialValidator{}
	crashService := &Service{repository: repository, validator: crashValidator}
	crashed, crashErr := crashService.executeGovernedCredentialValidation(ctx, repository, crashedReference, actor, envelope)
	if crashErr != nil || crashed.Status != "FAILED" || crashed.StableErrorCode == nil || *crashed.StableErrorCode != "CREDENTIAL_PROBE_OUTCOME_UNKNOWN" || crashValidator.calls.Load() != 0 {
		t.Fatalf("crash recovery result=%#v err=%v probes=%d", crashed, crashErr, crashValidator.calls.Load())
	}
	var validationAuditCount int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_CREDENTIAL_VALIDATED' AND resource_type='PROVIDER_CREDENTIAL' AND resource_id=$1`, *result.CredentialID).Scan(&validationAuditCount); err != nil || validationAuditCount != 1 {
		t.Fatalf("validation audit count=%d err=%v", validationAuditCount, err)
	}
	var auditText string
	if err = repository.pool.QueryRow(ctx, `SELECT COALESCE(string_agg(summary::text,' '),'') FROM token_audit_logs WHERE resource_id IN ($1,$2)`, accountID, *result.CredentialID).Scan(&auditText); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"isolated-integration-secret", "https://provider.example", "old-key"} {
		if strings.Contains(auditText, private) {
			t.Fatalf("private credential fact leaked to audit: %s", private)
		}
	}

	// The production credential adapter consumes the exact Account-frozen 11+0
	// snapshot and commits credential/account versions, audit, outbox and result
	// atomically. A later APT-138 consumer may activate this path, but this test
	// seam does not change the production PRE_ACTIVATION gate.
	evidence, err := repository.TokenCredentialImpact(ctx, accountID, result.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	critical, err := credentialRevokeCriticalFromEvidence(accountID, *result.CredentialID, evidence)
	if err != nil {
		t.Fatal(err)
	}
	expectedJSON, _ := json.Marshal(critical)
	revokeEnvelope := TokenCommandEnvelope{CommandType: "TOKEN_CREDENTIAL_REVOKE", Reason: "revoke the superseded provider credential", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: result.CredentialID.String(), ExpectedVersions: expectedJSON, PrecheckFingerprint: strings.Repeat("9", 64), Command: json.RawMessage(`{"token_account_id":"` + accountID.String() + `","expected_account_version":2,"credential_id":"` + result.CredentialID.String() + `","credential_version":2}`)}
	approvalID := uuid.New()
	executor := NewPostgresApprovedCredentialExecutor(repository)
	var governanceRowsBefore int
	if err = repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_command_results)+(SELECT count(*) FROM token_audit_logs)+(SELECT count(*) FROM outbox_events)`).Scan(&governanceRowsBefore); err != nil {
		t.Fatal(err)
	}
	for name, financial := range map[string]json.RawMessage{"missing": nil, "null": json.RawMessage(`null`), "array": json.RawMessage(`[]`), "extra": json.RawMessage(`{"unexpected":true}`)} {
		t.Run("revoke financial "+name, func(t *testing.T) {
			body := approvedTokenCommandBody{ActivationEpoch: 1, ApprovalRequestID: approvalID, RequesterUserID: actor, AuthorizationVersion: 3, CommandFingerprint: strings.Repeat("8", 64), ExecutionIdempotencyKey: "approval:" + approvalID.String() + ":TOKEN_CREDENTIAL_REVOKE:" + strings.Repeat("8", 64), CriticalResourceVersions: expectedJSON, FinancialSnapshot: financial, Envelope: revokeEnvelope}
			raw, marshalErr := json.Marshal(body)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			executorContext, contextErr := serviceauth.ContextForJob(ctx, "token-command-executor", "APPROVAL_EXECUTION", uuid.NewString())
			if contextErr != nil {
				t.Fatal(contextErr)
			}
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/commands/credential-revoke/execute", bytes.NewReader(raw)).WithContext(executorContext)
			response := httptest.NewRecorder()
			service := &Service{repository: repository, approvedCommandExecutor: executor}
			approvedCommandFixtureHandler(service, request.URL.Path, service.executeApprovedCredentialRevoke).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_APPROVED_COMMAND") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var governanceRowsAfter int
			if queryErr := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_command_results)+(SELECT count(*) FROM token_audit_logs)+(SELECT count(*) FROM outbox_events)`).Scan(&governanceRowsAfter); queryErr != nil || governanceRowsAfter != governanceRowsBefore {
				t.Fatalf("invalid financial snapshot changed governance rows before=%d after=%d err=%v", governanceRowsBefore, governanceRowsAfter, queryErr)
			}
		})
	}
	revokeCommand, _, err := repository.BeginTokenCommand(ctx, tokenCommandBegin{Envelope: revokeEnvelope, ActorUserID: actor, ActorPermission: "platform.tokens.credentials.revoke.request", AuthorizationVersion: 3, IdempotencyKey: "approval:" + approvalID.String() + ":TOKEN_CREDENTIAL_REVOKE:" + strings.Repeat("8", 64), AuditRequestID: "credential-revoke-request", ApprovalRequestID: approvalID, ApprovalFingerprint: strings.Repeat("8", 64), BeforeState: expectedJSON})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = executor.PrecheckApprovedTokenCommand(ctx, "TOKEN_CREDENTIAL_REVOKE", revokeEnvelope); err != nil {
		t.Fatal(err)
	}
	revokeContext := context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}, transactional: true})
	for _, rejectCall := range []int{1, 2} {
		calls := 0
		repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error {
			calls++
			if calls == rejectCall {
				return ErrTokenRouteActivationChanged
			}
			return nil
		}))
		if _, err := executor.ExecuteApprovedCredentialRevoke(revokeContext, revokeCommand.CommandReference, actor, revokeEnvelope); !errors.Is(err, ErrTokenRouteActivationChanged) || calls != rejectCall {
			t.Fatalf("revoke activation guard %d calls=%d err=%v", rejectCall, calls, err)
		}
		var credentialStatus, commandStatus string
		var accountVersion, effects int64
		if err := repository.pool.QueryRow(ctx, `SELECT credential.status,account.management_version,result.status,(SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_CREDENTIAL_REVOKED' AND resource_id=$2)+(SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_CREDENTIAL_REVOKE' AND summary->>'command_reference'=($3::uuid)::text)+(SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1' AND payload->>'command_reference'=($3::uuid)::text) FROM provider_credentials credential JOIN token_accounts account ON account.id=credential.token_account_id JOIN token_command_results result ON result.command_reference=$3 WHERE account.id=$1 AND credential.id=$2`, accountID, *result.CredentialID, revokeCommand.CommandReference).Scan(&credentialStatus, &accountVersion, &commandStatus, &effects); err != nil || credentialStatus != "ACTIVE" || accountVersion != 2 || commandStatus != "PENDING" || effects != 0 {
			t.Fatalf("revoke rejected guard changed facts: credential=%s version=%d command=%s effects=%d err=%v", credentialStatus, accountVersion, commandStatus, effects, err)
		}
	}
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error { return nil }))
	revoked, err := executor.ExecuteApprovedCredentialRevoke(revokeContext, revokeCommand.CommandReference, actor, revokeEnvelope)
	if err != nil || revoked.Status != "SUCCEEDED" {
		t.Fatalf("credential revoke result=%#v err=%v", revoked, err)
	}
	var revokedStatus string
	var revokedAccountVersion int64
	var revokeAuditCount, revokeGenericAuditCount, revokeOutboxCount int
	if err = repository.pool.QueryRow(ctx, `SELECT credential.status,account.management_version,(SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_CREDENTIAL_REVOKED' AND resource_id=$2),(SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_CREDENTIAL_REVOKE' AND summary->>'command_reference'=($3::uuid)::text),(SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1' AND payload->>'command_reference'=($3::uuid)::text) FROM provider_credentials credential JOIN token_accounts account ON account.id=credential.token_account_id WHERE credential.id=$2 AND account.id=$1`, accountID, *result.CredentialID, revokeCommand.CommandReference).Scan(&revokedStatus, &revokedAccountVersion, &revokeAuditCount, &revokeGenericAuditCount, &revokeOutboxCount); err != nil {
		t.Fatal(err)
	}
	if revokedStatus != "REVOKED" || revokedAccountVersion != 3 || revokeAuditCount != 1 || revokeGenericAuditCount != 1 || revokeOutboxCount != 1 {
		t.Fatalf("revoke status=%s account=%d audit=%d genericAudit=%d outbox=%d", revokedStatus, revokedAccountVersion, revokeAuditCount, revokeGenericAuditCount, revokeOutboxCount)
	}
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error {
		t.Error("terminal revoke replay must remain read-only without entering the write guard")
		return ErrTokenRouteActivationChanged
	}))
	if replayed, err := executor.ExecuteApprovedCredentialRevoke(revokeContext, revokeCommand.CommandReference, actor, revokeEnvelope); err != nil || replayed.CommandReference != revoked.CommandReference || replayed.Status != "SUCCEEDED" {
		t.Fatalf("terminal revoke replay=%+v err=%v", replayed, err)
	}
	repository.WithTokenRouteActivationTransactionGuard(nil)
	var rotationStageCount int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs WHERE resource_id IN ($1,$2) AND action IN ('TOKEN_CREDENTIAL_CANDIDATE_CREATED','TOKEN_CREDENTIAL_RETIRING','TOKEN_CREDENTIAL_ACTIVATED','TOKEN_CREDENTIAL_RETIRED')`, activeID, *result.CredentialID).Scan(&rotationStageCount); err != nil || rotationStageCount != 4 {
		t.Fatalf("rotation stage audit count=%d err=%v", rotationStageCount, err)
	}

	// Revocation leaves immutable history. A later ENROLL allocates MAX(all
	// history)+1 rather than reusing the missing ACTIVE version.
	reenroll, err := repository.CreateCredentialLaunch(ctx, accountID, actor, CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 3, ExpectedCredentialVersion: 0, Slot: "PRIMARY", Reason: "reenroll after the governed credential revocation"}, "credential-reenroll-idempotency", "credential-reenroll-request")
	if err != nil {
		t.Fatal(err)
	}
	reenrollSession, err := repository.ConsumeCredentialLaunch(ctx, reenroll.LaunchID, "https://credentials-uat.account.pinvou.com", strings.Repeat("q", 64), strings.Repeat("r", 64), strings.Repeat("s", 64), strings.Repeat("t", 64), strings.Repeat("u", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ConfirmCredentialSession(ctx, reenrollSession.ID, strings.Repeat("q", 64)); err != nil {
		t.Fatal(err)
	}
	if err = repository.AuthenticateCredentialSession(ctx, reenrollSession.ID, actor, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	reenrollSecret := []byte("reenrolled-integration-secret")
	reenrolled, err := repository.CompleteCredentialSession(ctx, reenrollSession.ID, "https://credentials-uat.account.pinvou.com", strings.Repeat("q", 64), strings.Repeat("u", 64), "NEW_API", "https://provider.example", []string{"model-a"}, reenrollSecret, acceptingCredentialValidator{}, policy)
	if err != nil || reenrolled.CredentialVersion == nil || *reenrolled.CredentialVersion != 3 {
		t.Fatalf("reenroll=%#v err=%v", reenrolled, err)
	}
	var activeCount int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM provider_credentials WHERE token_account_id=$1 AND status='ACTIVE'`, accountID).Scan(&activeCount); err != nil || activeCount != 1 {
		t.Fatalf("active credentials after reenroll=%d err=%v", activeCount, err)
	}

	secondAccount, secondWallet := uuid.New(), uuid.New()
	if _, err = repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Blocked window','ACTIVE',$4,$5,$5,'PLATFORM'); INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$5)`, pgx.QueryExecModeSimpleProtocol, secondAccount, ownerID, secondWallet, actor, now); err != nil {
		t.Fatal(err)
	}
	blocked, err := repository.CreateCredentialLaunch(ctx, secondAccount, actor, CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 1, ExpectedCredentialVersion: 0, Slot: "PRIMARY", Reason: "test blocked credential window"}, "blocked-window-idempotency-key", "blocked-window-request")
	if err != nil {
		t.Fatal(err)
	}
	blockedSession, err := repository.ConsumeCredentialLaunch(ctx, blocked.LaunchID, "https://credentials-uat.account.pinvou.com", strings.Repeat("d", 64), strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("g", 64), strings.Repeat("h", 64))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Second)
	if _, err = repository.ConfirmCredentialSession(ctx, blockedSession.ID, strings.Repeat("d", 64)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("late first heartbeat was accepted: %v", err)
	}
	if err = repository.RevokeCredentialLaunch(ctx, actor, blocked.ResultRef, "CREDENTIAL_WINDOW_BLOCKED"); err != nil {
		t.Fatal(err)
	}
	blockedResult, err := repository.CredentialIsolationResult(ctx, actor, blocked.ResultRef)
	if err != nil || blockedResult.Status != "REVOKED" || blockedResult.StableErrorCode == nil || *blockedResult.StableErrorCode != "CREDENTIAL_WINDOW_BLOCKED" {
		t.Fatalf("blocked result=%#v err=%v", blockedResult, err)
	}
	var sessionStatus string
	if err = repository.pool.QueryRow(ctx, `SELECT status FROM credential_isolation_sessions WHERE id=$1`, blockedSession.ID).Scan(&sessionStatus); err != nil || sessionStatus != "REVOKED" {
		t.Fatalf("blocked session=%s err=%v", sessionStatus, err)
	}

	raceAccount, raceWallet := uuid.New(), uuid.New()
	if _, err = repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Callback race','ACTIVE',$4,$5,$5,'PLATFORM'); INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$5)`, pgx.QueryExecModeSimpleProtocol, raceAccount, ownerID, raceWallet, actor, now); err != nil {
		t.Fatal(err)
	}
	raceLaunch, err := repository.CreateCredentialLaunch(ctx, raceAccount, actor, CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 1, ExpectedCredentialVersion: 0, Slot: "PRIMARY", Reason: "test callback and Portal revoke serialization"}, "callback-race-idempotency", "callback-race-request")
	if err != nil {
		t.Fatal(err)
	}
	raceSession, err := repository.ConsumeCredentialLaunch(ctx, raceLaunch.LaunchID, "https://credentials-uat.account.pinvou.com", strings.Repeat("i", 64), strings.Repeat("j", 64), strings.Repeat("k", 64), strings.Repeat("l", 64), strings.Repeat("m", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ConfirmCredentialSession(ctx, raceSession.ID, strings.Repeat("i", 64)); err != nil {
		t.Fatal(err)
	}
	if err = repository.RevokeCredentialLaunch(ctx, actor, raceLaunch.ResultRef, "CREDENTIAL_PERMISSION_REVOKED"); err != nil {
		t.Fatal(err)
	}
	if err = repository.AuthenticateCredentialSession(ctx, raceSession.ID, actor, now.Add(-time.Minute)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("OIDC callback won after Portal revocation: %v", err)
	}

	concurrentAccount, concurrentWallet := uuid.New(), uuid.New()
	if _, err = repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Concurrent enroll','ACTIVE',$4,$5,$5,'PLATFORM'); INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$5)`, pgx.QueryExecModeSimpleProtocol, concurrentAccount, ownerID, concurrentWallet, actor, now); err != nil {
		t.Fatal(err)
	}
	prepareConcurrent := func(key, binding, state, nonce, verifier, csrf string) credentialIsolationSession {
		launch, launchErr := repository.CreateCredentialLaunch(ctx, concurrentAccount, actor, CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 1, ExpectedCredentialVersion: 0, Slot: "PRIMARY", Reason: "concurrent enrollment must preserve one active credential"}, key, key+"-request")
		if launchErr != nil {
			t.Fatal(launchErr)
		}
		session, consumeErr := repository.ConsumeCredentialLaunch(ctx, launch.LaunchID, "https://credentials-uat.account.pinvou.com", binding, state, nonce, verifier, csrf)
		if consumeErr != nil {
			t.Fatal(consumeErr)
		}
		if _, confirmErr := repository.ConfirmCredentialSession(ctx, session.ID, binding); confirmErr != nil {
			t.Fatal(confirmErr)
		}
		if authErr := repository.AuthenticateCredentialSession(ctx, session.ID, actor, now.Add(-time.Minute)); authErr != nil {
			t.Fatal(authErr)
		}
		return session
	}
	concurrentA := prepareConcurrent("concurrent-enroll-key-a", strings.Repeat("A", 64), strings.Repeat("B", 64), strings.Repeat("C", 64), strings.Repeat("D", 64), strings.Repeat("E", 64))
	concurrentB := prepareConcurrent("concurrent-enroll-key-b", strings.Repeat("F", 64), strings.Repeat("G", 64), strings.Repeat("H", 64), strings.Repeat("I", 64), strings.Repeat("J", 64))
	completionErrors := make(chan error, 2)
	for _, candidate := range []struct {
		session credentialIsolationSession
		binding string
		csrf    string
		secret  string
	}{{concurrentA, strings.Repeat("A", 64), strings.Repeat("E", 64), "concurrent-secret-one"}, {concurrentB, strings.Repeat("F", 64), strings.Repeat("J", 64), "concurrent-secret-two"}} {
		concurrent.Add(1)
		go func(candidate struct {
			session credentialIsolationSession
			binding string
			csrf    string
			secret  string
		}) {
			defer concurrent.Done()
			_, completionErr := repository.CompleteCredentialSession(ctx, candidate.session.ID, "https://credentials-uat.account.pinvou.com", candidate.binding, candidate.csrf, "NEW_API", "https://provider.example", []string{"model-a"}, []byte(candidate.secret), acceptingCredentialValidator{}, policy)
			completionErrors <- completionErr
		}(candidate)
	}
	concurrent.Wait()
	close(completionErrors)
	successes, conflicts := 0, 0
	for completionErr := range completionErrors {
		if completionErr == nil {
			successes++
		} else if errors.Is(completionErr, ErrCatalogVersionConflict) || strings.Contains(completionErr.Error(), "could not serialize") {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent completion error: %v", completionErr)
		}
	}
	var concurrentVersion int64
	if err = repository.pool.QueryRow(ctx, `SELECT management_version,(SELECT count(*) FROM provider_credentials WHERE token_account_id=$1 AND status='ACTIVE') FROM token_accounts WHERE id=$1`, concurrentAccount).Scan(&concurrentVersion, &activeCount); err != nil || successes != 1 || conflicts != 1 || concurrentVersion != 2 || activeCount != 1 {
		t.Fatalf("concurrent enroll success=%d conflict=%d version=%d active=%d err=%v", successes, conflicts, concurrentVersion, activeCount, err)
	}

	down, err := os.ReadFile("../../migrations/token/000026_credential_isolation.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "credential isolation history must be retained") {
		t.Fatalf("destructive rollback was not refused: %v", err)
	}
}
