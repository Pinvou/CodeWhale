package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type reviewRefundProvider struct {
	retainedFinancialProvider
	refund          ProviderRefund
	calls           int
	providerID      string
	providerVersion string
}

func (p *reviewRefundProvider) Capabilities() PaymentCapabilities {
	caps := p.retainedFinancialProvider.Capabilities()
	if p.providerID != "" {
		caps.ProviderID = p.providerID
	}
	if p.providerVersion != "" {
		caps.Version = p.providerVersion
	}
	return caps
}

func (p *reviewRefundProvider) Refund(_ context.Context, _, merchant string, amount int64) (ProviderRefund, error) {
	p.calls++
	result := p.refund
	if result.MerchantRefundNo == "" {
		result.MerchantRefundNo = merchant
	}
	if result.AmountFen == 0 {
		result.AmountFen = amount
	}
	return result, nil
}

// Human resolution text cannot substitute for the original provider's proof.
func TestReviewRefundReconcileRequiresTerminalProviderEvidence(t *testing.T) {
	for _, state := range []string{"PENDING", "SUCCESS", "FAILED"} {
		t.Run(state, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			c.AllowNormalRefundCreation(true)
			refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled review")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.pool.Exec(ctx, `UPDATE refunds SET status='MANUAL_REVIEW' WHERE id=$1`, refund.ID); err != nil {
				t.Fatal(err)
			}
			provider := &reviewRefundProvider{refund: ProviderRefund{Status: state, ChannelRefundID: "controlled-original-refund"}}
			c.provider = provider
			result, err := c.processRefund(ctx, refund.ID, true)
			var status string
			var posted int64
			if queryErr := r.pool.QueryRow(ctx, `SELECT r.status,w.posted_microcredits FROM refunds r JOIN payment_orders o ON o.id=r.payment_order_id JOIN wallets w ON w.id=o.wallet_id WHERE r.id=$1`, refund.ID).Scan(&status, &posted); queryErr != nil {
				t.Fatal(queryErr)
			}
			if provider.calls != 1 {
				t.Fatalf("provider calls=%d", provider.calls)
			}
			switch state {
			case "PENDING":
				if !errors.Is(err, ErrConflict) || status != "MANUAL_REVIEW" || posted != 1000000 {
					t.Fatalf("unknown evidence changed money: status=%s posted=%d err=%v", status, posted, err)
				}
			case "SUCCESS":
				if err != nil || result.Status != "SUCCEEDED" || status != "SUCCEEDED" || posted != 0 {
					t.Fatalf("confirmed success did not settle once: %s %d %v", status, posted, err)
				}
				replayed, replayErr := c.processRefund(ctx, refund.ID, true)
				if replayErr != nil || replayed.Status != "SUCCEEDED" || provider.calls != 1 {
					t.Fatalf("terminal replay redispatched: calls=%d err=%v", provider.calls, replayErr)
				}
			case "FAILED":
				if err != nil || result.Status != "FAILED" || status != "FAILED" || posted != 1000000 {
					t.Fatalf("confirmed failure changed balance: %s %d %v", status, posted, err)
				}
				var packageStatus string
				if err := r.pool.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=$1`, pack).Scan(&packageStatus); err != nil || packageStatus != "ACTIVE" {
					t.Fatalf("failed refund did not restore package: %s %v", packageStatus, err)
				}
			}
			var journals int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='REFUND'`).Scan(&journals); err != nil {
				t.Fatal(err)
			}
			want := 0
			if state == "SUCCESS" {
				want = 1
			}
			if journals != want {
				t.Fatalf("refund journal count=%d want=%d", journals, want)
			}
		})
	}
}

func TestReviewRefundReconcileHonorsFinancialFence(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE refunds SET status='MANUAL_REVIEW' WHERE id=$1`, refund.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-competing-fence", 100, nil); err != nil {
		t.Fatal(err)
	}
	provider := &reviewRefundProvider{refund: ProviderRefund{Status: "SUCCESS", ChannelRefundID: "controlled-original-refund"}}
	c.provider = provider
	if _, err = c.processRefund(ctx, refund.ID, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("financial fence accepted refund: %v", err)
	}
	if provider.calls != 0 {
		t.Fatal("financial fence still dispatched provider refund")
	}
}

func TestReviewRefundReconcileRejectsChangedProvider(t *testing.T) {
	for _, mismatch := range []string{"identity", "version"} {
		t.Run(mismatch, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			c.AllowNormalRefundCreation(true)
			refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled review")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.pool.Exec(ctx, `UPDATE refunds SET status='MANUAL_REVIEW' WHERE id=$1`, refund.ID); err != nil {
				t.Fatal(err)
			}
			provider := &reviewRefundProvider{refund: ProviderRefund{Status: "SUCCESS", ChannelRefundID: "unrelated-provider"}}
			if mismatch == "identity" {
				provider.providerID = "another-provider"
			} else {
				provider.providerVersion = "v2"
			}
			c.provider = provider
			if _, err := c.processRefund(ctx, refund.ID, true); err == nil {
				t.Fatal("changed provider was accepted")
			}
			var status string
			var posted int64
			if err := r.pool.QueryRow(ctx, `SELECT r.status,w.posted_microcredits FROM refunds r JOIN payment_orders o ON o.id=r.payment_order_id JOIN wallets w ON w.id=o.wallet_id WHERE r.id=$1`, refund.ID).Scan(&status, &posted); err != nil {
				t.Fatal(err)
			}
			if provider.calls != 0 || status != "MANUAL_REVIEW" || posted != 1000000 {
				t.Fatalf("changed provider changed funds: calls=%d status=%s posted=%d", provider.calls, status, posted)
			}
		})
	}
}

func TestReviewRefundReconcileRejectsMismatchedEvidence(t *testing.T) {
	for _, mismatch := range []string{"merchant", "amount", "missing-success-reference"} {
		t.Run(mismatch, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			c.AllowNormalRefundCreation(true)
			refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled review")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.pool.Exec(ctx, `UPDATE refunds SET status='MANUAL_REVIEW' WHERE id=$1`, refund.ID); err != nil {
				t.Fatal(err)
			}
			proof := ProviderRefund{Status: "SUCCESS", ChannelRefundID: "controlled-provider-reference"}
			switch mismatch {
			case "merchant":
				proof.MerchantRefundNo = "another-original-refund"
			case "amount":
				proof.AmountFen = 99
			case "missing-success-reference":
				proof.ChannelRefundID = ""
			}
			provider := &reviewRefundProvider{refund: proof}
			c.provider = provider
			if _, err = c.processRefund(ctx, refund.ID, true); !errors.Is(err, ErrConflict) {
				t.Fatalf("invalid proof accepted: %v", err)
			}
			var status, dispatch string
			var posted int64
			var leaseCleared bool
			if err := r.pool.QueryRow(ctx, `SELECT r.status,r.dispatch_state,r.dispatch_lease_until IS NULL,w.posted_microcredits FROM refunds r JOIN payment_orders o ON o.id=r.payment_order_id JOIN wallets w ON w.id=o.wallet_id WHERE r.id=$1`, refund.ID).Scan(&status, &dispatch, &leaseCleared, &posted); err != nil {
				t.Fatal(err)
			}
			if provider.calls != 1 || status != "MANUAL_REVIEW" || dispatch != "RESULT_UNKNOWN" || !leaseCleared || posted != 1000000 {
				t.Fatalf("invalid proof changed money or blocked retry: calls=%d status=%s dispatch=%s cleared=%v posted=%d", provider.calls, status, dispatch, leaseCleared, posted)
			}
		})
	}
}
