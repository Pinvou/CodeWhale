package billingschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMemberFinancialReviewReasonPinned(t *testing.T) {
	full, e := migrations.Load("../../migrations/billing")
	if e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"valid", "missing", "bad-name", "bad-up", "bad-down", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			in := append([]migrations.Migration(nil), full...)
			for i := range in {
				if in[i].Version == memberFinancialReasonVersion {
					switch kind {
					case "missing":
						in = append(in[:i], in[i+1:]...)
					case "bad-name":
						in[i].Name = "other"
					case "bad-up":
						in[i].UpSQL += "\nSELECT 1;"
					case "bad-down":
						in[i].DownSQL = "DROP TABLE member_quota_grants;"
					case "duplicate":
						in = append(in, in[i])
					}
					break
				}
			}
			_, err := Ordered(in)
			if (kind == "valid") != (err == nil) {
				t.Fatal("migration identity guard mismatch", kind, err)
			}
		})
	}
}
func TestMemberFinancialReviewReasonPublicUpgrade(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" {
		t.Skip("dedicated PostgreSQL schema fixture is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	full, e := migrations.Load("../../migrations/billing")
	if e != nil {
		t.Fatal(e)
	}
	fresh := memberMigrationDatabase(t, ctx, raw)
	upgrade := memberMigrationDatabase(t, ctx, raw)
	ordered, e := Ordered(full)
	if e != nil {
		t.Fatal(e)
	}
	var prior []migrations.Migration
	for _, x := range ordered {
		if x.Version != memberFinancialReasonVersion && x.Version != memberIdentityVersion && x.Version != 18 && x.Version != 19 && x.Version != 20 && x.Version != 30 && x.Version != 41 && x.Version != 78 {
			prior = append(prior, x)
		}
	}
	// Retain the complete adjacent F05 package: late organization/member DDL
	// is installed after the already-hardened ownership stage, using actual hooks.
	if e = Up(ctx, upgrade, prior); e != nil {
		t.Fatal(e)
	}
	if _, e = upgrade.Exec(ctx, stageBillingSeed); e != nil {
		t.Fatal(e)
	}
	migrations.Unlock(ctx, upgrade)
	facts := stageWalletFacts(t, ctx, upgrade)
	if e = Up(ctx, upgrade, full); e != nil {
		t.Fatal(e)
	}
	if _, e = migrations.VerifyApplied(ctx, upgrade, full); e != nil {
		t.Fatal(e)
	}
	migrations.Unlock(ctx, upgrade)
	if facts != stageWalletFacts(t, ctx, upgrade) {
		t.Fatal("upgrade changed retained wallet facts")
	}
	if e = Up(ctx, fresh, full); e != nil {
		t.Fatal(e)
	}
	if _, e = migrations.VerifyApplied(ctx, fresh, full); e != nil {
		t.Fatal(e)
	}
	migrations.Unlock(ctx, fresh)
	if stageShape(t, ctx, fresh) != stageShape(t, ctx, upgrade) {
		t.Fatal("fresh/cumulative schema owner/ACL mismatch")
	}
	if !strings.Contains(stageValue(t, ctx, upgrade, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='member_quota_grants'::regclass AND conname='member_quota_grants_suspension_reason_check'`), "CHARGEBACK_REVIEW") {
		t.Fatal("mature financial reason missing")
	}
	if e = Up(ctx, upgrade, full); e != nil {
		t.Fatal("repeat member migration", e)
	}
	migrations.Unlock(ctx, upgrade)
	assertMemberMigrationRuntime(t, ctx, raw, upgrade, full)
	ledger := stageValue(t, ctx, upgrade, `SELECT jsonb_agg(jsonb_build_array(version,checksum) ORDER BY version)::text FROM schema_migrations`)
	if _, e = Ordered(full); e != nil {
		t.Fatal(e)
	}
	if e = migrations.DownOne(ctx, upgrade, full); e == nil {
		t.Fatal("financial reason rollback was permitted")
	}
	if ledger != stageValue(t, ctx, upgrade, `SELECT jsonb_agg(jsonb_build_array(version,checksum) ORDER BY version)::text FROM schema_migrations`) {
		t.Fatal("rejected rollback changed ledger")
	}
}

// Reuse pre-provisioned deployment identities; never mutate cluster-wide roles.
func memberMigrationDatabase(t *testing.T, ctx context.Context, raw string) *pgx.Conn {
	t.Helper()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	name := "f06_member_schema_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER billing_schema_owner"); err != nil {
		admin.Close(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		defer admin.Close(clean)
		var owner string
		var active int
		if e := admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&owner, &active); e != nil || owner != "billing_schema_owner" || active != 0 {
			t.Error("owned member migration cleanup refused", e)
			return
		}
		if _, e := admin.Exec(clean, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); e != nil {
			t.Error(e)
		}
	})
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = name
	config.User = "billing_migrator"
	config.Password = ""
	c, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	if _, err = c.Exec(ctx, `SET ROLE billing_schema_owner; ALTER SCHEMA public OWNER TO billing_schema_owner; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE,CREATE ON SCHEMA public TO billing_service; SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	return c
}
func assertMemberMigrationRuntime(t *testing.T, ctx context.Context, raw string, migrator *pgx.Conn, full []migrations.Migration) {
	t.Helper()
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = migrator.Config().Database
	config.User = "billing_service"
	config.Password = ""
	runtime, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	var actor bool
	if err = runtime.QueryRow(ctx, `SELECT current_user='billing_service' AND session_user='billing_service' AND NOT pg_has_role(current_user,'billing_schema_owner','MEMBER') AND NOT has_schema_privilege(current_user,'public','CREATE')`).Scan(&actor); err != nil || !actor {
		t.Fatal("runtime actor boundary", actor, err)
	}
	if v, e := migrations.VerifyApplied(ctx, runtime, full); e != nil || v.AppliedCount != len(full) {
		t.Fatal("runtime verifies complete immutable history", v, e)
	}
	if _, err = runtime.Exec(ctx, `ALTER TABLE organization_member_lifecycle_states ADD COLUMN unauthorized BOOLEAN`); err == nil {
		t.Fatal("runtime member DDL succeeded")
	}
	if _, err = runtime.Exec(ctx, `UPDATE schema_migrations SET checksum=checksum WHERE version=$1`, memberIdentityVersion); err == nil {
		t.Fatal("runtime ledger mutation succeeded")
	}
}
