package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/requestdeadline"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestTokenAdminQueryAdaptersPostgresIntegration exercises every APT-121
// local-domain membership query through the real APT-120 snapshot transaction.
// CI must run it against PINVOU_TEST_TOKEN_DATABASE_URL without SKIP.
func TestTokenAdminQueryAdaptersPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	if err := repository.ConfigureAdminPagination(AdminPaginationConfig{Enabled: true, CursorHMACKey: []byte("0123456789abcdef0123456789abcdef")}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetAdminPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	modelID, accountID, credentialID, provisioningID, usageID, reviewID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	actorID, ownerID, walletID, catalogID, capabilityID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	protocolID := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'integration',$2,$3,$3) RETURNING version`, catalogID, actorID, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	seed := `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at)
VALUES($1,'provider','adapter-v1','model-one',$12,$8,$10);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at)
VALUES($9,$1,1,$2,128000,8192,'["TEXT"]','v1',$8,$10);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available)
VALUES($11,$1,'Model One','model one','','{}','PUBLISHED',$9,$2,true);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode)
VALUES($3,'USER',$7,$6,'Account','ACTIVE',$8,$10,$10,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status)
VALUES($4,$3,'provider','https://provider.invalid','x'::bytea,'key','fingerprint',1,'ACTIVE',ARRAY['model-one'],$10,$8,$10,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,priority,default_scope,health_status,updated_at,route_generation,protocol_code)
VALUES(gen_random_uuid(),$3,'model-one',100,'NONE','HEALTHY',$10,1,'CHAT');
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at)
VALUES($5,'PERSONAL',$7,$6,$1,'model-one','CHAT',1,$12,'PENDING','REQUESTED',1,'{}',3,$10+interval '1 hour',$3,$4,$8,$10,$10);
INSERT INTO route_provisioning_attempts(id,provisioning_id,repair_round,attempt_number,worker_id,status,stage,started_at,ended_at)
VALUES(gen_random_uuid(),$5,0,1,'integration-worker','FAILED','PROVISIONING',$10,$10+interval '1 second');
INSERT INTO usage_requests(id,user_id,session_id,token_account_id,credential_id,wallet_id,model,status,maximum_microcredits,request_snapshot,started_at,updated_at,idempotency_key_digest,request_fingerprint)
VALUES($13,$7,'session',$3,$4,$6,'model-one','CREATED',100,'{}',$10,$10,$12,$12);
INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at)
VALUES($14,'TOKEN','USAGE_RECONCILIATION_REVIEW','USAGE_REQUEST',$13,'P1','OPEN',$10,$10+interval '1 hour',$10+interval '1 hour',$10+interval '1 hour',$10+interval '1 hour',$10);
INSERT INTO model_catalog_audit(id,action,model_id,after_status,catalog_version,actor_id,reason,evidence,created_at)
VALUES($15,'MODEL_PUBLISHED',$1,'PUBLISHED',$11,$8,'integration','{}',$10);
INSERT INTO token_audit_logs(id,actor_user_id,action,resource_type,resource_id,summary,occurred_at)
VALUES($16,$8,'TOKEN_ACCOUNT_CREATED','TOKEN_ACCOUNT',$3,'{}',$10);
INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at)
VALUES($17,$13,'CANCELLATION','adapter-v1','{}',$10);
INSERT INTO usage_convergence_evidence(id,usage_request_id,evidence_type,source_type,decision,evidence_key,payload_digest,sanitized_snapshot,observed_at)
VALUES($18,$13,'SETTLEMENT','BILLING','COMMIT','integration',$12,'{}',$10);`
	auditID, tokenAuditID, cancellationID, convergenceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	digest := strings.Repeat("0", 64)
	if _, err := repository.pool.Exec(ctx, seed, modelID, protocolID, accountID, credentialID, provisioningID, walletID, ownerID, actorID, capabilityID, now, catalogVersion, digest, usageID, reviewID, auditID, tokenAuditID, cancellationID, convergenceID); err != nil {
		t.Fatal(err)
	}
	shadowReviewID, badTypeReviewID, missingUsageReviewID := uuid.New(), uuid.New(), uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES
($1,'TOKEN','USAGE_RECONCILIATION_SHADOW','USAGE_REQUEST',$4,'P1','OPEN',$5,$5,$5,$5,$5,$5),
($2,'TOKEN','USAGE_RECONCILIATION_REVIEW','TOKEN_ACCOUNT',$4,'P1','OPEN',$5,$5,$5,$5,$5,$5),
($3,'TOKEN','USAGE_RECONCILIATION_REVIEW','USAGE_REQUEST',$6,'P1','OPEN',$5,$5,$5,$5,$5,$5)`, shadowReviewID, badTypeReviewID, missingUsageReviewID, usageID, now, uuid.New()); err != nil {
		t.Fatal(err)
	}

	assertPage := func(resource, sort string, source AdminSnapshotMemberSource, loader func(context.Context, []string) (int, error)) {
		t.Helper()
		page, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: uuid.New(), AuthorizationVersion: 1, Resource: resource, Scope: "platform", Filters: map[string]string{}, Sort: sort, Limit: 50}, source)
		if err != nil {
			t.Fatalf("%s: %v", resource, err)
		}
		if len(page.ResourceIDs) != 1 {
			t.Fatalf("%s members=%v", resource, page.ResourceIDs)
		}
		loaded, err := loader(ctx, page.ResourceIDs)
		if err != nil || loaded != 1 {
			t.Fatalf("%s loader count=%d err=%v", resource, loaded, err)
		}
	}
	empty := adminQueryFilter{values: map[string]string{}}
	modelSource, _ := modelSnapshotSource(empty)
	assertPage("models", adminModelsSort, modelSource, func(ctx context.Context, ids []string) (int, error) {
		items, err := repository.adminModelsByIDs(ctx, ids)
		return len(items), err
	})
	protocolSource, _ := protocolSnapshotSource(empty)
	assertPage("protocols", adminProtocolsSort, protocolSource, func(ctx context.Context, ids []string) (int, error) {
		items, err := repository.adminProtocolsByIDs(ctx, ids)
		return len(items), err
	})
	formulaSource, _ := formulaSnapshotSource(empty)
	assertPage("formulas", adminFormulasSort, formulaSource, func(ctx context.Context, ids []string) (int, error) {
		items, err := repository.adminFormulasByIDs(ctx, ids)
		return len(items), err
	})
	accountSource, _ := tokenAccountSnapshotSource(empty)
	assertPage("token-accounts", adminTokenAccountsSort, accountSource, func(ctx context.Context, ids []string) (int, error) {
		items, err := repository.adminTokenAccountsByIDs(ctx, ids)
		return len(items), err
	})
	provisioningSource, _ := provisioningSnapshotSource(empty)
	assertPage("provisionings", adminProvisioningsSort, provisioningSource, func(ctx context.Context, ids []string) (int, error) {
		items, err := repository.adminProvisioningsByIDs(ctx, ids)
		return len(items), err
	})
	attemptSource, _ := provisioningAttemptSnapshotSource(empty, provisioningID)
	assertPage("provisioning-attempts", adminProvisioningAttemptsSort, attemptSource, func(ctx context.Context, ids []string) (int, error) {
		items, err := repository.adminProvisioningAttemptsByIDs(ctx, provisioningID, ids)
		return len(items), err
	})
	usageSource, _ := usageSnapshotSource(empty)
	assertPage("usage-requests", adminUsageSort, usageSource, func(ctx context.Context, ids []string) (int, error) {
		items, err := repository.adminUsageByIDs(ctx, ids)
		return len(items), err
	})
	reviewSource, _ := reconciliationSnapshotSource(empty)
	assertPage("reconciliation-cases", adminReconciliationSort, reviewSource, func(ctx context.Context, ids []string) (int, error) {
		items, err := repository.adminReconciliationByIDs(ctx, ids)
		return len(items), err
	})
	if items, err := repository.adminReconciliationByIDs(ctx, []string{shadowReviewID.String(), badTypeReviewID.String(), missingUsageReviewID.String()}); err != nil || len(items) != 0 {
		t.Fatalf("reconciliation loader admitted non-authoritative cases: count=%d err=%v", len(items), err)
	}
	auditSource, _ := auditSnapshotSource(empty, "MODEL", modelID)
	assertPage("resource-audit:model", adminResourceAuditSort, auditSource, func(ctx context.Context, ids []string) (int, error) {
		items, err := repository.adminAuditByIDs(ctx, "MODEL", ids)
		return len(items), err
	})
	tokenAuditSource, _ := auditSnapshotSource(empty, "TOKEN_ACCOUNT", accountID)
	assertPage("resource-audit:token_account", adminResourceAuditSort, tokenAuditSource, func(ctx context.Context, ids []string) (int, error) {
		items, err := repository.adminAuditByIDs(ctx, "TOKEN_ACCOUNT", ids)
		return len(items), err
	})
	evidenceSource, _ := evidenceSnapshotSource(empty, usageID)
	evidenceSubject := uuid.New()
	evidenceRequest := AdminPaginationRequest{SubjectID: evidenceSubject, AuthorizationVersion: 1, Resource: "usage-evidence", Scope: usageID.String(), Filters: map[string]string{}, Sort: adminUsageEvidenceSort, Limit: 1}
	page, err := repository.AdminPaginationPage(ctx, evidenceRequest, evidenceSource)
	if err != nil || len(page.ResourceIDs) != 1 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("evidence=%v err=%v", page.ResourceIDs, err)
	}
	if items, loadErr := repository.adminEvidenceByIDs(ctx, page.ResourceIDs); loadErr != nil || len(items) != 1 {
		t.Fatalf("evidence loader count=%d err=%v", len(items), loadErr)
	}
	firstEvidence := page.ResourceIDs[0]
	evidenceRequest.Cursor = *page.NextCursor
	page, err = repository.AdminPaginationPage(ctx, evidenceRequest, nil)
	if err != nil || len(page.ResourceIDs) != 1 || page.ResourceIDs[0] == firstEvidence || page.HasMore {
		t.Fatalf("evidence continuation=%v first=%s err=%v", page.ResourceIDs, firstEvidence, err)
	}
	if items, loadErr := repository.adminEvidenceByIDs(ctx, page.ResourceIDs); loadErr != nil || len(items) != 1 {
		t.Fatalf("evidence continuation loader count=%d err=%v", len(items), loadErr)
	}

	// The query-attempt limiter is persisted in the APT-120 window table, so
	// independent service replicas share subject/resource and trusted-source
	// budgets. Continuations and malformed requests enter this reservation path
	// in the production handler before cursor parsing.
	replica := &PostgresRepository{pool: repository.pool}
	rateSubject := uuid.New()
	for index := 0; index < 60; index++ {
		active := repository
		if index%2 == 1 {
			active = replica
		}
		if err := active.reserveAdminQueryRate(ctx, rateSubject, "models", "203.0.113.20", false); err != nil {
			t.Fatalf("shared rate allowance %d: %v", index, err)
		}
	}
	if err := replica.reserveAdminQueryRate(ctx, rateSubject, "models", "203.0.113.20", false); !errors.Is(err, ErrAdminQueryRateLimited) {
		t.Fatalf("61st shared subject request=%v", err)
	}
	if err := replica.reserveAdminQueryRate(ctx, rateSubject, "protocols", "203.0.113.20", false); err != nil {
		t.Fatalf("resource budget leaked across resources: %v", err)
	}

	allGrants := map[string]bool{
		"platform.models.read": true, "platform.pricing.read": true, "platform.tokens.read": true,
		"platform.usage.read": true, "platform.tokens.owner-candidates.read": true, "platform.audit.read": true,
	}
	var authorizationVersion atomic.Int64
	authorizationVersion.Store(1)
	handler := newAdminQueryIntegrationHandler(t, repository, ownerID, &authorizationVersion, allGrants)
	requests := []string{
		"/admin/v1/models?q=model%20one&lifecycle_status=published&protocol_code=chat&source_adapter=adapter-v1&source_missing=false&limit=1",
		"/admin/v1/protocols?q=chat&status=published&protocol_code=chat&published=true&created_from=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)) + "&created_to=" + url.QueryEscape(now.Add(time.Hour).Format(time.RFC3339)) + "&limit=1",
		"/admin/v1/formulas?q=chat&status=published&protocol_definition_id=" + protocolID.String() + "&metering_unit_code=INPUT_TOKEN&published=true&created_from=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)) + "&created_to=" + url.QueryEscape(now.Add(time.Hour).Format(time.RFC3339)) + "&limit=1",
		"/admin/v1/token-accounts?q=account&owner_type=user&owner_id=" + ownerID.String() + "&status=active&health_status=healthy&credential_status=valid&limit=1",
		"/admin/v1/provisionings?q=" + provisioningID.String() + "&status=pending&model_id=" + modelID.String() + "&token_account_id=" + accountID.String() + "&scope_type=personal&scope_id=" + ownerID.String() + "&sla_overdue=false&limit=1",
		"/admin/v1/provisionings/" + provisioningID.String() + "/attempts?status=failed&limit=1",
		"/admin/v1/usage-requests?q=" + usageID.String() + "&execution_status=received&settlement_status=hold_pending&model=model-one&token_account_id=" + accountID.String() + "&started_from=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)) + "&started_to=" + url.QueryEscape(now.Add(time.Hour).Format(time.RFC3339)) + "&limit=1",
		"/admin/v1/reconciliation-cases?q=" + reviewID.String() + "&status=open&severity=p1&case_type=usage_reconciliation_review&sla_overdue=false&limit=1",
		"/admin/v1/resource-audit?resource_type=MODEL&resource_id=" + modelID.String() + "&action=model_published&actor_id=" + actorID.String() + "&occurred_from=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)) + "&occurred_to=" + url.QueryEscape(now.Add(time.Hour).Format(time.RFC3339)) + "&limit=1",
		"/admin/v1/owner-candidates?q=owner&owner_type=user&status=active&limit=1",
		"/admin/v1/usage-requests/" + usageID.String() + "/evidence?evidence_type=settlement&limit=1",
	}
	for _, path := range requests {
		response := performAdminQueryRequest(handler, path)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"items"`) {
			t.Fatalf("production handler %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	safeProvisioning := performAdminQueryRequest(handler, "/admin/v2/provisionings/"+provisioningID.String())
	if body := safeProvisioning.Body.String(); safeProvisioning.Code != http.StatusOK || strings.Contains(body, "attempts") || strings.Contains(body, "worker_id") || strings.Contains(body, "error_detail") || strings.Contains(body, "active_credential_id") {
		t.Fatalf("versioned provisioning detail status=%d body=%s", safeProvisioning.Code, body)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE usage_requests SET status='REVIEW',settlement_status='REVIEW',reconciliation_last_error='SECRET' WHERE id=$1`, usageID); err != nil {
		t.Fatal(err)
	}
	safeUsage := performAdminQueryRequest(handler, "/admin/v1/usage-requests/"+usageID.String())
	if body := safeUsage.Body.String(); safeUsage.Code != http.StatusOK || !strings.Contains(body, "RECONCILIATION_DIAGNOSTIC_REDACTED") || strings.Contains(body, `"SECRET"`) {
		t.Fatalf("usage handler did not redact an unsafe historical diagnostic status=%d body=%s", safeUsage.Code, body)
	}
	privateReason := "客户 13800138000 要求重新核对"
	if _, err := repository.AdminRetryReconciliation(ctx, usageID, privateReason, actorID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	var diagnosticCode, commandReason, auditSummary string
	if err := repository.pool.QueryRow(ctx, `SELECT request.reconciliation_last_error,command.reason,audit.summary::text FROM usage_requests request JOIN usage_reconciliation_commands command ON command.usage_request_id=request.id JOIN token_audit_logs audit ON audit.resource_type='USAGE_REQUEST' AND audit.resource_id=request.id AND audit.action='USAGE_RECONCILIATION_RETRIED' WHERE request.id=$1 ORDER BY audit.occurred_at DESC LIMIT 1`, usageID).Scan(&diagnosticCode, &commandReason, &auditSummary); err != nil {
		t.Fatal(err)
	}
	if diagnosticCode != "MANUAL_RECONCILIATION_REQUESTED" || commandReason != "AUDIT_ONLY" || strings.Contains(diagnosticCode, privateReason) || strings.Contains(commandReason, privateReason) || !strings.Contains(auditSummary, privateReason) {
		t.Fatalf("reason storage diagnostic=%q command=%q audit=%q", diagnosticCode, commandReason, auditSummary)
	}
	provisioner := &PostgresDefaultRouteProvisioner{repository: repository}
	legacyKey := "legacy-raw-key-0000001"
	legacyReason := "legacy operator repair"
	if _, err := repository.pool.Exec(ctx, `UPDATE route_provisionings SET status='PENDING',stage='MANUAL_REPAIR',repair_round=1,attempt_count=0 WHERE id=$1`, provisioningID); err != nil {
		t.Fatal(err)
	}
	legacyResult, err := scanProvisioning(repository.pool.QueryRow(ctx, provisioningSelect+` WHERE id=$1`, provisioningID))
	if err != nil {
		t.Fatal(err)
	}
	legacySnapshot, err := json.Marshal(legacyResult)
	if err != nil {
		t.Fatal(err)
	}
	legacyDigest := provisioningDigest(map[string]any{"provisioning_id": provisioningID, "reason": legacyReason})
	if _, err := repository.pool.Exec(ctx, `INSERT INTO route_provisioning_commands(idempotency_key,command_type,request_digest,provisioning_id,repair_round,actor_id,reason,result_snapshot,created_at) VALUES($1,'PROVISIONING_REPAIR',$2,$3,1,$4,$5,$6,$7)`, legacyKey, legacyDigest, provisioningID, actorID, legacyReason, legacySnapshot, now); err != nil {
		t.Fatal(err)
	}
	replayed, err := provisioner.Repair(ctx, provisioningID, actorID, legacyKey, legacyReason)
	if err != nil || replayed.RepairRound != 1 || replayed.Status != legacyResult.Status {
		t.Fatalf("pre-v2 same-actor replay round=%d status=%s err=%v", replayed.RepairRound, replayed.Status, err)
	}
	secondActor := uuid.New()
	if _, err := repository.pool.Exec(ctx, `UPDATE route_provisionings SET status='FAILED' WHERE id=$1`, provisioningID); err != nil {
		t.Fatal(err)
	}
	second, err := provisioner.Repair(ctx, provisioningID, secondActor, legacyKey, "second operator repair")
	if err != nil || second.RepairRound != 2 {
		t.Fatalf("pre-v2 raw key leaked across actors round=%d err=%v", second.RepairRound, err)
	}
	var legacyCommandCount, legacyRawKeyCount int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE idempotency_key=$1) FROM route_provisioning_commands WHERE provisioning_id=$2 AND command_type='PROVISIONING_REPAIR'`, legacyKey, provisioningID).Scan(&legacyCommandCount, &legacyRawKeyCount); err != nil || legacyCommandCount != 2 || legacyRawKeyCount != 1 {
		t.Fatalf("legacy actor isolation commands=%d raw=%d err=%v", legacyCommandCount, legacyRawKeyCount, err)
	}

	concurrentProvisioningID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES($1,'PERSONAL',$2,$3,$4,'model-one','CHAT',2,$5,'FAILED','VERIFY',1,'{}',3,$6+interval '1 hour',$7,$8,$9,$6,$6)`, concurrentProvisioningID, ownerID, walletID, modelID, strings.Repeat("1", 64), now, accountID, credentialID, actorID); err != nil {
		t.Fatal(err)
	}
	concurrentKey := "same-actor-concurrent-key-0001"
	concurrentReason := "concurrent operator repair"
	type repairResult struct {
		item RouteProvisioning
		err  error
	}
	start := make(chan struct{})
	results := make(chan repairResult, 2)
	var workers sync.WaitGroup
	for index := 0; index < 2; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			item, repairErr := provisioner.Repair(ctx, concurrentProvisioningID, actorID, concurrentKey, concurrentReason)
			results <- repairResult{item: item, err: repairErr}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	var concurrentResponses []RouteProvisioning
	for result := range results {
		if result.err != nil || result.item.RepairRound != 1 || result.item.Status != "PENDING" {
			t.Fatalf("concurrent same-actor replay round=%d status=%s err=%v", result.item.RepairRound, result.item.Status, result.err)
		}
		concurrentResponses = append(concurrentResponses, result.item)
	}
	if len(concurrentResponses) != 2 || concurrentResponses[0].ID != concurrentResponses[1].ID || !concurrentResponses[0].UpdatedAt.Equal(concurrentResponses[1].UpdatedAt) {
		t.Fatalf("concurrent responses differ: %+v", concurrentResponses)
	}
	if _, err := provisioner.Repair(ctx, concurrentProvisioningID, actorID, concurrentKey, "changed repair payload"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same actor changed payload=%v", err)
	}
	var commandCount, auditCount, repairRound int
	if err := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM route_provisioning_commands WHERE provisioning_id=$1 AND command_type='PROVISIONING_REPAIR'),(SELECT count(*) FROM route_provisioning_audit WHERE provisioning_id=$1 AND action='REPAIR_REQUESTED'),(SELECT repair_round FROM route_provisionings WHERE id=$1)`, concurrentProvisioningID).Scan(&commandCount, &auditCount, &repairRound); err != nil || commandCount != 1 || auditCount != 1 || repairRound != 1 {
		t.Fatalf("concurrent same-actor command=%d audit=%d round=%d err=%v", commandCount, auditCount, repairRound, err)
	}

	for _, matrix := range []struct {
		name   string
		grants map[string]bool
		status int
	}{
		{"neither", map[string]bool{}, http.StatusForbidden},
		{"read only", map[string]bool{"platform.models.read": true}, http.StatusForbidden},
		{"audit only", map[string]bool{"platform.audit.read": true}, http.StatusForbidden},
		{"atomic intersection", map[string]bool{"platform.models.read": true, "platform.audit.read": true}, http.StatusOK},
	} {
		auditHandler := newAdminQueryIntegrationHandler(t, repository, uuid.New(), &authorizationVersion, matrix.grants)
		response := performAdminQueryRequest(auditHandler, "/admin/v1/resource-audit?resource_type=MODEL&resource_id="+modelID.String())
		if response.Code != matrix.status {
			t.Fatalf("audit matrix %s status=%d body=%s", matrix.name, response.Code, response.Body.String())
		}
	}
	missingAuditID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO token_audit_logs(id,actor_user_id,action,resource_type,resource_id,summary,occurred_at) VALUES($1,$2,'TOKEN_ACCOUNT_UPDATED','TOKEN_ACCOUNT',$3,'{}',$4)`, missingAuditID, actorID, accountID, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	missingFirst := performAdminQueryRequest(handler, "/admin/v1/resource-audit?resource_type=TOKEN_ACCOUNT&resource_id="+accountID.String()+"&limit=1")
	var missingEnvelope AdminQueryEnvelope
	if err := json.Unmarshal(missingFirst.Body.Bytes(), &missingEnvelope); err != nil || missingEnvelope.NextCursor == nil {
		t.Fatalf("missing-resource first page=%s err=%v", missingFirst.Body.String(), err)
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_audit_logs WHERE id=$1`, missingAuditID); err != nil {
		t.Fatal(err)
	}
	missing := performAdminQueryRequest(handler, "/admin/v1/resource-audit?resource_type=TOKEN_ACCOUNT&resource_id="+accountID.String()+"&limit=1&cursor="+url.QueryEscape(*missingEnvelope.NextCursor))
	if missing.Code != http.StatusGone || !strings.Contains(missing.Body.String(), "SNAPSHOT_RESOURCE_CHANGED") {
		t.Fatalf("missing-resource continuation status=%d body=%s", missing.Code, missing.Body.String())
	}

	httpRateSubject := uuid.New()
	rateHandler := newAdminQueryIntegrationHandler(t, repository, httpRateSubject, &authorizationVersion, allGrants)
	for attempt := 0; attempt < 60; attempt++ {
		if err := repository.reserveAdminQueryRate(ctx, httpRateSubject, "models", "192.0.2.1", false); err != nil {
			t.Fatalf("rate prefill %d: %v", attempt, err)
		}
	}
	limited := performAdminQueryRequest(rateHandler, "/admin/v1/models")
	if limited.Code != http.StatusTooManyRequests || !strings.Contains(limited.Body.String(), "RATE_LIMITED") {
		t.Fatalf("HTTP rate limit status=%d body=%s", limited.Code, limited.Body.String())
	}

	firstEvidencePage := performAdminQueryRequest(handler, "/admin/v1/usage-requests/"+usageID.String()+"/evidence?limit=1")
	var envelope AdminQueryEnvelope
	if err := json.Unmarshal(firstEvidencePage.Body.Bytes(), &envelope); err != nil || envelope.NextCursor == nil {
		t.Fatalf("HTTP evidence first page=%s err=%v", firstEvidencePage.Body.String(), err)
	}
	authorizationVersion.Store(2)
	changed := performAdminQueryRequest(handler, "/admin/v1/usage-requests/"+usageID.String()+"/evidence?limit=1&cursor="+url.QueryEscape(*envelope.NextCursor))
	if changed.Code != http.StatusGone {
		t.Fatalf("authorization-version continuation status=%d body=%s", changed.Code, changed.Body.String())
	}
}

type integrationOwnerDirectory struct{ item AdminOwnerCandidate }

func (directory integrationOwnerDirectory) Scan(_ context.Context, _ identity.Principal, ownerType string, _ *uuid.UUID, consume func([]AdminOwnerCandidate) error) error {
	if ownerType != "" && ownerType != directory.item.Type {
		return consume(nil)
	}
	return consume([]AdminOwnerCandidate{directory.item})
}

func newAdminQueryIntegrationHandler(t *testing.T, repository *PostgresRepository, subjectID uuid.UUID, version *atomic.Int64, grants map[string]bool) http.Handler {
	t.Helper()
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Permission     string   `json:"permission"`
			AnyPermissions []string `json:"any_permissions"`
			AllPermissions []string `json:"all_permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		allowed := input.Permission == "" || grants[input.Permission]
		granted := []string{}
		if input.Permission != "" && grants[input.Permission] {
			granted = append(granted, input.Permission)
		}
		if len(input.AnyPermissions) > 0 {
			allowed = false
			for _, permission := range input.AnyPermissions {
				if grants[permission] {
					allowed = true
					granted = append(granted, permission)
				}
			}
		}
		for _, permission := range input.AllPermissions {
			if !grants[permission] {
				allowed = false
			} else {
				granted = append(granted, permission)
			}
		}
		w.Header().Set("X-Authorization-Version", strconv.FormatInt(version.Load(), 10))
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed, "user_id": subjectID, "granted_permissions": granted})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "integration-service-token")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithAuthorizer(authorizer).
		WithOwnerCandidateDirectory(integrationOwnerDirectory{item: AdminOwnerCandidate{ID: uuid.New(), Type: "USER", DisplayLabel: "Owner", Status: "ACTIVE"}})
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.integration.example", Subject: subjectID.String(), SessionID: "integration-session"}, nil
	}
	return requestdeadline.TokenAdminMiddleware(service.Handler("", verify), time.Now)
}

func performAdminQueryRequest(handler http.Handler, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer integration")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func newTokenQueryIntegrationRepository(t *testing.T, ctx context.Context, databaseURL string) *PostgresRepository {
	t.Helper()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "token_admin_queries_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = connection.Exec(cleanupCtx, `DROP SCHEMA `+identifier+` CASCADE`)
	})
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.MaxConns = 12
	if configuration.ConnConfig.RuntimeParams == nil {
		configuration.ConnConfig.RuntimeParams = map[string]string{}
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }, adminPaginationMetrics: newAdminPaginationMetrics()}
}
