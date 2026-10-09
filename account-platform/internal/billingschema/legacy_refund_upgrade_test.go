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

// Controlled legal pre-027 persisted shapes, not the retired partial-refund API
// or a real Provider refund. The migration must preserve undecided money facts.
func TestLegacyPendingRefundBlocksTargetedMigration(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("connect owned upgrade fixture")
	}
	defer admin.Close(context.Background())
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("parse owned upgrade fixture")
	}
	all, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var before27, through27 []migrations.Migration
	for _, m := range all {
		if m.Version <= 25 || m.Version == 900001 {
			before27 = append(before27, m)
			through27 = append(through27, m)
		}
		if m.Version == 27 {
			through27 = append(through27, m)
		}
	}
	for _, status := range []string{"REQUESTED", "MANUAL_REVIEW", "SUCCEEDED", "FAILED", "EMPTY", "CONCURRENT_REQUESTED"} {
		t.Run(status, func(t *testing.T) {
			name := "f02_legacy_refund_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
				t.Fatal("create exclusively owned upgrade DB")
			}
			defer func() {
				clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
				defer stop()
				var owner string
				var active int
				if err := admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&owner, &active); err != nil || owner != config.User || active != 0 {
					t.Error("owned upgrade DB cleanup guard failed")
					return
				}
				if _, err := admin.Exec(clean, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
					t.Error("nonforce owned upgrade DB cleanup failed")
				}
			}()
			own := config.Copy()
			own.Database = name
			c, err := pgx.ConnectConfig(ctx, own)
			if err != nil {
				t.Fatal("connect owned upgrade DB")
			}
			defer c.Close(context.Background())
			if err := Up(ctx, c, before27); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Exec(ctx, legacyUpgradeSeed); err != nil {
				t.Fatal(err)
			}
			var writer pgx.Tx
			storedStatus := status
			if status == "CONCURRENT_REQUESTED" {
				storedStatus = "REQUESTED"
				writer, err = c.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Rollback(context.Background())
			}
			if status != "EMPTY" {
				reserved := int64(0)
				if storedStatus == "REQUESTED" || storedStatus == "MANUAL_REVIEW" {
					reserved = 400000
				}
				if _, err := c.Exec(ctx, `UPDATE wallets SET reserved_microcredits=$1`, reserved); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Exec(ctx, `INSERT INTO refunds(id,payment_order_id,requester_id,merchant_refund_no,amount_fen,credit_microcredits,status,idempotency_key,request_hash,reason,created_at,updated_at) VALUES('81111111-1111-4111-8111-111111111111','21111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','owned-legacy-refund',40,400000,$1,'owned-legacy-key',repeat('a',64),'owned fixture','2026-01-01','2026-01-01')`, storedStatus); err != nil {
					t.Fatal(err)
				}
			}
			data := legacyUpgradeFacts(t, ctx, c)
			shape := stageShape(t, ctx, c)
			ledger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
			if writer == nil {
				err = Up(ctx, c, through27)
			} else {
				migrator, openErr := pgx.ConnectConfig(ctx, own)
				if openErr != nil {
					t.Fatal("connect concurrent migration fixture")
				}
				defer migrator.Close(context.Background())
				pid := migrator.PgConn().PID()
				migrationCtx, stop := context.WithTimeout(ctx, 10*time.Second)
				done := make(chan error, 1)
				go func() { done <- Up(migrationCtx, migrator, through27) }()
				finished := false
				defer func() {
					stop()
					if !finished {
						_ = writer.Rollback(context.Background())
						<-done
					}
				}()
				blocked := false
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if queryErr := admin.QueryRow(ctx, `SELECT $1::int = ANY(pg_blocking_pids($2::int))`, int(c.PgConn().PID()), int(pid)).Scan(&blocked); queryErr != nil {
						t.Fatal("observe exact old-writer migration wait")
					}
					if blocked {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				// Always release the owned writer before waiting for migration exit.
				if commitErr := writer.Commit(ctx); commitErr != nil {
					t.Fatal("commit controlled old pending refund")
				}
				err = <-done
				finished = true
				if !blocked {
					t.Fatal("migration did not wait for the exact concurrent legacy writer")
				}
			}
			pending := storedStatus == "REQUESTED" || storedStatus == "MANUAL_REVIEW"
			if pending {
				if err == nil {
					t.Fatal("legacy pending refund was silently converted to the whole-target contract")
				}
				if data != legacyUpgradeFacts(t, ctx, c) || shape != stageShape(t, ctx, c) || ledger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`) {
					t.Fatal("blocked migration changed persisted funds, history, schema, ACL or ledger")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if data != legacyUpgradeFacts(t, ctx, c) {
				t.Fatal("terminal/empty migration changed original stored money/history")
			}
			if status != "EMPTY" {
				var amount, credit, base int64
				var current, source string
				var transaction, pack uuid.UUID
				if err := c.QueryRow(ctx, `SELECT status,amount_fen,credit_microcredits,base_microcredits,payment_transaction_id,entitlement_package_id,entitlement_snapshot->>'migration_source' FROM refunds`).Scan(&current, &amount, &credit, &base, &transaction, &pack, &source); err != nil || current != status || amount != 40 || credit != 400000 || base != credit || transaction != uuid.MustParse("31111111-1111-4111-8111-111111111111") || pack != uuid.MustParse("51111111-1111-4111-8111-111111111111") || source != "LEGACY_ORDER_REFUND" {
					t.Fatal("terminal history not precisely backfilled")
				}
			}
		})
	}
}

func legacyUpgradeFacts(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	return stageValue(t, ctx, c, `SELECT jsonb_build_object('wallet',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),'refund',(SELECT jsonb_agg(jsonb_build_array(id,payment_order_id,requester_id,merchant_refund_no,channel_refund_id,amount_fen,credit_microcredits,status,idempotency_key,request_hash,reason,failure_reason,created_at,updated_at) ORDER BY id) FROM refunds),'transactions',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM payment_transactions p),'packages',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM entitlement_packages p),'lots',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM credit_lots p),'journals',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM journal_entries p),'postings',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM ledger_postings p))::text`)
}

const legacyUpgradeSeed = `
INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,version,created_at,updated_at) VALUES('71111111-1111-4111-8111-111111111111','PERSONAL','11111111-1111-4111-8111-111111111111','ACTIVE',1000000,0,1,'2026-01-01','2026-01-01');
INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,paid_at,created_at,updated_at) VALUES('21111111-1111-4111-8111-111111111111','owned-legacy-order','11111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','71111111-1111-4111-8111-111111111111','CNY_1','{"kind":"LEGACY"}','UNIONPAY',100,'CNY',1000000,'PAID','CREDITED','2027-01-01','2026-01-01','2026-01-01','2026-01-01');
INSERT INTO payment_transactions(id,payment_order_id,channel,provider_transaction_id,status,amount_fen,currency,source,verified_at,created_at,updated_at,fulfillment_status,fulfilled_at) VALUES('31111111-1111-4111-8111-111111111111','21111111-1111-4111-8111-111111111111','UNIONPAY','owned-legacy-transaction','PAID',100,'CNY','QUERY','2026-01-01','2026-01-01','2026-01-01','CREDITED','2026-01-01');
INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES('41111111-1111-4111-8111-111111111111','71111111-1111-4111-8111-111111111111','RECHARGE','owned-legacy-payment',1000000,1000000,true,'2027-01-01','2026-01-01');
INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES('51111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','71111111-1111-4111-8111-111111111111','21111111-1111-4111-8111-111111111111','31111111-1111-4111-8111-111111111111','41111111-1111-4111-8111-111111111111','CNY_1','Owned legacy','','100',1000000,1000000,'2026-01-01','2026-01-01','2027-01-01','ACTIVE','2026-01-01','2026-01-01','PAYMENT','owned-legacy-payment');
INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES('61111111-1111-4111-8111-111111111111','51111111-1111-4111-8111-111111111111','BASE','41111111-1111-4111-8111-111111111111',1000000,1000000,'2026-01-01','2027-01-01','ACTIVE','2026-01-01','2026-01-01');
`
