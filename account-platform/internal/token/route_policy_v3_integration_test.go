package token

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestRouteUpsertPolicyMatchesActualCommandFacts(t *testing.T) {
	policy, err := approvalpolicy.Lookup("TOKEN_ROUTE_UPSERT")
	if err != nil || policy.PolicyVersion != 3 || policy.SnapshotSchemaVersion != 3 || !reflect.DeepEqual(policy.CriticalResourceFields, tokenCommandSpecs["TOKEN_ROUTE_UPSERT"].RequiredFacts) {
		t.Fatalf("route policy must match the actual parser facts: %#v %v", policy, err)
	}
}

// Compatibility means a legally stored, frozen v2 command is replayed by the
// current real HTTP handler. This test does not claim to execute an old binary.
func TestLegacyRoutePolicyV2ReplayPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, tokenPaginationIntegrationDatabaseURL(t))
	repository.now = func() time.Time { return time.Now().UTC() }
	actor, account, owner, wallet, model, capability, credential := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	now := time.Now().UTC().Add(-time.Minute)
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'governed route test',$2,$3,$3) RETURNING version`, uuid.New(), actor, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	_, err := repository.pool.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'PROVIDER','adapter-v1','governed-model',$2,$3,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($5,$1,1,$6,128000,8192,'["TEXT"]','v1',$3,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($7,$1,'Governed Model','governed model','','{}','PUBLISHED',$5,$6,true);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($8,'USER',$9,$10,'Governed account','ACTIVE',$3,$4,$4,'PLATFORM');
INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($8,'OWNER_UNIT_ONLY',$4);
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($11,$8,'PROVIDER','https://provider.invalid','x'::bytea,'key','governed-fingerprint',1,'ACTIVE',ARRAY['governed-model'],$4,$3,$4,'VALID');`, pgx.QueryExecModeSimpleProtocol, model, strings.Repeat("1", 64), actor, now, capability, protocol, catalogVersion, account, owner, wallet, credential)
	if err != nil {
		t.Fatal(err)
	}

	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(_ context.Context, _ pgx.Tx, selected TokenCommandGovernanceActivation) error {
		if selected.State != "ACTIVE" || selected.Epoch != 7 {
			return ErrTokenRouteActivationChanged
		}
		return nil
	}))
	handler := newModelAccountGovernanceHandler(t, repository, nil, actor, AdminOwnerCandidate{ID: owner, Type: "USER", Status: "ACTIVE"})
	command := mustMarshalJSON(t, map[string]any{"token_account_id": account, "model_id": model, "protocol_version_id": protocol, "priority": 100, "default_scope": "NONE"})
	precheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), command)
	envelope := modelAccountEnvelope(t, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), command, "route policy version compatibility", precheck)
	response := performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", envelope, "route-policy-v3-control-key")
	var current TokenCommandResult
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &current) != nil || current.PolicyVersion != 3 || current.Status != "SUCCEEDED" {
		t.Fatalf("new route command must freeze v3: %d %s", response.Code, response.Body.String())
	}
	var route StableRouteResult
	if json.Unmarshal(current.RedactedResult, &route) != nil || route.AccountVersion != 2 || route.RouteVersion != 1 {
		t.Fatalf("invalid route result: %s", current.RedactedResult)
	}
	legacyKey := "route-policy-v2-stored-key"
	_, requestDigest, keyDigest, fingerprint, err := normalizeTokenCommand(envelope, legacyKey, "")
	if err != nil {
		t.Fatal(err)
	}
	legacyReference := uuid.New()
	// Insert a historical result; do not rewrite an immutable v3 row or
	// fabricate historical domain mutations, audit records, or outbox events.
	if _, err = repository.pool.Exec(ctx, `INSERT INTO token_command_results SELECT (jsonb_populate_record(NULL::token_command_results,to_jsonb(r)||jsonb_build_object('command_reference',$2::uuid,'policy_version',2,'idempotency_key_digest',$3::text,'idempotency_fingerprint',$4::text,'request_digest',$5::text))).* FROM token_command_results r WHERE command_reference=$1::uuid`, current.CommandReference, legacyReference, keyDigest, fingerprint, requestDigest); err != nil {
		t.Fatal(err)
	}

	assertFacts := func(effects, commandCount int, accountVersion, routeVersion int64) [4]string {
		t.Helper()
		var resultCount, routes, audits, events, mutations, tombstones int
		var actualAccountVersion, actualRouteVersion int64
		if err := repository.pool.QueryRow(ctx, `SELECT
    (SELECT count(*) FROM token_command_results),
    (SELECT count(*) FROM routing_policies WHERE token_account_id=$1),
    (SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_ROUTE_UPSERT'),
    (SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1'),
    (SELECT count(*) FROM routing_policy_mutations),
    (SELECT count(*) FROM routing_policy_tombstones),
    (SELECT management_version FROM token_accounts WHERE id=$1),
    (SELECT route_version FROM routing_policies WHERE id=$2)`, account, route.ID).Scan(&resultCount, &routes, &audits, &events, &mutations, &tombstones, &actualAccountVersion, &actualRouteVersion); err != nil || resultCount != commandCount || routes != 1 || audits != effects || events != effects || mutations != 0 || tombstones != 0 || actualAccountVersion != accountVersion || actualRouteVersion != routeVersion {
			t.Fatalf("route facts result/routes/audit/outbox/legacy/tombstones/account-version/route-version=%d/%d/%d/%d/%d/%d/%d/%d err=%v", resultCount, routes, audits, events, mutations, tombstones, actualAccountVersion, actualRouteVersion, err)
		}
		var evidence [4]string
		if err := repository.pool.QueryRow(ctx, `SELECT
    (SELECT to_jsonb(a)::text FROM token_accounts a WHERE id=$1),
    (SELECT to_jsonb(r)::text FROM routing_policies r WHERE id=$2),
    (SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY id),'[]'::jsonb)::text FROM token_audit_logs a),
    (SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY event_id),'[]'::jsonb)::text FROM outbox_events e)`, account, route.ID).Scan(&evidence[0], &evidence[1], &evidence[2], &evidence[3]); err != nil {
			t.Fatal(err)
		}
		return evidence
	}
	replayAll := func() {
		t.Helper()
		for _, lookup := range []struct {
			method, path, key string
			body              any
		}{
			{http.MethodGet, "/admin/v1/commands/" + legacyReference.String(), "", nil},
			{http.MethodGet, "/admin/v1/commands/by-idempotency-key?command_type=TOKEN_ROUTE_UPSERT", legacyKey, nil},
			{http.MethodPost, "/admin/v1/commands", legacyKey, envelope},
		} {
			got := performModelAccountRequest(handler, lookup.method, lookup.path, lookup.body, lookup.key)
			var result TokenCommandResult
			if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &result) != nil || result.CommandReference != legacyReference || result.PolicyVersion != 2 || result.Status != "SUCCEEDED" || string(result.RedactedResult) != string(current.RedactedResult) {
				t.Fatalf("historical v2 recovery changed: %s %d %s", lookup.method, got.Code, got.Body.String())
			}
		}
	}
	before := assertFacts(1, 2, 2, 1)
	replayAll()
	changed := envelope
	changed.Reason = "same old key with different reason"
	response = performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", changed, legacyKey)
	if response.Code != http.StatusConflict {
		t.Fatalf("changed payload=%d %s", response.Code, response.Body.String())
	}
	if after := assertFacts(1, 2, 2, 1); before != after {
		t.Fatal("v2 replay changed persisted route/audit/event evidence")
	}

	// A new key with fresh evidence uses v3 and makes exactly one new mutation.
	fresh := fetchModelAccountPrecheck(t, handler, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), command)
	next := modelAccountEnvelope(t, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), command, "new policy command", fresh)
	response = performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", next, "route-policy-v3-next-key")
	var updated TokenCommandResult
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &updated) != nil || updated.PolicyVersion != 3 || updated.CommandReference == legacyReference || updated.Status != "SUCCEEDED" {
		t.Fatalf("new key must use v3: %d %s", response.Code, response.Body.String())
	}
	afterUpdate := assertFacts(2, 3, 3, 2)
	replayAll() // Old precheck/versions are deliberately stale now.
	if afterReplay := assertFacts(2, 3, 3, 2); afterUpdate != afterReplay {
		t.Fatal("historical replay after mutation changed evidence")
	}

	// The forward policy revision does not re-enable text protocol inputs.
	invalid := next
	invalid.Command = mustMarshalJSON(t, map[string]any{"token_account_id": account, "model_id": model, "protocol_code": "CHAT", "priority": 100, "default_scope": "NONE"})
	response = performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", invalid, "route-policy-invalid-text-key")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("text protocol must remain invalid: %d %s", response.Code, response.Body.String())
	}
	if afterInvalid := assertFacts(2, 3, 3, 2); afterUpdate != afterInvalid {
		t.Fatal("invalid old protocol input changed evidence")
	}
}
