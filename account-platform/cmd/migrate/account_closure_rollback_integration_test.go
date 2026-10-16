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

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Build an actual early database from the original SQL. In particular, do not
// install the mature package and delete its ledger rows to manufacture MAX=28.
func TestAccountClosureLegacyRollbackCLI(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("parse isolated account closure CLI fixture")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("account closure CLI fixture requires a PostgreSQL URL")
	}
	control, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("connect isolated account closure CLI fixture")
	}
	defer control.Close(context.Background())
	name := "f08_closure_cli_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := control.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal("create owned account closure CLI database")
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		// The test's normal defers close connections before this cleanup. Use
		// a new control connection because the original control has also closed.
		cleanupControl, err := pgx.ConnectConfig(clean, config)
		if err != nil {
			t.Error("connect owned account closure cleanup control")
			return
		}
		defer cleanupControl.Close(context.Background())
		var owner string
		var active int
		if err := cleanupControl.QueryRow(clean, `SELECT pg_get_userbyid(datdba),
			(SELECT count(*) FROM pg_stat_activity WHERE datname=$1)
			FROM pg_database WHERE datname=$1`, name).Scan(&owner, &active); err != nil || owner != config.User || active != 0 {
			t.Error("owned account closure CLI database cleanup guard failed")
			return
		}
		if _, err := cleanupControl.Exec(clean, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Error("nonforce owned account closure CLI database cleanup failed")
		}
	})
	owned := config.Copy()
	owned.Database = name
	connection, err := pgx.ConnectConfig(ctx, owned)
	if err != nil {
		t.Fatal("connect owned account closure CLI database")
	}
	defer connection.Close(context.Background())
	loaded, err := migrations.Load(filepath.Join("../../migrations", "account"))
	if err != nil {
		t.Fatal(err)
	}
	// Full source validation must succeed before asserting the 028 guard. A
	// missing later identity/pin is not evidence that the guard has executed.
	if _, err := accountschema.Ordered(loaded); err != nil {
		t.Fatalf("complete original account package is invalid: %v", err)
	}
	var legacy []migrations.Migration
	for _, migration := range loaded {
		if migration.Version <= 28 {
			legacy = append(legacy, migration)
		}
	}
	if len(legacy) == 0 || legacy[len(legacy)-1].Version != 28 {
		t.Fatal("original early account package does not end at 028")
	}
	if err := migrations.Up(ctx, connection, legacy); err != nil {
		t.Fatalf("apply actual original early account migrations: %v", err)
	}
	if _, err := migrations.VerifyApplied(ctx, connection, legacy); err != nil {
		t.Fatalf("early ledger must contain exactly the original applied package: %v", err)
	}
	var latest int64
	if err := connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&latest); err != nil || latest != 28 {
		t.Fatal("actual early account database latest version is not 028")
	}
	migrations.Unlock(ctx, connection)
	seedAccountClosureRollbackFacts(t, ctx, connection)
	before := snapshotAccountClosureRollback(t, ctx, connection)

	t.Run("direct-public-guard-preserves-early-history", func(t *testing.T) {
		defer migrations.Unlock(context.Background(), connection)
		err := accountschema.DownOne(ctx, connection, loaded)
		if err == nil || !strings.Contains(err.Error(), "rollback migration 28:") ||
			!strings.Contains(err.Error(), "retained account closure cleanup rollback is unsupported") {
			t.Fatalf("public account DownOne must reach the explicit 028 guard, got %v", err)
		}
		migrations.Unlock(ctx, connection)
		if after := snapshotAccountClosureRollback(t, ctx, connection); after != before {
			t.Fatal("public rejected 028 rollback changed early data, ledger or schema")
		}
	})

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	for _, fixture := range []struct {
		name, alias string
		packageSQL  []migrations.Migration
	}{
		{"complete-package-account", "account", loaded},
		{"complete-package-renamed", "owned-renamed-account", loaded},
		{"early-package-renamed", "owned-renamed-early-account", legacy},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), fixture.alias)
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			for _, migration := range fixture.packageSQL {
				for _, direction := range []string{"up", "down"} {
					body := migration.UpSQL
					if direction == "down" {
						body = migration.DownSQL
					}
					path := filepath.Join(directory, fmt.Sprintf("%06d_%s.%s.sql", migration.Version, migration.Name, direction))
					if err := os.WriteFile(path, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			child := exec.CommandContext(ctx, executable, "-test.run=^TestPhaseMigrationCLIChild$")
			for _, value := range os.Environ() {
				key, _, _ := strings.Cut(value, "=")
				if key != "DATABASE_URL" && key != "MIGRATIONS_DIR" && key != "MIGRATION_ROLE" && key != "PINVOU_SCHEMA_CLI_CHILD" && key != "PINVOU_DEPLOYMENT_ENVIRONMENT" {
					child.Env = append(child.Env, value)
				}
			}
			child.Env = append(child.Env, "DATABASE_URL="+parsed.String(), "MIGRATIONS_DIR="+directory,
				"MIGRATION_ROLE=", "PINVOU_DEPLOYMENT_ENVIRONMENT=test", "PINVOU_SCHEMA_CLI_CHILD=owned-phase-down")
			output, err := child.CombinedOutput()
			if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
				t.Fatal("actual account CLI must reject rollback before changing the early database")
			}
			// The legacy-only source is recognized as account by the frozen 028
			// identity, then rejects incomplete Ordered input. Only the complete
			// package and direct public test above prove the specific 028 guard.
			if after := snapshotAccountClosureRollback(t, ctx, connection); after != before {
				t.Fatal("rejected account CLI rollback changed early data, ledger or schema")
			}
		})
	}
}

func seedAccountClosureRollbackFacts(t *testing.T, ctx context.Context, connection *pgx.Conn) {
	t.Helper()
	user, closedUser, organization, closure, invitation, audit := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	owner, membership, binding := uuid.New(), uuid.New(), uuid.New()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Original 021 requires a live OWNER even for DISABLED organizations.
	// Create the organization and its legal membership/binding in one ordinary
	// transaction so its deferred constraints see a complete owner at commit.
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,status,user_code,subject_lifecycle_version,created_at,updated_at)
		 VALUES($1,'CLOSING',generate_unique_user_code(),7,'2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`, []any{user}},
		{`INSERT INTO users(id,status,user_code,subject_lifecycle_version,anonymized_at,created_at,updated_at)
		 VALUES($1,'DELETED',generate_unique_user_code(),8,'2026-01-02T00:00:00Z','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`, []any{closedUser}},
		{`INSERT INTO users(id,status,user_code,created_at,updated_at)
		 VALUES($1,'ACTIVE',generate_unique_user_code(),'2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`, []any{owner}},
		{`INSERT INTO organizations(id,name,status,created_by,created_at,updated_at)
		 VALUES($1,'Owned closure rollback fixture','DISABLED',$2,'2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`, []any{organization, owner}},
		{`INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at)
		 VALUES($1,$2,$3,'ACTIVE','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`, []any{membership, organization, owner}},
		{`INSERT INTO role_bindings(id,membership_id,role,created_by,starts_at,created_at)
		 VALUES($1,$2,'OWNER',$3,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, []any{binding, membership, owner}},
		{`INSERT INTO account_closure_requests(id,user_id,idempotency_key_digest,request_fingerprint,status,stage,
		 reauthenticated_at,sla_expires_at,subject_lifecycle_version,cleanup_prepared_at,identity_deleted_at,
		 grants_isolated_at,created_at,updated_at)
		 VALUES($1,$2,repeat('a',64),repeat('b',64),'RETRY','IDENTITY_CLEANUP','2026-01-01T00:00:00Z','2026-01-03T00:00:00Z',
		 7,'2026-01-02T00:00:00Z','2026-01-02T00:00:00Z','2026-01-02T00:00:00Z','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`, []any{closure, user}},
		{`INSERT INTO account_closure_audit(id,closure_request_id,event_type,actor_user_id,from_stage,to_stage,occurred_at)
		 VALUES($1,$2,'CLEANUP_PREPARED',$3,'FINANCIAL_CLEARANCE','IDENTITY_CLEANUP','2026-01-02T00:00:00Z')`, []any{audit, closure, user}},
		{`INSERT INTO account_subject_retention_records(user_id,user_code,retention_class,retained_until,legal_hold,status,created_at)
		 SELECT id,user_code,'ACCOUNT_CLOSURE_LEGAL_RECORD','2030-01-01T00:00:00Z',true,'RETAINED','2026-01-02T00:00:00Z' FROM users WHERE id=$1`, []any{closedUser}},
		{`INSERT INTO invitations(id,organization_id,email_normalized,target_type,target_digest,target_hint,token_hash,
		 status,roles,invited_by,idempotency_key,request_hash,revoked_at,expires_at,created_at,updated_at)
		 VALUES($1,$2,NULL,'USERNAME',decode('00','hex'),NULL,decode('01','hex'),'CANCELLED_ACCOUNT_CLOSURE',
		 '[]',$3,'closure-rollback-invite',repeat('c',64),'2026-01-02T00:00:00Z','2026-01-03T00:00:00Z','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`, []any{invitation, organization, user}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed actual early account closure facts: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit complete early account closure facts and lawful organization owner: %v", err)
	}
}

// Snapshot all public table rows, not merely a count, plus structural definitions
// touched by original 028 Down (and any other unexpected public schema change).
func snapshotAccountClosureRollback(t *testing.T, ctx context.Context, connection *pgx.Conn) string {
	t.Helper()
	rows, err := connection.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		t.Fatal(rowErr)
	}
	var snapshot strings.Builder
	for _, table := range tables {
		var facts string
		query := `SELECT COALESCE(jsonb_agg(fact ORDER BY fact::text),'[]'::jsonb)::text FROM
		 (SELECT to_jsonb(t) AS fact FROM ` + pgx.Identifier{"public", table}.Sanitize() + ` t) facts`
		if err := connection.QueryRow(ctx, query).Scan(&facts); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&snapshot, "%s\n%s\n", table, facts)
	}
	var schema string
	if err := connection.QueryRow(ctx, `SELECT jsonb_build_object(
	 'columns',(SELECT jsonb_agg(to_jsonb(c) ORDER BY table_name,ordinal_position) FROM information_schema.columns c WHERE table_schema='public'),
	 'constraints',(SELECT jsonb_agg(jsonb_build_array(c.relname,k.conname,pg_get_constraintdef(k.oid)) ORDER BY c.relname,k.conname)
	   FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'),
	 'indexes',(SELECT jsonb_agg(to_jsonb(i) ORDER BY tablename,indexname) FROM pg_indexes i WHERE schemaname='public'),
	 'triggers',(SELECT jsonb_agg(jsonb_build_array(c.relname,t.tgname,pg_get_triggerdef(t.oid)) ORDER BY c.relname,t.tgname)
	   FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND NOT t.tgisinternal),
	 'functions',(SELECT jsonb_agg(jsonb_build_array(p.proname,pg_get_function_identity_arguments(p.oid),pg_get_functiondef(p.oid)) ORDER BY p.proname,pg_get_function_identity_arguments(p.oid))
	   FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' AND p.prokind IN ('f','p'))
	)::text`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	snapshot.WriteString(schema)
	return snapshot.String()
}
