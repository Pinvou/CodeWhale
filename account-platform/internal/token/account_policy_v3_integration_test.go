package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/Pinvou/pinvou3/account-platform/internal/account"
	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// This is compatibility with a real stored v2 command, not a claim that the
// old binary was executed. Fixture rows are inserted legally; no immutable
// result is rewritten and no database trigger is disabled.
func TestLegacyAccountPolicyV2RecoveryPostgresIntegration(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	for _, key := range []string{"WORKLOAD_IDENTITY_ISSUER", "WORKLOAD_IDENTITY_JWKS_URL", "WORKLOAD_IDENTITY_AUDIENCE", "WORKLOAD_IDENTITY_RULES_JSON"} {
		t.Setenv(key, "")
	}
	accountURL, billingURL, tokenURL := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"), os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"), os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL")
	if accountURL == "" || billingURL == "" || tokenURL == "" {
		t.Skip("three-service PostgreSQL URLs are not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	accountRepository, accountConnection := newThreeServiceAccountRepository(t, ctx, accountURL)
	billingRepository, billingConnection := newThreeServiceBillingRepository(t, ctx, billingURL)
	repository := newThreeServiceTokenRepository(t, ctx, tokenURL)
	actor := seedThreeServiceActor(t, ctx, accountConnection, "policy-v3-operator")
	owner := seedThreeServiceOwner(t, ctx, accountConnection, "policy-v2-owner")
	const serviceToken = "policy-v3-test-service"
	accountServer := httptest.NewServer(accountdomain.NewService(accountRepository, nil).WithServiceToken(serviceToken).Handler(nil))
	defer accountServer.Close()
	billingServer := httptest.NewServer(billingdomain.NewService(billingRepository).Handler(serviceToken))
	defer billingServer.Close()
	authorizer, err := authorization.NewClient(accountServer.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := NewHTTPOwnerAuthorityClient(accountServer.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	wallets, err := NewHTTPWalletResolver(billingServer.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	activation := &threeServiceActivation{current: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 31}}
	repository.WithTokenRouteActivationTransactionGuard(activation)
	verify := func(_ context.Context, bearer string) (identity.Principal, error) {
		if bearer != actor.token {
			return identity.Principal{}, errors.New("unknown test principal")
		}
		return identity.Principal{Issuer: actor.issuer, Subject: actor.subject, SessionID: "policy-version-session", AuthenticatedAt: time.Now().Add(-time.Minute)}, nil
	}
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).WithAuthorizer(authorizer).WithTokenAccountProvisioning(wallets, authority).WithTokenAccountCreateCapability(true).WithTokenCommandGovernanceActivation(activation)
	server := httptest.NewServer(service.Handler("", verify))
	defer server.Close()
	installThreeServicePolicy(t, ctx, repository, 2, 20, "policy version compatibility")
	envelope := threeServicePrecheckedEnvelope(t, ctx, server.URL, actor, owner.id, "Existing policy command")
	status, body, err := threeServiceHTTPJSON(ctx, server.URL, http.MethodPost, "/admin/v1/commands", actor.token, "policy-v3-new-command-key", envelope)
	current := decodeThreeServiceCommand(t, status, body, err)
	if current.PolicyVersion != 3 || current.Status != "PENDING" {
		t.Fatalf("new command must freeze v3: %#v", current)
	}
	legacyKey := "policy-v2-stored-command-key"
	_, requestDigest, keyDigest, fingerprint, err := normalizeTokenCommand(envelope, legacyKey, "")
	if err != nil {
		t.Fatal(err)
	}
	legacyReference := uuid.New()
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Reuse valid, authoritative v3 fixture evidence as the unchanged v2 wire
	// payload. The policy revision was never part of this command fingerprint.
	if _, err = tx.Exec(ctx, `INSERT INTO token_command_results SELECT (jsonb_populate_record(NULL::token_command_results,to_jsonb(r)||jsonb_build_object('command_reference',$2::text,'policy_version',2,'idempotency_key_digest',$3::text,'idempotency_fingerprint',$4::text,'request_digest',$5::text))).* FROM token_command_results r WHERE command_reference=$1`, current.CommandReference, legacyReference.String(), keyDigest, fingerprint, requestDigest); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO token_account_provisioning_reservations SELECT (jsonb_populate_record(NULL::token_account_provisioning_reservations,to_jsonb(r)||jsonb_build_object('command_reference',$2::text,'status','MANUAL_REVIEW','next_attempt_at',transaction_timestamp()))).* FROM token_account_provisioning_reservations r WHERE command_reference=$1`, current.CommandReference, legacyReference.String()); err != nil {
		t.Fatal(err)
	}
	// The independent v3 control remains pending outside the worker window.
	if _, err = tx.Exec(ctx, `UPDATE token_account_provisioning_reservations SET next_attempt_at=transaction_timestamp()+interval '1 day' WHERE command_reference=$1`, current.CommandReference); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, lookup := range []TokenCommandResult{
		threeServiceCommandResult(t, ctx, server.URL, actor, legacyReference),
		threeServiceLookupCommand(t, ctx, server.URL, actor, legacyKey),
	} {
		if lookup.PolicyVersion != 2 || lookup.CommandReference != legacyReference || lookup.Status != "PENDING" {
			t.Fatalf("legacy GET must retain frozen v2: %#v", lookup)
		}
	}
	status, body, err = threeServiceHTTPJSON(ctx, server.URL, http.MethodPost, "/admin/v1/commands", actor.token, legacyKey, envelope)
	replayed := decodeThreeServiceCommand(t, status, body, err)
	if replayed.PolicyVersion != 2 || replayed.CommandReference != legacyReference {
		t.Fatalf("same-payload POST must replay v2: %#v", replayed)
	}
	changed := envelope
	changed.Reason = "different payload using the same historical key"
	status, _, err = threeServiceHTTPJSON(ctx, server.URL, http.MethodPost, "/admin/v1/commands", actor.token, legacyKey, changed)
	if err != nil || status != http.StatusConflict {
		t.Fatalf("changed payload should conflict: %d %v", status, err)
	}
	var commands, reservations, accounts int
	if err = repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_command_results),(SELECT count(*) FROM token_account_provisioning_reservations),(SELECT count(*) FROM token_accounts WHERE owner_id=$1)`, owner.id).Scan(&commands, &reservations, &accounts); err != nil || commands != 2 || reservations != 2 || accounts != 0 {
		t.Fatalf("replay created extra facts: %d/%d/%d %v", commands, reservations, accounts, err)
	}
	manual := threeServiceCommandResult(t, ctx, server.URL, actor, legacyReference)
	if manual.RecoveryEpoch == nil {
		t.Fatal("legacy manual-review epoch is unavailable")
	}
	action := map[string]any{"action": "REQUEUE", "expected_recovery_epoch": *manual.RecoveryEpoch, "reason": "resume a stored v2 command under current policy"}
	status, body, err = threeServiceHTTPJSON(ctx, server.URL, http.MethodPost, "/admin/v1/commands/"+legacyReference.String()+"/recovery-actions", actor.token, "policy-v3-recover-old-command", action)
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("current v3 must permit valid old v2 recovery: %d %s %v", status, body, err)
	}
	var recovered TokenAccountRecoveryActionResult
	if json.Unmarshal(body, &recovered) != nil || recovered.Status != "SUCCEEDED" {
		t.Fatalf("recovery result invalid: %s", body)
	}
	worker, err := NewTokenAccountProvisioningWorker(repository, wallets, authority, "policy-v2-worker", activation.current)
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("legacy command provisioning failed: %t %v", processed, err)
	}
	finished := threeServiceCommandResult(t, ctx, server.URL, actor, legacyReference)
	if finished.PolicyVersion != 2 || finished.Status != "SUCCEEDED" {
		t.Fatalf("completion must preserve v2: %#v", finished)
	}
	// The stored historical fixture intentionally has no initial-submission
	// audit/event. Check each actual recovery/completion fact, not the helper
	// for a command originally submitted through the complete current flow.
	assertLegacyFacts := func() [3]string {
		t.Helper()
		var accountID uuid.UUID
		if err := repository.pool.QueryRow(ctx, `SELECT token_account_id FROM token_account_provisioning_reservations WHERE command_reference=$1 AND status='COMPLETED'`, legacyReference).Scan(&accountID); err != nil {
			t.Fatal(err)
		}
		var accountCount, commandCount, reservationCount, recoveryCount int
		if err := repository.pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM token_accounts WHERE owner_type='USER' AND owner_id=$1),
			(SELECT count(*) FROM token_command_results WHERE command_reference=$2 AND policy_version=2 AND status='SUCCEEDED'),
			(SELECT count(*) FROM token_account_provisioning_reservations WHERE command_reference=$2 AND status='COMPLETED'),
			(SELECT count(*) FROM token_account_provisioning_recovery_actions WHERE command_reference=$2 AND action='REQUEUE' AND status='SUCCEEDED')`, owner.id, legacyReference).Scan(&accountCount, &commandCount, &reservationCount, &recoveryCount); err != nil || accountCount != 1 || commandCount != 1 || reservationCount != 1 || recoveryCount != 1 {
			t.Fatalf("legacy account/command/reservation/recovery facts=%d/%d/%d/%d err=%v", accountCount, commandCount, reservationCount, recoveryCount, err)
		}
		var evidence [3]string
		var audits, createdAudit, completedAudit, recoveryAudit int
		if err := repository.pool.QueryRow(ctx, `SELECT count(*),
			count(*) FILTER (WHERE action='TOKEN_ACCOUNT_CREATED' AND resource_id=$2),
			count(*) FILTER (WHERE action='TOKEN_ACCOUNT_PROVISIONING_COMPLETED' AND resource_id=$2 AND summary->>'token_account_id'=($2::uuid)::text AND summary->>'owner_id'=($3::uuid)::text),
			count(*) FILTER (WHERE action='TOKEN_ACCOUNT_PROVISIONING_RECOVERY' AND resource_id=$1 AND summary->>'action'='REQUEUE' AND summary->>'audit_reference'=($4::uuid)::text AND summary->>'reason'=$5::text),
			COALESCE(jsonb_agg(to_jsonb(a) ORDER BY id),'[]'::jsonb)::text
			FROM token_audit_logs a WHERE summary->>'command_reference'=($1::uuid)::text OR resource_id=$2`, legacyReference, accountID, owner.id, recovered.AuditReference, action["reason"]).Scan(&audits, &createdAudit, &completedAudit, &recoveryAudit, &evidence[0]); err != nil || audits != 3 || createdAudit != 1 || completedAudit != 1 || recoveryAudit != 1 {
			t.Fatalf("legacy audits total/created/completed/recovery=%d/%d/%d/%d err=%v", audits, createdAudit, completedAudit, recoveryAudit, err)
		}
		var events, completedEvent, recoveryEvent int
		if err := repository.pool.QueryRow(ctx, `SELECT count(*),
			count(*) FILTER (WHERE event_type='token.account-provisioning.completed.v1' AND aggregate_id=$2 AND payload->>'command_reference'=($1::uuid)::text AND payload->>'token_account_id'=($2::uuid)::text),
			count(*) FILTER (WHERE event_type='token.account-provisioning.recovery.v1' AND aggregate_id=$1 AND payload->>'action_id'=($3::uuid)::text AND payload->>'action'='REQUEUE' AND payload->>'audit_reference'=($4::uuid)::text),
			COALESCE(jsonb_agg(to_jsonb(e) ORDER BY event_id),'[]'::jsonb)::text
			FROM outbox_events e WHERE payload->>'command_reference'=($1::uuid)::text OR aggregate_id=$1`, legacyReference, accountID, recovered.ActionID, recovered.AuditReference).Scan(&events, &completedEvent, &recoveryEvent, &evidence[1]); err != nil || events != 2 || completedEvent != 1 || recoveryEvent != 1 {
			t.Fatalf("legacy events total/completed/recovery=%d/%d/%d err=%v", events, completedEvent, recoveryEvent, err)
		}
		var receipts, successfulReceipts int
		if err := billingConnection.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE status='SUCCEEDED'),COALESCE(jsonb_agg(to_jsonb(r)),'[]'::jsonb)::text FROM token_account_wallet_provisioning_receipts r WHERE command_reference=$1`, legacyReference).Scan(&receipts, &successfulReceipts, &evidence[2]); err != nil || receipts != 1 || successfulReceipts != 1 {
			t.Fatalf("legacy receipt facts=%d/%d err=%v", receipts, successfulReceipts, err)
		}
		return evidence
	}
	beforeReplay := assertLegacyFacts()
	status, body, err = threeServiceHTTPJSON(ctx, server.URL, http.MethodPost, "/admin/v1/commands", actor.token, legacyKey, envelope)
	finishedReplay := decodeThreeServiceCommand(t, status, body, err)
	if finishedReplay.PolicyVersion != 2 || finishedReplay.CommandReference != legacyReference || finishedReplay.Status != "SUCCEEDED" || strings.TrimSpace(string(finishedReplay.RedactedResult)) != strings.TrimSpace(string(finished.RedactedResult)) {
		t.Fatalf("completed v2 replay changed: %#v", finishedReplay)
	}
	if afterReplay := assertLegacyFacts(); afterReplay != beforeReplay {
		t.Fatal("completed v2 replay changed audit, outbox or Billing receipt evidence")
	}
}
