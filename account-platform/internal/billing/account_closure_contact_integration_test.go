package billing

import (
	"testing"

	"github.com/google/uuid"
)

func TestAccountClosureIsolationScrubsContactWithoutFinancialMutation(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
	var payer uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT payer_user_id FROM payment_orders WHERE id=$1`, order.ID).Scan(&payer); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET mobile_e164='controlled-closure-contact' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var raw []byte
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('order',(SELECT to_jsonb(p)-'mobile_e164' FROM payment_orders p WHERE id=$1),'transactions',(SELECT jsonb_agg(to_jsonb(t) ORDER BY t.id) FROM payment_transactions t WHERE payment_order_id=$1),'packages',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM entitlement_packages p WHERE payment_order_id=$1),'wallet',(SELECT to_jsonb(w) FROM wallets w WHERE id=$2),'journal',(SELECT jsonb_agg(to_jsonb(j) ORDER BY j.id) FROM journal_entries j))`, order.ID, order.WalletID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	before := snapshot()
	closureID := uuid.New()
	first, err := r.IsolateAccountClosedGrants(ctx, closureID, payer, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertScrubbed := func() {
		var absent bool
		if err := r.pool.QueryRow(ctx, `SELECT mobile_e164 IS NULL FROM payment_orders WHERE id=$1`, order.ID).Scan(&absent); err != nil || !absent || snapshot() != before {
			t.Fatal("contact cleanup changed financial facts or left contact", absent, err)
		}
	}
	assertScrubbed()
	// The immutable receipt remains authoritative; a resumed metadata phase
	// still scrubs rather than skipping work because isolation already committed.
	if _, err = r.pool.Exec(ctx, `UPDATE payment_orders SET mobile_e164='controlled-replayed-contact' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	replay, err := r.IsolateAccountClosedGrants(ctx, closureID, payer, 2)
	if err != nil || replay.ClosureRequestID != first.ClosureRequestID || replay.IsolatedGrantCount != first.IsolatedGrantCount {
		t.Fatal("isolation receipt replay", replay, err)
	}
	assertScrubbed()
}
