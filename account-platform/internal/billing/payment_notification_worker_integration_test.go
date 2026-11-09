package billing

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPaymentNotificationWorkerEscalatesUnmatchedTrustedCallback(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	c.provider = &callbackBoundProvider{c.provider.(*retainedFinancialProvider), "controlled", "v1"}
	missingOrder := order
	missingOrder.MerchantOrderNo = uuid.NewString()
	id := uuid.New()
	body, signature := signedCoreCallback(t, c, missingOrder, id)
	if _, created, err := c.PersistNotification(ctx, order.Channel, signature, body, "fixture-source"); err != nil || !created {
		t.Fatal("trusted callback was not admitted", err)
	}
	worker := NewPaymentNotificationWorker(c, time.Second)

	const retryLimit = 12
	for attempt := 1; attempt <= retryLimit; attempt++ {
		// Advance only the persisted schedule. Exercise the real worker and
		// its transaction for each attempt without a wall-clock sleep.
		if _, err := r.pool.Exec(ctx, `UPDATE payment_notifications SET next_attempt_at=now()-interval '1 second' WHERE notification_id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if err := worker.processBatch(ctx); err != nil {
			t.Fatal(err)
		}
		var result string
		var count int
		var scheduled bool
		if err := r.pool.QueryRow(ctx, `SELECT result,processing_attempts,next_attempt_at IS NOT NULL FROM payment_notifications WHERE notification_id=$1`, id).Scan(&result, &count, &scheduled); err != nil {
			t.Fatal(err)
		}
		if count != attempt {
			t.Fatalf("attempt count = %d, want %d", count, attempt)
		}
		if attempt < retryLimit && (result != "RECEIVED" || !scheduled) {
			t.Fatalf("attempt %d entered review before its retry budget: %s", attempt, result)
		}
		if attempt == retryLimit && (result != "MANUAL_REVIEW" || scheduled) {
			t.Fatalf("exhausted unmatched notification did not enter review: %s", result)
		}
	}

	if _, created, err := c.PersistNotification(ctx, order.Channel, signature, body, "fixture-source"); err != nil || created {
		t.Fatal("same signed callback did not return its original receipt", err)
	}
	if err := worker.processBatch(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := r.AdminPaymentAnomalies(ctx, PaymentAnomalyFilter{AnomalyType: "PAYMENT_FACT_CONFLICT", Status: "OPEN"}, 10)
	if err != nil || len(items) != 1 || items[0].PaymentOrderID != nil || items[0].PaymentEvidenceID != nil {
		t.Fatalf("missing or incorrectly bound manual case: %d, %v", len(items), err)
	}
	detail, err := r.PaymentAnomalyDetail(ctx, items[0].ID)
	var snapshot struct {
		NotificationID uuid.UUID `json:"notification_id"`
		Reason         string    `json:"reason"`
	}
	if err != nil || len(detail.Events) != 1 || detail.Events[0].EventType != "CREATED" ||
		json.Unmarshal(detail.Case.ResourceSnapshot, &snapshot) != nil || snapshot.NotificationID != id || snapshot.Reason != "PAYMENT_ORDER_NOT_FOUND" {
		t.Fatal("manual case did not preserve the auditable notification reference", err)
	}
	var preserved bool
	var posted, journals, transactions int64
	err = r.pool.QueryRow(ctx, `SELECT n.payload=$2::jsonb AND n.provider_id='controlled' AND n.provider_version='v1'
 AND n.processed_at IS NOT NULL AND n.last_error_code='PAYMENT_ORDER_NOT_FOUND',
 w.posted_microcredits,(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM payment_transactions)
FROM payment_notifications n CROSS JOIN wallets w WHERE n.notification_id=$1 AND w.id=$3`, id, body, order.WalletID).Scan(&preserved, &posted, &journals, &transactions)
	if err != nil || !preserved || posted != 0 || journals != 0 || transactions != 0 {
		t.Fatal("manual review lost provenance or applied a financial effect", preserved, posted, journals, transactions, err)
	}
	actor := uuid.New()
	if _, err := r.ClaimPaymentAnomaly(ctx, items[0].ID, actor, uuid.NewString()); err != nil {
		t.Fatal("manual case is not actionable through existing review commands", err)
	}
	if _, err := r.ResolvePaymentAnomaly(ctx, items[0].ID, uuid.New(), uuid.NewString(), "DISMISS", "verified another transaction", "NONE"); !errors.Is(err, ErrConflict) {
		t.Fatal("another reviewer could bypass the existing case ownership fence", err)
	}
}

func TestPaymentNotificationWorkerKeepsUnverifiedCallbacksOnRetry(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	c.provider = &callbackBoundProvider{c.provider.(*retainedFinancialProvider), "controlled", "v1"}
	order.MerchantOrderNo = uuid.NewString()
	id := uuid.New()
	body, _ := signedCoreCallback(t, c, order, id)
	if _, err := r.pool.Exec(ctx, `INSERT INTO payment_notifications(notification_id,channel,request_hash,payload,received_at,result,processing_attempts)
VALUES($1,$2,'legacy-fixture',$3,now(),'RECEIVED',11)`, id, order.Channel, body); err != nil {
		t.Fatal(err)
	}
	if err := c.ProcessPersistedNotification(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("direct processing changed missing order error identity", err)
	}
	worker := NewPaymentNotificationWorker(c, time.Second)
	if err := worker.processBatch(ctx); err != nil {
		t.Fatal(err)
	}
	var kept bool
	if err := r.pool.QueryRow(ctx, `SELECT result='RECEIVED' AND provider_id IS NULL AND provider_version IS NULL AND processing_attempts=12
 AND next_attempt_at>now() AND NOT EXISTS(SELECT 1 FROM payment_anomaly_cases) FROM payment_notifications WHERE notification_id=$1`, id).Scan(&kept); err != nil || !kept {
		t.Fatal("legacy source was promoted or its retry state was lost", kept, err)
	}
}

func TestPaymentNotificationWorkerRollsBackAndRetriesManualCase(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	c.provider = &callbackBoundProvider{c.provider.(*retainedFinancialProvider), "controlled", "v1"}
	order.MerchantOrderNo = uuid.NewString()
	id := uuid.New()
	body, signature := signedCoreCallback(t, c, order, id)
	if _, _, err := c.PersistNotification(ctx, order.Channel, signature, body, "fixture-source"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE payment_notifications SET processing_attempts=11 WHERE notification_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION fixture_fail_unmatched_case() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'controlled manual case failure'; END $$;
CREATE TRIGGER fixture_fail_unmatched_case BEFORE INSERT ON manual_review_cases FOR EACH ROW EXECUTE FUNCTION fixture_fail_unmatched_case()`); err != nil {
		t.Fatal(err)
	}
	worker := NewPaymentNotificationWorker(c, time.Second)
	if err := worker.processBatch(ctx); err != nil {
		t.Fatal(err)
	}
	var retryable bool
	if err := r.pool.QueryRow(ctx, `SELECT result='RECEIVED' AND processed_at IS NULL AND processing_attempts=12
 AND next_attempt_at>now() AND last_error_code='POSTGRES_P0001' AND NOT EXISTS(SELECT 1 FROM manual_review_cases)
FROM payment_notifications WHERE notification_id=$1`, id).Scan(&retryable); err != nil || !retryable {
		t.Fatal("failed case creation did not leave one retryable notification", retryable, err)
	}
	if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER fixture_fail_unmatched_case ON manual_review_cases; DROP FUNCTION fixture_fail_unmatched_case()`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE payment_notifications SET next_attempt_at=now()-interval '1 second' WHERE notification_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- worker.processBatch(ctx) }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var reviewed bool
	if err := r.pool.QueryRow(ctx, `SELECT result='MANUAL_REVIEW' AND processing_attempts=13 AND next_attempt_at IS NULL
 AND (SELECT count(*) FROM payment_anomaly_cases)=1 AND (SELECT count(*) FROM manual_review_cases)=1
FROM payment_notifications WHERE notification_id=$1`, id).Scan(&reviewed); err != nil || !reviewed {
		t.Fatal("competing workers did not recover one durable case", reviewed, err)
	}
}
