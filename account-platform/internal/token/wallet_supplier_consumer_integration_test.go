package token

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokenauthority"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Reject external, credential-bearing or ordinary/default instances before any
// connection, role creation or database write. Both domains share one explicitly
// disposable instance; their actual UUID databases remain separate.
func walletConsumerFixtureConfiguration(tokenRaw, billingRaw string) (*pgx.ConnConfig, error) {
	var first *pgx.ConnConfig
	for _, raw := range []string{tokenRaw, billingRaw} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "postgresql" && u.Scheme != "postgres") || u.Hostname() != "127.0.0.1" || u.Path != "/postgres" || u.User == nil || u.User.Username() != "postgres" || u.Fragment != "" {
			return nil, errors.New("supplier fixture requires an explicit disposable local admin URL")
		}
		_, hasPassword := u.User.Password()
		port, err := strconv.Atoi(u.Port())
		query := u.Query()
		if hasPassword || err != nil || port < 1024 || port > 65535 || port == 5432 || len(query) != 1 || query.Get("sslmode") != "disable" || len(query["sslmode"]) != 1 {
			return nil, errors.New("supplier fixture refuses credentials, default ports or extra connection options")
		}
		config, err := pgx.ParseConfig(raw)
		if err != nil || config.Host != "127.0.0.1" || config.Port != uint16(port) || config.User != "postgres" || config.Password != "" || config.Database != "postgres" || config.TLSConfig != nil || len(config.Fallbacks) != 0 {
			return nil, errors.New("supplier fixture connection configuration is not local and explicit")
		}
		if first != nil && first.Port != config.Port {
			return nil, errors.New("supplier fixture domains must use the same disposable instance")
		}
		first = config
	}
	return first, nil
}

func TestTokenWalletSupplierFixtureConfiguration(t *testing.T) {
	good := "postgresql://postgres@127.0.0.1:61846/postgres?sslmode=disable"
	for _, raw := range []string{"", "postgresql://postgres@db.invalid:61846/postgres?sslmode=disable", "postgresql://postgres:secret@127.0.0.1:61846/postgres?sslmode=disable", "postgresql://postgres@127.0.0.1:5432/postgres?sslmode=disable", "postgresql://postgres@127.0.0.1:61846/existing?sslmode=disable", good + "&options=-csearch_path=other", strings.Replace(good, "61846", "61847", 1)} {
		if _, err := walletConsumerFixtureConfiguration(good, raw); err == nil {
			t.Fatalf("unsafe or unrelated instance accepted: %q", raw)
		}
	}
	if _, err := walletConsumerFixtureConfiguration(good, good); err != nil {
		t.Fatal("explicit same local disposable instance rejected", err)
	}
}

func walletConsumerBillingActors(t *testing.T) {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" || os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL") == "" {
		t.Skip("actual dedicated PostgreSQL is required")
	}
	config, err := walletConsumerFixtureConfiguration(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"), raw)
	if err != nil {
		t.Fatal(err)
	}
	c, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	roles := []string{"billing_service", "billing_schema_owner", "billing_migrator"}
	var count int
	allActors := append(append([]string(nil), roles...), "token_gateway", "token_cap_policy_deployer", "token_schema_owner", "token_migrator", "token_policy_operator")
	var adminIdentity bool
	if err = c.QueryRow(context.Background(), `SELECT current_user='postgres' AND session_user='postgres' AND current_database()='postgres', (SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)) + (SELECT count(*) FROM pg_database WHERE datname NOT IN ('postgres','template0','template1'))`, allActors).Scan(&adminIdentity, &count); err != nil || !adminIdentity || count != 0 {
		t.Fatal("fixture requires pristine owned instance and absent domain actors; no existing actor writes", count, err)
	}
	tx, err := c.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(context.Background(), `CREATE ROLE billing_service LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; CREATE ROLE billing_schema_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; CREATE ROLE billing_migrator LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; GRANT billing_service,billing_schema_owner TO billing_migrator; GRANT billing_service TO billing_schema_owner WITH INHERIT TRUE,SET TRUE`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var external bool
		err := c.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND dbid<>0) OR EXISTS(SELECT 1 FROM pg_auth_members WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))) AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))))`, roles).Scan(&external)
		if err != nil || external {
			t.Error("external dependencies: owned actors retained", err)
			return
		}
		for i := len(roles) - 1; i >= 0; i-- {
			if _, err = c.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{roles[i]}.Sanitize()); err != nil {
				t.Error(err)
			}
		}
	})
}

func walletConsumerWorkload(t *testing.T) func(string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "wallet-fixture", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(jwks.Close)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "ca.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: jwks.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := jwks.URL + "/realms/workload"
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", jwks.URL)
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "wallet-fixture")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"token","service":"token-gateway","permissions":["internal.billing.token-account-wallet-provision.execute","internal.billing.token-account-wallet-provision.read"]},{"subject":"read","service":"token-gateway","permissions":["internal.billing.token-account-wallet-provision.read"]},{"subject":"execute","service":"token-gateway","permissions":["internal.billing.token-account-wallet-provision.execute"]},{"subject":"wrong","service":"account-service","permissions":["internal.billing.wallet.ensure"]}]`)
	return func(subject string) string {
		if subject == "" {
			return ""
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "wallet-fixture"))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "wallet-fixture", "sub": subject, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
		v, err := signer.Sign(raw)
		if err != nil {
			t.Fatal(err)
		}
		s, err := v.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
}

// Actual governed precheck/command producer, persistent Token worker, signed
// Billing client and public migrations run as the non-owner runtime roles.
// The Account authority and request identity are controlled external fixtures.
// Controlled authority HTTP service: real Token authority client validates every
// echoed request digest and identity digest; this is not a real Account service.
func TestTokenWalletSupplierActualProvisioningConsumerPostgres(t *testing.T) {
	walletConsumerBillingActors(t)
	r, ctx := retainedManagementTokenRepository(t)
	br, _, bp := retainedManagementBillingRepository(t)
	signed := walletConsumerWorkload(t)
	actor := uuid.New()
	var permitted atomic.Bool
	permitted.Store(true)
	s := f04AuthorizedService(t, r, &actor, &permitted)
	var deny atomic.Bool
	authorityServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		var req OwnerAuthorityCheck
		if json.NewDecoder(q.Body).Decode(&req) != nil || req.Validate() != nil {
			http.Error(w, "invalid authority request", 400)
			return
		}
		owner := uuid.MustParse(*req.OwnerID)
		status, version, decision := "ACTIVE", "1", "ALLOW"
		var reason *string
		if req.Mode == tokenauthority.ModeT3CreatorRecheck && deny.Load() {
			status, version, decision = "DISABLED", "2", "DENY"
			v := "OWNER_INACTIVE"
			reason = &v
		}
		ownerDigest := tokenauthority.OwnerIdentityDigest(*req.OwnerType, owner, status, func() int64 {
			if version == "2" {
				return 2
			}
			return 1
		}())
		res := OwnerAuthorityDecision{AuthoritySchemaVersion: tokenauthority.SchemaVersion, Mode: req.Mode, Decision: decision, ReasonCode: reason, ActorUserID: actor.String(), ExpectedPlatformAuthorizationVersion: req.ExpectedPlatformAuthorizationVersion, ObservedPlatformAuthorizationVersion: "19", ExpectedActorVersion: req.ExpectedActorVersion, ExpectedActorIdentityDigest: req.ExpectedActorIdentityDigest, ObservedActorVersion: "1", ActorStatus: "ACTIVE", ActorIdentityDigest: tokenauthority.ActorIdentityDigest(actor, "ACTIVE", 1), ExpectedOwnerVersion: req.ExpectedOwnerVersion, ExpectedOwnerIdentityDigest: req.ExpectedOwnerIdentityDigest, ObservedOwnerVersion: &version, OwnerStatus: &status, OwnerIdentityDigest: &ownerDigest, CommandReference: req.CommandReference, CommandRequestDigest: req.CommandRequestDigest, AuthorityRequestSHA256: req.AuthorityRequestSHA256, ObservedAt: time.Now().UTC()}
		json.NewEncoder(w).Encode(res)
	}))
	defer authorityServer.Close()
	authority, err := NewHTTPOwnerAuthorityClient(authorityServer.URL, signed("token"))
	if err != nil {
		t.Fatal(err)
	}
	var posts, gets atomic.Int32
	var lose atomic.Bool
	handler := billing.NewService(br).Handler("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "GET" {
			gets.Add(1)
		} else if q.Method == "POST" {
			posts.Add(1)
			if lose.Swap(false) {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, q)
				if rec.Code != 201 {
					t.Errorf("before lost ACK: %d %s", rec.Code, rec.Body.String())
				}
				c, _, e := w.(http.Hijacker).Hijack()
				if e != nil {
					t.Error(e)
				} else {
					c.Close()
				}
				return
			}
		}
		handler.ServeHTTP(w, q)
	}))
	defer server.Close()
	wallets, err := NewHTTPWalletResolver(server.URL, signed("token"))
	if err != nil {
		t.Fatal(err)
	}
	s.WithTokenAccountProvisioning(wallets, authority)
	s.WithTokenAccountCreateCapability(true)
	digest := strings.Repeat("a", 64)
	store, err := r.GovernanceStore(digest, digest)
	if err != nil {
		t.Fatal(err)
	}
	cfg := r.pool.Config().ConnConfig.Copy()
	cfg.User = "token_migrator"
	owner, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err = owner.Exec(ctx, `SET ROLE token_schema_owner`); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `UPDATE token_command_governance_activation SET state='ACTIVE',epoch=1,handler_set_digest=$1,policy_set_digest=$1,approved_runtime_roles=ARRAY['token_gateway'],operator_id='controlled-fixture',reason='Actual supplier consumer regression',request_id=$2 WHERE singleton`, digest, uuid.New()); err != nil {
		t.Fatal(err)
	}
	cfg.User = "token_policy_operator"
	op, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = op.Exec(ctx, `SELECT * FROM public.apply_token_account_owner_cap_policy(20,NULL,$1,'controlled supplier consumer',$2)`, "wallet-consumer-cap-key", uuid.New())
	op.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.routeActivationGuard = store
	pre := f04IdentityHandler(http.HandlerFunc(s.adminTokenCommandPrecheck))
	post := f04IdentityHandler(approvedCommandFixtureHandler(s, "/admin/v1/commands", s.adminCreateTokenCommand))
	s.WithTokenCommandGovernanceActivation(store)
	worker, err := NewTokenAccountProvisioningWorker(r, wallets, authority, "actual-supplier-consumer", TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"normal-activates", "lost-ack-reads-original-receipt", "newer-owner-deny-does-not-activate"} {
		t.Run(name, func(t *testing.T) {
			deny.Store(false)
			user := uuid.New()
			command := governedAccountCreateCommand{OwnerType: "USER", OwnerID: user, Name: name, ShareScope: "OWNER_UNIT_ONLY", DefaultPriority: 100}
			in := TokenCommandPrecheckRequest{CommandType: "TOKEN_ACCOUNT_CREATE", TargetResourceType: "TOKEN_ACCOUNT", TargetResourceID: user.String(), Command: mustMarshalJSON(t, command)}
			p := f04Request(pre, "POST", "/precheck", mustMarshalJSON(t, in), "")
			var projection TokenCommandPrecheckResult
			if p.Code != 200 || json.Unmarshal(p.Body.Bytes(), &projection) != nil || len(projection.Blockers) != 0 {
				t.Fatalf("precheck %d %s", p.Code, p.Body.String())
			}
			envelope := modelAccountEnvelope(t, in.CommandType, in.TargetResourceType, in.TargetResourceID, in.Command, "Controlled supplier consumer", projection)
			key := "wallet-consumer-" + uuid.NewString()
			body := mustMarshalJSON(t, envelope)
			res := f04Request(post, "POST", "/admin/v1/commands", body, key)
			var receipt TokenCommandResult
			if (res.Code != 200 && res.Code != 201 && res.Code != 202) || json.Unmarshal(res.Body.Bytes(), &receipt) != nil || receipt.Status != "PENDING" {
				t.Fatalf("submit %d %s", res.Code, res.Body.String())
			}
			if name == "lost-ack-reads-original-receipt" {
				lose.Store(true)
			}
			if name == "newer-owner-deny-does-not-activate" {
				// Persist via actual signed supplier first. Historical success remains a
				// receipt fact; current authoritative owner evidence has now advanced.
				if _, err := wallets.ProvisionTokenAccountWallet(ctx, receipt.CommandReference, "PERSONAL", user, walletProvisioningDigest("PERSONAL", user)); err != nil {
					t.Fatal(err)
				}
				deny.Store(true)
			}
			beforePosts, beforeGets := posts.Load(), gets.Load()
			did, err := worker.ProcessOne(ctx)
			if !did || err != nil {
				t.Fatal("actual ProcessOne", did, err)
			}
			var result, reservation string
			var count int
			if err = r.pool.QueryRow(ctx, `SELECT c.status,p.status,(SELECT count(*) FROM token_accounts WHERE owner_type='USER' AND owner_id=$2) FROM token_command_results c JOIN token_account_provisioning_reservations p USING(command_reference) WHERE c.command_reference=$1`, receipt.CommandReference, user).Scan(&result, &reservation, &count); err != nil {
				t.Fatal(err)
			}
			if name == "newer-owner-deny-does-not-activate" {
				if result != "FAILED" || reservation != "FAILED" || count != 0 {
					t.Fatalf("old SUCCEEDED wallet revived new owner: %s %s %d", result, reservation, count)
				}
			} else if result != "SUCCEEDED" || reservation != "COMPLETED" || count != 1 {
				t.Fatalf("consumer terminal %s %s accounts%d", result, reservation, count)
			}
			var walletsCount, receiptsCount, liabilities int
			if err = bp.QueryRow(ctx, `SELECT (SELECT count(*) FROM wallets WHERE owner_type='PERSONAL' AND owner_id=$1),(SELECT count(*) FROM token_account_wallet_provisioning_receipts WHERE owner_id=$1),(SELECT count(*) FROM ledger_accounts WHERE wallet_id IN(SELECT id FROM wallets WHERE owner_id=$1))`, user).Scan(&walletsCount, &receiptsCount, &liabilities); err != nil {
				t.Fatal(err)
			}
			if walletsCount != 1 || receiptsCount != 1 || liabilities != 1 {
				t.Fatalf("duplicated supplier effects: %d %d %d", walletsCount, receiptsCount, liabilities)
			}
			if name == "lost-ack-reads-original-receipt" && (posts.Load() != beforePosts+1 || gets.Load() != beforeGets+1) {
				t.Fatal("lost ACK did not use POST then GET", posts.Load()-beforePosts, gets.Load()-beforeGets)
			}
			replay := f04Request(post, "POST", "/admin/v1/commands", body, key)
			var recovered TokenCommandResult
			if replay.Code != 200 || json.Unmarshal(replay.Body.Bytes(), &recovered) != nil || recovered.CommandReference != receipt.CommandReference || recovered.Status != result {
				t.Fatalf("same key terminal recovery: %d %s", replay.Code, replay.Body.String())
			}
			t.Logf("actual ProcessOne command=%s result=%s reservation=%s TokenAccounts=%d BillingWallets=%d receipts=%d liabilities=%d", receipt.CommandReference, result, reservation, count, walletsCount, receiptsCount, liabilities)
		})
	}
}
