package billing

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestOrdinaryRefundFrozenLegacyIdentity(t *testing.T) {
	for _, tc := range []struct {
		id, sku, snapshot string
		want              bool
	}{
		{"CNY_1", "", `{}`, true},
		{"CNY_100", "", `{"kind":"LEGACY"}`, true},
		{"CNY_200", "", `{"price_status":"LEGACY"}`, true},
		{"CNY_1", "", `{"price_status":"FROZEN"}`, false},
		{"CNY_1", "", `{"kind":"PLAN"}`, false},
		{"PLAN_TRIAL", "", `{"price_status":"LEGACY"}`, false},
		{"unknown", "", `{}`, false},
		{"CNY_1", "legacy-provider-sku", `{}`, true},
		{"CNY_1", "", `null`, false},
		{"CNY_1", "", `{"product_id":"PLAN_TRIAL"}`, false},
	} {
		t.Run(tc.id+tc.snapshot+tc.sku, func(t *testing.T) {
			if got := ordinaryRefundLegacyOrder(PaymentOrder{ProductID: tc.id, ProviderSKU: tc.sku}, []byte(tc.snapshot), nil); got != tc.want {
				t.Fatalf("admission=%v want %v", got, tc.want)
			}
		})
	}
}

func TestRefundRejectsPlanBeforeFinancialWritesPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	// Controlled producer fixture freezes PLAN commercial facts before payment.
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET product_id='PLAN_TRIAL',product_snapshot=product_snapshot||'{"product_id":"PLAN_TRIAL","price_status":"FROZEN"}'::jsonb WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	order.ProductID = "PLAN_TRIAL"
	provider := c.provider.(*retainedFinancialProvider)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, _, err = c.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "QUERY", uuid.NewString(), nil, provider.result, false, r.now()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var transaction, pack uuid.UUID
	if err = r.pool.QueryRow(ctx, `SELECT t.id,p.id FROM payment_transactions t JOIN entitlement_packages p ON p.source_payment_transaction_id=t.id WHERE t.payment_order_id=$1`, order.ID).Scan(&transaction, &pack); err != nil {
		t.Fatal(err)
	}
	c.AllowNormalRefundCreation(true)
	before, effects := refundRecoveryMoneySnapshot(t, ctx, r), refundEffectSnapshot(t, ctx, r)
	if _, created, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "plan cannot refund"); !errors.Is(err, ErrConflict) || created {
		t.Fatalf("PLAN admitted: %v", err)
	}
	if !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) || effects != refundEffectSnapshot(t, ctx, r) {
		t.Fatal("PLAN rejection changed money or commands")
	}
}

func TestRefundSuspendedMemberAdmissionWithoutEarlierBlockerPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 100, true)
	grant := refundRecoveryGrant(t, r, ctx, order, pack, "")
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, *order.BeneficiaryOrganizationID, grant.MemberUserID, "SUSPENDED", 2); err != nil {
		t.Fatal(err)
	}
	c.AllowNormalRefundCreation(true)
	before, effects := refundRecoveryMoneySnapshot(t, ctx, r), refundEffectSnapshot(t, ctx, r)
	if _, created, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "suspended member cannot refund"); !errors.Is(err, ErrConflict) || created {
		t.Fatalf("suspended grant admitted: %v", err)
	}
	if !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) || effects != refundEffectSnapshot(t, ctx, r) {
		t.Fatal("rejection mutated money or commands")
	}
	// With only this lifecycle suspension removed, the very same package is
	// admissible: no earlier order/component/base/freeze condition masks it.
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, *order.BeneficiaryOrganizationID, grant.MemberUserID, "ACTIVE", 3); err != nil {
		t.Fatal(err)
	}
	if _, created, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "active member full legacy refund"); err != nil || !created {
		t.Fatalf("positive admission control: %v", err)
	}
}

type reviewDispatchFailureProvider struct {
	retainedFinancialProvider
	calls, queries int
}

func (p *reviewDispatchFailureProvider) Refund(context.Context, string, string, int64) (ProviderRefund, error) {
	p.calls++
	return ProviderRefund{}, errors.New("controlled unknown result")
}
func (p *reviewDispatchFailureProvider) QueryRefund(context.Context, string, string) (ProviderRefund, error) {
	p.queries++
	return ProviderRefund{}, errors.New("controlled unknown result")
}

func TestRefundStalePreflightCannotReclaimUnknownDispatchPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled dispatch race")
	if err != nil {
		t.Fatal(err)
	}
	// Worker B completes preflight then pauses. Worker A wins the same CAS,
	// dispatches, and clears its lease after a quick unknown-result failure.
	if _, handled, err := c.recoverDispatchedRefund(ctx, refund); err != nil || handled {
		t.Fatalf("initial preflight: %v", err)
	}
	provider := &reviewDispatchFailureProvider{}
	c.provider = provider
	if err := c.ProcessRefund(ctx, refund.ID); !errors.Is(err, ErrPaymentProvider) {
		t.Fatalf("first dispatch: %v", err)
	}
	if err := c.claimUnstartedRefund(ctx, refund.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale worker reclaimed possible dispatch: %v", err)
	}
	if err := c.ProcessRefund(ctx, refund.ID); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || provider.queries != 1 {
		t.Fatalf("calls=%d queries=%d", provider.calls, provider.queries)
	}
}
