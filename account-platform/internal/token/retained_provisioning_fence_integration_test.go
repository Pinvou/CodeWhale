package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

// Public migrations and actual Ensure/claim/execute storage. The provider is
// controlled; no external provider or ordinary chat producer is involved.
func TestRetainedProvisioningCompletionFences(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	cfg := r.pool.Config().ConnConfig.Copy()
	cfg.User = "token_policy_operator"
	operator, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = operator.Exec(ctx, `SELECT * FROM public.apply_token_account_owner_cap_policy(20,NULL,$1,'controlled provisioning fence',$2)`, "controlled-provisioning-cap-key", uuid.New())
	operator.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wallet drift", "owner drift", "expired success", "expired failure", "lock waited success", "lock waited failure", "claim skips finishing request"} {
		t.Run(name, func(t *testing.T) {
			user, wallet := uuid.New(), uuid.New()
			p, err := NewDefaultRouteProvisioner(r, ProviderURLPolicy{environment: "test", allowedHosts: map[string]bool{"example.invalid": true}}, DefaultRouteConfig{Provider: "controlled", AdapterVersion: "new-api-v1", BaseURL: "https://example.invalid", Secret: []byte("controlled-provider-secret"), Priority: 2})
			if err != nil {
				t.Fatal(err)
			}
			p.WithTokenAccountCreateCapability(true)
			protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
			model := CatalogModel{ModelID: uuid.New(), ProtocolDefinitionID: protocol, ProtocolCode: "CHAT", ProviderID: "controlled", AdapterVersion: "new-api-v1", UpstreamModelRef: "fence-" + user.String(), LifecycleStatus: "PUBLISHED", SourceAvailable: true}
			digest := sha256.Sum256([]byte(model.UpstreamModelRef))
			if _, err = r.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,now())`, model.ModelID, model.ProviderID, model.AdapterVersion, model.UpstreamModelRef, hex.EncodeToString(digest[:]), user); err != nil {
				t.Fatal(err)
			}
			capability := uuid.New()
			var version int64
			if _, err = r.pool.Exec(ctx, `INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["TEXT"]','controlled',$4,now())`, capability, model.ModelID, protocol, user); err != nil {
				t.Fatal(err)
			}
			if err = r.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled fence',$2,now(),now()) RETURNING version`, uuid.New(), user).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if _, err = r.pool.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($1,$2,$3,$3,'PUBLISHED',$4,$5,true)`, version, model.ModelID, model.UpstreamModelRef, capability, protocol); err != nil {
				t.Fatal(err)
			}
			resolved := AuthorizationContext{UserID: user, ContextType: "PERSONAL"}
			item, err := p.Ensure(ctx, resolved, wallet, model)
			if err != nil {
				t.Fatal(err)
			}
			claimed, attempt, ok := p.claim(ctx)
			if !ok || claimed.ID != item.ID {
				t.Fatal("claim unavailable")
			}
			if name == "wallet drift" || name == "owner drift" {
				p.execute(ctx, claimed, attempt)
				ready, err := p.Detail(ctx, item.ID)
				if err != nil || ready.Status != "READY" {
					t.Fatalf("live completion did not become READY: %s %v", ready.Status, err)
				}
				if name == "wallet drift" {
					_, err = r.pool.Exec(ctx, `UPDATE token_accounts SET wallet_id=$2 WHERE id=$1`, *ready.ActiveTokenAccountID, uuid.New())
				} else {
					_, err = r.pool.Exec(ctx, `UPDATE token_accounts SET owner_id=$2 WHERE id=$1`, *ready.ActiveTokenAccountID, uuid.New())
				}
				if err != nil {
					t.Fatal(err)
				}
				before := retainedDefaultFacts(t, ctx, r, *ready.ActiveTokenAccountID)
				actual, err := p.Ensure(ctx, resolved, wallet, model)
				if err != nil {
					t.Fatal(err)
				}
				if actual.Status == "READY" {
					t.Error("Ensure returned READY for administrator-reassigned resource")
				}
				if before != retainedDefaultFacts(t, ctx, r, *ready.ActiveTokenAccountID) {
					t.Error("Ensure overwrote administrator facts")
				}
				// Consume this case's new pending generation through the real
				// worker failure path so unrelated cases cannot claim it.
				if actual.Status != "READY" {
					pending, a, ok := p.claim(ctx)
					if !ok || pending.ID != actual.ID {
						t.Fatal("drift generation claim unavailable")
					}
					p.failAttempt(ctx, pending, a, ErrForbidden, r.now())
				}
				return
			}
			if name == "lock waited success" || name == "lock waited failure" {
				if _, err = r.pool.Exec(ctx, `UPDATE route_provisionings SET lease_until=clock_timestamp()+interval '500 milliseconds' WHERE id=$1`, item.ID); err != nil {
					t.Fatal(err)
				}
				blocker, err := r.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = blocker.Exec(ctx, `SELECT id FROM route_provisionings WHERE id=$1 FOR UPDATE`, item.ID); err != nil {
					blocker.Rollback(ctx)
					t.Fatal(err)
				}
				var before string
				readFacts := func() string {
					var value string
					if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('provisioning',(SELECT to_jsonb(p) FROM route_provisionings p WHERE id=$1),'attempts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM route_provisioning_attempts a WHERE provisioning_id=$1),'audit',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM route_provisioning_audit a WHERE provisioning_id=$1))::text`, item.ID).Scan(&value); err != nil {
						t.Fatal(err)
					}
					return value
				}
				before = readFacts()
				done := make(chan struct{})
				go func() {
					defer close(done)
					if name == "lock waited success" {
						p.execute(ctx, claimed, attempt)
					} else {
						p.failAttempt(ctx, claimed, attempt, errors.New("controlled temporary failure"), r.now())
					}
				}()
				time.Sleep(650 * time.Millisecond)
				if err = blocker.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("completion did not return after releasing owned lock")
				}
				if before != readFacts() {
					t.Error("lease expired during lock wait but completion changed persistent result/audit")
				}
				next, a, ok := p.claim(ctx)
				if !ok || next.ID != item.ID {
					t.Fatal("expired case recovery unavailable")
				}
				p.failAttempt(ctx, next, a, ErrForbidden, r.now())
				return
			}
			// Controlled expiry changes only this owned lease, then the actual claim
			// producer records a new attempt; no completed result or authority is forged.
			if _, err = r.pool.Exec(ctx, `UPDATE route_provisionings SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, item.ID); err != nil {
				t.Fatal(err)
			}
			if name == "claim skips finishing request" {
				blocker, err := r.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer blocker.Rollback(ctx)
				if _, err = blocker.Exec(ctx, `SELECT id FROM route_provisionings WHERE id=$1 FOR UPDATE`, item.ID); err != nil {
					t.Fatal(err)
				}
				claimCtx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				done := make(chan bool, 1)
				go func() { _, _, ok := p.claim(claimCtx); done <- ok }()
				// An expired reclaimer must not take the attempt before the
				// finishing worker's request lock. That would invert the order.
				time.Sleep(150 * time.Millisecond)
				check, err := r.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, lockErr := check.Exec(ctx, `SELECT id FROM route_provisioning_attempts WHERE id=$1 FOR UPDATE NOWAIT`, attempt)
				check.Rollback(ctx)
				if lockErr != nil {
					t.Error("reclaimer locked attempt before the held request")
				}
				select {
				case ok := <-done:
					if ok {
						t.Error("claim acquired an owned finishing request")
					}
				case <-time.After(300 * time.Millisecond):
					t.Error("claim waited for finishing request instead of skipping it")
					if err = blocker.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					<-done
				}
				if err = blocker.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					t.Fatal(err)
				}
			}
			newer, newAttempt, ok := p.claim(ctx)
			if !ok || newer.ID != item.ID || newAttempt == attempt {
				t.Fatal("replacement claim unavailable")
			}
			facts := func() string {
				var s string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('provisioning',(SELECT to_jsonb(p) FROM route_provisionings p WHERE id=$1),'attempts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM route_provisioning_attempts a WHERE provisioning_id=$1),'audit',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM route_provisioning_audit a WHERE provisioning_id=$1))::text`, item.ID).Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			var resourceBefore string
			resourceFacts := func() string {
				var raw string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('accounts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM token_accounts a WHERE owner_id=$1),'routes',(SELECT jsonb_agg(to_jsonb(rp) ORDER BY rp.id) FROM routing_policies rp JOIN token_accounts a ON a.id=rp.token_account_id WHERE a.owner_id=$1),'credentials',(SELECT jsonb_agg(to_jsonb(pc) ORDER BY pc.id) FROM provider_credentials pc JOIN token_accounts a ON a.id=pc.token_account_id WHERE a.owner_id=$1))::text`, user).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				return raw
			}
			resourceBefore = resourceFacts()
			before := facts()
			if name == "expired success" {
				p.execute(ctx, claimed, attempt)
			} else {
				p.failAttempt(ctx, claimed, attempt, errors.New("controlled temporary failure"), r.now())
			}
			if before != facts() {
				t.Error("stale worker changed replacement lease/attempt or published false completion audit")
			}
			if resourceBefore != resourceFacts() {
				t.Error("lost lease changed account/credential/routes")
			}
			p.failAttempt(ctx, newer, newAttempt, ErrForbidden, r.now())
		})
	}
}
