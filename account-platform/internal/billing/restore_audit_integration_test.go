package billing

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func runLedgerRestoreAudit(t *testing.T, repo *PostgresRepository) map[string]int64 {
	t.Helper()
	body, err := os.ReadFile("../../deploy/runbooks/sql/billing-ledger-audit.sql")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	connection, err := repo.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	defer connection.Exec(ctx, "ROLLBACK")
	checks := map[string]int64{}
	// Execute the actual read-only runbook statements on one PostgreSQL session;
	// remove only psql's ON_ERROR_STOP directive, not SQL or transaction controls.
	for _, statement := range strings.Split(strings.ReplaceAll(string(body), "\\set ON_ERROR_STOP on", ""), ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		rows, err := connection.Query(ctx, statement)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			var count int64
			if err := rows.Scan(&name, &count); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			checks[name] = count
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	expected := []string{"unbalanced_journal_entries", "wallet_posted_snapshot_mismatch", "wallet_reserved_snapshot_mismatch", "wallet_receivable_snapshot_mismatch", "receivable_amount_mismatch", "receivable_allocation_mismatch", "chargeback_package_mismatch", "entitlement_component_conservation_mismatch", "entitlement_component_lot_mismatch", "entitlement_package_projection_mismatch", "entitlement_hold_allocation_mismatch", "expired_active_holds", "usage_hold_allocation_mismatch", "usage_hold_latest_lease_mismatch", "paid_orders_not_credited"}
	if len(checks) != len(expected) {
		t.Fatal("runbook did not execute all expected audit queries")
	}
	for _, name := range expected {
		if _, exists := checks[name]; !exists {
			t.Fatalf("runbook did not execute %s", name)
		}
	}
	return checks
}

func TestPostgresRestoreAuditCountsReviewHoldsAndPendingRefunds(t *testing.T) {
	ctx := context.Background()
	p, wallet, user := paymentFixture(t)
	first := creditedOrder(t, p, wallet, user, "restore-audit-first-order")
	source, packageID := refundTargetFixture(t, p, first.ID)
	creditedOrder(t, p, wallet, user, "restore-audit-second-order")
	if _, _, err := p.reserveRefund(ctx, first.ID, user, source, packageID, "restore-audit-refund", "restore-audit-hash", "fixture unused recharge"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p.repository.now = func() time.Time { return now }
	if _, err := p.repository.Reserve(ctx, uuid.New(), wallet.ID, 100, nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if count, err := p.repository.ExpireHolds(ctx, 100); err != nil || count != 1 {
		t.Fatal("fixture hold did not enter review")
	}
	for name, count := range runLedgerRestoreAudit(t, p.repository) {
		if count != 0 {
			t.Fatalf("valid review/refund funds reported %s=%d", name, count)
		}
	}
	// Only this isolated fixture is corrupted to prove the check detects a real
	// snapshot mismatch rather than hiding all nonzero reservations.
	if _, err := p.repository.pool.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits+1 WHERE id=$1`, wallet.ID); err != nil {
		t.Fatal(err)
	}
	if runLedgerRestoreAudit(t, p.repository)["wallet_reserved_snapshot_mismatch"] != 1 {
		t.Fatal("runbook missed corrupted reservation snapshot")
	}
}

func TestPostgresRestoreAuditRetainsMigratedLegacyRefundReservations(t *testing.T) {
	ctx := context.Background()
	p, wallet, user := paymentFixture(t)
	order := creditedOrder(t, p, wallet, user, "restore-audit-legacy-order")
	source, packageID := refundTargetFixture(t, p, order.ID)
	refund, _, err := p.reserveRefund(ctx, order.ID, user, source, packageID, "restore-audit-legacy-refund", "restore-audit-legacy-hash", "fixture unused recharge")
	if err != nil {
		t.Fatal(err)
	}
	// This controlled SQL fixture represents the persisted migration-27 marker
	// and old wallet reservation. It does not claim to execute a historical
	// partial-refund API, the actual upgrade, or provider refund recovery.
	if _, err := p.repository.pool.Exec(ctx, `UPDATE refunds SET entitlement_snapshot=jsonb_build_object('migration_source','LEGACY_ORDER_REFUND') WHERE id=$1`, refund.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.repository.pool.Exec(ctx, `UPDATE wallets SET reserved_microcredits=$2 WHERE id=$1`, wallet.ID, refund.CreditMicrocredits); err != nil {
		t.Fatal(err)
	}
	for name, count := range runLedgerRestoreAudit(t, p.repository) {
		if count != 0 {
			t.Fatalf("persisted legacy refund reservation falsely reported %s=%d", name, count)
		}
	}
	if _, err := p.repository.pool.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits-1 WHERE id=$1`, wallet.ID); err != nil {
		t.Fatal(err)
	}
	if runLedgerRestoreAudit(t, p.repository)["wallet_reserved_snapshot_mismatch"] != 1 {
		t.Fatal("runbook ignored a corrupted legacy refund reservation")
	}
}
