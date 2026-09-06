package token

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokenauthority"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type modelAccountWalletResolver struct {
	walletID uuid.UUID
	calls    atomic.Int32
}

type modelAccountProvisioningClient struct {
	walletID uuid.UUID
	calls    atomic.Int32
	gets     atomic.Int32
	postErr  bool
	receipt  WalletProvisioningReceipt
}

func (c *modelAccountProvisioningClient) ProvisionTokenAccountWallet(_ context.Context, reference uuid.UUID, ownerType string, ownerID uuid.UUID, digest string) (WalletProvisioningReceipt, error) {
	c.calls.Add(1)
	now := time.Now().UTC()
	c.receipt = WalletProvisioningReceipt{CommandReference: reference, OwnerType: ownerType, OwnerID: ownerID, RequestDigest: digest, Status: "SUCCEEDED", WalletID: &c.walletID, AttemptCount: 1, CreatedAt: now, CompletedAt: &now}
	if c.postErr {
		return WalletProvisioningReceipt{}, context.DeadlineExceeded
	}
	return c.receipt, nil
}

func (c *modelAccountProvisioningClient) TokenAccountWalletProvisioningReceipt(_ context.Context, reference uuid.UUID) (WalletProvisioningReceipt, error) {
	c.gets.Add(1)
	if c.receipt.CommandReference != reference {
		return WalletProvisioningReceipt{}, ErrNotFound
	}
	return c.receipt, nil
}

type modelAccountAuthorityClient struct {
	version *atomic.Int64
}

// Observation time changes on every Billing read even when the governed
// financial facts do not. Real Token snapshots independently use database time.
type rollbackAdvancingPricing struct {
	result ImpactPriceCoverageResult
	reads  int64
}

func (pricing *rollbackAdvancingPricing) ImpactPriceCoverage(context.Context, []ImpactPriceQuery) (ImpactPriceCoverageResult, error) {
	pricing.reads++
	result := pricing.result
	result.AsOf = result.AsOf.Add(time.Duration(pricing.reads) * time.Millisecond)
	return result, nil
}

func (client modelAccountAuthorityClient) CheckTokenOwnerAuthority(_ context.Context, input OwnerAuthorityCheck) (OwnerAuthorityDecision, error) {
	actor := uuid.New()
	if input.ActorUserID != nil {
		actor, _ = uuid.Parse(*input.ActorUserID)
	} else if input.IdentitySubject != nil {
		actor, _ = uuid.Parse(*input.IdentitySubject)
	}
	platformVersion, actorVersion, ownerVersion := "11", "11", "11"
	if client.version != nil {
		platformVersion = strconv.FormatInt(client.version.Load(), 10)
	}
	if input.ExpectedPlatformAuthorizationVersion != nil {
		platformVersion = *input.ExpectedPlatformAuthorizationVersion
	}
	if input.ExpectedActorVersion != nil {
		actorVersion = *input.ExpectedActorVersion
	}
	if input.ExpectedOwnerVersion != nil {
		ownerVersion = *input.ExpectedOwnerVersion
	}
	actorVersionNumber, _ := strconv.ParseInt(actorVersion, 10, 64)
	result := OwnerAuthorityDecision{AuthoritySchemaVersion: tokenauthority.SchemaVersion, Mode: input.Mode, Decision: "ALLOW", ActorUserID: actor.String(), ExpectedPlatformAuthorizationVersion: input.ExpectedPlatformAuthorizationVersion, ObservedPlatformAuthorizationVersion: platformVersion, ExpectedActorVersion: input.ExpectedActorVersion, ObservedActorVersion: actorVersion, ActorStatus: "ACTIVE", ActorIdentityDigest: tokenauthority.ActorIdentityDigest(actor, "ACTIVE", actorVersionNumber), ExpectedOwnerVersion: input.ExpectedOwnerVersion, CommandReference: input.CommandReference, CommandRequestDigest: input.CommandRequestDigest, AuthorityRequestSHA256: input.AuthorityRequestSHA256, ObservedAt: time.Now().UTC()}
	result.ExpectedActorIdentityDigest = input.ExpectedActorIdentityDigest
	result.ExpectedOwnerIdentityDigest = input.ExpectedOwnerIdentityDigest
	if input.OwnerID != nil {
		ownerID, _ := uuid.Parse(*input.OwnerID)
		ownerVersionNumber, _ := strconv.ParseInt(ownerVersion, 10, 64)
		status := "ACTIVE"
		digest := tokenauthority.OwnerIdentityDigest(*input.OwnerType, ownerID, status, ownerVersionNumber)
		result.ObservedOwnerVersion, result.OwnerStatus, result.OwnerIdentityDigest = &ownerVersion, &status, &digest
	}
	return result, nil
}

type mutableModelAccountOwnerDirectory struct {
	item    AdminOwnerCandidate
	version *atomic.Int64
}

func (directory mutableModelAccountOwnerDirectory) Scan(_ context.Context, _ identity.Principal, ownerType string, _ *uuid.UUID, consume func([]AdminOwnerCandidate) error) error {
	if ownerType != "" && ownerType != directory.item.Type {
		return consume(nil)
	}
	item := directory.item
	item.AuthorizationVersion = directory.version.Load()
	item.OwnerVersion = directory.version.Load()
	return consume([]AdminOwnerCandidate{item})
}

// TestCatalogRollbackGovernancePostgresIntegration exercises the exact
// approved envelope that APT-138 will eventually consume. It intentionally
// calls the installed internal HTTP handler without registering a consumer or
// changing the production PRE_ACTIVATION default.
func TestCatalogRollbackGovernancePostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error { return nil }))
	actorID, modelID, accountID, ownerID, walletID, credentialID, routeID, provisioningID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Add(-time.Minute)
	var protocolID uuid.UUID
	if err := repository.pool.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE protocol_code='CHAT' AND version=1 AND status='PUBLISHED'`).Scan(&protocolID); err != nil {
		t.Fatal(err)
	}
	var sourceVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'rollback source',$2,$3,$3) RETURNING version`, uuid.New(), actorID, now).Scan(&sourceVersion); err != nil {
		t.Fatal(err)
	}
	capabilityID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at)
VALUES($1,'integration','v1','rollback-model',$2,$3,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at)
VALUES($5,$1,1,$6,4096,1024,'["text"]','v1',$3,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available)
VALUES($7,$1,'Rollback Source','rollback source','','{}','PUBLISHED',$5,$6,true);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode)
VALUES($8,'USER',$9,$10,'Rollback Account','ACTIVE',$3,$4,$4,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status)
VALUES($11,$8,'integration','https://provider.invalid','x'::bytea,'integration-key',$2,1,'ACTIVE',ARRAY['rollback-model'],$4,$3,$4,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,model_id,protocol_version_id,reference_status,route_version,route_generation,health_version,health_observed_at,health_expires_at,health_diagnostic_code,health_failure_count,next_probe_at)
VALUES($12,$8,'rollback-model','CHAT',100,'NONE','HEALTHY',$4,$1,$6,'RESOLVED',1,1,1,$4,$4::timestamptz+interval '4 minutes','PROBE_SUCCEEDED',0,$4::timestamptz+interval '3 minutes');
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,attempt_count,repair_round,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at,protocol_version_id)
VALUES($13,'PERSONAL',$9,$10,$1,'rollback-model','CHAT',1,1,$2,'READY','READY',1,'{}',0,0,3,$4::timestamptz+interval '1 hour',$8,$11,$3,$4,$4,$6)`, pgx.QueryExecModeSimpleProtocol, modelID, strings.Repeat("c", 64), actorID, now, capabilityID, protocolID, sourceVersion, accountID, ownerID, walletID, credentialID, routeID, provisioningID); err != nil {
		t.Fatal(err)
	}
	var currentVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'rollback current',$2,$3,$3) RETURNING version`, uuid.New(), actorID, now.Add(time.Second)).Scan(&currentVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($1,$2,'Rollback Current','rollback current','','{}','PUBLISHED',$3,$4,true)`, currentVersion, modelID, capabilityID, protocolID); err != nil {
		t.Fatal(err)
	}
	// This model exists only in the current catalog and disappears on rollback.
	// Its runtime dependencies must still enter both preview and execution.
	currentOnlyModel, currentOnlyCapability, currentOnlyUsage := uuid.New(), uuid.New(), uuid.New()
	if _, err := repository.pool.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at)
VALUES($1,'integration','v1','rollback-current-only',$2,$3,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at)
VALUES($5,$1,1,$6,4096,1024,'["text"]','v1',$3,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available)
VALUES($7,$1,'Current only','current only','','{}','PUBLISHED',$5,$6,true);
INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,model_id,protocol_version_id,reference_status,route_version,route_generation,health_version,health_observed_at,health_expires_at,health_diagnostic_code,health_failure_count,next_probe_at)
SELECT $11,token_account_id,'rollback-current-only',protocol_code,priority,default_scope,health_status,updated_at,$1,protocol_version_id,reference_status,route_version,route_generation,health_version,health_observed_at,health_expires_at,health_diagnostic_code,health_failure_count,next_probe_at FROM routing_policies WHERE id=$8;
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,attempt_count,repair_round,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at,protocol_version_id)
SELECT $12,scope_type,scope_id,wallet_id,$1,'rollback-current-only',protocol_code,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,attempt_count,repair_round,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at,protocol_version_id FROM route_provisionings WHERE id=$9;
UPDATE provider_credentials SET allowed_models=ARRAY['rollback-model','rollback-current-only'] WHERE id=$10`, pgx.QueryExecModeSimpleProtocol, currentOnlyModel, strings.Repeat("a", 64), actorID, now, currentOnlyCapability, protocolID, currentVersion, routeID, provisioningID, credentialID, uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	preview, err := repository.CatalogRollbackImpact(ctx, sourceVersion)
	if err != nil || len(preview.ModelIDs) != 2 || len(preview.PriceDimensions) != 1 || preview.PriceDimensions[0].ModelID != modelID || preview.Dependencies.Routes != 2 || preview.Dependencies.Provisionings != 2 {
		t.Fatalf("current-only rollback dependencies=%+v error=%v", preview, err)
	}
	formulaID, priceID, priceVersion := uuid.New(), uuid.New(), int64(2)
	pricing := &rollbackAdvancingPricing{result: ImpactPriceCoverageResult{Source: "BILLING", AsOf: now.Add(2 * time.Second), FinancialFactRevision: 9, PriceSetFingerprint: strings.Repeat("d", 64), Items: []ImpactPriceCoverage{{ModelID: modelID, ProtocolCode: "CHAT", FormulaVersionID: &formulaID, CreditUnit: "MICROCREDIT", Covered: true, PriceVersionID: &priceID, PriceVersion: &priceVersion, EffectiveFrom: &now}}}}
	executor := NewPostgresApprovedCredentialExecutor(repository)
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, nil, NewHTTPUpstreamClient(), 0).
		WithImpactPrechecks(true, repository, pricing).
		WithApprovedTokenCommandExecutor(executor)
	command := mustMarshalJSON(t, governedCatalogRollbackCommand{SourceCatalogVersion: sourceVersion})
	projection, err := service.approvalCommandPrecheck(ctx, ApprovalSnapshotRequest{CommandType: "TOKEN_MODEL_CATALOG_ROLLBACK", TargetResourceType: "MODEL_CATALOG", TargetResourceID: strconv.FormatInt(sourceVersion, 10), NormalizedCommand: command})
	if err != nil {
		t.Fatal(err)
	}
	envelope := TokenCommandEnvelope{CommandType: "TOKEN_MODEL_CATALOG_ROLLBACK", Reason: "restore the reviewed catalog snapshot", TargetResourceType: "MODEL_CATALOG", TargetResourceID: strconv.FormatInt(sourceVersion, 10), ExpectedVersions: projection.CriticalResourceVersions, PrecheckFingerprint: projection.PrecheckFingerprint, Command: command}
	approvalID, fingerprint := uuid.New(), strings.Repeat("e", 64)
	body := approvedTokenCommandBody{ActivationEpoch: 1, ApprovalRequestID: approvalID, RequesterUserID: actorID, AuthorizationVersion: 12, CommandFingerprint: fingerprint, ExecutionIdempotencyKey: "approval:" + approvalID.String() + ":TOKEN_MODEL_CATALOG_ROLLBACK:" + fingerprint, CriticalResourceVersions: projection.CriticalResourceVersions, FinancialSnapshot: projection.FinancialSnapshot, Envelope: envelope}
	executorContext, err := serviceauth.ContextForJob(ctx, "token-command-executor", "APPROVAL_EXECUTION", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	perform := func() *httptest.ResponseRecorder {
		raw, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/commands/catalog-rollback/execute", bytes.NewReader(raw)).WithContext(executorContext)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		approvedCommandFixtureHandler(service, request.URL.Path, service.executeApprovedCatalogRollback).ServeHTTP(response, request)
		return response
	}
	// The approved snapshot must be rechecked before the atomic claim. An
	// in-flight request on a current-only model blocks rollback without writes.
	if _, err = repository.pool.Exec(ctx, `INSERT INTO usage_requests(id,user_id,session_id,token_account_id,credential_id,wallet_id,model,model_id,catalog_version,model_capability_version_id,protocol_definition_id,route_generation,model_snapshot,status,execution_status,execution_version,delivery_status,maximum_microcredits,request_snapshot,started_at,updated_at,idempotency_key_digest,request_fingerprint)
VALUES($1,$2,'rollback-current-only-session',$3,$4,$5,'rollback-current-only',$6,$7,$8,$9,1,'{}','UPSTREAM_ACTIVE','STREAMING',1,'DELIVERING',100,'{}',$10,$10,$11,$11)`, currentOnlyUsage, ownerID, accountID, credentialID, walletID, currentOnlyModel, currentVersion, currentOnlyCapability, protocolID, now, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	inflightPreview, err := repository.CatalogRollbackImpact(ctx, sourceVersion)
	if err != nil || inflightPreview.Dependencies.InflightRequests != 1 || inflightPreview.InflightSetFingerprint == preview.InflightSetFingerprint {
		t.Fatalf("current-only inflight preview=%+v err=%v", inflightPreview, err)
	}
	criticalTx, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	critical, inflightCount, criticalErr := catalogRollbackCriticalTx(ctx, criticalTx, sourceVersion, priceVersion)
	_ = criticalTx.Rollback(ctx)
	if criticalErr != nil || inflightCount != 1 || critical.InflightRequestFingerprint != inflightPreview.InflightSetFingerprint || critical.RouteSetFingerprint != inflightPreview.RouteSetFingerprint || critical.ProvisioningSetFingerprint != inflightPreview.ProvisioningSetFingerprint {
		t.Fatalf("current-only execution snapshot=%+v count=%d err=%v", critical, inflightCount, criticalErr)
	}
	blocked := perform()
	if blocked.Code != http.StatusConflict {
		t.Fatalf("current-only inflight rollback=%d/%s", blocked.Code, blocked.Body.String())
	}
	if _, err = repository.pool.Exec(ctx, `UPDATE usage_requests SET execution_status='SUCCEEDED',execution_version=execution_version+1 WHERE id=$1`, currentOnlyUsage); err != nil {
		t.Fatal(err)
	}
	// An equal-cardinality route replacement is still a drift and must leave no
	// command, catalog, audit or outbox fact behind.
	pricing.result.FinancialFactRevision++
	if financialDrift := perform(); financialDrift.Code != http.StatusConflict {
		t.Fatalf("rollback financial drift=%d/%s", financialDrift.Code, financialDrift.Body.String())
	}
	pricing.result.FinancialFactRevision--
	if _, err = repository.pool.Exec(ctx, `UPDATE routing_policies SET priority=priority+1,route_version=route_version+1 WHERE id=$1`, routeID); err != nil {
		t.Fatal(err)
	}
	drift := perform()
	if drift.Code != http.StatusConflict {
		t.Fatalf("rollback drift status=%d body=%s", drift.Code, drift.Body.String())
	}
	var driftCommands, driftCatalogs, driftAudits, driftOutbox int
	if err = repository.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM token_command_results WHERE approval_request_id=$1),
 (SELECT count(*) FROM model_catalog_versions WHERE version>$2),
 (SELECT count(*) FROM token_audit_logs WHERE summary->>'approval_request_id'=$1::text),
 (SELECT count(*) FROM outbox_events WHERE payload->>'approval_request_id'=$1::text)`, approvalID, currentVersion).Scan(&driftCommands, &driftCatalogs, &driftAudits, &driftOutbox); err != nil || driftCommands != 0 || driftCatalogs != 0 || driftAudits != 0 || driftOutbox != 0 {
		t.Fatalf("rollback drift facts command/catalog/audit/outbox=%d/%d/%d/%d err=%v", driftCommands, driftCatalogs, driftAudits, driftOutbox, err)
	}
	projection, err = service.approvalCommandPrecheck(ctx, ApprovalSnapshotRequest{CommandType: "TOKEN_MODEL_CATALOG_ROLLBACK", TargetResourceType: "MODEL_CATALOG", TargetResourceID: strconv.FormatInt(sourceVersion, 10), NormalizedCommand: command})
	if err != nil {
		t.Fatal(err)
	}
	envelope.ExpectedVersions, envelope.PrecheckFingerprint = projection.CriticalResourceVersions, projection.PrecheckFingerprint
	refreshed, refreshErr := service.approvalCommandPrecheck(ctx, ApprovalSnapshotRequest{CommandType: envelope.CommandType, TargetResourceType: envelope.TargetResourceType, TargetResourceID: envelope.TargetResourceID, NormalizedCommand: command})
	if refreshErr != nil || refreshed.PrecheckFingerprint != projection.PrecheckFingerprint || !jsonSemanticEqual(refreshed.CriticalResourceVersions, projection.CriticalResourceVersions) || !refreshed.TokenObservedAt.After(projection.TokenObservedAt) || refreshed.BillingObservedAt == nil || projection.BillingObservedAt == nil || !refreshed.BillingObservedAt.After(*projection.BillingObservedAt) {
		t.Fatalf("same facts with advancing observation clocks: original=%+v refreshed=%+v err=%v", projection, refreshed, refreshErr)
	}
	if _, err = executor.PrecheckApprovedTokenCommand(ctx, envelope.CommandType, envelope); err != nil {
		t.Fatalf("refreshed rollback executor precheck: %v; critical=%s", err, envelope.ExpectedVersions)
	}
	approvalID, fingerprint = uuid.New(), strings.Repeat("f", 64)
	body = approvedTokenCommandBody{ActivationEpoch: 1, ApprovalRequestID: approvalID, RequesterUserID: actorID, AuthorizationVersion: 12, CommandFingerprint: fingerprint, ExecutionIdempotencyKey: "approval:" + approvalID.String() + ":TOKEN_MODEL_CATALOG_ROLLBACK:" + fingerprint, CriticalResourceVersions: projection.CriticalResourceVersions, FinancialSnapshot: projection.FinancialSnapshot, Envelope: envelope}
	first, second := perform(), perform()
	var firstResult, replayResult TokenCommandResult
	if first.Code != http.StatusOK || second.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &firstResult) != nil || json.Unmarshal(second.Body.Bytes(), &replayResult) != nil || firstResult.Status != "SUCCEEDED" || firstResult.CommandReference != replayResult.CommandReference {
		t.Fatalf("rollback first=%d/%s replay=%d/%s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	var forwardVersion, source int64
	var resultRows, auditRows, outboxRows int
	if err = repository.pool.QueryRow(ctx, `SELECT max(version),max(rollback_source_version),
 (SELECT count(*) FROM token_command_results WHERE command_reference=$1 AND status='SUCCEEDED'),
 (SELECT count(*) FROM token_audit_logs WHERE summary->>'command_reference'=$1::text AND action='TOKEN_MODEL_CATALOG_ROLLBACK'),
 (SELECT count(*) FROM outbox_events WHERE payload->>'command_reference'=$1::text AND event_type='token.management-command.executed.v1')
 FROM model_catalog_versions WHERE version>$2`, firstResult.CommandReference, currentVersion).Scan(&forwardVersion, &source, &resultRows, &auditRows, &outboxRows); err != nil || forwardVersion <= currentVersion || source != sourceVersion || resultRows != 1 || auditRows != 1 || outboxRows != 1 {
		t.Fatalf("rollback facts version/source/result/audit/outbox=%d/%d/%d/%d/%d err=%v", forwardVersion, source, resultRows, auditRows, outboxRows, err)
	}
}

func (resolver *modelAccountWalletResolver) Ensure(context.Context, string, uuid.UUID) (uuid.UUID, error) {
	resolver.calls.Add(1)
	return resolver.walletID, nil
}

// TestModelAccountGovernancePostgresIntegration is the no-SKIP release gate
// for the four immediate APT-123-R2 adapters. Model publish already has its
// own real PostgreSQL gate because its Billing and route evidence is larger.
func TestModelAccountGovernancePostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error { return nil }))
	actorID, ownerID, walletID, modelID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	wallets := &modelAccountWalletResolver{walletID: walletID}
	handler := newModelAccountGovernanceHandler(t, repository, wallets, actorID, AdminOwnerCandidate{ID: ownerID, Type: "USER", DisplayLabel: "Integration owner", Status: "ACTIVE", AuthorizationVersion: 11, OwnerVersion: 11})
	policyEventID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO token_account_owner_cap_policy_revisions(policy_version,max_per_owner,config_digest,request_digest,idempotency_key_digest,principal_digest,changed_by,reason,request_id,outbox_event_id)
OVERRIDING SYSTEM VALUE VALUES(2,20,$1,$2,$3,$4,'integration-fixture','owner cap boundary fixture',$5,$6);
INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at)
SELECT $6,'token.account-owner-cap-policy.changed.v1',policy_id,2,jsonb_build_object('policy_version',2,'max_per_owner',20,'config_digest',$1,'changed_by','integration-fixture','request_id',$5),$5::text,transaction_timestamp()
FROM token_account_owner_cap_policy_active WHERE singleton_key;
UPDATE token_account_owner_cap_policy_active SET active_policy_version=2,pointer_version=2,updated_by='integration-fixture',updated_at=transaction_timestamp() WHERE singleton_key;
SELECT setval(pg_get_serial_sequence('token_account_owner_cap_policy_revisions','policy_version'),2,true);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode)
SELECT gen_random_uuid(),'USER',$7,gen_random_uuid(),'Existing '||ordinal,
 CASE WHEN ordinal<=7 THEN 'ACTIVE' WHEN ordinal<=13 THEN 'DISABLED' ELSE 'FROZEN' END,
 $8,transaction_timestamp(),transaction_timestamp(),'PLATFORM'
FROM generate_series(1,19) ordinal`, pgx.QueryExecModeSimpleProtocol, tokenAccountOwnerCapConfigDigest(20, 2), strings.Repeat("4", 64), strings.Repeat("5", 64), strings.Repeat("6", 64), uuid.New(), policyEventID, ownerID, actorID); err != nil {
		t.Fatal(err)
	}

	var protocolID uuid.UUID
	if err := repository.pool.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE protocol_code='CHAT' AND version=1 AND status='PUBLISHED'`).Scan(&protocolID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'APT-123-R2 integration',$2,$3,$3) RETURNING version`, uuid.New(), actorID, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	capabilityID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at)
VALUES($1,'integration','v1','apt-123-model',$2,$3,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at)
VALUES($5,$1,1,$6,4096,1024,'["text"]','v1',$3,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available)
VALUES($7,$1,'APT 123 Model','apt 123 model','','{}','PUBLISHED',$5,$6,true)`, pgx.QueryExecModeSimpleProtocol, modelID, strings.Repeat("a", 64), actorID, now, capabilityID, protocolID, catalogVersion); err != nil {
		t.Fatal(err)
	}

	draftCommand := mustMarshalJSON(t, governedModelDraftCommand{ModelID: modelID, ExpectedCatalogVersion: catalogVersion, ExpectedDraftVersion: 0, DisplayName: "APT 123 Model Updated", Description: "reviewed", Aliases: []string{"apt-123"}, ProtocolCode: "CHAT", ProtocolVersion: 1, ContextLength: 8192, MaximumOutputTokens: 2048, SupportedModalities: []string{"text"}, TokenizerVersion: "v2"})
	draftResult, draftReplay := executeAndReplayModelAccountHTTPCommand(t, handler, "TOKEN_MODEL_DRAFT_SAVE", "MODEL", modelID.String(), draftCommand, "save the reviewed model draft", "apt-123-draft-0001")
	if draftResult.Status != "SUCCEEDED" {
		t.Fatalf("draft result=%+v", draftResult)
	}
	if draftReplay.CommandReference != draftResult.CommandReference {
		t.Fatalf("draft replay reference=%s want=%s", draftReplay.CommandReference, draftResult.CommandReference)
	}
	var draftVersion int64
	if err := repository.pool.QueryRow(ctx, `SELECT draft_version FROM model_catalog_drafts WHERE model_id=$1`, modelID).Scan(&draftVersion); err != nil || draftVersion != 1 {
		t.Fatalf("draft version=%d err=%v", draftVersion, err)
	}

	createCommand := mustMarshalJSON(t, governedAccountCreateCommand{OwnerType: "USER", OwnerID: ownerID, Name: "APT 123 Account", ShareScope: "OWNER_UNIT_ONLY", DefaultPriority: 100})
	createPrecheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT", ownerID.String(), createCommand)
	var createExpected governedAccountCreateExpected
	if json.Unmarshal(createPrecheck.ExpectedVersions, &createExpected) != nil || createExpected.OwnerAccountCount != 19 || createExpected.OwnerAccountCap != 20 || createExpected.OwnerAccountCapPolicyVersion != 2 {
		t.Fatalf("account capacity precheck=%s", createPrecheck.ExpectedVersions)
	}
	createResult, createReplay := executeAndReplayModelAccountHTTPCommand(t, handler, "TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT", ownerID.String(), createCommand, "create an account for the selected owner", "apt-123-account-create-0001")
	if createResult.Status != "PENDING" || wallets.calls.Load() != 0 {
		t.Fatalf("create result=%+v wallet calls=%d", createResult, wallets.calls.Load())
	}
	if createReplay.CommandReference != createResult.CommandReference || wallets.calls.Load() != 0 {
		t.Fatalf("create replay=%+v wallet calls=%d", createReplay, wallets.calls.Load())
	}
	// The POST response is deliberately lost after Billing commits. Token must
	// recover exclusively through GET of the immutable receipt and must not
	// issue a second wallet-provisioning POST.
	provisioning := &modelAccountProvisioningClient{walletID: walletID, postErr: true}
	worker, err := NewTokenAccountProvisioningWorker(repository, provisioning, modelAccountAuthorityClient{}, "apt-123-worker", TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed || provisioning.calls.Load() != 1 || provisioning.gets.Load() != 1 {
		t.Fatalf("provisioning processed=%v post/get=%d/%d err=%v", processed, provisioning.calls.Load(), provisioning.gets.Load(), err)
	}
	createResult, err = repository.TokenCommandResult(ctx, createResult.CommandReference, actorID)
	if err != nil || createResult.Status != "SUCCEEDED" {
		t.Fatalf("completed create result=%+v err=%v", createResult, err)
	}
	var createProjection struct {
		TokenAccountID uuid.UUID `json:"token_account_id"`
	}
	if json.Unmarshal(createResult.RedactedResult, &createProjection) != nil || createProjection.TokenAccountID == uuid.Nil {
		t.Fatalf("invalid account projection: %s", createResult.RedactedResult)
	}
	blockedCommand := mustMarshalJSON(t, governedAccountCreateCommand{OwnerType: "USER", OwnerID: ownerID, Name: "Over owner cap", ShareScope: "OWNER_UNIT_ONLY", DefaultPriority: 100})
	blockedPrecheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT", ownerID.String(), blockedCommand)
	if !containsString(blockedPrecheck.Blockers, "TOKEN_ACCOUNT_CAP_REACHED") {
		t.Fatalf("cap blocker missing: %+v", blockedPrecheck.Blockers)
	}
	blockedResponse := performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", modelAccountEnvelope(t, "TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT", ownerID.String(), blockedCommand, "attempt beyond owner cap", blockedPrecheck), "apt-123-account-over-cap")
	if blockedResponse.Code != http.StatusConflict || !strings.Contains(blockedResponse.Body.String(), "TOKEN_ACCOUNT_CAP_REACHED") || wallets.calls.Load() != 0 {
		t.Fatalf("cap response=%d/%s wallet calls=%d", blockedResponse.Code, blockedResponse.Body.String(), wallets.calls.Load())
	}

	statusCommand := json.RawMessage(`{"target_status":"DISABLED"}`)
	stalePrecheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ACCOUNT_STATUS_CHANGE", "TOKEN_ACCOUNT", createProjection.TokenAccountID.String(), statusCommand)
	if _, err := repository.pool.Exec(ctx, `UPDATE token_accounts SET updated_at=updated_at+interval '1 second' WHERE id=$1`, createProjection.TokenAccountID); err != nil {
		t.Fatal(err)
	}
	staleEnvelope := modelAccountEnvelope(t, "TOKEN_ACCOUNT_STATUS_CHANGE", "TOKEN_ACCOUNT", createProjection.TokenAccountID.String(), statusCommand, "disable after dependency review", stalePrecheck)
	stale := performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", staleEnvelope, "apt-123-account-status-stale")
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "COMMAND_PRECHECK_CHANGED") {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
	statusResult := executeModelAccountHTTPCommand(t, handler, "TOKEN_ACCOUNT_STATUS_CHANGE", "TOKEN_ACCOUNT", createProjection.TokenAccountID.String(), statusCommand, "disable after dependency review", "apt-123-account-status-0001")
	if statusResult.Status != "SUCCEEDED" {
		t.Fatalf("status result=%+v", statusResult)
	}

	// Both callers freeze the same payload and race the same actor/key. The
	// target lock and existing-first recovery must yield one domain transition
	// and one immutable result rather than two account-version increments.
	enableCommand := json.RawMessage(`{"target_status":"ACTIVE"}`)
	enablePrecheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ACCOUNT_STATUS_CHANGE", "TOKEN_ACCOUNT", createProjection.TokenAccountID.String(), enableCommand)
	enableEnvelope := modelAccountEnvelope(t, "TOKEN_ACCOUNT_STATUS_CHANGE", "TOKEN_ACCOUNT", createProjection.TokenAccountID.String(), enableCommand, "enable after dependency review", enablePrecheck)
	responses := make([]*httptest.ResponseRecorder, 2)
	var concurrent sync.WaitGroup
	concurrent.Add(len(responses))
	for index := range responses {
		go func(index int) {
			defer concurrent.Done()
			responses[index] = performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", enableEnvelope, "apt-123-account-enable-concurrent")
		}(index)
	}
	concurrent.Wait()
	var concurrentResults [2]TokenCommandResult
	for index, response := range responses {
		if (response.Code != http.StatusCreated && response.Code != http.StatusOK) || json.Unmarshal(response.Body.Bytes(), &concurrentResults[index]) != nil {
			t.Fatalf("concurrent status[%d]=%d body=%s", index, response.Code, response.Body.String())
		}
	}
	if concurrentResults[0].CommandReference != concurrentResults[1].CommandReference {
		t.Fatalf("concurrent status references=%s/%s", concurrentResults[0].CommandReference, concurrentResults[1].CommandReference)
	}
	var enabledVersion int64
	if err := repository.pool.QueryRow(ctx, `SELECT management_version FROM token_accounts WHERE id=$1 AND status='ACTIVE'`, createProjection.TokenAccountID).Scan(&enabledVersion); err != nil || enabledVersion != 3 {
		t.Fatalf("concurrent status version=%d err=%v", enabledVersion, err)
	}

	// The same plaintext key belongs to a separate idempotency namespace for a
	// different authorized actor. It may execute a different payload, but can
	// never replay or disclose the first actor's result.
	secondActor := uuid.New()
	secondHandler := newModelAccountGovernanceHandler(t, repository, wallets, secondActor, AdminOwnerCandidate{ID: ownerID, Type: "USER", DisplayLabel: "Integration owner", Status: "ACTIVE", AuthorizationVersion: 11, OwnerVersion: 11})
	crossActor := executeModelAccountHTTPCommand(t, secondHandler, "TOKEN_ACCOUNT_STATUS_CHANGE", "TOKEN_ACCOUNT", createProjection.TokenAccountID.String(), statusCommand, "disable under a separate authorized actor", "apt-123-account-enable-concurrent")
	if crossActor.CommandReference == concurrentResults[0].CommandReference || crossActor.ActorUserID != secondActor {
		t.Fatalf("cross actor result=%+v first=%+v", crossActor, concurrentResults[0])
	}

	var accounts, results, audits, outbox int
	if err := repository.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM token_accounts WHERE id=$1 AND status='DISABLED' AND management_version=4),
 (SELECT count(*) FROM token_command_results WHERE actor_user_id=$2 AND command_type IN ('TOKEN_MODEL_DRAFT_SAVE','TOKEN_ACCOUNT_CREATE','TOKEN_ACCOUNT_STATUS_CHANGE')),
	(SELECT count(*) FROM token_audit_logs WHERE actor_user_id=$2 AND action IN ('TOKEN_MODEL_DRAFT_SAVE','TOKEN_ACCOUNT_STATUS_CHANGE','TOKEN_ACCOUNT_PROVISIONING_REQUESTED','TOKEN_ACCOUNT_CREATED','TOKEN_ACCOUNT_PROVISIONING_COMPLETED')),
	(SELECT count(*) FROM outbox_events WHERE event_type IN ('token.management-command.executed.v1','token.account-provisioning.requested.v1','token.account-provisioning.completed.v1'))`, createProjection.TokenAccountID, actorID).Scan(&accounts, &results, &audits, &outbox); err != nil || accounts != 1 || results != 4 || audits != 6 || outbox != 6 {
		t.Fatalf("facts account/results/audits/outbox=%d/%d/%d/%d err=%v", accounts, results, audits, outbox, err)
	}
}

// TestTokenAccountCreateAuthorityAndCapacityDriftPostgresIntegration proves
// that every mutable Account/owner/policy/count fact is re-read before the
// wallet or any Token command/domain fact is written. CI runs this gate with a
// real PostgreSQL database and does not accept SKIP.
func TestTokenAccountCreateAuthorityAndCapacityDriftPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error { return nil }))
	actorID, ownerID := uuid.New(), uuid.New()
	wallets := &modelAccountWalletResolver{walletID: uuid.New()}
	var authorizationVersion atomic.Int64
	authorizationVersion.Store(11)
	var allowed atomic.Bool
	allowed.Store(true)
	handler := newMutableModelAccountGovernanceHandler(t, repository, wallets, actorID, ownerID, &authorizationVersion, &allowed)

	commandFor := func(name string) json.RawMessage {
		return mustMarshalJSON(t, governedAccountCreateCommand{OwnerType: "USER", OwnerID: ownerID, Name: name, ShareScope: "OWNER_UNIT_ONLY", DefaultPriority: 100})
	}
	postFrozen := func(command json.RawMessage, key string, mutate func()) *httptest.ResponseRecorder {
		precheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT", ownerID.String(), command)
		envelope := modelAccountEnvelope(t, "TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT", ownerID.String(), command, "create only after authority and capacity revalidation", precheck)
		mutate()
		return performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", envelope, key)
	}

	authorizationDrift := postFrozen(commandFor("Authority drift"), "apt-123-authority-drift", func() { authorizationVersion.Store(12) })
	if authorizationDrift.Code != http.StatusConflict || !strings.Contains(authorizationDrift.Body.String(), "COMMAND_PRECHECK_CHANGED") {
		t.Fatalf("authority drift=%d/%s", authorizationDrift.Code, authorizationDrift.Body.String())
	}
	authorizationVersion.Store(11)
	permissionRevoked := postFrozen(commandFor("Permission revoked"), "apt-123-permission-revoked", func() { allowed.Store(false) })
	if permissionRevoked.Code != http.StatusForbidden || !strings.Contains(permissionRevoked.Body.String(), "PERMISSION_DENIED") {
		t.Fatalf("permission revoked=%d/%s", permissionRevoked.Code, permissionRevoked.Body.String())
	}
	allowed.Store(true)
	countDrift := postFrozen(commandFor("Count drift"), "apt-123-count-drift", func() {
		if _, err := repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Concurrent system account','DISABLED',$4,transaction_timestamp(),transaction_timestamp(),'PLATFORM')`, uuid.New(), ownerID, uuid.New(), actorID); err != nil {
			t.Fatal(err)
		}
	})
	if countDrift.Code != http.StatusConflict || !strings.Contains(countDrift.Body.String(), "COMMAND_PRECHECK_CHANGED") {
		t.Fatalf("count drift=%d/%s", countDrift.Code, countDrift.Body.String())
	}
	policyDrift := postFrozen(commandFor("Policy drift"), "apt-123-policy-drift", func() {
		eventID, requestID := uuid.New(), uuid.New()
		if _, err := repository.pool.Exec(ctx, `INSERT INTO token_account_owner_cap_policy_revisions(policy_version,max_per_owner,config_digest,request_digest,idempotency_key_digest,principal_digest,changed_by,reason,request_id,outbox_event_id)
OVERRIDING SYSTEM VALUE VALUES(2,999,$1,$2,$3,$4,'integration-fixture','policy drift fixture',$5,$6);
INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at)
SELECT $6,'token.account-owner-cap-policy.changed.v1',policy_id,2,jsonb_build_object('policy_version',2,'max_per_owner',999,'config_digest',$1,'changed_by','integration-fixture','request_id',$5),$5::text,transaction_timestamp() FROM token_account_owner_cap_policy_active WHERE singleton_key;
UPDATE token_account_owner_cap_policy_active SET active_policy_version=2,pointer_version=2,updated_by='integration-fixture',updated_at=transaction_timestamp() WHERE singleton_key`, pgx.QueryExecModeSimpleProtocol, tokenAccountOwnerCapConfigDigest(999, 2), strings.Repeat("7", 64), strings.Repeat("8", 64), strings.Repeat("9", 64), requestID, eventID); err != nil {
			t.Fatal(err)
		}
	})
	if policyDrift.Code != http.StatusConflict || !strings.Contains(policyDrift.Body.String(), "COMMAND_PRECHECK_CHANGED") {
		t.Fatalf("policy drift=%d/%s", policyDrift.Code, policyDrift.Body.String())
	}
	var createdAccounts, commandFacts, auditFacts, commandOutbox int
	if err := repository.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM token_accounts WHERE owner_id=$1 AND name IN ('Authority drift','Permission revoked','Count drift','Policy drift')),
 (SELECT count(*) FROM token_command_results WHERE target_resource_id=$1::text AND command_type='TOKEN_ACCOUNT_CREATE'),
 (SELECT count(*) FROM token_audit_logs WHERE resource_id=$1 AND action='TOKEN_ACCOUNT_CREATED'),
 (SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1' AND payload->>'target_resource_id'=$1::text)`, ownerID).Scan(&createdAccounts, &commandFacts, &auditFacts, &commandOutbox); err != nil || createdAccounts != 0 || commandFacts != 0 || auditFacts != 0 || commandOutbox != 0 || wallets.calls.Load() != 0 {
		t.Fatalf("drift facts accounts/commands/audits/outbox/wallet=%d/%d/%d/%d/%d err=%v", createdAccounts, commandFacts, auditFacts, commandOutbox, wallets.calls.Load(), err)
	}

	legacyBody := map[string]any{"owner_type": "USER", "owner_id": ownerID, "name": "Legacy compatible account", "share_scope": "OWNER_UNIT_ONLY", "default_priority": 100}
	activeLegacy := performModelAccountRequest(handler, http.MethodPost, "/admin/v1/token-accounts", legacyBody, "")
	if activeLegacy.Code != http.StatusGone || !strings.Contains(activeLegacy.Body.String(), "DIRECT_TOKEN_COMMAND_RETIRED") || wallets.calls.Load() != 0 {
		t.Fatalf("active legacy=%d/%s wallet=%d", activeLegacy.Code, activeLegacy.Body.String(), wallets.calls.Load())
	}
	preHandler := newMutableModelAccountGovernanceHandlerWithActivation(t, repository, wallets, actorID, ownerID, &authorizationVersion, &allowed, TokenCommandGovernanceActivation{State: "PRE_ACTIVATION"})
	allowed.Store(false)
	preDenied := performModelAccountRequest(preHandler, http.MethodPost, "/admin/v1/token-accounts", legacyBody, "")
	if preDenied.Code != http.StatusServiceUnavailable || !strings.Contains(preDenied.Body.String(), "TOKEN_ACCOUNT_CREATE_NOT_ACTIVE") || wallets.calls.Load() != 0 {
		t.Fatalf("PRE legacy missing dual permission=%d/%s wallet=%d", preDenied.Code, preDenied.Body.String(), wallets.calls.Load())
	}
	allowed.Store(true)
	preCreated := performModelAccountRequest(preHandler, http.MethodPost, "/admin/v1/token-accounts", legacyBody, "")
	if preCreated.Code != http.StatusServiceUnavailable || !strings.Contains(preCreated.Body.String(), "TOKEN_ACCOUNT_CREATE_NOT_ACTIVE") || wallets.calls.Load() != 0 {
		t.Fatalf("PRE legacy create=%d/%s wallet=%d", preCreated.Code, preCreated.Body.String(), wallets.calls.Load())
	}
	safeHandler := newMutableModelAccountGovernanceHandlerWithActivation(t, repository, wallets, actorID, ownerID, &authorizationVersion, &allowed, TokenCommandGovernanceActivation{State: "SAFE_ROLLBACK", Epoch: 8})
	safeLegacy := performModelAccountRequest(safeHandler, http.MethodPost, "/admin/v1/token-accounts", map[string]any{"owner_type": "USER", "owner_id": ownerID, "name": "Safe blocked", "share_scope": "OWNER_UNIT_ONLY", "default_priority": 100}, "")
	if safeLegacy.Code != http.StatusServiceUnavailable || !strings.Contains(safeLegacy.Body.String(), "TOKEN_ACCOUNT_CREATE_NOT_ACTIVE") || wallets.calls.Load() != 0 {
		t.Fatalf("SAFE legacy=%d/%s wallet=%d", safeLegacy.Code, safeLegacy.Body.String(), wallets.calls.Load())
	}
}

func TestTokenAccountCreateConcurrentOwnerCapPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error { return nil }))
	actorID, ownerID := uuid.New(), uuid.New()
	policyEventID, policyRequestID := uuid.New(), uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO token_account_owner_cap_policy_revisions(policy_version,max_per_owner,config_digest,request_digest,idempotency_key_digest,principal_digest,changed_by,reason,request_id,outbox_event_id)
OVERRIDING SYSTEM VALUE VALUES(2,2,$1,$2,$3,$4,'integration-fixture','concurrent owner boundary',$5,$6);
INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at)
SELECT $6,'token.account-owner-cap-policy.changed.v1',policy_id,2,jsonb_build_object('policy_version',2,'max_per_owner',2,'config_digest',$1,'changed_by','integration-fixture','request_id',$5),$5::text,transaction_timestamp() FROM token_account_owner_cap_policy_active WHERE singleton_key;
UPDATE token_account_owner_cap_policy_active SET active_policy_version=2,pointer_version=2,updated_by='integration-fixture',updated_at=transaction_timestamp() WHERE singleton_key;
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode)
VALUES($7,'USER',$8,$9,'Existing disabled','DISABLED',$10,transaction_timestamp(),transaction_timestamp(),'PLATFORM')`, pgx.QueryExecModeSimpleProtocol, tokenAccountOwnerCapConfigDigest(2, 2), strings.Repeat("4", 64), strings.Repeat("5", 64), strings.Repeat("3", 64), policyRequestID, policyEventID, uuid.New(), ownerID, uuid.New(), actorID); err != nil {
		t.Fatal(err)
	}
	wallets := &modelAccountWalletResolver{walletID: uuid.New()}
	handler := newModelAccountGovernanceHandler(t, repository, wallets, actorID, AdminOwnerCandidate{ID: ownerID, Type: "USER", DisplayLabel: "Concurrent owner", Status: "ACTIVE", AuthorizationVersion: 11, OwnerVersion: 11})
	commands := []json.RawMessage{
		mustMarshalJSON(t, governedAccountCreateCommand{OwnerType: "USER", OwnerID: ownerID, Name: "Concurrent A", ShareScope: "OWNER_UNIT_ONLY", DefaultPriority: 100}),
		mustMarshalJSON(t, governedAccountCreateCommand{OwnerType: "USER", OwnerID: ownerID, Name: "Concurrent B", ShareScope: "OWNER_UNIT_ONLY", DefaultPriority: 100}),
	}
	envelopes := make([]TokenCommandEnvelope, len(commands))
	for index, command := range commands {
		precheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT", ownerID.String(), command)
		envelopes[index] = modelAccountEnvelope(t, "TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT", ownerID.String(), command, "create at the reviewed owner capacity boundary", precheck)
	}
	responses := make([]*httptest.ResponseRecorder, len(commands))
	var group sync.WaitGroup
	for index := range commands {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			responses[index] = performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", envelopes[index], "apt-123-owner-cap-race-"+strconv.Itoa(index))
		}(index)
	}
	group.Wait()
	succeeded, conflicted := 0, 0
	for _, response := range responses {
		switch response.Code {
		case http.StatusCreated:
			succeeded++
		case http.StatusConflict:
			if !strings.Contains(response.Body.String(), "COMMAND_PRECHECK_CHANGED") && !strings.Contains(response.Body.String(), "TOKEN_ACCOUNT_CAP_REACHED") {
				t.Fatalf("unexpected cap conflict=%s", response.Body.String())
			}
			conflicted++
		default:
			t.Fatalf("unexpected cap race=%d/%s", response.Code, response.Body.String())
		}
	}
	var accountCount, reservationCount, commandCount int
	if err := repository.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM token_accounts WHERE owner_type='USER' AND owner_id=$1 AND management_mode='PLATFORM' AND status IN ('ACTIVE','DISABLED','FROZEN')),
 (SELECT count(*) FROM token_account_provisioning_reservations WHERE owner_type='USER' AND owner_id=$1 AND status='PENDING'),
 (SELECT count(*) FROM token_command_results WHERE command_type='TOKEN_ACCOUNT_CREATE' AND target_resource_id=$1::text)`, ownerID).Scan(&accountCount, &reservationCount, &commandCount); err != nil || succeeded != 1 || conflicted != 1 || accountCount != 1 || reservationCount != 1 || commandCount != 1 || wallets.calls.Load() != 0 {
		t.Fatalf("cap race success/conflict/accounts/reservations/commands/wallet=%d/%d/%d/%d/%d/%d err=%v", succeeded, conflicted, accountCount, reservationCount, commandCount, wallets.calls.Load(), err)
	}
}

func newModelAccountGovernanceHandler(t *testing.T, repository *PostgresRepository, wallets WalletResolver, actorID uuid.UUID, owner AdminOwnerCandidate) http.Handler {
	t.Helper()
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Authorization-Version", "11")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": actorID, "granted_permissions": []string{"platform.models.manage", "platform.tokens.manage", "platform.tokens.owner-candidates.read"}})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "integration-service-token")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, nil, wallets, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithAuthorizer(authorizer).
		WithTokenAccountProvisioning(nil, modelAccountAuthorityClient{}).
		WithOwnerCandidateDirectory(integrationOwnerDirectory{item: owner}).
		WithTokenAccountCreateCapability(true).
		WithTokenCommandGovernanceActivation(testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 7}})
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.integration.example", Subject: actorID.String(), SessionID: "apt-123-session", AuthenticatedAt: time.Now().UTC()}, nil
	}
	return httpserver.CorrelationHandler(service.Handler("", verify))
}

func newMutableModelAccountGovernanceHandler(t *testing.T, repository *PostgresRepository, wallets WalletResolver, actorID, ownerID uuid.UUID, version *atomic.Int64, allowed *atomic.Bool) http.Handler {
	return newMutableModelAccountGovernanceHandlerWithActivation(t, repository, wallets, actorID, ownerID, version, allowed, TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 7})
}

func newMutableModelAccountGovernanceHandlerWithActivation(t *testing.T, repository *PostgresRepository, wallets WalletResolver, actorID, ownerID uuid.UUID, version *atomic.Int64, allowed *atomic.Bool, activation TokenCommandGovernanceActivation) http.Handler {
	t.Helper()
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Authorization-Version", strconv.FormatInt(version.Load(), 10))
		grants := []string{"platform.models.manage", "platform.tokens.manage", "platform.tokens.owner-candidates.read"}
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed.Load(), "user_id": actorID, "granted_permissions": grants})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "integration-service-token")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, nil, wallets, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithAuthorizer(authorizer).
		WithTokenAccountProvisioning(nil, modelAccountAuthorityClient{version: version}).
		WithOwnerCandidateDirectory(mutableModelAccountOwnerDirectory{item: AdminOwnerCandidate{ID: ownerID, Type: "USER", DisplayLabel: "Mutable owner", Status: "ACTIVE"}, version: version}).
		WithTokenAccountCreateCapability(activation.State == "ACTIVE").
		WithTokenCommandGovernanceActivation(testTokenGovernanceActivationReader{activation: activation})
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.integration.example", Subject: actorID.String(), SessionID: "apt-123-mutable-session", AuthenticatedAt: time.Now().UTC()}, nil
	}
	return httpserver.CorrelationHandler(service.Handler("", verify))
}

func executeModelAccountHTTPCommand(t *testing.T, handler http.Handler, commandType, resourceType, resourceID string, command json.RawMessage, reason, key string) TokenCommandResult {
	t.Helper()
	precheck := fetchModelAccountPrecheck(t, handler, commandType, resourceType, resourceID, command)
	if len(precheck.Blockers) != 0 {
		t.Fatalf("%s blockers=%v", commandType, precheck.Blockers)
	}
	response := performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", modelAccountEnvelope(t, commandType, resourceType, resourceID, command, reason, precheck), key)
	if response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", commandType, response.Code, response.Body.String())
	}
	var result TokenCommandResult
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.CommandType != commandType || result.TargetResourceType != resourceType || result.TargetResourceID != resourceID {
		t.Fatalf("invalid %s result: %s", commandType, response.Body.String())
	}
	return result
}

func executeAndReplayModelAccountHTTPCommand(t *testing.T, handler http.Handler, commandType, resourceType, resourceID string, command json.RawMessage, reason, key string) (TokenCommandResult, TokenCommandResult) {
	t.Helper()
	precheck := fetchModelAccountPrecheck(t, handler, commandType, resourceType, resourceID, command)
	if len(precheck.Blockers) != 0 {
		t.Fatalf("%s blockers=%v", commandType, precheck.Blockers)
	}
	envelope := modelAccountEnvelope(t, commandType, resourceType, resourceID, command, reason, precheck)
	responses := []*httptest.ResponseRecorder{
		performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", envelope, key),
		performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", envelope, key),
	}
	var results [2]TokenCommandResult
	for index, response := range responses {
		want := http.StatusOK
		if index == 0 {
			want = http.StatusCreated
		}
		if response.Code != want || json.Unmarshal(response.Body.Bytes(), &results[index]) != nil || results[index].CommandType != commandType || results[index].TargetResourceType != resourceType || results[index].TargetResourceID != resourceID {
			t.Fatalf("%s replay[%d] status=%d body=%s", commandType, index, response.Code, response.Body.String())
		}
	}
	return results[0], results[1]
}

func fetchModelAccountPrecheck(t *testing.T, handler http.Handler, commandType, resourceType, resourceID string, command json.RawMessage) TokenCommandPrecheckResult {
	t.Helper()
	response := performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands:precheck", TokenCommandPrecheckRequest{CommandType: commandType, TargetResourceType: resourceType, TargetResourceID: resourceID, Command: command}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("%s precheck status=%d body=%s", commandType, response.Code, response.Body.String())
	}
	var result TokenCommandPrecheckResult
	if json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatalf("invalid %s precheck: %s", commandType, response.Body.String())
	}
	return result
}

func modelAccountEnvelope(t *testing.T, commandType, resourceType, resourceID string, command json.RawMessage, reason string, precheck TokenCommandPrecheckResult) TokenCommandEnvelope {
	t.Helper()
	var expected map[string]any
	if json.Unmarshal(precheck.ExpectedVersions, &expected) != nil {
		t.Fatal("precheck expected versions are invalid")
	}
	expected["precheck_fingerprint"] = precheck.PrecheckFingerprint
	envelope := TokenCommandEnvelope{CommandType: commandType, Reason: reason, TargetResourceType: resourceType, TargetResourceID: resourceID, PrecheckFingerprint: precheck.PrecheckFingerprint, Command: command}
	expected["request_fingerprint"] = governedRequestFingerprint(envelope, expected)
	envelope.ExpectedVersions = mustMarshalJSON(t, expected)
	return envelope
}

func performModelAccountRequest(handler http.Handler, method, path string, body any, key string) *httptest.ResponseRecorder {
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
