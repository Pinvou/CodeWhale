package main

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Exercise the built CLI, including flag/env selection, fatal exits, lock waits,
// committed-prefix recovery and transactional Down. No migration engine is mocked.
func TestMigrationCLIBudgetRecovery(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	goCommand := os.Getenv("PINVOU_ACCOUNT_TEST_GO")
	if goCommand == "" {
		goCommand = "go"
	}
	binary := filepath.Join(t.TempDir(), "migrate")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, goCommand, "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build actual migration CLI: %v\n%s", err, output)
	}
	control, err := cifixture.Connect(ctx, raw)
	if err != nil {
		t.Fatal("connect owned CLI fixture")
	}
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("parse owned CLI fixture")
	}
	name := "budget_cli_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	lease, err := cifixture.CreateDatabase(ctx, control, name, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	if err != nil {
		t.Fatal("create owned CLI database")
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if _, err := cifixture.DropDatabase(clean, control, lease, config.User); err != nil {
			t.Error("retire owned CLI database without disconnecting clients")
		}
	})
	ownedConfig := config.Copy()
	ownedConfig.Database = name
	connection, err := pgx.ConnectConfig(ctx, ownedConfig)
	if err != nil {
		t.Fatal("connect owned probe database")
	}
	defer connection.Close(context.Background())
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("fixture requires a PostgreSQL URL")
	}
	parsed.Path = "/" + name
	directory := t.TempDir()
	for file, sql := range map[string]string{
		"001_budget_probe.up.sql":   "CREATE TABLE budget_effects(n integer NOT NULL); INSERT INTO budget_effects VALUES (1);",
		"002_budget_wait.up.sql":    "INSERT INTO budget_effects VALUES (2); SELECT pg_advisory_xact_lock(8675309);",
		"002_budget_wait.down.sql":  "DELETE FROM budget_effects WHERE n=2; SELECT pg_advisory_xact_lock(8675309);",
		"001_budget_probe.down.sql": "DROP TABLE budget_effects;",
	} {
		if err := os.WriteFile(filepath.Join(directory, file), []byte(sql), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(t *testing.T, environment string, args ...string) (string, error) {
		t.Helper()
		commandCtx, stop := context.WithTimeout(ctx, 15*time.Second)
		defer stop()
		command := exec.CommandContext(commandCtx, binary, append([]string{"--dir", directory}, args...)...)
		for _, value := range os.Environ() {
			key, _, _ := strings.Cut(value, "=")
			switch strings.ToUpper(key) {
			case "DATABASE_URL", "MIGRATIONS_DIR", "MIGRATION_ROLE", "MIGRATION_TIMEOUT", "PINVOU_DEPLOYMENT_ENVIRONMENT":
				continue
			}
			command.Env = append(command.Env, value)
		}
		command.Env = append(command.Env, "DATABASE_URL="+parsed.String(), "MIGRATION_ROLE=", "PINVOU_DEPLOYMENT_ENVIRONMENT=test", "MIGRATION_TIMEOUT="+environment)
		output, err := command.CombinedOutput()
		if commandCtx.Err() != nil {
			t.Fatal("CLI exceeded independent harness deadline; this is not a product timeout pass")
		}
		return string(output), err
	}
	execSQL := func(t *testing.T, sql string) {
		t.Helper()
		if _, err := connection.Exec(ctx, sql); err != nil {
			t.Fatal("owned fixture SQL failed")
		}
	}
	assertFailure := func(t *testing.T, output string, err error, code string) {
		t.Helper()
		if err == nil || !strings.Contains(output, code) || strings.Contains(output, "migration.post_verification") || strings.Contains(output, parsed.String()) {
			t.Fatalf("CLI failure missing safe code %s or emitted success/secret", code)
		}
	}
	assertState := func(t *testing.T, want int) {
		t.Helper()
		var effects, ledger int
		var total int
		if err := connection.QueryRow(ctx, `SELECT count(*), coalesce(sum(n),0) FROM budget_effects`).Scan(&effects, &total); err != nil {
			t.Fatal("read actual migration effects")
		}
		if err := connection.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&ledger); err != nil || ledger != want || effects != want || total != want*(want+1)/2 {
			t.Fatalf("committed effects/ledger: got %d/%d sum %d, want %d", effects, ledger, total, want)
		}
	}
	t.Run("invalid_budget_before_database", func(t *testing.T) {
		output, err := run(t, "0s")
		assertFailure(t, output, err, "MIGRATION_TIMEOUT_INVALID")
	})
	t.Run("domain_lock_wait", func(t *testing.T) {
		execSQL(t, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`)
		defer execSQL(t, `SELECT pg_advisory_unlock(hashtext('pinvou_account_platform_migrations'))`)
		output, err := run(t, "1s")
		assertFailure(t, output, err, "MIGRATION_DEADLINE_EXCEEDED")
		var exists bool
		if err := connection.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists); err != nil || exists {
			t.Fatal("lock timeout changed migration state")
		}
	})
	t.Run("partial_up_timeout", func(t *testing.T) {
		execSQL(t, `SELECT pg_advisory_lock(8675309)`)
		defer execSQL(t, `SELECT pg_advisory_unlock(8675309)`)
		output, err := run(t, "1s")
		assertFailure(t, output, err, "MIGRATION_DEADLINE_EXCEEDED")
		assertState(t, 1)
	})
	t.Run("explicit_flag_overrides_invalid_env_and_retry", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			output, err := run(t, "invalid-environment", "--timeout", "5s")
			var evidence migrationEvidence
			if err != nil || json.Unmarshal([]byte(output), &evidence) != nil || evidence.Event != "migration.post_verification" || evidence.AppliedCount != 2 || evidence.LatestVersion != 2 || evidence.LatestChecksum == "" {
				t.Fatal("same-package retry/replay did not complete actual post-verification")
			}
			assertState(t, 2)
		}
	})
	t.Run("transactional_down_timeout", func(t *testing.T) {
		execSQL(t, `SELECT pg_advisory_lock(8675309)`)
		defer execSQL(t, `SELECT pg_advisory_unlock(8675309)`)
		output, err := run(t, "1s", "--direction", "down")
		assertFailure(t, output, err, "MIGRATION_DEADLINE_EXCEEDED")
		assertState(t, 2)
	})
	t.Run("down_retry", func(t *testing.T) {
		output, err := run(t, "5s", "--direction", "down")
		if err != nil || output != "" {
			t.Fatal("explicit down retry failed or emitted an up receipt")
		}
		assertState(t, 1)
	})
	t.Run("successful_unlock_preserves_connection", func(t *testing.T) {
		owner, err := pgx.ConnectConfig(ctx, ownedConfig)
		if err != nil {
			t.Fatal("connect owned lock session")
		}
		defer owner.Close(context.Background())
		for i := 0; i < 2; i++ {
			if _, err := owner.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`); err != nil {
				t.Fatal("acquire nested owned lock")
			}
		}
		if err := migrations.Unlock(ctx, owner); err != nil || owner.IsClosed() {
			t.Fatal("successful unlock retired a reusable session")
		}
		var acquired bool
		if err := connection.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&acquired); err != nil || acquired {
			t.Fatal("unlock released more than one nested acquisition")
		}
		if err := migrations.Unlock(ctx, owner); err != nil || owner.IsClosed() {
			t.Fatal("second successful unlock failed")
		}
		if err := connection.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&acquired); err != nil || !acquired {
			t.Fatal("balanced unlock retained the session lock")
		}
		if err := migrations.Unlock(ctx, connection); err != nil {
			t.Fatal("release observer's owned lock")
		}
	})
	t.Run("expired_unlock_retires_owned_connection", func(t *testing.T) {
		owner, err := pgx.ConnectConfig(ctx, ownedConfig)
		if err != nil {
			t.Fatal("connect owned cancellation session")
		}
		defer owner.Close(context.Background())
		if _, err := owner.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`); err != nil {
			t.Fatal("acquire owned cancellation lock")
		}
		expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer stop()
		if owner.IsClosed() {
			t.Fatal("cancellation fixture must start with a healthy session")
		}
		if err := migrations.Unlock(expired, owner); err == nil || !owner.IsClosed() {
			t.Fatal("expired cleanup retained a healthy locked connection")
		}
		// The database itself acknowledges release: wait on the same lock with
		// an independent finite observer, rather than assuming process exit did it.
		observe, stopObserve := context.WithTimeout(ctx, 5*time.Second)
		defer stopObserve()
		if _, err := connection.Exec(observe, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`); err != nil {
			t.Fatal("retired session still blocks the next owned acquisition")
		}
		if err := migrations.Unlock(observe, connection); err != nil {
			t.Fatal("release observer's cancellation probe lock")
		}
	})
}
