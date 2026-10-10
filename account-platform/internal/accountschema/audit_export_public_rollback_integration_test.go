package accountschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestAuditExportPublicDownRetainsOriginalHistory(t *testing.T) {
	raw := os.Getenv("F17_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("isolated PostgreSQL fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newAuditExportGuardFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal("LOAD_ORIGINAL_SQL")
	}
	// Reproduce the retained historical boundary using the original 1..32
	// package, not a fabricated audit schema or a destructive ledger rewrite.
	var historical []migrations.Migration
	for _, m := range loaded {
		if m.Version <= 32 {
			historical = append(historical, m)
		}
	}
	if err := migrations.Up(ctx, c, historical); err != nil {
		t.Fatal("ORIGINAL_HISTORY_UP", err)
	}
	migrations.Unlock(ctx, c)
	if _, err := migrations.VerifyApplied(ctx, c, historical); err != nil {
		t.Fatal("ORIGINAL_HISTORY_VERIFY", err)
	}
	snapshot := func() string {
		t.Helper()
		var result string
		if err := c.QueryRow(ctx, `SELECT jsonb_build_object(
'ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),
'roles',(SELECT jsonb_agg(to_jsonb(r) ORDER BY code) FROM roles r),
'permissions',(SELECT jsonb_agg(to_jsonb(r) ORDER BY role_code,permission_code) FROM role_permissions r),
'policies',(SELECT jsonb_agg(to_jsonb(p) ORDER BY command_type,policy_version) FROM approval_command_policies p),
'assignments',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM role_assignments r),
'jobs',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM audit_export_jobs j),
'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM audit_export_events e))::text`).Scan(&result); err != nil {
			t.Fatal("HISTORY_SNAPSHOT", err)
		}
		return result
	}
	t.Run("unbound_public_down_forbids_incomplete_inverse", func(t *testing.T) {
		before := snapshot()
		err := DownOne(ctx, c, loaded)
		migrations.Unlock(ctx, c)
		if err == nil || !strings.Contains(err.Error(), "rollback is incomplete and forbidden") {
			t.Fatal("UNSAFE_INVERSE_ACCEPTED", err)
		}
		if after := snapshot(); after != before {
			t.Fatal("REJECTED_DOWN_CHANGED_HISTORY")
		}
		var pii int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM role_permissions WHERE role_code='PLATFORM_SUPER_ADMIN' AND permission_code='platform.audit.pii.export'`).Scan(&pii); err != nil || pii != 0 {
			t.Fatal("REJECTED_DOWN_GRANTED_PII")
		}
	})
	id := uuid.New()
	code, err := account.GenerateUserCode()
	if err != nil {
		t.Fatal("FIXTURE_USER_CODE")
	}
	if _, err := c.Exec(ctx, `INSERT INTO users(id,user_code,status,created_at,updated_at) VALUES($1,$2,'ACTIVE',clock_timestamp(),clock_timestamp());`, id, code); err != nil {
		t.Fatal("FIXTURE_USER", err)
	}
	if _, err := c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,created_at,starts_at,expires_at) VALUES($1,$2,'PLATFORM_AUDIT_PII_EXPORT','ACCOUNT',clock_timestamp(),clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour')`, uuid.New(), id); err != nil {
		t.Fatal("FIXTURE_EXPIRED_BINDING", err)
	}
	t.Run("expired_binding_public_down_retains_evidence", func(t *testing.T) {
		before := snapshot()
		err := DownOne(ctx, c, loaded)
		migrations.Unlock(ctx, c)
		if err == nil || !strings.Contains(err.Error(), "role binding prevents rollback") {
			t.Fatal("BOUND_DOWN_ACCEPTED", err)
		}
		if after := snapshot(); after != before {
			t.Fatal("BOUND_DOWN_CHANGED_HISTORY")
		}
	})
}

// These synthetic migration facts test the generic transaction infrastructure;
// they do not stand in for an Account producer or the original 032 SQL.
func TestAuditExportGuardedRollbackTransactions(t *testing.T) {
	raw := os.Getenv("F17_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("isolated PostgreSQL fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newAuditExportGuardFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	first := migrations.Migration{Version: 1, Name: "guarded_rollback_probe", UpSQL: "CREATE TABLE rollback_probe(v integer)", DownSQL: "DROP TABLE rollback_probe"}
	second := migrations.Migration{Version: 2, Name: "probe_fact", UpSQL: "INSERT INTO rollback_probe VALUES(7)", DownSQL: "DELETE FROM rollback_probe WHERE v=7"}
	for _, m := range []*migrations.Migration{&first, &second} {
		sum := sha256.Sum256([]byte(m.UpSQL))
		m.Checksum = hex.EncodeToString(sum[:])
	}
	loaded := []migrations.Migration{first, second}
	if err := migrations.Up(ctx, c, loaded); err != nil {
		t.Fatal("PROBE_UP", err)
	}
	migrations.Unlock(ctx, c)
	unchanged := func() {
		t.Helper()
		var count int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM rollback_probe WHERE v=7`).Scan(&count); err != nil || count != 1 {
			t.Fatal("PROBE_FACT_CHANGED")
		}
		if err := c.QueryRow(ctx, `SELECT count(*) FROM rollback_probe WHERE v<>7`).Scan(&count); err != nil || count != 0 {
			t.Fatal("GUARD_EFFECT_COMMITTED")
		}
		if _, err := migrations.VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal("REJECTED_DOWN_CHANGED_LEDGER", err)
		}
	}
	t.Run("guard_rejection_rolls_back_guard_writes", func(t *testing.T) {
		calls := 0
		err := migrations.DownOneWithGuard(ctx, c, loaded, func(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
			calls++
			if m.Version != 2 {
				t.Error("WRONG_SELECTED_MIGRATION")
			}
			if _, err := tx.Exec(ctx, `INSERT INTO rollback_probe VALUES(99)`); err != nil {
				return err
			}
			return errors.New("fixture rejected rollback")
		})
		migrations.Unlock(ctx, c)
		if err == nil || calls != 1 {
			t.Fatal("GUARD_NOT_ENFORCED")
		}
		unchanged()
	})
	t.Run("sql_failure_rolls_back_guard_and_ledger", func(t *testing.T) {
		changed := append([]migrations.Migration(nil), loaded...)
		changed[1].DownSQL += "; SELECT 1/0"
		calls := 0
		err := migrations.DownOneWithGuard(ctx, c, changed, func(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
			calls++
			_, err := tx.Exec(ctx, `INSERT INTO rollback_probe VALUES(99)`)
			return err
		})
		migrations.Unlock(ctx, c)
		if err == nil || calls != 1 {
			t.Fatal("FAILING_SQL_ACCEPTED")
		}
		unchanged()
	})
	t.Run("guard_role_cannot_choose_sql_identity", func(t *testing.T) {
		err := migrations.DownOneWithGuard(ctx, c, loaded, func(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
			_, err := tx.Exec(ctx, `SET ROLE account_approval_control_owner`)
			return err
		})
		migrations.Unlock(ctx, c)
		if err != nil {
			t.Fatal("ORIGINAL_SQL_IDENTITY_NOT_RESTORED", err)
		}
		if _, err := migrations.VerifyApplied(ctx, c, []migrations.Migration{first}); err != nil {
			t.Fatal("VALID_GUARDED_DOWN_LEDGER", err)
		}
		var actor string
		if err := c.QueryRow(ctx, `SELECT current_user`).Scan(&actor); err != nil || actor != "account_service" {
			t.Fatal("GUARD_ROLE_ESCAPED_TRANSACTION")
		}
	})
	t.Run("original_down_without_guard_stays_compatible", func(t *testing.T) {
		if err := migrations.DownOne(ctx, c, []migrations.Migration{first}); err != nil {
			t.Fatal("ORIGINAL_DOWN_REJECTED", err)
		}
		migrations.Unlock(ctx, c)
		var remains bool
		if err := c.QueryRow(ctx, `SELECT to_regclass('rollback_probe') IS NOT NULL OR EXISTS(SELECT 1 FROM schema_migrations)`).Scan(&remains); err != nil || remains {
			t.Fatal("ORIGINAL_DOWN_INCOMPLETE")
		}
	})
}

func TestAuditExportFixtureConfigurationRejectsDeployment(t *testing.T) {
	for _, item := range []struct {
		name, url, profile string
		ci, allowed        bool
	}{
		{"local_owned", "postgres://pinvou_wave_test@127.0.0.1:55504/postgres?sslmode=disable", "", false, true},
		{"ci_owned", "postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable", "github-actions-service", true, true},
		{"ci_flag_outside_actions", "postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable", "github-actions-service", false, false},
		{"remote", "postgres://postgres@192.0.2.1:5432/postgres?sslmode=disable", "github-actions-service", true, false},
		{"deployment_database", "postgres://postgres@127.0.0.1:5432/deployment?sslmode=disable", "github-actions-service", true, false},
		{"unknown_profile", "postgres://pinvou_wave_test@127.0.0.1:55504/postgres?sslmode=disable", "other", false, false},
		{"fallback", "postgres://pinvou_wave_test@127.0.0.1:55504/postgres?sslmode=prefer", "", false, false},
		{"wrong_local_actor", "postgres://postgres@127.0.0.1:55504/postgres?sslmode=disable", "", false, false},
	} {
		t.Run(item.name, func(t *testing.T) {
			config, err := pgx.ParseConfig(item.url)
			if err != nil {
				t.Fatal("SYNTHETIC_CONFIG_PARSE")
			}
			if got := auditExportFixtureConfigurationAllowed(config, item.profile, item.ci); got != item.allowed {
				t.Fatal("FIXTURE_ADMISSION_MISMATCH")
			}
		})
	}
}
