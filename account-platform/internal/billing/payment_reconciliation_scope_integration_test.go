package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type scopedReconciliationProvider struct {
	legacyMappingProvider
	id, version string
}

func (p *scopedReconciliationProvider) Capabilities() payment.Capabilities {
	c := p.legacyMappingProvider.Capabilities()
	c.ProviderID, c.Version, c.BillSupported = p.id, p.version, true
	return c
}

func (p *scopedReconciliationProvider) Bill(context.Context, string, time.Time) ([]payment.BillEntry, error) {
	return []payment.BillEntry{}, nil
}

func TestPostgresReconciliationProviderScope(t *testing.T) {
	r, _, source := forcedRefundPaidSource(t)
	ctx := context.Background()
	var id, version string
	var paid time.Time
	if err := r.pool.QueryRow(ctx, `SELECT provider_id,provider_version,provider_paid_at FROM payment_transactions WHERE id=$1`, source).Scan(&id, &version, &paid); err != nil {
		t.Fatal(err)
	}
	date := normalizedReconciliationBusinessDate(paid.In(time.FixedZone("Shanghai", 8*3600)))
	for _, item := range []struct {
		name, id, version string
		cases             int
	}{
		{"current-source", id, version, 1},
		{"other-version", id, "controlled-next-version", 0},
		{"other-provider", "controlled-other-provider", version, 0},
	} {
		t.Run(item.name, func(t *testing.T) {
			c := &PaymentCoordinator{repository: r, provider: &scopedReconciliationProvider{id: item.id, version: item.version}, now: time.Now}
			run, err := c.RunAdminReconciliation(ctx, uuid.New(), "ALIPAY", date)
			if err != nil || run.CaseCount != item.cases || run.ProviderID != item.id || run.ProviderVersion != item.version {
				t.Fatalf("reconciliation crossed source scope: cases=%d err=%v", run.CaseCount, err)
			}
		})
	}
}

func TestFourWayReconciliationMerchantBinding(t *testing.T) {
	for _, kind := range []string{"PAYMENT", "REFUND", "CHARGEBACK"} {
		t.Run(kind, func(t *testing.T) {
			fact := ReconciliationInternalFact{EntryType: kind, ProviderReferenceID: "controlled-reference", MerchantOrderNo: "controlled-order-A", PaymentOrderID: uuid.New(), AmountFen: 100, Currency: "CNY", LedgerPresent: true}
			row := ProviderBillEntry{EntryType: kind, ProviderReferenceID: fact.ProviderReferenceID, MerchantOrderNo: fact.MerchantOrderNo, AmountFen: fact.AmountFen, Currency: fact.Currency, Status: "SUCCESS"}
			if differences := CompareFourWayReconciliation([]ProviderBillEntry{row}, []ReconciliationInternalFact{fact}); len(differences) != 0 {
				t.Fatal("same authoritative merchant facts did not match")
			}
			row.MerchantOrderNo = "controlled-order-B"
			differences := CompareFourWayReconciliation([]ProviderBillEntry{row}, []ReconciliationInternalFact{fact})
			if len(differences) != 1 || differences[0].CaseType != "DUPLICATE_TRANSACTION" || differences[0].Severity != "P0" || differences[0].Provider == nil || differences[0].MerchantOrderNo != fact.MerchantOrderNo || differences[0].Provider.MerchantOrderNo != row.MerchantOrderNo {
				t.Fatal("colliding provider reference omitted merchant attribution evidence")
			}
		})
	}
}

// A real wallet lock keeps refund reservation in flight. The source review must
// queue behind its source lock, rather than own the source while refund waits
// for wallet. This exercises reservation only, without dispatching a refund.
func TestPostgresRefundReservationSourceQueue(t *testing.T) {
	r, wallet, source := forcedRefundPaidSource(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var orderID, requesterID, packageID uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT payment.id,payment.requester_id,package.id FROM payment_transactions transaction JOIN payment_orders payment ON payment.id=transaction.payment_order_id JOIN entitlement_packages package ON package.source_payment_transaction_id=transaction.id WHERE transaction.id=$1`, source).Scan(&orderID, &requesterID, &packageID); err != nil {
		t.Fatal(err)
	}
	c := &PaymentCoordinator{repository: r, now: time.Now}
	if _, _, err := c.reserveRefund(ctx, orderID, requesterID, source, uuid.New(), uuid.NewString(), "controlled-facts", "controlled reason"); !errors.Is(err, ErrConflict) {
		t.Fatal("unbound package accepted", err)
	}
	owner, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Rollback(ctx)
	var ownerPID int
	if err := owner.QueryRow(ctx, `SELECT pg_backend_pid() FROM wallets WHERE id=$1 FOR UPDATE`, wallet.ID).Scan(&ownerPID); err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	refundResult := make(chan error, 1)
	go func() {
		_, _, err := c.reserveRefund(ctx, orderID, requesterID, source, packageID, key, "controlled-facts", "controlled reason")
		refundResult <- err
	}()
	var refundPID int
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		_ = r.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))`, ownerPID).Scan(&refundPID)
		if refundPID != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if refundPID == 0 {
		t.Fatal("refund did not queue on controlled wallet")
	}
	review := func() error {
		_, err := r.RecordAndFreezeForcedRefund(ctx, source, "controlled-refund-queue-review", 50, json.RawMessage(`{"confirmed":true}`))
		return err
	}
	reviewResult := make(chan error, 1)
	go func() { reviewResult <- review() }()
	blocked := false
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, ownerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := owner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	refundErr, reviewErr := <-refundResult, <-reviewResult
	if !blocked {
		t.Fatal("review bypassed shared wallet lock")
	}
	var sql *pgconn.PgError
	if errors.As(reviewErr, &sql) && sql.Code == "40001" {
		reviewErr = review() // Explicit failed transaction recovery, not automatic production retry.
	}
	if refundErr != nil || reviewErr != nil {
		t.Fatal("refund/review did not converge without deadlock", refundErr, reviewErr)
	}
	first, _, err := c.reserveRefund(ctx, orderID, requesterID, source, packageID, key, "controlled-facts", "controlled reason")
	if err != nil || first.ID == uuid.Nil {
		t.Fatal("same reservation receipt did not replay after review", err)
	}
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM refunds WHERE idempotency_key=$1`, key).Scan(&count); err != nil || count != 1 {
		t.Fatal("refund reservation duplicated", err)
	}
}
