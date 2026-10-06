package billing

import (
	"context"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

type journalReviewProvider struct{ legacyMappingProvider }

func (*journalReviewProvider) Capabilities() payment.Capabilities {
	c := (&legacyMappingProvider{}).Capabilities()
	c.ReviewLatePayment = true
	return c
}
func (*journalReviewProvider) Create(_ context.Context, order payment.Order, _ string) (payment.ProviderOrder, error) {
	return payment.ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Mobile: order.Mobile, ProviderSKU: order.ExternalSKU, Status: "PENDING"}, nil
}

// Exercises production package expiry and the audited repair convergence
// transaction with controlled provider evidence; not an Admin approval test.
func TestPostgresPreservedExpiryAndRepairJournalTypes(t *testing.T) {
	for _, operation := range []string{"expiry", "repair"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			r := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
			wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			mappings, err := r.ActiveExternalProducts(ctx, "development_simulator", "development-simulator-v1", true)
			if err != nil {
				t.Fatal(err)
			}
			var provider payment.Provider = &legacyMappingProvider{}
			if operation == "repair" {
				provider = &journalReviewProvider{}
			}
			p, err := NewPaymentCoordinatorWithProvider(r, provider, mappings, "test")
			if err != nil {
				t.Fatal(err)
			}
			p.AllowTentativePrices(true)
			order, _, err := p.CreateOrder(ctx, wallet.OwnerID, wallet.ID, "PLAN_TRIAL", "ALIPAY", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			kind := "EXPIRE"
			if operation == "expiry" {
				r.now = func() time.Time { return time.Now().UTC().Add(31 * 24 * time.Hour) }
				if count, err := r.ExpirePackages(ctx, 100); err != nil || count != 1 {
					t.Fatal("actual expiry failed", count, err)
				}
				current, err := r.GetWallet(ctx, wallet.ID)
				if err != nil || current.PostedMicrocredits != 0 || current.ReservedMicrocredits != 0 {
					t.Fatal("expiry did not conserve wallet", err)
				}
			} else {
				kind = "REPAIR_CREDIT"
				paidAt, transactionID := order.ExpiresAt.Add(time.Second), "controlled-repair-"+order.ID.String()
				fact := ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: &paidAt, ChannelTransactionID: &transactionID}
				for _, repair := range []bool{false, true, true} {
					tx, err := r.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					current, err := getPaymentOrderForUpdate(ctx, tx, order.MerchantOrderNo)
					if err == nil {
						_, _, err = p.mergeSuccessfulPaymentWithinTransaction(ctx, tx, current, "MANUAL_REPAIR", "controlled-repair-evidence", []byte(`{"confirmed":true}`), fact, repair, paidAt.Add(time.Minute))
					}
					if err == nil {
						err = tx.Commit(ctx)
					} else {
						_ = tx.Rollback(ctx)
					}
					if err != nil {
						t.Fatal("actual repair convergence failed", repair, err)
					}
				}
				current, err := r.GetWallet(ctx, wallet.ID)
				if err != nil || current.PostedMicrocredits != order.CreditMicrocredits || current.ReservedMicrocredits != 0 {
					t.Fatal("repair duplicated or omitted credit", err)
				}
			}
			var count int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type=$1`, kind).Scan(&count); err != nil || count != 1 {
				t.Fatal("preserved journal type or replay lost", kind, count, err)
			}
		})
	}
}
