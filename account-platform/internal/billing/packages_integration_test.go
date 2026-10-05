package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func packageFixture(t *testing.T) (*PaymentCoordinator, Wallet, uuid.UUID, PaymentOrder, uuid.UUID) {
	t.Helper()
	p, wallet, _ := paymentFixture(t)
	user := wallet.OwnerID
	p.AllowTentativePrices(true)
	ctx := context.Background()
	order, _, err := p.CreateOrder(ctx, user, wallet.ID, "PLAN_TRIAL", "ALIPAY", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	paidAt, transaction := p.now(), uuid.NewString()
	order, err = p.applyQueriedPayment(ctx, order.ID, ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: &paidAt, ChannelTransactionID: &transaction})
	if err != nil {
		t.Fatal(err)
	}
	var packageID uuid.UUID
	if err := p.repository.pool.QueryRow(ctx, `SELECT id FROM entitlement_packages WHERE payment_order_id=$1`, order.ID).Scan(&packageID); err != nil {
		t.Fatal(err)
	}
	return p, wallet, user, order, packageID
}

func TestPostgresPackageMixedBalanceCommitReleaseWaiverAndExpiry(t *testing.T) {
	for _, operation := range []string{"commit", "release", "waiver"} {
		t.Run(operation, func(t *testing.T) {
			p, wallet, _, order, packageID := packageFixture(t)
			r := p.repository
			ctx := context.Background()
			if _, err := r.Credit(ctx, wallet.ID, "ordinary", 1000, true); err != nil {
				t.Fatal(err)
			}
			hold, err := r.Reserve(ctx, uuid.New(), wallet.ID, order.CreditMicrocredits+600, nil, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			var reserved, remaining, used int64
			if err := r.pool.QueryRow(ctx, `SELECT points_reserved,points_remaining,points_used FROM entitlement_packages WHERE id=$1`, packageID).Scan(&reserved, &remaining, &used); err != nil || reserved != order.CreditMicrocredits || remaining != 0 || used != 0 {
				t.Fatal("reserve did not allocate package before ordinary credit")
			}
			if operation == "commit" {
				_, err = r.Commit(ctx, hold.UsageRequestID, hold.ID, order.CreditMicrocredits+400, map[string]any{"input_tokens": 1})
			} else if operation == "release" {
				_, err = r.Release(ctx, hold.UsageRequestID, hold.ID, "upstream_not_started")
			} else {
				var authority WaiverAuthority
				authority, err = r.ResolveUsageWaiver(ctx, hold.ID, hold.UsageRequestID, "operator final waiver", "controlled-actor")
				if err == nil && authority.Hold.Status != "RELEASED" {
					t.Fatal("waiver lost terminal authority")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if operation == "waiver" {
				if _, err := r.ResolveUsageWaiver(ctx, hold.ID, hold.UsageRequestID, "retry must not change reason", "different-actor"); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.pool.QueryRow(ctx, `SELECT points_reserved,points_remaining,points_used FROM entitlement_packages WHERE id=$1`, packageID).Scan(&reserved, &remaining, &used); err != nil {
				t.Fatal(err)
			}
			if reserved != 0 {
				t.Fatal("terminal hold leaked package reservation")
			}
			if operation == "commit" {
				if remaining != 0 || used != order.CreditMicrocredits {
					t.Fatal("commit lost exact package projection")
				}
			} else if remaining != order.CreditMicrocredits || used != 0 {
				t.Fatal("no-charge release consumed package evidence")
			}
			r.now = func() time.Time { return time.Now().UTC().Add(31 * 24 * time.Hour) }
			if _, err := r.ExpirePackages(ctx, 100); err != nil {
				t.Fatal(err)
			}
			stored, err := r.GetWallet(ctx, wallet.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected := int64(1000)
			if operation == "commit" {
				expected = 600
			}
			if stored.PostedMicrocredits != expected || stored.ReservedMicrocredits != 0 {
				t.Fatal("expiry spent ordinary balance or retained usage reservation")
			}
			assertBalanced(t, r)
		})
	}
}

func TestPostgresPackageExpiryRetainsUncertainHoldAndSerializesFinancialWinner(t *testing.T) {
	p, wallet, _, order, packageID := packageFixture(t)
	r := p.repository
	ctx := context.Background()
	hold, err := r.Reserve(ctx, uuid.New(), wallet.ID, order.CreditMicrocredits, nil, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if count, err := r.ExpireHolds(ctx, 100); err != nil || count != 1 {
		t.Fatal("hold expiry did not retain REVIEW protection")
	}
	if count, err := r.ExpirePackages(ctx, 100); err != nil || count != 0 {
		t.Fatal("package expiry released an uncertain reserved allocation")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, operation := range []string{"commit", "waiver"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			var err error
			if operation == "commit" {
				_, err = r.Commit(ctx, hold.UsageRequestID, hold.ID, order.CreditMicrocredits, map[string]any{"input_tokens": 1})
			} else {
				_, err = r.ResolveUsageWaiver(ctx, hold.ID, hold.UsageRequestID, "final decision", "operator")
			}
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, ErrUsageAlreadyTerminal) {
			t.Fatal(err)
		}
	}
	var reserved, used, remaining int64
	if err := r.pool.QueryRow(ctx, `SELECT points_reserved,points_used,points_remaining FROM entitlement_packages WHERE id=$1`, packageID).Scan(&reserved, &used, &remaining); err != nil || reserved != 0 || used+remaining != order.CreditMicrocredits {
		t.Fatal("commit/waiver race corrupted allocations")
	}
	if _, err := r.ExpirePackages(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stored, err := r.GetWallet(ctx, wallet.ID)
	if err != nil || stored.PostedMicrocredits != 0 || stored.ReservedMicrocredits != 0 {
		t.Fatal("single financial winner left inconsistent wallet")
	}
	assertBalanced(t, r)
}

func TestPostgresPackageAllocationFaultRollsBackWalletAndHold(t *testing.T) {
	p, wallet, _, order, packageID := packageFixture(t)
	r := p.repository
	ctx := context.Background()
	hold, err := r.Reserve(ctx, uuid.New(), wallet.ID, 100, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `CREATE FUNCTION reject_package_release() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled allocation fixture'; END $$;CREATE TRIGGER reject_package_release BEFORE UPDATE ON usage_hold_allocations FOR EACH ROW EXECUTE FUNCTION reject_package_release()`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Release(ctx, hold.UsageRequestID, hold.ID, "upstream_not_started"); err == nil {
		t.Fatal("allocation storage failure accepted")
	}
	stored, err := r.GetWallet(ctx, wallet.ID)
	if err != nil || stored.PostedMicrocredits != order.CreditMicrocredits || stored.ReservedMicrocredits != 100 {
		t.Fatal("failed allocation update changed wallet")
	}
	var reserved int64
	if err := r.pool.QueryRow(ctx, `SELECT points_reserved FROM entitlement_packages WHERE id=$1`, packageID).Scan(&reserved); err != nil || reserved != 100 {
		t.Fatal("failed release changed package")
	}
	if _, err := r.pool.Exec(ctx, `DROP TRIGGER reject_package_release ON usage_hold_allocations`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Release(ctx, hold.UsageRequestID, hold.ID, "upstream_not_started"); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresNewPackageAndOrderHistoryMoneyAreExactStrings(t *testing.T) {
	p, wallet, user, order, _ := packageFixture(t)
	ctx := context.Background()
	packages, summary, err := p.repository.ListPackages(ctx, user, wallet.ID, 100)
	if err != nil || len(packages) != 1 {
		t.Fatal("package read missing")
	}
	encoded, err := json.Marshal(map[string]any{"items": packages, "summary": summary})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"amount_fen", "points_total_microcredits", "points_reserved_microcredits", "points_used_microcredits", "points_remaining_microcredits", "available_microcredits", "next_expiry_microcredits"} {
		if !strings.Contains(string(encoded), `"`+field+`":"`) {
			t.Fatal("new monetary field did not use decimal string: " + field)
		}
	}
	items, err := p.ListOrders(ctx, user, wallet.ID, 100)
	if err != nil || len(items) != 1 || items[0].ID != order.ID {
		t.Fatal("order history lost scope")
	}
	encoded, err = json.Marshal(items)
	if err != nil || !strings.Contains(string(encoded), `"amount_fen":"`) || !strings.Contains(string(encoded), `"display_points":"`) {
		t.Fatal("new order history rounded money")
	}
	if wrongOwner, _, err := p.repository.ListPackages(ctx, uuid.New(), wallet.ID, 100); err != nil || len(wrongOwner) != 0 {
		t.Fatal("package read crossed the personal owner boundary")
	}
}
