package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
)

// Real public migrations and storage; provider configuration is controlled.
// No provider request or ordinary chat producer is installed by these cases.
func TestRetainedDefaultRoutePreservesManagedFacts(t *testing.T) {
	repository, ctx := retainedManagementTokenRepository(t)
	// Initialize through the original privileged production function, never
	// direct INSERTs into policy revisions/pointer or runtime role escalation.
	operatorConfig := repository.pool.Config().ConnConfig.Copy()
	operatorConfig.User = "token_policy_operator"
	operator, err := pgx.ConnectConfig(ctx, operatorConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, err = operator.Exec(ctx, `SELECT * FROM public.apply_token_account_owner_cap_policy(20,NULL,$1,'controlled default route regression',$2)`, "controlled-default-cap-key", uuid.New())
	operator.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}

	protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	for _, test := range []struct {
		name      string
		config    []string
		change    string
		wantRoute bool
	}{
		{"empty configured models", nil, "", true},
		{"configured allowed models", []string{"configured"}, "", true},
		{"rotated credential", nil, "rotation", false},
		{"manual account", nil, "manual", false},
		{"reduced allowed models", nil, "reduced", false},
		{"disabled account", nil, "disabled", false},
		{"wallet reassigned", nil, "wallet", false},
		{"existing unhealthy route", nil, "route", true},
		{"deleted route", nil, "deleted", true},
		{"deleted route tombstone", nil, "tombstone", false},
		{"prior actual provisioning", nil, "priorActual", false},
		{"configuration changed", nil, "signature", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			user, wallet := uuid.New(), uuid.New()
			provider, err := NewDefaultRouteProvisioner(repository, ProviderURLPolicy{environment: "test", allowedHosts: map[string]bool{"example.invalid": true}}, DefaultRouteConfig{Provider: "controlled", AdapterVersion: "new-api-v1", BaseURL: "https://example.invalid", Secret: []byte("controlled-provider-secret"), Models: test.config, Priority: 2})
			if err != nil {
				t.Fatal(err)
			}
			provider.WithTokenAccountCreateCapability(true)
			resolved := AuthorizationContext{UserID: user, ContextType: "PERSONAL"}
			makeModel := func(ref string) CatalogModel {
				t.Helper()
				model := CatalogModel{ModelID: uuid.New(), ProtocolDefinitionID: protocol, ProtocolCode: "CHAT", ProviderID: "controlled", AdapterVersion: "new-api-v1", UpstreamModelRef: ref, LifecycleStatus: "PUBLISHED", SourceAvailable: true}
				sum := sha256.Sum256([]byte(user.String() + ref))
				digest := hex.EncodeToString(sum[:])
				if _, err := repository.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,now())`, model.ModelID, model.ProviderID, model.AdapterVersion, model.UpstreamModelRef, digest, user); err != nil {
					t.Fatal(err)
				}
				return model
			}
			first, target := makeModel("first-"+user.String()), makeModel("second-"+user.String())
			if err := provider.provisionResources(ctx, resolved, wallet, first, 3); err != nil {
				t.Fatal(err)
			}
			var account, credential uuid.UUID
			if err := repository.pool.QueryRow(ctx, `SELECT ta.id,pc.id FROM token_accounts ta JOIN provider_credentials pc ON pc.token_account_id=ta.id WHERE ta.owner_id=$1`, user).Scan(&account, &credential); err != nil {
				t.Fatal(err)
			}
			// The first completed automatic provisioning is durable provenance for
			// extending its exact initial allowed set, rather than a guessed signature.
			insertPrior := func(model CatalogModel, generation int64, signature string) {
				t.Helper()
				if _, err := repository.pool.Exec(ctx, `INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES($1,'PERSONAL',$2,$3,$4,$5,$6,$7,$8,$8,$9,'READY','VERIFIED',1,'{}',8,now()+interval '1 hour',$10,$11,$2,now(),now())`, uuid.New(), user, wallet, model.ModelID, model.UpstreamModelRef, model.ProtocolCode, protocol, generation, signature, account, credential); err != nil {
					t.Fatal(err)
				}
			}
			signature := provider.desiredSignature("PERSONAL", user, wallet, first)
			if test.change == "signature" {
				signature = strings.Repeat("0", 64)
			}
			insertPrior(first, 3, signature)
			switch test.change {
			case "rotation":
				_, err = repository.pool.Exec(ctx, `UPDATE provider_credentials SET version=2 WHERE id=$1`, credential)
			case "manual":
				_, err = repository.pool.Exec(ctx, `INSERT INTO token_audit_logs(id,actor_user_id,action,resource_type,resource_id,summary,occurred_at) VALUES($1,$2,'TOKEN_ACCOUNT_CREATED','TOKEN_ACCOUNT',$3,'{}',now())`, uuid.New(), user, account)
			case "reduced":
				_, err = repository.pool.Exec(ctx, `UPDATE provider_credentials SET allowed_models=ARRAY['different'] WHERE id=$1`, credential)
			case "disabled":
				_, err = repository.pool.Exec(ctx, `UPDATE token_accounts SET status='DISABLED' WHERE id=$1`, account)
			case "wallet":
				_, err = repository.pool.Exec(ctx, `UPDATE token_accounts SET wallet_id=$2 WHERE id=$1`, account, uuid.New())
			case "route", "deleted", "tombstone":
				var route uuid.UUID
				route = uuid.New()
				_, err = repository.pool.Exec(ctx, `INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,updated_by,route_generation,model_id,protocol_version_id,reference_status,route_version,health_version,health_diagnostic_code) VALUES($1,$2,$3,'CHAT',99,'NONE','UNHEALTHY',now(),$4,7,$5,$6,'RESOLVED',4,6,'CONTROLLED_UNHEALTHY')`, route, account, target.UpstreamModelRef, user, target.ModelID, protocol)
				if err == nil && test.change != "route" {
					_, err = repository.pool.Exec(ctx, `INSERT INTO routing_policy_tombstones(route_id,token_account_id,model_id,protocol_version_id,route_version,route_generation,deleted_by,reason,deleted_at) VALUES($1,$2,$3,$4,4,7,$5,'controlled removal',now())`, route, account, target.ModelID, protocol, user)
					if err == nil {
						_, err = repository.pool.Exec(ctx, `UPDATE routing_policies SET reference_status='DELETED',health_status='STALE' WHERE id=$1`, route)
					}
					if err == nil && test.change == "tombstone" {
						_, err = repository.pool.Exec(ctx, `DELETE FROM routing_policies WHERE id=$1`, route)
					}
				}
			case "priorActual":
				insertPrior(target, 2, provider.desiredSignature("PERSONAL", user, wallet, target))
			}
			if err != nil {
				t.Fatal(err)
			}
			before := retainedDefaultFacts(t, ctx, repository, account)
			err = provider.provisionResources(ctx, resolved, wallet, target, 8)
			if test.change == "disabled" || test.change == "wallet" {
				if err != ErrNoEligibleRoute {
					t.Fatalf("changed account accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			after := retainedDefaultFacts(t, ctx, repository, account)
			var routes int
			if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM routing_policies WHERE token_account_id=$1 AND model_id=$2`, account, target.ModelID).Scan(&routes); err != nil {
				t.Fatal(err)
			}
			if (routes == 1) != test.wantRoute {
				t.Fatalf("target route count=%d expected created=%v", routes, test.wantRoute)
			}
			if test.change != "" {
				if before != after {
					t.Fatalf("existing administrator facts changed for %s", test.change)
				}
			} else {
				// Only the allowed model set and the new route may change. Account,
				// sharing, credential ciphertext/version and old route remain identical.
				var modelPresent bool
				var generation int64
				if err := repository.pool.QueryRow(ctx, `SELECT $2=ANY(allowed_models) FROM provider_credentials WHERE id=$1`, credential, target.UpstreamModelRef).Scan(&modelPresent); err != nil || !modelPresent {
					t.Fatal("model not appended", err)
				}
				if err := repository.pool.QueryRow(ctx, `SELECT route_generation FROM routing_policies WHERE token_account_id=$1 AND model_id=$2`, account, target.ModelID).Scan(&generation); err != nil || generation != 8 {
					t.Fatalf("desired generation not retained: %d %v", generation, err)
				}
				var details map[string]json.RawMessage
				if err := json.Unmarshal([]byte(before), &details); err != nil {
					t.Fatal(err)
				}
				var actual map[string]json.RawMessage
				if err := json.Unmarshal([]byte(after), &actual); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"account", "share", "credentialImmutable", "firstRoutes"} {
					if string(details[key]) != string(actual[key]) {
						t.Fatalf("immutable %s changed", key)
					}
				}
			}
		})
	}
}

func retainedDefaultFacts(t *testing.T, ctx context.Context, r *PostgresRepository, account uuid.UUID) string {
	t.Helper()
	var raw string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('account',(SELECT to_jsonb(a) FROM token_accounts a WHERE a.id=$1),'share',(SELECT to_jsonb(s) FROM token_share_policies s WHERE s.token_account_id=$1),'credentialImmutable',(SELECT to_jsonb(c)-'allowed_models' FROM provider_credentials c WHERE c.token_account_id=$1),'allowed',(SELECT to_jsonb(c.allowed_models) FROM provider_credentials c WHERE c.token_account_id=$1),'firstRoutes',(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id) FROM routing_policies r WHERE r.token_account_id=$1 AND r.route_generation=3),'routes',(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id) FROM routing_policies r WHERE r.token_account_id=$1))::text`, account).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
