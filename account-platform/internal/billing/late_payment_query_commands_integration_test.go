package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type lateQueryTestProvider struct {
	payment.Provider
	result     ProviderOrder
	getError   error
	gets       atomic.Int64
	closes     atomic.Int64
	afterGet   func()
	afterClose func()
}

func (p *lateQueryTestProvider) Capabilities() PaymentCapabilities {
	return PaymentCapabilities{ProviderID: "controlled", Version: "v1", Channels: []string{"ALIPAY"}, OrderQuerySupported: true, CloseSupported: true, AuthoritativeUnpaidTerminal: true, MinimumQueryIntervalSeconds: 1, TestOnly: true}
}
func (p *lateQueryTestProvider) Get(context.Context, string) (ProviderOrder, error) {
	p.gets.Add(1)
	if p.afterGet != nil {
		p.afterGet()
	}
	return p.result, p.getError
}
func (p *lateQueryTestProvider) Close(context.Context, string) (ProviderOrder, error) {
	p.closes.Add(1)
	if p.afterClose != nil {
		p.afterClose()
	}
	result := p.result
	result.Status = "CLOSED"
	return result, nil
}

func lateQueryFixture(t *testing.T) (*PostgresRepository, context.Context, *Service, *lateQueryTestProvider, LatePaymentReviewCase, uuid.UUID, LatePaymentQueryInput) {
	t.Helper()
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	item, err := r.EscalateLatePaymentReview(ctx, order.ID, "PENDING")
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	item, err = r.ClaimLatePaymentReview(ctx, item.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	provider := &lateQueryTestProvider{result: ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "PENDING", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ExternalCustomerKey: order.RequesterID.String()}}
	coordinator, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	input := LatePaymentQueryInput{1, item.PaymentOrderID, item.ProviderID, item.ProviderVersion, item.MerchantOrderNo}
	return r, ctx, NewService(r).WithPayments(coordinator), provider, item, actor, input
}

func lateQueryHTTP(s *Service, ctx context.Context, actor, caseID uuid.UUID, key string, input LatePaymentQueryInput, readonly bool) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(input)
	request := httptest.NewRequest("POST", "/admin/v1/late-payment-reviews/"+caseID.String()+"/query-commands", bytes.NewReader(raw)).WithContext(ctx)
	request.SetPathValue("case_id", caseID.String())
	request.Header.Set("Authorization", "Bearer PUBLIC_SYNTHETIC_QUERY")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler := s.adminLatePaymentQueryCommand
	if readonly {
		handler = s.adminLatePaymentQueryResult
	}
	principal := identity.Principal{Issuer: "controlled", Subject: actor.String(), SessionID: "controlled", AuthenticatedAt: time.Now()}
	identity.Middleware(func(context.Context, string) (identity.Principal, error) { return principal, nil }, http.HandlerFunc(handler)).ServeHTTP(response, request)
	return response
}
func requireLateQueryState(t *testing.T, response *httptest.ResponseRecorder, state string) LatePaymentQueryReceipt {
	t.Helper()
	var receipt LatePaymentQueryReceipt
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.State != state {
		t.Fatalf("want %s: HTTP%d %s", state, response.Code, response.Body.String())
	}
	return receipt
}
func lateQueryCount(t *testing.T, r *PostgresRepository, ctx context.Context, query string, args ...any) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func failLateQueryCompletion(t *testing.T, r *PostgresRepository, ctx context.Context) {
	t.Helper()
	_, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION fail_query_receipt_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled receipt failure'; END $$; CREATE TRIGGER fail_query_receipt BEFORE UPDATE ON late_payment_query_commands FOR EACH ROW EXECUTE FUNCTION fail_query_receipt_fixture()`)
	if err != nil {
		t.Fatal(err)
	}
}

type uncertainLateQueryRepository struct{ *PostgresRepository }

func (r uncertainLateQueryRepository) PrepareLatePaymentQuery(ctx context.Context, caseID, actor uuid.UUID, key string, input LatePaymentQueryInput) (LatePaymentQueryReceipt, error) {
	receipt, err := r.PostgresRepository.PrepareLatePaymentQuery(ctx, caseID, actor, key, input)
	if err != nil {
		return receipt, err
	}
	// Real marker committed, but the caller cannot establish the COMMIT ACK.
	// This controlled transport fault must grant no Provider execution token.
	return receipt, errors.New("controlled lost marker commit acknowledgement")
}

func TestLatePaymentQueryCommandPostgres(t *testing.T) {
	t.Run("lost_ACK_original_receipt_new_review_and_terminal_projection", func(t *testing.T) {
		r, ctx, s, p, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		first := requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, false), "COMPLETED")
		if p.gets.Load() != 1 {
			t.Fatal("first query did not use actual Provider flow")
		}
		r.now = func() time.Time { return time.Now().UTC().Add(2 * time.Hour) }
		if _, err := r.ScheduleLatePaymentReview(ctx, item.ID, actor, r.now().Add(48*time.Hour), "controlled later review schedule", "query-later-plan-"+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		for _, readonly := range []bool{true, false, true} {
			replay := requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, readonly), "COMPLETED")
			before, _ := json.Marshal(first)
			after, _ := json.Marshal(replay)
			if !bytes.Equal(before, after) || p.gets.Load() != 1 {
				t.Fatal("original replay changed its receipt or queried Provider")
			}
		}
		if lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_review_evidence WHERE case_id=$1 AND evidence_type='MANUAL_QUERY'`, item.ID) != 1 {
			t.Fatal("replay duplicated query evidence")
		}
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, "query-"+uuid.NewString(), input, false), "COMPLETED")
		if p.gets.Load() != 2 {
			t.Fatal("explicit fresh identity did not query current Provider", p.gets.Load())
		}
	})
	t.Run("missing_marker_readonly_and_crash_after_marker_never_dispatch", func(t *testing.T) {
		r, ctx, s, p, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, true), "UNKNOWN")
		if lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_query_commands`) != 0 {
			t.Fatal("result lookup invented a marker")
		}
		prepared, err := r.PrepareLatePaymentQuery(ctx, item.ID, actor, key, input)
		if err != nil || !prepared.dispatch {
			t.Fatal("marker fixture", err)
		}
		for _, readonly := range []bool{false, true, true} {
			requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, readonly), "UNKNOWN")
		}
		if p.gets.Load() != 0 || p.closes.Load() != 0 {
			t.Fatal("crashed invocation dispatched on replay")
		}
	})
	t.Run("concurrent_same_identity_has_one_actual_Provider_dispatch", func(t *testing.T) {
		r, ctx, s, p, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		var wg sync.WaitGroup
		responses := make(chan *httptest.ResponseRecorder, 8)
		for range 8 {
			wg.Add(1)
			go func() { defer wg.Done(); responses <- lateQueryHTTP(s, ctx, actor, item.ID, key, input, false) }()
		}
		wg.Wait()
		close(responses)
		for response := range responses {
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
		}
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, true), "COMPLETED")
		if p.gets.Load() != 1 || lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_query_commands`) != 1 {
			t.Fatal("same key admitted more than once", p.gets.Load())
		}
	})
	t.Run("actor_case_facts_and_current_owner_cannot_disclose_original_receipt", func(t *testing.T) {
		r, ctx, s, p, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, false), "COMPLETED")
		wrong := input
		wrong.PaymentOrderID = uuid.New()
		for _, response := range []*httptest.ResponseRecorder{lateQueryHTTP(s, ctx, uuid.New(), item.ID, key, input, true), lateQueryHTTP(s, ctx, actor, item.ID, key, wrong, true)} {
			if response.Code != 409 || strings.Contains(response.Body.String(), "last_query_at") {
				t.Fatal("foreign facts disclosed receipt", response.Code)
			}
		}
		newActor := uuid.New()
		if _, err := r.pool.Exec(ctx, `UPDATE manual_review_cases SET owner_id=$2 WHERE id=$1`, item.ID, newActor); err != nil {
			t.Fatal(err)
		}
		for _, candidate := range []uuid.UUID{actor, newActor} {
			if lateQueryHTTP(s, ctx, candidate, item.ID, key, input, true).Code != 409 {
				t.Fatal("owner transfer disclosed original receipt")
			}
		}
		if p.gets.Load() != 1 {
			t.Fatal("owner/facts conflict caused Provider I/O")
		}
	})
	t.Run("record_and_receipt_failure_roll_back_together_and_retry_stays_unknown", func(t *testing.T) {
		r, ctx, s, p, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		failLateQueryCompletion(t, r, ctx)
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, false), "UNKNOWN")
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, true), "UNKNOWN")
		if p.gets.Load() != 1 || lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_review_evidence WHERE case_id=$1 AND evidence_type='MANUAL_QUERY'`, item.ID) != 0 || lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_query_commands WHERE completed_at IS NOT NULL`) != 0 {
			t.Fatal("receipt failure left partial record or redispatched")
		}
	})
	t.Run("uncertain_marker_commit_grants_zero_external_dispatch", func(t *testing.T) {
		r, ctx, s, p, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		s.repository = uncertainLateQueryRepository{r}
		response := lateQueryHTTP(s, ctx, actor, item.ID, key, input, false)
		if response.Code < 500 {
			t.Fatal("controlled commit uncertainty was reported complete", response.Code)
		}
		s.repository = r
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, true), "UNKNOWN")
		if p.gets.Load() != 0 || p.closes.Load() != 0 || lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_query_commands`) != 1 {
			t.Fatal("uncertain admission dispatched")
		}
	})
	t.Run("marker_commit_failure_and_missing_result_do_not_dispatch", func(t *testing.T) {
		r, ctx, s, p, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		_, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION fail_query_admission_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled admission commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_query_admission_commit AFTER INSERT ON late_payment_query_commands DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_query_admission_commit()`)
		if err != nil {
			t.Fatal(err)
		}
		if response := lateQueryHTTP(s, ctx, actor, item.ID, key, input, false); response.Code < 500 {
			t.Fatal("failed admission commit not rejected", response.Code)
		}
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, true), "UNKNOWN")
		if p.gets.Load() != 0 || p.closes.Load() != 0 || lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_query_commands`) != 0 {
			t.Fatal("failed commit or readonly result admitted external work")
		}
	})
	t.Run("Provider_IO_holds_no_query_or_case_transaction_lock", func(t *testing.T) {
		r, ctx, s, p, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		p.afterGet = func() {
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			defer tx.Rollback(ctx)
			var acquired bool
			lock := fmt.Sprintf("%d:%s%s", len("late-payment.query-command"), "late-payment.query-command", key)
			if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, lock).Scan(&acquired); err != nil || !acquired {
				t.Error("query key lock held across Provider I/O", err)
			}
			if _, err = tx.Exec(ctx, `SELECT late.id FROM late_payment_review_cases late JOIN manual_review_cases manual ON manual.id=late.id WHERE late.id=$1 FOR UPDATE OF late,manual NOWAIT`, item.ID); err != nil {
				t.Error("case rows held across Provider I/O", err)
			}
		}
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, false), "COMPLETED")
	})
	t.Run("exact_900030_direct_Down_refuses_and_retains_marker", func(t *testing.T) {
		r, ctx, _, _, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		if receipt, err := r.PrepareLatePaymentQuery(ctx, item.ID, actor, key, input); err != nil || !receipt.dispatch {
			t.Fatal(err)
		}
		facts := func() string {
			var value string
			if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
                'ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY m.version) FROM schema_migrations m),
                'command',(SELECT to_jsonb(c) FROM late_payment_query_commands c WHERE c.idempotency_key=$1),
                'late',(SELECT to_jsonb(l) FROM late_payment_review_cases l WHERE l.id=$2),
                'manual',(SELECT to_jsonb(m) FROM manual_review_cases m WHERE m.id=$2),
                'order',(SELECT to_jsonb(o) FROM payment_orders o WHERE o.id=$3))::text`, key, item.ID, item.PaymentOrderID).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		before := facts()
		down, err := os.ReadFile("../../migrations/billing/900030_late_payment_query_commands.down.sql")
		if err != nil {
			t.Fatal(err)
		}
		// Use the owned migration identity and an explicit transaction. An ACL
		// denial is not proof that the published forward-only SQL executed.
		statement := `DO $$ BEGIN IF session_user <> 'billing_migrator' OR current_user <> 'billing_schema_owner' THEN RAISE EXCEPTION 'wrong query Down identity'; END IF; END $$; BEGIN;` + string(down) + `COMMIT;`
		_, err = retainedFinanceOwnerExec(t, r, ctx, statement)
		var refusal *pgconn.PgError
		if !errors.As(err, &refusal) || refusal.Code != "P0001" || refusal.Message != "late payment query dispatch identity and receipts are forward-only" {
			t.Fatal("published 900030 direct Down did not give its exact refusal", err)
		}
		if after := facts(); after != before {
			t.Fatal("failed direct Down changed the migration ledger, original marker or review/order facts")
		}
	})
	t.Run("PAY12_committed_before_receipt_fault_is_not_reexecuted_or_forged", func(t *testing.T) {
		r, ctx, s, p, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		failLateQueryCompletion(t, r, ctx)
		transaction := uuid.NewString()
		paidAt := time.Now().UTC()
		p.result.Status = "SUCCESS"
		p.result.ChannelTransactionID = &transaction
		p.result.PaidAt = &paidAt
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, false), "UNKNOWN")
		var status, fulfillment string
		if err := r.pool.QueryRow(ctx, `SELECT status,fulfillment_status FROM payment_orders WHERE id=$1`, item.PaymentOrderID).Scan(&status, &fulfillment); err != nil || (status != "PAID" && status != "PAID_LATE") || fulfillment != "CREDITED" {
			t.Fatal("actual PAY12 merge did not commit", status, fulfillment, err)
		}
		if lateQueryCount(t, r, ctx, `SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1 AND status IN ('PAID','LATE_PAID')`, item.PaymentOrderID) != 1 {
			t.Fatal("missing original payment transaction")
		}
		for range 2 {
			requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, true), "UNKNOWN")
		}
		if p.gets.Load() != 1 || lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_query_commands WHERE completed_at IS NOT NULL`) != 0 || lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_review_evidence WHERE case_id=$1 AND evidence_type='MANUAL_QUERY'`, item.ID) != 0 {
			t.Fatal("recovery forged completed receipt or called Provider")
		}
		var balance int64
		if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=(SELECT wallet_id FROM payment_orders WHERE id=$1)`, item.PaymentOrderID).Scan(&balance); err != nil || balance != 1000000 {
			t.Fatal("original credited amount changed", balance, err)
		}
	})
	t.Run("concurrent_legal_terminal_case_is_not_overwritten_by_completion", func(t *testing.T) {
		r, ctx, _, _, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		if receipt, err := r.PrepareLatePaymentQuery(ctx, item.ID, actor, key, input); err != nil || !receipt.dispatch {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE late_payment_review_cases SET review_status='RESOLVED_PAID',resolved_at=now() WHERE id=$1`, item.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.CompleteLatePaymentQuery(ctx, item.ID, actor, key, input, PaymentOrder{ID: item.PaymentOrderID, Status: "PENDING"}, nil); !errors.Is(err, ErrConflict) {
			t.Fatal("completion overwrote another terminal operation", err)
		}
		receipt, err := r.ReadLatePaymentQuery(ctx, item.ID, actor, key, input)
		if err != nil || receipt.State != "UNKNOWN" {
			t.Fatal("terminal projection forged original receipt", err)
		}
		var state string
		if err := r.pool.QueryRow(ctx, `SELECT review_status FROM late_payment_review_cases WHERE id=$1`, item.ID).Scan(&state); err != nil || state != "RESOLVED_PAID" {
			t.Fatal("terminal state overwritten", state, err)
		}
	})
	t.Run("cancellation_after_Get_preserves_original_unknown", func(t *testing.T) {
		_, ctx, s, p, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		operation, cancel := context.WithCancel(ctx)
		p.afterGet = cancel
		requireLateQueryState(t, lateQueryHTTP(s, operation, actor, item.ID, key, input, false), "UNKNOWN")
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, true), "UNKNOWN")
		if p.gets.Load() != 1 || p.closes.Load() != 0 {
			t.Fatal("canceled invocation was dispatched again")
		}
	})
	t.Run("reachable_Close_interruption_never_recloses_on_recovery", func(t *testing.T) {
		r, ctx, s, p, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		// Exercise the existing Sync close branch with its exact eligibility
		// state. The normal escalation fixture itself starts MANUAL_REVIEW.
		if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET status='PENDING',fulfillment_status='PENDING',expires_at=now()-interval '1 hour',provider_payable_until=now()-interval '1 hour' WHERE id=$1`, item.PaymentOrderID); err != nil {
			t.Fatal(err)
		}
		operation, cancel := context.WithCancel(ctx)
		p.afterClose = cancel
		requireLateQueryState(t, lateQueryHTTP(s, operation, actor, item.ID, key, input, false), "UNKNOWN")
		for range 2 {
			requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, true), "UNKNOWN")
		}
		if p.gets.Load() != 1 || p.closes.Load() != 1 {
			t.Fatal("Close path not reached exactly once", p.gets.Load(), p.closes.Load())
		}
	})
	t.Run("runtime_identity_and_completed_receipts_are_immutable", func(t *testing.T) {
		r, ctx, s, _, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		requireLateQueryState(t, lateQueryHTTP(s, ctx, actor, item.ID, key, input, false), "COMPLETED")
		for _, statement := range []string{`UPDATE late_payment_query_commands SET actor_id='00000000-0000-0000-0000-000000000001'`, `UPDATE late_payment_query_commands SET result_snapshot='{}'`, `DELETE FROM late_payment_query_commands`, `TRUNCATE late_payment_query_commands`} {
			if _, err := r.pool.Exec(ctx, statement); err == nil {
				t.Fatal("runtime changed original identity/receipt", statement)
			}
		}
	})
}
