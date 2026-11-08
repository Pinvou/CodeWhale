package billingschema

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// Called by the retained rollback root: install genuine numeric history, never
// delete a modern ledger to manufacture an early database. Complete current
// source still passes Ordered before the installed maximum chooses the Down.
func testRetainedDataRollbacks(t *testing.T, ctx context.Context, f *stageFixture, loaded []migrations.Migration, versions []int64) {
	t.Helper()
	if _, err := Ordered(loaded); err != nil {
		t.Fatal("complete Billing package must validate", err)
	}
	for _, version := range versions {
		for _, seeded := range []bool{false, true} {
			t.Run(fmt.Sprintf("version%d/seeded=%t", version, seeded), func(t *testing.T) {
				c := f.database(t, ctx, "billing", "billing_schema_owner")
				var legacy []migrations.Migration
				for _, m := range loaded {
					if m.Version < version {
						legacy = append(legacy, m)
					}
				}
				if err := migrations.Up(ctx, c, legacy); err != nil {
					t.Fatal("original numeric history", err)
				}
				migrations.Unlock(ctx, c)
				if seeded {
					seedRetainedDataRollback(t, ctx, c, version)
				}
				for _, m := range loaded {
					if m.Version == version {
						legacy = append(legacy, m)
					}
				}
				if err := migrations.Up(ctx, c, legacy); err != nil {
					t.Fatal("original target migration", err)
				}
				if _, err := migrations.VerifyApplied(ctx, c, legacy); err != nil {
					t.Fatal("original numeric ledger", err)
				}
				if got := stageValue(t, ctx, c, `SELECT max(version)::text FROM schema_migrations`); got != fmt.Sprint(version) {
					t.Fatal("fixture did not reach actual historical maximum")
				}
				if version == 46 && seeded && stageValue(t, ctx, c, `SELECT count(*)::text FROM payment_orders WHERE payment_url IS NOT NULL OR qr_data IS NOT NULL`) != "1" {
					t.Fatal("original cleanup must retain only the payable order instructions")
				}
				if version == 51 && seeded && stageValue(t, ctx, c, `SELECT etag::text FROM pricing_space_versions WHERE scope_type='MODEL' AND scope_key='owned-catalog-model'`) != "17" {
					t.Fatal("original repair did not install the retained concurrency fact")
				}
				migrations.Unlock(ctx, c)
				before := retainedDataRollbackSnapshot(t, ctx, c)
				err := DownOne(ctx, c, loaded)
				migrations.Unlock(ctx, c)
				reason := "prior payment mappings and prices"
				if version == 46 {
					reason = "payment instruction cleanup is irreversible"
				}
				if version == 51 {
					reason = "pricing space version repair is irreversible"
				}
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("rollback migration %d:", version)) || !strings.Contains(err.Error(), reason) {
					t.Fatalf("public DownOne did not reach the exact retained guard: %v", err)
				}
				if retainedDataRollbackSnapshot(t, ctx, c) != before {
					t.Fatal("rejected public Down changed data, schema or original ledger")
				}
				t.Run("actual-cli", func(t *testing.T) {
					binary := os.Getenv("PINVOU_TEST_ACCOUNT_MIGRATE_BINARY")
					if binary == "" {
						t.Skip("actual current migration CLI binary required; no CLI validation claimed")
					}
					directory, err := filepath.Abs("../../migrations/billing")
					if err != nil {
						t.Fatal(err)
					}
					childCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
					defer cancel()
					child := exec.CommandContext(childCtx, binary, "--direction", "down", "--dir", directory)
					for _, value := range os.Environ() {
						key, _, _ := strings.Cut(value, "=")
						switch key {
						case "DATABASE_URL", "MIGRATIONS_DIR", "MIGRATION_ROLE", "PINVOU_DEPLOYMENT_ENVIRONMENT":
							continue
						}
						child.Env = append(child.Env, value)
					}
					child.Env = append(child.Env, "DATABASE_URL="+f.serviceURL(t, c, "billing_migrator"), "MIGRATION_ROLE=billing_service", "PINVOU_DEPLOYMENT_ENVIRONMENT=test")
					output, err := child.CombinedOutput()
					if err == nil || childCtx.Err() != nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") {
						t.Fatal("actual Billing CLI must refuse the retained historical Down")
					}
					if retainedDataRollbackSnapshot(t, ctx, c) != before {
						t.Fatal("rejected CLI changed data, schema or original ledger")
					}
				})
			})
		}
	}
}

func TestBillingHistoricalCatalogRollbackPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("exclusive actual PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	goName, binaryName := "go", "migrate"
	if runtime.GOOS == "windows" {
		goName += ".exe"
		binaryName += ".exe"
	}
	binary := filepath.Join(t.TempDir(), binaryName)
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "build", "-tags=nomsgpack", "-mod=readonly", "-trimpath", "-o", binary, "./cmd/migrate")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build actual current migration CLI: %v\n%s", err, output)
	}
	t.Setenv("PINVOU_TEST_ACCOUNT_MIGRATE_BINARY", binary)
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	testRetainedDataRollbacks(t, ctx, f, loaded, []int64{35, 36, 37, 51})
}

func seedRetainedDataRollback(t *testing.T, ctx context.Context, c *pgx.Conn, version int64) {
	t.Helper()
	query := `INSERT INTO payment_product_mappings(id,provider_id,provider_version,product_id,mapping_version,external_sku,amount_fen,currency,channels,status,effective_from,created_at)
SELECT gen_random_uuid(),'h3c_mall','h3c-mall-v1','PLAN_TRIAL',max(mapping_version)+1,'OWNED_PRIOR_SKU',137,'CNY',ARRAY['ALIPAY'],'FROZEN','2026-01-01','2026-01-01'
FROM payment_product_mappings WHERE provider_id='h3c_mall' AND provider_version='h3c-mall-v1' AND product_id='PLAN_TRIAL'`
	if version == 35 || version == 36 || version == 37 {
		query += `; UPDATE recharge_products SET amount_fen=137 WHERE product_id='PLAN_TRIAL'`
	}
	if version == 51 {
		// Model an actual legacy price imported after the original space
		// backfill, which 051 repairs without a reversible deletion contract.
		query = `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_id,published_at,space_etag)
VALUES(gen_random_uuid(),'owned-catalog-model',137,271,19,'2026-01-01','2026-01-01','owned-catalog-model','2026-01-01',17)`
	}
	if version == 46 {
		query = `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at) VALUES('71111111-1111-4111-8111-111111111111','PERSONAL','11111111-1111-4111-8111-111111111111','ACTIVE','2026-01-01','2026-01-01');
INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,close_status,payment_url,qr_data,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at)
SELECT gen_random_uuid(),'owned-'||facts.label,'11111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','71111111-1111-4111-8111-111111111111','PLAN_TRIAL','{}','ALIPAY',10,'CNY',1,facts.status,'PENDING',facts.close_status,'https://payment.fixture.invalid/'||facts.label,'owned-qr-'||facts.label,'2026-02-01','2026-02-01','2026-02-02','2026-03-01','2026-01-01','2026-01-01'
FROM (VALUES('terminal','PAID','OPEN'),('closed','PENDING','PROVIDER_CLOSED'),('payable','PENDING','OPEN')) facts(label,status,close_status)`
	}
	if _, err := c.Exec(ctx, query); err != nil {
		t.Fatal("seed original retained rollback facts", err)
	}
}

func retainedDataRollbackSnapshot(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	t.Helper()
	rows, err := c.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	rows.Close()
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	var snapshot strings.Builder
	snapshot.WriteString(stageShape(t, ctx, c))
	for _, table := range tables {
		snapshot.WriteString(table)
		snapshot.WriteString(stageValue(t, ctx, c, `SELECT coalesce(jsonb_agg(v ORDER BY v::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(r) v FROM `+pgx.Identifier{table}.Sanitize()+` r) q`))
	}
	return snapshot.String()
}
