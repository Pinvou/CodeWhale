package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type organizationLifecycleControlledProvider struct {
	*retainedFinancialProvider
	calls    atomic.Int32
	queries  atomic.Int32
	lose     atomic.Bool
	mutex    sync.Mutex
	response ProviderOrder
}

func (p *organizationLifecycleControlledProvider) Create(_ context.Context, in payment.Order, _ string) (ProviderOrder, error) {
	p.calls.Add(1)
	out := ProviderOrder{MerchantOrderNo: in.MerchantOrderNo, Channel: in.Channel, AmountFen: in.AmountFen, Currency: in.Currency, ExternalCustomerKey: in.MerchantCustomerKey, ExternalSKU: in.ExternalSKU, Status: "PENDING", PaymentURL: "https://controlled.invalid/pay/" + in.MerchantOrderNo}
	p.mutex.Lock()
	p.response = out
	p.mutex.Unlock()
	if p.lose.Swap(false) {
		return ProviderOrder{}, io.ErrUnexpectedEOF
	}
	return out, nil
}
func (p *organizationLifecycleControlledProvider) Get(_ context.Context, key string) (ProviderOrder, error) {
	p.queries.Add(1)
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if p.response.MerchantOrderNo != key {
		return ProviderOrder{}, ErrNotFound
	}
	return p.response, nil
}

// Actual public schema/runtime and concurrent lifecycle transactions. The
// provider scheduling/response is controlled; no external money is collected.
func TestOrganizationPlanPurchaseLifecycleFencePostgres(t *testing.T) {
	f06WalletActors(t)
	r, ctx := retainedFinanceRepository(t)
	t.Run("new-order-wallet-lock-makes-deletion-see-original-pending", func(t *testing.T) {
		owner, org := uuid.New(), uuid.New()
		if _, e := r.ApplyOrganizationLifecycle(ctx, org, "ACTIVE", 1, false); e != nil {
			t.Fatal(e)
		}
		wallet, _, e := r.EnsureWallet(ctx, "ORGANIZATION", org)
		if e != nil {
			t.Fatal(e)
		}
		product := retainedSameKeyProduct(t, r, ctx, owner)
		provider := &organizationLifecycleControlledProvider{retainedFinancialProvider: &retainedFinancialProvider{}}
		c, e := NewPaymentCoordinatorWithProvider(r, provider, "test")
		if e != nil {
			t.Fatal(e)
		}
		c.AllowTentativePrices(true)
		const barrierKey int64 = 742031902645
		ddl := fmt.Sprintf(`CREATE FUNCTION owned_org_order_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.beneficiary_organization_id='%s'::uuid THEN PERFORM pg_advisory_xact_lock(%d); END IF; RETURN NEW; END $$; CREATE TRIGGER owned_org_order_barrier BEFORE INSERT ON payment_orders FOR EACH ROW EXECUTE FUNCTION owned_org_order_barrier()`, org, barrierKey)
		if _, e = retainedFinanceOwnerExec(t, r, ctx, ddl); e != nil {
			t.Fatal(e)
		}
		barrier, e := r.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer barrier.Rollback(context.Background())
		if _, e = barrier.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, barrierKey); e != nil {
			t.Fatal(e)
		}
		type answer struct {
			order PaymentOrder
			err   error
		}
		done := make(chan answer, 1)
		key := "race-original-organization-key-0001"
		go func() {
			o, _, e := c.CreateOrganizationMobileOrder(ctx, owner, wallet.ID, org, product, "ALIPAY", "+8613800138000", key)
			done <- answer{o, e}
		}()
		waiting := false
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if e = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND wait_event='advisory' AND query LIKE '%INSERT INTO payment_orders%')`).Scan(&waiting); e != nil {
				t.Fatal(e)
			}
			if waiting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("create did not reach actual pre-insert barrier")
		}
		type lifecycleAnswer struct {
			assessment OrganizationLifecycleAssessment
			err        error
		}
		disabledDone := make(chan lifecycleAnswer, 1)
		go func() {
			a, e := r.ApplyOrganizationLifecycle(ctx, org, "DISABLED", 2, false)
			disabledDone <- lifecycleAnswer{a, e}
		}()
		serialized := false
		deadline = time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if e = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%owner_type=''ORGANIZATION''%FOR UPDATE%')`).Scan(&serialized); e != nil {
				t.Fatal(e)
			}
			if serialized {
				break
			}
			select {
			case a := <-disabledDone:
				barrier.Rollback(ctx)
				<-done
				t.Fatalf("lifecycle escaped new-order wallet fence: %+v", a)
			default:
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !serialized {
			t.Fatal("actual lifecycle row-lock serialization not observed")
		}
		if e = barrier.Rollback(ctx); e != nil {
			t.Fatal(e)
		}
		got := <-done
		if got.err != nil || got.order.ID == uuid.Nil || provider.calls.Load() != 1 {
			t.Fatal("original admitted create", got, provider.calls.Load())
		}
		disabled := <-disabledDone
		if disabled.err != nil || !disabled.assessment.Ready {
			t.Fatal("disable after serialized order", disabled)
		}
		deleted, e := r.ApplyOrganizationLifecycle(ctx, org, "DELETED", 3, false)
		if e != nil || deleted.Ready {
			t.Fatal("deletion ignored committed order", deleted, e)
		}
		block := false
		for _, b := range deleted.Blockers {
			if b.Code == "UNFINISHED_PAYMENT" && b.ResourceID == got.order.ID.String() {
				block = true
			}
		}
		frozen, e := r.GetWallet(ctx, wallet.ID)
		if e != nil || !block || frozen.Status != "FROZEN" {
			t.Fatal("frozen exact pending blocker", deleted, frozen, e)
		}
		replay, created, e := c.CreateOrganizationMobileOrder(ctx, owner, wallet.ID, org, product, "ALIPAY", "+8613800138000", key)
		originalJSON, _ := json.Marshal(got.order)
		replayedJSON, _ := json.Marshal(replay)
		if e != nil || created || replay.ID != got.order.ID || !bytes.Equal(originalJSON, replayedJSON) || provider.calls.Load() != 1 || provider.queries.Load() != 0 {
			t.Fatal("original key recovery incorrectly rejected or redispatched", replay, e)
		}
	})
	t.Run("closed-lifecycle-first-rejects-new-order-without-provider-or-money", func(t *testing.T) {
		owner, org := uuid.New(), uuid.New()
		if _, e := r.ApplyOrganizationLifecycle(ctx, org, "ACTIVE", 1, false); e != nil {
			t.Fatal(e)
		}
		wallet, _, e := r.EnsureWallet(ctx, "ORGANIZATION", org)
		if e != nil {
			t.Fatal(e)
		}
		if a, e := r.ApplyOrganizationLifecycle(ctx, org, "DISABLED", 2, false); e != nil || !a.Ready {
			t.Fatal(a, e)
		}
		if a, e := r.ApplyOrganizationLifecycle(ctx, org, "DELETED", 3, false); e != nil || !a.Ready {
			t.Fatal(a, e)
		}
		product := retainedSameKeyProduct(t, r, ctx, owner)
		provider := &organizationLifecycleControlledProvider{retainedFinancialProvider: &retainedFinancialProvider{}}
		c, e := NewPaymentCoordinatorWithProvider(r, provider, "test")
		if e != nil {
			t.Fatal(e)
		}
		c.AllowTentativePrices(true)
		before := refundRecoveryMoneySnapshot(t, ctx, r)
		if _, _, e = c.CreateOrganizationMobileOrder(ctx, owner, wallet.ID, org, product, "ALIPAY", "+8613800138000", "closed-first-original-key-0001"); !errors.Is(e, ErrConflict) {
			t.Fatal("closed first admitted", e)
		}
		var count int
		if e = r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_orders WHERE wallet_id=$1`, wallet.ID).Scan(&count); e != nil || count != 0 || provider.calls.Load() != 0 || !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
			t.Fatal("closed first effects", count, e)
		}
	})
	t.Run("frozen-wallet-original-unknown-binding-still-queries-same-order", func(t *testing.T) {
		owner, org := uuid.New(), uuid.New()
		if _, e := r.ApplyOrganizationLifecycle(ctx, org, "ACTIVE", 1, false); e != nil {
			t.Fatal(e)
		}
		wallet, _, e := r.EnsureWallet(ctx, "ORGANIZATION", org)
		if e != nil {
			t.Fatal(e)
		}
		product := retainedSameKeyProduct(t, r, ctx, owner)
		provider := &organizationLifecycleControlledProvider{retainedFinancialProvider: &retainedFinancialProvider{}}
		c, e := NewPaymentCoordinatorWithProvider(r, provider, "test")
		if e != nil {
			t.Fatal(e)
		}
		c.AllowTentativePrices(true)
		key := "original-unknown-frozen-wallet-key-0001"
		provider.lose.Store(true)
		if _, _, e = c.CreateOrganizationMobileOrder(ctx, owner, wallet.ID, org, product, "ALIPAY", "+8613800138000", key); e == nil {
			t.Fatal("unknown provider ACK not observed")
		}
		var original uuid.UUID
		var state string
		if e = r.pool.QueryRow(ctx, `SELECT id,provider_call_status FROM payment_orders WHERE wallet_id=$1`, wallet.ID).Scan(&original, &state); e != nil || state != "RESULT_UNKNOWN" {
			t.Fatal("unknown binding missing", state, e)
		}
		if a, e := r.ApplyOrganizationLifecycle(ctx, org, "DISABLED", 2, false); e != nil || !a.Ready {
			t.Fatal(a, e)
		}
		got, created, e := c.CreateOrganizationMobileOrder(ctx, owner, wallet.ID, org, product, "ALIPAY", "+8613800138000", key)
		if e != nil || created || got.ID != original || got.BeneficiaryOrganizationID == nil || *got.BeneficiaryOrganizationID != org || provider.calls.Load() != 1 || provider.queries.Load() != 1 {
			t.Fatal("frozen unknown original key recovery", got, e, provider.calls.Load(), provider.queries.Load())
		}
	})

}
