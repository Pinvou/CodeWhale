package accountschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/f17fixture"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Tests actual role-assignment storage and the rollback guard, not the complete Down of a forward-only phase.
func TestAuditExportRollbackRoleStorageGuard(t *testing.T) {
	if os.Getenv("F17_TEST_DATABASE_URL") == "" {
		t.Skip("isolated PostgreSQL fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newAuditExportGuardFixture(t, ctx, os.Getenv("F17_TEST_DATABASE_URL"))
	c := f.database(t, ctx, "account", "account_service")
	loaded, e := migrations.Load("../../migrations/account")
	if e != nil {
		t.Fatal("SETUP_LOAD")
	}
	if e = Up(ctx, c, loaded); e != nil {
		t.Fatal("SETUP_UP")
	}
	migrations.Unlock(ctx, c)
	t.Run("unbound_actual_assignment_table", func(t *testing.T) {
		tx, e := c.Begin(ctx)
		if e != nil {
			t.Fatal("BEGIN")
		}
		defer tx.Rollback(ctx)
		if e = auditExportRollbackGuard(ctx, tx, migrations.Migration{Version: 32}); e != nil {
			t.Fatal("UNBOUND_GUARD_REJECTED")
		}
	})
	id := uuid.New()
	code, e := account.GenerateUserCode()
	if e != nil {
		t.Fatal("FIXTURE_CODE")
	}
	_, e = c.Exec(ctx, `INSERT INTO users(id,user_code,status,created_at,updated_at)VALUES($1,$2,'ACTIVE',clock_timestamp(),clock_timestamp())`, id, code)
	if e != nil {
		t.Fatal("FIXTURE_USER")
	}
	_, e = c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,created_at,starts_at,expires_at)VALUES($1,$2,'PLATFORM_AUDIT_PII_EXPORT','ACCOUNT',clock_timestamp(),clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour')`, uuid.New(), id)
	if e != nil {
		t.Fatal("FIXTURE_EXPIRED_BINDING")
	}
	t.Run("expired_assignment_preserved", func(t *testing.T) {
		tx, e := c.Begin(ctx)
		if e != nil {
			t.Fatal("BEGIN")
		}
		defer tx.Rollback(ctx)
		if e = auditExportRollbackGuard(ctx, tx, migrations.Migration{Version: 32}); e == nil {
			t.Fatal("BOUND_GUARD_ACCEPTED")
		}
		var n int
		if e = tx.QueryRow(ctx, `SELECT count(*) FROM role_assignments WHERE user_id=$1 AND role_code='PLATFORM_AUDIT_PII_EXPORT'`, id).Scan(&n); e != nil || n != 1 {
			t.Fatal("BINDING_CHANGED")
		}
	})
}

type auditExportGuardFixture struct {
	admin       *pgx.Conn
	config      *pgx.ConnConfig
	roles       []string
	dbs         map[string]string
	connections []*pgx.Conn
}

func newAuditExportGuardFixture(t *testing.T, ctx context.Context, raw string) *auditExportGuardFixture {
	t.Helper()
	config, err := pgx.ParseConfig(raw)
	if err != nil || !auditExportFixtureConfigurationAllowed(config, os.Getenv("F17_TEST_DATABASE_PROFILE"), os.Getenv("GITHUB_ACTIONS") == "true") {
		t.Fatal("dedicated isolated fixture configuration required")
	}
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("connect schema setup identity")
	}
	// Only this branch's fixtures share the deployment-actor set on their isolated instance.
	// The session lock serializes those fixtures across Go packages; it grants no authority.
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou.f17.audit-export.boundary.fixture'))`); err != nil {
		t.Fatal("exclusive fixture lock failed")
	}
	var directory, session, database string
	var port int
	var super bool
	if err := admin.QueryRow(ctx, `SELECT current_setting('data_directory'),session_user,current_database(),current_setting('port')::int,(SELECT rolsuper FROM pg_roles WHERE rolname=session_user)`).Scan(&directory, &session, &database, &port, &super); err != nil || session != config.User || database != "postgres" || port != int(config.Port) || !super {
		t.Fatal("isolated coordinator identity rejected")
	}
	if os.Getenv("F17_TEST_DATABASE_PROFILE") == "github-actions-service" {
		if directory != "/var/lib/postgresql/data" {
			t.Fatal("isolated CI service directory rejected")
		}
	} else if os.Getenv("F17_TEST_DATABASE_PROFILE") == f17fixture.OwnedProfile {
		if !f17fixture.DirectoryMatches(directory, os.Getenv("F17_TEST_OWNED_DATA_DIRECTORY")) {
			t.Fatal("owned temporary directory rejected")
		}
	} else if strings.ReplaceAll(directory, "\\", "/") != "D:/Projects/pinvou3-account-functional/.cache/parallel-runtimes/f04/data" {
		t.Fatal("owned local runtime directory rejected")
	}
	var outsideDatabases, outsideActors int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname NOT IN ('postgres','template0','template1')`).Scan(&outsideDatabases); err != nil || outsideDatabases != 0 {
		t.Fatal("outside databases prohibit fixture writes")
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>$1`, session).Scan(&outsideActors); err != nil || outsideActors != 0 {
		t.Fatal("outside actors prohibit fixture writes")
	}
	f := &auditExportGuardFixture{admin: admin, config: config, dbs: make(map[string]string), roles: []string{"account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery", "billing_service", "billing_schema_owner", "billing_migrator"}}
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

// CI is a separate ephemeral service profile, never a bypass for an arbitrary
// deployment URL. Local runs additionally verify their exact owned directory.
func auditExportFixtureConfigurationAllowed(config *pgx.ConnConfig, profile string, githubActions bool) bool {
	if config == nil || (config.Host != "127.0.0.1" && config.Host != "localhost") || config.Database != "postgres" || len(config.Fallbacks) != 0 {
		return false
	}
	if profile == f17fixture.OwnedProfile {
		return f17fixture.OwnedConfigurationAllowed(config, os.Getenv("F17_TEST_OWNED_PORT"), os.Getenv("F17_TEST_OWNED_DATA_DIRECTORY"))
	}
	if profile == "github-actions-service" {
		return githubActions && config.Port == 5432 && config.User == "postgres"
	}
	return profile == "" && config.Port == 55504 && config.User == "pinvou_wave_test"
}

func (f *auditExportGuardFixture) database(t *testing.T, ctx context.Context, domain, owner string) *pgx.Conn {
	t.Helper()
	name := "f17_guard_" + domain + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

func (f *auditExportGuardFixture) serviceURL(t *testing.T, c *pgx.Conn, user string) string {
	t.Helper()
	parsed, err := url.Parse(f.config.ConnString())
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("schema fixture requires a PostgreSQL URL")
	}
	parsed.User = url.User(user)
	parsed.Path = "/" + c.Config().Database
	return parsed.String()
}
