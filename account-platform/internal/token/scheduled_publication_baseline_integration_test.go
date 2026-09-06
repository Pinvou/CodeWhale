package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestScheduledPublicationBaselinePostgresIntegration is independent of the
// legacy lifecycle fixture. Catalog entries are always legal, drafts remain
// separate records, and pricing is an explicit fixed evidence fixture.
func TestScheduledPublicationBaselinePostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, tokenPaginationIntegrationDatabaseURL(t))
	activation := TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}
	ctx = context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: activation, transactional: true})
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(_ context.Context, _ pgx.Tx, selected TokenCommandGovernanceActivation) error {
		if selected != activation {
			return ErrTokenRouteActivationChanged
		}
		return nil
	}))
	actor := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var catalog int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'scheduled publication baseline fixture',$2,$3,$3) RETURNING version`, uuid.New(), actor, now).Scan(&catalog); err != nil {
		t.Fatal(err)
	}
	var protocol uuid.UUID
	if err := repository.pool.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE protocol_code='CHAT' AND version=1 AND status='PUBLISHED'`).Scan(&protocol); err != nil {
		t.Fatal(err)
	}
	models := make([]uuid.UUID, 8)
	states := []string{"PUBLISHED", "DEPRECATED", "REMOVED", "PUBLISHED", "PUBLISHED", "", "PUBLISHED", "PUBLISHED"}
	for i, state := range states {
		models[i] = uuid.New()
		seedScheduledPublicationBaseline(t, ctx, repository, actor, models[i], protocol, catalog, state, now)
	}
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, nil, NewHTTPUpstreamClient(), 0).
		WithImpactPrechecks(true, repository, lifecyclePricingStub{observedAt: now})
	// HEAD's helper need not include the uncommitted APT-138 middleware fix.
	handler := httpserver.CorrelationHandler(newGovernedPublishIntegrationHandler(t, service, actor))
	freeze := func(t *testing.T, model uuid.UUID, planVersion int64) (TokenCommandEnvelope, uuid.UUID) {
		t.Helper()
		id := uuid.New()
		effective := now.Add(time.Hour).Truncate(time.Second)
		command := modelPublishCommand{ModelID: model, PublishMode: "SCHEDULED", LifecyclePlanID: &id, LifecyclePlanVersion: &planVersion, EffectiveAt: &effective}
		raw := mustMarshalJSON(t, command)
		precheck, err := service.buildTokenCommandPrecheck(ctx, TokenCommandPrecheckRequest{CommandType: "TOKEN_MODEL_PUBLISH", TargetResourceType: "MODEL", TargetResourceID: model.String(), Command: raw})
		if err != nil || len(precheck.Blockers) != 0 {
			t.Fatalf("baseline precheck: err=%v blockers=%v", err, precheck.Blockers)
		}
		var expected map[string]any
		if err := json.Unmarshal(precheck.ExpectedVersions, &expected); err != nil {
			t.Fatal(err)
		}
		return bindModelPublishRequestFingerprint(t, TokenCommandEnvelope{CommandType: "TOKEN_MODEL_PUBLISH", TargetResourceType: "MODEL", TargetResourceID: model.String(), Reason: "schedule reviewed independent draft publication", Command: raw, PrecheckFingerprint: precheck.PrecheckFingerprint}, expected), id
	}
	invoke := func(t *testing.T, envelope TokenCommandEnvelope, key string, want int) TokenCommandResult {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/commands", bytes.NewReader(mustMarshalJSON(t, envelope))).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer scheduled-publication-fixture")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var result TokenCommandResult
		if response.Code != want || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Status != "SUCCEEDED" || result.CommandReference == uuid.Nil {
			t.Fatalf("scheduled publication HTTP=%d want=%d body=%s", response.Code, want, response.Body.String())
		}
		return result
	}
	reject := func(t *testing.T, envelope TokenCommandEnvelope, want error) {
		t.Helper()
		before := scheduledPublicationFacts(t, ctx, repository, false)
		_, created, err := repository.ScheduleModelPublish(ctx, tokenCommandBegin{Envelope: envelope, ActorUserID: actor, ActorPermission: "platform.models.manage", AuthorizationVersion: 7, IdempotencyKey: "schedule-reject-" + uuid.NewString(), AuditRequestID: uuid.NewString()})
		if !errors.Is(err, want) || created {
			t.Fatalf("expected precise schedule rejection %v, got created=%v err=%v", want, created, err)
		}
		if after := scheduledPublicationFacts(t, ctx, repository, false); before != after {
			t.Fatal("rejected schedule mutated persistent domain/command/audit/outbox facts")
		}
	}
	t.Run("wrong_activation_epoch_is_read_only", func(t *testing.T) {
		envelope, _ := freeze(t, models[0], 1)
		wrongContext := context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 2}, transactional: true})
		before := scheduledPublicationFacts(t, ctx, repository, false)
		result, created, err := repository.ScheduleModelPublish(wrongContext, tokenCommandBegin{Envelope: envelope, ActorUserID: actor, ActorPermission: "platform.models.manage", AuthorizationVersion: 7, IdempotencyKey: "schedule-wrong-epoch-" + uuid.NewString(), AuditRequestID: uuid.NewString()})
		if !errors.Is(err, ErrTokenRouteActivationChanged) || created || result.CommandReference != uuid.Nil {
			t.Fatalf("wrong epoch must reject before recording a command: created=%v err=%v", created, err)
		}
		if after := scheduledPublicationFacts(t, ctx, repository, false); before != after {
			t.Fatal("wrong epoch changed persistent domain/command/audit/outbox facts")
		}
	})
	var original TokenCommandEnvelope
	var originalResult TokenCommandResult
	var originalKey string
	for i, state := range states[:3] {
		t.Run("catalog_"+state, func(t *testing.T) {
			envelope, id := freeze(t, models[i], 1)
			key := "scheduled-baseline-" + uuid.NewString()
			beforeCatalog := scheduledPublicationFacts(t, ctx, repository, true)
			result := invoke(t, envelope, key, http.StatusCreated)
			plan, err := repository.LifecyclePlan(ctx, models[i], id)
			if err != nil || plan.CurrentLifecycle != "DRAFT" || plan.TargetLifecycle != "PUBLISHED" || plan.Status != "SCHEDULED" || plan.ExpectedCatalogVersion != catalog || plan.GovernedCommandReference == nil || *plan.GovernedCommandReference != result.CommandReference {
				t.Fatalf("independent draft plan semantics invalid: %+v err=%v", plan, err)
			}
			if after := scheduledPublicationFacts(t, ctx, repository, true); beforeCatalog != after {
				t.Fatal("scheduling prematurely published catalog or consumed draft")
			}
			var audits, outbox, events, plans int
			if err := repository.pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_MODEL_PUBLISH' AND summary->>'command_reference'=$1::text),
(SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1' AND payload->>'command_reference'=$1::text),
(SELECT count(*) FROM model_lifecycle_plan_events WHERE plan_id=$2 AND event_type='CREATED'),
(SELECT count(*) FROM model_lifecycle_plans WHERE governed_command_reference=$1::uuid)`, result.CommandReference.String(), id).Scan(&audits, &outbox, &events, &plans); err != nil {
				t.Fatal(err)
			}
			if audits != 1 || outbox != 1 || events != 1 || plans != 1 {
				t.Fatalf("schedule facts audit/outbox/event/plan=%d/%d/%d/%d", audits, outbox, events, plans)
			}
			beforeReplay := scheduledPublicationFacts(t, ctx, repository, false)
			replayed := invoke(t, envelope, key, http.StatusOK)
			if replayed.CommandReference != result.CommandReference || beforeReplay != scheduledPublicationFacts(t, ctx, repository, false) {
				t.Fatal("duplicate scheduling changed facts or original command reference")
			}
			if i == 0 {
				original, originalResult, originalKey = envelope, result, key
			}
		})
	}
	t.Run("active_plan_conflict", func(t *testing.T) {
		envelope, _ := freeze(t, models[0], 2)
		reject(t, envelope, ErrCatalogVersionConflict)
	})
	t.Run("missing_draft", func(t *testing.T) {
		envelope, _ := freeze(t, models[3], 1)
		if _, err := repository.pool.Exec(ctx, `DELETE FROM model_catalog_drafts WHERE model_id=$1`, models[3]); err != nil {
			t.Fatal(err)
		}
		reject(t, envelope, ErrNotFound)
	})
	t.Run("draft_changed", func(t *testing.T) {
		envelope, _ := freeze(t, models[4], 1)
		if _, err := repository.pool.Exec(ctx, `UPDATE model_catalog_drafts SET draft_version=draft_version+1,description='changed after precheck',updated_at=transaction_timestamp() WHERE model_id=$1`, models[4]); err != nil {
			t.Fatal(err)
		}
		reject(t, envelope, ErrCatalogVersionConflict)
	})
	t.Run("missing_catalog_entry", func(t *testing.T) {
		envelope, _ := freeze(t, models[5], 1)
		reject(t, envelope, ErrCatalogVersionConflict)
	})
	t.Run("provisioning_not_ready", func(t *testing.T) {
		envelope, _ := freeze(t, models[6], 1)
		if _, err := repository.pool.Exec(ctx, `UPDATE route_provisionings SET status='FAILED',stage='FAILED',updated_at=transaction_timestamp() WHERE model_id=$1`, models[6]); err != nil {
			t.Fatal(err)
		}
		reject(t, envelope, ErrCatalogVersionConflict)
	})
	t.Run("existing_first_after_draft_drift", func(t *testing.T) {
		if _, err := repository.pool.Exec(ctx, `UPDATE model_catalog_drafts SET draft_version=draft_version+1,description='newer unscheduled draft',updated_at=transaction_timestamp() WHERE model_id=$1`, models[0]); err != nil {
			t.Fatal(err)
		}
		before := scheduledPublicationFacts(t, ctx, repository, false)
		result := invoke(t, original, originalKey, http.StatusOK)
		if result.CommandReference != originalResult.CommandReference || before != scheduledPublicationFacts(t, ctx, repository, false) {
			t.Fatal("old request replay after draft drift was not read-only")
		}
	})
	t.Run("catalog_baseline_drift", func(t *testing.T) {
		envelope, _ := freeze(t, models[7], 1)
		var next int64
		if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'subsequent catalog fixture',$2,$3,$3) RETURNING version`, uuid.New(), actor, now).Scan(&next); err != nil || next <= catalog {
			t.Fatalf("new catalog version: %d err=%v", next, err)
		}
		reject(t, envelope, ErrCatalogVersionConflict)
	})
}

func seedScheduledPublicationBaseline(t *testing.T, ctx context.Context, repository *PostgresRepository, actor, model, protocol uuid.UUID, catalog int64, state string, now time.Time) {
	t.Helper()
	capability, account, owner, wallet, credential := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	upstream := "scheduled-" + model.String()
	digest := strings.ReplaceAll(model.String(), "-", "")
	_, err := repository.pool.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'integration','v1',$2,$3,$4,$5);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($6,$1,1,$7,4096,1024,'["TEXT"]','v1',$4,$5);
INSERT INTO model_catalog_drafts(model_id,base_catalog_version,draft_version,display_name,description,aliases,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at) VALUES($1,$8,1,$2,'independent draft','{}','CHAT',1,4096,1024,'["TEXT"]','v1',$4,$5);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($9,'USER',$10,$11,'Scheduled publish account','ACTIVE',$4,$5,$5,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($12,$9,'integration','https://provider.invalid','fixture'::bytea,'integration-key',$3,1,'ACTIVE',ARRAY[$2],$5,$4,$5,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,model_id,protocol_version_id,reference_status,route_version,route_generation,health_version,health_observed_at,health_expires_at,health_diagnostic_code,health_failure_count,next_probe_at) VALUES($13,$9,$2,'CHAT',100,'NONE','UNKNOWN',$5,$1,$7,'RESOLVED',1,1,1,$5,NULL,'NOT_YET_PROBED',0,$5);
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,attempt_count,repair_round,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES($14,'PERSONAL',$10,$11,$1,$2,'CHAT',$7,1,1,$3,'READY','READY',1,'{}',0,0,3,$15,$9,$12,$4,$5,$5)`, pgx.QueryExecModeSimpleProtocol, model, upstream, digest+digest, actor, now, capability, protocol, catalog, account, owner, wallet, credential, uuid.New(), uuid.New(), now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if state != "" {
		if _, err := repository.pool.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($1,$2,$3,$3,'current discovered model','{}',$4,$5,$6,true)`, catalog, model, upstream, state, capability, protocol); err != nil {
			t.Fatal(err)
		}
	}
}

func scheduledPublicationFacts(t *testing.T, ctx context.Context, repository *PostgresRepository, catalogOnly bool) string {
	t.Helper()
	var catalog string
	if err := repository.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'versions',(SELECT coalesce(jsonb_agg(to_jsonb(v) ORDER BY v.version),'[]'::jsonb) FROM model_catalog_versions v),
'entries',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.catalog_version,e.model_id),'[]'::jsonb) FROM model_catalog_entries e),
'drafts',(SELECT coalesce(jsonb_agg(to_jsonb(d) ORDER BY d.model_id),'[]'::jsonb) FROM model_catalog_drafts d),
'capabilities',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]'::jsonb) FROM model_capability_versions c),
'catalog_audit',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.id),'[]'::jsonb) FROM model_catalog_audit a))::text`).Scan(&catalog); err != nil {
		t.Fatal(err)
	}
	if catalogOnly {
		return catalog
	}
	var domain string
	if err := repository.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'plans',(SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY p.id),'[]'::jsonb) FROM model_lifecycle_plans p),
'events',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM model_lifecycle_plan_events e),
'commands',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.command_reference),'[]'::jsonb) FROM token_command_results c),
'audits',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.id),'[]'::jsonb) FROM token_audit_logs a),
'outbox',(SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY o.event_id),'[]'::jsonb) FROM outbox_events o),
'routes',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY r.id),'[]'::jsonb) FROM routing_policies r),
'provisionings',(SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY p.id),'[]'::jsonb) FROM route_provisionings p))::text`).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s/%s", catalog, domain)
}
