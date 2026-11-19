package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
	"github.com/google/uuid"
)

// Keep the real Create/query/PAY-12/HTTP/repository and Billing money guards.
// Supplier replies/barriers and Account admission responses are controlled;
// no authority SQL supplies the current-query observations. Provider CLOSED
// is not Account identity cleanup. Real Account cleanup is tested separately.
type archiveAdmissionProvider struct {
	*instructionCreateProvider
	response ProviderOrder
	failure  error
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	gets     atomic.Int64
}

func (p *archiveAdmissionProvider) Get(ctx context.Context, _ string) (ProviderOrder, error) {
	p.gets.Add(1)
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return ProviderOrder{}, ctx.Err()
		}
	}
	if p.failure != nil {
		return ProviderOrder{}, p.failure
	}
	return p.response, nil
}

// This Billing-only fixture returns a matching Account refusal; it is not a
// real Account database cleanup transition or a provider payment status.
type archiveAccountAdmissionRefusal struct {
	state string
	calls atomic.Int64
}

func (a *archiveAccountAdmissionRefusal) Admit(_ context.Context, input paymentcredit.Request) (paymentcredit.Decision, error) {
	a.calls.Add(1)
	if !input.Valid() {
		return paymentcredit.Decision{}, paymentcredit.ErrProtocol
	}
	return paymentcredit.Decision{ID: input.ID, Fingerprint: input.Fingerprint(), State: a.state}, nil
}

func (p *archiveAdmissionProvider) finish() {
	if p.release != nil {
		p.once.Do(func() { close(p.release) })
	}
}

func archiveCurrentQueryAdmission(t *testing.T, r *PostgresRepository, ctx context.Context, actor uuid.UUID, makeHandler func(*PaymentCoordinator) http.Handler) {
	previousClock := r.now
	defer func() { r.now = previousClock }()
	fixture := func(t *testing.T) (*PaymentCoordinator, *instructionCreateProvider, PaymentOrder, LatePaymentReviewCase) {
		t.Helper()
		clock := time.Now().UTC().Truncate(time.Microsecond)
		r.now = func() time.Time { return clock }
		c, provider, owner, wallet, product := instructionCreateFixture(t, r, ctx)
		provider.authoritative = true
		c.now = r.now
		created, _, err := c.CreateOrder(ctx, owner, wallet, product, "ALIPAY", "archive-current-"+uuid.NewString())
		if err != nil {
			t.Fatal("real Create", err)
		}
		// Advance only the owned scheduler clock. All subsequent observations
		// intentionally share the same instant, including repeated payloads.
		clock = created.FinalArchiveEligibleAt.Add(time.Second)
		item, err := r.EscalateLatePaymentReview(ctx, created.ID, "PENDING")
		if err != nil {
			t.Fatal(err)
		}
		item, err = r.ClaimLatePaymentReview(ctx, item.ID, actor)
		if err != nil {
			t.Fatal(err)
		}
		return c, provider, created, item
	}
	coordinator := func(t *testing.T, base *instructionCreateProvider, response ProviderOrder, failure error, held bool) (*PaymentCoordinator, *archiveAdmissionProvider) {
		t.Helper()
		provider := &archiveAdmissionProvider{instructionCreateProvider: base, response: response, failure: failure}
		if held {
			provider.entered, provider.release = make(chan struct{}, 1), make(chan struct{})
		}
		// Control only Account admission; retain the real Billing receipt and money guard.
		// An older successful payment must still exercise the original financial merge.
		c, err := newBillingFixturePaymentCoordinator(r, provider, "test")
		if err != nil {
			t.Fatal(err)
		}
		c.now = r.now
		return c, provider
	}
	query := func(handler http.Handler, caseID uuid.UUID, key string, input LatePaymentQueryInput, queryCtx context.Context) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(input)
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/late-payment-reviews/"+caseID.String()+"/query-commands", bytes.NewReader(raw)).WithContext(queryCtx)
		request.Header.Set("Authorization", "Bearer "+actor.String())
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	archive := func(t *testing.T, handler http.Handler, item LatePaymentReviewCase, key string, status int) []byte {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/late-payment-reviews/"+item.ID.String()+"/archive-command", nil).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+actor.String())
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("real archive route: got%d want%d %s", response.Code, status, response.Body.String())
		}
		return append([]byte(nil), response.Body.Bytes()...)
	}
	current := func(t *testing.T, orderID uuid.UUID) (uuid.UUID, *bool) {
		t.Helper()
		var generation uuid.UUID
		var authority *bool
		if err := r.pool.QueryRow(ctx, `SELECT archive_query_generation,archive_query_authoritative_unpaid FROM payment_orders WHERE id=$1`, orderID).Scan(&generation, &authority); err != nil {
			t.Fatal(err)
		}
		return generation, authority
	}
	protected := func(t *testing.T, item LatePaymentReviewCase) string {
		t.Helper()
		var value string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('order',to_jsonb(payment),'late',to_jsonb(late),'manual',to_jsonb(manual),'events',(SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM manual_review_case_events e WHERE e.case_id=late.id),'archive_commands',(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]'::jsonb) FROM late_payment_review_archive_commands c WHERE c.case_id=late.id),'query_commands',(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.idempotency_key),'[]'::jsonb) FROM late_payment_query_commands c WHERE c.case_id=late.id),'review_evidence',(SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM late_payment_review_evidence e WHERE e.case_id=late.id),'payment_evidence',(SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM payment_evidence e WHERE e.payment_order_id=payment.id))::text FROM late_payment_review_cases late JOIN manual_review_cases manual ON manual.id=late.id JOIN payment_orders payment ON payment.id=late.payment_order_id WHERE late.id=$1`, item.ID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	rejected := func(t *testing.T, handler http.Handler, item LatePaymentReviewCase) {
		t.Helper()
		before, financial := protected(t, item), instructionCommandFactsSnapshot(t, r, ctx)
		archive(t, handler, item, "archive-"+uuid.NewString(), 409)
		if before != protected(t, item) || !reflect.DeepEqual(financial, instructionCommandFactsSnapshot(t, r, ctx)) {
			t.Fatal("rejected current observation changed complete protected facts")
		}
	}
	t.Run("real-CLOSED-then-failure-PENDING-and-identical-CLOSED", func(t *testing.T) {
		_, base, order, item := fixture(t)
		input := LatePaymentQueryInput{1, order.ID, item.ProviderID, item.ProviderVersion, item.MerchantOrderNo}
		closed := base.result
		closed.Status, closed.ProviderStatus = "CLOSED", "PAY_CLOSED"
		closed.PaymentURL, closed.QRData = "", ""
		c, _ := coordinator(t, base, closed, nil, false)
		handler := makeHandler(c)
		requireLateQueryState(t, query(handler, item.ID, "query-"+uuid.NewString(), input, ctx), "COMPLETED")
		first, allowed := current(t, order.ID)
		if allowed == nil || !*allowed {
			t.Fatal("real authoritative CLOSED did not publish current authority")
		}
		var closedEvidence string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id)::text FROM payment_evidence e WHERE e.payment_order_id=$1 AND e.evidence_type='QUERY' AND e.payload_snapshot->>'status'='CLOSED'`, order.ID).Scan(&closedEvidence); err != nil {
			t.Fatal(err)
		}
		c, _ = coordinator(t, base, ProviderOrder{}, errors.New("controlled provider unavailable"), false)
		handler = makeHandler(c)
		requireLateQueryState(t, query(handler, item.ID, "query-"+uuid.NewString(), input, ctx), "COMPLETED")
		failed, allowed := current(t, order.ID)
		if failed == first || allowed != nil {
			t.Fatal("latest failed query reused old authority")
		}
		rejected(t, handler, item)
		pending := base.result
		pending.Status, pending.ProviderStatus = "PENDING", "PENDING"
		c, _ = coordinator(t, base, pending, nil, false)
		handler = makeHandler(c)
		pendingReceipt := requireLateQueryState(t, query(handler, item.ID, "query-"+uuid.NewString(), input, ctx), "COMPLETED")
		_, allowed = current(t, order.ID)
		if allowed == nil || *allowed || pendingReceipt.Result == nil || pendingReceipt.Result.LastQueryResult == nil || *pendingReceipt.Result.LastQueryResult == "PENDING" {
			t.Fatal("current canonical PENDING was confused with historical order status")
		}
		rejected(t, handler, item)
		c, _ = coordinator(t, base, closed, nil, false)
		handler = makeHandler(c)
		requireLateQueryState(t, query(handler, item.ID, "query-"+uuid.NewString(), input, ctx), "COMPLETED")
		var repeatedEvidence string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id)::text FROM payment_evidence e WHERE e.payment_order_id=$1 AND e.evidence_type='QUERY' AND e.payload_snapshot->>'status'='CLOSED'`, order.ID).Scan(&repeatedEvidence); err != nil || repeatedEvidence != closedEvidence {
			t.Fatal("repeated same-time CLOSED rewrote or duplicated immutable evidence", err)
		}
		key := "archive-" + uuid.NewString()
		original := archive(t, handler, item, key, 200)
		// A subsequent real failed query invalidates only current admission;
		// the old command must still return its original immutable receipt.
		failedCoordinator, _ := coordinator(t, base, ProviderOrder{}, errors.New("controlled later provider unavailable"), false)
		if _, err := failedCoordinator.SyncOrder(ctx, order.ID); err == nil {
			t.Fatal("controlled later query unexpectedly succeeded")
		}
		before := protected(t, item)
		if replay := archive(t, handler, item, key, 200); !bytes.Equal(original, replay) || before != protected(t, item) {
			t.Fatal("new unknown current fact blocked or rewrote original archive receipt")
		}
	})
	t.Run("latest-canceled-query-stays-UNKNOWN-and-never-redispatches", func(t *testing.T) {
		_, base, order, item := fixture(t)
		input := LatePaymentQueryInput{1, order.ID, item.ProviderID, item.ProviderVersion, item.MerchantOrderNo}
		closed := base.result
		closed.Status, closed.ProviderStatus = "CLOSED", "PAY_CLOSED"
		closed.PaymentURL, closed.QRData = "", ""
		c, held := coordinator(t, base, closed, nil, true)
		defer held.finish()
		handler := makeHandler(c)
		queryCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		key := "query-" + uuid.NewString()
		results, done := make(chan *httptest.ResponseRecorder, 1), make(chan struct{})
		go func() { defer close(done); results <- query(handler, item.ID, key, input, queryCtx) }()
		defer func() {
			cancel()
			held.finish()
			select {
			case <-done:
			case <-ctx.Done():
				t.Error("owned canceled query did not exit")
			}
		}()
		select {
		case <-held.entered:
		case <-ctx.Done():
			t.Fatal("cancel case missed supplier barrier")
		}
		generation, authority := current(t, order.ID)
		if authority != nil {
			t.Fatal("in-flight accepted query is not unknown")
		}
		cancel()
		select {
		case response := <-results:
			requireLateQueryState(t, response, "UNKNOWN")
		case <-ctx.Done():
			t.Fatal("canceled query did not exit")
		}
		after, authority := current(t, order.ID)
		if after != generation || authority != nil {
			t.Fatal("canceled query manufactured current authority")
		}
		before := protected(t, item)
		requireLateQueryState(t, query(handler, item.ID, key, input, ctx), "UNKNOWN")
		if held.gets.Load() != 1 || before != protected(t, item) {
			t.Fatal("unknown replay redispatched or changed protected facts")
		}
		rejected(t, handler, item)
	})
	for _, oldResult := range []string{"failure", "PENDING", "CLOSED", "SUCCESS"} {
		t.Run("older-"+oldResult+"-cannot-cover-newer-CLOSED", func(t *testing.T) {
			_, base, order, item := fixture(t)
			input := LatePaymentQueryInput{1, order.ID, item.ProviderID, item.ProviderVersion, item.MerchantOrderNo}
			older := base.result
			older.Status, older.ProviderStatus = oldResult, oldResult
			if oldResult != "PENDING" {
				older.PaymentURL, older.QRData = "", ""
			}
			var failure error
			if oldResult == "failure" {
				failure = errors.New("controlled older query failure")
			}
			if oldResult == "SUCCESS" {
				transaction, paidAt := uuid.NewString(), order.ExpiresAt.Add(-time.Second)
				older.ChannelTransactionID, older.PaidAt = &transaction, &paidAt
			}
			a, held := coordinator(t, base, older, failure, true)
			defer held.finish()
			results, done := make(chan *httptest.ResponseRecorder, 1), make(chan struct{})
			go func() {
				defer close(done)
				results <- query(makeHandler(a), item.ID, "query-"+uuid.NewString(), input, ctx)
			}()
			defer func() {
				held.finish()
				select {
				case <-done:
				case <-ctx.Done():
					t.Error("owned old query did not exit")
				}
			}()
			select {
			case <-held.entered:
			case <-ctx.Done():
				t.Fatal("old query did not reach real supplier barrier")
			}
			closed := base.result
			closed.Status, closed.ProviderStatus = "CLOSED", "PAY_CLOSED"
			closed.PaymentURL, closed.QRData = "", ""
			b, _ := coordinator(t, base, closed, nil, false)
			handler := makeHandler(b)
			requireLateQueryState(t, query(handler, item.ID, "query-"+uuid.NewString(), input, ctx), "COMPLETED")
			generation, allowed := current(t, order.ID)
			if allowed == nil || !*allowed {
				t.Fatal("newer query did not establish authority")
			}
			// The accepted older durable marker still blocks new archives even
			// after the newer generation's query has completed successfully.
			rejected(t, handler, item)
			held.finish()
			select {
			case response := <-results:
				requireLateQueryState(t, response, "COMPLETED")
			case <-ctx.Done():
				t.Fatal("old query did not complete")
			}
			after, allowed := current(t, order.ID)
			if after != generation || allowed == nil || !*allowed || held.gets.Load() != 1 {
				t.Fatal("late old response replaced the newer authority or redispatched")
			}
			if oldResult == "SUCCESS" {
				stored := instructionStoredOrder(t, r, ctx, order.ID)
				if (stored.Status != "PAID" && stored.Status != "PAID_LATE") || stored.FulfillmentStatus != "CREDITED" {
					t.Fatal("generation fence discarded a real strong payment")
				}
				var merged int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_evidence WHERE payment_order_id=$1 AND evidence_type='QUERY' AND payload_snapshot->>'status'='SUCCESS' AND merge_result='MERGED'`, order.ID).Scan(&merged); err != nil || merged != 1 {
					t.Fatal("old strong payment lost immutable merge evidence", err)
				}
				wallet, err := r.GetWallet(ctx, order.WalletID)
				if err != nil || wallet.PostedMicrocredits != order.CreditMicrocredits || wallet.ReservedMicrocredits != 0 {
					t.Fatal("old strong payment lost exact wallet credit", err)
				}
				rejected(t, handler, item)
			} else {
				archive(t, handler, item, "archive-"+uuid.NewString(), 200)
			}
		})
	}
	for _, accountState := range []string{"CLOSED", "SUBJECT_UNAVAILABLE"} {
		t.Run("older-SUCCESS-newer-provider-CLOSED-Account-"+accountState, func(t *testing.T) {
			_, base, order, item := fixture(t)
			input := LatePaymentQueryInput{1, order.ID, item.ProviderID, item.ProviderVersion, item.MerchantOrderNo}
			older := base.result
			older.Status, older.ProviderStatus = "SUCCESS", "SUCCESS"
			older.PaymentURL, older.QRData = "", ""
			transaction, paidAt := uuid.NewString(), order.ExpiresAt.Add(-time.Second)
			older.ChannelTransactionID, older.PaidAt = &transaction, &paidAt
			a, held := coordinator(t, base, older, nil, true)
			refusal := &archiveAccountAdmissionRefusal{state: accountState}
			a.WithPaymentCreditAdmission(refusal)
			defer held.finish()
			results, done := make(chan *httptest.ResponseRecorder, 1), make(chan struct{})
			go func() {
				defer close(done)
				results <- query(makeHandler(a), item.ID, "query-"+uuid.NewString(), input, ctx)
			}()
			defer func() {
				held.finish()
				select {
				case <-done:
				case <-ctx.Done():
					t.Error("owned old query did not exit")
				}
			}()
			select {
			case <-held.entered:
			case <-ctx.Done():
				t.Fatal("old query did not reach real supplier barrier")
			}
			closed := base.result
			closed.Status, closed.ProviderStatus = "CLOSED", "PAY_CLOSED"
			closed.PaymentURL, closed.QRData = "", ""
			b, _ := coordinator(t, base, closed, nil, false)
			handler := makeHandler(b)
			requireLateQueryState(t, query(handler, item.ID, "query-"+uuid.NewString(), input, ctx), "COMPLETED")
			generation, authority := current(t, order.ID)
			if authority == nil || !*authority {
				t.Fatal("newer provider query did not establish authority")
			}
			rejected(t, handler, item)
			moneyFacts := func() map[string]string {
				facts := entitlementFinancialFactsSnapshot(t, ctx, r)
				var accounts string
				if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY to_jsonb(a)::text),'[]'::jsonb)::text FROM ledger_accounts a`).Scan(&accounts); err != nil {
					t.Fatal("capture complete ledger accounts", err)
				}
				facts["ledger_accounts"] = accounts
				return facts
			}
			before := moneyFacts()
			held.finish()
			select {
			case response := <-results:
				requireLateQueryState(t, response, "COMPLETED")
			case <-ctx.Done():
				t.Fatal("old query did not complete")
			}
			if refusal.calls.Load() == 0 || held.gets.Load() != 1 {
				t.Fatal("test did not reach matching Account refusal or redispatched supplier")
			}
			afterGeneration, afterAuthority := current(t, order.ID)
			if afterGeneration != generation || !reflect.DeepEqual(authority, afterAuthority) {
				t.Fatal("old payment refusal replaced the newer query authority")
			}
			stored := instructionStoredOrder(t, r, ctx, order.ID)
			if stored.Status != "MANUAL_REVIEW" || stored.FulfillmentStatus != "MANUAL_REVIEW" {
				t.Fatal("Account refusal did not preserve original manual payment recovery")
			}
			after := moneyFacts()
			for _, table := range []string{"wallets", "credit_lots", "entitlement_packages", "entitlement_components", "entitlement_component_corrections", "journal_entries", "ledger_postings", "ledger_accounts", "member_quota_grants", "usage_holds", "usage_hold_allocations", "refunds", "refund_component_reservations", "forced_refund_cases", "chargeback_cases", "receivables", "receivable_allocations"} {
				left, leftOK := before[table]
				right, rightOK := after[table]
				if !leftOK || !rightOK || left != right {
					t.Fatal("Account refusal changed complete financial rows", table)
				}
			}
			var evidence, transactions, attempts, applied, manual int
			if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_evidence WHERE payment_order_id=$1 AND evidence_type='QUERY' AND payload_snapshot->>'status'='SUCCESS' AND merge_result='REVIEW_REQUIRED'),(SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1 AND status='REVIEW_REQUIRED' AND fulfillment_status='REVIEW_REQUIRED'),(SELECT count(*) FROM payment_credit_attempts WHERE payment_order_id=$1),(SELECT count(*) FROM payment_credit_attempts WHERE payment_order_id=$1 AND state='APPLIED'),(SELECT count(*) FROM payment_anomaly_cases WHERE payment_order_id=$1 AND status='OPEN' AND resource_snapshot->>'reason_code'='PAYMENT_CREDIT_ADMISSION_UNCONFIRMED')`, order.ID).Scan(&evidence, &transactions, &attempts, &applied, &manual); err != nil || evidence != 1 || transactions != 1 || attempts != 0 || applied != 0 || manual != 1 {
				t.Fatal("Account refusal lost original immutable evidence or admitted money", evidence, transactions, attempts, applied, manual, err)
			}
			rejected(t, handler, item)
		})
	}
	for _, newerResult := range []string{"failure", "PENDING"} {
		t.Run("older-CLOSED-cannot-clear-newer-"+newerResult, func(t *testing.T) {
			_, base, order, item := fixture(t)
			input := LatePaymentQueryInput{1, order.ID, item.ProviderID, item.ProviderVersion, item.MerchantOrderNo}
			closed := base.result
			closed.Status, closed.ProviderStatus = "CLOSED", "PAY_CLOSED"
			closed.PaymentURL, closed.QRData = "", ""
			a, held := coordinator(t, base, closed, nil, true)
			defer held.finish()
			results, done := make(chan *httptest.ResponseRecorder, 1), make(chan struct{})
			go func() {
				defer close(done)
				results <- query(makeHandler(a), item.ID, "query-"+uuid.NewString(), input, ctx)
			}()
			defer func() {
				held.finish()
				select {
				case <-done:
				case <-ctx.Done():
					t.Error("owned old query did not exit")
				}
			}()
			select {
			case <-held.entered:
			case <-ctx.Done():
				t.Fatal("old query did not reach supplier barrier")
			}
			response := base.result
			response.Status, response.ProviderStatus = "PENDING", "PENDING"
			var failure error
			if newerResult == "failure" {
				failure = errors.New("controlled newer query failure")
			}
			b, _ := coordinator(t, base, response, failure, false)
			handler := makeHandler(b)
			requireLateQueryState(t, query(handler, item.ID, "query-"+uuid.NewString(), input, ctx), "COMPLETED")
			generation, authority := current(t, order.ID)
			if newerResult == "failure" && authority != nil || newerResult == "PENDING" && (authority == nil || *authority) {
				t.Fatal("newer nonterminal result was not unknown/false")
			}
			held.finish()
			select {
			case response := <-results:
				requireLateQueryState(t, response, "COMPLETED")
			case <-ctx.Done():
				t.Fatal("old query did not complete")
			}
			after, afterAuthority := current(t, order.ID)
			if after != generation || !reflect.DeepEqual(authority, afterAuthority) {
				t.Fatal("old CLOSED promoted newer failed/PENDING observation")
			}
			rejected(t, handler, item)
		})
	}
}
