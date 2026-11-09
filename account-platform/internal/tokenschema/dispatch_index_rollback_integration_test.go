package tokenschema

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func testDispatchIndexPublicRollback(t *testing.T, ctx context.Context, connection *pgx.Conn, config *pgx.ConnConfig, loaded []migrations.Migration) {
	t.Helper()
	ordered, err := Ordered(loaded)
	if err != nil {
		t.Fatal(err)
	}
	const snapshot = `SELECT jsonb_build_object('ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),'evidence',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY id),'[]'::jsonb) FROM usage_cancellation_evidence e),'index',pg_get_indexdef('public.usage_cancellation_dispatch_fence_idx'::regclass))::text`
	var before, after string
	if err = connection.QueryRow(ctx, snapshot).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// This is the exact DownOne branch called by cmd/migrate. The normal Up
	// identity must still fail closed, without changing the ledger or evidence.
	err = migrations.DownOne(ctx, connection, ordered)
	migrations.Unlock(ctx, connection)
	var denied *pgconn.PgError
	if !errors.As(err, &denied) || denied.Code != "42501" {
		t.Fatal("runtime-selected rollback must require the index owner", err)
	}
	if err = connection.QueryRow(ctx, snapshot).Scan(&after); err != nil || before != after {
		t.Fatal("denied rollback changed persistent facts", err)
	}
	var retained string
	if err = connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<>900008`).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("Go tool is required to exercise the shipped migration command", err)
	}
	binary := filepath.Join(t.TempDir(), "migrate.exe")
	build := exec.CommandContext(ctx, goTool, "build", "-tags=nomsgpack", "-mod=readonly", "-o", binary, "../../cmd/migrate")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public migration command: %v\n%s", err, output)
	}
	// pgx ConnString retains the original parsed DSN, before this fixture sets
	// its owned database and migrator. Serialize those current fields explicitly.
	cliURL := url.URL{Scheme: "postgresql", User: url.User(config.User), Host: net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))), Path: "/" + config.Database, RawQuery: "sslmode=disable"}
	run := func(role, direction string) {
		t.Helper()
		command := exec.CommandContext(ctx, binary, "--dir", "../../migrations/token", "--direction", direction)
		command.Env = append(os.Environ(), "DATABASE_URL="+cliURL.String(), "MIGRATION_ROLE="+role, "PINVOU_DEPLOYMENT_ENVIRONMENT=test")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("public migration command role=%s direction=%s: %v\n%s", role, direction, err, output)
		}
	}
	run("token_schema_owner", "down")
	var removed bool
	if err = connection.QueryRow(ctx, `SELECT to_regclass('public.usage_cancellation_dispatch_fence_idx') IS NULL AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900008)`).Scan(&removed); err != nil || !removed {
		t.Fatal("controlled CLI rollback did not remove exactly the new index identity", err)
	}
	var current string
	if err = connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&current); err != nil || current != retained {
		t.Fatal("controlled rollback changed predecessor ledger", err)
	}
	run("token_gateway", "up")
	if err = connection.QueryRow(ctx, snapshot).Scan(&after); err != nil {
		t.Fatal(err)
	}
	// Ledger installation timestamps legitimately change on re-install. Check
	// original rows separately and verify every current published SQL identity.
	if err = connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<>900008`).Scan(&current); err != nil || current != retained {
		t.Fatal("reinstallation changed predecessor ledger", err)
	}
	if _, err = VerifyApplied(ctx, connection, loaded); err != nil {
		t.Fatal("reinstalled public package failed identity verification", err)
	}
	t.Log("token_migrator session: gateway Down denied atomically; owner CLI Down and gateway CLI Up passed")
}
