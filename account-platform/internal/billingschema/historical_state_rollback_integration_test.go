package billingschema

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

func TestBillingHistoricalStateRollbackPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("exclusive actual PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	binary := buildRetainedBillingMigrationCLI(t, ctx)
	f := newStageFixture(t, ctx, raw)
	directory, err := filepath.Abs("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{10, 14, 27, 49} {
		scenarios := []string{"seeded=false", "seeded=true"}
		switch version {
		case 14:
			scenarios = append(scenarios, "repair-only")
		case 27:
			scenarios = append(scenarios, "revoked-only")
		case 49:
			scenarios = append(scenarios, "advanced-after-expiry")
		}
		for _, scenario := range scenarios {
			t.Run(fmt.Sprintf("version%d/%s", version, scenario), func(t *testing.T) {
				seeded := scenario == "seeded=true"
				wantBlocked := scenario != "seeded=false" || version == 10
				c := f.database(t, ctx, "billing", "billing_schema_owner")
				retireRetainedBillingDatabase(t, f, c)
				var legacy []migrations.Migration
				for _, migration := range loaded {
					if migration.Version < version {
						legacy = append(legacy, migration)
					}
				}
				installHistoricalBilling(t, ctx, c, legacy)
				if seeded || scenario == "advanced-after-expiry" {
					seedHistoricalBillingBefore(t, ctx, c, version)
				}
				original := retainedDataRollbackSnapshot(t, ctx, c)
				for _, migration := range loaded {
					if migration.Version == version {
						legacy = append(legacy, migration)
					}
				}
				installHistoricalBilling(t, ctx, c, legacy)
				if seeded {
					seedHistoricalBillingAfter(t, ctx, c, version)
				}
				switch scenario {
				case "repair-only":
					seedHistoricalPaymentRepair(t, ctx, c)
				case "revoked-only":
					seedHistoricalRevokedEntitlement(t, ctx, c)
				case "advanced-after-expiry":
					if _, err := c.Exec(ctx, `UPDATE payment_orders SET status='PAID_LATE' WHERE status='EXPIRED'`); err != nil {
						t.Fatal("advance migrated expiry to an allowed later state", err)
					}
					if got := stageValue(t, ctx, c, `SELECT status||'/'||(payment_url IS NULL AND qr_data IS NULL)::text FROM payment_orders`); got != "PAID_LATE/true" {
						t.Fatalf("migration did not clear the retained order instructions: %s", got)
					}
				}
				if got := stageValue(t, ctx, c, `SELECT max(version)::text FROM schema_migrations`); got != fmt.Sprint(version) {
					t.Fatalf("unexpected historical maximum %s", got)
				}
				before := retainedDataRollbackSnapshot(t, ctx, c)
				if wantBlocked {
					err := DownOne(ctx, c, loaded)
					migrations.Unlock(ctx, c)
					if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("rollback migration %d:", version)) || !strings.Contains(err.Error(), "cannot preserve") {
						t.Fatalf("public rollback accepted or missed the historical guard: %v", err)
					}
					if retainedDataRollbackSnapshot(t, ctx, c) != before {
						t.Fatal("guard changed retained history")
					}
				}
				t.Run("actual-cli", func(t *testing.T) {
					output, err := runRetainedBillingDownCLI(t, ctx, binary, directory, f.serviceURL(t, c, "billing_migrator"))
					if wantBlocked {
						if err == nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") || retainedDataRollbackSnapshot(t, ctx, c) != before {
							t.Fatal("CLI failed to preserve the guarded history")
						}
						return
					}
					if err != nil || retainedDataRollbackSnapshot(t, ctx, c) != original {
						t.Fatalf("empty compatible history did not round trip: %v %s", err, output)
					}
				})
				if seeded {
					if !t.Run("archived-cli", func(t *testing.T) {
						archive := copyHistoricalBillingPackage(t, directory, legacy)
						output, err := runRetainedBillingDownCLI(t, ctx, binary, archive, f.serviceURL(t, c, "billing_migrator"))
						if err == nil || !strings.Contains(string(output), "MIGRATION_EXECUTION_FAILED") || retainedDataRollbackSnapshot(t, ctx, c) != before {
							t.Fatal("archived numeric package bypassed the Billing entry")
						}
					}) {
						return
					}
					assertHistoricalBillingOriginalDownLosesFacts(t, ctx, c, loaded, version)
				}
				switch scenario {
				case "repair-only":
					assertOriginalBillingDownValue(t, ctx, c, loaded, `SELECT (to_regclass('payment_order_repairs') IS NULL)::text`, "true")
				case "revoked-only":
					assertOriginalBillingDownValue(t, ctx, c, loaded, `SELECT status FROM entitlement_packages WHERE source_reference='owned-revoked'`, "FROZEN")
				case "advanced-after-expiry":
					assertOriginalBillingDownValue(t, ctx, c, loaded, `SELECT status||'/'||(payment_url IS NULL AND qr_data IS NULL)::text FROM payment_orders`, "PAID_LATE/true")
				}
			})
		}
	}
}

func copyHistoricalBillingPackage(t *testing.T, directory string, items []migrations.Migration) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "archived-sql")
	if err := os.Mkdir(archive, 0700); err != nil {
		t.Fatal(err)
	}
	for _, migration := range items {
		for _, direction := range []string{"up", "down"} {
			name := fmt.Sprintf("%06d_%s.%s.sql", migration.Version, migration.Name, direction)
			body, err := os.ReadFile(filepath.Join(directory, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(archive, name), body, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return archive
}

func installHistoricalBilling(t *testing.T, ctx context.Context, c *pgx.Conn, items []migrations.Migration) {
	t.Helper()
	if err := migrations.Up(ctx, c, items); err != nil {
		t.Fatal("install original numeric migrations", err)
	}
	if _, err := migrations.VerifyApplied(ctx, c, items); err != nil {
		t.Fatal("verify original migration ledger", err)
	}
	migrations.Unlock(ctx, c)
}

const historicalRollbackWallet = `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at)
VALUES('71111111-1111-4111-8111-111111111111','PERSONAL','11111111-1111-4111-8111-111111111111','ACTIVE',now(),now());`

func seedHistoricalBillingBefore(t *testing.T, ctx context.Context, c *pgx.Conn, version int64) {
	t.Helper()
	var query string
	switch version {
	case 10:
		query = `UPDATE recharge_products SET enabled=false WHERE product_id='CNY_1'`
	case 27:
		query = historicalRollbackWallet + `
INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,created_at,updated_at)
VALUES('21111111-1111-4111-8111-111111111111','retained-original-refunded','11111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','71111111-1111-4111-8111-111111111111','PLAN_TRIAL','{}','ALIPAY',10,'CNY',1,'REFUNDED','REFUNDED',now(),now(),now())`
	case 49:
		query = historicalRollbackWallet + `
INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,close_status,payment_url,qr_data,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at)
VALUES(gen_random_uuid(),'retained-original-expired','11111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','71111111-1111-4111-8111-111111111111','PLAN_TRIAL','{}','ALIPAY',10,'CNY',1,'CREATED','PENDING','CLOSE_REQUESTED','https://payment.fixture.invalid/owned','owned-qr',now()-interval '2 days',now()-interval '2 days',now()-interval '1 day',now()+interval '1 day',now()-interval '3 days',now()-interval '3 days')`
	}
	if query != "" {
		if _, err := c.Exec(ctx, query); err != nil {
			t.Fatal("seed pre-migration evidence", err)
		}
	}
}

func seedHistoricalBillingAfter(t *testing.T, ctx context.Context, c *pgx.Conn, version int64) {
	t.Helper()
	if version != 14 {
		return
	}
	if _, err := c.Exec(ctx, `INSERT INTO payment_notifications(notification_id,channel,request_hash,payload,received_at,result)
VALUES(gen_random_uuid(),'ALIPAY','owned-manual-review','{}',now(),'MANUAL_REVIEW')`); err != nil {
		t.Fatal("seed historical manual review", err)
	}
}

func assertHistoricalBillingOriginalDownLosesFacts(t *testing.T, ctx context.Context, c *pgx.Conn, loaded []migrations.Migration, version int64) {
	t.Helper()
	var query, want string
	switch version {
	case 10:
		query, want = `SELECT enabled::text FROM recharge_products WHERE product_id='CNY_1'`, "true"
	case 14:
		query, want = `SELECT result FROM payment_notifications WHERE request_hash='owned-manual-review'`, "REJECTED"
	case 27:
		query, want = `SELECT status||'/'||fulfillment_status FROM payment_orders WHERE merchant_order_no='retained-original-refunded'`, "PAID/CREDITED"
	case 49:
		query, want = `SELECT status||'/'||(payment_url IS NULL AND qr_data IS NULL)::text FROM payment_orders WHERE merchant_order_no='retained-original-expired'`, "PENDING/true"
	}
	assertOriginalBillingDownValue(t, ctx, c, loaded, query, want)
}

func assertOriginalBillingDownValue(t *testing.T, ctx context.Context, c *pgx.Conn, loaded []migrations.Migration, query, want string) {
	t.Helper()
	if err := migrations.DownOne(ctx, c, loaded); err != nil {
		t.Fatal("original historical rollback did not reproduce state loss", err)
	}
	migrations.Unlock(ctx, c)
	if got := stageValue(t, ctx, c, query); got != want {
		t.Fatalf("original historical Down loss mismatch: got %q, want %q", got, want)
	}
}

func seedHistoricalPaymentRepair(t *testing.T, ctx context.Context, c *pgx.Conn) {
	t.Helper()
	_, err := c.Exec(ctx, historicalRollbackWallet+`
INSERT INTO payment_orders(id,merchant_order_no,requester_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,created_at,updated_at)
VALUES('21111111-1111-4111-8111-111111111111','retained-original-repaired','11111111-1111-4111-8111-111111111111','71111111-1111-4111-8111-111111111111','PLAN_TRIAL','{}','ALIPAY',10,'CNY',1,'PAID','CREDITED',now(),now(),now());
INSERT INTO payment_order_repairs(id,payment_order_id,actor_id,reason,provider_snapshot,created_at)
VALUES(gen_random_uuid(),'21111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','owned repair','{}',now())`)
	if err != nil {
		t.Fatal("seed repair history without review notification", err)
	}
}

func seedHistoricalRevokedEntitlement(t *testing.T, ctx context.Context, c *pgx.Conn) {
	t.Helper()
	_, err := c.Exec(ctx, historicalRollbackWallet+`
INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,created_at)
VALUES('81111111-1111-4111-8111-111111111111','71111111-1111-4111-8111-111111111111','PROMOTION','owned-revoked',10,10,false,now());
INSERT INTO entitlement_packages(id,user_id,wallet_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,points_revoked,credited_at,valid_from,valid_until,status,source_type,source_reference,created_at,updated_at)
VALUES(gen_random_uuid(),'11111111-1111-4111-8111-111111111111','71111111-1111-4111-8111-111111111111','81111111-1111-4111-8111-111111111111','PLAN_TRIAL','Owned','OWNED',0,10,10,0,now(),now(),now()+interval '1 day','REVOKED','SYSTEM','owned-revoked',now(),now())`)
	if err != nil {
		t.Fatal("seed revoked entitlement without any payment order", err)
	}
}
