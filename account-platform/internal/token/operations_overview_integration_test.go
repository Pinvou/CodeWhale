package token

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTokenOperationsOverviewPostgresIntegration is a release gate. CI must
// run it against the complete Token migration chain without SKIP.
func TestTokenOperationsOverviewPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	now := time.Now().UTC()
	modelID, capabilityID, catalogID := uuid.New(), uuid.New(), uuid.New()
	accountID, credentialID, routeID := uuid.New(), uuid.New(), uuid.New()
	ownerID, walletID, actorID := uuid.New(), uuid.New(), uuid.New()
	protocolID := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'overview',$2,$3,$3) RETURNING version`, catalogID, actorID, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("1", 64)
	if _, err := repository.pool.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'provider','adapter-v1','overview-model',$2,$3,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($5,$1,1,$6,128000,4096,'["TEXT"]','v1',$3,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($7,$1,'Overview Model','overview model','PUBLISHED',$5,$6,true);
INSERT INTO model_catalog_drafts(model_id,base_catalog_version,draft_version,display_name,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at) VALUES($1,$7,1,'Overview Draft','CHAT',1,128000,4096,'["TEXT"]','v1',$3,$4);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($8,'USER',$9,$10,'Overview Account','ACTIVE',$3,$4,$4,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($11,$8,'provider','https://provider.invalid','x'::bytea,'key','fingerprint',1,'ACTIVE',ARRAY['overview-model'],$4,$3,$4,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,model_id,protocol_version_id,protocol_code,priority,default_scope,reference_status,health_status,health_observed_at,health_expires_at,route_version,route_generation,updated_at) VALUES($12,$8,'overview-model',$1,$6,'CHAT',100,'NONE','RESOLVED','HEALTHY',$4,$4::timestamptz+interval '4 minutes',1,1,$4);
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES($13,'PERSONAL',$9,$10,$1,'overview-model','CHAT',$6,1,$2,'FAILED','VERIFY',1,'{}',3,$4,$8,$11,$3,$4,$4);
INSERT INTO usage_requests(id,user_id,session_id,token_account_id,credential_id,wallet_id,model,status,maximum_microcredits,request_snapshot,started_at,updated_at,idempotency_key_digest,request_fingerprint,reconciliation_stopped_at) VALUES($14,$9,'overview-session',$8,$11,$10,'overview-model','REVIEW',100,'{}',$4::timestamptz-interval '1 hour',$4,$2,$2,NULL)`,
		modelID, digest, actorID, now, capabilityID, protocolID, catalogVersion, accountID, ownerID, walletID, credentialID, routeID, uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	values, err := repository.AdminTokenOperationsOverview(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if values.PublishedModels != 1 || values.ModelDrafts != 1 || values.StableRoutes != 1 || values.HealthyRoutes != 1 || values.RouteAttention != 0 || values.ProvisioningFailedOrRepair != 1 || values.UsageRequests != 1 || values.ReconciliationPending != 1 || values.WindowTo.Sub(values.WindowFrom) != 24*time.Hour {
		t.Fatalf("unexpected overview: %+v", values)
	}
	explicitFrom, explicitTo := now.Add(-2*time.Hour), now.Add(-30*time.Minute)
	explicit, err := repository.AdminTokenOperationsOverview(ctx, &explicitFrom, &explicitTo)
	if err != nil || explicit.UsageRequests != 1 || !explicit.WindowFrom.Equal(explicitFrom) || !explicit.WindowTo.Equal(explicitTo) {
		t.Fatalf("explicit window overview=%+v err=%v", explicit, err)
	}
}
