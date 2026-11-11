package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This isolates original027 as the only protected identity: a full current
// package or ledger would otherwise mask a missing027 registration with other
// Account identities. The original historical SQL writes its own ledger.
func TestAccountClosureSagaIdentityDispatchPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit owned PostgreSQL required")
	}
	config, err := pgx.ParseConfig(raw)
	if err != nil || config.Host != "127.0.0.1" || config.User != "postgres" || config.Password != "" || config.Database != "postgres" || config.Port < 1024 || config.TLSConfig != nil || len(config.Fallbacks) != 0 || len(config.RuntimeParams) != 0 {
		t.Fatal("exclusive loopback postgres coordinator required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	admin, err := cifixture.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("owned identity coordinator rejected", err)
	}
	ready := false
	var roleFacts, memberFacts string
	roleQuery := `SELECT jsonb_agg(jsonb_build_array(oid,rolname,rolsuper,rolinherit,rolcreaterole,rolcreatedb,rolcanlogin,rolreplication,rolbypassrls) ORDER BY rolname)::text FROM pg_roles WHERE rolname IN('account_service','account_migrator')`
	memberQuery := `SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY roleid,member,grantor),'[]'::jsonb)::text FROM pg_auth_members m WHERE roleid IN(SELECT oid FROM pg_roles WHERE rolname IN('account_service','account_migrator')) OR member IN(SELECT oid FROM pg_roles WHERE rolname IN('account_service','account_migrator'))`
	// Registered before database cleanups, so all leased databases and their
	// clients retire first. Keep this same non-reconnecting coordinator backend.
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		defer admin.Close(clean)
		if !ready {
			return // Failed/unknown setup grants no role deletion authority.
		}
		if err := cifixture.Verify(clean, admin); err != nil {
			t.Error(err)
			return
		}
		var session, current string
		var super bool
		if err := admin.QueryRow(clean, `SELECT session_user,current_user,rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&session, &current, &super); err != nil || session != "postgres" || current != "postgres" || !super {
			t.Error("owned coordinator identity changed; no role deletion", err)
			return
		}
		var outside int
		if err := admin.QueryRow(clean, `SELECT
 (SELECT count(*) FROM pg_database WHERE datname NOT IN('postgres','template0','template1')) +
 (SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid()) +
 (SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname NOT IN('postgres','account_service','account_migrator'))`).Scan(&outside); err != nil || outside != 0 {
			t.Error("unconfirmed resources prohibit identity role cleanup", err)
			return
		}
		var roles, members string
		if err := admin.QueryRow(clean, roleQuery).Scan(&roles); err != nil || roles != roleFacts {
			t.Error("identity role OIDs or privileges changed; no deletion", err)
			return
		}
		if err := admin.QueryRow(clean, memberQuery).Scan(&members); err != nil || members != memberFacts {
			t.Error("identity role memberships changed; no deletion", err)
			return
		}
		if _, err := admin.Exec(clean, "BEGIN; DROP ROLE account_migrator; DROP ROLE account_service; COMMIT"); err != nil {
			t.Error("owned identity role cleanup", err)
		}
	})
	var session, current string
	var super bool
	if err := admin.QueryRow(ctx, `SELECT session_user,current_user,rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&session, &current, &super); err != nil || session != "postgres" || current != "postgres" || !super {
		t.Fatal("declared setup coordinator required before DDL", err)
	}
	var outside int
	if err := admin.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>'postgres') +
 (SELECT count(*) FROM pg_database WHERE datname NOT IN('postgres','template0','template1')) +
 (SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid()) +
 (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname !~ '^pg_' AND n.nspname<>'information_schema') +
 (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname !~ '^pg_' AND n.nspname<>'information_schema')`).Scan(&outside); err != nil || outside != 0 {
		t.Fatal("outside resources prohibit identity fixture writes", err)
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `CREATE ROLE account_service LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE account_migrator LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
GRANT account_service TO account_migrator`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, roleQuery).Scan(&roleFacts); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, memberQuery).Scan(&memberFacts); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("identity setup commit unconfirmed", err)
	}
	ready = true

	newDatabase := func(t *testing.T) (*pgx.Conn, string) {
		t.Helper()
		name := "f08_identity_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		lease, err := cifixture.CreateDatabase(ctx, admin, name, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER account_service")
		if err != nil {
			t.Fatal(err)
		}
		var connection *pgx.Conn
		t.Cleanup(func() {
			clean, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			if connection != nil {
				if err := connection.Close(clean); err != nil {
					t.Error(err)
				}
			}
			if _, err := cifixture.DropDatabase(clean, admin, lease, "account_service"); err != nil {
				t.Error("owned identity database cleanup", err)
			}
		})
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		parsed.User, parsed.Path = url.User("account_migrator"), "/"+name
		connection, err = pgx.Connect(ctx, parsed.String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(ctx, "SET ROLE account_service"); err != nil {
			t.Fatal(err)
		}
		var session, current string
		var privileged bool
		if err := connection.QueryRow(ctx, `SELECT session_user,current_user,rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&session, &current, &privileged); err != nil || session != "account_migrator" || current != "account_service" || privileged {
			t.Fatal("actual Account migration/runtime identity mismatch", err)
		}
		return connection, parsed.String()
	}
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var original migrations.Migration
	var predecessors []migrations.Migration
	for _, migration := range loaded {
		if migration.Version == 27 {
			original = migration
		}
		if migration.Version < 27 {
			predecessors = append(predecessors, migration)
		}
	}
	if original.Name != "account_closure_saga" || original.Checksum != "cfd14d187cd1c3fa3aab134de59cbfb88a44a7bbc88bc3e3406d8d6c7d2505db" {
		t.Fatal("original027 identity changed")
	}
	connection, databaseURL := newDatabase(t)
	probe := func(t *testing.T, c *pgx.Conn, selected []migrations.Migration, expected string) {
		t.Helper()
		defer migrations.Unlock(context.Background(), c)
		domain, err := migrationDomain(ctx, c, selected, "owned-renamed-account")
		if err != nil || domain != expected {
			t.Fatal("protected directory/partial/history dispatch", domain, err)
		}
	}
	t.Run("source-only-name-and-checksum", func(t *testing.T) {
		for _, candidate := range []migrations.Migration{original,
			{Version: 27, Name: original.Name, Checksum: "changed"},
			{Version: 27, Name: "owned-renamed-saga", Checksum: original.Checksum}} {
			probe(t, connection, []migrations.Migration{candidate}, "account")
		}
	})
	if err := migrations.Up(ctx, connection, append(predecessors, original)); err != nil {
		t.Fatal("actual original predecessor and027 writer", err)
	}
	migrations.Unlock(ctx, connection)
	t.Run("ledger-only-renamed-directory", func(t *testing.T) { probe(t, connection, nil, "account") })
	t.Run("ledger-checksum-only-from-original-writer", func(t *testing.T) {
		legacy, _ := newDatabase(t)
		alias := original
		alias.Name = "owned_renamed_saga"
		selected, err := migrations.Load(f08IdentitySources(t, []migrations.Migration{alias}))
		if err != nil || len(selected) != 1 || selected[0].Checksum != original.Checksum {
			t.Fatal("renamed original writer source changed", err)
		}
		if err := migrations.Up(ctx, legacy, append(predecessors, selected[0])); err != nil {
			t.Fatal("real historical original writer", err)
		}
		migrations.Unlock(ctx, legacy)
		probe(t, legacy, nil, "account")
	})
	t.Run("actual-cli-rejects-mutated-down-without-effects", func(t *testing.T) {
		before := f08IdentitySnapshot(t, ctx, connection)
		changed := original
		changed.DownSQL = "SELECT 1;" // Controlled bad source, never frozen repository SQL.
		directory := f08IdentitySources(t, []migrations.Migration{changed})
		output, err := f08IdentityCLI(t, ctx, databaseURL, directory)
		if err == nil || !strings.Contains(output, "MIGRATION_EXECUTION_FAILED") || f08IdentitySnapshot(t, ctx, connection) != before {
			t.Fatal("actual Account CLI bypassed its dedicated source guard", err, output)
		}
	})
	t.Run("mixed-domains-reject-before-writes", func(t *testing.T) {
		before := f08IdentitySnapshot(t, ctx, connection)
		mixed := []migrations.Migration{original, {Version: 900001, Name: "outbox_publication_generation"}}
		defer migrations.Unlock(context.Background(), connection)
		if _, err := migrationDomain(ctx, connection, mixed, "owned-renamed-account"); err == nil || !strings.Contains(err.Error(), "mixed protected") {
			t.Fatal("mixed Account027/Billing identities accepted", err)
		}
		if f08IdentitySnapshot(t, ctx, connection) != before {
			t.Fatal("mixed dispatch changed published facts")
		}
	})
	t.Run("unknown-custom027-keeps-generic-cli", func(t *testing.T) {
		custom, customURL := newDatabase(t)
		directory := f08IdentitySources(t, []migrations.Migration{{Version: 27, Name: "owned_custom_business", UpSQL: "CREATE TABLE owned_custom_facts(id BIGINT);", DownSQL: "DROP TABLE owned_custom_facts;"}})
		selected, err := migrations.Load(directory)
		if err != nil {
			t.Fatal(err)
		}
		if err := migrations.Up(ctx, custom, selected); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, custom)
		probe(t, custom, selected, "owned-renamed-account")
		output, err := f08IdentityCLI(t, ctx, customURL, directory)
		if err != nil {
			t.Fatal("unrelated custom027 no longer uses generic rollback", err, output)
		}
		var remains bool
		if err := custom.QueryRow(ctx, `SELECT to_regclass('owned_custom_facts') IS NOT NULL OR EXISTS(SELECT 1 FROM schema_migrations)`).Scan(&remains); err != nil || remains {
			t.Fatal("custom027 CLI did not preserve generic behavior", err)
		}
	})
}

func f08IdentitySources(t *testing.T, selected []migrations.Migration) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "owned-renamed-account")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, migration := range selected {
		for direction, body := range map[string]string{"up": migration.UpSQL, "down": migration.DownSQL} {
			if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%06d_%s.%s.sql", migration.Version, migration.Name, direction)), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return directory
}

func f08IdentityCLI(t *testing.T, ctx context.Context, raw, directory string) (string, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, executable, "-test.run=^TestPhaseMigrationCLIChild$")
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key != "DATABASE_URL" && key != "MIGRATIONS_DIR" && key != "MIGRATION_ROLE" && key != "PINVOU_SCHEMA_CLI_CHILD" && key != "PINVOU_DEPLOYMENT_ENVIRONMENT" {
			command.Env = append(command.Env, item)
		}
	}
	command.Env = append(command.Env, "DATABASE_URL="+raw, "MIGRATIONS_DIR="+directory, "MIGRATION_ROLE=account_service", "PINVOU_SCHEMA_CLI_CHILD=owned-phase-down", "PINVOU_DEPLOYMENT_ENVIRONMENT=test")
	output, err := command.CombinedOutput()
	return string(output), err
}

func f08IdentitySnapshot(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	t.Helper()
	var snapshot string
	if err := c.QueryRow(ctx, `SELECT jsonb_build_object('ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),'shape',(SELECT jsonb_agg(jsonb_build_array(c.oid,c.relname,c.relkind,c.relowner,c.relacl) ORDER BY c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'))::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
