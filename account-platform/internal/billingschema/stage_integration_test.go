package billingschema

import (
	"context"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/simulatorschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This fixture requires exclusively absent deployment actors. It never alters
// an existing actor, forces a connection closed, or installs into the input DB.
// Original guarded SQL runs under genuine non-superuser migrator/service roles.
func TestF02SparseSchemaUpgradeAndRelayFence(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual PostgreSQL schema fixture is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	account, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	billing, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	simulator, err := migrations.Load("../../migrations/payment-simulator")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("account-first-delivery-upgrade", func(t *testing.T) {
		first := selectVersions(account, []int64{1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 22, 29, 34, 35, 36, 42, 78, 79, 80, 81, 82, 83, 84, 85, 86, 900001})
		initial := selectVersions(first, []int64{1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 16, 17})
		connections := []*pgx.Conn{f.database(t, ctx, "account", "account_service"), f.database(t, ctx, "account", "account_service")}
		for _, c := range connections {
			if err := migrations.Up(ctx, c, initial); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Exec(ctx, stageAccountSeed); err != nil {
				t.Fatal(err)
			}
		}
		before := stageAccountFacts(t, ctx, connections[0])
		if before != stageAccountFacts(t, ctx, connections[1]) {
			t.Fatal("initial owned fixtures differ")
		}
		if err := accountschema.Up(ctx, connections[0], first); err != nil {
			t.Fatal(err)
		}
		// Original 033 also records its command policy in original 018. The
		// bridge must restore permission metadata without bypassing that table.
		withApprovals := append(slices.Clone(first), selectVersions(account, []int64{18})...)
		if err := accountschema.Up(ctx, connections[0], withApprovals); err != nil {
			t.Fatal(err)
		}
		versions := stageValue(t, ctx, connections[0], `SELECT jsonb_agg(to_jsonb(v) ORDER BY user_id,scope,scope_id)::text FROM authorization_versions v`)
		late33 := append(slices.Clone(withApprovals), selectVersions(account, []int64{33})...)
		if err := accountschema.Up(ctx, connections[0], late33); err != nil {
			t.Fatal(err)
		}
		if versions != stageValue(t, ctx, connections[0], `SELECT jsonb_agg(to_jsonb(v) ORDER BY user_id,scope,scope_id)::text FROM authorization_versions v`) {
			t.Fatal("retained metadata bridge advanced authority without a permission change")
		}
		for _, c := range connections {
			if err := accountschema.Up(ctx, c, account); err != nil {
				t.Fatal(err)
			}
			if _, err := migrations.VerifyApplied(ctx, c, account); err != nil {
				t.Fatal(err)
			}
			if before != stageAccountFacts(t, ctx, c) {
				t.Fatal("upgrade rewrote original users, assignments, sessions, identities or event facts")
			}
		}
		if stageShape(t, ctx, connections[0]) != stageShape(t, ctx, connections[1]) {
			t.Fatal("fresh/sparse Account schema or ACL differs")
		}
		for _, query := range []string{
			`SELECT jsonb_agg(jsonb_build_array(user_id,scope,scope_id,version) ORDER BY user_id,scope,scope_id)::text FROM authorization_versions`,
			`SELECT jsonb_agg(to_jsonb(r) ORDER BY code)::text FROM roles r`,
			`SELECT jsonb_agg(to_jsonb(p) ORDER BY code)::text FROM permissions p`,
			`SELECT jsonb_agg(to_jsonb(p) ORDER BY role_code,permission_code)::text FROM role_permissions p`,
		} {
			if stageValue(t, ctx, connections[0], query) != stageValue(t, ctx, connections[1], query) {
				t.Fatal("fresh/sparse Account authority facts differ")
			}
		}
	})

	t.Run("billing-first-delivery-data-and-activation", func(t *testing.T) {
		connections := []*pgx.Conn{f.database(t, ctx, "billing", "billing_schema_owner"), f.database(t, ctx, "billing", "billing_schema_owner")}
		base := selectVersions(billing, []int64{1, 2})
		for _, c := range connections {
			if err := migrations.Up(ctx, c, base); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Exec(ctx, stageBillingSeed); err != nil {
				t.Fatal(err)
			}
		}
		before := stageWalletFacts(t, ctx, connections[0])
		for _, c := range connections {
			if err := Up(ctx, c, billing); err != nil {
				t.Fatal(err)
			}
			if _, err := migrations.VerifyApplied(ctx, c, billing); err != nil {
				t.Fatal(err)
			}
			if stageWalletFacts(t, ctx, c) != before {
				t.Fatal("upgrade rewrote first-delivery wallet identity or stored balances")
			}
			if got := stageValue(t, ctx, c, `SELECT jsonb_agg(jsonb_build_array(command_type,policy_version,snapshot_schema_version) ORDER BY command_type)::text FROM billing_approval_command_activations WHERE state='ACTIVE'`); got != `[["PAYMENT_PRODUCT_RESUME", 1, 1], ["PAYMENT_PROVIDER_RESUME", 2, 1], ["WALLET_ADJUSTMENT", 1, 1]]` {
				t.Fatalf("actual activation set: %s", got)
			}
			var future bool
			if err := c.QueryRow(ctx, `SELECT to_regclass('payment_financial_repair_cases') IS NOT NULL OR to_regclass('member_grant_repair_cases') IS NOT NULL OR to_regclass('gift_campaigns') IS NOT NULL`).Scan(&future); err != nil || future {
				t.Fatal("future repair/gift tables installed")
			}
		}
		if stageShape(t, ctx, connections[0]) != stageShape(t, ctx, connections[1]) {
			t.Fatal("fresh/sparse Billing schema or ACL differs")
		}
		c := connections[0]
		if _, err := c.Exec(ctx, `
INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES('91111111-1111-4111-8111-111111111111','BILLING','PAYMENT_FACT_CONFLICT','PAYMENT_ORDER','21111111-1111-4111-8111-111111111111','P1','OPEN','2026-01-01','2026-01-02','2026-01-02','2026-01-03','2026-01-04','2026-01-01');
INSERT INTO payment_anomaly_cases(id,anomaly_type,dedupe_key,status,resource_snapshot,created_at,updated_at) VALUES('91111111-1111-4111-8111-111111111111','PAYMENT_FACT_CONFLICT','owned-stage-anomaly','OPEN','{}','2026-01-01','2026-01-01');
INSERT INTO payment_anomaly_resolution_intents(payment_anomaly_case_id,actor_id,idempotency_key,request_hash,resolution,reason,target_snapshot,created_at) VALUES('91111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','owned-stage-resolution',repeat('a',64),'DISMISS','owned immutable proof','{}','2026-01-01');`); err != nil {
			t.Fatal("persist real immutable anomaly intent", err)
		}
		intent := stageValue(t, ctx, c, `SELECT to_jsonb(i)::text FROM payment_anomaly_resolution_intents i`)
		for _, query := range []string{`UPDATE payment_anomaly_resolution_intents SET reason='changed'`, `DELETE FROM payment_anomaly_resolution_intents`} {
			if _, err := c.Exec(ctx, query); err == nil {
				t.Fatal("immutable anomaly intent was altered")
			}
		}
		if intent != stageValue(t, ctx, c, `SELECT to_jsonb(i)::text FROM payment_anomaly_resolution_intents i`) {
			t.Fatal("rejected intent mutation changed durable decision")
		}
		var old string
		if err := c.QueryRow(ctx, `SELECT jsonb_build_array(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,publication_generation)::text FROM outbox_events WHERE event_id='51111111-1111-4111-8111-111111111111'`).Scan(&old); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(old, ", 0]") {
			t.Fatal("old outbox did not retain generation zero")
		}
		store, err := events.NewPostgresStore(ctx, f.serviceURL(t, c, "billing_service"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		claimed, err := store.Claim(ctx, "owned-stage-relay", 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("actual relay claim: count=%d error=%v", len(claimed), err)
		}
		event := claimed[0]
		if event.PublicationGeneration != 0 || event.LeaseUntil.IsZero() {
			t.Fatal("actual claim omitted durable generation/lease")
		}
		if _, err := c.Exec(ctx, `UPDATE outbox_events SET publication_generation=publication_generation+1 WHERE event_id=$1`, event.ID); err != nil {
			t.Fatal(err)
		}
		snapshot := stageValue(t, ctx, c, `SELECT to_jsonb(e)::text FROM outbox_events e`)
		if err := store.MarkPublished(ctx, "owned-stage-relay", event); err == nil {
			t.Fatal("stale generation acknowledgement accepted")
		}
		if snapshot != stageValue(t, ctx, c, `SELECT to_jsonb(e)::text FROM outbox_events e`) {
			t.Fatal("stale generation acknowledgement mutated outbox")
		}
		event.PublicationGeneration++
		if err := store.MarkPublished(ctx, "owned-stage-relay", event); err != nil {
			t.Fatal(err)
		}
		ledger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
		if err := migrations.DownOne(ctx, c, billing); err == nil {
			t.Fatal("forward-only financial phase migration was removed")
		}
		if ledger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`) {
			t.Fatal("failed rollback changed migration ledger")
		}
	})

	t.Run("simulator-durable-identity-and-rollback", func(t *testing.T) {
		c := f.database(t, ctx, "simulator", f.admin.Config().User)
		if err := migrations.Up(ctx, c, selectVersions(simulator, []int64{1, 2, 3, 4})); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `INSERT INTO simulator_orders(id,merchant_order_no,channel,amount_fen,currency,status,scenario,pay_token_hash,callback_url,expires_at,created_at,updated_at) VALUES('61111111-1111-4111-8111-111111111111','owned-old','UNIONPAY',100,'CNY','PENDING','SUCCESS',decode('01','hex'),'https://owned.invalid/callback','2030-01-01','2026-01-01','2026-01-01')`); err != nil {
			t.Fatal(err)
		}
		if err := simulatorschema.Up(ctx, c, simulator); err != nil {
			t.Fatal(err)
		}
		var mobile, sku, customer string
		if err := c.QueryRow(ctx, `SELECT mobile,provider_sku,merchant_customer_key FROM simulator_orders`).Scan(&mobile, &sku, &customer); err != nil || mobile != "" || sku != "" || customer != "" {
			t.Fatal("historical order identity was fabricated")
		}
		if _, err := c.Exec(ctx, `UPDATE simulator_orders SET mobile='13800138000',provider_sku='owned-sku',merchant_customer_key='11111111-1111-4111-8111-111111111111'`); err != nil {
			t.Fatal(err)
		}
		before := stageValue(t, ctx, c, `SELECT to_jsonb(o)::text FROM simulator_orders o`)
		for _, bad := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"} {
			if _, err := c.Exec(ctx, `UPDATE simulator_orders SET merchant_customer_key=$1`, bad); err == nil {
				t.Fatal("non-canonical customer UUID accepted")
			}
		}
		if err := migrations.DownOne(ctx, c, simulator); err == nil {
			t.Fatal("durable customer identity rollback accepted")
		}
		if before != stageValue(t, ctx, c, `SELECT to_jsonb(o)::text FROM simulator_orders o`) {
			t.Fatal("rollback or rejected UUID mutation lost original mapped facts")
		}
	})
}

type stageFixture struct {
	admin       *pgx.Conn
	config      *pgx.ConnConfig
	roles       []string
	dbs         map[string]string
	connections []*pgx.Conn
}

func newStageFixture(t *testing.T, ctx context.Context, raw string) *stageFixture {
	t.Helper()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("connect schema setup identity")
	}
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		admin.Close(context.Background())
		t.Fatal("parse schema fixture")
	}
	f := &stageFixture{admin: admin, config: config, dbs: make(map[string]string), roles: []string{"account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery", "billing_service", "billing_schema_owner", "billing_migrator"}}
	var count int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, f.roles).Scan(&count); err != nil || count != 0 {
		admin.Close(context.Background())
		t.Fatal("existing deployment actors: no fixture writes permitted")
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, role := range f.roles {
		login, inherit := "LOGIN", "NOINHERIT"
		if strings.HasSuffix(role, "_owner") || role == "account_approval_recovery" {
			login = "NOLOGIN"
		}
		if role == "account_service" || role == "billing_service" {
			inherit = "INHERIT"
		}
		if _, err := tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+login+" "+inherit+" NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal("exclusive actor creation rejected")
		}
	}
	if _, err := tx.Exec(ctx, `GRANT account_service,account_approval_control_owner TO account_migrator; GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT FALSE,SET TRUE,ADMIN TRUE; GRANT billing_service,billing_schema_owner TO billing_migrator`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 45*time.Second)
		defer stop()
		defer admin.Close(clean)
		for _, c := range f.connections {
			if err := c.Close(clean); err != nil {
				t.Error("owned migration connection close failed")
			}
		}
		allGone := true
		for name, owner := range f.dbs {
			var got string
			var active int
			deadline := time.Now().Add(5 * time.Second)
			var queryErr error
			for {
				queryErr = admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&got, &active)
				if queryErr != nil || got != owner || active == 0 || !time.Now().Before(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if queryErr != nil || got != owner || active != 0 {
				t.Error("owned DB cleanup identity/connection guard failed")
				allGone = false
				continue
			}
			if _, err := admin.Exec(clean, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
				t.Error("nonforce owned DB cleanup failed")
				allGone = false
			}
		}
		if !allGone {
			t.Error("owned actors retained after incomplete DB cleanup")
			return
		}
		var outside bool
		if err := admin.QueryRow(clean, `SELECT EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND dbid<>0) OR EXISTS(SELECT 1 FROM pg_auth_members WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))) AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))))`, f.roles).Scan(&outside); err != nil || outside {
			t.Error("external dependencies forbid actor cleanup")
			return
		}
		for i := len(f.roles) - 1; i >= 0; i-- {
			if _, err := admin.Exec(clean, "DROP ROLE "+pgx.Identifier{f.roles[i]}.Sanitize()); err != nil {
				t.Error("nonforce owned actor cleanup failed")
			}
		}
		if err := admin.QueryRow(clean, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, f.roles).Scan(&count); err != nil || count != 0 {
			t.Error("owned actors remain after cleanup")
		}
	})
	return f
}

func (f *stageFixture) database(t *testing.T, ctx context.Context, domain, owner string) *pgx.Conn {
	t.Helper()
	name := "f02_schema_" + domain + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := f.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER "+pgx.Identifier{owner}.Sanitize()); err != nil {
		t.Fatal("create exclusively owned DB")
	}
	f.dbs[name] = owner
	t.Logf("owned fixture database=%s owner=%s", name, owner)
	config := f.config.Copy()
	config.Database = name
	if domain == "account" {
		config.User = "account_migrator"
		config.Password = ""
		if _, err := f.admin.Exec(ctx, "GRANT CREATE ON DATABASE "+pgx.Identifier{name}.Sanitize()+" TO account_migrator"); err != nil {
			t.Fatal(err)
		}
	}
	if domain == "billing" {
		config.User = "billing_migrator"
		config.Password = ""
	}
	c, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("connect isolated migration actor")
	}
	f.connections = append(f.connections, c)
	if domain == "billing" {
		if _, err := c.Exec(ctx, `SET ROLE billing_schema_owner; ALTER SCHEMA public OWNER TO billing_schema_owner; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE,CREATE ON SCHEMA public TO billing_service`); err != nil {
			t.Fatal(err)
		}
	}
	if domain == "account" || domain == "billing" {
		if _, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{domain + "_service"}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		var session, current string
		var super bool
		if err := c.QueryRow(ctx, `SELECT session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&session, &current, &super); err != nil || session != domain+"_migrator" || current != domain+"_service" || super {
			t.Fatal("non-super migration actor contract failed")
		}
	}
	return c
}

func (f *stageFixture) serviceURL(t *testing.T, c *pgx.Conn, user string) string {
	t.Helper()
	parsed, err := url.Parse(f.config.ConnString())
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("schema fixture requires a PostgreSQL URL")
	}
	parsed.User = url.User(user)
	parsed.Path = "/" + c.Config().Database
	return parsed.String()
}
func selectVersions(input []migrations.Migration, versions []int64) []migrations.Migration {
	var out []migrations.Migration
	for _, m := range input {
		if slices.Contains(versions, m.Version) {
			out = append(out, m)
		}
	}
	return out
}
func stageValue(t *testing.T, ctx context.Context, c *pgx.Conn, query string) string {
	t.Helper()
	var value string
	if err := c.QueryRow(ctx, query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
func stageWalletFacts(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	return stageValue(t, ctx, c, `SELECT jsonb_agg(jsonb_build_array(id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,version,created_at,updated_at) ORDER BY id)::text FROM wallets`)
}
func stageAccountFacts(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	return stageValue(t, ctx, c, `SELECT jsonb_build_object('users',(SELECT jsonb_agg(jsonb_build_array(id,status,display_name,username,user_code,created_at,updated_at,membership_version,version) ORDER BY id) FROM users),'assignments',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM role_assignments p WHERE source='ACCOUNT'),'identities',(SELECT jsonb_agg(to_jsonb(p) ORDER BY issuer,subject) FROM identities p),'sessions',(SELECT jsonb_agg(to_jsonb(p) ORDER BY issuer,session_id) FROM user_sessions p),'outbox',(SELECT jsonb_agg(jsonb_build_array(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,next_attempt_at,attempts,locked_by,locked_until,published_at,dead_lettered_at,publication_generation) ORDER BY event_id) FROM outbox_events))::text`)
}

const stageAccountSeed = `
INSERT INTO users(id,status,display_name,username,user_code,created_at,updated_at) VALUES
('11111111-1111-4111-8111-111111111111','ACTIVE','owned active admin','stage-active','USR1111111111111111'||user_code_checksum('USR1111111111111111'),'2026-01-01','2026-01-01'),
('22222222-2222-4222-8222-222222222222','DISABLED','owned disabled user','stage-disabled','USR2222222222222222'||user_code_checksum('USR2222222222222222'),'2026-01-01','2026-01-01');
INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES
('41111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','PLATFORM_SUPER_ADMIN','ACCOUNT','2026-01-01','2026-01-01'),
('42222222-2222-4222-8222-222222222222','22222222-2222-4222-8222-222222222222','PLATFORM_BILLING_OPERATOR','ACCOUNT','2026-01-01','2026-01-01');
UPDATE authorization_versions SET version=10,updated_at='2026-01-01';
INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('https://owned.invalid','owned-subject','11111111-1111-4111-8111-111111111111','2026-01-01','2026-01-01');
INSERT INTO user_sessions(issuer,session_id,user_id,active_context_type,membership_version,last_seen_at,created_at) VALUES('https://owned.invalid','owned-session','11111111-1111-4111-8111-111111111111','PERSONAL',1,'2026-01-01','2026-01-01');
INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,next_attempt_at) VALUES('51111111-1111-4111-8111-111111111111','fixture.retained.v1','11111111-1111-4111-8111-111111111111',1,'{"fixture":"owned"}','owned','2026-01-01','2026-01-01');
`
const stageBillingSeed = `
INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,version,created_at,updated_at) VALUES('71111111-1111-4111-8111-111111111111','PERSONAL','11111111-1111-4111-8111-111111111111','ACTIVE',0,0,1,'2026-01-01','2026-01-01');
INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) VALUES('51111111-1111-4111-8111-111111111111','fixture.retained.v1','11111111-1111-4111-8111-111111111111',1,'{"fixture":"owned"}','owned','2026-01-01');
`

func stageShape(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	return stageValue(t, ctx, c, `
WITH ns AS(SELECT oid,nspname,nspowner,nspacl FROM pg_namespace WHERE nspname IN('public','account_approval_control','account_approval_authorization')),facts(value) AS(
 SELECT jsonb_build_array('namespace',nspname,pg_get_userbyid(nspowner),nspacl)::text FROM ns
 UNION ALL SELECT jsonb_build_array('relation',n.nspname,c.relname,c.relkind::text,pg_get_userbyid(c.relowner),c.relacl,c.relrowsecurity,c.relforcerowsecurity)::text FROM pg_class c JOIN ns n ON n.oid=c.relnamespace
 UNION ALL SELECT jsonb_build_array('column',n.nspname,c.relname,a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,a.attidentity::text,a.attgenerated::text,pg_get_expr(d.adbin,d.adrelid),a.attacl)::text FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN ns n ON n.oid=c.relnamespace LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attnum>0 AND NOT a.attisdropped
 UNION ALL SELECT jsonb_build_array('constraint',n.nspname,c.relname,k.conname,k.convalidated,pg_get_constraintdef(k.oid))::text FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN ns n ON n.oid=c.relnamespace
 UNION ALL SELECT jsonb_build_array('index',n.nspname,c.relname,pg_get_indexdef(i.indexrelid),i.indisvalid,i.indisready)::text FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN ns n ON n.oid=c.relnamespace
 UNION ALL SELECT jsonb_build_array('trigger',n.nspname,c.relname,g.tgname,g.tgenabled::text,pg_get_triggerdef(g.oid))::text FROM pg_trigger g JOIN pg_class c ON c.oid=g.tgrelid JOIN ns n ON n.oid=c.relnamespace WHERE NOT g.tgisinternal
 UNION ALL SELECT jsonb_build_array('function',n.nspname,p.proname,pg_get_function_identity_arguments(p.oid),pg_get_functiondef(p.oid),pg_get_userbyid(p.proowner),p.proacl)::text FROM pg_proc p JOIN ns n ON n.oid=p.pronamespace WHERE p.prokind='f'
) SELECT jsonb_agg(value ORDER BY value)::text FROM facts`)
}
