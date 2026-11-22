package tokenschema

import (
	"context"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// The original SQL and both production hooks run on the retained hardened
// public schema. A controlled post-DDL hook failure proves migration-TX rollback;
// it is not a provider failure, lifecycle business effect or live deployment.
func testRetainedLifecycleMigrationRollback(t *testing.T, ctx context.Context, connection *pgx.Conn, loaded []migrations.Migration) {
	t.Helper()
	snapshot := func() string {
		t.Helper()
		var value string
		err := connection.QueryRow(ctx, `SELECT jsonb_build_object(
   'role',current_user,
   'runtime_create',has_schema_privilege('token_gateway','public','CREATE'),
   'runtime_owner_member',pg_has_role('token_gateway','token_schema_owner','MEMBER'),
   'schema_acl',(SELECT nspacl::text FROM pg_namespace WHERE nspname='public'),
   'ledger',(SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY version),'[]'::jsonb) FROM schema_migrations m),
   'relations',(SELECT coalesce(jsonb_agg(jsonb_build_array(c.relname,pg_get_userbyid(c.relowner),c.relacl::text) ORDER BY c.relname),'[]'::jsonb) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','S','v','m')),
   'functions',(SELECT coalesce(jsonb_agg(jsonb_build_array(p.oid::regprocedure::text,pg_get_userbyid(p.proowner),p.proacl::text,p.prosecdef,p.prosrc,p.proconfig) ORDER BY p.oid),'[]'::jsonb) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public')
  )::text`).Scan(&value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, version := range []int64{8, 9} {
		t.Run("controlled-rollback-"+map[int64]string{8: "organization", 9: "unit"}[version], func(t *testing.T) {
			var migration migrations.Migration
			for _, m := range loaded {
				if m.Version == version {
					migration = m
				}
			}
			if migration.Version != version {
				t.Fatal("original lifecycle SQL missing")
			}
			before := snapshot()
			failure := errors.New("controlled post-DDL migration hook failure")
			loanSeen, sqlSeen := false, false
			err := migrations.UpWithHooks(ctx, connection, []migrations.Migration{migration}, migrations.UpHooks{
				LedgerRole: "token_schema_owner",
				BeforeApply: func(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
					if err := prepareTokenMigration(ctx, tx, m); err != nil {
						return err
					}
					return tx.QueryRow(ctx, `SELECT current_user='token_gateway' AND has_schema_privilege('token_gateway','public','CREATE')`).Scan(&loanSeen)
				},
				AfterApply: func(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
					table := "organization_lifecycle_states"
					if m.Version == 9 {
						table = "organization_unit_lifecycle_states"
					}
					if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&sqlSeen); err != nil {
						return err
					}
					if err := finishTokenMigration(ctx, tx, m); err != nil {
						return err
					}
					return failure
				},
			})
			migrations.Unlock(ctx, connection)
			if !errors.Is(err, failure) || !loanSeen || !sqlSeen {
				t.Fatalf("original hooks/SQL did not reach controlled failure: loan=%v sql=%v err=%v", loanSeen, sqlSeen, err)
			}
			if after := snapshot(); after != before {
				t.Fatal("failed migration changed ledger, owners, ACLs, role or partial DDL")
			}
			assertTokenRuntimeStorage(t, ctx, connection)
			t.Logf("original %d SQL reached; temporary CREATE and all persistent catalog/ledger facts rolled back", version)
		})
	}
}
