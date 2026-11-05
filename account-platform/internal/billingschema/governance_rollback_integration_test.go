package billingschema

import (
	"context"
	"errors"
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
	"github.com/jackc/pgx/v5/pgconn"
)

// Only genuine original numeric 001..039 SQL is applied to each new owned
// database. No high-numbered ledger rows are deleted to fake a rollback phase.
// Runtime/schema owner and non-super migration identities are unchanged.
func TestF10OrganizationGovernanceRollbackPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("exclusive owned PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	all, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var prefix []migrations.Migration
	for _, m := range all {
		if m.Version <= 39 {
			prefix = append(prefix, m)
		}
	}
	if len(prefix) == 0 || prefix[len(prefix)-1].Version != 39 {
		t.Fatal("original039 prefix absent")
	}
	legacy := func() *pgx.Conn {
		t.Helper()
		c := f.database(t, ctx, "account", "account_service")
		if err := migrations.Up(ctx, c, prefix); err != nil {
			t.Fatal("original numeric001..039 setup", err)
		}
		migrations.Unlock(ctx, c)
		if _, err := migrations.VerifyApplied(ctx, c, prefix); err != nil {
			t.Fatal(err)
		}
		if stageValue(t, ctx, c, `SELECT max(version)::text FROM schema_migrations`) != "39" {
			t.Fatal("legacy latest is not real039")
		}
		return c
	}
	snapshot := func(c *pgx.Conn) string {
		return stageValue(t, ctx, c, `SELECT jsonb_build_object('ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),'policies',(SELECT jsonb_agg(to_jsonb(p) ORDER BY command_type,policy_version) FROM approval_command_policies p),'permissions',(SELECT jsonb_agg(to_jsonb(p) ORDER BY code) FROM permissions p),'edges',(SELECT jsonb_agg(to_jsonb(p) ORDER BY role_code,permission_code) FROM role_permissions p),'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY user_id,scope,scope_id) FROM authorization_versions v),'assignments',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM role_assignments a),'requests',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM approval_requests a),'audit',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM audit_logs a),'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id) FROM outbox_events o))::text`)
	}
	identity := func(c *pgx.Conn) string { return stageValue(t, ctx, c, `SELECT session_user||':'||current_user`) }
	trigger := func(c *pgx.Conn) string {
		return stageValue(t, ctx, c, `SELECT jsonb_build_object('definition',pg_get_triggerdef(oid),'enabled',tgenabled)::text FROM pg_trigger WHERE tgrelid='approval_command_policies'::regclass AND tgname='approval_command_policies_immutable'`)
	}
	assertOriginalFailure := func(c *pgx.Conn) {
		t.Helper()
		before, shape, actor := snapshot(c), stageShape(t, ctx, c), identity(c)
		var pgErr *pgconn.PgError
		err := migrations.DownOne(ctx, c, all)
		migrations.Unlock(ctx, c)
		if !errors.As(err, &pgErr) || pgErr.Code != "42883" || !strings.Contains(pgErr.Message, "void") {
			t.Fatal("original void DISTINCT failure not reproduced", err)
		}
		if snapshot(c) != before || stageShape(t, ctx, c) != shape || identity(c) != actor {
			t.Fatal("original failed rollback changed retained facts")
		}
	}
	assertDown := func(c *pgx.Conn, apply func() error) {
		t.Helper()
		beforeActor, beforeTrigger := identity(c), trigger(c)
		remaining := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<>39`)
		remainingPolicies := stageValue(t, ctx, c, `SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY command_type,policy_version),'[]'::jsonb)::text FROM approval_command_policies p WHERE NOT(policy_version=2 AND command_type IN('ACCOUNT_ORGANIZATION_RENAME','ACCOUNT_MEMBER_ADD','ACCOUNT_MEMBER_ROLES_REPLACE','ACCOUNT_MEMBER_REMOVE','ACCOUNT_UNIT_CREATE','ACCOUNT_UNIT_RENAME','ACCOUNT_UNIT_DELETE'))`)
		if err := apply(); err != nil {
			t.Fatal("official Account039 rollback", err)
		}
		migrations.Unlock(ctx, c)
		if _, err := migrations.VerifyApplied(ctx, c, prefix[:len(prefix)-1]); err != nil {
			t.Fatal(err)
		}
		if stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`) != remaining || identity(c) != beforeActor || trigger(c) != beforeTrigger {
			t.Fatal("rollback changed other ledger, role identity or policy trigger")
		}
		if stageValue(t, ctx, c, `SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY command_type,policy_version),'[]'::jsonb)::text FROM approval_command_policies p`) != remainingPolicies {
			t.Fatal("unrelated approval history changed")
		}
		if stageValue(t, ctx, c, `SELECT ((SELECT count(*) FROM permissions WHERE code IN('platform.organizations.profile.manage','platform.organizations.members.manage','platform.organizations.units.manage'))+(SELECT count(*) FROM role_permissions WHERE permission_code IN('platform.organizations.profile.manage','platform.organizations.members.manage','platform.organizations.units.manage')))::text`) != "0" {
			t.Fatal("governance permissions not withdrawn")
		}
		if _, err := c.Exec(ctx, `DELETE FROM approval_command_policies WHERE command_type='OWNED_NO_MATCH'`); err == nil {
			t.Fatal("immutable policy trigger not restored")
		}
	}
	seed := func(c *pgx.Conn) (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) {
		t.Helper()
		user, revoked, other, binding := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		for _, id := range []uuid.UUID{user, revoked, other} {
			if _, err := c.Exec(ctx, `INSERT INTO users(id,status,display_name,user_code,created_at,updated_at) VALUES($1,'ACTIVE','Controlled rollback actor',generate_unique_user_code(),now(),now())`, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.Exec(ctx, `INSERT INTO roles(code,scope,assignable,built_in,description) VALUES('OWNED_GOVERNANCE_ADMIN','PLATFORM',true,false,'controlled rollback role'),('OWNED_UNRELATED_ADMIN','PLATFORM',true,false,'controlled unaffected role'); INSERT INTO role_permissions(role_code,permission_code) SELECT 'OWNED_GOVERNANCE_ADMIN',code FROM permissions WHERE code IN('platform.organizations.profile.manage','platform.organizations.members.manage','platform.organizations.units.manage'); INSERT INTO role_permissions(role_code,permission_code) VALUES('OWNED_UNRELATED_ADMIN','platform.organizations.read')`); err != nil {
			t.Fatal(err)
		}
		revokedBinding := uuid.New()
		if _, err := c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,'OWNED_GOVERNANCE_ADMIN','ACCOUNT',now()-interval '1 day',now()-interval '1 day'),($3,$4,'OWNED_GOVERNANCE_ADMIN','ACCOUNT',now()-interval '1 day',now()-interval '1 day'),($5,$6,'OWNED_UNRELATED_ADMIN','ACCOUNT',now()-interval '1 day',now()-interval '1 day')`, binding, user, revokedBinding, revoked, uuid.New(), other); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `UPDATE role_assignments SET revoked_at=now() WHERE id=$1`, revokedBinding); err != nil {
			t.Fatal(err)
		}
		return user, revoked, other, binding
	}
	version := func(c *pgx.Conn, user uuid.UUID) int64 {
		t.Helper()
		var n int64
		if err := c.QueryRow(ctx, `SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'`, user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t.Run("empty-original-fails-compatible-down-succeeds", func(t *testing.T) {
		c := legacy()
		assertOriginalFailure(c)
		assertDown(c, func() error { return accountschema.DownOne(ctx, c, all) })
	})
	t.Run("nonempty-distinct-assignment-inputs-and-revocation", func(t *testing.T) {
		c := legacy()
		user, revoked, other, _ := seed(c)
		a, b, d := version(c, user), version(c, revoked), version(c, other)
		assignments := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM role_assignments a`)
		assertOriginalFailure(c)
		assertDown(c, func() error { return accountschema.DownOne(ctx, c, all) })
		if version(c, user) != a+1 || version(c, revoked) != b || version(c, other) != d {
			t.Fatal("permission join duplicated invalidation or touched revoked/unrelated assignment")
		}
		if stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM role_assignments a`) != assignments {
			t.Fatal("permission withdrawal altered role assignments")
		}
	})
	t.Run("later-permission-failure-rolls-back-complete-down", func(t *testing.T) {
		c := legacy()
		seed(c)
		if _, err := c.Exec(ctx, `CREATE FUNCTION public.owned_governance_rollback_failure() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'controlled governance withdrawal failure' USING ERRCODE='23514'; END$$; CREATE TRIGGER owned_governance_rollback_failure BEFORE DELETE ON permissions FOR EACH STATEMENT EXECUTE FUNCTION public.owned_governance_rollback_failure()`); err != nil {
			t.Fatal(err)
		}
		before, shape, actor := snapshot(c), stageShape(t, ctx, c), identity(c)
		var pgErr *pgconn.PgError
		err := accountschema.DownOne(ctx, c, all)
		migrations.Unlock(ctx, c)
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || !strings.Contains(pgErr.Message, "controlled governance") {
			t.Fatal("controlled later statement failure did not execute", err)
		}
		if snapshot(c) != before || stageShape(t, ctx, c) != shape || identity(c) != actor {
			t.Fatal("failed compatible rollback committed invalidation, privileges, policy or ledger")
		}
	})
	for _, field := range []string{"name", "checksum"} {
		t.Run("unknown-applied-"+field+"-refused-before-effects", func(t *testing.T) {
			c := legacy()
			seed(c)
			// Deliberately corrupt only this real039 row to test provenance refusal;
			// no version is removed, inserted, or lowered to fake a legacy phase.
			value := "OWNED_UNRECOGNIZED"
			if field == "checksum" {
				value = strings.Repeat("0", 64)
			}
			if _, err := c.Exec(ctx, `UPDATE schema_migrations SET `+pgx.Identifier{field}.Sanitize()+`=$1 WHERE version=39`, value); err != nil {
				t.Fatal(err)
			}
			before, shape := snapshot(c), stageShape(t, ctx, c)
			err := accountschema.DownOne(ctx, c, all)
			migrations.Unlock(ctx, c)
			if err == nil || !strings.Contains(err.Error(), "applied organization governance migration identity") {
				t.Fatal("unknown applied identity accepted", err)
			}
			if snapshot(c) != before || stageShape(t, ctx, c) != shape {
				t.Fatal("unknown identity caused a rollback effect")
			}
		})
	}
	t.Run("referenced-v2-approval-history-is-not-deleted", func(t *testing.T) {
		c := legacy()
		user, _, _, binding := seed(c)
		if _, err := c.Exec(ctx, `INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,expires_at,created_at,updated_at) VALUES($1,'ACCOUNT_ORGANIZATION_RENAME',2,$2,$3,$4,'owned-retained-v2-approval','{}',decode(repeat('00',32),'hex'),'ORGANIZATION',$5,'controlled retained approval history','[]','{}','DRAFT',now()+interval '1 hour',now(),now())`, uuid.New(), user, binding, version(c, user), uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		before, shape := snapshot(c), stageShape(t, ctx, c)
		var pgErr *pgconn.PgError
		err := accountschema.DownOne(ctx, c, all)
		migrations.Unlock(ctx, c)
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatal("referenced policy must retain original FK refusal", err)
		}
		if snapshot(c) != before || stageShape(t, ctx, c) != shape {
			t.Fatal("referenced policy rollback changed retained evidence")
		}
	})
	t.Run("official-cli-applied-original039", func(t *testing.T) {
		binary := os.Getenv("PINVOU_TEST_ACCOUNT_MIGRATE_BINARY")
		if binary == "" {
			t.Skip("actual current migration CLI binary required; no CLI validation claimed")
		}
		source, err := filepath.Abs("../../migrations/account")
		if err != nil {
			t.Fatal(err)
		}
		c := legacy()
		user, revoked, other, _ := seed(c)
		a, b, d := version(c, user), version(c, revoked), version(c, other)
		assertDown(c, func() error {
			child := exec.CommandContext(ctx, binary, "--direction", "down")
			for _, value := range os.Environ() {
				key, _, _ := strings.Cut(value, "=")
				switch key {
				case "DATABASE_URL", "MIGRATIONS_DIR", "MIGRATION_ROLE", "PINVOU_DEPLOYMENT_ENVIRONMENT":
					continue
				}
				child.Env = append(child.Env, value)
			}
			child.Env = append(child.Env, "DATABASE_URL="+f.serviceURL(t, c, "account_migrator"), "MIGRATIONS_DIR="+source, "MIGRATION_ROLE=account_service", "PINVOU_DEPLOYMENT_ENVIRONMENT=test")
			if output, err := child.CombinedOutput(); err != nil {
				t.Log("actual CLI failure", string(output))
				return err
			}
			return nil
		})
		if version(c, user) != a+1 || version(c, revoked) != b || version(c, other) != d {
			t.Fatal("actual CLI invalidation differs from official SDK path")
		}
	})

}
