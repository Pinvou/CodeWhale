package tokenschema

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestManagementPublicMigrationInstallation(t *testing.T) {
	dsn := os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL")
	if dsn == "" {
		t.Skip("PINVOU_TEST_TOKEN_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	roles := []string{"token_gateway", "token_cap_policy_deployer", "token_schema_owner", "token_migrator", "token_policy_operator"}
	var existing int
	if err = admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&existing); err != nil || existing != 0 {
		t.Fatalf("isolated role absence guard: count=%d err=%v", existing, err)
	}
	created := []string{}
	database := "f04_token_management_" + uuid.New().String()[:8] + fmt.Sprintf("%d", time.Now().UnixNano())
	databaseCreated := false
	defer func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if databaseCreated {
			var owner string
			var active int
			deadline := time.Now().Add(5 * time.Second)
			for {
				if err := admin.QueryRow(cleanupCtx, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, database).Scan(&owner, &active); err != nil {
					t.Error("owned database cleanup identity unavailable")
					return
				}
				if owner != admin.Config().User {
					t.Error("owned database owner changed; cleanup refused")
					return
				}
				if active == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Error("owned database still active; cleanup refused")
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := admin.Exec(cleanupCtx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
				t.Error("owned database cleanup failed; roles retained")
				return
			}
		}
		for i := len(created) - 1; i >= 0; i-- {
			if _, err := admin.Exec(cleanupCtx, "DROP ROLE "+pgx.Identifier{created[i]}.Sanitize()); err != nil {
				t.Errorf("owned test role cleanup refused: %s", created[i])
			}
		}
	}()
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
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
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
	bootstrap, err := os.ReadFile("../../deploy/production/token-database-bootstrap.sql")
	if err != nil {
		owner.Close(context.Background())
		t.Fatal(err)
	}
	t.Run("runtime-cannot-bootstrap", func(t *testing.T) {
		if _, err := owner.Exec(ctx, `SET ROLE token_gateway`); err != nil {
			t.Fatal(err)
		}
		_, err := owner.Exec(ctx, string(bootstrap))
		if err == nil || !strings.Contains(err.Error(), "independently controlled database operator") {
			t.Fatalf("runtime bootstrap must be refused: %v", err)
		}
		if _, err := owner.Exec(ctx, `ROLLBACK; RESET ROLE`); err != nil {
			t.Fatal(err)
		}
	})
	// Execute the actual shipped operator artifact before migration. No fixture
	// adds an extension or grants outside that production bootstrap entry point.
	if _, err = owner.Exec(ctx, string(bootstrap)); err != nil {
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
	if err = Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	verification, err := migrations.VerifyApplied(ctx, connection, loaded)
	if err != nil || verification.AppliedCount != 28 {
		t.Fatalf("public migration verification: %+v err=%v", verification, err)
	}
	var runtimeCanCreate bool
	if err = connection.QueryRow(ctx, `SELECT has_schema_privilege('token_gateway','public','CREATE')`).Scan(&runtimeCanCreate); err != nil || runtimeCanCreate {
		t.Fatalf("overview migration must not retain runtime schema CREATE: %v err=%v", runtimeCanCreate, err)
	}
	// Each Up acquires one session advisory lock; release the first installation
	// before testing a second independent invocation on this same connection.
	migrations.Unlock(ctx, connection)
	probeConfig := config.Copy()
	probeConfig.User = admin.Config().User
	probe, err := pgx.ConnectConfig(ctx, probeConfig)
	if err != nil {
		t.Fatal(err)
	}
	var unlocked bool
	err = probe.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&unlocked)
	if unlocked {
		migrations.Unlock(ctx, probe)
	}
	probe.Close(context.Background())
	if err != nil || !unlocked {
		t.Fatalf("one Up followed by one Unlock retained the migration lock: acquired=%v err=%v", unlocked, err)
	}
	if err = Up(ctx, connection, loaded); err != nil {
		t.Fatalf("exact installed migration replay: %v", err)
	}
	var activation string
	if err = connection.QueryRow(ctx, `SELECT state FROM public.token_command_governance_activation WHERE singleton`).Scan(&activation); err != nil || activation != "PRE_ACTIVATION" {
		t.Fatalf("migration must not fabricate runtime authority: %q err=%v", activation, err)
	}
	var future bool
	if err = connection.QueryRow(ctx, `SELECT to_regclass('client_readiness_attestations') IS NOT NULL OR to_regclass('organization_lifecycle_states') IS NOT NULL OR to_regclass('usage_history_groups') IS NOT NULL`).Scan(&future); err != nil || future {
		t.Fatalf("future private stages must remain absent: %v err=%v", future, err)
	}
	migrations.Unlock(ctx, connection)
	config.User = admin.Config().User
	operator, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close(context.Background())
	t.Run("installed-history-cannot-regrant-runtime-create", func(t *testing.T) {
		_, err := operator.Exec(ctx, string(bootstrap))
		if err == nil || !strings.Contains(err.Error(), "cannot reset an installed or partial migration history") {
			t.Fatalf("installed bootstrap must be refused: %v", err)
		}
		if _, err := operator.Exec(ctx, `ROLLBACK`); err != nil {
			t.Fatal(err)
		}
		var canCreate bool
		if err := operator.QueryRow(ctx, `SELECT has_schema_privilege('token_gateway','public','CREATE')`).Scan(&canCreate); err != nil || canCreate {
			t.Fatalf("failed bootstrap lent schema authority: %v err=%v", canCreate, err)
		}
	})
}
