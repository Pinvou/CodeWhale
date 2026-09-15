package account

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	tokenservice "github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This gate runs actual Account and Token handlers, JWT verification, clients,
// repositories and approved domain adapters. Pricing is an explicit evidence
// fixture, not a Billing integration. Persistent activation/CAS is covered by
// TestTokenGovernanceTransitionPostgresIntegration; this controllable selection
// fixture isolates the HTTP dispatch boundary without pretending to test CAS.
type consumerChainActivation struct {
	mu    sync.Mutex
	state string
	epoch int64
}

func (a *consumerChainActivation) set(state string, epoch int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state, a.epoch = state, epoch
}
func (a *consumerChainActivation) Snapshot(context.Context) (tokengovernance.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return tokengovernance.Snapshot{State: a.state, Epoch: a.epoch}, nil
}
func (a *consumerChainActivation) CurrentTokenCommandGovernanceActivation(ctx context.Context) (tokenservice.TokenCommandGovernanceActivation, error) {
	s, err := a.Snapshot(ctx)
	return tokenservice.TokenCommandGovernanceActivation{State: s.State, Epoch: s.Epoch}, err
}
func (a *consumerChainActivation) LockAndCheckTokenRouteActivation(ctx context.Context, _ pgx.Tx, selected tokenservice.TokenCommandGovernanceActivation) error {
	current, _ := a.CurrentTokenCommandGovernanceActivation(ctx)
	if current != selected {
		return tokenservice.ErrTokenRouteActivationChanged
	}
	return nil
}

func TestTokenApprovalConsumersPostgresIntegration(t *testing.T) {
	accountURL, tokenURL := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"), os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL")
	if accountURL == "" || tokenURL == "" {
		t.Skip("real Account and Token PostgreSQL URLs required")
	}
	for _, commandType := range []string{"TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE"} {
		t.Run(commandType, func(t *testing.T) {
			t.Setenv("APP_ENV", "test")
			t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
			t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "")
			t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", "")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			// Current approval producers require the dedicated authorization
			// writer. Use the complete real Account database chain, not the old
			// random-schema fixture which predates migration 51.
			accountDatabase, _ := newApprovalRelayIntegrationDatabase(t)
			accountRepository := &PostgresRepository{pool: accountDatabase.runtimePool, approvalWriterPool: accountDatabase.writerPool, now: func() time.Time { return time.Now().UTC() }}
			tokenRepository, connection := newApprovalSnapshotTokenRepository(t, ctx, tokenURL)
			activation := &consumerChainActivation{state: "ACTIVE", epoch: 1}
			accountRepository.WithTokenGovernanceActivation(activation)
			tokenRepository.WithTokenRouteActivationTransactionGuard(activation)
			requester := seedTokenOwnerCandidateActor(t, ctx, accountDatabase.adminPool, "PLATFORM_SUPER_ADMIN", "consumer-requester")
			approver := seedTokenOwnerCandidateActor(t, ctx, accountDatabase.adminPool, "PLATFORM_SUPER_ADMIN", "consumer-approver")
			modelID := seedApprovalSnapshotTokenModel(t, ctx, connection)
			pricing := &approvalSnapshotPricingStub{modelID: modelID, observedAt: time.Now().UTC(), fingerprint: strings.Repeat("b", 64)}
			// Rollback validates protocol support through the same concrete adapter
			// used by production; this flow does not issue upstream model requests.
			tokenApp := tokenservice.NewService(tokenRepository, nil, nil, nil, tokenservice.ProviderURLPolicy{}, nil, tokenservice.NewHTTPUpstreamClient(), 0).
				WithImpactPrechecks(true, tokenRepository, pricing).
				WithApprovedTokenCommandExecutor(tokenservice.NewPostgresApprovedCredentialExecutor(tokenRepository)).
				WithTokenCommandGovernanceActivation(activation)
			// Build the Account-only snapshot source with its original legacy test
			// transport before enabling actual executor JWT authentication below.
			sourceServer := httptest.NewServer(httpserver.CorrelationHandler(tokenApp.Handler("consumer-snapshot-fixture", func(context.Context, string) (identity.Principal, error) { return identity.Principal{}, ErrForbidden })))
			defer sourceServer.Close()
			source, err := NewHTTPApprovalSnapshotSource(sourceServer.URL, "consumer-snapshot-fixture")
			if err != nil {
				t.Fatal(err)
			}
			accountRepository.WithApprovalSnapshotSource(source)
			_, executorJWT, _ := newApprovalExecutorTokenHandler(t, tokenApp, requester.ID)
			rules, _ := json.Marshal([]map[string]any{{"subject": requester.ID.String(), "service": "token-command-executor", "permissions": []string{"internal.authorization.check", "internal.approval-execution.begin", "internal.approval-execution.complete", "internal.token.model-lifecycle.execute", "internal.token.catalog-rollback.execute", "internal.token.credential-revoke.execute"}}})
			t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(rules))
			accountApp := NewService(accountRepository, nil)
			verify := func(_ context.Context, raw string) (identity.Principal, error) {
				if raw == "requester" {
					return approvalIntegrationPrincipal(requester), nil
				}
				if raw == "approver" {
					return approvalIntegrationPrincipal(approver), nil
				}
				return identity.Principal{}, ErrForbidden
			}
			accountHandler := httpserver.CorrelationHandler(accountApp.Handler(verify))
			tokenHandler := httpserver.CorrelationHandler(tokenApp.Handler("", verify))
			var mode string
			var executePosts, beginPosts, completePosts int
			var lock sync.Mutex
			var trace []string
			var observedLease ApprovalExecution
			record := func(stage string) {
				lock.Lock()
				defer lock.Unlock()
				trace = append(trace, stage)
			}
			accountHandler = consumerChainTraceHandler(accountHandler, record, func(r *http.Request, status int, body []byte) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/executions") && status == http.StatusOK {
					lock.Lock()
					defer lock.Unlock()
					_ = json.Unmarshal(body, &observedLease)
				}
			})
			tokenHandler = consumerChainTraceHandler(tokenHandler, record, nil)
			accountServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/executions") {
					lock.Lock()
					beginPosts++
					lose := mode == "lose_begin"
					if lose {
						mode = ""
					}
					lock.Unlock()
					if lose {
						serveConsumerChainLostResponse(t, accountHandler, w, r)
						return
					}
				}
				if strings.HasSuffix(r.URL.Path, "/executions/complete") {
					lock.Lock()
					completePosts++
					lock.Unlock()
				}
				accountHandler.ServeHTTP(w, r)
			}))
			defer accountServer.Close()
			tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execute") {
					lock.Lock()
					executePosts++
					selectedMode := mode
					mode = ""
					lock.Unlock()
					if selectedMode == "epoch_barrier" {
						activation.set("SAFE_ROLLBACK", 2)
						activation.set("ACTIVE", 3)
					}
					if selectedMode == "lose_domain" {
						serveConsumerChainLostResponse(t, tokenHandler, w, r)
						return
					}
				}
				tokenHandler.ServeHTTP(w, r)
			}))
			defer tokenServer.Close()
			consumer, err := tokenservice.NewTokenApprovalConsumer(commandType, tokenServer.URL, accountServer.URL, executorJWT, activation)
			if err != nil {
				t.Fatal(err)
			}
			draft := seedConsumerChainDraft(t, ctx, connection, requester.ID, modelID, commandType)
			approved, event := createConsumerChainApproval(t, ctx, accountRepository, accountServer.URL, draft)
			// Deliberately discard the committed domain response. The production
			// consumer must GET that original result, complete Account and ACK.
			lock.Lock()
			mode = "lose_domain"
			lock.Unlock()
			if err = consumer.Handle(ctx, event); err != nil {
				lock.Lock()
				stages := append([]string(nil), trace...)
				counts := fmt.Sprintf("begin=%d domain=%d complete=%d", beginPosts, executePosts, completePosts)
				checks := consumerLeaseBindingChecks(event, observedLease)
				lock.Unlock()
				t.Fatalf("first delivery: %v; %s; lease_checks=%v; HTTP stages=%v", err, counts, checks, stages)
			}
			assertConsumerChainCompleted(t, ctx, accountRepository, connection, approved, 1)
			before := consumerChainEvidence(t, ctx, accountRepository, connection, approved.ID)
			if err = consumer.Handle(ctx, event); err != nil {
				t.Fatalf("duplicate delivery: %v", err)
			}
			if after := consumerChainEvidence(t, ctx, accountRepository, connection, approved.ID); before != after {
				t.Fatal("duplicate changed durable approval/domain/audit/outbox facts")
			}
			lock.Lock()
			posts := executePosts
			lock.Unlock()
			if posts != 1 {
				t.Fatalf("domain POST count=%d", posts)
			}
			if commandType != "TOKEN_CREDENTIAL_REVOKE" {
				return
			}
			for _, failure := range []string{"lose_begin", "epoch_barrier"} {
				activation.set("ACTIVE", 1)
				failedDraft := seedConsumerChainDraft(t, ctx, connection, requester.ID, modelID, commandType)
				pending, pendingEvent := createConsumerChainApproval(t, ctx, accountRepository, accountServer.URL, failedDraft)
				beforeFailure := consumerChainDomainEvidence(t, ctx, connection)
				lock.Lock()
				mode = failure
				beforePosts := executePosts
				lock.Unlock()
				if err = consumer.Handle(ctx, pendingEvent); err == nil {
					t.Fatalf("%s unexpectedly acknowledged", failure)
				}
				if err = consumer.Handle(ctx, pendingEvent); err == nil {
					t.Fatalf("%s repeat unexpectedly acknowledged", failure)
				}
				var commands, executions int
				var status string
				if err = connection.QueryRow(ctx, `SELECT count(*) FROM token_command_results WHERE approval_request_id=$1`, pending.ID).Scan(&commands); err != nil {
					t.Fatal(err)
				}
				if err = accountRepository.pool.QueryRow(ctx, `SELECT a.status,(SELECT count(*) FROM approval_executions e WHERE e.approval_request_id=a.id) FROM approval_requests a WHERE a.id=$1`, pending.ID).Scan(&status, &executions); err != nil {
					t.Fatal(err)
				}
				lock.Lock()
				delta := executePosts - beforePosts
				lock.Unlock()
				wanted := 0
				if failure == "epoch_barrier" {
					wanted = 1
				}
				if commands != 0 || executions != 1 || status != "EXECUTING" || delta != wanted {
					t.Fatalf("%s commands=%d executions=%d status=%s POSTs=%d", failure, commands, executions, status, delta)
				}
				var credentialStatus string
				if err = connection.QueryRow(ctx, `SELECT status FROM provider_credentials WHERE id=$1::uuid`, pending.TargetResourceID).Scan(&credentialStatus); err != nil || credentialStatus != "RETIRED" {
					t.Fatalf("%s changed the original credential: status=%s err=%v", failure, credentialStatus, err)
				}
				if after := consumerChainDomainEvidence(t, ctx, connection); beforeFailure != after {
					t.Fatalf("%s mutated domain/command/audit/outbox facts despite unresolved execution", failure)
				}
			}
		})
	}
}

// Log only transport paths, statuses, key names/nullability and the boolean
// first-claim discriminator. Never print commands, credentials or raw bodies.
func consumerChainTraceHandler(handler http.Handler, record func(string), capture func(*http.Request, int, []byte)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		if capture != nil {
			capture(r, rec.Code, rec.Body.Bytes())
		}
		var object map[string]json.RawMessage
		var keys []string
		claim := "absent"
		if json.Unmarshal(rec.Body.Bytes(), &object) == nil {
			for key, value := range object {
				if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					keys = append(keys, key+"=null")
				} else {
					keys = append(keys, key)
				}
			}
			if value, ok := object["first_claim"]; ok {
				var first bool
				if json.Unmarshal(value, &first) == nil {
					claim = strconv.FormatBool(first)
				}
			}
		}
		sort.Strings(keys)
		record(fmt.Sprintf("%s %s -> %d keys=%v first_claim=%s", r.Method, r.URL.Path, rec.Code, keys, claim))
		for key, value := range rec.Header() {
			w.Header()[key] = value
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

// Mirror the binding predicates only for safe diagnostics: values and frozen
// payloads stay in memory; the failure output contains field names and booleans.
func consumerLeaseBindingChecks(event events.Event, lease ApprovalExecution) map[string]bool {
	var selected ApprovalExecution
	decoded := json.Unmarshal(event.Payload, &selected) == nil
	return map[string]bool{
		"event_decoded":                  decoded,
		"approval_request_id":            lease.ApprovalRequestID == selected.ApprovalRequestID,
		"command_type":                   lease.CommandType == selected.CommandType,
		"policy_version":                 lease.PolicyVersion == selected.PolicyVersion,
		"command_fingerprint":            lease.CommandFingerprint == selected.CommandFingerprint,
		"executor_identity":              lease.ExecutorIdentity == "token-command-executor",
		"target_resource_type":           lease.TargetResourceType == selected.TargetResourceType,
		"target_resource_id":             lease.TargetResourceID == selected.TargetResourceID,
		"requester_nonzero":              lease.RequesterUserID != uuid.Nil,
		"approver_nonzero":               lease.ApproverUserID != uuid.Nil,
		"actors_different":               lease.RequesterUserID != lease.ApproverUserID,
		"authorization_version_positive": lease.RequesterAuthorizationVersion > 0,
		"reason_nonempty":                strings.TrimSpace(lease.RequesterReason) != "",
		"execution_status":               lease.Status == "EXECUTING" || lease.Status == "EXECUTED",
		"execution_idempotency_key":      lease.ExecutionIdempotencyKey == fmt.Sprintf("approval:%s:%s:%s", selected.ApprovalRequestID, selected.CommandType, selected.CommandFingerprint),
		"normalized_command":             sameJSONObject(lease.NormalizedCommand, selected.NormalizedCommand),
		"critical_resource_versions":     sameJSONObject(lease.CriticalResourceVersions, selected.CriticalResourceVersions),
		"financial_snapshot":             sameJSONObject(lease.FinancialSnapshot, selected.FinancialSnapshot),
	}
}

func serveConsumerChainLostResponse(t *testing.T, handler http.Handler, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Errorf("response-loss fixture got non-success %d: %s", rec.Code, rec.Body.String())
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	connection, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	_ = connection.Close()
}

func consumerChainRequest(t *testing.T, ctx context.Context, target, bearer string, body any) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+bearer)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("HTTP %s=%d %s", request.URL.Path, response.StatusCode, result)
	}
	return result
}

func createConsumerChainApproval(t *testing.T, ctx context.Context, repo *PostgresRepository, base string, draft ApprovalDraft) (ApprovalRequest, events.Event) {
	t.Helper()
	var item ApprovalRequest
	if err := json.Unmarshal(consumerChainRequest(t, ctx, base+"/admin/v1/approval-requests", "requester", approvalDraftBodyFromModel(draft)), &item); err != nil {
		t.Fatal(err)
	}
	consumerChainRequest(t, ctx, base+"/admin/v1/approval-requests/"+item.ID.String()+"/actions/submit", "requester", map[string]any{"expected_version": 1})
	if err := json.Unmarshal(consumerChainRequest(t, ctx, base+"/admin/v1/approval-requests/"+item.ID.String()+"/actions/decide", "approver", map[string]any{"expected_version": 2, "decision": "APPROVED", "reason": "independent consumer integration review", "observed_versions": item.CriticalResourceVersions}), &item); err != nil {
		t.Fatal(err)
	}
	var event events.Event
	if err := repo.pool.QueryRow(ctx, `SELECT event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at FROM outbox_events WHERE event_type='account.approval-command.authorized.v1' AND aggregate_id=$1`, item.ID).Scan(&event.ID, &event.Type, &event.AggregateID, &event.AggregateVersion, &event.Payload, &event.TraceID, &event.OccurredAt); err != nil {
		t.Fatal(err)
	}
	return item, event
}

func seedConsumerChainDraft(t *testing.T, ctx context.Context, c *pgx.Conn, actor, model uuid.UUID, command string) ApprovalDraft {
	t.Helper()
	draft := ApprovalDraft{CommandType: command, IdempotencyKey: "consumer-" + uuid.NewString(), Reason: "independently reviewed consumer integration action", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`), CriticalResourceVersions: json.RawMessage(`{}`), FinancialSnapshot: json.RawMessage(`{}`)}
	switch command {
	case "TOKEN_MODEL_LIFECYCLE_CHANGE":
		plan, version, _, effective := seedApprovalSnapshotLifecyclePlan(t, ctx, c, actor, model)
		draft.TargetResourceType, draft.TargetResourceID = "MODEL", model.String()
		draft.NormalizedCommand, _ = json.Marshal(map[string]any{"model_id": model, "lifecycle_plan_id": plan, "lifecycle_plan_version": version, "target_lifecycle": "DEPRECATED", "effective_at": effective, "existing_session_cutoff_at": nil, "replacement_model_id": nil})
	case "TOKEN_CREDENTIAL_REVOKE":
		accountID, credentialID := uuid.New(), uuid.New()
		_, err := c.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Consumer credential','ACTIVE',$4,$5,$5,'PLATFORM'); INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at,validation_status) VALUES($6,$1,'integration','https://provider.invalid','secret'::bytea,'integration-key',$7,1,'RETIRED',ARRAY['model-one'],$4,$5,'VALID')`, pgx.QueryExecModeSimpleProtocol, accountID, uuid.New(), uuid.New(), actor, time.Now().UTC(), credentialID, strings.Repeat("d", 64))
		if err != nil {
			t.Fatal(err)
		}
		draft.TargetResourceType, draft.TargetResourceID = "PROVIDER_CREDENTIAL", credentialID.String()
		draft.NormalizedCommand, _ = json.Marshal(map[string]any{"token_account_id": accountID, "expected_account_version": 1, "credential_id": credentialID, "credential_version": 1})
	case "TOKEN_MODEL_CATALOG_ROLLBACK":
		var source, current int64
		var protocol uuid.UUID
		if err := c.QueryRow(ctx, `SELECT catalog_version,protocol_definition_id FROM model_catalog_entries WHERE model_id=$1 ORDER BY catalog_version DESC LIMIT 1`, model).Scan(&source, &protocol); err != nil {
			t.Fatal(err)
		}
		accountID, credentialID, owner, wallet := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		_, err := c.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Consumer rollback','ACTIVE',$4,$5,$5,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($6,$1,'integration','https://provider.invalid','x'::bytea,'integration-key',$7,1,'ACTIVE',ARRAY['account-snapshot-model'],$5,$4,$5,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,model_id,protocol_version_id,reference_status,route_version,route_generation,health_version,health_observed_at,health_expires_at,health_diagnostic_code,health_failure_count,next_probe_at) VALUES($8,$1,'account-snapshot-model','CHAT',100,'NONE','HEALTHY',$5,$9,$10,'RESOLVED',1,1,1,$5,$5::timestamptz+interval '4 minutes','PROBE_SUCCEEDED',0,$5::timestamptz+interval '3 minutes');
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,attempt_count,repair_round,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at,protocol_version_id) VALUES($11,'PERSONAL',$2,$3,$9,'account-snapshot-model','CHAT',1,1,$7,'READY','READY',1,'{}',0,0,3,$5::timestamptz+interval '1 hour',$1,$6,$4,$5,$5,$10)`, pgx.QueryExecModeSimpleProtocol, accountID, owner, wallet, actor, time.Now().UTC(), credentialID, strings.Repeat("a", 64), uuid.New(), model, protocol, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		if err = c.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'consumer current',$2,$3,$3) RETURNING version`, uuid.New(), actor, time.Now().UTC()).Scan(&current); err != nil {
			t.Fatal(err)
		}
		if _, err = c.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) SELECT $1,model_id,'Consumer current','consumer current',description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available FROM model_catalog_entries WHERE catalog_version=$2`, current, source); err != nil {
			t.Fatal(err)
		}
		draft.TargetResourceType, draft.TargetResourceID = "MODEL_CATALOG", strconv.FormatInt(source, 10)
		draft.NormalizedCommand, _ = json.Marshal(map[string]any{"source_catalog_version": source})
	}
	return draft
}

func assertConsumerChainCompleted(t *testing.T, ctx context.Context, repo *PostgresRepository, c *pgx.Conn, item ApprovalRequest, want int) {
	t.Helper()
	var status, domainStatus string
	var executions, commands int
	if err := repo.pool.QueryRow(ctx, `SELECT a.status,(SELECT count(*) FROM approval_executions e WHERE e.approval_request_id=a.id AND e.status='EXECUTED') FROM approval_requests a WHERE a.id=$1`, item.ID).Scan(&status, &executions); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `SELECT count(*),COALESCE(min(status),'') FROM token_command_results WHERE approval_request_id=$1`, item.ID).Scan(&commands, &domainStatus); err != nil {
		t.Fatal(err)
	}
	if status != "EXECUTED" || executions != want || commands != want || domainStatus != "SUCCEEDED" {
		t.Fatalf("completion status=%s executions=%d commands=%d domain=%s", status, executions, commands, domainStatus)
	}
	var reference uuid.UUID
	if err := c.QueryRow(ctx, `SELECT command_reference FROM token_command_results WHERE approval_request_id=$1`, item.ID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	var audits, outbox int
	if err := c.QueryRow(ctx, `SELECT
(SELECT count(*) FROM token_audit_logs WHERE action=$2 AND summary->>'command_reference'=$1),
(SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1' AND payload->>'command_reference'=$1)`, reference.String(), item.CommandType).Scan(&audits, &outbox); err != nil {
		t.Fatal(err)
	}
	if audits != 1 || outbox != 1 {
		t.Fatalf("completed command generic audit/outbox=%d/%d, want exactly 1/1", audits, outbox)
	}
	var fact string
	switch item.CommandType {
	case "TOKEN_MODEL_LIFECYCLE_CHANGE":
		if err := c.QueryRow(ctx, `SELECT status FROM model_lifecycle_plans WHERE approval_request_id=$1`, item.ID).Scan(&fact); err != nil || fact != "SCHEDULED" {
			t.Fatalf("actual lifecycle plan effect=%s err=%v", fact, err)
		}
	case "TOKEN_CREDENTIAL_REVOKE":
		if err := c.QueryRow(ctx, `SELECT status FROM provider_credentials WHERE id=$1::uuid`, item.TargetResourceID).Scan(&fact); err != nil || fact != "REVOKED" {
			t.Fatalf("actual credential effect=%s err=%v", fact, err)
		}
	case "TOKEN_MODEL_CATALOG_ROLLBACK":
		var source int64
		if err := c.QueryRow(ctx, `SELECT rollback_source_version FROM model_catalog_versions ORDER BY version DESC LIMIT 1`).Scan(&source); err != nil || strconv.FormatInt(source, 10) != item.TargetResourceID {
			t.Fatalf("actual forward rollback source=%d err=%v", source, err)
		}
	}
}

func consumerChainEvidence(t *testing.T, ctx context.Context, repo *PostgresRepository, c *pgx.Conn, id uuid.UUID) string {
	t.Helper()
	var account string
	if err := repo.pool.QueryRow(ctx, `SELECT jsonb_build_object('request',(SELECT to_jsonb(a) FROM approval_requests a WHERE a.id=$1),'execution',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM approval_executions e WHERE e.approval_request_id=$1),'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM approval_state_events e WHERE e.approval_request_id=$1),'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY o.event_id) FROM outbox_events o WHERE o.aggregate_id=$1))::text`, id).Scan(&account); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s/%s", account, consumerChainDomainEvidence(t, ctx, c))
}

func consumerChainDomainEvidence(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	t.Helper()
	// This connection owns an isolated schema: compare complete ordered records,
	// not counts that could miss an in-place mutation or an unrelated replacement.
	var domain string
	if err := c.QueryRow(ctx, `SELECT jsonb_build_object(
'commands',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.command_reference),'[]'::jsonb) FROM token_command_results c),
'credentials',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]'::jsonb) FROM provider_credentials c),
'accounts',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.id),'[]'::jsonb) FROM token_accounts a),
'catalog_versions',(SELECT coalesce(jsonb_agg(to_jsonb(v) ORDER BY v.version),'[]'::jsonb) FROM model_catalog_versions v),
'catalog_entries',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.catalog_version,e.model_id),'[]'::jsonb) FROM model_catalog_entries e),
'plans',(SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY p.id),'[]'::jsonb) FROM model_lifecycle_plans p),
'plan_events',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM model_lifecycle_plan_events e),
'routes',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY r.id),'[]'::jsonb) FROM routing_policies r),
'provisionings',(SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY p.id),'[]'::jsonb) FROM route_provisionings p),
'audits',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.id),'[]'::jsonb) FROM token_audit_logs a),
'outbox',(SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY o.event_id),'[]'::jsonb) FROM outbox_events o))::text`).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	return domain
}
