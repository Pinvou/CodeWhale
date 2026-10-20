package billingschema

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// Use the actual bootstrap script in a fixture database owned by the cluster
// administrator, rather than silently supplying database CREATE via owner=the
// schema owner. All actors/databases are the original exclusive fixture's.
func TestF09BootstrapDatabaseCreateKeepsDBAOwner(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual isolated PostgreSQL schema fixture is required")
	}
	psql := os.Getenv("PINVOU_TEST_PSQL")
	if psql == "" {
		var err error
		psql, err = exec.LookPath("psql")
		if err != nil {
			t.Fatal("enabled PostgreSQL bootstrap test requires psql or PINVOU_TEST_PSQL")
		}
	}
	script, err := filepath.Abs("../../deploy/postgres/billing-roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	makeDBAOwned := func(t *testing.T) *pgx.Conn {
		t.Helper()
		c := f.database(t, ctx, "billing", "billing_schema_owner")
		name := c.Config().Database
		if f.dbs[name] != "billing_schema_owner" || !strings.HasPrefix(name, "f02_schema_billing_") || len(strings.TrimPrefix(name, "f02_schema_billing_")) != 32 || f.config.User == "billing_schema_owner" {
			t.Fatal("only the exclusive fixture database may change owner")
		}
		if _, err := f.admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER TO "+pgx.Identifier{f.config.User}.Sanitize()); err != nil {
			t.Fatal("fixture DBA owner", err)
		}
		// The shared cleanup must validate the new fixture owner too.
		f.dbs[name] = f.config.User
		return c
	}
	runBootstrap := func(t *testing.T, c *pgx.Conn) (string, error) {
		t.Helper()
		// ConnConfig.ConnString retains the originally parsed database even
		// after its Database field changes. Rebuild the original URL's path
		// explicitly, retaining authentication and connection options.
		connection, err := url.Parse(f.config.ConnString())
		if err != nil || (connection.Scheme != "postgres" && connection.Scheme != "postgresql") {
			t.Fatal("bootstrap fixture requires a PostgreSQL URL")
		}
		name := c.Config().Database
		connection.Path = "/" + name
		cmd := exec.CommandContext(ctx, psql, "-X", "-v", "ON_ERROR_STOP=1", "-v", "billing_database="+name, "--dbname", connection.String(), "-f", script)
		out, err := cmd.CombinedOutput()
		// Do not emit connection strings, secret delivery, or arbitrary psql
		// connection diagnostics into the public test log.
		t.Logf("actual psql bootstrap output bytes=%d", len(out))
		// Log only fixed classifications, never raw psql output, the URL,
		// identities from a connection failure, or credential values.
		for _, diagnostic := range []struct{ needle, label string }{
			{"connected database differs from the explicit Billing target", "target_database_mismatch"},
			{"database CREATE requires operator investigation", "existing_database_create_guard"},
			{"syntax error", "sql_syntax_error"},
			{"permission denied", "sql_permission_denied"},
			{"unsafe existing billing identities require operator investigation", "existing_role_guard"},
		} {
			if strings.Contains(string(out), diagnostic.needle) {
				t.Logf("actual psql bootstrap diagnostic=%s", diagnostic.label)
			}
		}
		return string(out), err
	}
	t.Run("fresh-DBA-owned-public-up", func(t *testing.T) {
		c := makeDBAOwned(t)
		if _, err := runBootstrap(t, c); err != nil {
			t.Fatal("actual target bootstrap failed", err)
		}
		var owner, current, session string
		var ownerCreate, runtimeCreate, migratorCreate, ownerLogin, ownerCreateDB bool
		if err := c.QueryRow(ctx, `SELECT pg_get_userbyid(datdba),
has_database_privilege('billing_schema_owner',oid,'CREATE'),
has_database_privilege('billing_service',oid,'CREATE'),
has_database_privilege('billing_migrator',oid,'CREATE'),current_user,session_user,
(SELECT rolcanlogin FROM pg_roles WHERE rolname='billing_schema_owner'),
(SELECT rolcreatedb FROM pg_roles WHERE rolname='billing_schema_owner')
FROM pg_database WHERE datname=current_database()`).Scan(&owner, &ownerCreate, &runtimeCreate, &migratorCreate, &current, &session, &ownerLogin, &ownerCreateDB); err != nil || owner != f.config.User || !ownerCreate || runtimeCreate || migratorCreate || ownerLogin || ownerCreateDB || current != "billing_service" || session != "billing_migrator" {
			t.Fatalf("bootstrap owner=%s create(owner/runtime/migrator)=%v/%v/%v identity=%s/%s ownerLogin/CreateDB=%v/%v err=%v", owner, ownerCreate, runtimeCreate, migratorCreate, session, current, ownerLogin, ownerCreateDB, err)
		}
		var predecessor []migrations.Migration
		for _, m := range loaded {
			// Preserve the original F09 hardened predecessor. The F10 Ledger
			// 061/062/900014 phases require the correction capability; the
			// complete public upgrade below still includes every migration.
			if (m.Version >= 56 && m.Version <= 59) || m.Version == 900013 || m.Version == 61 || m.Version == 62 || m.Version == 900014 {
				continue
			}
			predecessor = append(predecessor, m)
		}
		if err := Up(ctx, c, predecessor); err != nil {
			t.Fatal("original hardened predecessor", err)
		}
		if _, err := c.Exec(ctx, stageBillingSeed); err != nil {
			t.Fatal("controlled retained wallet/outbox", err)
		}
		before := stageWalletFacts(t, ctx, c)
		var eventBefore string
		if err := c.QueryRow(ctx, `SELECT to_jsonb(e)::text FROM outbox_events e WHERE event_id='51111111-1111-4111-8111-111111111111'`).Scan(&eventBefore); err != nil {
			t.Fatal(err)
		}
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal("ordinary full public Up including original900013", err)
		}
		if _, err := migrations.VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		var installed, capability bool
		var eventAfter string
		if err := c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900013),
to_regprocedure('billing_approval_execution.lock_entitlement_activation()') IS NOT NULL,
pg_get_userbyid(datdba),current_user,session_user,
has_database_privilege('billing_service',oid,'CREATE'),has_database_privilege('billing_migrator',oid,'CREATE')
FROM pg_database WHERE datname=current_database()`).Scan(&installed, &capability, &owner, &current, &session, &runtimeCreate, &migratorCreate); err != nil || !installed || !capability || owner != f.config.User || current != "billing_service" || session != "billing_migrator" || runtimeCreate || migratorCreate {
			t.Fatal("public Up changed deployment identity or omitted capability", err)
		}
		if err := c.QueryRow(ctx, `SELECT to_jsonb(e)::text FROM outbox_events e WHERE event_id='51111111-1111-4111-8111-111111111111'`).Scan(&eventAfter); err != nil || eventAfter != eventBefore || stageWalletFacts(t, ctx, c) != before {
			t.Fatal("public upgrade rewrote retained seed facts", err)
		}
	})
	t.Run("existing-PUBLIC-create-refused-without-revocation", func(t *testing.T) {
		c := makeDBAOwned(t)
		name := c.Config().Database
		if _, err := f.admin.Exec(ctx, "GRANT CREATE ON DATABASE "+pgx.Identifier{name}.Sanitize()+" TO PUBLIC"); err != nil {
			t.Fatal(err)
		}
		facts := func() string {
			t.Helper()
			var result string
			if err := f.admin.QueryRow(ctx, `SELECT jsonb_build_object(
'database',(SELECT jsonb_build_array(pg_get_userbyid(datdba),datacl) FROM pg_database WHERE datname=$1),
'roles',(SELECT jsonb_agg(to_jsonb(r) ORDER BY rolname) FROM pg_roles r WHERE rolname=ANY($2)),
'members',(SELECT jsonb_agg(to_jsonb(a) ORDER BY roleid,member) FROM pg_auth_members a WHERE roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($2)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($2))))::text`, name, f.roles).Scan(&result); err != nil {
				t.Fatal(err)
			}
			return result
		}
		before := facts()
		output, err := runBootstrap(t, c)
		if err == nil || !strings.Contains(output, "database CREATE requires operator investigation") {
			t.Fatal("actual database CREATE guard diagnostic was not observed")
		}
		if after := facts(); after != before {
			t.Fatal("refused bootstrap revoked or changed existing database/role facts")
		}
	})
	t.Run("existing-inherited-create-refused-without-membership-change", func(t *testing.T) {
		c := makeDBAOwned(t)
		name := c.Config().Database
		if _, err := f.admin.Exec(ctx, "GRANT CREATE ON DATABASE "+pgx.Identifier{name}.Sanitize()+" TO billing_schema_owner; GRANT billing_schema_owner TO billing_migrator WITH INHERIT TRUE,SET TRUE"); err != nil {
			t.Fatal(err)
		}
		var effective bool
		if err := f.admin.QueryRow(ctx, `SELECT has_database_privilege('billing_migrator',$1,'CREATE')`, name).Scan(&effective); err != nil || !effective {
			t.Fatal("fixture did not establish actual inherited database CREATE", err)
		}
		facts := func() string {
			t.Helper()
			var result string
			if err := f.admin.QueryRow(ctx, `SELECT jsonb_build_object(
'database',(SELECT jsonb_build_array(pg_get_userbyid(datdba),datacl) FROM pg_database WHERE datname=$1),
'roles',(SELECT jsonb_agg(to_jsonb(r) ORDER BY rolname) FROM pg_roles r WHERE rolname=ANY($2)),
'members',(SELECT jsonb_agg(to_jsonb(a) ORDER BY roleid,member) FROM pg_auth_members a WHERE roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($2)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($2))))::text`, name, f.roles).Scan(&result); err != nil {
				t.Fatal(err)
			}
			return result
		}
		before := facts()
		output, err := runBootstrap(t, c)
		if err == nil || !strings.Contains(output, "database CREATE requires operator investigation") {
			t.Fatal("actual inherited database CREATE guard diagnostic was not observed")
		}
		if after := facts(); after != before {
			t.Fatal("refused bootstrap changed inherited membership or existing database/role facts")
		}
	})
}
