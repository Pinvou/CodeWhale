package accountschema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Invoked under TestAuditExportPublicDownRetainsOriginalHistory so the existing
// required CI selection and non-skip result guard execute this public rollback.
func testThreeScopePublicRollback(t *testing.T, ctx context.Context, f *auditExportGuardFixture, loaded []migrations.Migration) {
	for _, lineEnding := range []string{"LF", "CRLF"} {
		t.Run(lineEnding, func(t *testing.T) {
			c := f.database(t, ctx, "account", "account_service")
			var history, after []migrations.Migration
			for _, m := range loaded {
				if m.Version <= 34 {
					if m.Version == 34 && lineEnding == "CRLF" {
						m.UpSQL = strings.ReplaceAll(normalizedGovernanceSQL(m.UpSQL), "\n", "\r\n")
						m.DownSQL = strings.ReplaceAll(normalizedGovernanceSQL(m.DownSQL), "\n", "\r\n")
						// The generic engine writes the real historical checkout digest.
						m.Checksum = governanceSQLDigest(m.UpSQL)
					}
					history = append(history, m)
					if m.Version < 34 {
						after = append(after, m)
					}
				}
			}
			if err := migrations.Up(ctx, c, history); err != nil {
				t.Fatal("ORIGINAL_1_34_UP", err)
			}
			migrations.Unlock(ctx, c)
			if _, err := migrations.VerifyApplied(ctx, c, history); err != nil {
				t.Fatal(err)
			}
			user, org, unit := uuid.New(), uuid.New(), uuid.New()
			code, err := account.GenerateUserCode()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Exec(ctx, `INSERT INTO users(id,user_code,status,created_at,updated_at) VALUES($1,$2,'ACTIVE',now(),now())`, user, code); err != nil {
				t.Fatal(err)
			}
			setup, err := c.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer setup.Rollback(context.Background())
			if _, err := setup.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,'Owned fixture','ACTIVE',$2,now(),now())`, org, user); err != nil {
				t.Fatal(err)
			}
			membership := uuid.New()
			if _, err := setup.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,'ACTIVE',now(),now())`, membership, org, user); err != nil {
				t.Fatal(err)
			}
			if _, err := setup.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at) VALUES($1,$2,'OWNER',$3,now(),now())`, uuid.New(), membership, user); err != nil {
				t.Fatal(err)
			}
			if err := setup.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Exec(ctx, `INSERT INTO organization_units(id,organization_id,name,status,created_at,updated_at) VALUES($1,$2,'Owned unit','ACTIVE',now(),now())`, unit, org); err != nil {
				t.Fatal(err)
			}
			bind := func(role string, organization, u any) uuid.UUID {
				t.Helper()
				id := uuid.New()
				if _, err := c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,organization_id,unit_id,source,created_at,starts_at) VALUES($1,$2,$3,$4,$5,'ACCOUNT',now(),now())`, id, user, role, organization, u); err != nil {
					t.Fatal(err)
				}
				return id
			}
			bind("UNIT_ADMIN", org, unit)
			bind("PLATFORM_IDENTITY_ADMIN", nil, nil)
			personal := bind("PERSONAL_USER", nil, nil)
			if _, err := c.Exec(ctx, `UPDATE role_assignments SET source='ACCOUNT_LIFECYCLE' WHERE id=$1`, personal); err != nil {
				t.Fatal(err)
			}
			snapshot := func(preserved bool) string {
				t.Helper()
				rows, err := c.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`)
				if err != nil {
					t.Fatal(err)
				}
				var names []string
				for rows.Next() {
					var n string
					if err := rows.Scan(&n); err != nil {
						t.Fatal(err)
					}
					names = append(names, n)
				}
				rows.Close()
				if rows.Err() != nil {
					t.Fatal(rows.Err())
				}
				var result strings.Builder
				for _, n := range names {
					if preserved && (n == "roles" || n == "permissions" || n == "role_permissions" || n == "authorization_versions") {
						continue
					}
					where := ""
					if preserved && n == "schema_migrations" {
						where = " WHERE version<>34"
					}
					if preserved && n == "role_assignments" {
						where = " WHERE NOT(role_code='PERSONAL_USER' AND source='ACCOUNT_LIFECYCLE')"
					}
					var data string
					if err := c.QueryRow(ctx, "SELECT coalesce(jsonb_agg(v ORDER BY v::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(r) v FROM "+pgx.Identifier{n}.Sanitize()+" r"+where+") q").Scan(&data); err != nil {
						t.Fatal(err)
					}
					result.WriteString(n + data)
				}
				if !preserved {
					var schema string
					if err := c.QueryRow(ctx, `SELECT jsonb_build_object('constraints',(SELECT jsonb_agg(jsonb_build_array(conrelid::regclass::text,conname,pg_get_constraintdef(oid)) ORDER BY conrelid::regclass::text,conname) FROM pg_constraint WHERE connamespace='public'::regnamespace),'functions',(SELECT jsonb_agg(pg_get_functiondef(oid) ORDER BY oid::regprocedure::text) FROM pg_proc WHERE pronamespace='public'::regnamespace AND prokind='f'),'triggers',(SELECT jsonb_agg(pg_get_triggerdef(oid) ORDER BY tgrelid,tgname) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid IN(SELECT oid FROM pg_class WHERE relnamespace='public'::regnamespace)))::text`).Scan(&schema); err != nil {
						t.Fatal(err)
					}
					result.WriteString(schema)
				}
				return result.String()
			}
			reject := func(label string, input []migrations.Migration, original bool, sqlstate string) {
				t.Helper()
				before := snapshot(false)
				var err error
				if original {
					err = migrations.DownOne(ctx, c, input)
				} else {
					err = DownOne(ctx, c, input)
				}
				migrations.Unlock(ctx, c)
				if err == nil {
					t.Fatal(label, "ACCEPTED")
				}
				if sqlstate != "" {
					var pgerr *pgconn.PgError
					if !errors.As(err, &pgerr) || pgerr.Code != sqlstate {
						t.Fatal(label, err)
					}
					if original && pgerr.ConstraintName != "authorization_versions_scope_check" {
						t.Fatal("unexpected original failure constraint", pgerr.ConstraintName)
					}
				}
				if snapshot(false) != before {
					t.Fatal(label, "CHANGED_WHOLE_STATE")
				}
				t.Log(label, "rejected; whole state unchanged")
			}
			reject("ORIGINAL_UNIT_CHECK_NEGATIVE", history, true, "23514")
			for _, change := range []string{"name", "checksum", "up", "down", "version"} {
				changed := append([]migrations.Migration(nil), loaded...)
				for i := range changed {
					if changed[i].Version != 34 {
						continue
					}
					switch change {
					case "name":
						changed[i].Name += "_drift"
					case "checksum":
						changed[i].Checksum = strings.Repeat("0", 64)
					case "up":
						changed[i].UpSQL += "\nSELECT 1;"
					case "down":
						changed[i].DownSQL += "\nSELECT 1;"
					case "version":
						changed[i].Version = 340000
					}
				}
				reject("SOURCE_"+change, changed, false, "")
			}
			for _, column := range []string{"name", "checksum"} {
				var old string
				if err := c.QueryRow(ctx, "SELECT "+column+" FROM schema_migrations WHERE version=34").Scan(&old); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Exec(ctx, "UPDATE schema_migrations SET "+column+"=$1 WHERE version=34", strings.Repeat("0", 64)); err != nil {
					t.Fatal(err)
				}
				reject("LEDGER_"+column, loaded, false, "")
				if _, err := c.Exec(ctx, "UPDATE schema_migrations SET "+column+"=$1 WHERE version=34", old); err != nil {
					t.Fatal(err)
				}
			}
			model := bind("ORG_MODEL_ADMIN", org, nil)
			reject("BOUND_MODEL_ORIGINAL_FK", loaded, false, "23503")
			if _, err := c.Exec(ctx, `DELETE FROM role_assignments WHERE id=$1`, model); err != nil {
				t.Fatal(err)
			}
			versions := func() (int64, int64) {
				t.Helper()
				var o, p int64
				if err := c.QueryRow(ctx, `SELECT max(version) FILTER(WHERE scope='ORGANIZATION'),max(version) FILTER(WHERE scope='PLATFORM') FROM authorization_versions WHERE user_id=$1`, user).Scan(&o, &p); err != nil {
					t.Fatal(err)
				}
				return o, p
			}
			o, p := versions()
			before := snapshot(true)
			if err := DownOne(ctx, c, loaded); err != nil {
				t.Fatal("PUBLIC_ADAPTED_DOWN", err)
			}
			migrations.Unlock(ctx, c)
			if snapshot(true) != before {
				t.Fatal("PRESERVED_DATA_OR_LEDGER_CHANGED")
			}
			no, np := versions()
			// The real owner provisioning trigger also creates OWNER's binding:
			// both OWNER and UNIT_ADMIN invalidate this organization once.
			if no != o+2 || np != p+1 {
				t.Fatal("CACHE_INVALIDATION", o, p, no, np)
			}
			var retired, ledger, personalLeft int
			if err := c.QueryRow(ctx, `SELECT (SELECT count(*) FROM authorization_versions WHERE scope IN('PERSONAL','UNIT')),(SELECT count(*) FROM schema_migrations WHERE version=34),(SELECT count(*) FROM role_assignments WHERE role_code='PERSONAL_USER')`).Scan(&retired, &ledger, &personalLeft); err != nil || retired != 0 || ledger != 0 || personalLeft != 0 {
				t.Fatal("RETIRED_STATE_REMAINS", err)
			}
			if _, err := migrations.VerifyApplied(ctx, c, after); err != nil {
				t.Fatal(err)
			}
			t.Log("PUBLIC_ADAPTED_DOWN_OK: real history, preserved UNIT binding and original data; ORGANIZATION +2 (OWNER, UNIT_ADMIN), PLATFORM +1; retired caches absent; engine removed only ledger34")
		})
	}
}
