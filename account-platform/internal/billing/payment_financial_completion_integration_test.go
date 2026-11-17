package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func completionHTTP(t *testing.T, origin string, caseID, actorID uuid.UUID, key string, proposalID uuid.UUID, body any) (int, PaymentFinancialCompletionResult, []byte) {
	t.Helper()
	path := "/admin/v1/payment-anomalies/" + caseID.String() + "/financial-completion-proposals"
	if proposalID != uuid.Nil {
		path += "/" + proposalID.String() + "/confirm"
	}
	raw, _ := json.Marshal(body)
	request, _ := http.NewRequest("POST", origin+path, bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+actorID.String())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var result PaymentFinancialCompletionResult
	if response.StatusCode == 200 && json.Unmarshal(raw, &result) != nil {
		t.Fatal("invalid completion response", string(raw))
	}
	return response.StatusCode, result, raw
}

func completionEscalate(t *testing.T, r *PostgresRepository, ctx context.Context, caseID uuid.UUID) PaymentAnomalyCase {
	t.Helper()
	actor := uuid.New()
	retainedClaimAnomaly(t, r, ctx, caseID, actor)
	item, err := r.ResolvePaymentAnomaly(ctx, caseID, actor, uuid.NewString(), "ESCALATE_FINANCIAL_REPAIR", "controlled financial handoff before completion", "PENDING")
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func completionNoEffectFixture(t *testing.T, r *PostgresRepository, ctx context.Context, orderOnly bool) (*PaymentCoordinator, PaymentOrder, PaymentAnomalyCase) {
	t.Helper()
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	order.ProviderSKU = "completion-sku"
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_sku=$2 WHERE id=$1`, order.ID, order.ProviderSKU); err != nil {
		t.Fatal(err)
	}
	provider := c.provider.(*retainedFinancialProvider)
	provider.result = ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "REVIEW_REQUIRED", ProviderStatus: "FAILED", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ExternalCustomerKey: order.RequesterID.String(), ExternalSKU: order.ProviderSKU}
	if orderOnly {
		if _, err := c.applyProviderReviewStatus(ctx, order, "FAILED"); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := c.recordProviderObservation(ctx, order.ID, "QUERY", provider.result); !errors.Is(err, ErrConflict) {
			t.Fatal("original failed review producer", err)
		}
	}
	var caseID uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM payment_anomaly_cases WHERE payment_order_id=$1`, order.ID).Scan(&caseID); err != nil {
		t.Fatal(err)
	}
	return c, order, completionEscalate(t, r, ctx, caseID)
}

func completionMoneySnapshot(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"wallets", "journal_entries", "ledger_postings", "entitlement_packages", "entitlement_components", "member_quota_grants", "refunds", "payment_transactions", "payment_order_repairs"} {
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(row) ORDER BY to_jsonb(row)::text),'[]'::jsonb)::text FROM `+table+` row`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, table+raw)
	}
	return strings.Join(parts, "\n")
}

func TestEscalatedPaymentFinancialCompletionPostgres(t *testing.T) {
	t.Run("lost-ack-replays-exact-committed-result", paymentCompletionLostACK)
	t.Run("concurrent-reviewers-complete-once", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, _, item := completionNoEffectFixture(t, r, ctx, false)
		proposal, err := c.PaymentFinancialCompletion(ctx, item.ID, uuid.New(), uuid.NewString(), "controlled concurrent completion proposal", PaymentFinancialCompletionInput{ProofKind: "NO_FINANCIAL_EFFECT"}, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		before := completionMoneySnapshot(t, r, ctx)
		actors := [2]uuid.UUID{uuid.New(), uuid.New()}
		keys := [2]string{uuid.NewString(), uuid.NewString()}
		errs := [2]error{}
		start := make(chan struct{})
		var ready, done sync.WaitGroup
		ready.Add(2)
		done.Add(2)
		for i := range actors {
			go func(i int) {
				defer done.Done()
				ready.Done()
				<-start
				_, errs[i] = c.PaymentFinancialCompletion(ctx, item.ID, actors[i], keys[i], "controlled independent concurrent review", PaymentFinancialCompletionInput{}, proposal.Proposal.ID)
			}(i)
		}
		ready.Wait()
		close(start)
		done.Wait()
		success := 0
		for i, err := range errs {
			if err == nil {
				success++
				if _, err := c.PaymentFinancialCompletion(ctx, item.ID, actors[i], keys[i], "controlled independent concurrent review", PaymentFinancialCompletionInput{}, proposal.Proposal.ID); err != nil {
					t.Fatal("winner receipt did not replay", err)
				}
				continue
			}
			var pgErr *pgconn.PgError
			if !errors.Is(err, ErrConflict) && !(errors.As(err, &pgErr) && pgErr.Code == "40001") {
				t.Fatal("unexpected concurrent error", err)
			}
			if _, err := c.PaymentFinancialCompletion(ctx, item.ID, actors[i], keys[i], "controlled independent concurrent review", PaymentFinancialCompletionInput{}, proposal.Proposal.ID); !errors.Is(err, ErrConflict) {
				t.Fatal("loser acquired completed case", err)
			}
		}
		var commands, events int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_anomaly_commands WHERE payment_anomaly_case_id=$1 AND action=$2),(SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type=$2)`, item.ID, paymentCompletionConfirm).Scan(&commands, &events); err != nil || success != 1 || commands != 1 || events != 1 || completionMoneySnapshot(t, r, ctx) != before {
			t.Fatal("concurrent confirmation duplicated completion", success, commands, events, err)
		}
	})
	for _, field := range []string{"amount", "currency", "customer", "sku", "channel"} {
		t.Run("conflicting-original-"+field, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
			order.ProviderSKU = "completion-sku"
			if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_sku=$2 WHERE id=$1`, order.ID, order.ProviderSKU); err != nil {
				t.Fatal(err)
			}
			provider := c.provider.(*retainedFinancialProvider)
			valid := ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "REVIEW_REQUIRED", ProviderStatus: "FAILED", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ExternalCustomerKey: order.RequesterID.String(), ExternalSKU: order.ProviderSKU}
			provider.result = valid
			switch field {
			case "amount":
				provider.result.AmountFen++
			case "currency":
				provider.result.Currency = "USD"
			case "customer":
				provider.result.ExternalCustomerKey = uuid.NewString()
			case "sku":
				provider.result.ExternalSKU = "other-sku"
			case "channel":
				provider.result.Channel = "WECHAT"
			}
			if err := c.recordProviderObservation(ctx, order.ID, "QUERY", provider.result); !errors.Is(err, ErrConflict) {
				t.Fatal("conflict producer", err)
			}
			var caseID uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT id FROM payment_anomaly_cases WHERE payment_order_id=$1 AND anomaly_type='PAYMENT_FACT_CONFLICT'`, order.ID).Scan(&caseID); err != nil {
				t.Fatal(err)
			}
			completionEscalate(t, r, ctx, caseID)
			provider.result = valid
			before := completionMoneySnapshot(t, r, ctx)
			if _, err := c.PaymentFinancialCompletion(ctx, caseID, uuid.New(), uuid.NewString(), "controlled contradictory source rejection", PaymentFinancialCompletionInput{ProofKind: "NO_FINANCIAL_EFFECT"}, uuid.Nil); !errors.Is(err, ErrConflict) {
				t.Fatal("conflicting original facts were ignored", field, err)
			}
			assessment, err := r.AccountClosureAssessment(ctx, order.RequesterID)
			if err != nil || assessment.Ready || completionMoneySnapshot(t, r, ctx) != before {
				t.Fatal("conflicting case released funds/blocker", assessment, err)
			}
		})
	}
	t.Run("actual-full-refund-receipt", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
		// These legacy backlog cases use the real anomaly creator. Cash and
		// entitlement completion comes from the unmodified refund writer.
		var evidenceID uuid.UUID
		if err := r.pool.QueryRow(ctx, `SELECT id FROM payment_evidence WHERE payment_order_id=$1 AND payment_transaction_id=$2 LIMIT 1`, order.ID, transaction).Scan(&evidenceID); err != nil {
			t.Fatal(err)
		}
		// The controlled legacy provider's original fixture omits the raw
		// status. Record a proper trusted SUCCESS observation for this cash.
		provider := c.provider.(*retainedFinancialProvider)
		provider.result.ProviderStatus = "PAY_SUCCESS"
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, evidenceID, err = c.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "QUERY", "completion-refund-source", nil, provider.result, false, r.now())
		if err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		refund, _, err := c.reserveRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), strings.Repeat("a", 64), "controlled real full refund")
		if err != nil {
			t.Fatal("reserve actual refund", err)
		}
		refund, _, err = c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: "confirmed-" + refund.ID.String(), AmountFen: refund.AmountFen, Status: "SUCCESS"}, false)
		if err != nil || refund.Status != "SUCCEEDED" {
			t.Fatal("actual refund completion", refund, err)
		}
		tx, err = r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		item, err := createPaymentAnomalyWithinTransaction(ctx, tx, "DUPLICATE_PAYMENT", "P2", uuid.NewString(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentTransactionID: &transaction, PaymentEvidenceID: &evidenceID}, map[string]any{"historical_backlog": true}, r.now())
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		item = completionEscalate(t, r, ctx, item.ID)
		before := completionMoneySnapshot(t, r, ctx)
		detail, err := r.PaymentAnomalyDetail(ctx, item.ID)
		if err != nil || len(detail.FinancialCompletionProofs) != 1 || detail.FinancialCompletionProofs[0].ReferenceID != refund.ID || completionMoneySnapshot(t, r, ctx) != before {
			t.Fatal("real detail did not discover readonly full refund receipt", detail, err)
		}
		first, second := uuid.New(), uuid.New()
		proposal, err := c.PaymentFinancialCompletion(ctx, item.ID, first, uuid.NewString(), "controlled completed full refund proposal", PaymentFinancialCompletionInput{ProofKind: "REFUND", ReferenceID: refund.ID}, uuid.Nil)
		if err != nil {
			t.Fatal("completed refund evidence", err)
		}
		if _, err := c.PaymentFinancialCompletion(ctx, item.ID, second, uuid.NewString(), "controlled independent refund confirmation", PaymentFinancialCompletionInput{}, proposal.Proposal.ID); err != nil {
			t.Fatal(err)
		}
		assessment, err := r.AccountClosureAssessment(ctx, order.RequesterID)
		if err != nil || !assessment.Ready || completionMoneySnapshot(t, r, ctx) != before {
			t.Fatal("refund completion did not unblock exactly once", assessment, err)
		}
	})
	t.Run("confirmation-audit-failure-rolls-back", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, order, item := completionNoEffectFixture(t, r, ctx, false)
		first, second := uuid.New(), uuid.New()
		proposal, err := c.PaymentFinancialCompletion(ctx, item.ID, first, uuid.NewString(), "controlled no effect completion proposal", PaymentFinancialCompletionInput{ProofKind: "NO_FINANCIAL_EFFECT"}, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION fixture_completion_audit_failure() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.event_type='CONFIRM_ESCALATED_FINANCIAL_COMPLETION' THEN RAISE EXCEPTION 'controlled completion audit fault'; END IF; RETURN NEW; END$$; CREATE TRIGGER fixture_completion_audit_failure BEFORE INSERT ON manual_review_case_events FOR EACH ROW EXECUTE FUNCTION fixture_completion_audit_failure()`); err != nil {
			t.Fatal(err)
		}
		key, reason := uuid.NewString(), "controlled independently confirmed no effect"
		if _, err := c.PaymentFinancialCompletion(ctx, item.ID, second, key, reason, PaymentFinancialCompletionInput{}, proposal.Proposal.ID); err == nil {
			t.Fatal("audit failure not reached")
		}
		current, err := r.PaymentAnomalyDetail(ctx, item.ID)
		if err != nil || current.Case.Version != item.Version || current.Case.FinancialActionStatus != "PENDING" {
			t.Fatal("failed audit partially closed case", current, err)
		}
		actual, err := getPaymentOrderByID(ctx, r.pool, order.ID, false)
		if err != nil || actual.Status != "MANUAL_REVIEW" {
			t.Fatal("failed audit partially changed order", actual, err)
		}
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER fixture_completion_audit_failure ON manual_review_case_events; DROP FUNCTION fixture_completion_audit_failure()`); err != nil {
			t.Fatal(err)
		}
		if _, err := c.PaymentFinancialCompletion(ctx, item.ID, second, key, reason, PaymentFinancialCompletionInput{}, proposal.Proposal.ID); err != nil {
			t.Fatal("original command could not resume", err)
		}
	})
	for _, duplicate := range []bool{false, true} {
		name := "late"
		if duplicate {
			name = "duplicate"
		}
		t.Run("payment-repair-"+name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			originalClock := r.now
			if !duplicate {
				r.now = func() time.Time { return originalClock().Add(-2 * time.Hour) }
			}
			c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, duplicate)
			r.now = originalClock
			var otherRefundID uuid.UUID
			if duplicate {
				var firstTransaction, firstPackage uuid.UUID
				if err := r.pool.QueryRow(ctx, `SELECT transaction.id,package.id FROM payment_transactions transaction JOIN entitlement_packages package ON package.source_payment_transaction_id=transaction.id WHERE transaction.payment_order_id=$1`, order.ID).Scan(&firstTransaction, &firstPackage); err != nil {
					t.Fatal(err)
				}
				refund, _, err := c.reserveRefund(ctx, order.ID, order.RequesterID, firstTransaction, firstPackage, uuid.NewString(), strings.Repeat("b", 64), "controlled first payment full refund")
				if err != nil {
					t.Fatal(err)
				}
				refund, _, err = c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: "first-payment-refund-" + refund.ID.String(), AmountFen: refund.AmountFen, Status: "SUCCESS"}, false)
				if err != nil || refund.Status != "SUCCEEDED" {
					t.Fatal("first payment real refund", refund, err)
				}
				otherRefundID = refund.ID
			}
			provider := c.provider.(*retainedFinancialProvider)
			paid, trade := r.now(), uuid.NewString()
			provider.result.ProviderStatus = "PAY_SUCCESS"
			provider.result.PaidAt = &paid
			provider.result.ChannelTransactionID = &trade
			review, err := c.SyncOrder(ctx, order.ID)
			if err != nil || review.Status != "MANUAL_REVIEW" {
				t.Fatal("real payment producer did not create review", review, err)
			}
			var caseID uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT id FROM payment_anomaly_cases WHERE payment_order_id=$1 AND anomaly_type=$2`, order.ID, map[bool]string{false: "LATE_PAYMENT_CONFLICT", true: "DUPLICATE_PAYMENT"}[duplicate]).Scan(&caseID); err != nil {
				t.Fatal(err)
			}
			item := completionEscalate(t, r, ctx, caseID)
			if _, err := c.RepairReviewedOrder(ctx, order.ID, uuid.New(), "controlled original financial repair", uuid.NewString()); err != nil {
				t.Fatal("real repair writer", err)
			}
			var repairID uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT repair.id FROM payment_order_repairs repair JOIN payment_transactions transaction ON transaction.id=repair.payment_transaction_id WHERE repair.payment_order_id=$1 AND transaction.provider_transaction_id=$2`, order.ID, trade).Scan(&repairID); err != nil {
				t.Fatal(err)
			}
			before := completionMoneySnapshot(t, r, ctx)
			detail, err := r.PaymentAnomalyDetail(ctx, item.ID)
			if err != nil || len(detail.FinancialCompletionProofs) != 1 || detail.FinancialCompletionProofs[0].ReferenceID != repairID || completionMoneySnapshot(t, r, ctx) != before {
				t.Fatal("real detail did not discover original repair receipt", detail, err)
			}
			first, second := uuid.New(), uuid.New()
			proposed, err := c.PaymentFinancialCompletion(ctx, item.ID, first, uuid.NewString(), "controlled completed repair proposal", PaymentFinancialCompletionInput{ProofKind: "PAYMENT_REPAIR", ReferenceID: repairID}, uuid.Nil)
			if err != nil || proposed.Proposal.Facts.ProviderTransactionID != trade {
				t.Fatal("completed repair not bound to original movement", proposed, err)
			}
			// A receipt for the same order but a different transaction cannot
			// close this duplicate-payment case.
			if duplicate {
				if _, err := c.PaymentFinancialCompletion(ctx, item.ID, first, uuid.NewString(), "controlled wrong transaction proof rejection", PaymentFinancialCompletionInput{ProofKind: "REFUND", ReferenceID: otherRefundID}, uuid.Nil); !errors.Is(err, ErrConflict) {
					t.Fatal("same order other transaction refund proof accepted", err)
				}
				if completionMoneySnapshot(t, r, ctx) != before {
					t.Fatal("rejected other-transaction proof changed funds")
				}
			}
			completed, err := c.PaymentFinancialCompletion(ctx, item.ID, second, uuid.NewString(), "controlled independent repair confirmation", PaymentFinancialCompletionInput{}, proposed.Proposal.ID)
			if err != nil || completed.Case.FinancialActionStatus != "APPLIED" || completionMoneySnapshot(t, r, ctx) != before {
				t.Fatal("completion repeated financial effect", completed, err)
			}
			assessment, err := r.AccountClosureAssessment(ctx, order.RequesterID)
			if err != nil {
				t.Fatal(err)
			}
			for _, blocker := range assessment.Blockers {
				if blocker.Code == "PAYMENT_ANOMALY_OPEN" || blocker.Code == "PAYMENT_PROCESSING" {
					t.Fatal("proven repair left original anomaly/order blocked", assessment)
				}
			}
			if assessment.Ready {
				t.Fatal("completion bypassed nonzero balance blocker")
			}
		})
	}
	for _, orderOnly := range []bool{false, true} {
		name := "failed-evidence"
		if orderOnly {
			name = "legacy-order-only"
		}
		t.Run("no-effect-"+name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, item := completionNoEffectFixture(t, r, ctx, orderOnly)
			allowed := &atomic.Bool{}
			allowed.Store(true)
			server := httptest.NewServer(directRepairHandler(t, r, c, allowed))
			defer server.Close()
			proposer, reviewer := uuid.New(), uuid.New()
			before := completionMoneySnapshot(t, r, ctx)
			assessment, err := r.AccountClosureAssessment(ctx, order.RequesterID)
			if err != nil || assessment.Ready {
				t.Fatal("pending case not blocking closure", assessment, err)
			}
			body := map[string]any{"proof_kind": "NO_FINANCIAL_EFFECT", "reason": "controlled independent completion proposal"}
			key := "completion-proposal-" + uuid.NewString()
			code, proposed, raw := completionHTTP(t, server.URL, item.ID, proposer, key, uuid.Nil, body)
			if code != 200 || proposed.Proposal.ProposerID != proposer || proposed.Case.FinancialActionStatus != "PENDING" {
				t.Fatal("propose", code, string(raw))
			}
			proposal := proposed.Proposal.ID
			confirm := map[string]any{"reason": "controlled independent completion confirmation"}
			if code, _, _ := completionHTTP(t, server.URL, item.ID, proposer, "self-confirm-"+uuid.NewString(), proposal, confirm); code != 409 {
				t.Fatal("proposer confirmed own evidence", code)
			}
			allowed.Store(false)
			if code, _, _ := completionHTTP(t, server.URL, item.ID, reviewer, "denied-confirm-"+uuid.NewString(), proposal, confirm); code != 403 {
				t.Fatal("revoked authorization", code)
			}
			allowed.Store(true)
			if completionMoneySnapshot(t, r, ctx) != before {
				t.Fatal("proposal/denial minted or removed money")
			}
			confirmKey := "confirm-" + uuid.NewString()
			code, completed, raw := completionHTTP(t, server.URL, item.ID, reviewer, confirmKey, proposal, confirm)
			if code != 200 || completed.Case.FinancialActionStatus != "NONE" || completed.Case.Version != item.Version+1 {
				t.Fatal("confirm", code, string(raw))
			}
			assessment, err = r.AccountClosureAssessment(ctx, order.RequesterID)
			if err != nil || !assessment.Ready || completionMoneySnapshot(t, r, ctx) != before {
				t.Fatal("completed no-effect did not unblock without funds", assessment, err)
			}
			// Receipt-first survives an unavailable provider and current terminal
			// state. This models retry after an earlier ACK was lost.
			c.provider.(*retainedFinancialProvider).err = errors.New("provider unavailable after committed confirmation")
			code, replayed, _ := completionHTTP(t, server.URL, item.ID, reviewer, confirmKey, proposal, confirm)
			if code != 200 || replayed.Case.Version != completed.Case.Version || completionMoneySnapshot(t, r, ctx) != before {
				t.Fatal("committed result not replayed first", code, replayed)
			}
			if code, _, _ := completionHTTP(t, server.URL, item.ID, uuid.New(), confirmKey, proposal, confirm); code != 409 {
				t.Fatal("other actor stole receipt", code)
			}
			var commands int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_anomaly_commands WHERE payment_anomaly_case_id=$1 AND action=$2`, item.ID, paymentCompletionConfirm).Scan(&commands); err != nil || commands != 1 {
				t.Fatal("duplicate completion", commands, err)
			}
		})
	}
	t.Run("new-payment-evidence-keeps-blocked", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, order, item := completionNoEffectFixture(t, r, ctx, false)
		first, second := uuid.New(), uuid.New()
		reason := "controlled no effect completion proposal"
		proposal, err := c.PaymentFinancialCompletion(ctx, item.ID, first, uuid.NewString(), reason, PaymentFinancialCompletionInput{ProofKind: "NO_FINANCIAL_EFFECT"}, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		provider := c.provider.(*retainedFinancialProvider)
		paid := r.now()
		trade := uuid.NewString()
		provider.result.Status = "SUCCESS"
		provider.result.ProviderStatus = "PAY_SUCCESS"
		provider.result.ChannelTransactionID = &trade
		provider.result.PaidAt = &paid
		before := completionMoneySnapshot(t, r, ctx)
		if _, err := c.PaymentFinancialCompletion(ctx, item.ID, second, uuid.NewString(), reason, PaymentFinancialCompletionInput{}, proposal.Proposal.ID); !errors.Is(err, ErrConflict) {
			t.Fatal("new authoritative payment was ignored", err)
		}
		if completionMoneySnapshot(t, r, ctx) != before {
			t.Fatal("rejected completion changed money")
		}
		assessment, err := r.AccountClosureAssessment(ctx, order.RequesterID)
		if err != nil || assessment.Ready {
			t.Fatal("conflicting evidence unblocked closure", assessment, err)
		}
	})
}
