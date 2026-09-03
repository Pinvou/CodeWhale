package token

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type routeHealthProbeStub struct{}

func (routeHealthProbeStub) AdapterVersion() string { return "adapter-v1" }

func (routeHealthProbeStub) DiscoverModels(context.Context, string, []byte) ([]DiscoveredModel, error) {
	return []DiscoveredModel{{UpstreamModelRef: "route-health-model"}}, nil
}

// TestRouteHealthPostgresIntegration exercises the APT-125 lease, monotonic
// completion and five-minute routability evidence against the real migration.
// CI supplies PINVOU_TEST_TOKEN_DATABASE_URL and rejects a skipped result.
func TestRouteHealthPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	repository.routeHealthMetrics = newRouteHealthMetrics()
	policy, err := NewProviderURLPolicy("development", []string{"provider.invalid"})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(-time.Minute)
	modelID, capabilityID, catalogID := uuid.New(), uuid.New(), uuid.New()
	accountID, ownerID, walletID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	credentialID, routeID := uuid.New(), uuid.New()
	protocolID := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'route health integration',$2,$3,$3) RETURNING version`, catalogID, actorID, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at)
VALUES($1,'provider','adapter-v1','route-health-model',$2,$3,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at)
VALUES($5,$1,1,$6,128000,8192,'["TEXT"]','v1',$3,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available)
VALUES($7,$1,'Route Health Model','route health model','','{}','PUBLISHED',$5,$6,true);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode)
VALUES($8,'USER',$9,$10,'Route health account','ACTIVE',$3,$4,$4,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status)
VALUES($11,$8,'provider','https://provider.invalid','x'::bytea,'key','route-health-fingerprint',1,'ACTIVE',ARRAY['route-health-model'],$4,$3,$4,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,updated_by,route_generation,model_id,protocol_version_id,reference_status,route_version,health_version,health_diagnostic_code,next_probe_at)
VALUES($12,$8,'route-health-model','CHAT',100,'NONE','UNKNOWN',$4,$3,1,$1,$6,'RESOLVED',1,1,'AWAITING_INITIAL_PROBE',$4);`, modelID, strings.Repeat("0", 64), actorID, now, capabilityID, protocolID, catalogVersion, accountID, ownerID, walletID, credentialID, routeID); err != nil {
		t.Fatal(err)
	}

	first, err := NewRouteHealthWorker(repository, routeHealthProbeStub{}, policy, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	target, ok := first.claim(ctx)
	if !ok || target.RouteID != routeID || target.HealthVersion != 2 {
		t.Fatalf("unexpected claimed target: %#v ok=%v", target, ok)
	}
	second, err := NewRouteHealthWorker(repository, routeHealthProbeStub{}, policy, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed := second.claim(ctx); claimed {
		t.Fatal("a second worker claimed an active probe lease")
	}
	// A route mutation that races an in-flight probe makes that completion stale.
	if _, err = repository.pool.Exec(ctx, `UPDATE routing_policies SET route_version=route_version+1 WHERE id=$1`, routeID); err != nil {
		t.Fatal(err)
	}
	first.complete(ctx, target, true, "PROBE_SUCCEEDED", 10*time.Millisecond)
	var observations int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM route_health_observations WHERE route_id=$1`, routeID).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("stale completion persisted observations=%d err=%v", observations, err)
	}
	// An expired lease is recoverable by a different worker, while preserving
	// the monotonic route and health versions used by the completion CAS.
	if _, err = repository.pool.Exec(ctx, `UPDATE routing_policies SET probe_lease_until=transaction_timestamp()-interval '1 second',next_probe_at=transaction_timestamp() WHERE id=$1`, routeID); err != nil {
		t.Fatal(err)
	}
	recovered, ok := second.claim(ctx)
	if !ok || recovered.RouteID != routeID || recovered.RouteVersion != 2 || recovered.HealthVersion != 3 {
		t.Fatalf("expired lease was not recovered monotonically: %#v ok=%v", recovered, ok)
	}
	second.complete(ctx, recovered, true, "PROBE_SUCCEEDED", 10*time.Millisecond)
	var status, diagnostic string
	var observedAt, expiresAt time.Time
	if err := repository.pool.QueryRow(ctx, `SELECT health_status,health_diagnostic_code,health_observed_at,health_expires_at FROM routing_policies WHERE id=$1`, routeID).Scan(&status, &diagnostic, &observedAt, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if status != "HEALTHY" || diagnostic != "PROBE_SUCCEEDED" || !expiresAt.After(observedAt) || expiresAt.Sub(observedAt) != routeHealthyTTL {
		t.Fatalf("unexpected healthy evidence: status=%s diagnostic=%s observed=%s expires=%s", status, diagnostic, observedAt, expiresAt)
	}
	accounts, err := repository.AdminTokenAccounts(ctx, 10)
	if err != nil || len(accounts) != 1 || accounts[0].ID != accountID {
		t.Fatalf("admin account list projection=%#v err=%v", accounts, err)
	}
	detail, err := repository.AdminTokenAccount(ctx, accountID)
	if err != nil || detail.AsOf.IsZero() || len(detail.Routes) != 1 || detail.Routes[0].HealthObservedAt == nil || detail.Routes[0].HealthObservedAt.After(detail.AsOf) {
		t.Fatalf("repeatable-read account detail=%#v err=%v", detail, err)
	}
	var authorizationVersion atomic.Int64
	authorizationVersion.Store(1)
	handler := newAdminQueryIntegrationHandler(t, repository, actorID, &authorizationVersion, map[string]bool{"platform.tokens.read": true})
	response := performAdminQueryRequest(handler, "/admin/v1/token-accounts/"+accountID.String())
	var projected AdminTokenAccountDetail
	if err := json.Unmarshal(response.Body.Bytes(), &projected); response.Code != http.StatusOK || err != nil || projected.AsOf.IsZero() || len(projected.Routes) != 1 || projected.Routes[0].ID != routeID {
		t.Fatalf("admin detail handler status=%d body=%s projection=%#v err=%v", response.Code, response.Body.String(), projected, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE routing_policies SET next_probe_at=transaction_timestamp(),probe_lease_owner=NULL,probe_lease_until=NULL WHERE id=$1`, routeID); err != nil {
		t.Fatal(err)
	}
	target, ok = first.claim(ctx)
	if !ok {
		t.Fatal("healthy route was not claimed for its next probe")
	}
	target.FailureCount = 2
	first.complete(ctx, target, false, "MODEL_NOT_DISCOVERED", 15*time.Millisecond)
	var failures int
	var unhealthyExpiry *time.Time
	if err := repository.pool.QueryRow(ctx, `SELECT health_status,health_failure_count,health_expires_at FROM routing_policies WHERE id=$1`, routeID).Scan(&status, &failures, &unhealthyExpiry); err != nil {
		t.Fatal(err)
	}
	if status != "UNHEALTHY" || failures != 3 || unhealthyExpiry != nil {
		t.Fatalf("unexpected unhealthy convergence: status=%s failures=%d expires=%v", status, failures, unhealthyExpiry)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM route_health_observations WHERE route_id=$1`, routeID).Scan(&observations); err != nil || observations != 2 {
		t.Fatalf("observations=%d err=%v", observations, err)
	}
	if err := repository.AdminDeleteRoute(ctx, accountID, "route-health-model", actorID); err != nil {
		t.Fatalf("v1 compatibility delete of a probed migrated route failed: %v", err)
	}
	var referenceStatus string
	if err := repository.pool.QueryRow(ctx, `SELECT reference_status FROM routing_policies WHERE id=$1`, routeID).Scan(&referenceStatus); err != nil || referenceStatus != "DELETED" {
		t.Fatalf("v1 delete did not preserve a tombstoned route: status=%s err=%v", referenceStatus, err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM route_health_observations WHERE route_id=$1`, routeID).Scan(&observations); err != nil || observations != 2 {
		t.Fatalf("v1 delete lost immutable observations=%d err=%v", observations, err)
	}
}
