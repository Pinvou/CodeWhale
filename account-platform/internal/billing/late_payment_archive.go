package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Archive recovery confirms the original invocation, not the current terminal
// state. Current permission and ownership still gate its immutable receipt.
func (r *PostgresRepository) ArchiveLatePaymentReview(ctx context.Context, caseID, actorID uuid.UUID, key string) (LatePaymentReviewCase, error) {
	key = strings.TrimSpace(key)
	if caseID == uuid.Nil || actorID == uuid.Nil || len(key) < 8 || len(key) > 120 {
		return LatePaymentReviewCase{}, ErrConflict
	}
	request, err := json.Marshal(struct {
		CaseID  uuid.UUID `json:"case_id"`
		ActorID uuid.UUID `json:"actor_id"`
	}{caseID, actorID})
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	digest := sha256.Sum256(request)
	hash := hex.EncodeToString(digest[:])
	var result LatePaymentReviewCase
	err = reconciliationRetry(ctx, func() error {
		var attemptErr error
		result, attemptErr = r.archiveLatePaymentReviewOnce(ctx, caseID, actorID, key, hash, request)
		return attemptErr
	})
	return result, err
}

func (r *PostgresRepository) archiveLatePaymentReviewOnce(ctx context.Context, caseID, actorID uuid.UUID, key, hash string, request []byte) (LatePaymentReviewCase, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "late-payment-review.archive", key); err != nil {
		return LatePaymentReviewCase{}, err
	}
	item, found, err := scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1 FOR UPDATE OF late,manual`, caseID))
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	if !found || item.OwnerID == nil || *item.OwnerID != actorID {
		return LatePaymentReviewCase{}, ErrConflict
	}
	var savedHash string
	var savedActor, savedCase uuid.UUID
	var receipt []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,actor_id,case_id,after_snapshot FROM late_payment_review_archive_commands WHERE idempotency_key=$1`, key).Scan(&savedHash, &savedActor, &savedCase, &receipt)
	if err == nil {
		if savedHash != hash || savedActor != actorID || savedCase != caseID {
			return LatePaymentReviewCase{}, ErrConflict
		}
		var original LatePaymentReviewCase
		if err := json.Unmarshal(receipt, &original); err != nil {
			return LatePaymentReviewCase{}, err
		}
		if original.ID != caseID || original.OwnerID == nil || *original.OwnerID != actorID || original.ReviewStatus != "ARCHIVED_CONFIRMED_UNPAID" {
			return LatePaymentReviewCase{}, ErrConflict
		}
		return original, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return LatePaymentReviewCase{}, err
	}
	// Only a new command may attempt the original active/proof admission below.
	if item.ReviewStatus != "REVIEW_REQUIRED_LATE_PAYMENT" {
		return LatePaymentReviewCase{}, ErrConflict
	}
	before, err := json.Marshal(item)
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	now := r.now()
	var eligibleAt time.Time
	var valid bool
	err = tx.QueryRow(ctx, `SELECT final_archive_eligible_at,
		$2>=final_archive_eligible_at AND last_successful_query_at>=final_archive_eligible_at AND last_authoritative_unpaid_at>=final_archive_eligible_at
		AND NOT EXISTS(SELECT 1 FROM payment_transactions WHERE payment_order_id=payment.id AND status IN ('PAID','LATE_PAID','REVIEW_REQUIRED'))
		AND NOT EXISTS(SELECT 1 FROM payment_evidence evidence WHERE evidence.payment_order_id=payment.id AND
            (evidence.merge_result='REVIEW_REQUIRED' OR
             (evidence.evidence_type='QUERY' AND evidence.merge_result='RECORDED' AND evidence.evidence_key LIKE '%|deadline'
              AND evidence.provider_id=payment.provider_id AND evidence.provider_version=payment.provider_version
              AND evidence.merchant_order_no=payment.merchant_order_no AND evidence.amount_fen=payment.amount_fen AND evidence.currency=payment.currency
              AND evidence.payload_snapshot->>'status'='SUCCESS'
              AND evidence.payload_snapshot->>'merchant_order_no'=payment.merchant_order_no
              AND evidence.payload_snapshot->>'external_customer_key'=payment.requester_id::text
              AND evidence.payload_snapshot->>'external_sku'=payment.provider_sku
              AND evidence.payload_snapshot->>'channel'=payment.channel
              AND evidence.payload_snapshot->'amount_fen'=to_jsonb(payment.amount_fen)
              AND evidence.payload_snapshot->>'currency'=payment.currency)))
		AND NOT EXISTS(SELECT 1 FROM late_payment_review_evidence claim WHERE claim.case_id=$1 AND claim.evidence_type='USER_PAYMENT_CLAIM' AND NOT EXISTS(SELECT 1 FROM late_payment_review_evidence query WHERE query.case_id=claim.case_id AND query.evidence_type IN ('MANUAL_QUERY','ARCHIVE_QUERY') AND query.created_at>claim.created_at AND query.payload_snapshot->>'query_succeeded'='true' AND payment.last_authoritative_unpaid_at>claim.created_at))
		FROM payment_orders payment WHERE id=$3 FOR UPDATE`, caseID, now, item.PaymentOrderID).Scan(&eligibleAt, &valid)
	if err != nil || !valid {
		return LatePaymentReviewCase{}, ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE payment_orders SET archive_status='ARCHIVED_CONFIRMED_UNPAID',archived_at=$2,payment_url=NULL,qr_data=NULL,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,updated_at=$2 WHERE id=$1`, item.PaymentOrderID, now)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE late_payment_review_cases SET review_status='ARCHIVED_CONFIRMED_UNPAID',updated_at=$2,resolved_at=$2 WHERE id=$1`, caseID, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE manual_review_cases SET status='RESOLVED',resolved_at=$2,updated_at=$2 WHERE id=$1`, caseID, now)
	}
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, caseID, "ARCHIVED_CONFIRMED_UNPAID", &actorID, map[string]any{"final_archive_eligible_at": eligibleAt}, now)
	}
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	item, found, err = scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1`, caseID))
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	if !found {
		return LatePaymentReviewCase{}, ErrNotFound
	}
	after, err := json.Marshal(item)
	if err != nil {
		return LatePaymentReviewCase{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO late_payment_review_archive_commands(id,case_id,actor_id,idempotency_key,request_hash,request_snapshot,before_snapshot,after_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, uuid.Must(uuid.NewV7()), caseID, actorID, key, hash, json.RawMessage(request), json.RawMessage(before), json.RawMessage(after), now); err != nil {
		return LatePaymentReviewCase{}, err
	}
	return item, tx.Commit(ctx)
}
