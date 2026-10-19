package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type catalogWaitDeadlineProbe struct {
	provisioningOperations
	deadline time.Time
	observed bool
	err      error
}

func (p *catalogWaitDeadlineProbe) ContextEligible(ctx context.Context, _ AuthorizationContext, _ uuid.UUID, _ string) (bool, string, error) {
	p.deadline, p.observed = ctx.Deadline()
	select {
	case <-ctx.Done():
		p.err = ctx.Err()
	case <-time.After(250 * time.Millisecond):
		// A broken caller passing Background must fail, rather than hanging CI.
	}
	return false, "ROUTE_PROVISIONING_UNAVAILABLE", p.err
}

func TestCatalogWaitPassesBudgetAndCancellationToEligibility(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "budget", true: "parent cancellation"}[cancelled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelled {
				cancel()
			}
			probe := &catalogWaitDeadlineProbe{}
			started := time.Now()
			budget := 20 * time.Millisecond
			waitForCatalogRoutes(ctx, probe, AuthorizationContext{}, []CatalogModel{{SourceAvailable: true, LifecycleStatus: "PUBLISHED"}}, budget)
			if !probe.observed || probe.deadline.After(started.Add(budget+5*time.Millisecond)) || probe.err == nil {
				t.Fatalf("query did not receive original finite budget: deadline=%v observed=%v err=%v", probe.deadline, probe.observed, probe.err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("catalog wait exceeded bounded test window")
			}
		})
	}
}

// The public migration fixture and real provisioner exercise the same durable
// resources used by catalog discovery. Identity and provider health are controlled;
// this is not a deployed issuer/provider or a Billing balance acceptance test.
func TestContextEligibilityUsesCurrentScopedProductionRoutes(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	cfg := r.pool.Config().ConnConfig.Copy()
	cfg.User = "token_policy_operator"
	operator, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = operator.Exec(ctx, `SELECT * FROM public.apply_token_account_owner_cap_policy(100,NULL,$1,'controlled context eligibility',$2)`, "controlled-context-eligibility-key", uuid.New())
	operator.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"ready", "deprecated before cutoff", "another user", "organization", "personal with organization", "missing user", "missing model", "pending", "failed", "new pending generation", "actual generation drift", "wallet drift", "owner drift", "disabled account", "unhealthy", "unknown health", "health becomes ready", "expired health", "invalid credential", "credential model removed", "provider drift", "source missing", "removed model", "protocol mismatch", "protocol version drift", "configuration drift", "database unavailable"} {
		t.Run(name, func(t *testing.T) {
			user, wallet, modelID, capability := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			p, err := NewDefaultRouteProvisioner(r, ProviderURLPolicy{environment: "test", allowedHosts: map[string]bool{"example.invalid": true}}, DefaultRouteConfig{Provider: "controlled", AdapterVersion: "new-api-v1", BaseURL: "https://example.invalid", Secret: []byte("controlled-provider-secret"), Priority: 2})
			if err != nil {
				t.Fatal(err)
			}
			p.WithTokenAccountCreateCapability(true)
			protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
			model := CatalogModel{ModelID: modelID, ProtocolDefinitionID: protocol, ProtocolCode: "CHAT", ProviderID: "controlled", AdapterVersion: "new-api-v1", UpstreamModelRef: modelID.String(), LifecycleStatus: "PUBLISHED", SourceAvailable: true}
			for _, statement := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'controlled','new-api-v1',$2,$3,$4,now())`, []any{modelID, model.UpstreamModelRef, digestString([]byte(model.UpstreamModelRef)), user}},
				{`INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["TEXT"]','controlled',$4,now())`, []any{capability, modelID, protocol, user}},
			} {
				if _, err := r.pool.Exec(ctx, statement.sql, statement.args...); err != nil {
					t.Fatal(err)
				}
			}
			var version int64
			if err := r.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled context eligibility',$2,now(),now()) RETURNING version`, uuid.New(), user).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($1,$2,$3,$3,'PUBLISHED',$4,$5,true)`, version, modelID, model.UpstreamModelRef, capability, protocol); err != nil {
				t.Fatal(err)
			}
			resolved := AuthorizationContext{UserID: user, ContextType: "PERSONAL", ContextVersion: 3, AuthorizationVersion: 17}
			item, err := p.Ensure(ctx, resolved, wallet, model)
			if err != nil {
				t.Fatal(err)
			}
			claimed, attempt, ok := p.claim(ctx)
			if !ok || claimed.ID != item.ID {
				t.Fatal("actual provisioning claim unavailable")
			}
			p.execute(ctx, claimed, attempt)
			ready, err := p.Detail(ctx, item.ID)
			if err != nil || ready.Status != "READY" {
				t.Fatalf("actual provisioner did not complete: %s %v", ready.Status, err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE routing_policies SET health_status='HEALTHY',health_observed_at=now(),health_expires_at=now()+interval '2 minutes' WHERE token_account_id=$1`, ready.ActiveTokenAccountID); err != nil {
				t.Fatal(err)
			}

			changed := func(sql string, args ...any) {
				t.Helper()
				if _, err := r.pool.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			publishCatalog := func(lifecycle string, available bool) {
				t.Helper()
				var next int64
				if err := r.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled new catalog snapshot',$2,now(),now()) RETURNING version`, uuid.New(), user).Scan(&next); err != nil {
					t.Fatal(err)
				}
				changed(`INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,deprecation_effective_at,new_session_cutoff_at,continuation_cutoff_at) VALUES($1,$2,$3,$3,$4,$5,$6,$7,CASE WHEN $4='PUBLISHED' THEN NULL ELSE now() END,now()+interval '1 hour',now()+interval '2 hours')`, next, modelID, model.UpstreamModelRef, lifecycle, capability, protocol, available)
			}
			want := name == "ready" || name == "deprecated before cutoff" || name == "health becomes ready"
			switch name {
			case "deprecated before cutoff":
				publishCatalog("DEPRECATED", true)
			case "another user":
				resolved.UserID = uuid.New()
			case "organization":
				resolved.ContextType = "ORGANIZATION"
				resolved.OrganizationID = ptrUUID(uuid.New())
			case "personal with organization":
				resolved.OrganizationID = ptrUUID(uuid.New())
			case "missing user":
				resolved.UserID = uuid.Nil
			case "missing model":
				model.ModelID = uuid.Nil
			case "pending", "failed":
				changed(`UPDATE route_provisionings SET status=$2 WHERE id=$1`, item.ID, strings.ToUpper(name))
			case "new pending generation":
				changed(`INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,requested_by,created_at,updated_at) SELECT $2,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation+1,repeat('b',64),'PENDING','QUEUED',policy_version,policy_snapshot,max_attempts,sla_deadline_at,requested_by,created_at,updated_at FROM route_provisionings WHERE id=$1`, item.ID, uuid.New())
			case "actual generation drift":
				changed(`UPDATE route_provisionings SET actual_generation=actual_generation+1 WHERE id=$1`, item.ID)
			case "wallet drift":
				changed(`UPDATE token_accounts SET wallet_id=$2 WHERE id=$1`, ready.ActiveTokenAccountID, uuid.New())
			case "owner drift":
				changed(`UPDATE token_accounts SET owner_id=$2 WHERE id=$1`, ready.ActiveTokenAccountID, uuid.New())
			case "disabled account":
				changed(`UPDATE token_accounts SET status='DISABLED' WHERE id=$1`, ready.ActiveTokenAccountID)
			case "unhealthy":
				changed(`UPDATE routing_policies SET health_status='UNHEALTHY',health_expires_at=NULL WHERE token_account_id=$1`, ready.ActiveTokenAccountID)
			case "unknown health", "health becomes ready":
				changed(`UPDATE routing_policies SET health_status='UNKNOWN',health_expires_at=NULL WHERE token_account_id=$1`, ready.ActiveTokenAccountID)
				if name == "health becomes ready" {
					done := make(chan error, 1)
					go func() {
						time.Sleep(75 * time.Millisecond)
						_, err := r.pool.Exec(ctx, `UPDATE routing_policies SET health_status='HEALTHY',health_observed_at=now(),health_expires_at=now()+interval '2 minutes' WHERE token_account_id=$1`, ready.ActiveTokenAccountID)
						done <- err
					}()
					operations, ok := any(p).(provisioningOperations)
					if !ok {
						t.Fatal("production eligibility contract missing")
					}
					waitForCatalogRoutes(ctx, operations, resolved, []CatalogModel{model}, time.Second)
					eligible, _, err := operations.ContextEligible(ctx, resolved, modelID, "CHAT")
					if err != nil || !eligible {
						t.Error("catalog stopped waiting before controlled health became ready", err)
					}
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
			case "expired health":
				changed(`UPDATE routing_policies SET health_observed_at=now()-interval '2 minutes',health_expires_at=now()-interval '1 second' WHERE token_account_id=$1`, ready.ActiveTokenAccountID)
			case "invalid credential":
				changed(`UPDATE provider_credentials SET validation_status='INVALID' WHERE id=$1`, ready.ActiveCredentialID)
			case "credential model removed":
				changed(`UPDATE provider_credentials SET allowed_models=ARRAY['other'] WHERE id=$1`, ready.ActiveCredentialID)
			case "provider drift":
				p.config.Provider = "other"
			case "source missing":
				publishCatalog("PUBLISHED", false)
			case "removed model":
				publishCatalog("REMOVED", true)
			case "protocol mismatch":
				model.ProtocolCode = "OTHER"
			case "protocol version drift":
				changed(`UPDATE route_provisionings SET protocol_version_id=NULL WHERE id=$1`, item.ID)
			case "configuration drift":
				p.config.Priority++
			}

			// Read-only model permission deliberately omits invoke: the real HTTP
			// producer must report eligibility without provisioning or charging.
			service, _, closeServer := newModelUsageAuthorizationService(t, resolved)
			defer closeServer()
			service.WithManagement(r, nil, ProviderURLPolicy{}, "controlled-service", successfulBillingVerify).WithDefaultRoutes(p)
			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			request.Header.Set("Authorization", "Bearer controlled-user")
			response := httptest.NewRecorder()
			if name == "ready" {
				service.Handler(successfulBillingVerify).ServeHTTP(response, request)
				var payload struct {
					Items []struct {
						Eligible bool   `json:"context_eligible"`
						Reason   string `json:"eligibility_reason"`
					} `json:"items"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || response.Code != http.StatusOK || len(payload.Items) != 1 || !payload.Items[0].Eligible || payload.Items[0].Reason != "READY" {
					t.Fatalf("production catalog rejected real ready route: status=%d body=%s err=%v", response.Code, response.Body.String(), err)
				}
				admin := httptest.NewRecorder()
				adminRequest := httptest.NewRequest(http.MethodGet, "/admin/v1/provisionings/"+item.ID.String(), nil)
				adminRequest.Header.Set("Authorization", "Bearer controlled-user")
				service.Handler(successfulBillingVerify).ServeHTTP(admin, adminRequest)
				if admin.Code != http.StatusOK || !strings.Contains(admin.Body.String(), item.ID.String()) {
					t.Fatalf("real admin provisioning interface unavailable: %d %s", admin.Code, admin.Body.String())
				}
			}
			operations, supported := any(p).(provisioningOperations)
			if !supported {
				t.Fatal("production provisioner does not implement its HTTP operations contract")
			}
			before := retainedDefaultFacts(t, ctx, r, *ready.ActiveTokenAccountID)
			queryCtx := ctx
			if name == "database unavailable" {
				var cancel func()
				queryCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			eligible, reason, eligibilityErr := operations.ContextEligible(queryCtx, resolved, model.ModelID, model.ProtocolCode)
			if eligible != want || (want && reason != "READY") || (name == "database unavailable" && eligibilityErr == nil) || (name != "database unavailable" && eligibilityErr != nil) {
				t.Fatalf("eligibility=%v reason=%q err=%v want=%v", eligible, reason, eligibilityErr, want)
			}
			if before != retainedDefaultFacts(t, ctx, r, *ready.ActiveTokenAccountID) {
				t.Fatal("read-only eligibility changed account/credential/routes")
			}
			// No outstanding work may bleed into the next case's real claim.
			changed(`UPDATE route_provisionings SET status='REPAIR_REQUIRED',next_retry_at=NULL,lease_until=NULL WHERE scope_id=$1 AND status IN ('PENDING','FAILED')`, user)
		})
	}
}
