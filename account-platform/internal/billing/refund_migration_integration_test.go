package billing

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

func TestPostgresTargetedRefundMigrationPreservesUnfinishedLegacyFacts(t *testing.T) {
	for _, status := range []string{"REQUESTED", "MANUAL_REVIEW", "SUCCEEDED", "FAILED", "EMPTY"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			p, wallet, user := paymentFixture(t)
			order := creditedOrder(t, p, wallet, user, uuid.NewString())
			source, packageID := refundTargetFixture(t, p, order.ID)
			loaded, err := migrations.Load("../../migrations/billing")
			if err != nil {
				t.Fatal(err)
			}
			connection, err := p.repository.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Release()
			defer migrations.Unlock(ctx, connection.Conn())
			for {
				var version int64
				if err = connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
					t.Fatal(err)
				}
				if version == 26 {
					break
				}
				if err = migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
					t.Fatal(err)
				}
			}
			refundID, key := uuid.New(), uuid.NewString()
			if status != "EMPTY" {
				// Persist the actual preceding schema's partial command shape. This
				// exercises real Up/Down; it does not dispatch a historical provider.
				if _, err = connection.Exec(ctx, `INSERT INTO refunds(id,payment_order_id,requester_id,merchant_refund_no,amount_fen,credit_microcredits,status,idempotency_key,request_hash,reason,created_at,updated_at) VALUES($1,$2,$3,$4,40,400000,$5,$4,'historical-facts','controlled preceding refund',$6,$6)`, refundID, order.ID, user, key, status, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			pending := status == "REQUESTED" || status == "MANUAL_REVIEW"
			if pending {
				if _, err = connection.Exec(ctx, `UPDATE wallets SET reserved_microcredits=400000 WHERE id=$1`, wallet.ID); err != nil {
					t.Fatal(err)
				}
			}
			const snapshotSQL = `SELECT jsonb_build_object(
				'wallets',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),
				'refunds',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM refunds r),
				'journal',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j),
				'postings',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM ledger_postings p),
				'migrations',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),
				'columns',(SELECT jsonb_agg(to_jsonb(c) ORDER BY table_name,ordinal_position) FROM information_schema.columns c WHERE table_schema=current_schema()),
				'constraints',(SELECT jsonb_agg(jsonb_build_array(conname,pg_get_constraintdef(oid)) ORDER BY conname,oid) FROM pg_constraint WHERE connamespace=current_schema()::regnamespace))::text`
			var before, after string
			if err = connection.QueryRow(ctx, snapshotSQL).Scan(&before); err != nil {
				t.Fatal(err)
			}
			err = migrations.Up(ctx, connection.Conn(), loaded)
			if pending {
				if err == nil || !strings.Contains(err.Error(), "finish and verify pending legacy refunds") {
					t.Fatal("unfinished legacy refund was not fenced before upgrade", err)
				}
				if err = connection.QueryRow(ctx, snapshotSQL).Scan(&after); err != nil {
					t.Fatal(err)
				}
				if before != after {
					t.Fatal("failed upgrade changed old schema, ledger, wallet, refund or migration facts")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if status != "EMPTY" {
				var actualStatus, marker string
				var actualSource, actualPackage uuid.UUID
				var amount, credit, reserved int64
				if err = connection.QueryRow(ctx, `SELECT r.status,r.amount_fen,r.credit_microcredits,r.payment_transaction_id,r.entitlement_package_id,r.entitlement_snapshot->>'migration_source',w.reserved_microcredits FROM refunds r JOIN payment_orders o ON o.id=r.payment_order_id JOIN wallets w ON w.id=o.wallet_id WHERE r.id=$1`, refundID).Scan(&actualStatus, &amount, &credit, &actualSource, &actualPackage, &marker, &reserved); err != nil {
					t.Fatal(err)
				}
				if actualStatus != status || amount != 40 || credit != 400000 || actualSource != source || actualPackage != packageID || marker != "LEGACY_ORDER_REFUND" || reserved != 0 {
					t.Fatal("completed legacy history did not retain exact financial and target facts")
				}
			}
		})
	}
}
