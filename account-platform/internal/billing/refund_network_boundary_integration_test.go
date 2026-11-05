package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The real simulator HTTP handler commits its result before the controlled ACK
// barrier. Billing, the limited-role PG stores and the HTTP provider are real.
type refundWireBoundary struct {
	method   string
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	used     atomic.Bool
	loseACK  bool
	merchant string
	amount   int64
}

func (b *refundWireBoundary) open() { b.once.Do(func() { close(b.release) }) }
func (b *refundWireBoundary) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/refunds") || r.Method != b.method || !b.used.CompareAndSwap(false, true) {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "controlled request read failure", 500)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if r.Method == http.MethodPost {
			var facts struct {
				Merchant string `json:"merchant_refund_no"`
				Amount   int64  `json:"amount_fen"`
			}
			_ = json.Unmarshal(body, &facts)
			b.merchant, b.amount = facts.Merchant, facts.Amount
		}
		recorded := httptest.NewRecorder()
		next.ServeHTTP(recorded, r)
		close(b.entered)
		select {
		case <-b.release:
		case <-r.Context().Done():
			return
		}
		if b.loseACK {
			http.Error(w, "controlled lost acknowledgement after provider commit", 503)
			return
		}
		for name, values := range recorded.Header() {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
	})
}

func TestRefundProviderNetworkOutsideFinancialTransactionPostgres(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method+"-same-wallet-writer-and-final-success-evidence", func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			wire := &refundWireBoundary{method: method, entered: make(chan struct{}), release: make(chan struct{})}
			provider, reads, writes := refundQuerySimulator(t, ctx, wire.wrap)
			defer wire.open()
			c, order, transaction, pack := refundQuerySimulatorPaidOrder(t, r, ctx, provider)
			c.AllowNormalRefundCreation(true)
			refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "network boundary original intent")
			if err != nil {
				t.Fatal(err)
			}
			if method == http.MethodGet {
				if _, err = provider.Refund(ctx, order.MerchantOrderNo, refund.MerchantRefundNo, refund.AmountFen); err != nil {
					t.Fatal(err)
				}
				if _, err = r.pool.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN' WHERE id=$1`, refund.ID); err != nil {
					t.Fatal(err)
				}
			}
			finished := make(chan error, 1)
			go func() { finished <- c.ProcessRefund(ctx, refund.ID) }()
			defer func() {
				wire.open()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("owned refund worker did not finish")
				}
			}()
			select {
			case <-wire.entered:
			case <-ctx.Done():
				t.Fatal("real provider HTTP barrier not reached")
			}
			var idle int
			if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=current_user AND state='idle in transaction'`).Scan(&idle); err != nil {
				t.Fatal(err)
			}
			t.Logf("PAY27 external %s barrier: idle financial transactions=%d", method, idle)
			writerCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			source, writerErr := r.RecordAndFreezeForcedRefund(writerCtx, transaction, "network-overlap-source", 10, nil)
			stop()
			if writerErr != nil {
				t.Fatal("same-wallet financial writer blocked by provider network", writerErr)
			}
			if source.ID == uuid.Nil || idle != 0 {
				t.Fatal("network retained a financial transaction", idle)
			}
			wire.open()
			if err = <-finished; err != nil {
				t.Fatal("valid provider result after competing evidence", err)
			}
			finished <- nil // The deferred join is shared by success and assertion failures.
			if method == http.MethodPost && (wire.merchant != refund.MerchantRefundNo || wire.amount != refund.AmountFen) {
				t.Fatal("provider request changed original cash/key")
			}
			var status, dispatch string
			var posted int64
			var journals, evidence int
			if err = r.pool.QueryRow(ctx, `SELECT r.status,r.dispatch_state,w.posted_microcredits,(SELECT count(*) FROM journal_entries WHERE entry_type='REFUND'),(SELECT count(*) FROM payment_evidence evidence WHERE evidence.payment_order_id=r.payment_order_id AND evidence.provider_refund_id IS NOT NULL AND evidence.provider_status='SUCCESS') FROM refunds r JOIN payment_orders o ON o.id=r.payment_order_id JOIN wallets w ON w.id=o.wallet_id WHERE r.id=$1`, refund.ID).Scan(&status, &dispatch, &posted, &journals, &evidence); err != nil {
				t.Fatal(err)
			}
			if status != "MANUAL_REVIEW" || dispatch != "RESULT_UNKNOWN" || posted != 100 || journals != 0 || evidence != 1 {
				t.Fatal("competing evidence bypassed or lost original success", status, dispatch, posted, journals, evidence)
			}
			before := refundRecoveryMoneySnapshot(t, ctx, r)
			if err = c.ProcessRefund(ctx, refund.ID); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) || writes.Load() != 1 || reads.Load() != int32(map[bool]int{true: 1, false: 0}[method == http.MethodGet]) {
				t.Fatal("known success re-queried, dispatched, or changed money", reads.Load(), writes.Load())
			}
		})
	}
	t.Run("read-only-query-wins-before-original-create-ACK", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		wire := &refundWireBoundary{method: http.MethodPost, entered: make(chan struct{}), release: make(chan struct{})}
		provider, reads, writes := refundQuerySimulator(t, ctx, wire.wrap)
		defer wire.open()
		c, order, transaction, pack := refundQuerySimulatorPaidOrder(t, r, ctx, provider)
		c.AllowNormalRefundCreation(true)
		refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "query completed while original ACK delayed")
		if err != nil {
			t.Fatal(err)
		}
		first := make(chan error, 1)
		go func() { first <- c.ProcessRefund(ctx, refund.ID) }()
		defer func() {
			wire.open()
			select {
			case <-first:
			case <-time.After(5 * time.Second):
				t.Error("first worker did not finish")
			}
		}()
		select {
		case <-wire.entered:
		case <-ctx.Done():
			t.Fatal("real refund ACK barrier not reached")
		}
		queryCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		err = c.ProcessRefund(queryCtx, refund.ID)
		stop()
		if err != nil {
			t.Fatal("original result query blocked behind external ACK", err)
		}
		before := refundRecoveryMoneySnapshot(t, ctx, r)
		wire.open()
		if err = <-first; err != nil {
			t.Fatal(err)
		}
		first <- nil
		if !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) || writes.Load() != 1 || reads.Load() != 1 {
			t.Fatal("late original ACK repeated or changed settlement", reads.Load(), writes.Load())
		}
		refundBoundarySettledOnce(t, r, ctx, refund.ID, order.WalletID)
	})
	for _, failure := range []string{"lost-external-ACK", "local-posting-rollback"} {
		t.Run(failure+"-recovers-only-original-query", func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			wire := &refundWireBoundary{method: http.MethodPost, entered: make(chan struct{}), release: make(chan struct{}), loseACK: failure == "lost-external-ACK"}
			wire.open()
			provider, reads, writes := refundQuerySimulator(t, ctx, wire.wrap)
			c, order, transaction, pack := refundQuerySimulatorPaidOrder(t, r, ctx, provider)
			c.AllowNormalRefundCreation(true)
			refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "original provider result must survive uncertain local completion")
			if err != nil {
				t.Fatal(err)
			}
			if failure == "local-posting-rollback" {
				if _, err = retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION controlled_refund_posting_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.entry_type='REFUND' THEN RAISE EXCEPTION 'controlled refund posting rollback'; END IF; RETURN NEW; END $$; CREATE TRIGGER controlled_refund_posting_failure BEFORE INSERT ON journal_entries FOR EACH ROW EXECUTE FUNCTION controlled_refund_posting_failure()`); err != nil {
					t.Fatal(err)
				}
			}
			if err = c.ProcessRefund(ctx, refund.ID); err == nil {
				t.Fatal("uncertain completion returned success")
			}
			var state string
			var posted int64
			var count int
			if err = r.pool.QueryRow(ctx, `SELECT r.dispatch_state,w.posted_microcredits,(SELECT count(*) FROM journal_entries WHERE entry_type='REFUND') FROM refunds r JOIN payment_orders o ON o.id=r.payment_order_id JOIN wallets w ON w.id=o.wallet_id WHERE r.id=$1`, refund.ID).Scan(&state, &posted, &count); err != nil {
				t.Fatal(err)
			}
			if state == "NOT_STARTED" || posted != 100 || count != 0 || writes.Load() != 1 {
				t.Fatal("uncertain call lost dispatch boundary or posted money", state, posted, count, writes.Load())
			}
			if failure == "local-posting-rollback" {
				if _, err = retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER controlled_refund_posting_failure ON journal_entries; DROP FUNCTION controlled_refund_posting_failure()`); err != nil {
					t.Fatal(err)
				}
			}
			if err = c.ProcessRefund(ctx, refund.ID); err != nil {
				t.Fatal(err)
			}
			if err = c.ProcessRefund(ctx, refund.ID); err != nil {
				t.Fatal(err)
			}
			if reads.Load() != 1 || writes.Load() != 1 {
				t.Fatal("original intent re-created or terminal result queried", reads.Load(), writes.Load())
			}
			refundBoundarySettledOnce(t, r, ctx, refund.ID, order.WalletID)
		})
	}
}

func refundBoundarySettledOnce(t *testing.T, r *PostgresRepository, ctx context.Context, id, wallet uuid.UUID) {
	t.Helper()
	var status string
	var posted int64
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT r.status,w.posted_microcredits,(SELECT count(*) FROM journal_entries WHERE entry_type='REFUND') FROM refunds r JOIN wallets w ON w.id=$2 WHERE r.id=$1`, id, wallet).Scan(&status, &posted, &count); err != nil {
		t.Fatal(err)
	}
	if status != "SUCCEEDED" || posted != 0 || count != 1 {
		t.Fatal("refund did not settle exactly once", status, posted, count)
	}
}
