package token

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The owned Account actor/database setup below follows the existing
// retained_wallet_fences fixture and installs only public Up migrations.
// Approval, authorization bytes, scheduling and emergency policies are always
// produced by the real repositories/HTTP consumer, never by fixture SQL.
type forceCancelAccountFixture struct {
	ctx    context.Context
	admin  *pgx.Conn
	config *pgx.ConnConfig
}

func forceCancelAccountActors(t *testing.T) *forceCancelAccountFixture {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("actual dedicated PostgreSQL required for wallet lease fences")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	admin, err := cifixture.Connect(ctx, raw)
	if err != nil {
		t.Fatal("open fixture setup actor")
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("parse fixture setup configuration")
	}
	roles := []string{"account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery"}
	var existing int
	if err = admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&existing); err != nil || existing != 0 {
		t.Fatal("fixed actor conflict: no setup writes")
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal("begin fixture actor transaction")
	}
	defer tx.Rollback(context.Background())
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&existing); err != nil || existing != 0 {
		t.Fatal("concurrent actor conflict")
	}
	for _, role := range roles {
		login, inherit := "LOGIN", "NOINHERIT"
		if strings.HasSuffix(role, "_owner") || role == "account_approval_recovery" {
			login = "NOLOGIN"
		}
		if role == "account_service" {
			inherit = "INHERIT"
		}
		if _, err = tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+login+" "+inherit+" NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal("owned actor setup failed")
		}
	}
	if _, err = tx.Exec(ctx, "GRANT account_service,account_approval_control_owner TO account_migrator; GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT FALSE,SET TRUE,ADMIN TRUE"); err != nil {
		t.Fatal("retained actor membership setup failed")
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal("commit owned actors")
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		var outside bool
		err := admin.QueryRow(clean, `SELECT EXISTS(SELECT 1 FROM pg_shdepend d WHERE refclassid='pg_authid'::regclass AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND dbid<>0) OR EXISTS(SELECT 1 FROM pg_auth_members a WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))) AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))))`, roles).Scan(&outside)
		if err != nil || outside {
			t.Error("external dependencies: owned roles retained")
			return
		}
		for i := len(roles) - 1; i >= 0; i-- {
			if _, err = admin.Exec(clean, "DROP ROLE "+pgx.Identifier{roles[i]}.Sanitize()); err != nil {
				t.Error("owned role cleanup failed")
			}
		}
		if err = admin.QueryRow(clean, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&existing); err != nil || existing != 0 {
			t.Error("owned actors remain")
		}
	})
	return &forceCancelAccountFixture{ctx, admin, config}
}

func (f *forceCancelAccountFixture) repository(t *testing.T) *account.PostgresRepository {
	return f.repositoryWithInitialStage(t, nil)
}

func (f *forceCancelAccountFixture) repositoryWithInitialStage(t *testing.T, initial func(context.Context, *pgx.Conn, []migrations.Migration) error) *account.PostgresRepository {
	t.Helper()
	ctx := f.ctx
	name := "f01_wallet_fence_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var fixtureDatabase *cifixture.Database
	quoted := pgx.Identifier{name}.Sanitize()
	fixtureDatabaseCreated, creationError := cifixture.CreateDatabase(ctx, f.admin, name, "CREATE DATABASE "+quoted+" OWNER account_service")
	fixtureDatabase = fixtureDatabaseCreated
	if err := creationError; err != nil {
		t.Fatal("owned database setup failed")
	}
	t.Logf("owned wallet fixture database=%s owner=account_service", name)
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := cifixture.DropDatabase(clean, f.admin, fixtureDatabase, "account_service"); err != nil {
			t.Error("nonforce owned database cleanup failed")
			return
		}
		var absent bool
		if err := f.admin.QueryRow(clean, `SELECT NOT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, name).Scan(&absent); err != nil || !absent {
			t.Error("owned database remains")
		}
	})
	if _, err := f.admin.Exec(ctx, "GRANT CREATE ON DATABASE "+quoted+" TO account_migrator"); err != nil {
		t.Fatal("original migration CREATE grant failed")
	}
	migrationConfig := f.config.Copy()
	migrationConfig.Database = name
	migrationConfig.User = "account_migrator"
	migrationConfig.Password = ""
	connection, err := pgx.ConnectConfig(ctx, migrationConfig)
	if err != nil {
		t.Fatal("open genuine migrator")
	}
	defer connection.Close(context.Background())
	if _, err = connection.Exec(ctx, "SET ROLE account_service"); err != nil {
		t.Fatal("select original schema owner")
	}
	var actor, session string
	var super bool
	if err = connection.QueryRow(ctx, `SELECT current_user,session_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &session, &super); err != nil || actor != "account_service" || session != "account_migrator" || super {
		t.Fatal("migration actor isolation not proven")
	}
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if initial != nil {
		if err = initial(ctx, connection, installed); err != nil {
			t.Fatal(err)
		}
	}
	if err = accountschema.Up(ctx, connection, installed); err != nil {
		t.Fatal(err)
	}
	if _, err = migrations.VerifyApplied(ctx, connection, installed); err != nil {
		t.Fatal(err)
	}
	configuration, err := pgxpool.ParseConfig(f.config.ConnString())
	if err != nil {
		t.Fatal("parse genuine runtime configuration")
	}
	configuration.MaxConns = 4
	configuration.ConnConfig.Database = name
	configuration.ConnConfig.User = "account_service"
	configuration.ConnConfig.Password = ""
	configuration.ConnConfig.RuntimeParams["application_name"] = "walletfence_" + name
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal("open genuine runtime")
	}
	t.Cleanup(pool.Close)
	if err = pool.QueryRow(ctx, `SELECT current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &super); err != nil || actor != "account_service" || super {
		t.Fatal("runtime actor isolation not proven")
	}
	runtime, err := account.NewPostgresRepository(ctx, forceCancelActorURL(t, f.config.ConnString(), name, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Close)
	return runtime
}

func signForceCancelJWT(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "approval-executor"})
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

type forceCancelActor struct {
	ID      uuid.UUID
	Subject string
}

func seedForceCancelActor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role, label string) forceCancelActor {
	t.Helper()
	actor := forceCancelActor{uuid.New(), label + "-" + uuid.NewString()}
	var digits strings.Builder
	digits.WriteString("USR")
	for _, b := range actor.ID {
		digits.WriteByte('1' + b%9)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO users(id,status,display_name,username,user_code,created_at,updated_at) VALUES($1,'ACTIVE',$2,$2,$3||user_code_checksum($3),now(),now())`, actor.ID, actor.Subject, digits.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('https://entitlement-owned.invalid',$1,$2,now(),now())`, actor.Subject, actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,$3,'ACCOUNT',now()-interval '1 day',now())`, uuid.New(), actor.ID, role); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return actor
}

func forceCancelPrincipal(actor forceCancelActor) identity.Principal {
	return identity.Principal{Issuer: "https://entitlement-owned.invalid", Subject: actor.Subject, SessionID: actor.Subject + "-session", AuthenticatedAt: time.Now().UTC()}
}

func configureForceCancelIdentity(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			http.NotFound(w, r)
			return
		}
		modulus := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
		exponent := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "approval-executor", "n": modulus, "e": exponent,
		}}})
	}))
	t.Cleanup(issuer.Close)
	caFile := filepath.Join(t.TempDir(), "issuer-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	audience := "entitlement-cross-domain-integration"
	accountSubject, executorSubject := uuid.NewString(), uuid.NewString()
	rules, err := json.Marshal([]map[string]any{
		{
			"subject": accountSubject, "service": "account-service",
			"permissions": []string{"internal.token.approval-proof.read", "internal.token.approval-snapshot.read", "internal.token.governance.read"},
		},
		{
			"subject": executorSubject, "service": "token-command-executor",
			"permissions": []string{
				"internal.authorization.check", "internal.approval-authorization.consume", "internal.token.model-lifecycle.execute", "internal.billing.pricing.read", "internal.billing.hold.reserve", "internal.billing.hold.release", "internal.billing.hold.commit",
				"internal.approval-execution.begin", "internal.approval-execution.complete",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", caFile)
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer.URL)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", audience)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", issuer.URL+"/jwks")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(rules))
	mint := func(subject string) string {
		now := time.Now().UTC()
		token, signErr := signForceCancelJWT(key, map[string]any{
			"iss": issuer.URL, "sub": subject, "aud": audience, "exp": now.Add(10 * time.Minute).Unix(), "iat": now.Unix(),
		})
		if signErr != nil {
			t.Fatal(signErr)
		}
		return token
	}
	return mint(accountSubject), mint(executorSubject)
}

// This is the existing retainedManagementTokenRepository public-migration
// fixture with one lifecycle difference: its installation connection closes
// before returning, because real governance activation must inventory zero old
// writer sessions. Role/migration/runtime constraints are otherwise identical.
func forceCancelTokenRepository(t *testing.T) (*PostgresRepository, context.Context) {
	t.Helper()
	dsn := os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL")
	if dsn == "" {
		t.Skip("PINVOU_TEST_TOKEN_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	admin, err := cifixture.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	roles := []string{"token_gateway", "token_cap_policy_deployer", "token_schema_owner", "token_migrator", "token_policy_operator"}
	var existing int
	if err = admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&existing); err != nil || existing != 0 {
		t.Fatalf("isolated role absence guard: count=%d err=%v", existing, err)
	}
	created := []string{}
	database := "f04_token_management_" + uuid.New().String()[:8] + fmt.Sprintf("%d", time.Now().UnixNano())
	var fixtureDatabase *cifixture.Database
	databaseCreated := false
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if databaseCreated {
			if _, err := cifixture.DropDatabase(cleanupCtx, admin, fixtureDatabase, admin.Config().User); err != nil {
				t.Error("owned database cleanup failed; roles retained")
				return
			}
		}
		for i := len(created) - 1; i >= 0; i-- {
			if _, err := admin.Exec(cleanupCtx, "DROP ROLE "+pgx.Identifier{created[i]}.Sanitize()); err != nil {
				t.Errorf("owned test role cleanup refused: %s", created[i])
			}
		}
	})
	for _, role := range roles {
		options := " NOLOGIN NOINHERIT"
		if role != "token_schema_owner" && role != "token_cap_policy_deployer" {
			options = " LOGIN NOINHERIT"
		}
		if _, err = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+options); err != nil {
			t.Fatal(err)
		}
		created = append(created, role)
	}
	if _, err = admin.Exec(ctx, `GRANT token_gateway,token_schema_owner,token_cap_policy_deployer TO token_migrator`); err != nil {
		t.Fatal(err)
	}
	fixtureDatabaseCreated, creationError := cifixture.CreateDatabase(ctx, admin, database, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize())
	fixtureDatabase = fixtureDatabaseCreated
	if err := creationError; err != nil {
		t.Fatal(err)
	}
	databaseCreated = true
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = database
	owner, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	// The existing production database initializer installs this trusted
	// extension as the bootstrap administrator before handing schema authority
	// to the dedicated non-owner runtime and migrator roles.
	if _, err = owner.Exec(ctx, `GRANT token_gateway TO token_schema_owner WITH INHERIT TRUE,SET TRUE; CREATE EXTENSION IF NOT EXISTS pgcrypto; ALTER SCHEMA public OWNER TO token_schema_owner; GRANT USAGE,CREATE ON SCHEMA public TO token_gateway`); err != nil {
		owner.Close(context.Background())
		t.Fatal(err)
	}
	owner.Close(context.Background())
	config.User = "token_migrator"
	config.Password = ""
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	defer migrations.Unlock(context.Background(), connection)
	if _, err = connection.Exec(ctx, `SET ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if err = tokenschema.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	verification, err := migrations.VerifyApplied(ctx, connection, loaded)
	if err != nil || verification.AppliedCount != len(loaded) {
		t.Fatalf("public migration verification: %+v err=%v", verification, err)
	}
	migrations.Unlock(ctx, connection)
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.Database = database
	poolConfig.ConnConfig.User = "token_gateway"
	poolConfig.ConnConfig.Password = ""
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	secret, err := NewLocalAESProvider("controlled-key", bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &PostgresRepository{pool: pool, secrets: secret, now: func() time.Time { return time.Now().UTC() }}, ctx
}

type forceCancelActivation struct{ store *TokenGovernanceStore }

func (a forceCancelActivation) Snapshot(ctx context.Context) (tokengovernance.Snapshot, error) {
	s, e := a.store.Snapshot(ctx)
	return tokengovernance.Snapshot{State: s.State, Epoch: s.Epoch, PolicySetDigest: s.PolicySetDigest, HandlerSetDigest: s.HandlerSetDigest, ApprovedRuntimeRoles: s.ApprovedRuntimeRoles, UpdatedAt: s.UpdatedAt}, e
}
func forceCancelActorURL(t *testing.T, raw, database, role string) string {
	t.Helper()
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("owned PostgreSQL URL required")
	}
	u.Path = "/" + database
	u.User = url.User(role)
	return u.String()
}
func forceCancelActivate(t *testing.T, ctx context.Context, r *PostgresRepository, accountPool *pgxpool.Pool) *TokenGovernanceStore {
	t.Helper()
	cfg := r.pool.Config().ConnConfig.Copy()
	r.pool.Close()
	admin, e := cifixture.Connect(ctx, os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
	if e != nil {
		t.Fatal("owned Token bootstrap unavailable")
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	role := "force_cancel_runtime_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, e = admin.Exec(ctx, "ALTER ROLE token_gateway NOLOGIN; ALTER ROLE token_policy_operator NOLOGIN; CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" NOLOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS; GRANT token_gateway TO "+pgx.Identifier{role}.Sanitize()+"; GRANT pg_read_all_stats TO token_migrator"); e != nil {
		t.Fatal("owned activation actors", e)
	}
	t.Cleanup(func() {
		r.pool.Close()
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, e := admin.Exec(clean, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); e != nil {
			t.Error("owned runtime cleanup", e)
		}
	})
	cfg.User = "token_migrator"
	cfg.Password = ""
	migrationConfig, e := pgxpool.ParseConfig(cfg.ConnString())
	if e != nil {
		t.Fatal(e)
	}
	migrationConfig.ConnConfig = cfg.Copy()
	migrationConfig.MaxConns = 1
	migrator, e := pgxpool.NewWithConfig(ctx, migrationConfig)
	if e != nil {
		t.Fatal(e)
	}
	defer migrator.Close()
	activated, e := CheckOrTransitionTokenGovernance(ctx, migrator, accountPool, GovernanceTransitionRequest{ExpectedState: "PRE_ACTIVATION", TargetState: "ACTIVE", RequestID: uuid.New(), OperatorID: "owned-force-cancel-test", Reason: "activate installed lifecycle contract", ApprovedRuntimeRoles: []string{role}, Apply: true})
	if e != nil || activated.State != "ACTIVE" {
		t.Fatal("real governance activation", e)
	}
	if _, e = admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN"); e != nil {
		t.Fatal(e)
	}
	cfg.User = role
	runtimeConfig, e := pgxpool.ParseConfig(cfg.ConnString())
	if e != nil {
		t.Fatal(e)
	}
	runtimeConfig.ConnConfig = cfg.Copy()
	runtimeConfig.MaxConns = 1
	r.pool, e = pgxpool.NewWithConfig(ctx, runtimeConfig)
	if e != nil {
		t.Fatal(e)
	}
	store, e := r.GovernanceStore(activated.HandlerSetDigest, activated.PolicySetDigest)
	if e != nil {
		t.Fatal(e)
	}
	r.WithTokenRouteActivationTransactionGuard(store)
	return store
}
func forceCancelModel(t *testing.T, ctx context.Context, r *PostgresRepository, actor uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	model, capability := uuid.New(), uuid.New()
	ref := model.String()
	protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	if _, e := r.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'controlled','new-api-v1',$2,$3,$4,now())`, model, ref, digestString([]byte(ref)), actor); e != nil {
		t.Fatal(e)
	}
	if _, e := r.pool.Exec(ctx, `INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["TEXT"]','controlled',$4,now())`, capability, model, protocol, actor); e != nil {
		t.Fatal(e)
	}
	return model, capability
}
func forceCancelPublishPrice(t *testing.T, ctx context.Context, funds *billing.PostgresRepository, model, actor uuid.UUID) {
	t.Helper()
	local := time.Now().In(time.FixedZone("Shanghai", 8*60*60)).AddDate(0, 0, -1)
	from := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
	id := model.String()
	one := int64(1000000)
	units := []billing.PriceUnitConfiguration{}
	for _, raw := range []string{"00000000-0000-7000-8100-000000000001", "00000000-0000-7000-8100-000000000002", "00000000-0000-7000-8100-000000000003"} {
		units = append(units, billing.PriceUnitConfiguration{UnitDefinitionID: uuid.MustParse(raw), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &one})
	}
	draft, e := funds.CreatePriceDraft(ctx, billing.PriceDraftCommand{ScopeType: "MODEL", ScopeID: &id, ProtocolCode: "CHAT", FormulaVersionID: uuid.MustParse("00000000-0000-7000-8200-000000000001"), CreditUnit: "MICROCREDIT", EffectiveFrom: from, ExpectedETag: 1, Units: units}, actor, uuid.NewString())
	if e != nil {
		t.Fatal("real price draft", e)
	}
	if _, e = funds.PublishPriceVersion(ctx, draft.PriceVersionID, actor, draft.SpaceETag, uuid.NewString()); e != nil {
		t.Fatal("real price publication", e)
	}
}

// No usage/hold state is fabricated: the proxy's production persistence and
// reservation methods create it. The controlled provider is never contacted.
func forceCancelUsage(t *testing.T, ctx context.Context, r *PostgresRepository, funds *billing.PostgresRepository, client *HTTPBillingClient, user, wallet, model, capability uuid.UUID, version int64, running bool) uuid.UUID {
	t.Helper()
	account, e := r.CreateAccount(ctx, CreateAccountRequest{OwnerType: "USER", OwnerID: user, WalletID: wallet, Name: uuid.NewString(), ShareScope: "OWNER_UNIT_ONLY", DefaultPriority: 100, ManagementMode: "PLATFORM", CreatedBy: user})
	if e != nil {
		t.Fatal(e)
	}
	credential := uuid.New()
	cipher, e := r.secrets.Encrypt([]byte("controlled-provider-fact"), []byte(account.ID.String()+":"+credential.String()))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at) VALUES($1,$2,'controlled','https://provider.invalid',$3,$4,$5,1,'ACTIVE',$6,$7,now())`, credential, account.ID, cipher, r.secrets.KeyID(), strings.Repeat("a", 64), []string{model.String()}, user); e != nil {
		t.Fatal(e)
	}
	id := uuid.New()
	protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	generation := int64(1)
	digest := digestString([]byte(id.String()))
	usage, created, e := r.BeginUsage(ctx, UsageRequest{ID: id, IdempotencyNamespace: "model-inference", IdempotencyKeyDigest: digest, CanonicalVersion: 1, RequestFingerprint: digest, UserID: user, SessionID: "controlled-session", TokenAccountID: account.ID, CredentialID: credential, WalletID: wallet, Model: model.String(), ModelID: &model, CatalogVersion: &version, CapabilityVersionID: &capability, ProtocolDefinitionID: &protocol, RouteGeneration: &generation, ModelSnapshot: json.RawMessage(`{}`), MaximumMicrocredits: 100}, map[string]any{"context_type": "PERSONAL", "provider_id": "controlled", "adapter_version": "new-api-v1"})
	if e != nil || !created {
		t.Fatal("real usage acceptance", e)
	}
	price, e := client.Price(ctx, model.String())
	if e != nil {
		t.Fatal("original price resolution", e)
	}
	envelope := DispatchEnvelope{BaseURL: "https://provider.invalid", Provider: "controlled", AdapterVersion: "new-api-v1", Credential: []byte("controlled-provider-fact"), RequestBody: []byte(`{"model":"` + model.String() + `"}`), Price: price, ContextType: "PERSONAL", MaximumCost: 100}
	if e = r.PrepareDispatch(ctx, id, envelope); e != nil {
		t.Fatal(e)
	}
	if e = r.MarkHoldReserving(ctx, id); e != nil {
		t.Fatal(e)
	}
	priceRaw, _ := json.Marshal(price)
	hold, e := funds.ReservePriced(ctx, id, wallet, 100, &price.ID, time.Now().Add(time.Hour), &billing.HoldPricingContext{ModelID: model.String(), ProtocolCode: "CHAT", FormulaVersionID: price.FormulaVersionID, PricingSnapshot: priceRaw, RequestSnapshot: usage.RequestSnapshot})
	if e != nil {
		t.Fatal("real priced reservation", e)
	}
	// Retain the real acknowledged-hold window before dispatch promotion for
	// the pre-dispatch sample; this is the first production step of CompleteHoldAndDispatch.
	if !running {
		if e = r.persistAcknowledgedHold(ctx, id, hold.ID, price.ID); e != nil {
			t.Fatal(e)
		}
		return id
	}
	if e = r.CompleteHoldAndDispatch(ctx, id, hold.ID, price.ID); e != nil {
		t.Fatal(e)
	}
	if running {
		if _, ok, e := r.AcquireDispatch(ctx, id, "force-cancel-owner", dispatchLeaseDuration); e != nil || !ok {
			t.Fatal("real dispatch claim", e)
		}
		if e = r.MarkUpstream(ctx, id); e != nil {
			t.Fatal(e)
		}
	}
	return id
}
func TestLifecycleForceCancelApprovalPostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL") == "" || os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL") == "" || os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("three real owned PostgreSQL fixtures required")
	}
	af := forceCancelAccountActors(t)
	ctx := af.ctx
	ar := af.repository(t)
	var database string
	if e := af.admin.QueryRow(ctx, `SELECT datname FROM pg_database WHERE datname LIKE 'f01_wallet_fence_%' AND datdba=(SELECT oid FROM pg_roles WHERE rolname='account_service')`).Scan(&database); e != nil {
		t.Fatal("owned Account database", e)
	}
	accountURL := func(role string) string {
		return forceCancelActorURL(t, os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"), database, role)
	}
	operator, e := pgxpool.New(ctx, accountURL("account_migrator"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(operator.Close)
	version := int64(1)
	for _, step := range []account.FreshApprovalStep{account.FreshApprovalQuiesce, account.FreshApprovalFence, account.FreshApprovalResources, account.FreshApprovalActivate, account.FreshApprovalResume} {
		control, e := account.RunFreshApprovalStep(ctx, accountURL("account_migrator"), version, step, func(context.Context, account.AdminApprovalControl) (json.RawMessage, error) {
			return json.RawMessage(`{"controlled_broker_check":true}`), nil
		})
		if e != nil {
			t.Fatal("real fresh approval transition", e)
		}
		version = control.Version
	}
	if e = ar.ConfigureApprovalWriter(ctx, accountURL("account_approval_writer")); e != nil {
		t.Fatal(e)
	}
	// This dedicated login uses the same real relay readiness/privilege contract
	// as newEntitlementApprovalFixture; FreshApprovalResume above opens its gate.
	relay, e := account.NewApprovalRelayStore(ctx, accountURL("account_approval_relay"))
	if e != nil {
		t.Fatal("real frozen relay readiness", e)
	}
	t.Cleanup(relay.Close)
	actorPool, e := pgxpool.New(ctx, accountURL("account_service"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(actorPool.Close)
	requester := seedForceCancelActor(t, ctx, actorPool, "PLATFORM_SUPER_ADMIN", "force-cancel-requester")
	reviewer := seedForceCancelActor(t, ctx, actorPool, "PLATFORM_SUPER_ADMIN", "force-cancel-reviewer")
	r, _ := forceCancelTokenRepository(t)
	// Public migrations intentionally leave owner-cap policy unconfigured. Match
	// retained_default_route_integration_test.go: the dedicated policy operator
	// must install it through the original CAS function before account creation.
	capConfig := r.pool.Config().ConnConfig.Copy()
	capConfig.User = "token_policy_operator"
	capOperator, e := pgx.ConnectConfig(ctx, capConfig)
	if e != nil {
		t.Fatal("owned cap policy operator", e)
	}
	_, e = capOperator.Exec(ctx, `SELECT * FROM public.apply_token_account_owner_cap_policy(20,NULL,$1,'owned lifecycle replacement capacity',$2)`, uuid.NewString(), uuid.New())
	capOperator.Close(ctx)
	if e != nil {
		t.Fatal("real owner cap policy initialization", e)
	}
	funds, _, _ := retainedManagementBillingRepository(t)
	// Match the installed token-governance CLI's limited Account policy reader.
	policyRole := "force_cancel_policy_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, e = af.admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{policyRole}.Sanitize()+" LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS"); e != nil {
		t.Fatal(e)
	}
	policySetup, e := pgx.Connect(ctx, accountURL("account_migrator"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = policySetup.Exec(ctx, "SET ROLE account_service; GRANT USAGE ON SCHEMA public TO "+pgx.Identifier{policyRole}.Sanitize()+"; GRANT SELECT,UPDATE ON approval_command_policies TO "+pgx.Identifier{policyRole}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	policyPool, e := pgxpool.New(ctx, accountURL(policyRole))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		policyPool.Close()
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := policySetup.Exec(clean, "REVOKE ALL ON approval_command_policies FROM "+pgx.Identifier{policyRole}.Sanitize()+"; REVOKE USAGE ON SCHEMA public FROM "+pgx.Identifier{policyRole}.Sanitize()); err != nil {
			t.Error("owned policy grants cleanup", err)
		}
		policySetup.Close(clean)
		if _, err := af.admin.Exec(clean, "DROP ROLE "+pgx.Identifier{policyRole}.Sanitize()); err != nil {
			t.Error("owned policy role cleanup", err)
		}
	})
	store := forceCancelActivate(t, ctx, r, policyPool)
	current, e := store.CurrentTokenCommandGovernanceActivation(ctx)
	if e != nil {
		t.Fatal(e)
	}
	ctx = context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: current, transactional: true})
	activation := forceCancelActivation{store}
	proofToken, executorToken := configureForceCancelIdentity(t)
	var failRelease atomic.Bool
	billingHandler := billing.NewService(funds).Handler("unused-static-token")
	billingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/internal/v1/holds/release" && failRelease.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		billingHandler.ServeHTTP(w, request)
	}))
	t.Cleanup(billingServer.Close)
	client, e := NewHTTPBillingClient(billingServer.URL, executorToken)
	if e != nil {
		t.Fatal(e)
	}
	adapter := NewNewAPIGatewayAdapter()
	service := NewManagementService(r, nil, nil, nil, ProviderURLPolicy{}, client, nil).WithImpactPrechecks(true, r, client).WithTokenCommandGovernanceActivation(store).WithApprovedTokenCommandExecutor(NewPostgresApprovedCredentialExecutor(r))
	service.executionBilling = client
	service.cancellationAdapter = adapter
	service.upstream = adapter
	// Production NewApplicationServer installs this same correlation wrapper.
	// Governed command persistence requires its server-owned audit request ID.
	tokenServer := httptest.NewServer(httpserver.CorrelationHandler(service.ManagementHandler("unused-static-token", nil)))
	t.Cleanup(tokenServer.Close)
	snapshot, e := account.NewHTTPApprovalSnapshotSource(tokenServer.URL, proofToken)
	if e != nil {
		t.Fatal(e)
	}
	proof, e := account.NewHTTPApprovalProofSourceWithToken(billingServer.URL, tokenServer.URL, proofToken)
	if e != nil {
		t.Fatal(e)
	}
	ar.WithApprovalSnapshotSource(snapshot).WithApprovalProofSource(proof).WithTokenGovernanceActivation(activation)
	accountServer := httptest.NewServer(account.NewService(ar, nil).WithServiceToken("unused-static-token").Handler(nil))
	t.Cleanup(accountServer.Close)
	// A controlled supplier endpoint feeds the real adapter and health worker;
	// neither READY nor HEALTHY is written directly by this fixture.
	var probeModel atomic.Value
	probeModel.Store("")
	var probes atomic.Int64
	supplier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/models" {
			t.Error("unexpected controlled supplier probe")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		probes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": probeModel.Load().(string)}}})
	}))
	t.Cleanup(supplier.Close)
	supplierURL, err := url.Parse(supplier.URL)
	if err != nil {
		t.Fatal(err)
	}
	supplierPolicy := ProviderURLPolicy{environment: "test", allowedHosts: map[string]bool{supplierURL.Hostname(): true}}

	for _, sample := range []struct {
		name   string
		policy int
		force  bool
	}{{"v2-false-compatibility", 2, false}, {"v3-false", 3, false}, {"v3-true-removed", 3, true}} {
		t.Run(sample.name, func(t *testing.T) {
			model, capability := forceCancelModel(t, ctx, r, requester.ID)
			replacement, replacementCapability := forceCancelModel(t, ctx, r, requester.ID)
			var catalog int64
			if e = r.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'owned lifecycle approval catalog',$2,now(),now()) RETURNING version`, uuid.New(), requester.ID).Scan(&catalog); e != nil {
				t.Fatal(e)
			}
			protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
			for _, item := range []struct{ id, cap uuid.UUID }{{model, capability}, {replacement, replacementCapability}} {
				if _, e = r.pool.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($1,$2,$3,$3,'PUBLISHED',$4,$5,true)`, catalog, item.id, item.id.String(), item.cap, protocol); e != nil {
					t.Fatal(e)
				}
				forceCancelPublishPrice(t, ctx, funds, item.id, requester.ID)
			}
			wallet, _, e := funds.EnsureWallet(ctx, "PERSONAL", requester.ID)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = funds.Credit(ctx, wallet.ID, "force-cancel-credit-"+uuid.NewString(), 1000, false); e != nil {
				t.Fatal(e)
			}
			probeModel.Store(replacement.String())
			probes.Store(0)
			provisioner, e := NewDefaultRouteProvisioner(r, supplierPolicy, DefaultRouteConfig{Provider: "controlled", AdapterVersion: "new-api-v1", BaseURL: supplier.URL, Secret: []byte("controlled-provider-fact"), Priority: 2})
			if e != nil {
				t.Fatal(e)
			}
			provisioner.WithTokenAccountCreateCapability(true)
			item, e := provisioner.Ensure(ctx, AuthorizationContext{UserID: requester.ID, ContextType: "PERSONAL", ContextVersion: 1, AuthorizationVersion: 1}, wallet.ID, CatalogModel{ModelID: replacement, ProtocolDefinitionID: protocol, ProtocolCode: "CHAT", ProviderID: "controlled", AdapterVersion: "new-api-v1", UpstreamModelRef: replacement.String(), LifecycleStatus: "PUBLISHED", SourceAvailable: true})
			if e != nil {
				t.Fatal(e)
			}
			claimed, attempt, ok := provisioner.claim(ctx)
			if !ok || claimed.ID != item.ID {
				t.Fatal("real replacement provisioning claim")
			}
			provisioner.execute(ctx, claimed, attempt)
			ready, e := provisioner.Detail(ctx, item.ID)
			if e != nil || ready.Status != "READY" {
				t.Fatalf("real replacement readiness: status=%s stage=%s code=%v error=%v", ready.Status, ready.Stage, ready.LastErrorCode, e)
			}
			health, e := NewRouteHealthWorker(r, adapter, supplierPolicy, time.Second)
			if e != nil {
				t.Fatal(e)
			}
			target, ok := health.claim(ctx)
			if !ok || target.UpstreamModel != replacement.String() {
				t.Fatal("real replacement health claim")
			}
			health.execute(ctx, target)
			var healthy bool
			if e = r.pool.QueryRow(ctx, `SELECT health_status='HEALTHY' AND health_diagnostic_code='PROBE_SUCCEEDED' AND health_expires_at>transaction_timestamp() AND EXISTS(SELECT 1 FROM route_health_observations WHERE route_id=$1 AND diagnostic_code='PROBE_SUCCEEDED') FROM routing_policies WHERE id=$1`, target.RouteID).Scan(&healthy); e != nil || !healthy || probes.Load() != 1 {
				t.Fatal("real controlled supplier health evidence", healthy, probes.Load(), e)
			}
			var pre, running uuid.UUID
			if sample.force {
				pre = forceCancelUsage(t, ctx, r, funds, client, requester.ID, wallet.ID, model, capability, catalog, false)
				running = forceCancelUsage(t, ctx, r, funds, client, requester.ID, wallet.ID, model, capability, catalog, true)
			}
			effective := time.Now().UTC().Add(6 * time.Minute).Truncate(time.Microsecond)
			if sample.force {
				t.Run("false-with-inflight-rejected", func(t *testing.T) {
					denied, _, err := r.CreateLifecyclePlan(ctx, model, LifecyclePlanInput{ExpectedCatalogVersion: catalog, TargetLifecycle: "REMOVED", EffectiveAt: effective, ExistingSessionCutoffAt: &effective, ReplacementModelID: &replacement, Reason: "owned false removal negative"}, requester.ID, uuid.NewString())
					if err != nil {
						t.Fatal("real false plan", err)
					}
					deniedCommand := lifecyclePlanCommandFromPlan(denied)
					deniedRaw, _ := json.Marshal(deniedCommand)
					if _, err = snapshot.PrecheckApprovalSnapshot(ctx, account.ApprovalSnapshotRequest{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", TargetResourceType: "MODEL", TargetResourceID: model.String(), NormalizedCommand: deniedRaw}); err == nil {
						t.Fatal("false plan accepted while requests are inflight")
					}
					if _, err = r.CancelLifecyclePlan(ctx, model, denied.ID, denied.PlanVersion, requester.ID, "close controlled rejected plan", uuid.NewString()); err != nil {
						t.Fatal("real rejected plan close", err)
					}
				})
				t.Run("non-removed-plan-rejected", func(t *testing.T) {
					if _, _, err := r.CreateLifecyclePlan(ctx, model, LifecyclePlanInput{ExpectedCatalogVersion: catalog, TargetLifecycle: "DEPRECATED", EffectiveAt: effective, ForceCancelInflight: true, Reason: "owned invalid force target"}, requester.ID, uuid.NewString()); err == nil {
						t.Fatal("non-REMOVED force creation accepted")
					}
				})
			}
			plan, _, e := r.CreateLifecyclePlan(ctx, model, LifecyclePlanInput{ExpectedCatalogVersion: catalog, TargetLifecycle: "REMOVED", EffectiveAt: effective, ExistingSessionCutoffAt: &effective, ReplacementModelID: &replacement, ForceCancelInflight: sample.force, Reason: "owned reviewed removal"}, requester.ID, uuid.NewString())
			if e != nil {
				t.Fatal("real pending plan", e)
			}
			command := lifecyclePlanCommandFromPlan(plan)
			if sample.policy == 2 {
				command.ForceCancelInflight = nil
			}
			raw, _ := json.Marshal(command)
			if sample.force {
				for _, negative := range []string{"missing-flag", "mismatched-flag", "non-removed"} {
					t.Run(negative, func(t *testing.T) {
						bad := command
						switch negative {
						case "missing-flag":
							bad.ForceCancelInflight = nil
						case "mismatched-flag":
							v := false
							bad.ForceCancelInflight = &v
						case "non-removed":
							bad.TargetLifecycle = "DEPRECATED"
						}
						b, _ := json.Marshal(bad)
						if _, err := snapshot.PrecheckApprovalSnapshot(ctx, account.ApprovalSnapshotRequest{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", TargetResourceType: "MODEL", TargetResourceID: model.String(), NormalizedCommand: b}); err == nil {
							t.Fatal("unbound lifecycle authority accepted")
						}
					})
				}
			}
			if sample.force {
				t.Run("v2-true-rejected", func(t *testing.T) {
					current, err := snapshot.PrecheckApprovalSnapshot(ctx, account.ApprovalSnapshotRequest{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", TargetResourceType: "MODEL", TargetResourceID: model.String(), NormalizedCommand: raw})
					if err != nil {
						t.Fatal(err)
					}
					var critical map[string]any
					if err = json.Unmarshal(current.CriticalResourceVersions, &critical); err != nil {
						t.Fatal(err)
					}
					critical["policy_version"] = 2
					criticalRaw, _ := json.Marshal(critical)
					if _, err = approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", PolicyVersion: 2, SnapshotSchemaVersion: 2, TargetResourceType: "MODEL", TargetResourceID: model.String(), NormalizedCommand: raw, CriticalResourceVersions: criticalRaw, FinancialSnapshot: current.FinancialSnapshot}); err == nil {
						t.Fatal("legacy v2 rebound to force cancellation")
					}
				})
			}
			draft := account.ApprovalDraft{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", IdempotencyKey: uuid.NewString(), NormalizedCommand: raw, TargetResourceType: "MODEL", TargetResourceID: model.String(), Reason: "owned reviewed removal", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`)}
			created, e := ar.CreateApprovalDraft(ctx, forceCancelPrincipal(requester), draft)
			if e != nil {
				t.Fatal("real Account draft", e)
			}
			if created.PolicyVersion != int64(sample.policy) {
				t.Fatal("wrong frozen policy", created.PolicyVersion)
			}
			submitted, e := ar.SubmitApprovalRequest(ctx, forceCancelPrincipal(requester), created.ID, created.Version)
			if e != nil {
				t.Fatal("real Account submit", e)
			}
			detail, e := ar.ApprovalRequestV2(ctx, forceCancelPrincipal(reviewer), created.ID)
			if e != nil {
				t.Fatal("real reviewer detail", e)
			}
			detailJSON, _ := json.Marshal(detail)
			if sample.policy == 3 && !bytes.Contains(detailJSON, []byte(`"force_cancel_inflight":`)) {
				t.Fatal("reviewer detail omitted force decision")
			}
			if _, e = ar.DecideApprovalRequest(ctx, forceCancelPrincipal(reviewer), created.ID, submitted.Version, "APPROVED", "reviewed owned lifecycle", submitted.CriticalResourceVersions); e != nil {
				t.Fatal("real Account independent approval", e)
			}
			var eventID uuid.UUID
			var frozen []byte
			conn, e := operator.Acquire(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = conn.Exec(ctx, "SET ROLE account_approval_authorization_owner"); e != nil {
				t.Fatal(e)
			}
			e = conn.QueryRow(ctx, `SELECT o.event_id,s.delivery_bytes FROM outbox_events o JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id WHERE o.aggregate_id=$1 AND o.event_type='account.approval-command.authorized.v1'`, created.ID).Scan(&eventID, &frozen)
			_, resetErr := conn.Exec(ctx, "RESET ROLE")
			conn.Release()
			if e != nil || resetErr != nil {
				t.Fatal("real frozen authorization bytes", e, resetErr)
			}
			// Account route resolution requires an actual published outbox receipt.
			// Run the real frozen relay; only the broker PubAck is controlled here.
			claims, err := relay.ClaimFrozen(ctx, "force-cancel-frozen-relay", 1, 30*time.Second)
			if err != nil || len(claims) != 1 || claims[0].EventID != eventID {
				t.Fatal("real authorization relay claim", err)
			}
			var published []byte
			publicationCount := 0
			disposition, err := relay.ProcessFrozen(ctx, "force-cancel-frozen-relay", claims[0], 12, time.Now().UTC(), func(_ context.Context, message events.FrozenMessage) events.FrozenPublishResult {
				if message.EventID != eventID || message.Subject != account.ApprovalAuthorizationEventType || message.MsgID != eventID.String() || !bytes.Equal(message.Body, frozen) {
					t.Fatal("frozen authorization publication changed")
				}
				publicationCount++
				published = append([]byte(nil), message.Body...)
				return events.FrozenPublishResult{Disposition: events.FrozenPublished}
			})
			if err != nil || disposition != events.FrozenPublished || publicationCount != 1 || !bytes.Equal(published, frozen) {
				t.Fatal("real authorization relay publication", disposition, publicationCount, err)
			}
			frozen = published
			descriptor, ok := approvalconsumer.LookupDurable("token-model-lifecycle-executor-v1")
			if !ok {
				t.Fatal("missing canonical consumer")
			}
			consumer, e := NewTokenAuthorizationCommandConsumer("TOKEN_MODEL_LIFECYCLE_CHANGE", descriptor.Durable, tokenServer.URL, accountServer.URL, executorToken, activation)
			if e != nil {
				t.Fatal(e)
			}
			delivery := &tokenAuthorizationDeliveryStub{raw: frozen, id: eventID.String(), meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: uint64(sample.policy), NumDelivered: 1}}
			if sample.force {
				t.Run("frozen-flag-tamper-rejected", func(t *testing.T) {
					changed := bytes.Replace(frozen, []byte(`"force_cancel_inflight":true`), []byte(`"force_cancel_inflight":false`), 1)
					if bytes.Equal(changed, frozen) {
						t.Fatal("controlled force flag tamper did not hit frozen wire")
					}
					invalid := &tokenAuthorizationDeliveryStub{raw: changed, id: eventID.String(), meta: delivery.meta}
					if err := consumer.handle(ctx, invalid); err == nil {
						t.Fatal("tampered frozen flag accepted")
					}
					pending, err := r.LifecyclePlan(ctx, model, plan.ID)
					if err != nil || pending.Status != "PENDING_APPROVAL" {
						t.Fatal("rejected wire scheduled the plan", pending.Status, err)
					}
				})
			}
			if e = consumer.handle(ctx, delivery); e != nil {
				t.Fatal("real authorization consumer", e)
			}
			scheduled, e := r.LifecyclePlan(ctx, model, plan.ID)
			if e != nil || scheduled.Status != "SCHEDULED" || scheduled.ApprovalRequestID == nil || *scheduled.ApprovalRequestID != created.ID {
				t.Fatal("real schedule freeze", scheduled.Status, e)
			}
			worked, e := NewLifecyclePlanWorker(service, r, "owned-lifecycle-worker").runOnce(ctx, &effective)
			if e != nil || !worked {
				t.Fatal("real due lifecycle worker", worked, e)
			}
			applied, e := r.LifecyclePlan(ctx, model, plan.ID)
			if e != nil || applied.Status != "APPLIED" {
				t.Fatal("plan did not apply", applied.Status, e)
			}
			var policies int
			if e = r.pool.QueryRow(ctx, `SELECT count(*) FROM model_emergency_cancellation_policies WHERE model_id=$1`, model).Scan(&policies); e != nil || policies != map[bool]int{false: 0, true: 1}[sample.force] {
				t.Fatal("force policy boundary", policies, e)
			}
			if !sample.force {
				return
			}
			initialTime := effective.Add(time.Second)
			var testTime atomic.Pointer[time.Time]
			testTime.Store(&initialTime)
			r.now = func() time.Time { return *testTime.Load() }
			defer func() { r.now = func() time.Time { return time.Now().UTC() } }()
			// A real dispatch owner on replica A waits on its existing heartbeat.
			// The existing emergency worker below uses separate Service B.
			heartbeatOwner := &Service{repository: r}
			heartbeatTicks := make(chan time.Time, 1)
			heartbeatCancelled := make(chan struct{}, 1)
			var heartbeatCancellations atomic.Int32
			heartbeatAbort := func() { heartbeatCancellations.Add(1); heartbeatCancelled <- struct{}{} }
			unregisterHeartbeat := heartbeatOwner.registerActiveUpstream(running, heartbeatAbort)
			defer unregisterHeartbeat()
			heartbeatFailed, stopHeartbeat, heartbeatErr := heartbeatOwner.renewDispatchLease(ctx, heartbeatAbort, running, "force-cancel-owner", heartbeatTicks)
			if heartbeatErr != nil {
				t.Fatal("real owner heartbeat start", heartbeatErr)
			}
			defer stopHeartbeat()
			if heartbeatCancellations.Load() != 0 || service.abortActiveUpstream(running) {
				t.Fatal("owner was already cancelled or shared another Service registry")
			}
			failRelease.Store(true)
			t.Run("cancel-failure-retains-funds", func(t *testing.T) {
				if err := NewEmergencyModelCancellationWorker(r, service, time.Second).runOnce(ctx); err != nil {
					t.Fatal(err)
				}
				failed, err := r.GetUsage(ctx, pre)
				if err != nil || failed.CancelStatus == "LOCAL_CONFIRMED" || failed.SettlementStatus == "RELEASED" {
					t.Fatal("failed release invented confirmation", err)
				}
				held, err := funds.GetWallet(ctx, wallet.ID)
				if err != nil || held.ReservedMicrocredits != 200 {
					t.Fatal("failed release lost actual funds", held.ReservedMicrocredits, err)
				}
			})
			failRelease.Store(false)
			var retryAt time.Time
			if err := r.pool.QueryRow(ctx, `SELECT cancellation.next_attempt_at
FROM model_emergency_cancellations cancellation
JOIN model_emergency_cancellation_policies policy ON policy.id=cancellation.policy_id
WHERE cancellation.usage_request_id=$1 AND policy.model_id=$2 AND cancellation.status='PENDING'`, pre, model).Scan(&retryAt); err != nil || !retryAt.After(initialTime) {
				t.Fatal("failed release did not schedule a future retry", retryAt, err)
			}
			testTime.Store(&retryAt)
			if e = NewEmergencyModelCancellationWorker(r, service, time.Second).runOnce(ctx); e != nil {
				t.Fatal("real emergency worker", e)
			}
			stopped, e := r.GetUsage(ctx, pre)
			if e != nil || stopped.SettlementStatus != "RELEASED" || stopped.CancelStatus != "LOCAL_CONFIRMED" || !stopped.EmergencyCancellationRequested {
				t.Fatal("predispatch funds not confirmed", stopped.ExecutionStatus, stopped.CancelStatus, e)
			}
			unknown, e := r.GetUsage(ctx, running)
			if e != nil || unknown.CancelStatus != "UNSUPPORTED" || unknown.SettlementStatus == "RELEASED" || unknown.ActualMicrocredits != nil || !unknown.EmergencyCancellationRequested {
				t.Fatal("unsupported cancellation invented free usage", unknown.CancelStatus, unknown.SettlementStatus, e)
			}
			money, e := funds.GetWallet(ctx, wallet.ID)
			if e != nil || money.ReservedMicrocredits != 100 {
				t.Fatal("unsupported request lost actual reservation", money.ReservedMicrocredits, e)
			}
			t.Run("emergency-then-user-origin-retains-authority", func(t *testing.T) {
				// The existing real worker has committed REQUESTED/CONVERGING.
				// Use the actual first USER writer; never manufacture its origin.
				key := digestString([]byte("emergency-then-user-" + running.String()))
				result, preDispatch, err := r.RequestCancellation(ctx, running, requester.ID, key, cancellationFingerprint(running), adapter.AdapterVersion())
				if err != nil || preDispatch || result.CancelStatus != "UNSUPPORTED" {
					t.Fatal("actual first USER cancellation", preDispatch, err)
				}
				current, err := r.GetUsage(ctx, running)
				if err != nil || current.CancellationOrigin != "USER" || !current.EmergencyCancellationRequested {
					t.Fatal("USER origin overwrote immutable emergency authority", err)
				}
				var origin, target string
				var originals, commands int
				if err := r.pool.QueryRow(ctx, `SELECT request.cancellation_origin,cancellation.status,(SELECT count(*) FROM model_emergency_cancellation_evidence evidence WHERE evidence.emergency_cancellation_id=cancellation.id AND evidence.evidence_type='REQUESTED'),(SELECT count(*) FROM usage_cancel_commands command WHERE command.usage_request_id=request.id) FROM usage_requests request JOIN model_emergency_cancellations cancellation ON cancellation.usage_request_id=request.id WHERE request.id=$1`, running).Scan(&origin, &target, &originals, &commands); err != nil || origin != "USER" || target != "CONVERGING" || originals != 1 || commands != 1 {
					t.Fatal("actual writer composition facts differ", origin, target, originals, commands, err)
				}
				if _, _, err := r.RequestCancellation(ctx, running, requester.ID, key, cancellationFingerprint(running), adapter.AdapterVersion()); err != nil {
					t.Fatal("USER receipt replay", err)
				}
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancel_commands WHERE usage_request_id=$1`, running).Scan(&commands); err != nil || commands != 1 {
					t.Fatal("USER replay duplicated command", commands, err)
				}
				held, err := funds.GetWallet(ctx, wallet.ID)
				if err != nil || held.ReservedMicrocredits != money.ReservedMicrocredits || held.PostedMicrocredits != money.PostedMicrocredits {
					t.Fatal("USER override changed existing funds", err)
				}
				t.Run("owner-heartbeat-observes-remote-request", func(t *testing.T) {
					// REQUESTED came from the real worker; USER origin came from the
					// actual first USER writer. No new dispatch or money fact is seeded.
					if service.abortActiveUpstream(running) {
						t.Fatal("worker Service B unexpectedly owned A transport")
					}
					heartbeatTicks <- r.now()
					select {
					case <-heartbeatCancelled:
					case <-time.After(3 * time.Second):
						t.Fatal("real owner loop missed persistent remote emergency")
					}
					stopHeartbeat()
					if heartbeatCancellations.Load() != 1 {
						t.Fatal("owner heartbeat cancellation repeated")
					}
					select {
					case <-heartbeatFailed:
						t.Fatal("requested emergency became lease loss")
					default:
					}
					heartbeatTicks <- r.now()
					if heartbeatCancellations.Load() != 1 {
						t.Fatal("stopped heartbeat repeated cancellation")
					}
					afterTick, err := funds.GetWallet(ctx, wallet.ID)
					if err != nil || afterTick.ReservedMicrocredits != money.ReservedMicrocredits || afterTick.PostedMicrocredits != money.PostedMicrocredits {
						t.Fatal("owner cancellation altered financial truth", err)
					}
				})
				unknown = current
			})
			t.Run("trusted-final-usage-original-price", func(t *testing.T) {
				if unknown.PriceVersionID == nil || unknown.HoldID == nil {
					t.Fatal("original financial identity missing")
				}
				original, err := client.PriceByID(ctx, *unknown.PriceVersionID)
				if err != nil {
					t.Fatal("original immutable price", err)
				}
				usage := Usage{InputTokens: 2, OutputTokens: 3}
				actual, snapshot, err := settlementRecord(original, usage, nil)
				if err != nil || actual != 5 {
					t.Fatal("original price arithmetic", actual, err)
				}
				ownerContext := withDispatchLeaseOwner(ctx, "force-cancel-owner")
				if err = r.SaveReplayResult(ownerContext, running, ReplayResult{Kind: "JSON", HTTPStatus: http.StatusOK, ContentType: "application/json", Payload: []byte(`{"usage":{"prompt_tokens":2,"completion_tokens":3}}`), ResponseReceived: true, ResponseDisposition: responseDispositionTrustedUsage, FinalUsage: &usage, ActualMicrocredits: &actual, SettlementSnapshot: snapshot}); err != nil {
					t.Fatal("real trusted final usage persistence", err)
				}
				if result := service.settlePrepared(ownerContext, running, *unknown.HoldID, actual, usage, snapshot); result != "" {
					t.Fatal("actual Billing settlement", result)
				}
				current, err := r.GetUsage(ctx, running)
				if err != nil || current.ActualMicrocredits == nil || *current.ActualMicrocredits != 5 || current.Status != "COMMITTED" || current.SettlementStatus != "SETTLED" {
					t.Fatal("terminal usage truth", current.SettlementStatus, err)
				}
				after, err := funds.GetWallet(ctx, wallet.ID)
				if err != nil || after.ReservedMicrocredits != 0 || after.PostedMicrocredits != money.PostedMicrocredits-5 {
					t.Fatal("actual wallet debit", after.ReservedMicrocredits, err)
				}
				if err = NewEmergencyModelCancellationWorker(r, service, time.Second).runOnce(ctx); err != nil {
					t.Fatal(err)
				}
				var converged int
				if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM model_emergency_cancellations WHERE usage_request_id=ANY($1) AND status='CONVERGED'`, []uuid.UUID{pre, running}).Scan(&converged); err != nil || converged != 2 {
					t.Fatal("both emergency cancellation targets must converge", converged, err)
				}
				replay, err := funds.GetWallet(ctx, wallet.ID)
				if err != nil || replay.ReservedMicrocredits != after.ReservedMicrocredits || replay.PostedMicrocredits != after.PostedMicrocredits {
					t.Fatal("repeat cancellation changed funds", err)
				}
			})
		})
	}
}
