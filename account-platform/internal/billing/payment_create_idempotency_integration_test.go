package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Lawful catalog/admission and PostgreSQL; only provider scheduling is controlled.
func TestRetainedCreateSameKeyExternalWait(t *testing.T) {
	for _, name := range []string{"same-facts", "changed-product", "changed-owner"} {
		changed := name != "same-facts"
		t.Run(name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			owner := uuid.New()
			wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", owner)
			if err != nil {
				t.Fatal(err)
			}
			product := retainedSameKeyProduct(t, r, ctx, owner)
			secondProduct := product
			if name == "changed-product" {
				secondProduct = retainedSameKeyProduct(t, r, ctx, owner)
			}
			provider := &retainedSameKeyProvider{retainedFinancialProvider: &retainedFinancialProvider{}, entered: make(chan struct{}), release: make(chan struct{})}
			c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
			if err != nil {
				t.Fatal(err)
			}
			c.AllowTentativePrices(true)
			type result struct {
				order PaymentOrder
				err   error
			}
			finished := make(chan result, 1)
			key := "independent-create-wait-" + uuid.NewString()
			go func() {
				o, _, e := c.CreateOrder(ctx, owner, wallet.ID, product, "ALIPAY", key)
				finished <- result{o, e}
			}()
			select {
			case <-provider.entered:
			case <-ctx.Done():
				t.Fatal("provider wait not reached")
			}
			secondOwner, secondWallet := owner, wallet.ID
			if name == "changed-owner" {
				secondOwner = uuid.New()
				w, _, e := r.EnsureWallet(ctx, "PERSONAL", secondOwner)
				if e != nil {
					close(provider.release)
					<-finished
					t.Fatal(e)
				}
				secondWallet = w.ID
			}
			second, _, secondErr := c.CreateOrder(ctx, secondOwner, secondWallet, secondProduct, "ALIPAY", key)
			close(provider.release)
			var first result
			select {
			case first = <-finished:
			case <-ctx.Done():
				t.Fatal("first call failed to finish")
			}
			replay, _, replayErr := c.CreateOrder(ctx, owner, wallet.ID, product, "ALIPAY", key)
			if first.err != nil {
				t.Errorf("first same-key owner could not persist result after external wait: %v", first.err)
			}
			if changed {
				if secondErr == nil {
					t.Errorf("different immutable product facts accepted for in-flight same key: secondID=%s err=%v calls=%d", second.ID, secondErr, provider.calls.Load())
				}
			} else {
				if secondErr != nil {
					t.Errorf("same facts unexpectedly failed: %v", secondErr)
				}
				if first.err == nil {
					a, _ := json.Marshal(first.order)
					b, _ := json.Marshal(second)
					d, _ := json.Marshal(replay)
					if replayErr != nil || !bytes.Equal(a, b) || !bytes.Equal(b, d) {
						t.Errorf("first delivered receipt was not preserved for first/replay: replayErr=%v", replayErr)
					}
				}
			}
			if provider.calls.Load() != 1 {
				t.Errorf("one logical creation dispatched multiple provider calls: %d", provider.calls.Load())
			}
		})
	}
}

func retainedSameKeyProduct(t *testing.T, r *PostgresRepository, ctx context.Context, owner uuid.UUID) string {
	t.Helper()
	input := validRechargeProductForEditTests()
	input.ProductID = "WAIT_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	input.Mapping.ProviderID = "controlled"
	input.Mapping.ExternalSKUBOM = input.ProductID
	item, e := r.CreateRechargeProduct(ctx, input)
	if e != nil {
		t.Fatal(e)
	}
	verification, _, e := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, owner, nil)
	if e != nil {
		t.Fatal(e)
	}
	evidence, e := r.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "controlled", "controlled-admission", "independent-private-probe", time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &evidence)
	if e != nil {
		t.Fatal(e)
	}
	return input.ProductID
}

type retainedSameKeyProvider struct {
	*retainedFinancialProvider
	calls            atomic.Int32
	entered, release chan struct{}
}

func (p *retainedSameKeyProvider) Create(ctx context.Context, input payment.Order, _ string) (ProviderOrder, error) {
	if p.calls.Add(1) == 1 {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return ProviderOrder{}, ctx.Err()
		}
	}
	return ProviderOrder{MerchantOrderNo: input.MerchantOrderNo, Status: "PENDING", PaymentURL: "https://provider.invalid/pay/" + input.MerchantOrderNo}, nil
}
