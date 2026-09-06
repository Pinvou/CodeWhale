package migrations

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This uses no application fixtures or unpublished migrations: HEAD plus this
// infrastructure patch must independently prove the restricted upgrade path.
func TestRestrictedMigrationLedgerPostgresIntegration(t *testing.T) {
	rawURL := os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL")
	if rawURL == "" {
		t.Skip("PINVOU_TEST_TOKEN_DATABASE_URL is required for real PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	control, err := pgx.Connect(ctx, rawURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	role, deniedRole := "ledger_writer_"+suffix, "ledger_denied_"+suffix
	schema, emptySchema := "ledger_upgrade_"+suffix, "ledger_empty_"+suffix
	password := strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedRole, quotedDenied := pgx.Identifier{role}.Sanitize(), pgx.Identifier{deniedRole}.Sanitize()
	quotedSchema, quotedEmpty := pgx.Identifier{schema}.Sanitize(), pgx.Identifier{emptySchema}.Sanitize()
	if _, err = control.Exec(ctx, "CREATE ROLE "+quotedRole+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT PASSWORD '"+password+"'; CREATE ROLE "+quotedDenied+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT PASSWORD '"+password+"'; CREATE SCHEMA "+quotedSchema+"; CREATE SCHEMA "+quotedEmpty+"; GRANT USAGE,CREATE ON SCHEMA "+quotedSchema+" TO "+quotedRole+"; GRANT USAGE ON SCHEMA "+quotedSchema+" TO "+quotedDenied+"; GRANT USAGE ON SCHEMA "+quotedEmpty+" TO "+quotedRole); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := control.Exec(cleanup, "DROP SCHEMA "+quotedSchema+" CASCADE; DROP SCHEMA "+quotedEmpty+" CASCADE; DROP ROLE "+quotedRole+"; DROP ROLE "+quotedDenied); err != nil {
			t.Errorf("cleanup isolated migration fixture: %v", err)
		}
	})
	connect := func(actor, path string) *pgx.Conn {
		t.Helper()
		configuration, parseErr := pgx.ParseConfig(rawURL)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		configuration.User, configuration.Password = actor, password
		configuration.RuntimeParams["search_path"] = path
		connection, connectErr := pgx.ConnectConfig(ctx, configuration)
		if connectErr != nil {
			t.Fatal(connectErr)
		}
		t.Cleanup(func() { _ = connection.Close(context.Background()) })
		var database, session, current string
		var super, create bool
		if identityErr := connection.QueryRow(ctx, `SELECT current_database(),session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=session_user),has_schema_privilege(current_schema(),'CREATE')`).Scan(&database, &session, &current, &super, &create); identityErr != nil {
			t.Fatal(identityErr)
		}
		if database != configuration.Database || session != actor || current != actor || super {
			t.Fatalf("not actual restricted identity database=%s session=%s current=%s super=%v", database, session, current, super)
		}
		return connection
	}
	writer := connect(role, schema)
	first := Migration{Version: 1, Name: "bootstrap", UpSQL: `CREATE TABLE migration_probe(id INTEGER PRIMARY KEY)`, DownSQL: `DROP TABLE migration_probe`}
	first.Checksum = normalizedChecksum(first.UpSQL)
	second := Migration{Version: 2, Name: "restricted_dml", UpSQL: `INSERT INTO migration_probe(id) VALUES(7)`, DownSQL: `DELETE FROM migration_probe WHERE id=7`}
	second.Checksum = normalizedChecksum(second.UpSQL)
	if err = Up(ctx, writer, []Migration{first}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	Unlock(ctx, writer)
	if _, err = control.Exec(ctx, "REVOKE CREATE ON SCHEMA "+quotedSchema+" FROM "+quotedRole); err != nil {
		t.Fatal(err)
	}
	var mayCreate bool
	if err = writer.QueryRow(ctx, `SELECT has_schema_privilege(current_schema(),'CREATE')`).Scan(&mayCreate); err != nil || mayCreate {
		t.Fatalf("CREATE was not revoked: %v create=%v", err, mayCreate)
	}
	loaded := []Migration{first, second}
	t.Run("existing_ledger_upgrade_and_down", func(t *testing.T) {
		if err := Up(ctx, writer, loaded); err != nil {
			t.Fatalf("restricted upgrade: %v", err)
		}
		Unlock(ctx, writer)
		if _, err := VerifyApplied(ctx, writer, loaded); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := writer.QueryRow(ctx, `SELECT count(*) FROM migration_probe WHERE id=7`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("DML effect count=%d err=%v", count, err)
		}
		if err := Up(ctx, writer, loaded); err != nil {
			t.Fatalf("idempotent upgrade: %v", err)
		}
		Unlock(ctx, writer)
		if err := DownOne(ctx, writer, loaded); err != nil {
			t.Fatalf("restricted down: %v", err)
		}
		Unlock(ctx, writer)
		if _, err := VerifyApplied(ctx, writer, []Migration{first}); err != nil {
			t.Fatal(err)
		}
		if err := writer.QueryRow(ctx, `SELECT count(*) FROM migration_probe`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("down effect count=%d err=%v", count, err)
		}
		if err := Up(ctx, writer, loaded); err != nil {
			t.Fatal(err)
		}
		Unlock(ctx, writer)
	})
	t.Run("checksum_remains_closed", func(t *testing.T) {
		changed := first
		changed.UpSQL = `CREATE TABLE changed_probe(id INTEGER)`
		changed.Checksum = normalizedChecksum(changed.UpSQL)
		if err := Up(ctx, writer, []Migration{changed, second}); err == nil || !strings.Contains(err.Error(), "checksum changed") {
			t.Fatalf("modified history accepted: %v", err)
		}
		Unlock(ctx, writer)
		if _, err := VerifyApplied(ctx, writer, loaded); err != nil {
			t.Fatalf("history changed: %v", err)
		}
	})
	t.Run("existing_ledger_without_permission", func(t *testing.T) {
		denied := connect(deniedRole, schema)
		assertMigrationPermissionDenied(t, Up(ctx, denied, loaded))
		Unlock(ctx, denied)
	})
	t.Run("empty_schema_without_create", func(t *testing.T) {
		empty := connect(role, emptySchema)
		assertMigrationPermissionDenied(t, Up(ctx, empty, []Migration{first}))
		Unlock(ctx, empty)
		var exists bool
		if err := empty.QueryRow(ctx, `SELECT pg_catalog.to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil || exists {
			t.Fatalf("unauthorized bootstrap created ledger=%v err=%v", exists, err)
		}
	})
	t.Run("advisory_lock_still_serializes", func(t *testing.T) {
		if _, err := control.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`); err != nil {
			t.Fatal(err)
		}
		defer Unlock(ctx, control)
		waiter := connect(role, schema)
		bounded, done := context.WithTimeout(ctx, 80*time.Millisecond)
		defer done()
		if err := Up(bounded, waiter, loaded); err == nil || !strings.Contains(err.Error(), "acquire migration lock") || !errors.Is(bounded.Err(), context.DeadlineExceeded) {
			t.Fatalf("expected bounded wait for the migration lock, got %v", err)
		}
		_ = waiter.Close(context.Background()) // cancel cannot leave a session lock behind
		Unlock(ctx, control)
		if err := Up(ctx, writer, loaded); err != nil {
			t.Fatalf("after lock release: %v", err)
		}
		Unlock(ctx, writer)
	})
}

func assertMigrationPermissionDenied(t *testing.T, err error) {
	t.Helper()
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "42501" {
		t.Fatalf("expected real permission denial, got %v", err)
	}
}
