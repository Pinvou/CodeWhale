package billing

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"testing"
	"time"
)

type controlledArchiveProvider struct {
	legacyMappingProvider
	reply ProviderOrder
}

func (p *controlledArchiveProvider) Capabilities() payment.Capabilities {
	c := p.legacyMappingProvider.Capabilities()
	c.AuthoritativeUnpaidTerminal = true
	return c
}
func (p *controlledArchiveProvider) Get(context.Context, string) (ProviderOrder, error) {
	return p.reply, nil
}
func TestPostgresLateClaimFailedQueryCannotConfirmUnpaid(t *testing.T) {
	r, order, user := lateClaimFixture(t)
	ctx := context.Background()
	item, e := r.LatePaymentReview(ctx, func() uuid.UUID {
		var id uuid.UUID
		if e := r.pool.QueryRow(ctx, `SELECT id FROM late_payment_review_cases WHERE payment_order_id=$1`, order).Scan(&id); e != nil {
			t.Fatal(e)
		}
		return id
	}())
	if e != nil {
		t.Fatal(e)
	}
	actor := uuid.New()
	if _, e = r.ClaimLatePaymentReview(ctx, item.ID, actor); e != nil {
		t.Fatal(e)
	}
	now := r.now().Add(9 * 24 * time.Hour)
	r.now = func() time.Time { return now }
	provider := &controlledArchiveProvider{}
	if e = r.pool.QueryRow(ctx, `SELECT merchant_order_no,channel,amount_fen,currency FROM payment_orders WHERE id=$1`, order).Scan(&provider.reply.MerchantOrderNo, &provider.reply.Channel, &provider.reply.AmountFen, &provider.reply.Currency); e != nil {
		t.Fatal(e)
	}
	provider.reply.Status = "CLOSED"
	c := &PaymentCoordinator{repository: r, provider: provider, now: r.now}
	queried, e := c.SyncReviewedOrder(ctx, order)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.RecordLatePaymentReviewQuery(ctx, item.ID, actor, queried, nil); e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Minute)
	if _, e = r.AddLatePaymentClaim(ctx, order, user, "controlled newly asserted payment claim"); e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Minute)
	if _, e = r.RecordLatePaymentReviewQuery(ctx, item.ID, actor, PaymentOrder{}, errors.New("controlled provider unavailable")); e != nil {
		t.Fatal(e)
	}
	archived, e := r.ArchiveLatePaymentReview(ctx, item.ID, actor)
	if e == nil {
		t.Fatalf("failed query after newer claim archived unconfirmed case: status=%s", archived.ReviewStatus)
	}
	if !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}

type controlledEmptyBill struct{ legacyMappingProvider }

func (p *controlledEmptyBill) Capabilities() payment.Capabilities {
	c := p.legacyMappingProvider.Capabilities()
	c.BillSupported = true
	return c
}
func (p *controlledEmptyBill) Bill(context.Context, string, time.Time) ([]payment.BillEntry, error) {
	return []payment.BillEntry{}, nil
}
func controlledReconCase(t *testing.T) (*PostgresRepository, Wallet, uuid.UUID) {
	r, w, source := forcedRefundPaidSource(t)
	ctx := context.Background()
	var paid time.Time
	if e := r.pool.QueryRow(ctx, `SELECT provider_paid_at FROM payment_transactions WHERE id=$1`, source).Scan(&paid); e != nil {
		t.Fatal(e)
	}
	c := &PaymentCoordinator{repository: r, provider: &controlledEmptyBill{}, now: time.Now}
	run, e := c.RunAdminReconciliation(ctx, uuid.New(), "ALIPAY", normalizedReconciliationBusinessDate(paid.In(time.FixedZone("Shanghai", 8*3600))))
	if e != nil || run.CaseCount != 1 {
		t.Fatal("controlled actual reconciliation case absent", e)
	}
	var id uuid.UUID
	if e = r.pool.QueryRow(ctx, `SELECT id FROM reconciliation_cases WHERE reconciliation_run_id=$1`, run.ID).Scan(&id); e != nil {
		t.Fatal(e)
	}
	return r, w, id
}
func TestPostgresInReviewReconciliationBlocksOrganizationDeletion(t *testing.T) {
	r, w, id := controlledReconCase(t)
	ctx := context.Background()
	hold, e := r.Reserve(ctx, uuid.New(), w.ID, w.PostedMicrocredits, nil, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Commit(ctx, hold.UsageRequestID, hold.ID, w.PostedMicrocredits, map[string]any{"controlled_usage": true}); e != nil {
		t.Fatal(e)
	}
	if _, e = r.ApplyOrganizationLifecycle(ctx, w.OwnerID, "DISABLED", 1, false); e != nil {
		t.Fatal(e)
	}
	before, e := r.ApplyOrganizationLifecycle(ctx, w.OwnerID, "DELETED", 2, true)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, b := range before.Blockers {
		if b.Code == "OPEN_RECONCILIATION_DIFFERENCE" {
			found = true
		}
	}
	if !found {
		t.Fatal("real open reconciliation never blocked deletion")
	}
	if _, e = r.UpdateReconciliationCase(ctx, id, ReconciliationCaseCommand{Action: "CLAIM", ActorID: uuid.New(), Reason: "controlled review assignment", IdempotencyKey: uuid.NewString()}); e != nil {
		t.Fatal(e)
	}
	after, e := r.ApplyOrganizationLifecycle(ctx, w.OwnerID, "DELETED", 2, true)
	if e != nil {
		t.Fatal(e)
	}
	for _, b := range after.Blockers {
		if b.Code == "OPEN_RECONCILIATION_DIFFERENCE" {
			return
		}
	}
	t.Fatalf("claimed unresolved case no longer protects deletion: ready=%v blockers=%v", after.Ready, after.Blockers)
}
func TestPostgresReconciliationClaimReplayBindsActor(t *testing.T) {
	r, _, id := controlledReconCase(t)
	ctx := context.Background()
	key := uuid.NewString()
	a, b := uuid.New(), uuid.New()
	command := ReconciliationCaseCommand{Action: "CLAIM", ActorID: a, Reason: "controlled review assignment", IdempotencyKey: key}
	if _, e := r.UpdateReconciliationCase(ctx, id, command); e != nil {
		t.Fatal(e)
	}
	command.ActorID = b
	reply, e := r.UpdateReconciliationCase(ctx, id, command)
	if e == nil {
		t.Fatalf("foreign actor reused claim receipt: priorOwner=%v", reply.OwnerID != nil && *reply.OwnerID == a)
	}
	if !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}

func TestPostgresLateClaimSuccessfulRepeatQueryAllowsArchive(t *testing.T) {
	r, order, user := lateClaimFixture(t)
	ctx := context.Background()
	item, e := r.LatePaymentReview(ctx, func() uuid.UUID {
		var id uuid.UUID
		if e := r.pool.QueryRow(ctx, `SELECT id FROM late_payment_review_cases WHERE payment_order_id=$1`, order).Scan(&id); e != nil {
			t.Fatal(e)
		}
		return id
	}())
	if e != nil {
		t.Fatal(e)
	}
	actor := uuid.New()
	if _, e = r.ClaimLatePaymentReview(ctx, item.ID, actor); e != nil {
		t.Fatal(e)
	}
	now := r.now().Add(9 * 24 * time.Hour)
	r.now = func() time.Time { return now }
	provider := &controlledArchiveProvider{}
	if e = r.pool.QueryRow(ctx, `SELECT merchant_order_no,channel,amount_fen,currency FROM payment_orders WHERE id=$1`, order).Scan(&provider.reply.MerchantOrderNo, &provider.reply.Channel, &provider.reply.AmountFen, &provider.reply.Currency); e != nil {
		t.Fatal(e)
	}
	provider.reply.Status = "CLOSED"
	c := &PaymentCoordinator{repository: r, provider: provider, now: r.now}
	queried, e := c.SyncReviewedOrder(ctx, order)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.RecordLatePaymentReviewQuery(ctx, item.ID, actor, queried, nil); e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Minute)
	if _, e = r.AddLatePaymentClaim(ctx, order, user, "controlled newly asserted payment claim"); e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Minute)
	queried, e = c.SyncReviewedOrder(ctx, order)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.RecordLatePaymentReviewQuery(ctx, item.ID, actor, queried, nil); e != nil {
		t.Fatal(e)
	}
	archived, e := r.ArchiveLatePaymentReview(ctx, item.ID, actor)
	if e != nil || archived.ReviewStatus != "ARCHIVED_CONFIRMED_UNPAID" {
		t.Fatalf("actual successful repeated authoritative query did not review newer claim: status=%s err=%v", archived.ReviewStatus, e)
	}
}
