package tokenschema

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestManagementPublicMigrationInstallation(t *testing.T) {
	t.Run("fresh", func(t *testing.T) { testManagementPublicMigrationInstallation(t, false, 0) })
	t.Run("legacy-upgrade", func(t *testing.T) { testManagementPublicMigrationInstallation(t, true, 0) })
	t.Run("installed-29-upgrade", func(t *testing.T) { testManagementPublicMigrationInstallation(t, false, 29) })
	t.Run("installed-30-upgrade", func(t *testing.T) { testManagementPublicMigrationInstallation(t, false, 30) })
}
func testManagementPublicMigrationInstallation(t *testing.T, legacy bool, previousStage int) {
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
	if previousStage > 0 {
		previous := []migrations.Migration{}
		for _, m := range loaded {
			if (m.Version < 900002 || (previousStage == 30 && m.Version == 900002)) && m.Version != 16 && m.Version != 17 && m.Version != 34 {
				previous = append(previous, m)
			}
		}
		if err = migrations.UpWithHooks(ctx, connection, previous, migrations.UpHooks{BeforeApply: prepareTokenMigration, AfterApply: finishTokenMigration, LedgerRole: "token_schema_owner"}); err != nil {
			t.Fatal("installed previous stage fixture", err)
		}
		migrations.Unlock(ctx, connection)
		verified, e := migrations.VerifyApplied(ctx, connection, previous)
		if e != nil || verified.AppliedCount != previousStage {
			t.Fatal("installed previous stage fixture verification", verified, e)
		}
	}
	if legacy {
		operatorConfig := config.Copy()
		operatorConfig.User = admin.Config().User
		op, e := pgx.ConnectConfig(ctx, operatorConfig)
		if e != nil {
			t.Fatal(e)
		}
		defer op.Close(context.Background())
		if _, e = op.Exec(ctx, `REVOKE token_gateway FROM token_schema_owner`); e != nil {
			t.Fatal(e)
		}
		old := []migrations.Migration{}
		for _, m := range loaded {
			if m.Version < 900001 && m.Version != 16 && m.Version != 17 && m.Version != 34 {
				old = append(old, m)
			}
		}
		if e = migrations.UpWithHooks(ctx, connection, old, migrations.UpHooks{BeforeApply: lendOverviewIndexDDL, AfterApply: revokeOverviewIndexDDL}); e != nil {
			t.Fatal("legacy installation", e)
		}
		migrations.Unlock(ctx, connection)
		if e = Up(ctx, connection, loaded); e == nil {
			t.Fatal("unprovisioned legacy upgrade accepted")
		}
		migrations.Unlock(ctx, connection)
		var count int
		var marker bool
		if e = connection.QueryRow(ctx, `SELECT (SELECT count(*) FROM schema_migrations),to_regclass('public.token_runtime_hardening') IS NOT NULL`).Scan(&count, &marker); e != nil || count != 28 || marker {
			t.Fatal("failed upgrade changed history", count, marker, e)
		}
		artifact, e := os.ReadFile("../../deploy/production/token-storage-hardening-bootstrap.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = connection.Exec(ctx, string(artifact)); e == nil {
			t.Fatal("migrator accepted independent operator bootstrap")
		}
		if _, e = connection.Exec(ctx, `ROLLBACK;SET ROLE token_gateway`); e != nil {
			t.Fatal(e)
		}
		if _, e = op.Exec(ctx, string(artifact)); e != nil {
			t.Fatal("legacy operator provisioning", e)
		}
	}
	if err = Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	verification, err := migrations.VerifyApplied(ctx, connection, loaded)
	if err != nil || verification.AppliedCount != len(installed) {
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
	if err = connection.QueryRow(ctx, `SELECT to_regclass('organization_lifecycle_states') IS NOT NULL`).Scan(&future); err != nil || future {
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
	t.Run("runtime-storage-and-ledger-authority", func(t *testing.T) {
		defer connection.Exec(context.Background(), `SET ROLE token_gateway`)
		runtimeConfig := config.Copy()
		runtimeConfig.User = "token_gateway"
		runtimeConfig.Password = ""
		runtime, err := pgx.ConnectConfig(ctx, runtimeConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close(context.Background())
		assertTokenRuntimeStorage(t, ctx, runtime)
		verified, e := migrations.VerifyApplied(ctx, runtime, loaded)
		if e != nil || verified.AppliedCount != len(installed) {
			t.Fatal("independent runtime migration verification", verified, e)
		}
		var unguarded int
		if err = operator.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p') AND pg_get_userbyid(c.relowner) IN ('token_gateway','token_schema_owner','token_cap_policy_deployer') AND NOT EXISTS(SELECT 1 FROM pg_trigger trigger WHERE trigger.tgrelid=c.oid AND NOT trigger.tgisinternal AND (trigger.tgtype & 32)=32 AND (trigger.tgtype & 1)=0)`).Scan(&unguarded); err != nil || unguarded != 0 {
			t.Fatal("missing statement guard", unguarded, err)
		}
		if _, err = runtime.Exec(ctx, `INSERT INTO token_audit_logs(id,actor_user_id,action,resource_type,resource_id,summary,occurred_at) VALUES($1,$2,'CONTROLLED_REVIEW','FIXTURE',$3,'{}',clock_timestamp())`, uuid.New(), uuid.New(), uuid.New()); err != nil {
			t.Fatal("runtime audit append broken", err)
		}
		for _, statement := range []string{`TRUNCATE token_audit_logs`, `UPDATE token_audit_logs SET action='REWRITTEN'`, `DELETE FROM token_audit_logs`, `ALTER TABLE token_audit_logs DISABLE TRIGGER ALL`, `DROP TRIGGER token_audit_logs_immutable ON token_audit_logs`, `CREATE TABLE public.unapproved_runtime_ddl(id integer)`, `INSERT INTO schema_migrations(version,name,checksum) VALUES(999,'forged','forged')`, `SET ROLE token_schema_owner`, `SET ROLE token_cap_policy_deployer`, `SELECT public.harden_token_runtime_objects()`} {
			if _, err = runtime.Exec(ctx, statement); err == nil {
				t.Fatal("runtime authority leaked", statement)
			}
		}
		// Owner still cannot accidentally truncate even an empty immutable table;
		// this proves the statement trigger rather than only runtime's narrow ACL.
		for _, entry := range []struct{ role, table string }{{"token_schema_owner", "usage_convergence_evidence"}, {"token_cap_policy_deployer", "token_account_owner_cap_policy_revisions"}} {
			if _, err = connection.Exec(ctx, "SET ROLE "+pgx.Identifier{entry.role}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			_, err = connection.Exec(ctx, "TRUNCATE "+pgx.Identifier{"public", entry.table}.Sanitize()+" CASCADE")
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "55000" {
				t.Fatal("statement protection not enforced", entry, err)
			}
		}
		if _, err = connection.Exec(ctx, `SET ROLE token_gateway`); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("history-projection-authority", func(t *testing.T) {
		assertTokenHistoryAuthority(t, ctx, connection)
		tx, e := connection.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(context.Background())
		if _, e = tx.Exec(ctx, `SET ROLE token_schema_owner; SELECT public.harden_token_runtime_objects(); SET ROLE token_gateway`); e != nil {
			t.Fatal("approved definers rejected by forward hardener", e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		assertTokenHistoryAuthority(t, ctx, connection)
	})
	t.Run("reject-unreviewed-history-definers", func(t *testing.T) {
		for _, mutation := range []string{
			`CREATE FUNCTION public.unreviewed_projection() RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN RETURN; END $$`,
			`ALTER FUNCTION public.project_usage_history_request(uuid,uuid,uuid,jsonb,timestamptz) SET search_path=public`,
			`CREATE OR REPLACE FUNCTION public.project_usage_history_request(projected_id uuid,projected_user_id uuid,projected_organization_id uuid,projected_request_snapshot jsonb,projected_started_at timestamptz) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN RETURN; END $$`,
		} {
			tx, e := connection.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			func() {
				defer tx.Rollback(context.Background())
				if _, e = tx.Exec(ctx, `SET ROLE token_schema_owner; `+mutation+`; SET ROLE token_gateway`); e != nil {
					t.Fatal(e)
				}
				if strings.Contains(mutation, "unreviewed_projection") {
					_, e = tx.Exec(ctx, `SET ROLE token_schema_owner; SELECT public.harden_token_runtime_objects()`)
				} else {
					e = prepareTokenMigration(ctx, tx, migrations.Migration{Version: 35})
				}
				if e == nil {
					t.Fatal("unreviewed definer accepted", mutation)
				}
			}()
		}
		assertTokenHistoryAuthority(t, ctx, connection)
	})
	t.Run("subsequent-low-version-migration-rehardens-storage", func(t *testing.T) {
		// The generic migration engine is exercised with a synthetic next-stage DDL;
		// Ordered still rejects this unsupported version in the production F04 set.
		next := migrations.Migration{Version: 35, Name: "controlled_future_storage", UpSQL: `CREATE TABLE public.controlled_future_storage(id UUID PRIMARY KEY); CREATE INDEX controlled_future_usage_idx ON usage_requests(id); SET ROLE token_schema_owner; GRANT CREATE ON SCHEMA public TO token_cap_policy_deployer; SET ROLE token_cap_policy_deployer; CREATE TABLE public.controlled_future_policy(id UUID PRIMARY KEY); SET ROLE token_schema_owner; REVOKE CREATE ON SCHEMA public FROM token_cap_policy_deployer; SET ROLE token_gateway;`}
		if err := migrations.UpWithHooks(ctx, connection, []migrations.Migration{next}, migrations.UpHooks{BeforeApply: prepareTokenMigration, AfterApply: finishTokenMigration, LedgerRole: "token_schema_owner"}); err != nil {
			t.Fatal(err)
		}
		var owner, ledgerOwner string
		if err := connection.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.controlled_future_storage'::regclass`).Scan(&owner); err != nil || owner != "token_schema_owner" {
			t.Fatal("future owner not recovered", owner, err)
		}
		if err := connection.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.schema_migrations'::regclass`).Scan(&ledgerOwner); err != nil || ledgerOwner != "token_schema_owner" {
			t.Fatal("ledger ownership regressed", ledgerOwner, err)
		}
		var ledgerRow bool
		if err := connection.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=35 AND name='controlled_future_storage')`).Scan(&ledgerRow); err != nil || !ledgerRow {
			t.Fatal("owner ledger pin failed", ledgerRow, err)
		}
		var policyGuard bool
		if err := connection.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='public.controlled_future_policy'::regclass AND tgname='token_no_truncate' AND (tgtype & 32)=32 AND (tgtype & 1)=0)`).Scan(&policyGuard); err != nil || !policyGuard {
			t.Fatal("future policy missing guard", policyGuard, err)
		}
		runtimeConfig := config.Copy()
		runtimeConfig.User = "token_gateway"
		runtimeConfig.Password = ""
		runtime, err := pgx.ConnectConfig(ctx, runtimeConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close(context.Background())
		assertTokenRuntimeStorage(t, ctx, runtime)
		assertTokenHistoryAuthority(t, ctx, runtime)
		for _, statement := range []string{`UPDATE token_audit_logs SET action='REWRITTEN'`, `DELETE FROM token_audit_logs`, `TRUNCATE controlled_future_storage`, `TRUNCATE controlled_future_policy`, `CREATE TABLE public.future_leaked_ddl(id integer)`, `INSERT INTO schema_migrations(version,name,checksum) VALUES(998,'forged','forged')`} {
			if _, err = runtime.Exec(ctx, statement); err == nil {
				t.Fatal("future hardening authority leaked", statement)
			}
		}
	})

}

func assertTokenRuntimeStorage(t *testing.T, ctx context.Context, c *pgx.Conn) {
	t.Helper()
	var current string
	var super, unsafeMembership, create bool
	var owned int
	if err := c.QueryRow(ctx, `SELECT current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user),pg_has_role(current_user,'token_schema_owner','MEMBER') OR pg_has_role(current_user,'token_cap_policy_deployer','MEMBER'),has_schema_privilege(current_user,'public','CREATE'),(SELECT count(*) FROM pg_class object JOIN pg_namespace n ON n.oid=object.relnamespace WHERE n.nspname='public' AND pg_get_userbyid(object.relowner)=current_user)`).Scan(&current, &super, &unsafeMembership, &create, &owned); err != nil || current != "token_gateway" || super || unsafeMembership || create || owned != 0 {
		t.Fatal("runtime is not independently restricted", current, super, unsafeMembership, create, owned, err)
	}
}

func assertTokenHistoryAuthority(t *testing.T, ctx context.Context, c *pgx.Conn) {
	t.Helper()
	for _, table := range []string{"usage_history_projection_state", "usage_history_members", "usage_history_groups"} {
		var read, write bool
		if e := c.QueryRow(ctx, `SELECT has_table_privilege('token_gateway',$1,'SELECT'),has_table_privilege('token_gateway',$1,'INSERT,UPDATE,DELETE')`, "public."+table).Scan(&read, &write); e != nil || !read || write {
			t.Fatal("history authority", table, read, write, e)
		}
	}
	for _, expected := range historyDefiners {
		var definer, execute bool
		var owner string
		if e := c.QueryRow(ctx, `SELECT p.prosecdef,pg_get_userbyid(p.proowner),has_function_privilege('token_gateway',p.oid,'EXECUTE') FROM pg_proc p WHERE p.oid=to_regprocedure($1)`, expected.signature).Scan(&definer, &owner, &execute); e != nil || !definer || owner != "token_schema_owner" || execute != strings.Contains(expected.signature, "backfill_") {
			t.Fatal("history definer authority", expected.signature, definer, owner, execute, e)
		}
	}
	var processed int
	var complete bool
	if e := c.QueryRow(ctx, `SELECT processed,complete FROM public.backfill_usage_history_projection(100)`).Scan(&processed, &complete); e != nil || processed != 0 || !complete {
		t.Fatal("bounded history worker authority", processed, complete, e)
	}
}
