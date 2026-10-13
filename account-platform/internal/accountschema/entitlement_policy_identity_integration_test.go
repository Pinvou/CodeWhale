package accountschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The outer owned-instance coordinator provisions the original isolated actors.
// Every database below is fresh and UUID-owned; no ledger rows or SQL identities
// are rewritten to manufacture an upgrade state.
func TestF09EntitlementPolicyMigrationIdentityPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual owned PostgreSQL migration fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("parse owned policy migration fixture")
	}
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var original migrations.Migration
	baseline := make([]migrations.Migration, 0, len(loaded))
	for _, m := range loaded {
		if m.Version == 69 {
			original = m
			continue
		}
		if m.Version == 70 {
			continue
		}
		baseline = append(baseline, m)
	}
	if original.Name != "entitlement_component_adjustment_policy_v2" || original.Checksum != "5dfcd5e1f9e3f41206e594653cc6b55c0e1c3c015033e8d59a294f966710dd6e" {
		t.Fatal("original policy migration source missing")
	}
	// Original 070 alone declares the 069 dependency; later retained originals
	// stay in their genuine order and have not been fabricated in the ledger.
	if _, err := Ordered(baseline); err != nil {
		t.Fatal(err)
	}
	makeDatabase := func() string {
		t.Helper()
		admin, err := pgx.ConnectConfig(ctx, config.Copy())
		if err != nil {
			t.Fatal("connect owned fixture control")
		}
		defer admin.Close(context.Background())
		name := "f09_policy_identity_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER account_service"); err != nil {
			t.Fatal("create UUID-owned policy database")
		}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			control, err := pgx.ConnectConfig(cleanup, config.Copy())
			if err != nil {
				t.Error("connect policy cleanup control")
				return
			}
			defer control.Close(cleanup)
			var owner string
			var active int
			deadline := time.Now().Add(5 * time.Second)
			for {
				err = control.QueryRow(cleanup, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&owner, &active)
				if err != nil || owner != "account_service" || active == 0 || !time.Now().Before(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err != nil || owner != "account_service" || active != 0 {
				t.Error("UUID database identity/session cleanup guard failed")
				return
			}
			if _, err := control.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
				t.Error("normal owned policy database cleanup failed")
			}
		})
		setupConfig := config.Copy()
		setupConfig.Database = name
		setup, err := pgx.ConnectConfig(ctx, setupConfig)
		if err != nil {
			t.Fatal("connect owned schema setup")
		}
		defer setup.Close(context.Background())
		if _, err := setup.Exec(ctx, `SET ROLE account_service; ALTER SCHEMA public OWNER TO account_service; REVOKE CREATE ON SCHEMA public FROM PUBLIC; RESET ROLE`); err != nil {
			t.Fatal("set original runtime schema ownership")
		}
		// Original 050 consumes and revokes this migration-only CREATE privilege.
		if _, err := setup.Exec(ctx, "GRANT CREATE ON DATABASE "+pgx.Identifier{name}.Sanitize()+" TO account_migrator"); err != nil {
			t.Fatal("grant original temporary migration schema privilege")
		}
		migrationConfig := config.Copy()
		migrationConfig.Database = name
		migrationConfig.User = "account_migrator"
		migrationConfig.Password = ""
		connection, err := pgx.ConnectConfig(ctx, migrationConfig)
		if err != nil {
			t.Fatal("connect original migration actor")
		}
		defer connection.Close(context.Background())
		defer migrations.Unlock(context.Background(), connection)
		if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
			t.Fatal("select original migration role")
		}
		if err := Up(ctx, connection, baseline); err != nil {
			t.Fatalf("genuine baseline public Up: %v", err)
		}
		if _, err := migrations.VerifyApplied(ctx, connection, baseline); err != nil {
			t.Fatal(err)
		}
		return name
	}
	connect := func(database, user, role string) *pgx.Conn {
		t.Helper()
		c := config.Copy()
		c.Database = database
		if user != "" {
			c.User = user
			c.Password = ""
		}
		connection, err := pgx.ConnectConfig(ctx, c)
		if err != nil {
			t.Fatal("connect finite policy actor")
		}
		if role != "" {
			if _, err := connection.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				connection.Close(ctx)
				t.Fatal("select finite policy actor role")
			}
		}
		return connection
	}
	facts := func(database string) string {
		t.Helper()
		c := connect(database, "", "")
		defer c.Close(context.Background())
		var value string
		if err := c.QueryRow(ctx, `SELECT jsonb_build_object('policies',(SELECT jsonb_agg(to_jsonb(p) ORDER BY command_type,policy_version) FROM approval_command_policies p),'ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m))::text`).Scan(&value); err != nil {
			t.Fatal("read complete policy/history facts")
		}
		return value
	}
	database := makeDatabase()
	for _, tc := range []struct {
		name, user, role   string
		rename, exactGuard bool
	}{
		{"runtime-cannot-install-policy", "account_service", "", false, true},
		{"renamed-original-runtime-cannot-install-policy", "account_service", "", true, true},
		{"wrong-current-role-fails-closed", "account_migrator", "", false, false},
		{"superuser-session-is-not-contract", "", "account_service", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := facts(database)
			candidate := append([]migrations.Migration(nil), loaded...)
			if tc.rename {
				for i := range candidate {
					if candidate[i].Version == 69 {
						candidate[i].Name = "renamed_original_entitlement_policy"
					}
				}
			}
			c := connect(database, tc.user, tc.role)
			defer c.Close(context.Background())
			defer migrations.Unlock(context.Background(), c)
			var session, current string
			if err := c.QueryRow(ctx, `SELECT session_user,current_user`).Scan(&session, &current); err != nil {
				t.Fatal("read actual actor tuple")
			}
			if tc.user == "" {
				var superuser bool
				if err := c.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname=session_user`).Scan(&superuser); err != nil || !superuser {
					t.Fatal("superuser negative requires the actual controlled fixture identity")
				}
			}
			if session == "account_migrator" && current == "account_service" {
				t.Fatal("negative fixture accidentally used approved tuple")
			}
			err := Up(ctx, c, candidate)
			if err == nil {
				t.Fatal("unqualified actor installed immutable policy")
			}
			if tc.exactGuard && (!strings.Contains(err.Error(), "apply migration 69:") || !strings.Contains(err.Error(), "requires the isolated account migration identities")) {
				t.Fatalf("did not reach exact pending 069 guard: %v", err)
			}
			// The unselected NOINHERIT migrator may be rejected by the original ledger
			// read privileges before BeforeApply; do not claim that case reached the hook.
			if facts(database) != before {
				t.Fatal("refused upgrade changed original policy or migration history")
			}
		})
	}
	t.Run("qualified-original-public-up", func(t *testing.T) {
		c := connect(database, "account_migrator", "account_service")
		defer c.Close(context.Background())
		defer migrations.Unlock(context.Background(), c)
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		if _, err := migrations.VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM approval_command_policies WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2`).Scan(&count); err != nil || count != 1 {
			t.Fatal("original qualified policy did not install")
		}
	})
	t.Run("unrelated-custom69-runtime-remains-generic", func(t *testing.T) {
		customDB := makeDatabase()
		custom := migrations.Migration{Version: 69, Name: "custom_policy_fixture", UpSQL: `CREATE TABLE f09_custom69_identity_control(id INTEGER PRIMARY KEY);`, DownSQL: `DROP TABLE f09_custom69_identity_control;`}
		sum := sha256.Sum256([]byte(custom.UpSQL))
		custom.Checksum = hex.EncodeToString(sum[:])
		candidate := append(append([]migrations.Migration(nil), baseline...), custom)
		c := connect(customDB, "account_service", "")
		defer c.Close(context.Background())
		defer migrations.Unlock(context.Background(), c)
		if err := Up(ctx, c, candidate); err != nil {
			t.Fatal(err)
		}
		if _, err := migrations.VerifyApplied(ctx, c, candidate); err != nil {
			t.Fatal(err)
		}
		var exists bool
		if err := c.QueryRow(ctx, `SELECT to_regclass('f09_custom69_identity_control') IS NOT NULL AND NOT EXISTS(SELECT 1 FROM approval_command_policies WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2)`).Scan(&exists); err != nil || !exists {
			t.Fatal("custom69 was misclassified or installed original policy")
		}
	})
}
